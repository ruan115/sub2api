package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExecutionFeatureMigrationDefaultsExistingAccountsToLegacy(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "execution-defaults.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name) VALUES ('legacy-account')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	var allowedModes, preferredMode, migrationStatus, runtimeStatus, slotID, provider string
	var generation, epoch int64
	var cliLimit, apiLimit, totalLimit int
	if err := a.db.QueryRow(`SELECT execution_allowed_modes, execution_preferred_mode, execution_migration_status,
		runtime_status, runtime_generation, runtime_slot_id, runtime_provider, runtime_execution_epoch,
		cli_native_limit, oauth_api_limit, execution_total_limit FROM accounts WHERE id = ?`, accountID).Scan(
		&allowedModes, &preferredMode, &migrationStatus, &runtimeStatus, &generation, &slotID, &provider, &epoch,
		&cliLimit, &apiLimit, &totalLimit,
	); err != nil {
		t.Fatal(err)
	}
	if allowedModes != `["cli_native","oauth_api"]` || preferredMode != "cli_native" || migrationStatus != "legacy" || runtimeStatus != "legacy" {
		t.Fatalf("unexpected execution defaults: modes=%s preferred=%s migration=%s runtime=%s", allowedModes, preferredMode, migrationStatus, runtimeStatus)
	}
	if generation != 0 || slotID != "" || provider != "" || epoch != 0 || cliLimit != 1 || apiLimit != 3 || totalLimit != 3 {
		t.Fatalf("unexpected runtime defaults: generation=%d slot=%q provider=%q epoch=%d limits=%d/%d/%d", generation, slotID, provider, epoch, cliLimit, apiLimit, totalLimit)
	}
	var policy, queueMode, imageChannel string
	if err := a.db.QueryRow(`SELECT execution_policy, worker_queue_mode, worker_image_channel FROM groups WHERE id = 'a'`).Scan(&policy, &queueMode, &imageChannel); err != nil {
		t.Fatal(err)
	}
	if policy != "auto" || queueMode != "queue" || imageChannel != "stable" {
		t.Fatalf("unexpected group execution defaults: %s/%s/%s", policy, queueMode, imageChannel)
	}
}

func TestRuntimeOnboardingResultCursorSchemaStaysInSyncAcrossDialects(t *testing.T) {
	for dialect, statements := range map[string][]string{
		"sqlite": sqliteExecutionSchema(),
		"mysql":  mysqlExecutionSchema(),
	} {
		schema := strings.Join(statements, "\n")
		for _, fragment := range []string{
			"CREATE TABLE IF NOT EXISTS runtime_onboarding_result_cursors",
			"cursor_name",
			"last_sequence",
			"updated_at",
		} {
			if !strings.Contains(schema, fragment) {
				t.Fatalf("%s execution schema is missing %q", dialect, fragment)
			}
		}
	}
}

func TestRuntimeOnboardingSubmissionSchemaStaysStrictAcrossDialects(t *testing.T) {
	sqliteSchema := strings.Join(sqliteExecutionSchema(), "\n")
	if !strings.Contains(sqliteSchema, "CREATE UNIQUE INDEX IF NOT EXISTS uq_runtime_onboarding_submission_account_generation") {
		t.Fatal("SQLite execution schema does not enforce one onboarding submission per account generation")
	}
	for _, fragment := range []string{"intake_idempotency_key TEXT", "intake_attempt INTEGER", "intent_expires_at_millis INTEGER"} {
		if !strings.Contains(sqliteSchema, fragment) {
			t.Fatalf("SQLite execution schema is missing %q", fragment)
		}
	}
	mysqlSchema := strings.Join(mysqlExecutionSchema(), "\n")
	for _, fragment := range []string{
		"idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY",
		"intake_idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL",
		"intake_attempt BIGINT UNSIGNED NOT NULL",
		"intent_expires_at_millis BIGINT NOT NULL",
		"UNIQUE KEY uq_runtime_onboarding_submission_intake_key (intake_idempotency_key)",
		"UNIQUE KEY uq_runtime_onboarding_submission_account_generation (account_id, desired_generation)",
	} {
		if !strings.Contains(mysqlSchema, fragment) {
			t.Fatalf("MySQL execution schema is missing %q", fragment)
		}
	}
}

func TestRuntimeOnboardingReceiptCommitMargin(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	if runtimeOnboardingReceiptHasCommitMargin(now.Add(runtimeOnboardingReceiptCommitMargin-time.Nanosecond), now) {
		t.Fatal("receipt with less than the commit margin was accepted")
	}
	if !runtimeOnboardingReceiptHasCommitMargin(now.Add(runtimeOnboardingReceiptCommitMargin), now) {
		t.Fatal("receipt with the exact commit margin was rejected")
	}
}

