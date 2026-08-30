package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type runtimeProxyReservationFixture struct {
	app         *app
	accountID   int64
	proxyID     int64
	submission  runtimeOnboardingSubmission
	receipt     runtimeOnboardingIntentReceipt
	onboarding  runtimeOutboxEvent
	reservation runtimeProxyReservation
}

func TestRuntimeProxyReservationSchemaStaysStrictAcrossDialects(t *testing.T) {
	for dialect, schema := range map[string]string{
		"sqlite": strings.Join(sqliteExecutionSchema(), "\n"),
		"mysql":  strings.Join(mysqlExecutionSchema(), "\n"),
	} {
		for _, fragment := range []string{
			"CREATE TABLE IF NOT EXISTS runtime_proxy_reservations",
			"reservation_id",
			"account_id",
			"proxy_id",
			"desired_generation",
			"binding_revision",
			"grant_event_id",
			"revoke_event_id",
			"status",
			"revoked_at",
			"created_at",
			"updated_at",
		} {
			if !strings.Contains(schema, fragment) {
				t.Fatalf("%s runtime proxy reservation schema is missing %q", dialect, fragment)
			}
		}
	}
	sqliteSchema := strings.Join(sqliteExecutionSchema(), "\n")
	if !strings.Contains(sqliteSchema, "UNIQUE (account_id, desired_generation)") ||
		!strings.Contains(sqliteSchema, "revoke_event_id TEXT UNIQUE") ||
		!strings.Contains(sqliteSchema, "account_id INTEGER NOT NULL REFERENCES accounts(id),\n\t\t\tproxy_id INTEGER NOT NULL REFERENCES proxies(id)") ||
		!strings.Contains(sqliteSchema, "status IN ('active', 'revoked')") {
		t.Fatal("SQLite runtime proxy reservation schema lacks generation uniqueness or lifecycle constraint")
	}
	mysqlSchema := strings.Join(mysqlExecutionSchema(), "\n")
	for _, fragment := range []string{
		"reservation_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY",
		"UNIQUE KEY uq_runtime_proxy_reservation_account_generation (account_id, desired_generation)",
		"UNIQUE KEY uq_runtime_proxy_reservation_grant_event (grant_event_id)",
		"UNIQUE KEY uq_runtime_proxy_reservation_revoke_event (revoke_event_id)",
	} {
		if !strings.Contains(mysqlSchema, fragment) {
			t.Fatalf("MySQL runtime proxy reservation schema is missing %q", fragment)
		}
	}
}

