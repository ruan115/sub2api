package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

func workerReadinessFixture(t *testing.T) (*MemoryRepository, time.Time) {
	t.Helper()
	r, now := executionBindingFixture(t)
	// Intentionally no envelope, hint, KMS or credential material: a metadata
	// projection must not validate, decrypt or copy any secret-bearing fields.
	r.credentialVaults["account-1"] = memoryCredentialVault{ActiveVersionID: "opaque-version-1", AuthType: "oauth",
		CreatedAt: now.Add(-2 * time.Second), UpdatedAt: now.Add(-time.Second)}
	r.credentialVersions["opaque-version-1"] = credential.VersionRecord{ID: "opaque-version-1", AccountID: "account-1",
		VersionNumber: 1, AuthType: "oauth", CreatedAt: now.Add(-time.Second)}
	if _, err := r.GrantProxyReservation(context.Background(), ProxyReservationGrant{
		ReservationID: "reservation-1", AccountID: "account-1", DesiredGeneration: 1,
		ProxyBindingID: "17", BindingRevision: 2, GrantEventID: "grant-1",
		CreatedAt: now.Add(-time.Second), UpdatedAt: now.Add(-time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GrantProxyLease(context.Background(), ProxyLease{
		ID: "proxy-1", ReservationID: "reservation-1", AccountID: "account-1", DesiredGeneration: 1,
		BindingRevision: 2, SlotID: "slot-1", ExecutionEpoch: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return r, now
}

func requireReadinessDenied(t *testing.T, got WorkerReadinessBinding, err error) {
	t.Helper()
	if got != (WorkerReadinessBinding{}) || err != ErrWorkerReadinessBindingUnavailable {
		t.Fatalf("expected fixed denial and zero metadata, got %+v / %v", got, err)
	}
}

func TestReadWorkerReadinessBindingMemoryUsesCurrentPointersWithoutSecrets(t *testing.T) {
	r, now := workerReadinessFixture(t)
	// The greatest version and an old operation must not override the active pointer.
	r.credentialVersions["opaque-version-99"] = credential.VersionRecord{ID: "opaque-version-99", AccountID: "account-1", VersionNumber: 99}
	r.credentialVersionIDs["account-1"] = []string{"opaque-version-1", "opaque-version-99"}
	r.credentialOperations["old-operation"] = "opaque-version-99"
	got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.ReadExecutionBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil || got != (WorkerReadinessBinding{ExecutionBinding: base,
		CredentialVersionID: "opaque-version-1", CredentialVersionNumber: 1, AuthType: "oauth",
		ProxyLeaseID: "proxy-1", ProxyReservationID: "reservation-1", ProxyBindingID: "17", ProxyBindingRevision: 2}) {
		t.Fatalf("unexpected metadata: %+v / %v", got, err)
	}
	got.CredentialVersionID, got.ProxyLeaseID, got.AccountID = "local", "local", "local"
	again, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil || again.CredentialVersionID != "opaque-version-1" || again.ProxyLeaseID != "proxy-1" || again.AccountID != "account-1" || !again.ObservedAt.Equal(base.ObservedAt) {
		t.Fatal("read changed authority, refreshed observation or returned mutable state")
	}
	for _, state := range []string{"ready", "running", "busy"} {
		for _, auth := range []string{"oauth", "setup_token", "api_key"} {
			r.assignments["slot-1"][0].ActualState = state
			vault := r.credentialVaults["account-1"]
			vault.AuthType = auth
			r.credentialVaults["account-1"] = vault
			if got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second); err != nil || got.AuthType != auth {
				t.Fatalf("B1 state/auth %s/%s rejected: %v", state, auth, err)
			}
		}
	}
}

func TestReadWorkerReadinessBindingMemoryRejectsInvalidMetadata(t *testing.T) {
	tests := []struct {
		name string
		edit func(*memoryCredentialVault, *credential.VersionRecord, *ProxyLease, *ProxyReservationGrant, time.Time)
	}{
		{"no active pointer", func(v *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.ActiveVersionID = ""
		}},
		{"orphan active pointer", func(v *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.ActiveVersionID = "missing"
		}},
		{"unknown auth", func(v *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.AuthType = "future_auth"
		}},
		{"padded auth", func(v *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.AuthType = "oauth "
		}},
		{"vault zero creation", func(v *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.CreatedAt = time.Time{}
		}},
		{"vault future update", func(v *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, now time.Time) {
			v.UpdatedAt = now.Add(time.Second)
		}},
		{"vault reversed history", func(v *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.UpdatedAt = v.CreatedAt.Add(-time.Second)
		}},
		{"cross account version", func(_ *memoryCredentialVault, v *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.AccountID = "account-2"
		}},
		{"padded version account", func(_ *memoryCredentialVault, v *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.AccountID += " "
		}},
		{"version case mismatch", func(_ *memoryCredentialVault, v *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.ID = "Opaque-version-1"
		}},
		{"version ID mismatch", func(_ *memoryCredentialVault, v *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.ID = "other-version"
		}},
		{"version number zero", func(_ *memoryCredentialVault, v *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.VersionNumber = 0
		}},
		{"version zero creation", func(_ *memoryCredentialVault, v *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			v.CreatedAt = time.Time{}
		}},
		{"version future creation", func(_ *memoryCredentialVault, v *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, now time.Time) {
			v.CreatedAt = now.Add(time.Second)
		}},
		{"version predates vault", func(v *memoryCredentialVault, c *credential.VersionRecord, _ *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			c.CreatedAt = v.CreatedAt.Add(-time.Second)
		}},
		{"proxy ID mismatch", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.ID = "proxy-2"
		}},
		{"proxy wrong slot", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.SlotID = "slot-2"
		}},
		{"proxy wrong epoch", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.ExecutionEpoch++
		}},
		{"proxy wrong account", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.AccountID = "account-2"
		}},
		{"proxy wrong generation", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.DesiredGeneration++
		}},
		{"proxy missing reservation", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.ReservationID = "missing"
		}},
		{"proxy wrong revision", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.BindingRevision++
		}},
		{"proxy revoked", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, now time.Time) {
			p.RevokedAt = &now
		}},
		{"proxy zero creation", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.CreatedAt = time.Time{}
		}},
		{"proxy future update", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, now time.Time) {
			p.UpdatedAt = now.Add(time.Second)
		}},
		{"proxy reversed history", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.UpdatedAt = p.CreatedAt.Add(-time.Second)
		}},
		{"reservation ID mismatch", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.ReservationID += "x"
		}},
		{"reservation wrong account", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.AccountID = "account-2"
		}},
		{"reservation wrong generation", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.DesiredGeneration++
		}},
		{"reservation wrong revision", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.BindingRevision++
		}},
		{"reservation zero revision", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, q *ProxyReservationGrant, _ time.Time) {
			p.BindingRevision, q.BindingRevision = 0, 0
		}},
		{"reservation endpoint as binding", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.ProxyBindingID = "127.0.0.1:3128"
		}},
		{"reservation padded binding", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.ProxyBindingID = "017"
		}},
		{"reservation revoked", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, now time.Time) {
			p.RevokedAt = &now
		}},
		{"reservation revoke event only", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.RevokeEventID = "revoke-1"
		}},
		{"reservation zero creation", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.CreatedAt = time.Time{}
		}},
		{"reservation future update", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, now time.Time) {
			p.UpdatedAt = now.Add(time.Second)
		}},
		{"reservation reversed history", func(_ *memoryCredentialVault, _ *credential.VersionRecord, _ *ProxyLease, p *ProxyReservationGrant, _ time.Time) {
			p.UpdatedAt = p.CreatedAt.Add(-time.Second)
		}},
		{"reservation after lease creation", func(_ *memoryCredentialVault, _ *credential.VersionRecord, p *ProxyLease, _ *ProxyReservationGrant, _ time.Time) {
			p.CreatedAt = p.CreatedAt.Add(-2 * time.Second)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, now := workerReadinessFixture(t)
			vault, version := r.credentialVaults["account-1"], r.credentialVersions["opaque-version-1"]
			proxy, reservation := r.proxyLeases["proxy-1"], r.proxyReservations["reservation-1"]
			test.edit(&vault, &version, &proxy, &reservation, now)
			r.credentialVaults["account-1"], r.credentialVersions["opaque-version-1"] = vault, version
			r.proxyLeases["proxy-1"], r.proxyReservations["reservation-1"] = proxy, reservation
			got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireReadinessDenied(t, got, err)
		})
	}
	for _, missing := range []string{"vault", "version", "proxy index", "proxy", "reservation"} {
		t.Run("missing "+missing, func(t *testing.T) {
			r, now := workerReadinessFixture(t)
			switch missing {
			case "vault":
				delete(r.credentialVaults, "account-1")
			case "version":
				delete(r.credentialVersions, "opaque-version-1")
			case "proxy index":
				delete(r.proxyLeaseIDsByEpoch, executionLeaseKey("slot-1", 1))
			case "proxy":
				delete(r.proxyLeases, "proxy-1")
			case "reservation":
				delete(r.proxyReservations, "reservation-1")
			}
			got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireReadinessDenied(t, got, err)
			if _, err := r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second); err != nil {
				t.Fatal("missing loaded metadata must not block independent health/inspect authority")
			}
		})
	}
}

