package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRuntimeEnrollmentBindingDoesNotRequireWorkerOrProvider(t *testing.T) {
	r, now := executionBindingFixture(t)
	r.mu.Lock()
	a := &r.assignments["slot-1"][0]
	a.ProviderRef = ""
	a.Healthy = false
	a.ActualState = "missing"
	a.LastObservedAt = nil
	a.ObservedControlSessionID = ""
	r.mu.Unlock()
	g, err := r.ReadRuntimeEnrollmentBinding(context.Background(), "slot-1", now, 45*time.Second)
	if err != nil || g.AssignmentID == "" || g.NodeID != "srv74" || g.ControlSessionID != bindingSessionOne || g.RuntimeBinding().Validate() != nil {
		t.Fatalf("bootstrap grant rejected: %+v / %v", g, err)
	}
	if _, err := r.ReadProbeBinding(context.Background(), "slot-1", now, 45*time.Second); err == nil {
		t.Fatal("fixture must distinguish old provider-dependent gate")
	}
}

func TestRuntimeEnrollmentBindingRejectsChangedAuthority(t *testing.T) {
	for name, change := range map[string]func(*Slot, *Assignment, *Node, *ExecutionLease, time.Time){
		"desired-state": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.DesiredState = "absent" },
		"generation":    func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { s.DesiredGeneration++ },
		"image": func(s *Slot, _ *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) {
			s.ImageDigest = "sha256:" + strings.Repeat("b", 64)
		},
		"released":          func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, n time.Time) { a.ReleasedAt = &n },
		"assignment-id":     func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, _ time.Time) { a.ID = "" },
		"node-disconnected": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.Status = "disconnected" },
		"session-format":    func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, _ time.Time) { n.ControlSessionID = "legacy" },
		"stale-node": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, now time.Time) {
			v := now.Add(-45 * time.Second)
			n.LastSeenAt = &v
		},
		"future-node": func(_ *Slot, _ *Assignment, n *Node, _ *ExecutionLease, now time.Time) {
			v := now.Add(time.Second)
			n.LastSeenAt = &v
		},
		"lease-expired": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, n time.Time) { l.ExpiresAt = n },
		"lease-revoked": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, n time.Time) { l.RevokedAt = &n },
		"lease-owner":   func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.OwnerID = "" },
		"lease-node":    func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, _ time.Time) { l.NodeID = "other-node" },
		"future-assignment": func(_ *Slot, a *Assignment, _ *Node, _ *ExecutionLease, n time.Time) {
			a.AssignedAt = n.Add(time.Second)
		},
		"future-lease": func(_ *Slot, _ *Assignment, _ *Node, l *ExecutionLease, n time.Time) {
			l.UpdatedAt = n.Add(time.Second)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, now := executionBindingFixture(t)
			s := r.slots["slot-1"]
			a := r.assignments["slot-1"][0]
			n := r.nodes[a.NodeID]
			key := executionLeaseKey(a.SlotID, a.ExecutionEpoch)
			l := r.executionLeases[key]
			change(&s, &a, &n, &l, now)
			r.slots[s.ID] = s
			r.assignments["slot-1"][0] = a
			r.nodes[n.ID] = n
			r.executionLeases[key] = l
			if got, err := r.ReadRuntimeEnrollmentBinding(context.Background(), "slot-1", now, 45*time.Second); err != ErrRuntimeEnrollmentBindingUnavailable || got.AssignmentID != "" {
				t.Fatal("changed authority admitted")
			}
		})
	}
	r, now := executionBindingFixture(t)
	r.assignments["slot-1"] = append(r.assignments["slot-1"], r.assignments["slot-1"][0])
	if _, err := r.ReadRuntimeEnrollmentBinding(context.Background(), "slot-1", now, 45*time.Second); err != ErrRuntimeEnrollmentBindingUnavailable {
		t.Fatal("multiple active assignments admitted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ReadRuntimeEnrollmentBinding(ctx, "slot-1", now, 45*time.Second); err != ErrRuntimeEnrollmentBindingUnavailable {
		t.Fatal("cancelled read admitted")
	}
}

func TestRuntimeEnrollmentBindingSQLProjection(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "db-error", "wrong-slot", "stale"} {
		t.Run(mode, func(t *testing.T) {
			memory, now := executionBindingFixture(t)
			g, err := memory.ReadRuntimeEnrollmentBinding(context.Background(), "slot-1", now, 45*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repo, err := NewRepository(db)
			if err != nil {
				t.Fatal(err)
			}
			q := mock.ExpectQuery(regexp.QuoteMeta(runtimeEnrollmentBindingSQL)).WithArgs("slot-1", now.Add(-45*time.Second), now, now, now, now, now)
			if mode == "missing" {
				q.WillReturnError(sql.ErrNoRows)
			} else if mode == "db-error" {
				q.WillReturnError(errors.New("synthetic diagnostic"))
			} else {
				if mode == "wrong-slot" {
					g.SlotID = "other"
				}
				if mode == "stale" {
					g.NodeSeenAt = now.Add(-45 * time.Second)
				}
				q.WillReturnRows(sqlmock.NewRows([]string{"assignment", "account", "slot", "node", "image", "session", "owner", "epoch", "generation", "seen", "expiry", "assigned", "created", "updated"}).AddRow(g.AssignmentID, g.AccountID, g.SlotID, g.NodeID, g.ImageDigest, g.ControlSessionID, g.LeaseOwnerID, g.Epoch, g.Generation, g.NodeSeenAt, g.LeaseExpiresAt, now.Add(-time.Minute), now.Add(-time.Minute), now.Add(-time.Second)))
			}
			got, err := repo.ReadRuntimeEnrollmentBinding(context.Background(), "slot-1", now, 45*time.Second)
			if mode == "valid" {
				if err != nil || got != g {
					t.Fatal("projection differs from memory", err)
				}
			} else if err != ErrRuntimeEnrollmentBindingUnavailable || got.AssignmentID != "" {
				t.Fatal("unsafe SQL result admitted")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, forbidden := range []string{"provider_ref", "healthy", "observed", "actual_state"} {
		if strings.Contains(runtimeEnrollmentBindingSQL, forbidden) {
			t.Fatal("bootstrap again depends on worker readiness")
		}
	}
}

func TestRuntimeCertificateMigrationPinsAssignmentAndSlotEpoch(t *testing.T) {
	up, _ := Migrations("up")
	down, _ := Migrations("down")
	var schema string
	for _, m := range up {
		if m.Name == "014_runtime_certificates.up.sql" {
			schema = m.SQL
		}
	}
	for _, part := range []string{"PRIMARY KEY (assignment_id)", "UNIQUE KEY uq_runtime_certificates_slot_epoch (slot_id, execution_epoch)", "public_key_sha256 BINARY(32)", "ca_sha256 BINARY(32)", "certificate_pem VARBINARY(16384)", "expires_at DATETIME(6)"} {
		if !strings.Contains(schema, part) {
			t.Fatal("missing receipt boundary", part)
		}
	}
	for _, part := range []string{"private_key", "csr_pem", "credential", "token"} {
		if strings.Contains(schema, part) {
			t.Fatal("secret field in public receipt schema")
		}
	}
	if down[len(down)-1].Name != "014_runtime_certificates.down.sql" {
		t.Fatal("missing down migration")
	}
}
