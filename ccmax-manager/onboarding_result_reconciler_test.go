package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeRuntimeOnboardingResultReader struct {
	result runtimeOnboardingResult
	err    error
	calls  []runtimeOnboardingCandidate
}

func (f *fakeRuntimeOnboardingResultReader) GetResult(
	_ context.Context,
	intentID string,
	accountID int64,
	desiredGeneration uint64,
) (runtimeOnboardingResult, error) {
	f.calls = append(f.calls, runtimeOnboardingCandidate{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: desiredGeneration,
	})
	return f.result, f.err
}

func TestRuntimeOnboardingResultReconcilerCompletesGenerationFencedAccount(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-result.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID, intentID, slotID := insertPendingRuntimeOnboarding(t, a, "pending-owner", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	now := time.Now().UTC().Truncate(time.Millisecond)
	reader := &fakeRuntimeOnboardingResultReader{result: runtimeOnboardingResult{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: 1, SlotID: slotID, ExecutionEpoch: 17,
		AuthType: "oauth", EmailAddress: "owner@example.com", OrganizationID: "org-10380",
		UpstreamAccountID: "upstream-10380", Scope: "user:inference", SubscriptionType: "max",
		RateLimitTier: "tier-1", ExpiresAt: now.Add(time.Hour), ProjectedAt: now,
	}}
	stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
	if err != nil || stats.Checked != 1 || stats.Completed != 1 || stats.Pending != 0 || stats.Failed != 0 {
		t.Fatalf("reconcile stats=%+v err=%v", stats, err)
	}
	var name, authType, migrationStatus, runtimeStatus, credentialsJSON, authStatus, subscription, tier string
	var generation, epoch uint64
	var schedulable int
	if err := a.db.QueryRow(`SELECT name, auth_type, execution_migration_status, runtime_status, runtime_generation,
		runtime_execution_epoch, schedulable, credentials_json, auth_status, subscription_type, rate_limit_tier
		FROM accounts WHERE id = ?`, accountID).Scan(
		&name, &authType, &migrationStatus, &runtimeStatus, &generation, &epoch, &schedulable,
		&credentialsJSON, &authStatus, &subscription, &tier,
	); err != nil {
		t.Fatal(err)
	}
	if name != "owner@example.com" || authType != "oauth" || migrationStatus != "migrated" || runtimeStatus != "ready" ||
		generation != 1 || epoch != 17 || schedulable != 1 || credentialsJSON != "{}" || authStatus != "valid" ||
		subscription != "max" || tier != "tier-1" {
		t.Fatalf("completed account=%s/%s/%s/%s/%d/%d/%d/%s/%s/%s/%s", name, authType, migrationStatus,
			runtimeStatus, generation, epoch, schedulable, credentialsJSON, authStatus, subscription, tier)
	}
	for _, mode := range []string{executionModeCLINative, executionModeOAuthAPI} {
		var status, errorCode string
		if err := a.db.QueryRow(`SELECT status, error_code FROM account_mode_health WHERE account_id = ? AND mode = ?`, accountID, mode).Scan(&status, &errorCode); err != nil {
			t.Fatal(err)
		}
		if status != "healthy" || errorCode != "" {
			t.Fatalf("mode %s = %s/%s", mode, status, errorCode)
		}
	}
	var auditDetail string
	if err := a.db.QueryRow(`SELECT detail_json FROM runtime_operation_audit WHERE account_id = ? AND status = 'completed'`, accountID).Scan(&auditDetail); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(auditDetail)) || containsRuntimeCredentialMaterial(auditDetail) {
		t.Fatalf("unsafe audit detail: %s", auditDetail)
	}
	stats, err = a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
	if err != nil || stats.Checked != 0 || len(reader.calls) != 1 {
		t.Fatalf("idempotent reconcile stats=%+v calls=%d err=%v", stats, len(reader.calls), err)
	}
}