func TestMigrateExecutionFeaturesCreatesRuntimeProxyReservations(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "proxy-reservation-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	if _, err := a.db.Exec(`DROP TABLE runtime_proxy_reservations`); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateExecutionFeatures(); err != nil {
		t.Fatal(err)
	}
	var table string
	if err := a.db.QueryRow(`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'runtime_proxy_reservations'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
	if table != "runtime_proxy_reservations" {
		t.Fatalf("migrated table = %q", table)
	}
}

func TestMigrateExecutionFeaturesRemovesLegacyReservationAccountCascade(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "proxy-reservation-fk-migration.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	if _, err := a.db.Exec(`DROP TABLE runtime_proxy_reservations`); err != nil {
		t.Fatal(err)
	}
	var currentDDL string
	for _, statement := range sqliteExecutionSchema() {
		if strings.Contains(statement, "CREATE TABLE IF NOT EXISTS runtime_proxy_reservations") {
			currentDDL = statement
			break
		}
	}
	legacyDDL := strings.Replace(currentDDL,
		"account_id INTEGER NOT NULL REFERENCES accounts(id)",
		"account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE", 1)
	if legacyDDL == currentDDL || currentDDL == "" {
		t.Fatal("could not construct legacy reservation schema")
	}
	if _, err := a.db.Exec(legacyDDL); err != nil {
		t.Fatal(err)
	}
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('legacy-fk-account', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	reservationID := newRuntimeProxyReservationID()
	if _, err := a.db.Exec(`INSERT INTO runtime_proxy_reservations
		(reservation_id, account_id, proxy_id, desired_generation, binding_revision, grant_event_id, status)
		VALUES (?, ?, ?, 1, 1, ?, 'active')`, reservationID, accountID, proxyID, newRuntimeEventID()); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateExecutionFeatures(); err != nil {
		t.Fatal(err)
	}
	rows, err := a.db.Query(`PRAGMA foreign_key_list(runtime_proxy_reservations)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	foundAccountFK := false
	for rows.Next() {
		var id, sequence int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &sequence, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			t.Fatal(err)
		}
		if table == "accounts" && from == "account_id" {
			foundAccountFK = true
			if strings.EqualFold(onDelete, "CASCADE") {
				t.Fatalf("legacy account FK still cascades: %s", onDelete)
			}
		}
	}
	var reservations int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_proxy_reservations WHERE reservation_id = ?`, reservationID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if !foundAccountFK || reservations != 1 {
		t.Fatalf("migrated account FK/data = found=%v reservations=%d", foundAccountFK, reservations)
	}
}

func TestSQLiteRuntimeProxyReservationRevokeEventIsUniqueAndNullable(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "proxy-reservation-revoke-unique.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyResult, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, status)
		VALUES (1, 'revoke-unique-proxy', 'http', '127.0.0.1', 10380, 'active')`)
	if err != nil {
		t.Fatal(err)
	}
	proxyID, _ := proxyResult.LastInsertId()
	firstAccount, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('revoke-unique-a', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	secondAccount, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('revoke-unique-b', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	firstAccountID, _ := firstAccount.LastInsertId()
	secondAccountID, _ := secondAccount.LastInsertId()
	revokeEventID := newRuntimeEventID()
	if _, err := a.db.Exec(`INSERT INTO runtime_proxy_reservations
		(reservation_id, account_id, proxy_id, desired_generation, binding_revision, grant_event_id,
		 revoke_event_id, status, revoked_at)
		VALUES (?, ?, ?, 1, 1, ?, ?, 'revoked', `+nowSQL+`)`,
		newRuntimeProxyReservationID(), firstAccountID, proxyID, newRuntimeEventID(), revokeEventID); err != nil {
		t.Fatal(err)
	}
	secondReservationID := newRuntimeProxyReservationID()
	if _, err := a.db.Exec(`INSERT INTO runtime_proxy_reservations
		(reservation_id, account_id, proxy_id, desired_generation, binding_revision, grant_event_id, status)
		VALUES (?, ?, ?, 1, 1, ?, 'active')`,
		secondReservationID, secondAccountID, proxyID, newRuntimeEventID()); err != nil {
		t.Fatalf("multiple active reservations could not retain NULL revoke ids: %v", err)
	}
	if _, err := a.db.Exec(`UPDATE runtime_proxy_reservations SET status = 'revoked', revoke_event_id = ?,
		revoked_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE reservation_id = ?`, revokeEventID, secondReservationID); err == nil {
		t.Fatal("duplicate durable revoke event id was accepted")
	}
}

func TestRuntimeProxyReservationPayloadRejectsDuplicateUnknownMissingAndTrailingFields(t *testing.T) {
	valid := `{"reservation_id":"rpr-00000000-0000-0000-0000-000000000001","proxy_binding_id":"42","binding_revision":7}`
	decoded, err := decodeRuntimeProxyReservationPayload(valid)
	if err != nil || decoded.ReservationID != "rpr-00000000-0000-0000-0000-000000000001" ||
		decoded.ProxyBindingID != "42" || decoded.BindingRevision != 7 {
		t.Fatalf("valid authority payload = %+v, err=%v", decoded, err)
	}
	for _, test := range []struct {
		name    string
		payload string
	}{
		{name: "duplicate", payload: `{"reservation_id":"rpr-00000000-0000-0000-0000-000000000001","reservation_id":"rpr-00000000-0000-0000-0000-000000000002","proxy_binding_id":"42","binding_revision":7}`},
		{name: "unknown", payload: `{"reservation_id":"rpr-00000000-0000-0000-0000-000000000001","proxy_binding_id":"42","binding_revision":7,"proxy_url":"http://forbidden"}`},
		{name: "missing", payload: `{"reservation_id":"rpr-00000000-0000-0000-0000-000000000001","proxy_binding_id":"42"}`},
		{name: "trailing", payload: valid + ` {}`},
		{name: "secret-looking id", payload: `{"reservation_id":"sk-ant-forbidden","proxy_binding_id":"42","binding_revision":7}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeRuntimeProxyReservationPayload(test.payload); err == nil {
				t.Fatalf("non-exact authority payload was accepted: %s", test.payload)
			}
		})
	}
}

func newRuntimeProxyReservationFixture(t *testing.T, name string) runtimeProxyReservationFixture {
	t.Helper()
	a, err := newApp(filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Close() })
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES (?, '{}')`, name)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	seedRuntimeOnboardingSubmission(t, a, name+"-key", accountID, 1,
		"account.runtime.provision_requested", "migrating", "session_key", "oauth")
	submission, err := a.getRuntimeOnboardingSubmission(context.Background(), name+"-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE proxies SET host = 'proxy.internal.invalid', username = 'proxy-user',
		password = 'proxy-password-10380' WHERE id = ?`, submission.ProxyID.Int64); err != nil {
		t.Fatal(err)
	}
	receipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey: submission.IntakeIdempotencyKey, IntentID: name + "-intent",
		AccountID: accountID, DesiredGeneration: 1, ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	submission, err = a.persistRuntimeOnboardingReceipt(context.Background(), name+"-key", submission, receipt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := a.requestRuntimeOnboardingTransition(context.Background(), runtimeTransitionRequest{
		AccountID: accountID, EventType: submission.EventType, MigrationStatus: submission.MigrationStatus,
		RuntimeStatus: "provisioning", OnboardingKey: submission.IdempotencyKey,
	}, receipt)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := a.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := loadRuntimeProxyReservationByGenerationTx(context.Background(), tx, accountID, 1, false)
	_ = tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	return runtimeProxyReservationFixture{
		app: a, accountID: accountID, proxyID: submission.ProxyID.Int64, submission: submission,
		receipt: receipt, onboarding: onboarding, reservation: reservation,
	}
}

func TestRuntimeOnboardingGrantsProxyReservationBeforeIntentInSameTransaction(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-grant-order")
	rows, err := fixture.app.db.Query(`SELECT event_id, event_type, desired_generation, payload_json
		FROM runtime_outbox WHERE account_id = ? ORDER BY sequence`, fixture.accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []runtimeOutboxEvent
	for rows.Next() {
		var event runtimeOutboxEvent
		if err := rows.Scan(&event.EventID, &event.EventType, &event.DesiredGeneration, &event.PayloadJSON); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].EventType != runtimeProxyReservationGranted ||
		events[1].EventType != fixture.submission.EventType || events[1].EventID != fixture.onboarding.EventID {
		t.Fatalf("ordered runtime events = %+v", events)
	}
	expectedGrantPayload := fmt.Sprintf(`{"binding_revision":1,"proxy_binding_id":%q,"reservation_id":%q}`,
		strconv.FormatInt(fixture.proxyID, 10), fixture.reservation.ReservationID)
	if events[0].EventID != fixture.reservation.GrantEventID || events[0].DesiredGeneration != 1 ||
		events[0].PayloadJSON != expectedGrantPayload ||
		events[1].PayloadJSON != fmt.Sprintf(`{"onboarding_intent_id":%q}`, fixture.receipt.IntentID) {
		t.Fatalf("grant/onboarding payloads = %+v", events)
	}
	allPayloads := events[0].PayloadJSON + events[1].PayloadJSON
	for _, forbidden := range []string{"proxy.internal.invalid", "proxy-user", "proxy-password-10380", "http://", "https://", "socks5://"} {
		if strings.Contains(allPayloads, forbidden) {
			t.Fatalf("runtime outbox leaked proxy material %q: %s", forbidden, allPayloads)
		}
	}
	if fixture.reservation.ProxyID != fixture.proxyID || fixture.reservation.BindingRevision != 1 ||
		fixture.reservation.Status != runtimeProxyReservationActive || fixture.reservation.RevokeEventID.Valid {
		t.Fatalf("stored proxy reservation = %+v", fixture.reservation)
	}
}

func TestRuntimeOnboardingRollbackLeavesNoProxyGrant(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "proxy-grant-rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('proxy-grant-rollback', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	seedRuntimeOnboardingSubmission(t, a, "proxy-grant-rollback-key", accountID, 1,
		"account.runtime.provision_requested", "migrating", "session_key", "oauth")
	submission, err := a.getRuntimeOnboardingSubmission(context.Background(), "proxy-grant-rollback-key")
	if err != nil {
		t.Fatal(err)
	}
	receipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey: submission.IntakeIdempotencyKey, IntentID: "proxy-grant-rollback-intent",
		AccountID: accountID, DesiredGeneration: 1, ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	submission, err = a.persistRuntimeOnboardingReceipt(context.Background(), submission.IdempotencyKey, submission, receipt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.requestRuntimeTransition(context.Background(), runtimeTransitionRequest{
		AccountID: accountID, EventType: submission.EventType, MigrationStatus: submission.MigrationStatus,
		RuntimeStatus: "provisioning", ExpectedGeneration: 1, ExpectedProxyID: submission.ProxyID.Int64,
		OnboardingKey: submission.IdempotencyKey, OnboardingIntakeKey: submission.IntakeIdempotencyKey,
		OnboardingAttempt: submission.IntakeAttempt + 1, OnboardingIntentID: receipt.IntentID,
		OnboardingExpiresAt: receipt.ExpiresAt, Payload: map[string]any{"onboarding_intent_id": receipt.IntentID},
	})
	if !errors.Is(err, errRuntimeMigration) {
		t.Fatalf("mismatched onboarding CAS error = %v", err)
	}
	var generation uint64
	var reservations, events int
	if err := a.db.QueryRow(`SELECT runtime_generation FROM accounts WHERE id = ?`, accountID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_proxy_reservations WHERE account_id = ?`, accountID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?`, accountID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if generation != 0 || reservations != 0 || events != 0 {
		t.Fatalf("rolled back generation/reservations/events = %d/%d/%d", generation, reservations, events)
	}
}

func TestRuntimeProxyReservationExactGrantReplayDoesNotDuplicate(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-grant-replay")
	tx, err := fixture.app.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	var generation uint64
	if err := tx.QueryRowContext(context.Background(), `SELECT runtime_generation FROM accounts WHERE id = ?`, fixture.accountID).Scan(&generation); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if generation != fixture.reservation.DesiredGeneration {
		_ = tx.Rollback()
		t.Fatalf("grant replay account generation = %d", generation)
	}
	replayed, grant, err := ensureRuntimeProxyReservationTx(
		context.Background(), tx, fixture.accountID, fixture.proxyID, fixture.reservation.DesiredGeneration,
	)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if replayed != fixture.reservation || grant.EventID != fixture.reservation.GrantEventID {
		t.Fatalf("grant replay = %+v/%+v, want %+v", replayed, grant, fixture.reservation)
	}
	var reservations, grants int
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_proxy_reservations WHERE account_id = ?`, fixture.accountID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = ?`,
		fixture.accountID, runtimeProxyReservationGranted).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 || grants != 1 {
		t.Fatalf("reservation/grant replay counts = %d/%d", reservations, grants)
	}
}

func TestRuntimeProxyReservationRevokeAndReplayAreExact(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-revoke")
	if _, err := fixture.app.requestRuntimeTransition(context.Background(), runtimeTransitionRequest{
		AccountID: fixture.accountID, EventType: "account.runtime.drain_requested",
		MigrationStatus: "migrating", RuntimeStatus: "draining", Payload: map[string]any{"reason_code": "test"},
	}); err != nil {
		t.Fatal(err)
	}
	first, err := fixture.app.revokeRuntimeProxyReservation(context.Background(), fixture.reservation.ReservationID,
		fixture.accountID, fixture.proxyID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.app.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	newer, _, err := ensureRuntimeProxyReservationTx(context.Background(), tx, fixture.accountID, fixture.proxyID, 2)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	replay, err := fixture.app.revokeRuntimeProxyReservation(context.Background(), fixture.reservation.ReservationID,
		fixture.accountID, fixture.proxyID, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if first.EventID == "" || replay.EventID != first.EventID || first.EventType != runtimeProxyReservationRevoke ||
		first.DesiredGeneration != 1 {
		t.Fatalf("revoke/replay events = %+v/%+v", first, replay)
	}
	expectedPayload := fmt.Sprintf(`{"binding_revision":1,"proxy_binding_id":%q,"reservation_id":%q}`,
		strconv.FormatInt(fixture.proxyID, 10), fixture.reservation.ReservationID)
	if first.PayloadJSON != expectedPayload || replay.PayloadJSON != expectedPayload {
		t.Fatalf("revoke/replay payloads = %s/%s", first.PayloadJSON, replay.PayloadJSON)
	}
	var oldStatus, oldRevokeID, newerStatus string
	var newerRevokeID sql.NullString
	if err := fixture.app.db.QueryRow(`SELECT status, revoke_event_id FROM runtime_proxy_reservations WHERE reservation_id = ?`,
		fixture.reservation.ReservationID).Scan(&oldStatus, &oldRevokeID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.QueryRow(`SELECT status, revoke_event_id FROM runtime_proxy_reservations WHERE reservation_id = ?`,
		newer.ReservationID).Scan(&newerStatus, &newerRevokeID); err != nil {
		t.Fatal(err)
	}
	var revokes int
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE event_type = ? AND account_id = ?`,
		runtimeProxyReservationRevoke, fixture.accountID).Scan(&revokes); err != nil {
		t.Fatal(err)
	}
	if oldStatus != runtimeProxyReservationRevoked || oldRevokeID != first.EventID ||
		newerStatus != runtimeProxyReservationActive || newerRevokeID.Valid || revokes != 1 {
		t.Fatalf("old/new reservation and revoke count = %s/%s, %s/%v, %d", oldStatus, oldRevokeID, newerStatus, newerRevokeID, revokes)
	}
}

func TestRuntimeProxyReservationRevokeMismatchFailsClosed(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-revoke-mismatch")
	for _, test := range []struct {
		name          string
		reservationID string
		proxyID       int64
	}{
		{name: "proxy", reservationID: fixture.reservation.ReservationID, proxyID: fixture.proxyID + 1},
		{name: "secret-looking id", reservationID: "sk-ant-proxy-reservation", proxyID: fixture.proxyID},
		{name: "overlong id", reservationID: strings.Repeat("r", 129), proxyID: fixture.proxyID},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := fixture.app.revokeRuntimeProxyReservation(context.Background(), test.reservationID,
				fixture.accountID, test.proxyID, fixture.reservation.DesiredGeneration, fixture.reservation.BindingRevision)
			if !errors.Is(err, errRuntimeMigration) {
				t.Fatalf("mismatched revoke error = %v", err)
			}
		})
	}
	var status string
	var revokeEventID sql.NullString
	var revokedAt sql.NullString
	if err := fixture.app.db.QueryRow(`SELECT status, revoke_event_id, revoked_at FROM runtime_proxy_reservations WHERE reservation_id = ?`,
		fixture.reservation.ReservationID).Scan(&status, &revokeEventID, &revokedAt); err != nil {
		t.Fatal(err)
	}
	var revokes int
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE event_type = ? AND account_id = ?`,
		runtimeProxyReservationRevoke, fixture.accountID).Scan(&revokes); err != nil {
		t.Fatal(err)
	}
	if status != runtimeProxyReservationActive || revokeEventID.Valid || revokedAt.Valid || revokes != 0 {
		t.Fatalf("mismatched revoke mutated reservation = %s/%v/%v, events=%d", status, revokeEventID, revokedAt, revokes)
	}
}

func TestProxyAuthorityEventsCannotUseAccountTransitionAPI(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-authority-transition")
	var beforeEvents int
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?`, fixture.accountID).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	for _, eventType := range []string{runtimeProxyReservationGranted, runtimeProxyReservationRevoke} {
		if _, err := fixture.app.requestRuntimeTransition(context.Background(), runtimeTransitionRequest{
			AccountID: fixture.accountID, EventType: eventType, MigrationStatus: "migrating",
			RuntimeStatus: "provisioning", Payload: map[string]any{},
		}); err == nil {
			t.Fatalf("authority event %q entered account transition API", eventType)
		}
	}
	var generation uint64
	var afterEvents int
	if err := fixture.app.db.QueryRow(`SELECT runtime_generation FROM accounts WHERE id = ?`, fixture.accountID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?`, fixture.accountID).Scan(&afterEvents); err != nil {
		t.Fatal(err)
	}
	if generation != 1 || afterEvents != beforeEvents {
		t.Fatalf("authority transition changed generation/events = %d/%d, before=%d", generation, afterEvents, beforeEvents)
	}
}

func TestRuntimeProxyReservationSupersessionOrdersRevokeGrantAndOnboarding(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-supersession")
	tx, err := fixture.app.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	candidate := runtimeOnboardingSubmission{
		IdempotencyKey: "proxy-supersession-key-2", OperationType: runtimeOnboardingOperationReauthorize,
		AccountID: fixture.accountID, DesiredGeneration: 2, EventType: "account.credential.rotate_requested",
		MigrationStatus: "migrating", SourceType: "session_key", AuthType: "oauth",
		ProxyID: sql.NullInt64{Int64: fixture.proxyID, Valid: true}, Status: runtimeOnboardingSubmissionPending,
	}
	created, err := insertRuntimeOnboardingSubmissionTx(context.Background(), tx, candidate)
	if err != nil || !created {
		_ = tx.Rollback()
		t.Fatalf("insert successor submission: created=%v err=%v", created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	submission, err := fixture.app.getRuntimeOnboardingSubmission(context.Background(), candidate.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	receipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey: submission.IntakeIdempotencyKey, IntentID: "proxy-supersession-intent-2",
		AccountID: fixture.accountID, DesiredGeneration: 2, ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	submission, err = fixture.app.persistRuntimeOnboardingReceipt(context.Background(), submission.IdempotencyKey, submission, receipt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := fixture.app.requestRuntimeOnboardingTransition(context.Background(), runtimeTransitionRequest{
		AccountID: fixture.accountID, EventType: submission.EventType, MigrationStatus: submission.MigrationStatus,
		RuntimeStatus: "provisioning", OnboardingKey: submission.IdempotencyKey,
	}, receipt)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := fixture.app.db.Query(`SELECT event_type, desired_generation FROM runtime_outbox
		WHERE account_id = ? ORDER BY sequence`, fixture.accountID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type eventIdentity struct {
		eventType  string
		generation uint64
	}
	events := make([]eventIdentity, 0, 5)
	for rows.Next() {
		var event eventIdentity
		if err := rows.Scan(&event.eventType, &event.generation); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []eventIdentity{
		{runtimeProxyReservationGranted, 1},
		{"account.runtime.provision_requested", 1},
		{runtimeProxyReservationRevoke, 1},
		{runtimeProxyReservationGranted, 2},
		{"account.credential.rotate_requested", 2},
	}
	if fmt.Sprint(events) != fmt.Sprint(want) || onboarding.DesiredGeneration != 2 {
		t.Fatalf("supersession events = %+v, onboarding=%+v", events, onboarding)
	}
	var oldStatus, newStatus string
	if err := fixture.app.db.QueryRow(`SELECT status FROM runtime_proxy_reservations
		WHERE reservation_id = ?`, fixture.reservation.ReservationID).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.QueryRow(`SELECT status FROM runtime_proxy_reservations
		WHERE account_id = ? AND desired_generation = 2`, fixture.accountID).Scan(&newStatus); err != nil {
		t.Fatal(err)
	}
	if oldStatus != runtimeProxyReservationRevoked || newStatus != runtimeProxyReservationActive {
		t.Fatalf("supersession statuses = %q/%q", oldStatus, newStatus)
	}
}

func TestRuntimeProxyReservationOrdinaryTransitionRetainsGrant(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-non-onboarding-retains")
	if _, err := fixture.app.requestRuntimeTransition(context.Background(), runtimeTransitionRequest{
		AccountID: fixture.accountID, EventType: "account.runtime.drain_requested",
		MigrationStatus: "migrating", RuntimeStatus: "draining", Payload: map[string]any{"reason_code": "test"},
	}); err != nil {
		t.Fatal(err)
	}
	var status string
	var revokeEventID sql.NullString
	if err := fixture.app.db.QueryRow(`SELECT status, revoke_event_id FROM runtime_proxy_reservations
		WHERE reservation_id = ?`, fixture.reservation.ReservationID).Scan(&status, &revokeEventID); err != nil {
		t.Fatal(err)
	}
	var revokes int
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = ?`,
		fixture.accountID, runtimeProxyReservationRevoke).Scan(&revokes); err != nil {
		t.Fatal(err)
	}
	if status != runtimeProxyReservationActive || revokeEventID.Valid || revokes != 0 {
		t.Fatalf("ordinary transition revoked business reservation: %s/%v events=%d", status, revokeEventID, revokes)
	}
}

