package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
		"/api/accounts/" + strconv.FormatInt(accountID, 10) + "/oauth-exchange",
		"/api/accounts/" + strconv.FormatInt(accountID, 10) + "/session-auth",
	} {
		putJSON(t, handler, http.MethodPost, path, map[string]any{}, http.StatusConflict, nil)
	}
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