func TestRuntimeOnboardingResultReconcilerLeavesPendingResultUntouched(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID, _, _ := insertPendingRuntimeOnboarding(t, a, "pending", "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	reader := &fakeRuntimeOnboardingResultReader{err: errRuntimeOnboardingResultPending}
	stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
	if err != nil || stats.Checked != 1 || stats.Pending != 1 || stats.Completed != 0 {
		t.Fatalf("pending stats=%+v err=%v", stats, err)
	}
	var runtimeStatus string
	var schedulable int
	if err := a.db.QueryRow(`SELECT runtime_status, schedulable FROM accounts WHERE id = ?`, accountID).Scan(&runtimeStatus, &schedulable); err != nil {
		t.Fatal(err)
	}
	if runtimeStatus != "provisioning" || schedulable != 0 {
		t.Fatalf("pending account=%s/%d", runtimeStatus, schedulable)
	}
}

func TestRuntimeOnboardingResultReconcilerPersistsTerminalFailure(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		errorCode string
		summary   string
		withSlot  bool
	}{
		{"failed", runtimeOnboardingResultFailed, runtimeOnboardingErrorFailed, runtimeOnboardingSummaryFailed, true},
		{"expired", runtimeOnboardingResultExpired, runtimeOnboardingErrorExpired, runtimeOnboardingSummaryExpired, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a, err := newApp(filepath.Join(t.TempDir(), "onboarding-terminal.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer a.db.Close()
			accountID, intentID, slotID := insertPendingRuntimeOnboarding(t, a, "terminal-"+test.name, "terminal-intent-"+test.name)
			result := runtimeOnboardingResult{
				IntentID: intentID, AccountID: accountID, DesiredGeneration: 1,
				Status: test.status, ErrorCode: test.errorCode, ErrorSummary: test.summary,
				FinishedAt: time.Now().UTC(),
			}
			if test.withSlot {
				result.SlotID, result.ExecutionEpoch = slotID, 17
			}
			reader := &fakeRuntimeOnboardingResultReader{result: result}
			stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
			if err != nil || stats.Checked != 1 || stats.Terminated != 1 || stats.Completed != 0 || stats.Failed != 0 {
				t.Fatalf("terminal stats=%+v err=%v", stats, err)
			}
			var runtimeStatus, runtimeError, authStatus, authError, accountStatus string
			var schedulable int
			if err := a.db.QueryRow(`SELECT runtime_status, runtime_error_code, auth_status, auth_error, status, schedulable
				FROM accounts WHERE id = ?`, accountID).Scan(
				&runtimeStatus, &runtimeError, &authStatus, &authError, &accountStatus, &schedulable,
			); err != nil {
				t.Fatal(err)
			}
			if runtimeStatus != "failed" || runtimeError != test.errorCode || authStatus != "invalid" ||
				authError != test.summary || accountStatus != "error" || schedulable != 0 {
				t.Fatalf("terminal account=%s/%s/%s/%s/%s/%d", runtimeStatus, runtimeError, authStatus, authError, accountStatus, schedulable)
			}
			for _, mode := range []string{executionModeCLINative, executionModeOAuthAPI} {
				var status, errorCode string
				if err := a.db.QueryRow(`SELECT status, error_code FROM account_mode_health WHERE account_id = ? AND mode = ?`,
					accountID, mode).Scan(&status, &errorCode); err != nil {
					t.Fatal(err)
				}
				if status != "unavailable" || errorCode != test.errorCode {
					t.Fatalf("terminal mode=%s/%s/%s", mode, status, errorCode)
				}
			}
			stats, err = a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
			if err != nil || stats.Checked != 0 || len(reader.calls) != 1 {
				t.Fatalf("terminal replay stats=%+v calls=%d err=%v", stats, len(reader.calls), err)
			}
		})
	}
}

func TestRuntimeOnboardingResultReconcilerPersistsCursorPastPendingPrefix(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "onboarding-pending-prefix.db")
	a, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	accountIDs := make([]int64, 0, 5)
	for index := 0; index < 5; index++ {
		accountID, _, _ := insertPendingRuntimeOnboarding(t, a, "pending-prefix-"+strconv.Itoa(index), "pending-intent-"+strconv.Itoa(index))
		accountIDs = append(accountIDs, accountID)
	}
	reader := &fakeRuntimeOnboardingResultReader{err: errRuntimeOnboardingResultPending}
	stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 2)
	if err != nil || stats.Checked != 2 || stats.Pending != 2 {
		t.Fatalf("first pending page stats=%+v err=%v", stats, err)
	}
	if len(reader.calls) != 2 || reader.calls[0].AccountID != accountIDs[0] || reader.calls[1].AccountID != accountIDs[1] {
		t.Fatalf("first pending page calls=%+v", reader.calls)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	stats, err = a.reconcileRuntimeOnboardingResults(context.Background(), reader, 2)
	if err != nil || stats.Checked != 2 || stats.Pending != 2 {
		t.Fatalf("second pending page stats=%+v err=%v", stats, err)
	}
	if len(reader.calls) != 4 || reader.calls[2].AccountID != accountIDs[2] || reader.calls[3].AccountID != accountIDs[3] {
		t.Fatalf("persistent cursor did not reach later candidates: calls=%+v", reader.calls)
	}
	var cursor int64
	if err := a.db.QueryRow(`SELECT last_sequence FROM runtime_onboarding_result_cursors WHERE cursor_name = ?`, runtimeOnboardingResultCursorName).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor < 4 {
		t.Fatalf("persisted cursor=%d want at least fourth outbox sequence", cursor)
	}
	stats, err = a.reconcileRuntimeOnboardingResults(context.Background(), reader, 2)
	if err != nil || stats.Checked != 2 || stats.Pending != 2 {
		t.Fatalf("wrapped pending page stats=%+v err=%v", stats, err)
	}
	if len(reader.calls) != 6 || reader.calls[4].AccountID != accountIDs[4] || reader.calls[5].AccountID != accountIDs[0] {
		t.Fatalf("cursor did not wrap after the outbox tail: calls=%+v", reader.calls)
	}
}

