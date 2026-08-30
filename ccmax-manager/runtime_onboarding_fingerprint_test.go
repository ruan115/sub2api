package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/ccmax-manager/internal/executionv1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRuntimeOnboardingCreateFingerprintIsCanonicalAndExcludesSensitiveFreeFormFields(t *testing.T) {
	schedulable := true
	poolID, proxyID := int64(1), int64(7)
	first := accountInput{
		Name: "first-display-name", Platform: "anthropic", AuthType: "oauth",
		SessionKey: "session-key-one", OnboardingSecret: "onboarding-secret-one", OnboardingAuxiliary: "auxiliary-one",
		Credentials: json.RawMessage(`{"access_token":"credential-one"}`),
		Extra:       json.RawMessage(`{"potentially_secret":"extra-one"}`),
		Status:      "active", Schedulable: &schedulable, Concurrency: 3, Priority: 10,
		RateMultiplier: 1, Notes: "note-one", ErrorMessage: "error-one",
		ExpiresAt: "free-form-one", RateLimitResetAt: "free-form-reset-one",
		GroupIDs: []string{"b", "a", "a"}, ProxyPoolID: &poolID, ProxyID: &proxyID,
		ProxyText: "http://proxy-user:proxy-password@127.0.0.1:18080",
		BaseRPM:   12, RPMStickyBuffer: 3, AccountPrice: 0.25,
	}
	if _, _, err := normalizeAccountInput(&first, "{}", "{}"); err != nil {
		t.Fatal(err)
	}
	secondSchedulable := false
	secondPoolID, secondProxyID := int64(1), int64(7)
	second := accountInput{
		Name: "second-display-name", Platform: "anthropic", AuthType: "oauth",
		SessionKey: "session-key-two", OnboardingSecret: "onboarding-secret-two", OnboardingAuxiliary: "auxiliary-two",
		Credentials: json.RawMessage(`{"api_key":"credential-two"}`),
		Extra:       json.RawMessage(`{"potentially_secret":"extra-two"}`),
		Status:      "active", Schedulable: &secondSchedulable, Concurrency: 3, Priority: 10,
		RateMultiplier: 1, Notes: "note-two", ErrorMessage: "error-two",
		ExpiresAt: "free-form-two", RateLimitResetAt: "free-form-reset-two",
		GroupIDs: []string{"a", "b"}, ProxyPoolID: &secondPoolID, ProxyID: &secondProxyID,
		ProxyText: "socks5://other-user:other-password@127.0.0.1:19090",
		BaseRPM:   12, RPMStickyBuffer: 3, AccountPrice: 0.25,
	}
	if _, _, err := normalizeAccountInput(&second, "{}", "{}"); err != nil {
		t.Fatal(err)
	}
	firstMaterial := &runtimeOnboardingMaterial{
		Source: "session_key", AuthType: "oauth", Secret: []byte("first-secret"), Auxiliary: []byte("first-auxiliary"),
	}
	secondMaterial := &runtimeOnboardingMaterial{
		Source: "session_key", AuthType: "oauth", Secret: []byte("second-secret"), Auxiliary: []byte("second-auxiliary"),
	}
	firstDigest, err := runtimeOnboardingCreateFingerprint(&first, firstMaterial, 0)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := runtimeOnboardingCreateFingerprint(&second, secondMaterial, 0)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("excluded fields or group order changed digest: %s != %s", hex.EncodeToString(firstDigest[:]), hex.EncodeToString(secondDigest[:]))
	}

	second.Priority++
	changedDigest, err := runtimeOnboardingCreateFingerprint(&second, secondMaterial, 0)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == firstDigest {
		t.Fatal("typed account configuration change did not change digest")
	}
}

