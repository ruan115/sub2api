package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/ccmax-manager/internal/executionv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRuntimeOnboardingStatusFindsCanonicalSubmissionWithoutExposingKeys(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, accountID := newRuntimeOnboardingResumeTestAccount(t, "status", "migrated", "ready", 2, 1)
	defer a.db.Close()
	seedRuntimeOnboardingSubmission(t, a, "lost-status-external-key", accountID, 3,
		"account.credential.rotate_requested", "migrated", "session_key", "oauth")
	submission, err := a.getRuntimeOnboardingSubmission(context.Background(), "lost-status-external-key")
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet,
		"/api/accounts/"+strconv.FormatInt(accountID, 10)+"/runtime-onboarding", nil)
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status response = %d/%s", response.Code, response.Body.String())
	}
	var decoded runtimeOnboardingStatusResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.AccountID != accountID || decoded.Status != runtimeOnboardingSubmissionPending ||
		decoded.DesiredGeneration != 3 || decoded.SourceType != "session_key" || decoded.AuthType != "oauth" ||
		!decoded.MayRequireMaterial || decoded.ResumeURL == "" {
		t.Fatalf("runtime onboarding status = %+v", decoded)
	}
	if strings.Contains(response.Body.String(), submission.IdempotencyKey) ||
		strings.Contains(response.Body.String(), submission.IntakeIdempotencyKey) {
		t.Fatalf("status exposed canonical keys: %s", response.Body.String())
	}
}