func TestReadWorkerReadinessBindingMemoryKeepsB1Fence(t *testing.T) {
	for _, fault := range []string{"unhealthy", "stale observation", "missing observation", "old session", "disconnected", "expired lease", "revoked lease", "wrong generation", "wrong image", "released"} {
		t.Run(fault, func(t *testing.T) {
			r, now := workerReadinessFixture(t)
			a := &r.assignments["slot-1"][0]
			switch fault {
			case "unhealthy":
				a.Healthy = false
			case "stale observation":
				at := now.Add(-45 * time.Second)
				a.LastObservedAt = &at
			case "missing observation":
				a.LastObservedAt = nil
			case "old session":
				a.ObservedControlSessionID = bindingSessionTwo
			case "disconnected":
				n := r.nodes["srv74"]
				n.Status = "disconnected"
				r.nodes["srv74"] = n
			case "expired lease":
				l := r.executionLeases[executionLeaseKey("slot-1", 1)]
				l.ExpiresAt = now
				r.executionLeases[executionLeaseKey("slot-1", 1)] = l
			case "revoked lease":
				l := r.executionLeases[executionLeaseKey("slot-1", 1)]
				l.RevokedAt = &now
				r.executionLeases[executionLeaseKey("slot-1", 1)] = l
			case "wrong generation":
				a.DesiredGeneration++
			case "wrong image":
				a.ImageDigest = "sha256:" + strings.Repeat("b", 64)
			case "released":
				a.ReleasedAt = &now
			}
			got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireReadinessDenied(t, got, err)
		})
	}
}