func TestRuntimeProxyReservationBlocksIdentityBearingProxyMutations(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, runtimeProxyReservationFixture)
	}{
		{
			name: "proxy update",
			mutate: func(t *testing.T, fixture runtimeProxyReservationFixture) {
				putJSON(t, fixture.app.routes(), http.MethodPut, "/api/proxies/"+strconv.FormatInt(fixture.proxyID, 10), map[string]any{
					"name": "mutated", "status": "disabled", "username": "other-user", "password": "other-password",
				}, http.StatusConflict, nil)
			},
		},
		{
			name: "pool protocol update",
			mutate: func(t *testing.T, fixture runtimeProxyReservationFixture) {
				var poolID int64
				if err := fixture.app.db.QueryRow(`SELECT pool_id FROM proxies WHERE id = ?`, fixture.proxyID).Scan(&poolID); err != nil {
					t.Fatal(err)
				}
				putJSON(t, fixture.app.routes(), http.MethodPut, "/api/proxy-pools/"+strconv.FormatInt(poolID, 10), map[string]any{
					"name": "default", "source_type": "manual", "api_headers": "{}",
					"default_protocol": "https", "status": "active", "single_use_enabled": true,
				}, http.StatusConflict, nil)
			},
		},
		{
			name: "proxy delete",
			mutate: func(t *testing.T, fixture runtimeProxyReservationFixture) {
				putJSON(t, fixture.app.routes(), http.MethodDelete, "/api/proxies/"+strconv.FormatInt(fixture.proxyID, 10), nil, http.StatusConflict, nil)
			},
		},
		{
			name: "pool delete",
			mutate: func(t *testing.T, fixture runtimeProxyReservationFixture) {
				var poolID int64
				if err := fixture.app.db.QueryRow(`SELECT pool_id FROM proxies WHERE id = ?`, fixture.proxyID).Scan(&poolID); err != nil {
					t.Fatal(err)
				}
				putJSON(t, fixture.app.routes(), http.MethodDelete, "/api/proxy-pools/"+strconv.FormatInt(poolID, 10), nil, http.StatusConflict, nil)
			},
		},
		{
			name: "proxy restore",
			mutate: func(t *testing.T, fixture runtimeProxyReservationFixture) {
				if _, err := fixture.app.db.Exec(`UPDATE proxies SET status = 'disabled', deleted_at = `+nowSQL+` WHERE id = ?`, fixture.proxyID); err != nil {
					t.Fatal(err)
				}
				putJSON(t, fixture.app.routes(), http.MethodPost, "/api/proxies/"+strconv.FormatInt(fixture.proxyID, 10)+"/restore",
					map[string]any{"pool_id": 1}, http.StatusConflict, nil)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			fixture := newRuntimeProxyReservationFixture(t, "proxy-mutation-"+strings.ReplaceAll(test.name, " ", "-"))
			var beforeProtocol, beforeHost, beforeUsername, beforePassword, beforeStatus string
			var beforeDeleted sql.NullString
			if err := fixture.app.db.QueryRow(`SELECT protocol, host, username, password, status, deleted_at
				FROM proxies WHERE id = ?`, fixture.proxyID).Scan(
				&beforeProtocol, &beforeHost, &beforeUsername, &beforePassword, &beforeStatus, &beforeDeleted,
			); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, fixture)
			var protocol, host, username, password, status string
			var deleted sql.NullString
			if err := fixture.app.db.QueryRow(`SELECT protocol, host, username, password, status, deleted_at
				FROM proxies WHERE id = ?`, fixture.proxyID).Scan(&protocol, &host, &username, &password, &status, &deleted); err != nil {
				t.Fatal(err)
			}
			if test.name != "proxy restore" && (protocol != beforeProtocol || host != beforeHost || username != beforeUsername ||
				password != beforePassword || status != beforeStatus || deleted != beforeDeleted) {
				t.Fatalf("blocked mutation changed proxy: %q/%q/%q/%q/%q/%v", protocol, host, username, password, status, deleted)
			}
			if test.name == "proxy restore" && (status != "disabled" || !deleted.Valid) {
				t.Fatalf("blocked restore revived proxy: status=%q deleted=%v", status, deleted)
			}
			var reservationStatus string
			if err := fixture.app.db.QueryRow(`SELECT status FROM runtime_proxy_reservations WHERE reservation_id = ?`,
				fixture.reservation.ReservationID).Scan(&reservationStatus); err != nil {
				t.Fatal(err)
			}
			if reservationStatus != runtimeProxyReservationActive {
				t.Fatalf("blocked mutation changed reservation status = %q", reservationStatus)
			}
		})
	}
}