func TestRuntimeOnboardingResumeUsesCanonicalKeysAfterExternalKeyLoss(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, accountID := newRuntimeOnboardingResumeTestAccount(t, "lost-key", "migrated", "ready", 2, 1)
	defer a.db.Close()
	seedRuntimeOnboardingSubmission(t, a, "lost-reauth-external-key", accountID, 3,
		"account.credential.rotate_requested", "migrated", "session_key", "oauth")
	initial, err := a.getRuntimeOnboardingSubmission(context.Background(), "lost-reauth-external-key")
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeOnboardingIntakeRPC{respond: func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
		return runtimeOnboardingResumeReceipt(request, "resume-created-intent")
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}

	response := postRuntimeOnboardingResume(t, a, accountID, map[string]any{
		"onboarding_source": "session_key", "auth_type": "oauth",
		"onboarding_secret": "replacement-session-material",
	}, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("resume response = %d/%s", response.Code, response.Body.String())
	}
	if service.recoverCalls != 1 || service.calls != 1 ||
		service.recoverRequest.GetIdempotencyKey() != initial.IntakeIdempotencyKey ||
		service.request.GetIdempotencyKey() != initial.IntakeIdempotencyKey ||
		service.request.GetIdempotencyKey() == initial.IdempotencyKey {
		t.Fatalf("resume recovery/create = %+v/%+v calls=%d/%d", service.recoverRequest, service.request, service.recoverCalls, service.calls)
	}
	stored, err := a.getRuntimeOnboardingSubmission(context.Background(), initial.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != runtimeOnboardingSubmissionQueued || stored.IntakeIdempotencyKey != initial.IntakeIdempotencyKey {
		t.Fatalf("stored canonical submission = %+v", stored)
	}
	var submissionCount, outboxCount, grantCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions WHERE account_id = ?`, accountID).Scan(&submissionCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?
		AND event_type IN ('account.runtime.provision_requested', 'account.credential.migrate_requested', 'account.credential.rotate_requested')`, accountID).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = ?`,
		accountID, runtimeProxyReservationGranted).Scan(&grantCount); err != nil {
		t.Fatal(err)
	}
	if submissionCount != 1 || outboxCount != 1 || grantCount != 1 {
		t.Fatalf("submission/outbox/grant count = %d/%d/%d", submissionCount, outboxCount, grantCount)
	}
	var auditBody, auditAction string
	if err := a.db.QueryRow(`SELECT request_body, action FROM audit_logs
		WHERE path = ? ORDER BY id DESC LIMIT 1`,
		"/api/accounts/"+strconv.FormatInt(accountID, 10)+"/runtime-onboarding/resume").Scan(&auditBody, &auditAction); err != nil {
		t.Fatal(err)
	}
	if auditAction != "account.runtime_onboarding_resume" || strings.Contains(auditBody, "replacement-session-material") ||
		!strings.Contains(auditBody, "[REDACTED]") {
		t.Fatalf("resume audit action/body = %q/%s", auditAction, auditBody)
	}
}

func TestRuntimeOnboardingResumeRecoversReceiptWithoutMaterial(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, accountID := newRuntimeOnboardingResumeTestAccount(t, "recover", "migrated", "ready", 2, 1)
	defer a.db.Close()
	seedRuntimeOnboardingSubmission(t, a, "lost-receipt-external-key", accountID, 3,
		"account.credential.rotate_requested", "migrated", "oauth_code", "oauth")
	initial, err := a.getRuntimeOnboardingSubmission(context.Background(), "lost-receipt-external-key")
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeOnboardingIntakeRPC{recoverResponse: &executionv1.CreateOnboardingIntentResponse{
		IntentId: "resume-recovered-intent", AccountId: strconv.FormatInt(accountID, 10), DesiredGeneration: 3,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}

	response := postRuntimeOnboardingResume(t, a, accountID, map[string]any{}, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("receipt recovery response = %d/%s", response.Code, response.Body.String())
	}
	stored, err := a.getRuntimeOnboardingSubmission(context.Background(), initial.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != runtimeOnboardingSubmissionQueued || service.recoverCalls != 1 || service.calls != 0 {
		t.Fatalf("recovered submission/calls = %+v/%d/%d", stored, service.recoverCalls, service.calls)
	}
}

func TestRuntimeOnboardingResumeAdvancesOnlyExactExpiredAttempt(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, accountID := newRuntimeOnboardingResumeTestAccount(t, "expired", "migrated", "ready", 2, 1)
	defer a.db.Close()
	seedRuntimeOnboardingSubmission(t, a, "lost-expired-external-key", accountID, 3,
		"account.credential.rotate_requested", "migrated", "session_key", "oauth")
	initial, err := a.getRuntimeOnboardingSubmission(context.Background(), "lost-expired-external-key")
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeOnboardingIntakeRPC{
		recoverRespond: func(request *executionv1.RecoverOnboardingIntentRequest) (*executionv1.CreateOnboardingIntentResponse, error) {
			if request.GetIdempotencyKey() == initial.IntakeIdempotencyKey {
				return nil, status.Error(codes.Aborted, "expired")
			}
			return nil, status.Error(codes.NotFound, "new attempt")
		},
		respond: func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
			return runtimeOnboardingResumeReceipt(request, "resume-rotated-intent")
		},
	}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}

	response := postRuntimeOnboardingResume(t, a, accountID, map[string]any{
		"onboarding_secret": "replacement-after-expiry",
	}, "")
	if response.Code != http.StatusAccepted {
		t.Fatalf("expired resume response = %d/%s", response.Code, response.Body.String())
	}
	stored, err := a.getRuntimeOnboardingSubmission(context.Background(), initial.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != runtimeOnboardingSubmissionQueued || stored.IntakeAttempt != initial.IntakeAttempt+1 ||
		stored.IntakeIdempotencyKey == initial.IntakeIdempotencyKey ||
		service.request.GetIdempotencyKey() != stored.IntakeIdempotencyKey || service.recoverCalls != 2 || service.calls != 1 {
		t.Fatalf("expired attempt submission/calls = %+v/%d/%d", stored, service.recoverCalls, service.calls)
	}
}

func TestRuntimeOnboardingResumeMapsRecoveryFailuresWithoutMutation(t *testing.T) {
	for _, test := range []struct {
		name       string
		err        error
		wantStatus int
	}{
		{"conflict", status.Error(codes.FailedPrecondition, "claimed"), http.StatusConflict},
		{"unavailable", status.Error(codes.Unavailable, "down"), http.StatusServiceUnavailable},
		{"timeout", status.Error(codes.DeadlineExceeded, "late"), http.StatusGatewayTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			a, accountID := newRuntimeOnboardingResumeTestAccount(t, test.name, "migrated", "ready", 2, 1)
			defer a.db.Close()
			seedRuntimeOnboardingSubmission(t, a, "lost-failure-"+test.name, accountID, 3,
				"account.credential.rotate_requested", "migrated", "session_key", "oauth")
			initial, err := a.getRuntimeOnboardingSubmission(context.Background(), "lost-failure-"+test.name)
			if err != nil {
				t.Fatal(err)
			}
			service := &fakeOnboardingIntakeRPC{recoverErr: test.err}
			a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
			response := postRuntimeOnboardingResume(t, a, accountID, map[string]any{}, "")
			if response.Code != test.wantStatus {
				t.Fatalf("failure response = %d/%s, want %d", response.Code, response.Body.String(), test.wantStatus)
			}
			stored, err := a.getRuntimeOnboardingSubmission(context.Background(), initial.IdempotencyKey)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != runtimeOnboardingSubmissionPending || stored.IntakeAttempt != initial.IntakeAttempt ||
				stored.IntakeIdempotencyKey != initial.IntakeIdempotencyKey || service.calls != 0 {
				t.Fatalf("failure mutated submission/calls = %+v/%d", stored, service.calls)
			}
		})
	}
}

func TestRuntimeOnboardingResumeRejectsClientKeyAndCompletedQueuedSubmission(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, accountID := newRuntimeOnboardingResumeTestAccount(t, "completed", "migrated", "ready", 2, 1)
	defer a.db.Close()
	seedRuntimeOnboardingSubmission(t, a, "completed-external-key", accountID, 3,
		"account.credential.rotate_requested", "migrated", "session_key", "oauth")
	submission, err := a.getRuntimeOnboardingSubmission(context.Background(), "completed-external-key")
	if err != nil {
		t.Fatal(err)
	}
	receipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey: submission.IntakeIdempotencyKey, IntentID: "completed-intent", AccountID: accountID,
		DesiredGeneration: 3, ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	if _, err := a.persistRuntimeOnboardingReceipt(context.Background(), submission.IdempotencyKey, submission, receipt, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.requestRuntimeOnboardingTransition(context.Background(), runtimeTransitionRequest{
		AccountID: accountID, EventType: submission.EventType, MigrationStatus: submission.MigrationStatus,
		RuntimeStatus: "provisioning", OnboardingKey: submission.IdempotencyKey,
	}, receipt); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET runtime_status = 'ready', schedulable = 1 WHERE id = ?`, accountID); err != nil {
		t.Fatal(err)
	}
	service := &fakeOnboardingIntakeRPC{}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}

	statusRequest := httptest.NewRequest(http.MethodGet,
		"/api/accounts/"+strconv.FormatInt(accountID, 10)+"/runtime-onboarding", nil)
	statusResponse := httptest.NewRecorder()
	a.routes().ServeHTTP(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusConflict {
		t.Fatalf("completed status response = %d/%s", statusResponse.Code, statusResponse.Body.String())
	}

	withKey := postRuntimeOnboardingResume(t, a, accountID, map[string]any{}, "replacement-client-key")
	if withKey.Code != http.StatusBadRequest {
		t.Fatalf("client key response = %d/%s", withKey.Code, withKey.Body.String())
	}
	completed := postRuntimeOnboardingResume(t, a, accountID, map[string]any{}, "")
	if completed.Code != http.StatusConflict || service.recoverCalls != 0 || service.calls != 0 {
		t.Fatalf("completed resume response/calls = %d/%s/%d/%d", completed.Code, completed.Body.String(), service.recoverCalls, service.calls)
	}
}