func TestSQLiteRuntimeOnboardingIntakeKeyIsUnique(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-onboarding-intake-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	rows, err := a.db.Query(`PRAGMA index_list(runtime_onboarding_submissions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if err := rows.Scan(&sequence, &name, &unique, &origin, &partial); err != nil {
			t.Fatal(err)
		}
		if name == "uq_runtime_onboarding_submission_intake_key" && unique == 1 {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("SQLite execution schema does not enforce unique internal intake keys")
	}
}

func TestRuntimeTransitionAndOutboxCheckpointAreAtomicAndAtLeastOnce(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "execution-outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('runtime-account', '{"access_token":"must-stay-out-of-outbox"}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	ctx := context.Background()
	if _, err := a.requestRuntimeTransition(ctx, runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.credential.migrate_requested", MigrationStatus: "migrating",
		RuntimeStatus: "provisioning", Payload: map[string]any{"access_token": "forbidden"},
	}); err == nil {
		t.Fatal("sensitive payload was accepted")
	}
	assertRuntimeGenerationAndEvents(t, a, accountID, 0, 0)

	first, err := a.requestRuntimeTransition(ctx, runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.credential.migrate_requested", MigrationStatus: "migrating",
		RuntimeStatus: "provisioning", Payload: map[string]any{"provider": "docker", "reason_code": "canary"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || first.DesiredGeneration != 1 {
		t.Fatalf("first event = %+v", first)
	}
	var migrationStatus, runtimeStatus, slotID, provider string
	var schedulable int
	if err := a.db.QueryRow(`SELECT execution_migration_status, runtime_status, runtime_slot_id, runtime_provider, schedulable FROM accounts WHERE id = ?`, accountID).Scan(
		&migrationStatus, &runtimeStatus, &slotID, &provider, &schedulable,
	); err != nil {
		t.Fatal(err)
	}
	if migrationStatus != "migrating" || runtimeStatus != "provisioning" || slotID == "" || provider != "docker" || schedulable != 0 {
		t.Fatalf("unexpected runtime desired state: %s/%s/%s/%s schedulable=%d", migrationStatus, runtimeStatus, slotID, provider, schedulable)
	}

	now := time.Unix(2_000_000_000, 0).UTC()
	claimed, ok, err := a.claimRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-a", now, 30*time.Second)
	if err != nil || !ok || claimed.EventID != first.EventID {
		t.Fatalf("first claim: event=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, _, err := a.claimRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-b", now.Add(time.Second), 30*time.Second); !errors.Is(err, errRuntimeOutboxBusy) {
		t.Fatalf("second owner claim error = %v", err)
	}
	if err := a.nackRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-a", first.Sequence, "dispatch_unavailable", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	replayed, ok, err := a.claimRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-b", now.Add(3*time.Second), 30*time.Second)
	if err != nil || !ok || replayed.EventID != first.EventID {
		t.Fatalf("replayed claim: event=%+v ok=%v err=%v", replayed, ok, err)
	}
	if err := a.ackRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-b", first.Sequence, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := a.ackRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-b", first.Sequence, now.Add(5*time.Second)); err != nil {
		t.Fatalf("idempotent ack: %v", err)
	}

	if _, err := a.requestRuntimeTransition(ctx, runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.runtime.provision_requested", MigrationStatus: "migrated",
		RuntimeStatus: "ready", Payload: map[string]any{"provider": "docker"},
	}); !errors.Is(err, errRuntimePlaintext) {
		t.Fatalf("plaintext migration completion error = %v", err)
	}
	assertRuntimeGenerationAndEvents(t, a, accountID, 1, 1)
	if _, err := a.db.Exec(`UPDATE accounts SET credentials_json = '{}' WHERE id = ?`, accountID); err != nil {
		t.Fatal(err)
	}
	second, err := a.requestRuntimeTransition(ctx, runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.runtime.provision_requested", MigrationStatus: "migrated",
		RuntimeStatus: "ready", Payload: map[string]any{"provider": "docker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.DesiredGeneration != 2 {
		t.Fatalf("second desired generation = %d", second.DesiredGeneration)
	}
	stolen, ok, err := a.claimRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-a", now.Add(6*time.Second), time.Second)
	if err != nil || !ok || stolen.EventID != second.EventID {
		t.Fatalf("second claim: event=%+v ok=%v err=%v", stolen, ok, err)
	}
	stolen, ok, err = a.claimRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-b", now.Add(8*time.Second), 30*time.Second)
	if err != nil || !ok || stolen.EventID != second.EventID {
		t.Fatalf("expired lease steal: event=%+v ok=%v err=%v", stolen, ok, err)
	}
	if err := a.ackRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-a", second.Sequence, now.Add(9*time.Second)); !errors.Is(err, errRuntimeOutboxNotClaimed) {
		t.Fatalf("old owner ack error = %v", err)
	}
	if err := a.ackRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-b", second.Sequence, now.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := a.claimRuntimeOutboxEvent(ctx, "worker-orchestrator", "replica-c", now.Add(10*time.Second), time.Second); err != nil || ok {
		t.Fatalf("empty outbox claim: ok=%v err=%v", ok, err)
	}
	var audits int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_operation_audit WHERE account_id = ?`, accountID).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("runtime audits=%d err=%v", audits, err)
	}
}