func TestRuntimeOnboardingResultReconcilerAdvancesCursorPastMalformedPrefix(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-malformed-prefix.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountIDs := make([]int64, 0, 4)
	for index := 0; index < 4; index++ {
		accountID, _, _ := insertPendingRuntimeOnboarding(t, a, "malformed-prefix-"+strconv.Itoa(index), "malformed-intent-"+strconv.Itoa(index))
		accountIDs = append(accountIDs, accountID)
	}
	if _, err := a.db.Exec(`UPDATE runtime_outbox SET payload_json = '{"unexpected":"field"}' WHERE account_id IN (?, ?)`, accountIDs[0], accountIDs[1]); err != nil {
		t.Fatal(err)
	}
	reader := &fakeRuntimeOnboardingResultReader{err: errRuntimeOnboardingResultPending}
	stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 2)
	if err != nil || stats.Checked != 2 || stats.Failed != 2 || stats.Pending != 0 || len(reader.calls) != 0 {
		t.Fatalf("malformed prefix stats=%+v calls=%+v err=%v", stats, reader.calls, err)
	}
	stats, err = a.reconcileRuntimeOnboardingResults(context.Background(), reader, 2)
	if err != nil || stats.Checked != 2 || stats.Pending != 2 || stats.Failed != 0 {
		t.Fatalf("post-malformed page stats=%+v err=%v", stats, err)
	}
	if len(reader.calls) != 2 || reader.calls[0].AccountID != accountIDs[2] || reader.calls[1].AccountID != accountIDs[3] {
		t.Fatalf("malformed prefix starved later candidates: calls=%+v", reader.calls)
	}
}