func TestReadWorkerReadinessBindingMemoryAtomicAndCancellation(t *testing.T) {
	r, now := workerReadinessFixture(t)
	r.credentialVersions["opaque-version-2"] = credential.VersionRecord{ID: "opaque-version-2", AccountID: "account-1", VersionNumber: 2, CreatedAt: now.Add(-time.Second)}
	p := r.proxyLeases["proxy-1"]
	p.ID = "proxy-2"
	r.proxyLeases[p.ID] = p
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			r.mu.Lock()
			v := r.credentialVaults["account-1"]
			v.ActiveVersionID = fmt.Sprintf("opaque-version-%d", 1+i%2)
			r.credentialVaults["account-1"] = v
			r.proxyLeaseIDsByEpoch[executionLeaseKey("slot-1", 1)] = fmt.Sprintf("proxy-%d", 1+i%2)
			r.mu.Unlock()
		}
	}()
	for i := 0; i < 1000; i++ {
		got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
		if err != nil || got.CredentialVersionID != fmt.Sprintf("opaque-version-%d", got.CredentialVersionNumber) || got.ProxyLeaseID != fmt.Sprintf("proxy-%d", got.CredentialVersionNumber) {
			t.Errorf("torn authority projection: %+v / %v", got, err)
			break
		}
	}
	wg.Wait()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &firstErrObservedContext{Context: parent, observed: make(chan struct{})}
	r.mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		got, err := r.ReadWorkerReadinessBinding(ctx, "slot-1", now, 45*time.Second)
		requireReadinessDenied(t, got, err)
	}()
	<-ctx.observed
	cancel()
	r.mu.Unlock()
	<-done
}