func TestRuntimeOnboardingResumeRejectsLifecycleTransition(t *testing.T) {
	for _, runtimeStatus := range []string{"provisioning", "draining", "destroying", "archived", "deleted"} {
		t.Run(runtimeStatus, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			a, accountID := newRuntimeOnboardingResumeTestAccount(t, "lifecycle-"+runtimeStatus, "migrated", runtimeStatus, 2, 0)
			defer a.db.Close()
			seedRuntimeOnboardingSubmission(t, a, "lifecycle-"+runtimeStatus+"-key", accountID, 3,
				"account.credential.rotate_requested", "migrated", "session_key", "oauth")
			service := &fakeOnboardingIntakeRPC{}
			a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}

			response := postRuntimeOnboardingResume(t, a, accountID, map[string]any{
				"onboarding_secret": "must-not-cross-lifecycle-fence",
			}, "")
			if response.Code != http.StatusConflict || service.recoverCalls != 0 || service.calls != 0 {
				t.Fatalf("lifecycle response/calls = %d/%s/%d/%d", response.Code, response.Body.String(), service.recoverCalls, service.calls)
			}
		})
	}
}

func TestRuntimeOnboardingCreateFailureReturnsResumeHandleAndResumesSameAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-create-resume.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	service := &fakeOnboardingIntakeRPC{err: status.Error(codes.Unavailable, "down")}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	payload := map[string]any{
		"name": "pending-create-resume", "platform": "anthropic", "auth_type": "oauth",
		"execution_onboarding": true, "onboarding_source": "session_key",
		"onboarding_secret": "first-create-material", "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "rate_multiplier": 1,
		"group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}
	encoded, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "lost-create-external-key")
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("create failure response = %d/%s", response.Code, response.Body.String())
	}
	var failure struct {
		PendingAccountID int64  `json:"pending_account_id"`
		ResumeURL        string `json:"resume_url"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.PendingAccountID <= 0 || failure.ResumeURL == "" {
		t.Fatalf("create failure omitted resume handle: %s", response.Body.String())
	}

	service.err = nil
	service.respond = func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
		return runtimeOnboardingResumeReceipt(request, "create-resumed-intent")
	}
	resumed := postRuntimeOnboardingResume(t, a, failure.PendingAccountID, map[string]any{
		"onboarding_secret": "replacement-create-material",
	}, "")
	if resumed.Code != http.StatusAccepted {
		t.Fatalf("create resume response = %d/%s", resumed.Code, resumed.Body.String())
	}
	var accounts, submissions, outbox, grants int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts`).Scan(&accounts); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions`).Scan(&submissions); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox
		WHERE event_type IN ('account.runtime.provision_requested', 'account.credential.migrate_requested', 'account.credential.rotate_requested')`).Scan(&outbox); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE event_type = ?`, runtimeProxyReservationGranted).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if accounts != 1 || submissions != 1 || outbox != 1 || grants != 1 {
		t.Fatalf("create resume accounts/submissions/outbox/grants = %d/%d/%d/%d", accounts, submissions, outbox, grants)
	}
}

func newRuntimeOnboardingResumeTestAccount(
	t *testing.T,
	name, migrationStatus, runtimeStatus string,
	generation uint64,
	schedulable int,
) (*app, int64) {
	t.Helper()
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-resume-"+name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, platform, auth_type, credentials_json, execution_migration_status, runtime_status,
		 runtime_generation, schedulable)
		VALUES (?, 'anthropic', 'oauth', '{}', ?, ?, ?, ?)`, name, migrationStatus, runtimeStatus, generation, schedulable)
	if err != nil {
		a.db.Close()
		t.Fatal(err)
	}
	accountID, err := result.LastInsertId()
	if err != nil {
		a.db.Close()
		t.Fatal(err)
	}
	return a, accountID
}

func postRuntimeOnboardingResume(
	t *testing.T,
	a *app,
	accountID int64,
	payload map[string]any,
	idempotencyKey string,
) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost,
		"/api/accounts/"+strconv.FormatInt(accountID, 10)+"/runtime-onboarding/resume", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	return response
}

func runtimeOnboardingResumeReceipt(
	request *executionv1.CreateOnboardingIntentRequest,
	intentID string,
) *executionv1.CreateOnboardingIntentResponse {
	return &executionv1.CreateOnboardingIntentResponse{
		IntentId: intentID, AccountId: request.GetAccountId(), DesiredGeneration: request.GetDesiredGeneration(),
		Source: request.GetSource(), AuthType: request.GetAuthType(),
		ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
	}
}
