package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/ccmax-manager/internal/executionv1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestExecutionOAuthCallbackReplaysQueuedWithoutSessionOrIntake(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "oauth-queued-replay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID := createExecutionOAuthReplayAccount(t, a, "oauth-queued-replay")
	service := executionOAuthReplayService()
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	handler := a.routes()
	sessionID := startExecutionOAuthReplaySession(t, handler, accountID)

	first := exchangeExecutionOAuthReplay(handler, accountID, "oauth-replay-key", sessionID, "oauth-code#state")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first callback status/body = %d/%s", first.Code, first.Body.String())
	}
	firstResult := decodeExecutionOAuthReplayResult(t, first)
	if _, ok := a.oauthSessions.peek(sessionID, accountID); ok {
		t.Fatal("committed callback retained OAuth session")
	}

	// Simulate a lost HTTP response plus a temporarily unavailable intake. The
	// durable queued record is enough to reproduce the original response.
	a.onboardingIntake = nil
	replay := exchangeExecutionOAuthReplay(handler, accountID, "oauth-replay-key", "missing-session", "")
	if replay.Code != http.StatusAccepted {
		t.Fatalf("queued replay status/body = %d/%s", replay.Code, replay.Body.String())
	}
	replayResult := decodeExecutionOAuthReplayResult(t, replay)
	if replayResult != firstResult || service.calls != 1 {
		t.Fatalf("queued replay result/calls = %+v/%d, want %+v/1", replayResult, service.calls, firstResult)
	}
	var submissions, events, grants int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions WHERE idempotency_key = ?`, "oauth-replay-key").Scan(&submissions); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?
		AND event_type IN ('account.runtime.provision_requested', 'account.credential.migrate_requested', 'account.credential.rotate_requested')`, accountID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = ?`,
		accountID, runtimeProxyReservationGranted).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if submissions != 1 || events != 1 || grants != 1 {
		t.Fatalf("durable replay rows submissions/events/grants = %d/%d/%d", submissions, events, grants)
	}
}

func TestExecutionOAuthCallbackRecoversReceiptWithoutPKCESession(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "oauth-receipt-recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID := createExecutionOAuthReplayAccount(t, a, "oauth-receipt-recovery")
	seedRuntimeOnboardingSubmission(t, a, "oauth-receipt-recovery-key", accountID, 3,
		"account.credential.rotate_requested", "migrated", "oauth_code", "oauth")
	submission, err := a.getRuntimeOnboardingSubmission(context.Background(), "oauth-receipt-recovery-key")
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeOnboardingIntakeRPC{recoverResponse: &executionv1.CreateOnboardingIntentResponse{
		IntentId: "oauth-recovered-intent", AccountId: strconv.FormatInt(accountID, 10), DesiredGeneration: 3,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	replay := exchangeExecutionOAuthReplay(a.routes(), accountID, "oauth-receipt-recovery-key", "lost-session", "")
	if replay.Code != http.StatusAccepted {
		t.Fatalf("recovered callback status/body = %d/%s", replay.Code, replay.Body.String())
	}
	stored, err := a.getRuntimeOnboardingSubmission(context.Background(), "oauth-receipt-recovery-key")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != runtimeOnboardingSubmissionQueued || stored.IntakeIdempotencyKey != submission.IntakeIdempotencyKey ||
		service.recoverCalls != 1 || service.calls != 0 {
		t.Fatalf("recovered OAuth submission/calls = %+v/%d/%d", stored, service.recoverCalls, service.calls)
	}
}

func TestExecutionOAuthCallbackRecoveryDiscardsRetainedPKCESession(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "oauth-recovery-discards-session.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID := createExecutionOAuthReplayAccount(t, a, "oauth-recovery-discards-session")
	seedRuntimeOnboardingSubmission(t, a, "oauth-recovery-discards-session-key", accountID, 3,
		"account.credential.rotate_requested", "migrated", "oauth_code", "oauth")
	service := &fakeOnboardingIntakeRPC{recoverResponse: &executionv1.CreateOnboardingIntentResponse{
		IntentId: "oauth-recovered-session-intent", AccountId: strconv.FormatInt(accountID, 10), DesiredGeneration: 3,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	handler := a.routes()
	sessionID := startExecutionOAuthReplaySession(t, handler, accountID)

	replay := exchangeExecutionOAuthReplay(handler, accountID, "oauth-recovery-discards-session-key", sessionID, "")
	if replay.Code != http.StatusAccepted {
		t.Fatalf("recovered callback status/body = %d/%s", replay.Code, replay.Body.String())
	}
	if _, ok := a.oauthSessions.peek(sessionID, accountID); ok {
		t.Fatal("durable receipt recovery retained PKCE session")
	}
	if service.recoverCalls != 1 || service.calls != 0 {
		t.Fatalf("recovery/create calls = %d/%d, want 1/0", service.recoverCalls, service.calls)
	}
}

func TestExecutionOAuthCallbackPreservesSessionAcrossPendingFailure(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "oauth-pending-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountID := createExecutionOAuthReplayAccount(t, a, "oauth-pending-retry")
	service := executionOAuthReplayService()
	service.err = errors.New("temporary intake outage")
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	handler := a.routes()
	sessionID := startExecutionOAuthReplaySession(t, handler, accountID)

	first := exchangeExecutionOAuthReplay(handler, accountID, "oauth-pending-key", sessionID, "oauth-code#state")
	if first.Code != http.StatusBadGateway {
		t.Fatalf("failed callback status/body = %d/%s", first.Code, first.Body.String())
	}
	if _, ok := a.oauthSessions.peek(sessionID, accountID); !ok {
		t.Fatal("pending intake failure consumed OAuth session")
	}
	var status string
	if err := a.db.QueryRow(`SELECT status FROM runtime_onboarding_submissions WHERE idempotency_key = ?`, "oauth-pending-key").Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != runtimeOnboardingSubmissionPending {
		t.Fatalf("submission status = %q, want pending", status)
	}

	service.err = nil
	retry := exchangeExecutionOAuthReplay(handler, accountID, "oauth-pending-key", sessionID, "oauth-code#state")
	if retry.Code != http.StatusAccepted {
		t.Fatalf("pending retry status/body = %d/%s", retry.Code, retry.Body.String())
	}
	if _, ok := a.oauthSessions.peek(sessionID, accountID); ok {
		t.Fatal("successful pending retry retained OAuth session")
	}
	if service.calls != 2 {
		t.Fatalf("intake calls = %d, want 2", service.calls)
	}
}

func TestExecutionOAuthCallbackRejectsKeyAccountConflictWithoutConsumingSession(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "oauth-key-account-conflict.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	firstAccountID := createExecutionOAuthReplayAccount(t, a, "oauth-conflict-first")
	secondAccountID := createExecutionOAuthReplayAccount(t, a, "oauth-conflict-second")
	service := executionOAuthReplayService()
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	handler := a.routes()
	firstSession := startExecutionOAuthReplaySession(t, handler, firstAccountID)
	first := exchangeExecutionOAuthReplay(handler, firstAccountID, "oauth-conflict-key", firstSession, "first-code#state")
	if first.Code != http.StatusAccepted {
		t.Fatalf("first callback status/body = %d/%s", first.Code, first.Body.String())
	}

	secondSession := startExecutionOAuthReplaySession(t, handler, secondAccountID)
	conflict := exchangeExecutionOAuthReplay(handler, secondAccountID, "oauth-conflict-key", secondSession, "second-code#state")
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflicting callback status/body = %d/%s", conflict.Code, conflict.Body.String())
	}
	if _, ok := a.oauthSessions.peek(secondSession, secondAccountID); !ok {
		t.Fatal("idempotency conflict consumed unrelated account OAuth session")
	}
	retry := exchangeExecutionOAuthReplay(handler, secondAccountID, "oauth-second-key", secondSession, "second-code#state")
	if retry.Code != http.StatusAccepted {
		t.Fatalf("post-conflict retry status/body = %d/%s", retry.Code, retry.Body.String())
	}
	if service.calls != 2 {
		t.Fatalf("intake calls = %d, want one per account", service.calls)
	}
}

type executionOAuthReplayResponse struct {
	Status            string `json:"status"`
	EventID           string `json:"event_id"`
	DesiredGeneration uint64 `json:"desired_generation"`
}

func createExecutionOAuthReplayAccount(t *testing.T, a *app, name string) int64 {
	t.Helper()
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, auth_type, credentials_json, proxy_id, execution_migration_status, runtime_status,
		runtime_generation, runtime_slot_id, runtime_provider, runtime_execution_epoch, schedulable)
		VALUES (?, 'oauth', '{}', ?, 'migrated', 'ready', 2, ?, 'docker', 9, 1)`, name, proxyID, "slot-"+name)
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return accountID
}

func startExecutionOAuthReplaySession(t *testing.T, handler http.Handler, accountID int64) string {
	t.Helper()
	var authorization struct {
		SessionID string `json:"session_id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/auth-url",
		map[string]any{"mode": "oauth"}, http.StatusOK, &authorization)
	if authorization.SessionID == "" {
		t.Fatal("authorization response omitted session id")
	}
	return authorization.SessionID
}

func exchangeExecutionOAuthReplay(handler http.Handler, accountID int64, idempotencyKey, sessionID, code string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"session_id": sessionID, "code": code})
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/oauth-exchange", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeExecutionOAuthReplayResult(t *testing.T, response *httptest.ResponseRecorder) executionOAuthReplayResponse {
	t.Helper()
	var decoded executionOAuthReplayResponse
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func executionOAuthReplayService() *fakeOnboardingIntakeRPC {
	return &fakeOnboardingIntakeRPC{respond: func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
		return &executionv1.CreateOnboardingIntentResponse{
			IntentId: "intent-oauth-account-" + request.GetAccountId(), AccountId: request.GetAccountId(),
			DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
			ExpiresAt: timestamppb.New(time.Now().UTC().Add(30 * time.Minute)),
		}
	}}
}