func TestRuntimeOnboardingTransitionStoresOnlyOpaqueIntentAndFencesGeneration(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "execution-onboarding-outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('runtime-onboarding', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	seedRuntimeOnboardingSubmission(t, a, "event-runtime-onboarding", accountID, 1,
		"account.runtime.provision_requested", "migrating", "session_key", "oauth")
	request := runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.runtime.provision_requested", MigrationStatus: "migrating",
		RuntimeStatus: "provisioning", OnboardingKey: "event-runtime-onboarding",
		Payload: map[string]any{"session_key": "must-be-replaced"},
	}
	submission, err := a.getRuntimeOnboardingSubmission(context.Background(), "event-runtime-onboarding")
	if err != nil {
		t.Fatal(err)
	}
	receipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey: submission.IntakeIdempotencyKey, IntentID: "intent-near-expiry",
		AccountID: accountID, DesiredGeneration: 1, ExpiresAt: time.Now().Add(4 * time.Second),
	}
	submission, err = a.persistRuntimeOnboardingReceipt(context.Background(), "event-runtime-onboarding", submission, receipt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.requestRuntimeOnboardingTransition(context.Background(), request, receipt); !errors.Is(err, errRuntimeMigration) {
		t.Fatalf("near-expiry receipt error = %v", err)
	}
	assertRuntimeGenerationAndEvents(t, a, accountID, 0, 0)
	oldReceipt := receipt
	staleSubmission := submission
	submission, err = a.advanceRuntimeOnboardingAttempt(context.Background(), submission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.persistRuntimeOnboardingReceipt(context.Background(), "event-runtime-onboarding", staleSubmission, oldReceipt, time.Now()); !errors.Is(err, errRuntimeOnboardingAttemptSuperseded) {
		t.Fatalf("late superseded receipt error = %v", err)
	}
	afterLateReceipt, err := a.getRuntimeOnboardingSubmission(context.Background(), "event-runtime-onboarding")
	if err != nil {
		t.Fatal(err)
	}
	if afterLateReceipt.IntakeAttempt != submission.IntakeAttempt ||
		afterLateReceipt.IntakeIdempotencyKey != submission.IntakeIdempotencyKey ||
		afterLateReceipt.IntentID != "" || afterLateReceipt.IntentExpiresAtMillis != 0 {
		t.Fatalf("late receipt changed current attempt: %+v", afterLateReceipt)
	}
	receipt.IdempotencyKey = submission.IntakeIdempotencyKey
	receipt.IntentID = "intent-10380"
	receipt.ExpiresAt = time.Now().Add(time.Minute)
	submission, err = a.persistRuntimeOnboardingReceipt(context.Background(), "event-runtime-onboarding", submission, receipt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	event, err := a.requestRuntimeOnboardingTransition(context.Background(), request, receipt)
	if err != nil {
		t.Fatal(err)
	}
	if event.DesiredGeneration != 1 || event.PayloadJSON != `{"onboarding_intent_id":"intent-10380"}` {
		t.Fatalf("onboarding event = %+v", event)
	}
	var payload string
	if err := a.db.QueryRow(`SELECT payload_json FROM runtime_outbox WHERE event_id = ?`, event.EventID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != event.PayloadJSON || strings.Contains(payload, "session_key") || strings.Contains(payload, "must-be-replaced") {
		t.Fatalf("stored onboarding payload = %s", payload)
	}
	if _, err := a.requestRuntimeOnboardingTransition(context.Background(), request, runtimeOnboardingIntentReceipt{
		IdempotencyKey: submission.IntakeIdempotencyKey, IntentID: "intent-stale", AccountID: accountID, DesiredGeneration: 1,
		ExpiresAt: time.Now().Add(time.Minute),
	}); !errors.Is(err, errRuntimeMigration) {
		t.Fatalf("stale generation error = %v", err)
	}
	assertRuntimeGenerationAndEvents(t, a, accountID, 1, 2)
	if _, err := a.requestRuntimeOnboardingTransition(context.Background(), request, runtimeOnboardingIntentReceipt{
		IdempotencyKey: submission.IntakeIdempotencyKey, IntentID: "sk-ant-must-not-pass", AccountID: accountID, DesiredGeneration: 2,
		ExpiresAt: time.Now().Add(time.Minute),
	}); !errors.Is(err, errRuntimeMigration) {
		t.Fatalf("secret-looking intent id error = %v", err)
	}
	assertRuntimeGenerationAndEvents(t, a, accountID, 1, 2)
}

func seedRuntimeOnboardingSubmission(
	t *testing.T,
	a *app,
	idempotencyKey string,
	accountID int64,
	desiredGeneration uint64,
	eventType string,
	migrationStatus string,
	sourceType string,
	authType string,
) int64 {
	t.Helper()
	proxyID := createTestForwardProxy(t, a)
	if _, err := a.db.Exec(`UPDATE accounts SET proxy_id = ? WHERE id = ?`, proxyID, accountID); err != nil {
		t.Fatal(err)
	}
	tx, err := a.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	candidate := runtimeOnboardingSubmission{
		IdempotencyKey: idempotencyKey, OperationType: runtimeOnboardingOperationForEvent(eventType),
		AccountID: accountID, DesiredGeneration: desiredGeneration, EventType: eventType,
		MigrationStatus: migrationStatus, SourceType: sourceType, AuthType: authType,
		ProxyID: sql.NullInt64{Int64: proxyID, Valid: true}, Status: runtimeOnboardingSubmissionPending,
	}
	if candidate.OperationType == runtimeOnboardingOperationCreate {
		candidate.RequestFingerprintVersion = runtimeOnboardingCreateFingerprintVersion
		candidate.RequestFingerprintSHA256[0] = 1
		candidate.RequestFingerprintPresent = true
	}
	created, err := insertRuntimeOnboardingSubmissionTx(context.Background(), tx, candidate)
	if err != nil || !created {
		t.Fatalf("seed runtime onboarding submission: created=%v err=%v", created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return proxyID
}

func TestEnsureRuntimeOnboardingSubmissionReplaysOnlyExactKeyAndRequest(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-onboarding-submission.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, credentials_json, proxy_id, execution_migration_status, runtime_status, runtime_generation)
		VALUES ('submission-replay', '{}', ?, 'migrated', 'ready', 7)`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	request := runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.credential.rotate_requested",
		MigrationStatus: "migrated", RuntimeStatus: "provisioning",
	}
	material := &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth"}
	created, err := a.ensureRuntimeOnboardingSubmission(context.Background(), "submission-key-a", request, material)
	if err != nil {
		t.Fatal(err)
	}
	if created.DesiredGeneration != 8 || created.OperationType != runtimeOnboardingOperationReauthorize {
		t.Fatalf("created submission = %+v", created)
	}
	replayed, err := a.ensureRuntimeOnboardingSubmission(context.Background(), "submission-key-a", request, material)
	if err != nil || replayed != created {
		t.Fatalf("exact replay = %+v, err=%v", replayed, err)
	}

	mismatchedEvent := request
	mismatchedEvent.EventType = "account.runtime.provision_requested"
	if _, err := a.ensureRuntimeOnboardingSubmission(context.Background(), "submission-key-a", mismatchedEvent, material); !errors.Is(err, errRuntimeOnboardingIdempotency) {
		t.Fatalf("mismatched operation/event error = %v", err)
	}
	mismatchedMigration := request
	mismatchedMigration.MigrationStatus = "migrating"
	if _, err := a.ensureRuntimeOnboardingSubmission(context.Background(), "submission-key-a", mismatchedMigration, material); !errors.Is(err, errRuntimeOnboardingIdempotency) {
		t.Fatalf("mismatched migration error = %v", err)
	}
	if _, err := a.ensureRuntimeOnboardingSubmission(context.Background(), "submission-key-b", request, material); !errors.Is(err, errRuntimeOnboardingIdempotency) {
		t.Fatalf("different key for the same account generation error = %v", err)
	}
	var submissions int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions WHERE account_id = ?`, accountID).Scan(&submissions); err != nil {
		t.Fatal(err)
	}
	if submissions != 1 {
		t.Fatalf("submission count = %d, want 1", submissions)
	}
}

func TestRuntimeOnboardingAccountValidationRefreshesExactQueuedSuccessor(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-onboarding-queued-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, credentials_json, proxy_id, execution_migration_status, runtime_status, runtime_generation)
		VALUES ('queued-refresh', '{}', ?, 'migrated', 'ready', 7)`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	request := runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.credential.rotate_requested",
		MigrationStatus: "migrated", RuntimeStatus: "provisioning", OnboardingKey: "queued-refresh-key",
	}
	material := &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth"}
	stalePending, err := a.ensureRuntimeOnboardingSubmission(context.Background(), "queued-refresh-key", request, material)
	if err != nil {
		t.Fatal(err)
	}
	receipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey:    stalePending.IntakeIdempotencyKey,
		IntentID:          "intent-queued-refresh",
		AccountID:         accountID,
		DesiredGeneration: stalePending.DesiredGeneration,
		ExpiresAt:         time.Now().Add(time.Minute),
	}
	persisted, err := a.persistRuntimeOnboardingReceipt(context.Background(), "queued-refresh-key", stalePending, receipt, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	event, err := a.requestRuntimeOnboardingTransition(context.Background(), request, receipt)
	if err != nil {
		t.Fatal(err)
	}

	refreshed, err := a.validateRuntimeOnboardingSubmissionAccountOrRefreshQueued(
		context.Background(), stalePending, "queued-refresh-key", request, material,
	)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Status != runtimeOnboardingSubmissionQueued || refreshed.EventID != event.EventID ||
		refreshed.IntakeIdempotencyKey != persisted.IntakeIdempotencyKey {
		t.Fatalf("refreshed submission = %+v, event=%+v", refreshed, event)
	}
}

func TestAdvanceRuntimeOnboardingAttemptReplaysNewerQueuedSuccessor(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-onboarding-advance-queued-successor.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, credentials_json, execution_migration_status, runtime_status, runtime_generation)
		VALUES ('advance-queued-successor', '{}', 'migrated', 'ready', 2)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	seedRuntimeOnboardingSubmission(t, a, "advance-queued-successor-key", accountID, 3,
		"account.credential.rotate_requested", "migrated", "session_key", "oauth")
	staleAttempt, err := a.getRuntimeOnboardingSubmission(context.Background(), "advance-queued-successor-key")
	if err != nil {
		t.Fatal(err)
	}
	firstReceipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey:    staleAttempt.IntakeIdempotencyKey,
		IntentID:          "intent-stale-attempt",
		AccountID:         accountID,
		DesiredGeneration: staleAttempt.DesiredGeneration,
		ExpiresAt:         time.Now().Add(time.Minute),
	}
	staleAttempt, err = a.persistRuntimeOnboardingReceipt(
		context.Background(), "advance-queued-successor-key", staleAttempt, firstReceipt, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	newerAttempt, err := a.advanceRuntimeOnboardingAttempt(context.Background(), staleAttempt)
	if err != nil {
		t.Fatal(err)
	}
	secondReceipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey:    newerAttempt.IntakeIdempotencyKey,
		IntentID:          "intent-newer-attempt",
		AccountID:         accountID,
		DesiredGeneration: newerAttempt.DesiredGeneration,
		ExpiresAt:         time.Now().Add(time.Minute),
	}
	newerAttempt, err = a.persistRuntimeOnboardingReceipt(
		context.Background(), "advance-queued-successor-key", newerAttempt, secondReceipt, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	event, err := a.requestRuntimeOnboardingTransition(context.Background(), runtimeTransitionRequest{
		AccountID: accountID, EventType: newerAttempt.EventType, MigrationStatus: newerAttempt.MigrationStatus,
		RuntimeStatus: "provisioning", OnboardingKey: "advance-queued-successor-key",
	}, secondReceipt)
	if err != nil {
		t.Fatal(err)
	}

	replayed, err := a.advanceRuntimeOnboardingAttempt(context.Background(), staleAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != runtimeOnboardingSubmissionQueued || replayed.EventID != event.EventID ||
		replayed.IntakeAttempt != newerAttempt.IntakeAttempt {
		t.Fatalf("replayed successor = %+v, event=%+v, newer=%+v", replayed, event, newerAttempt)
	}
}

func TestRuntimeOnboardingReloadErrorSeparatesMissingFenceFromDatabaseFailure(t *testing.T) {
	if err := runtimeOnboardingReloadError("reload test", sql.ErrNoRows); !errors.Is(err, errRuntimeOnboardingIdempotency) {
		t.Fatalf("missing reload error = %v", err)
	}
	databaseFailure := errors.New("database connection lost")
	err := runtimeOnboardingReloadError("reload test", databaseFailure)
	if !errors.Is(err, databaseFailure) || errors.Is(err, errRuntimeOnboardingIdempotency) {
		t.Fatalf("database reload error = %v", err)
	}
}

func TestEnsureRuntimeOnboardingSubmissionRejectsNewKeyWhileProvisioning(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-onboarding-provisioning.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, credentials_json, proxy_id, execution_migration_status, runtime_status, runtime_generation)
		VALUES ('submission-provisioning', '{}', ?, 'migrated', 'provisioning', 4)`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	_, err = a.ensureRuntimeOnboardingSubmission(context.Background(), "submission-key-provisioning", runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.credential.rotate_requested",
		MigrationStatus: "migrated", RuntimeStatus: "provisioning",
	}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth"})
	if !errors.Is(err, errRuntimeOnboardingIdempotency) {
		t.Fatalf("new key while provisioning error = %v", err)
	}
	var submissions int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions WHERE account_id = ?`, accountID).Scan(&submissions); err != nil {
		t.Fatal(err)
	}
	if submissions != 0 {
		t.Fatalf("submission count = %d, want 0", submissions)
	}
}

func TestEnsureRuntimeOnboardingSubmissionRejectsLifecycleTransition(t *testing.T) {
	for _, runtimeStatus := range []string{"provisioning", "draining", "destroying", "archived", "deleted"} {
		t.Run(runtimeStatus, func(t *testing.T) {
			a, err := newApp(filepath.Join(t.TempDir(), "runtime-onboarding-lifecycle.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer a.db.Close()
			proxyID := createTestForwardProxy(t, a)
			result, err := a.db.Exec(`INSERT INTO accounts
				(name, credentials_json, proxy_id, execution_migration_status, runtime_status, runtime_generation)
				VALUES ('submission-lifecycle', '{}', ?, 'migrated', ?, 4)`, proxyID, runtimeStatus)
			if err != nil {
				t.Fatal(err)
			}
			accountID, _ := result.LastInsertId()
			_, err = a.ensureRuntimeOnboardingSubmission(context.Background(), "submission-lifecycle-key", runtimeTransitionRequest{
				AccountID: accountID, EventType: "account.credential.rotate_requested",
				MigrationStatus: "migrated", RuntimeStatus: "provisioning",
			}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth"})
			if !errors.Is(err, errRuntimeOnboardingIdempotency) {
				t.Fatalf("new key during %s error = %v", runtimeStatus, err)
			}
			var submissions int
			if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions WHERE account_id = ?`, accountID).Scan(&submissions); err != nil {
				t.Fatal(err)
			}
			if submissions != 0 {
				t.Fatalf("submission count = %d, want 0", submissions)
			}
		})
	}
}