func TestPendingRuntimeAccountBlocksProxyMutationBeforeGrant(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "pending-runtime-proxy-guard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, credentials_json, execution_migration_status, runtime_status, proxy_id)
		VALUES ('pending-runtime-proxy-guard', '{}', 'migrating', 'provisioning', ?)`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	putJSON(t, a.routes(), http.MethodPut, "/api/proxies/"+strconv.FormatInt(proxyID, 10), map[string]any{
		"name": "mutated", "status": "disabled", "username": "other-user", "password": "other-password",
	}, http.StatusConflict, nil)
	var migrationStatus, proxyStatus string
	var reservations int
	if err := a.db.QueryRow(`SELECT execution_migration_status FROM accounts WHERE id = ?`, accountID).Scan(&migrationStatus); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT status FROM proxies WHERE id = ?`, proxyID).Scan(&proxyStatus); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_proxy_reservations WHERE account_id = ?`, accountID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if migrationStatus != "migrating" || proxyStatus != "active" || reservations != 0 {
		t.Fatalf("pending mutation guard state = %q/%q/%d", migrationStatus, proxyStatus, reservations)
	}
}

func TestRuntimeProxyReservationExcludesOtherAccountAllocationAndGrant(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-cross-account")
	if _, err := fixture.app.db.Exec(`UPDATE proxy_pools SET single_use_enabled = 0 WHERE id =
		(SELECT pool_id FROM proxies WHERE id = ?)`, fixture.proxyID); err != nil {
		t.Fatal(err)
	}
	// Simulate the PRD soft-delete state that retains the durable reservation;
	// the legacy accounts.proxy_id occupancy check can no longer see this owner.
	if _, err := fixture.app.db.Exec(`UPDATE accounts SET deleted_at = `+nowSQL+` WHERE id = ?`, fixture.accountID); err != nil {
		t.Fatal(err)
	}
	var poolID int64
	if err := fixture.app.db.QueryRow(`SELECT pool_id FROM proxies WHERE id = ?`, fixture.proxyID).Scan(&poolID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.app.selectProxyForNewAccount(&poolID, &fixture.proxyID, false); err == nil {
		t.Fatal("new-account selector reused a runtime-reserved proxy")
	}
	result, err := fixture.app.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('proxy-cross-account-other', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	otherAccountID, _ := result.LastInsertId()
	tx, err := fixture.app.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if assigned, err := assignAccountProxy(tx, otherAccountID, &poolID, &fixture.proxyID, false); err == nil || assigned != nil {
		_ = tx.Rollback()
		t.Fatalf("other account assignment = %v, err=%v", assigned, err)
	}
	_ = tx.Rollback()
	if _, err := fixture.app.db.Exec(`UPDATE accounts SET proxy_id = ?, runtime_generation = 1 WHERE id = ?`,
		fixture.proxyID, otherAccountID); err != nil {
		t.Fatal(err)
	}
	tx, err = fixture.app.db.BeginTx(context.Background(), &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureRuntimeProxyReservationTx(context.Background(), tx, otherAccountID, fixture.proxyID, 1); !errors.Is(err, errRuntimeProxyReservationActive) {
		_ = tx.Rollback()
		t.Fatalf("cross-account grant error = %v", err)
	}
	_ = tx.Rollback()
	var otherReservations int
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_proxy_reservations WHERE account_id = ?`, otherAccountID).Scan(&otherReservations); err != nil {
		t.Fatal(err)
	}
	if otherReservations != 0 {
		t.Fatalf("cross-account conflict stored %d reservations", otherReservations)
	}
}