func TestAccountCreateRuntimeFingerprintRejectsMismatchAndReplaysExcludedChanges(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-create-fingerprint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	service := &fakeOnboardingIntakeRPC{respond: func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
		return &executionv1.CreateOnboardingIntentResponse{
			IntentId: "fingerprint-intent-10380", AccountId: request.GetAccountId(),
			DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
			ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
		}
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	basePayload := func(secret string) map[string]any {
		return map[string]any{
			"name": "fingerprint-create", "platform": "anthropic", "auth_type": "oauth",
			"execution_onboarding": true, "onboarding_source": "session_key", "onboarding_secret": secret,
			"extra": map[string]any{}, "status": "active", "schedulable": true, "concurrency": 2,
			"priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"}, "rpm_strategy": "tiered",
			"user_msg_queue_mode": "off", "proxy_pool_id": 1, "proxy_id": proxyID,
		}
	}
	post := func(payload map[string]any) *httptest.ResponseRecorder {
		encoded, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "fingerprint-create-key")
		response := httptest.NewRecorder()
		a.routes().ServeHTTP(response, request)
		return response
	}

	createdResponse := post(basePayload("first-runtime-secret"))
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("initial create status/body = %d/%s", createdResponse.Code, createdResponse.Body.String())
	}
	var created account
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var version int64
	var storedDigest []byte
	if err := a.db.QueryRow(`SELECT request_fingerprint_version, request_fingerprint_sha256
		FROM runtime_onboarding_submissions WHERE idempotency_key = ?`, "fingerprint-create-key").Scan(&version, &storedDigest); err != nil {
		t.Fatal(err)
	}
	if version != runtimeOnboardingCreateFingerprintVersion || len(storedDigest) != runtimeOnboardingFingerprintSize {
		t.Fatalf("stored fingerprint version/size = %d/%d", version, len(storedDigest))
	}

	mismatch := basePayload("mismatched-secret-must-not-be-submitted")
	mismatch["priority"] = 11
	mismatchResponse := post(mismatch)
	if mismatchResponse.Code != http.StatusConflict {
		t.Fatalf("mismatched replay status/body = %d/%s", mismatchResponse.Code, mismatchResponse.Body.String())
	}
	if service.calls != 1 {
		t.Fatalf("mismatched replay reached intake: calls=%d", service.calls)
	}

	exact := basePayload("replacement-secret-must-not-be-submitted")
	exact["name"] = "different-display-name"
	exact["notes"] = "different free-form note"
	exact["error_message"] = "different free-form error"
	exact["expires_at"] = "different free-form expiry"
	exact["rate_limit_reset_at"] = "different free-form reset"
	exact["extra"] = map[string]any{"different_free_form": "value"}
	if _, err := a.db.Exec(`UPDATE proxies SET status = 'disabled' WHERE id = ?`, proxyID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE proxy_pools SET status = 'disabled' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	exactResponse := post(exact)
	if exactResponse.Code != http.StatusOK {
		t.Fatalf("exact replay with excluded changes status/body = %d/%s", exactResponse.Code, exactResponse.Body.String())
	}
	var replayed account
	if err := json.Unmarshal(exactResponse.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	var accountCount, submissionCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions`).Scan(&submissionCount); err != nil {
		t.Fatal(err)
	}
	if replayed.ID != created.ID || accountCount != 1 || submissionCount != 1 || service.calls != 1 {
		t.Fatalf("replay id/accounts/submissions/calls = %d/%d/%d/%d", replayed.ID, accountCount, submissionCount, service.calls)
	}
}

func TestAccountCreateExecutionOnboardingRejectsProxyTextBeforeSideEffects(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-create-proxy-text.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	service := &fakeOnboardingIntakeRPC{}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	var proxiesBefore int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxies`).Scan(&proxiesBefore); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"name": "proxy-text-rejected", "platform": "anthropic", "auth_type": "oauth",
		"execution_onboarding": true, "onboarding_secret": "temporary-runtime-secret",
		"concurrency": 1, "rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1,
		"proxy_text": "http://proxy-user:proxy-password@127.0.0.1:18080",
	}
	encoded, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "proxy-text-rejected-key")
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "proxy_text") {
		t.Fatalf("proxy_text status/body = %d/%s", response.Code, response.Body.String())
	}
	var accounts, submissions, proxiesAfter int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions`).Scan(&submissions); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxies`).Scan(&proxiesAfter); err != nil {
		t.Fatal(err)
	}
	if accounts != 0 || submissions != 0 || proxiesAfter != proxiesBefore || service.calls != 0 {
		t.Fatalf("proxy_text produced side effects: accounts/submissions/proxies/calls=%d/%d/%d->%d/%d",
			accounts, submissions, proxiesBefore, proxiesAfter, service.calls)
	}
}