func TestRuntimeOnboardingResultReconcilerBlocksDuplicateIdentity(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-duplicate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	existing, err := a.db.Exec(`INSERT INTO accounts (name, platform, auth_type, credentials_json) VALUES ('owner@example.com', 'anthropic', 'oauth', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	existingID, _ := existing.LastInsertId()
	accountID, intentID, slotID := insertPendingRuntimeOnboarding(t, a, "pending-duplicate", "cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa")
	reader := &fakeRuntimeOnboardingResultReader{result: runtimeOnboardingResult{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: 1, SlotID: slotID, ExecutionEpoch: 3,
		AuthType: "oauth", EmailAddress: "OWNER@example.com", ProjectedAt: time.Now().UTC(),
	}}
	// The mTLS client canonicalizes email case before this boundary.
	reader.result.EmailAddress = normalizeAccountEmail(reader.result.EmailAddress)
	stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
	if err != nil || stats.Conflicted != 1 || stats.Completed != 0 {
		t.Fatalf("duplicate stats=%+v err=%v", stats, err)
	}
	var runtimeStatus, errorCode, migrationStatus, credentialsJSON string
	var schedulable int
	if err := a.db.QueryRow(`SELECT runtime_status, runtime_error_code, execution_migration_status, credentials_json, schedulable
		FROM accounts WHERE id = ?`, accountID).Scan(&runtimeStatus, &errorCode, &migrationStatus, &credentialsJSON, &schedulable); err != nil {
		t.Fatal(err)
	}
	if runtimeStatus != "failed" || errorCode != "duplicate_identity" || migrationStatus != "migrating" || credentialsJSON != "{}" || schedulable != 0 {
		t.Fatalf("duplicate account=%s/%s/%s/%s/%d", runtimeStatus, errorCode, migrationStatus, credentialsJSON, schedulable)
	}
	var conflictID int64
	var detail string
	if err := a.db.QueryRow(`SELECT detail_json FROM runtime_operation_audit WHERE account_id = ? AND status = 'blocked'`, accountID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]int64
	if err := json.Unmarshal([]byte(detail), &decoded); err != nil {
		t.Fatal(err)
	}
	conflictID = decoded["conflict_account_id"]
	if conflictID != existingID {
		t.Fatalf("conflict id=%d want=%d", conflictID, existingID)
	}
}

func TestRuntimeOnboardingResultReconcilerKeepsAPIKeyOutOfCLIMode(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-api-key.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID, intentID, slotID := insertPendingRuntimeOnboarding(t, a, "api-key", "dddddddd-eeee-4fff-8aaa-bbbbbbbbbbbb")
	reader := &fakeRuntimeOnboardingResultReader{result: runtimeOnboardingResult{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: 1, SlotID: slotID, ExecutionEpoch: 5,
		AuthType: "api_key", ProjectedAt: time.Now().UTC(),
	}}
	stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
	if err != nil || stats.Completed != 1 {
		t.Fatalf("api key stats=%+v err=%v", stats, err)
	}
	for mode, expected := range map[string]string{executionModeCLINative: "unavailable", executionModeOAuthAPI: "healthy"} {
		var status string
		if err := a.db.QueryRow(`SELECT status FROM account_mode_health WHERE account_id = ? AND mode = ?`, accountID, mode).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != expected {
			t.Fatalf("mode %s=%s want=%s", mode, status, expected)
		}
	}
}

func TestRuntimeOnboardingResultReconcilerPreservesExistingAccountNameDuringMigration(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-reauthorization.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID, intentID, slotID := insertPendingRuntimeOnboarding(t, a, "operator-label", "abababab-cdcd-4efe-8a8a-bcbcbcbcbcbc")
	if _, err := a.db.Exec(`UPDATE accounts SET onboarded_at = `+nowSQL+` WHERE id = ?`, accountID); err != nil {
		t.Fatal(err)
	}
	result := runtimeOnboardingResult{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: 1, SlotID: slotID, ExecutionEpoch: 23,
		AuthType: "oauth", EmailAddress: "authenticated@example.com", ProjectedAt: time.Now().UTC(),
	}
	candidates, err := a.listRuntimeOnboardingCandidates(context.Background(), 100)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("reauthorization candidates=%+v err=%v", candidates, err)
	}
	if err := a.applyRuntimeOnboardingResult(context.Background(), candidates[0], result); err != nil {
		t.Fatalf("apply reauthorization result: %v", err)
	}
	var name string
	var reauthorizationCount int
	if err := a.db.QueryRow(`SELECT name, reauthorization_count FROM accounts WHERE id = ?`, accountID).Scan(&name, &reauthorizationCount); err != nil {
		t.Fatal(err)
	}
	if name != "operator-label" || reauthorizationCount != 1 {
		t.Fatalf("reauthorized account name/count=%q/%d", name, reauthorizationCount)
	}
}

func TestRuntimeOnboardingResultReconcilerRejectsChangedBinding(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-stale.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID, intentID, _ := insertPendingRuntimeOnboarding(t, a, "stale", "eeeeeeee-ffff-4aaa-8bbb-cccccccccccc")
	reader := &fakeRuntimeOnboardingResultReader{result: runtimeOnboardingResult{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: 1, SlotID: "different-slot", ExecutionEpoch: 7,
		AuthType: "oauth", ProjectedAt: time.Now().UTC(),
	}}
	stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
	if err != nil || stats.Completed != 0 || stats.Failed != 1 {
		t.Fatalf("stale stats=%+v err=%v", stats, err)
	}
	var runtimeStatus string
	if err := a.db.QueryRow(`SELECT runtime_status FROM accounts WHERE id = ?`, accountID).Scan(&runtimeStatus); err != nil {
		t.Fatal(err)
	}
	if runtimeStatus != "provisioning" {
		t.Fatalf("stale result changed status to %s", runtimeStatus)
	}
}

func insertPendingRuntimeOnboarding(t *testing.T, a *app, name, intentID string) (int64, string, string) {
	t.Helper()
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, platform, auth_type, credentials_json, auth_status, execution_migration_status, runtime_status,
		runtime_generation, runtime_slot_id, runtime_provider, schedulable)
		VALUES (?, 'anthropic', 'oauth', '{}', 'unknown', 'migrating', 'provisioning', 1, '', 'docker', 0)`, name)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	slotID := "ccmax-account-" + fmtInt64(accountID)
	if _, err := a.db.Exec(`UPDATE accounts SET runtime_slot_id = ? WHERE id = ?`, slotID, accountID); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"onboarding_intent_id": intentID})
	if _, err := a.db.Exec(`INSERT INTO runtime_outbox
		(event_id, account_id, event_type, desired_generation, payload_json)
		VALUES (?, ?, 'account.runtime.provision_requested', 1, ?)`, "event-"+fmtInt64(accountID), accountID, string(payload)); err != nil {
		t.Fatal(err)
	}
	return accountID, intentID, slotID
}

func fmtInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}

func containsRuntimeCredentialMaterial(value string) bool {
	for _, key := range []string{"access_token", "refresh_token", "api_key", "session_key", "cookie"} {
		if strings.Contains(strings.ToLower(value), key) {
			return true
		}
	}
	return false
}