func TestRuntimeLifecycleLegacyEndpointsFailClosedAtomically(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	fixture := newRuntimeProxyReservationFixture(t, "runtime-lifecycle-guard")
	legacyResult, err := fixture.app.db.Exec(`INSERT INTO accounts
		(name, credentials_json, status, schedulable, invalidated_at) VALUES ('lifecycle-legacy', '{}', 'error', 0, ` + nowSQL + `)`)
	if err != nil {
		t.Fatal(err)
	}
	legacyID, _ := legacyResult.LastInsertId()
	if _, err := fixture.app.db.Exec(`UPDATE accounts SET status = 'error', schedulable = 0,
		invalidated_at = `+nowSQL+` WHERE id = ?`, fixture.accountID); err != nil {
		t.Fatal(err)
	}
	handler := fixture.app.routes()
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(fixture.accountID, 10)+"/archive",
		map[string]any{}, http.StatusConflict, nil)
	putJSON(t, handler, http.MethodDelete, "/api/accounts/"+strconv.FormatInt(fixture.accountID, 10),
		nil, http.StatusConflict, nil)
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-archive",
		map[string]any{"ids": []int64{legacyID, fixture.accountID}}, http.StatusConflict, nil)
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-delete",
		map[string]any{"ids": []int64{legacyID, fixture.accountID}}, http.StatusConflict, nil)
	for _, accountID := range []int64{legacyID, fixture.accountID} {
		var deletedAt, archivedAt sql.NullString
		if err := fixture.app.db.QueryRow(`SELECT deleted_at, archived_at FROM accounts WHERE id = ?`, accountID).Scan(&deletedAt, &archivedAt); err != nil {
			t.Fatal(err)
		}
		if deletedAt.Valid || archivedAt.Valid {
			t.Fatalf("blocked lifecycle partially changed account %d: deleted=%v archived=%v", accountID, deletedAt, archivedAt)
		}
	}
	if _, err := fixture.app.db.Exec(`UPDATE accounts SET archived_at = `+nowSQL+`, archived_proxy_id = proxy_id,
		proxy_id = NULL, status = 'disabled' WHERE id = ?`, fixture.accountID); err != nil {
		t.Fatal(err)
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(fixture.accountID, 10)+"/restore",
		map[string]any{}, http.StatusConflict, nil)
	var archivedAt sql.NullString
	var proxyID sql.NullInt64
	if err := fixture.app.db.QueryRow(`SELECT archived_at, proxy_id FROM accounts WHERE id = ?`, fixture.accountID).Scan(&archivedAt, &proxyID); err != nil {
		t.Fatal(err)
	}
	var reservationStatus string
	var revokes int
	if err := fixture.app.db.QueryRow(`SELECT status FROM runtime_proxy_reservations WHERE reservation_id = ?`,
		fixture.reservation.ReservationID).Scan(&reservationStatus); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = ?`,
		fixture.accountID, runtimeProxyReservationRevoke).Scan(&revokes); err != nil {
		t.Fatal(err)
	}
	if !archivedAt.Valid || proxyID.Valid || reservationStatus != runtimeProxyReservationActive || revokes != 0 {
		t.Fatalf("blocked restore/revoke state = archived=%v proxy=%v reservation=%q revokes=%d",
			archivedAt, proxyID, reservationStatus, revokes)
	}
}

func TestRuntimeLifecycleGuardDoesNotBlockLegacyAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "legacy-lifecycle-unblocked.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, credentials_json, status, schedulable, invalidated_at) VALUES ('legacy-lifecycle-unblocked', '{}', 'error', 0, ` + nowSQL + `)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	putJSON(t, a.routes(), http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/archive",
		map[string]any{}, http.StatusOK, nil)
	putJSON(t, a.routes(), http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/restore",
		map[string]any{}, http.StatusOK, nil)
	putJSON(t, a.routes(), http.MethodDelete, "/api/accounts/"+strconv.FormatInt(accountID, 10),
		nil, http.StatusNoContent, nil)
}

func TestRuntimeProxyReservationPreventsSQLiteHardDelete(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-hard-delete")
	if _, err := fixture.app.db.Exec(`DELETE FROM accounts WHERE id = ?`, fixture.accountID); err == nil {
		t.Fatal("hard delete cascaded a durable runtime proxy reservation")
	}
	var accounts, reservations, outbox int
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE id = ?`, fixture.accountID).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_proxy_reservations WHERE account_id = ?`, fixture.accountID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?`, fixture.accountID).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 || reservations != 1 || outbox != 2 {
		t.Fatalf("hard-delete preservation = accounts=%d reservations=%d outbox=%d", accounts, reservations, outbox)
	}
}

func TestStartupQuarantineSkipsRuntimeOwnedProxy(t *testing.T) {
	fixture := newRuntimeProxyReservationFixture(t, "proxy-quarantine-skip")
	if _, err := fixture.app.db.Exec(`UPDATE accounts SET archived_at = `+nowSQL+`, archived_proxy_id = proxy_id,
		proxy_id = NULL WHERE id = ?`, fixture.accountID); err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.app.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := quarantineProxyIDs(tx, []int64{fixture.proxyID}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var status string
	var deletedAt sql.NullString
	if err := fixture.app.db.QueryRow(`SELECT status, deleted_at FROM proxies WHERE id = ?`, fixture.proxyID).Scan(&status, &deletedAt); err != nil {
		t.Fatal(err)
	}
	if status != "active" || deletedAt.Valid {
		t.Fatalf("quarantine changed runtime-owned proxy: status=%q deleted=%v", status, deletedAt)
	}
}