func TestRuntimeTransitionRejectsArchivedAccount(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "execution-archived-transition.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json, archived_at) VALUES ('archived-transition', '{}', ` + nowSQL + `)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	if _, err := a.requestRuntimeTransition(context.Background(), runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.runtime.provision_requested", MigrationStatus: "migrating",
		RuntimeStatus: "provisioning", Payload: map[string]any{"provider": "docker"},
	}); err == nil {
		t.Fatal("archived account accepted a runtime transition")
	}
	assertRuntimeGenerationAndEvents(t, a, accountID, 0, 0)
}

func TestMigratedAccountRejectsSynchronousRuntimeRoutingMutations(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-routing-owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	otherProxyID := createTestForwardProxy(t, a)
	created, err := a.db.Exec(`INSERT INTO accounts
		(name, platform, auth_type, credentials_json, status, auth_status, schedulable,
		 proxy_pool_id, proxy_id, execution_migration_status, runtime_status, runtime_generation)
		VALUES ('runtime-owned-routing', 'anthropic', 'oauth', '{}', 'active', 'valid', 0,
		 1, ?, 'migrated', 'ready', 2)`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := created.LastInsertId()
	handler := a.routes()
	base := map[string]any{
		"name": "runtime-owned-routing", "platform": "anthropic", "auth_type": "oauth",
		"status": "active", "schedulable": false, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1,
		"proxy_id": proxyID, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}
	base["name"] = "runtime-owned-routing-renamed"
	base["notes"] = "metadata remains CCMAX-owned"
	putJSON(t, handler, http.MethodPut, "/api/accounts/"+strconv.FormatInt(accountID, 10), base, http.StatusOK, nil)
	base["schedulable"] = true
	putJSON(t, handler, http.MethodPut, "/api/accounts/"+strconv.FormatInt(accountID, 10), base, http.StatusConflict, nil)
	base["schedulable"] = false
	base["proxy_id"] = otherProxyID
	putJSON(t, handler, http.MethodPut, "/api/accounts/"+strconv.FormatInt(accountID, 10), base, http.StatusConflict, nil)
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-schedule", map[string]any{
		"ids": []int64{accountID}, "schedulable": true,
	}, http.StatusConflict, nil)

	var storedProxyID int64
	var schedulable int
	var generation uint64
	var eventCount int
	if err := a.db.QueryRow(`SELECT proxy_id, schedulable, runtime_generation FROM accounts WHERE id = ?`, accountID).Scan(
		&storedProxyID, &schedulable, &generation,
	); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?`, accountID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if storedProxyID != proxyID || schedulable != 0 || generation != 2 || eventCount != 0 {
		t.Fatalf("runtime routing changed outside execution plane: proxy/schedulable/generation/events=%d/%d/%d/%d",
			storedProxyID, schedulable, generation, eventCount)
	}
}

func TestAccountModeHealthIsModeIsolatedAndRejectsSecrets(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "execution-health.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name) VALUES ('mode-health')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	ctx := context.Background()
	recoverAt := time.Unix(2_000_000_000, 0).UTC().Format(time.RFC3339Nano)
	if err := a.setAccountModeHealth(ctx, accountModeHealth{
		AccountID: accountID, Mode: "cli_native", Status: "billing_blocked", ErrorCode: "extra_usage_400", RecoverAt: recoverAt,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.setAccountModeHealth(ctx, accountModeHealth{
		AccountID: accountID, Mode: "oauth_api", Status: "healthy",
	}); err != nil {
		t.Fatal(err)
	}
	cli, err := a.getAccountModeHealth(ctx, accountID, "cli_native")
	if err != nil {
		t.Fatal(err)
	}
	api, err := a.getAccountModeHealth(ctx, accountID, "oauth_api")
	if err != nil {
		t.Fatal(err)
	}
	if cli.Status != "billing_blocked" || api.Status != "healthy" || cli.RecoverAt == "" {
		t.Fatalf("mode health leaked across modes: cli=%+v api=%+v", cli, api)
	}
	if err := a.setAccountModeHealth(ctx, accountModeHealth{
		AccountID: accountID, Mode: "oauth_api", Status: "auth_failed", ErrorCode: "Bearer leaked-token",
	}); err == nil {
		t.Fatal("sensitive mode health error was accepted")
	}
}

func TestMigratedAccountCannotFallBackToLegacyGateway(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var migratedCalls, legacyCalls atomic.Int32
	migratedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		migratedCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"migrated","type":"message","role":"assistant","content":[{"type":"text","text":"wrong"}],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer migratedUpstream.Close()
	legacyUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		legacyCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"legacy","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer legacyUpstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	migrated := createGatewayTestAccount(t, a, handler, "migrated", migratedUpstream.URL, 0, nil, map[string]any{"access_token": "will-be-cleared"})
	legacy := createGatewayTestAccount(t, a, handler, "legacy", legacyUpstream.URL, 1, nil, map[string]any{"access_token": "legacy-token"})
	if _, err := a.db.Exec(`UPDATE accounts SET execution_migration_status = 'migrated', runtime_status = 'ready', credentials_json = '{}', schedulable = 1 WHERE id = ?`, migrated.ID); err != nil {
		t.Fatal(err)
	}
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 16, "stream": false,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusOK, nil)
	if migratedCalls.Load() != 0 || legacyCalls.Load() != 1 {
		t.Fatalf("legacy gateway routed migrated=%d legacy=%d", migratedCalls.Load(), legacyCalls.Load())
	}
	resolved, _, err := a.resolveAccount("default", false)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ID != legacy.ID {
		t.Fatalf("legacy pool resolved account %d, want %d", resolved.ID, legacy.ID)
	}
}

func TestMigratedAccountRejectsEveryLegacyCredentialWritePath(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-credential-owner.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, auth_type, credentials_json, execution_migration_status, runtime_status, runtime_generation, runtime_slot_id, runtime_provider, runtime_execution_epoch, auth_status, status, schedulable)
		VALUES ('execution-owned', 'oauth', '{}', 'migrated', 'ready', 3, 'ccmax-account-7', 'docker', 9, 'valid', 'active', 1)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	handler := a.routes()
	for _, path := range []string{
		"/api/accounts/" + strconv.FormatInt(accountID, 10) + "/auth-url",
		"/api/accounts/" + strconv.FormatInt(accountID, 10) + "/session-auth",
	} {
		putJSON(t, handler, http.MethodPost, path, map[string]any{}, http.StatusConflict, nil)
	}
	// Migrated OAuth exchange is an execution-onboarding endpoint now; without
	// a matching one-time OAuth session it fails before accepting material.
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/oauth-exchange", map[string]any{}, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/session-auth", map[string]any{
		"session_key": "must-not-enter-legacy-auth",
	}, http.StatusConflict, nil)
	putJSON(t, handler, http.MethodPut, "/api/accounts/"+strconv.FormatInt(accountID, 10), map[string]any{
		"name": "execution-owned", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "must-not-return"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "rate_multiplier": 1,
		"group_ids": []string{"a"}, "base_rpm": 0, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusConflict, nil)

	if err := a.saveClaudeToken(accountID, "oauth", &claudeTokenInfo{AccessToken: "must-not-save", ExpiresAt: time.Now().Add(time.Hour).Unix()}, false); !errors.Is(err, errRuntimeCredentialOwner) {
		t.Fatalf("direct token save error=%v", err)
	}
	if err := a.updateBatchAuthorizedAccount(accountID, batchAuthorizationInput{}, "oauth", "session-key", &claudeTokenInfo{AccessToken: "must-not-save"}, "owner"); !errors.Is(err, errRuntimeCredentialOwner) {
		t.Fatalf("batch token save error=%v", err)
	}
	a.markAccountReauth(accountID, "legacy refresh must not mutate migrated account")
	var credentials, status, authStatus string
	if err := a.db.QueryRow(`SELECT credentials_json, status, auth_status FROM accounts WHERE id = ?`, accountID).Scan(&credentials, &status, &authStatus); err != nil {
		t.Fatal(err)
	}
	if credentials != "{}" || status != "active" || authStatus != "valid" {
		t.Fatalf("execution-owned account mutated: credentials=%s status=%s auth=%s", credentials, status, authStatus)
	}
}

func TestMySQLExecutionOutboxIntegration(t *testing.T) {
	dsn := os.Getenv("CCMAX_EXECUTION_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set CCMAX_EXECUTION_MYSQL_TEST_DSN to run the CCMAX MySQL execution integration")
	}
	t.Setenv("CCMAX_MYSQL_DSN", dsn)
	a, err := newApp("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.db.Close() })
	suffix := newRuntimeEventID()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES (?, '{"access_token":"temporary"}')`, "mysql-runtime-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	consumerName := "mysql-integration-" + suffix
	t.Cleanup(func() {
		_, _ = a.db.Exec(`DELETE FROM runtime_outbox_consumers WHERE consumer_name = ?`, consumerName)
		_, _ = a.db.Exec(`DELETE FROM runtime_operation_audit WHERE account_id = ?`, accountID)
		_, _ = a.db.Exec(`DELETE FROM runtime_outbox WHERE account_id = ?`, accountID)
		_, _ = a.db.Exec(`DELETE FROM accounts WHERE id = ?`, accountID)
	})
	ctx := context.Background()
	event, err := a.requestRuntimeTransition(ctx, runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.credential.migrate_requested", MigrationStatus: "migrating",
		RuntimeStatus: "provisioning", Payload: map[string]any{"provider": "docker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, ok, err := a.claimRuntimeOutboxEvent(ctx, consumerName, "replica-a", now, time.Minute)
	if err != nil || !ok || claimed.EventID != event.EventID {
		t.Fatalf("MySQL outbox claim: event=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := a.nackRuntimeOutboxEvent(ctx, consumerName, "replica-a", event.Sequence, "integration_retry", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err = a.claimRuntimeOutboxEvent(ctx, consumerName, "replica-b", now.Add(2*time.Second), time.Minute)
	if err != nil || !ok || claimed.EventID != event.EventID {
		t.Fatalf("MySQL outbox replay: event=%+v ok=%v err=%v", claimed, ok, err)
	}
	if err := a.ackRuntimeOutboxEvent(ctx, consumerName, "replica-b", event.Sequence, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func assertRuntimeGenerationAndEvents(t *testing.T, a *app, accountID, generation, events int64) {
	t.Helper()
	var storedGeneration, storedEvents int64
	if err := a.db.QueryRow(`SELECT runtime_generation FROM accounts WHERE id = ?`, accountID).Scan(&storedGeneration); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?`, accountID).Scan(&storedEvents); err != nil {
		t.Fatal(err)
	}
	if storedGeneration != generation || storedEvents != events {
		t.Fatalf("runtime transaction partially committed: generation=%d events=%d, want %d/%d", storedGeneration, storedEvents, generation, events)
	}
}