func readinessSQLMatcher(_ string, actual string) error {
	normalized := strings.Join(strings.Fields(actual), " ")
	for _, guard := range []string{
		"SELECT s.account_id, s.slot_id, sa.node_id, sa.provider_ref, el.owner_id",
		"sa.last_observed_at, n.last_seen_at, el.expires_at", "sa.assigned_at, el.created_at, el.updated_at",
		"v.version_id, v.version_number, cv.auth_type", "pl.proxy_lease_id, prg.reservation_id, prg.proxy_binding_id, prg.binding_revision",
		"cv.created_at, cv.updated_at, v.created_at, pl.created_at, pl.updated_at, prg.created_at, prg.updated_at",
		"FROM slots s", "JOIN slot_assignments sa", "BINARY sa.slot_id = BINARY s.slot_id AND sa.released_at IS NULL",
		"JOIN nodes n ON n.node_id = sa.node_id AND BINARY n.node_id = BINARY sa.node_id",
		"JOIN execution_leases el", "BINARY el.slot_id = BINARY sa.slot_id", "el.execution_epoch = sa.execution_epoch", "BINARY el.node_id = BINARY sa.node_id",
		"JOIN credential_vault cv", "BINARY cv.account_id = BINARY s.account_id",
		"JOIN credential_versions v", "BINARY v.version_id = BINARY cv.active_version_id", "BINARY v.account_id = BINARY cv.account_id",
		"JOIN proxy_leases pl", "BINARY pl.slot_id = BINARY sa.slot_id", "pl.execution_epoch = sa.execution_epoch", "BINARY pl.account_id = BINARY s.account_id", "pl.desired_generation = s.desired_generation",
		"JOIN proxy_reservation_grants prg", "BINARY prg.reservation_id = BINARY pl.reservation_id", "BINARY prg.account_id = BINARY pl.account_id",
		"prg.desired_generation = pl.desired_generation AND prg.binding_revision = pl.binding_revision", "WHERE s.slot_id = ?",
		"s.desired_state = 'ready'", "s.desired_generation > 0 AND sa.desired_generation = s.desired_generation", "sa.execution_epoch > 0",
		"BINARY sa.image_digest = BINARY s.image_digest", "sa.healthy = 1 AND sa.actual_state IN ('ready', 'running', 'busy')",
		"sa.provider_ref IS NOT NULL AND sa.provider_ref <> ''", "n.status = 'connected'", "n.control_session_id IS NOT NULL AND n.control_session_id <> ''",
		"sa.observed_control_session_id IS NOT NULL", "BINARY sa.observed_control_session_id = BINARY n.control_session_id",
		"sa.last_observed_at > ? AND sa.last_observed_at <= ?", "n.last_seen_at > ? AND n.last_seen_at <= ?", "sa.assigned_at <= ?",
		"el.revoked_at IS NULL AND el.created_at <= ? AND el.updated_at <= ?", "el.expires_at > ?",
		"cv.active_version_id IS NOT NULL AND v.version_number > 0", "pl.revoked_at IS NULL AND prg.revoked_at IS NULL AND prg.revoke_event_id IS NULL",
	} {
		if !strings.Contains(normalized, guard) {
			return fmt.Errorf("missing worker readiness query guard: %s", guard)
		}
	}
	if regexp.MustCompile(`(?i)\b(UPDATE|INSERT|DELETE|REPLACE|MAX)\b|ciphertext|encrypted_dek|nonce|aad|hint|kms|password|endpoint|url|credential_leases|credential_version_operations|UTC_TIMESTAMP|route_cache`).MatchString(normalized) || strings.Count(normalized, "SELECT ") != 1 {
		return errors.New("query must be one read-only secret-free metadata projection")
	}
	return nil
}

var readinessColumns = append(append([]string(nil), bindingColumns...), "version_id", "version_number", "auth_type", "proxy_lease_id", "reservation_id", "proxy_binding_id", "binding_revision", "vault_created_at", "vault_updated_at", "version_created_at", "proxy_created_at", "proxy_updated_at", "reservation_created_at", "reservation_updated_at")

func readinessRow(now time.Time) []driver.Value {
	return append(bindingRow(now), "opaque-version-1", int64(1), "oauth", "proxy-1", "reservation-1", "17", int64(2),
		now.Add(-3*time.Second), now.Add(-time.Second), now.Add(-time.Second), now, now, now.Add(-time.Second), now.Add(-time.Second))
}

func readinessSQLFixture(t *testing.T) (*Repository, sqlmock.Sqlmock, time.Time) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherFunc(readinessSQLMatcher)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r, _ := NewRepository(db)
	return r, mock, time.Unix(2_000_000_000, 0).UTC()
}