func TestAccountCreateRuntimeFingerprintRejectsLegacySubmission(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-create-legacy-fingerprint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, platform, auth_type, credentials_json, status, schedulable, concurrency, rate_multiplier,
		 proxy_pool_id, proxy_id, execution_migration_status, runtime_status)
		VALUES ('legacy-fingerprint', 'anthropic', 'oauth', '{}', 'active', 0, 1, 1, 1, ?, 'migrating', 'provisioning')`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO runtime_onboarding_submissions
		(idempotency_key, intake_idempotency_key, operation_type, account_id, desired_generation,
		 event_type, migration_status, source_type, auth_type, proxy_id, status)
		VALUES ('legacy-create-key', 'legacy-create-intake-key', 'account_create', ?, 1,
		 'account.runtime.provision_requested', 'migrating', 'session_key', 'oauth', ?, 'pending')`, accountID, proxyID); err != nil {
		t.Fatal(err)
	}
	service := &fakeOnboardingIntakeRPC{}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	payload := map[string]any{
		"name": "legacy-fingerprint", "platform": "anthropic", "auth_type": "oauth",
		"execution_onboarding": true, "onboarding_secret": "legacy-replay-secret",
		"status": "active", "concurrency": 1, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID,
	}
	encoded, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "legacy-create-key")
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	if response.Code != http.StatusConflict || service.calls != 0 {
		t.Fatalf("legacy replay status/body/calls = %d/%s/%d", response.Code, response.Body.String(), service.calls)
	}
	var accountCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 1 {
		t.Fatalf("legacy replay account count = %d", accountCount)
	}
}

func TestAccountCreateRuntimeFingerprintPreservesReplayDatabaseError(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-create-db-error.db"))
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeOnboardingIntakeRPC{}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	secret := []byte("direct-db-error-secret")
	material := &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: secret}
	_, _, found, replayErr := a.replayRuntimeOnboardingCreate(
		context.Background(), a.onboardingIntake, "db-error-create-key", material, [runtimeOnboardingFingerprintSize]byte{},
	)
	if !found || replayErr == nil || errors.Is(replayErr, errRuntimeOnboardingIdempotency) ||
		material.Secret != nil || !allRuntimeOnboardingZero(secret) {
		t.Fatalf("database replay classification/found/material = %v/%v/%v", replayErr, found, material)
	}
	payload := map[string]any{
		"name": "db-error-fingerprint", "platform": "anthropic", "auth_type": "oauth",
		"execution_onboarding": true, "onboarding_secret": "temporary-db-error-secret",
		"status": "active", "concurrency": 1, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": 1,
	}
	encoded, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "db-error-create-key")
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || service.calls != 0 {
		t.Fatalf("database error replay status/body/calls = %d/%s/%d", response.Code, response.Body.String(), service.calls)
	}
}