func TestReadWorkerReadinessBindingSQLSingleSecretFreeProjection(t *testing.T) {
	r, mock, now := readinessSQLFixture(t)
	expectBindingRead(mock, now).WillReturnRows(sqlmock.NewRows(readinessColumns).AddRow(readinessRow(now)...))
	got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil || got.CredentialVersionID != "opaque-version-1" || got.CredentialVersionNumber != 1 || got.ProxyLeaseID != "proxy-1" || got.ProxyReservationID != "reservation-1" || got.ProxyBindingID != "17" || got.ProxyBindingRevision != 2 || !got.ObservedAt.Equal(now.Add(-time.Second)) {
		t.Fatalf("unexpected SQL metadata: %+v / %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReadWorkerReadinessBindingSQLDeniesMalformedAndFutureMetadata(t *testing.T) {
	for _, test := range []struct {
		name  string
		index int
		value driver.Value
	}{
		{"wrong slot", 1, "other-slot"}, {"bad session", 5, "bad"}, {"stale observed time", 9, time.Unix(0, 0)},
		{"missing version", 15, nil}, {"empty version", 15, ""}, {"padded version", 15, "version "}, {"oversized version", 15, strings.Repeat("v", 129)},
		{"zero version", 16, int64(0)}, {"negative version", 16, int64(-1)}, {"unknown auth", 17, "unknown"}, {"padded auth", 17, "oauth "},
		{"empty proxy", 18, ""}, {"unsafe proxy", 18, "proxy\n"}, {"empty reservation", 19, ""}, {"unsafe reservation", 19, "https://proxy"},
		{"binding endpoint", 20, "127.0.0.1:3128"}, {"binding leading zero", 20, "017"}, {"binding overflow", 20, "9223372036854775808"}, {"zero revision", 21, int64(0)},
		{"zero vault creation", 22, time.Time{}}, {"zero vault update", 23, time.Time{}}, {"zero version creation", 24, time.Time{}},
		{"zero proxy creation", 25, time.Time{}}, {"zero proxy update", 26, time.Time{}}, {"zero reservation creation", 27, time.Time{}}, {"zero reservation update", 28, time.Time{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, mock, now := readinessSQLFixture(t)
			row := readinessRow(now)
			row[test.index] = test.value
			expectBindingRead(mock, now).WillReturnRows(sqlmock.NewRows(readinessColumns).AddRow(row...))
			got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireReadinessDenied(t, got, err)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for index := 22; index < len(readinessColumns); index++ {
		t.Run("future "+readinessColumns[index], func(t *testing.T) {
			r, mock, now := readinessSQLFixture(t)
			row := readinessRow(now)
			row[index] = now.Add(time.Nanosecond)
			expectBindingRead(mock, now).WillReturnRows(sqlmock.NewRows(readinessColumns).AddRow(row...))
			got, err := r.ReadWorkerReadinessBinding(context.Background(), "slot-1", now, 45*time.Second)
			requireReadinessDenied(t, got, err)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadWorkerReadinessBindingSQLFailureHasNoPartialResult(t *testing.T) {
	for _, failure := range []string{"storage", "missing", "cancellation"} {
		t.Run(failure, func(t *testing.T) {
			r, mock, now := readinessSQLFixture(t)
			query := expectBindingRead(mock, now)
			ctx := context.Background()
			switch failure {
			case "storage":
				query.WillReturnError(errors.New("synthetic private storage detail"))
			case "missing":
				query.WillReturnRows(sqlmock.NewRows(readinessColumns))
			case "cancellation":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 5*time.Millisecond)
				defer cancel()
				query.WillDelayFor(time.Second).WillReturnRows(sqlmock.NewRows(readinessColumns).AddRow(readinessRow(now)...))
			}
			got, err := r.ReadWorkerReadinessBinding(ctx, "slot-1", now, 45*time.Second)
			requireReadinessDenied(t, got, err)
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReadWorkerReadinessBindingInvalidInputsDoNotQuery(t *testing.T) {
	r, mock, now := readinessSQLFixture(t)
	memory, _ := workerReadinessFixture(t)
	for _, impl := range []WorkerReadinessBindingRepository{r, memory, (*Repository)(nil), (*MemoryRepository)(nil), &Repository{}} {
		for _, test := range []struct {
			ctx  context.Context
			slot string
			at   time.Time
			age  time.Duration
		}{
			{nil, "slot-1", now, time.Second}, {context.Background(), "", now, time.Second},
			{context.Background(), "slot-1 ", now, time.Second}, {context.Background(), "slot-1", time.Time{}, time.Second},
			{context.Background(), "slot-1", now, 0}, {context.Background(), "slot-1", now, -time.Second},
			{context.Background(), "slot-1", now, 45*time.Second + time.Nanosecond},
		} {
			got, err := impl.ReadWorkerReadinessBinding(test.ctx, test.slot, test.at, test.age)
			requireReadinessDenied(t, got, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := impl.ReadWorkerReadinessBinding(ctx, "slot-1", now, time.Second)
		requireReadinessDenied(t, got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