func TestRuntimeOnboardingFingerprintSchemaAndSQLiteUpgrade(t *testing.T) {
	for dialect, fragments := range map[string][]string{
		"sqlite": {
			"request_fingerprint_version INTEGER NOT NULL DEFAULT 0",
			"request_fingerprint_sha256 BLOB",
		},
		"mysql": {
			"request_fingerprint_version SMALLINT UNSIGNED NOT NULL DEFAULT 0",
			"request_fingerprint_sha256 VARBINARY(32) NULL",
		},
	} {
		schema := strings.Join(sqliteExecutionSchema(), "\n")
		if dialect == "mysql" {
			schema = strings.Join(mysqlExecutionSchema(), "\n")
		}
		for _, fragment := range fragments {
			if !strings.Contains(schema, fragment) {
				t.Fatalf("%s fingerprint schema is missing %q", dialect, fragment)
			}
		}
	}

	a, err := newApp(filepath.Join(t.TempDir(), "runtime-fingerprint-upgrade.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	for _, column := range []string{"request_fingerprint_sha256", "request_fingerprint_version"} {
		if _, err := a.db.Exec(`ALTER TABLE runtime_onboarding_submissions DROP COLUMN ` + column); err != nil {
			t.Fatalf("remove %s to simulate pre-fingerprint schema: %v", column, err)
		}
	}
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('legacy-upgrade', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	proxyID := createTestForwardProxy(t, a)
	if _, err := a.db.Exec(`UPDATE accounts SET proxy_id = ? WHERE id = ?`, proxyID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO runtime_onboarding_submissions
		(idempotency_key, intake_idempotency_key, operation_type, account_id, desired_generation,
		 event_type, migration_status, source_type, auth_type, proxy_id, status)
		VALUES ('legacy-upgrade-key', 'legacy-upgrade-intake', 'account_create', ?, 1,
		 'account.runtime.provision_requested', 'migrating', 'session_key', 'oauth', ?, 'pending')`, accountID, proxyID); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateExecutionFeatures(); err != nil {
		t.Fatal(err)
	}
	var version int64
	var digest []byte
	if err := a.db.QueryRow(`SELECT request_fingerprint_version, request_fingerprint_sha256
		FROM runtime_onboarding_submissions WHERE idempotency_key = 'legacy-upgrade-key'`).Scan(&version, &digest); err != nil {
		t.Fatal(err)
	}
	if version != 0 || len(digest) != 0 {
		t.Fatalf("legacy row was unsafely backfilled: version/digest=%d/%x", version, digest)
	}
	if _, err := a.getRuntimeOnboardingSubmission(context.Background(), "legacy-upgrade-key"); err != nil && !errors.Is(err, errRuntimeOnboardingIdempotency) {
		t.Fatalf("legacy row load classification = %v", err)
	}
	var intakeKey string
	var attempt uint64
	var expires int64
	if err := a.db.QueryRow(`SELECT intake_idempotency_key, intake_attempt, intent_expires_at_millis
		FROM runtime_onboarding_submissions WHERE idempotency_key = 'legacy-upgrade-key'`).Scan(&intakeKey, &attempt, &expires); err != nil {
		t.Fatal(err)
	}
	if intakeKey != "legacy-upgrade-intake" || attempt != 0 || expires != 0 {
		t.Fatalf("receipt recovery columns changed during fingerprint upgrade: %q/%d/%d", intakeKey, attempt, expires)
	}
}

func TestRuntimeOnboardingIntakeUniqueIndexMustBeExactlyOneColumn(t *testing.T) {
	if !exactMySQLUniqueIndexDefinition(true, []string{"intake_idempotency_key"}, "intake_idempotency_key") {
		t.Fatal("exact single-column unique intake-key index was rejected")
	}
	for _, candidate := range []struct {
		unique  bool
		columns []string
	}{
		{false, []string{"intake_idempotency_key"}},
		{true, []string{"intake_idempotency_key", "account_id"}},
		{true, []string{"account_id", "intake_idempotency_key"}},
		{true, nil},
	} {
		if exactMySQLUniqueIndexDefinition(candidate.unique, candidate.columns, "intake_idempotency_key") {
			t.Fatalf("incompatible intake-key index was accepted: unique=%v columns=%v", candidate.unique, candidate.columns)
		}
	}
}

func TestRuntimeOnboardingSubmissionRejectsMalformedFingerprintLength(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-malformed-fingerprint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json, proxy_id) VALUES ('malformed-fingerprint', '{}', ?)`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO runtime_onboarding_submissions
		(idempotency_key, intake_idempotency_key, operation_type, account_id, desired_generation,
		 event_type, migration_status, source_type, auth_type, proxy_id, status,
		 request_fingerprint_version, request_fingerprint_sha256)
		VALUES ('malformed-fingerprint-key', 'malformed-fingerprint-intake', 'account_create', ?, 1,
		 'account.runtime.provision_requested', 'migrating', 'session_key', 'oauth', ?, 'pending', 1, ?)`,
		accountID, proxyID, []byte("short")); err != nil {
		t.Fatal(err)
	}
	_, err = a.getRuntimeOnboardingSubmission(context.Background(), "malformed-fingerprint-key")
	if !errors.Is(err, errRuntimeOnboardingIdempotency) {
		t.Fatalf("malformed fingerprint error = %v", err)
	}
}
