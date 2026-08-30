package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/ccmax-manager/internal/executionv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestRuntimeOnboardingIntakeClientConsumesMaterialAndReturnsOpaqueReceipt(t *testing.T) {
	expires := time.Now().UTC().Add(30 * time.Minute)
	service := &fakeOnboardingIntakeRPC{response: &executionv1.CreateOnboardingIntentResponse{
		IntentId: "11111111-2222-4333-8444-555555555555", AccountId: "10380", DesiredGeneration: 7,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ExpiresAt: timestamppb.New(expires),
	}}
	client := &runtimeOnboardingIntakeClient{service: service}
	secret := []byte("ccmax-temporary-session")
	alias := secret
	material := &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: secret}
	receipt, err := client.Create(context.Background(), "event-10380", 10380, 7, material)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.IntentID != service.response.GetIntentId() || receipt.AccountID != 10380 || receipt.DesiredGeneration != 7 ||
		!receipt.ExpiresAt.Equal(expires) {
		t.Fatalf("intake receipt = %+v", receipt)
	}
	if material.Secret != nil || !allRuntimeOnboardingZero(alias) {
		t.Fatal("caller onboarding material was not erased")
	}
	if service.request == nil || string(service.request.GetSecret()) != "ccmax-temporary-session" || service.request.GetAccountId() != "10380" {
		t.Fatalf("intake RPC request = %+v", service.request)
	}
	encoded, err := json.Marshal(runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: []byte("must-not-log")})
	if err != nil || bytes.Contains(encoded, []byte("must-not-log")) || strings.Contains((runtimeOnboardingMaterial{Secret: []byte("must-not-log")}).String(), "must-not-log") {
		t.Fatalf("material redaction = %s/%v", encoded, err)
	}
}

func TestRuntimeOnboardingIntakeClientBoundsRPCAndDestroysMaterial(t *testing.T) {
	service := &blockingOnboardingIntakeRPC{}
	client := &runtimeOnboardingIntakeClient{service: service, rpcTimeout: 20 * time.Millisecond}
	secret := []byte("bounded-temporary-session")
	auxiliary := []byte("bounded-temporary-auxiliary")
	started := time.Now()
	_, err := client.Create(context.Background(), "bounded-intake-key", 10380, 7, &runtimeOnboardingMaterial{
		Source: "oauth_code", AuthType: "oauth", Secret: secret, Auxiliary: auxiliary,
	})
	if !errors.Is(err, errRuntimeOnboardingTimeout) {
		t.Fatalf("bounded create error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("bounded create exceeded deadline: %s", time.Since(started))
	}
	if !allRuntimeOnboardingZero(secret) || !allRuntimeOnboardingZero(auxiliary) {
		t.Fatal("bounded create retained caller-owned material")
	}
	if service.createDeadline.IsZero() || service.createDeadline.Sub(started) > 100*time.Millisecond {
		t.Fatalf("create RPC deadline = %s, started=%s", service.createDeadline, started)
	}

	client.rpcTimeout = 15 * time.Millisecond
	started = time.Now()
	_, err = client.Recover(context.Background(), "bounded-recover-key", 10380, 7, "session_key", "oauth")
	if !errors.Is(err, errRuntimeOnboardingTimeout) {
		t.Fatalf("bounded recover error = %v", err)
	}
	if time.Since(started) > time.Second || service.recoverDeadline.IsZero() ||
		service.recoverDeadline.Sub(started) > 100*time.Millisecond {
		t.Fatalf("recover RPC deadline/elapsed = %s/%s", service.recoverDeadline, time.Since(started))
	}
}

func TestParseRuntimeOnboardingRPCTimeout(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"", defaultRuntimeOnboardingRPCTimeout, true},
		{"1s", time.Second, true},
		{"2m", 2 * time.Minute, true},
		{"999ms", 0, false},
		{"3m", 0, false},
		{"invalid", 0, false},
	} {
		got, err := parseRuntimeOnboardingRPCTimeout(test.value)
		if (err == nil) != test.ok || got != test.want {
			t.Fatalf("parse timeout %q = %s, %v; want %s, ok=%v", test.value, got, err, test.want, test.ok)
		}
	}
}

func TestRuntimeOnboardingIntakeClientRejectsMismatchedReceiptAndErasesMaterial(t *testing.T) {
	service := &fakeOnboardingIntakeRPC{response: &executionv1.CreateOnboardingIntentResponse{
		IntentId: "11111111-2222-4333-8444-555555555555", AccountId: "other", DesiredGeneration: 7,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_API_KEY,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_API_KEY,
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)),
	}}
	client := &runtimeOnboardingIntakeClient{service: service}
	secret := []byte("sk-ant-test")
	material := &runtimeOnboardingMaterial{Source: "api_key", AuthType: "api_key", Secret: secret}
	if _, err := client.Create(context.Background(), "event-mismatch", 10380, 7, material); !errors.Is(err, errRuntimeOnboardingIntake) {
		t.Fatalf("mismatched receipt error = %v", err)
	}
	if material.Secret != nil || !allRuntimeOnboardingZero(secret) {
		t.Fatal("failed intake retained caller material")
	}
}

func TestRuntimeOnboardingIntakeClientRecoversExactReceiptAndClassifiesState(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	response := &executionv1.CreateOnboardingIntentResponse{
		IntentId: "recover-intent-10380", AccountId: "10380", DesiredGeneration: 7,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ExpiresAt: timestamppb.New(now.Add(30 * time.Minute)),
	}
	service := &fakeOnboardingIntakeRPC{recoverResponse: response}
	client := &runtimeOnboardingIntakeClient{service: service, now: func() time.Time { return now }}
	receipt, err := client.Recover(context.Background(), "onb-recover-key", 10380, 7, "oauth_code", "oauth")
	if err != nil || receipt.IntentID != response.GetIntentId() || receipt.IdempotencyKey != "onb-recover-key" ||
		service.recoverRequest.GetAccountId() != "10380" || service.recoverRequest.GetIdempotencyKey() != "onb-recover-key" {
		t.Fatalf("recovered receipt/request = %+v/%+v err=%v", receipt, service.recoverRequest, err)
	}

	for name, test := range map[string]struct {
		code codes.Code
		want error
	}{
		"missing":  {codes.NotFound, errRuntimeOnboardingReceiptNotFound},
		"expired":  {codes.Aborted, errRuntimeOnboardingReceiptExpired},
		"conflict": {codes.FailedPrecondition, errRuntimeOnboardingReceiptConflict},
		"storage":  {codes.Unavailable, errRuntimeOnboardingUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			client := &runtimeOnboardingIntakeClient{service: &fakeOnboardingIntakeRPC{
				recoverErr: status.Error(test.code, name),
			}}
			_, err := client.Recover(context.Background(), "onb-recover-key", 10380, 7, "oauth_code", "oauth")
			if !errors.Is(err, test.want) {
				t.Fatalf("recovery error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestRuntimeOnboardingIntakeClientReadsBoundSafeResult(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	service := &fakeOnboardingIntakeRPC{resultResponse: &executionv1.GetOnboardingResultResponse{
		IntentId: "intent-10380", AccountId: "10380", DesiredGeneration: 7,
		SlotId: "ccmax-account-10380", ExecutionEpoch: 19,
		AuthType:     executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		EmailAddress: "Owner@Example.COM", OrganizationId: "org-10380", UpstreamAccountId: "upstream-10380",
		Scope: "user:inference", SubscriptionType: "max", RateLimitTier: "tier-1",
		ExpiresAt: timestamppb.New(now.Add(time.Hour)), ProjectedAt: timestamppb.New(now),
	}}
	client := &runtimeOnboardingIntakeClient{service: service}
	result, err := client.GetResult(context.Background(), "intent-10380", 10380, 7)
	if err != nil {
		t.Fatal(err)
	}
	if result.EmailAddress != "owner@example.com" || result.SubscriptionType != "max" || result.AuthType != "oauth" ||
		result.AccountID != 10380 || result.SlotID != "ccmax-account-10380" || result.ExecutionEpoch != 19 ||
		!result.ProjectedAt.Equal(now) || service.resultRequest.GetAccountId() != "10380" {
		t.Fatalf("runtime onboarding result = %+v / request=%+v", result, service.resultRequest)
	}
	encoded, err := json.Marshal(result)
	if err != nil || bytes.Contains(encoded, []byte("access_token")) || bytes.Contains(encoded, []byte("refresh_token")) {
		t.Fatalf("runtime result serialization = %s, %v", encoded, err)
	}
}

func TestRuntimeOnboardingIntakeClientRejectsMetadataOutsideCCMAXSchema(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	base := &executionv1.GetOnboardingResultResponse{
		IntentId: "intent-schema-bound", AccountId: "10380", DesiredGeneration: 7,
		SlotId: "ccmax-account-10380", ExecutionEpoch: 19,
		AuthType:    executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ProjectedAt: timestamppb.New(now),
	}
	for name, mutate := range map[string]func(*executionv1.GetOnboardingResultResponse){
		"email exceeds account name": func(response *executionv1.GetOnboardingResultResponse) {
			response.EmailAddress = strings.Repeat("a", 244) + "@example.com"
		},
		"subscription exceeds column": func(response *executionv1.GetOnboardingResultResponse) {
			response.SubscriptionType = strings.Repeat("s", 65)
		},
	} {
		t.Run(name, func(t *testing.T) {
			response := proto.Clone(base).(*executionv1.GetOnboardingResultResponse)
			mutate(response)
			client := &runtimeOnboardingIntakeClient{service: &fakeOnboardingIntakeRPC{resultResponse: response}}
			if _, err := client.GetResult(context.Background(), "intent-schema-bound", 10380, 7); !errors.Is(err, errRuntimeOnboardingIntake) {
				t.Fatalf("out-of-schema metadata error = %v", err)
			}
		})
	}
}

func TestRuntimeOnboardingIntakeClientDistinguishesPendingResult(t *testing.T) {
	service := &fakeOnboardingIntakeRPC{resultErr: status.Error(codes.FailedPrecondition, "not ready")}
	client := &runtimeOnboardingIntakeClient{service: service}
	if _, err := client.GetResult(context.Background(), "intent-10380", 10380, 7); !errors.Is(err, errRuntimeOnboardingResultPending) {
		t.Fatalf("pending onboarding result error = %v", err)
	}
}

func TestRuntimeOnboardingIntakeClientReadsSafeTerminalResults(t *testing.T) {
	finishedAt := time.Unix(2_000_000_123, 0).UTC()
	tests := []struct {
		name       string
		status     executionv1.OnboardingResultStatus
		errorCode  string
		summary    string
		slotID     string
		epoch      uint64
		wantStatus string
	}{
		{"failed", executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_FAILED,
			runtimeOnboardingErrorFailed, runtimeOnboardingSummaryFailed, "ccmax-account-10380", 19, runtimeOnboardingResultFailed},
		{"expired", executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_EXPIRED,
			runtimeOnboardingErrorExpired, runtimeOnboardingSummaryExpired, "", 0, runtimeOnboardingResultExpired},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &fakeOnboardingIntakeRPC{resultResponse: &executionv1.GetOnboardingResultResponse{
				IntentId: "intent-terminal", AccountId: "10380", DesiredGeneration: 7,
				Status: test.status, ErrorCode: test.errorCode, ErrorSummary: test.summary,
				SlotId: test.slotID, ExecutionEpoch: test.epoch, FinishedAt: timestamppb.New(finishedAt),
			}}
			result, err := (&runtimeOnboardingIntakeClient{service: service}).GetResult(
				context.Background(), "intent-terminal", 10380, 7,
			)
			if err != nil || result.Status != test.wantStatus || result.ErrorCode != test.errorCode ||
				result.ErrorSummary != test.summary || !result.FinishedAt.Equal(finishedAt) {
				t.Fatalf("terminal result=%+v err=%v", result, err)
			}
		})
	}
}

func TestRuntimeOnboardingIntakeClientRejectsUnboundedTerminalError(t *testing.T) {
	service := &fakeOnboardingIntakeRPC{resultResponse: &executionv1.GetOnboardingResultResponse{
		IntentId: "intent-terminal", AccountId: "10380", DesiredGeneration: 7,
		Status:    executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_FAILED,
		ErrorCode: "token_exchange_failed", ErrorSummary: "raw provider response must not cross the boundary",
		SlotId: "ccmax-account-10380", ExecutionEpoch: 19, FinishedAt: timestamppb.Now(),
	}}
	if _, err := (&runtimeOnboardingIntakeClient{service: service}).GetResult(
		context.Background(), "intent-terminal", 10380, 7,
	); !errors.Is(err, errRuntimeOnboardingIntake) {
		t.Fatalf("unsafe terminal result error=%v", err)
	}
}

func TestRuntimeOnboardingMaterialBridgeCommitsOnlyReceiptAfterIntake(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-intake-bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('intake-bridge', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	seedRuntimeOnboardingSubmission(t, a, "event-intake-bridge", accountID, 1,
		"account.runtime.provision_requested", "migrating", "session_key", "oauth")
	expires := time.Now().UTC().Add(30 * time.Minute)
	service := &fakeOnboardingIntakeRPC{response: &executionv1.CreateOnboardingIntentResponse{
		IntentId: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", AccountId: strconv.FormatInt(accountID, 10), DesiredGeneration: 1,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ExpiresAt: timestamppb.New(expires),
	}}
	client := &runtimeOnboardingIntakeClient{service: service}
	secret := []byte("bridge-session-secret")
	material := &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: secret}
	event, err := a.requestRuntimeOnboardingWithMaterial(context.Background(), client, "event-intake-bridge", runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.runtime.provision_requested", MigrationStatus: "migrating",
		RuntimeStatus: "provisioning", Payload: map[string]any{"session_key": "must-be-erased"},
	}, material)
	if err != nil {
		t.Fatal(err)
	}
	if material.Secret != nil || !allRuntimeOnboardingZero(secret) || event.PayloadJSON != `{"onboarding_intent_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}` {
		t.Fatalf("bridge material/event = %+v/%+v", material, event)
	}
}

func TestRuntimeOnboardingRecoversLostReceiptWithoutResubmittingMaterial(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-recover-lost-response.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('recover-lost-response', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	seedRuntimeOnboardingSubmission(t, a, "recover-lost-response-key", accountID, 1,
		"account.runtime.provision_requested", "migrating", "session_key", "oauth")
	submission, err := a.getRuntimeOnboardingSubmission(context.Background(), "recover-lost-response-key")
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeOnboardingIntakeRPC{recoverResponse: &executionv1.CreateOnboardingIntentResponse{
		IntentId: "recovered-intent-10380", AccountId: strconv.FormatInt(accountID, 10), DesiredGeneration: 1,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
	}}
	event, err := a.requestRuntimeOnboardingWithMaterial(context.Background(), &runtimeOnboardingIntakeClient{service: service},
		"recover-lost-response-key", runtimeTransitionRequest{
			AccountID: accountID, EventType: "account.runtime.provision_requested",
			MigrationStatus: "migrating", RuntimeStatus: "provisioning",
		}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth"})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := a.getRuntimeOnboardingSubmission(context.Background(), "recover-lost-response-key")
	if err != nil {
		t.Fatal(err)
	}
	if event.DesiredGeneration != 1 || stored.Status != runtimeOnboardingSubmissionQueued ||
		stored.IntentID != "recovered-intent-10380" || stored.IntakeIdempotencyKey != submission.IntakeIdempotencyKey ||
		service.recoverCalls != 1 || service.calls != 0 {
		t.Fatalf("recovery event/submission/calls = %+v/%+v/%d/%d", event, stored, service.recoverCalls, service.calls)
	}
}

func TestRuntimeOnboardingCommitsPersistedReceiptWhileIntakeOffline(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-offline-receipt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('offline-receipt', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	seedRuntimeOnboardingSubmission(t, a, "offline-receipt-key", accountID, 1,
		"account.runtime.provision_requested", "migrating", "session_key", "oauth")
	submission, err := a.getRuntimeOnboardingSubmission(context.Background(), "offline-receipt-key")
	if err != nil {
		t.Fatal(err)
	}
	receipt := runtimeOnboardingIntentReceipt{
		IdempotencyKey: submission.IntakeIdempotencyKey, IntentID: "offline-persisted-intent",
		AccountID: accountID, DesiredGeneration: 1, ExpiresAt: time.Now().Add(30 * time.Minute),
	}
	if _, err := a.persistRuntimeOnboardingReceipt(context.Background(), "offline-receipt-key", submission, receipt, time.Now()); err != nil {
		t.Fatal(err)
	}
	event, err := a.requestRuntimeOnboardingWithMaterial(context.Background(), nil, "offline-receipt-key",
		runtimeTransitionRequest{
			AccountID: accountID, EventType: "account.runtime.provision_requested",
			MigrationStatus: "migrating", RuntimeStatus: "provisioning",
		}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth"})
	if err != nil || event.DesiredGeneration != 1 {
		t.Fatalf("offline persisted receipt event=%+v err=%v", event, err)
	}
}

func TestRuntimeOnboardingRotatesOnlyExactExpiredAttempt(t *testing.T) {
	t.Run("exact expired rotates and queues", func(t *testing.T) {
		a, err := newApp(filepath.Join(t.TempDir(), "onboarding-expired-attempt.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer a.db.Close()
		result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('expired-attempt', '{}')`)
		if err != nil {
			t.Fatal(err)
		}
		accountID, _ := result.LastInsertId()
		seedRuntimeOnboardingSubmission(t, a, "expired-attempt-key", accountID, 1,
			"account.runtime.provision_requested", "migrating", "session_key", "oauth")
		initial, err := a.getRuntimeOnboardingSubmission(context.Background(), "expired-attempt-key")
		if err != nil {
			t.Fatal(err)
		}
		service := &fakeOnboardingIntakeRPC{}
		service.recoverRespond = func(request *executionv1.RecoverOnboardingIntentRequest) (*executionv1.CreateOnboardingIntentResponse, error) {
			if request.GetIdempotencyKey() == initial.IntakeIdempotencyKey {
				return nil, status.Error(codes.Aborted, "exact attempt expired")
			}
			return nil, status.Error(codes.NotFound, "new attempt")
		}
		service.respond = func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
			return &executionv1.CreateOnboardingIntentResponse{
				IntentId: "rotated-intent-10380", AccountId: request.GetAccountId(),
				DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
				ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
			}
		}
		event, err := a.requestRuntimeOnboardingWithMaterial(context.Background(), &runtimeOnboardingIntakeClient{service: service},
			"expired-attempt-key", runtimeTransitionRequest{
				AccountID: accountID, EventType: "account.runtime.provision_requested",
				MigrationStatus: "migrating", RuntimeStatus: "provisioning",
			}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: []byte("fresh-material")})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := a.getRuntimeOnboardingSubmission(context.Background(), "expired-attempt-key")
		if err != nil {
			t.Fatal(err)
		}
		if event.DesiredGeneration != 1 || stored.Status != runtimeOnboardingSubmissionQueued || stored.IntakeAttempt != 1 ||
			stored.IntakeIdempotencyKey == initial.IntakeIdempotencyKey || service.recoverCalls != 2 || service.calls != 1 {
			t.Fatalf("rotated event/submission/calls = %+v/%+v/%d/%d", event, stored, service.recoverCalls, service.calls)
		}
	})

	t.Run("identity or lifecycle conflict fails closed", func(t *testing.T) {
		a, err := newApp(filepath.Join(t.TempDir(), "onboarding-conflicted-attempt.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer a.db.Close()
		result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('conflicted-attempt', '{}')`)
		if err != nil {
			t.Fatal(err)
		}
		accountID, _ := result.LastInsertId()
		seedRuntimeOnboardingSubmission(t, a, "conflicted-attempt-key", accountID, 1,
			"account.runtime.provision_requested", "migrating", "session_key", "oauth")
		service := &fakeOnboardingIntakeRPC{recoverErr: status.Error(codes.FailedPrecondition, "claimed or mismatched")}
		_, err = a.requestRuntimeOnboardingWithMaterial(context.Background(), &runtimeOnboardingIntakeClient{service: service},
			"conflicted-attempt-key", runtimeTransitionRequest{
				AccountID: accountID, EventType: "account.runtime.provision_requested",
				MigrationStatus: "migrating", RuntimeStatus: "provisioning",
			}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: []byte("must-not-retry")})
		if !errors.Is(err, errRuntimeMigration) {
			t.Fatalf("conflicted attempt error = %v", err)
		}
		stored, loadErr := a.getRuntimeOnboardingSubmission(context.Background(), "conflicted-attempt-key")
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if stored.Status != runtimeOnboardingSubmissionPending || stored.IntakeAttempt != 0 || stored.IntentID != "" || service.calls != 0 {
			t.Fatalf("conflicted submission/create calls = %+v/%d", stored, service.calls)
		}
	})
}

func TestRuntimeOnboardingLateRecoverResponseContinuesWithNewerPendingAttempt(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "onboarding-late-recover.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('late-recover', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	seedRuntimeOnboardingSubmission(t, a, "late-recover-key", accountID, 1,
		"account.runtime.provision_requested", "migrating", "session_key", "oauth")
	initial, err := a.getRuntimeOnboardingSubmission(context.Background(), "late-recover-key")
	if err != nil {
		t.Fatal(err)
	}

	service := &fakeOnboardingIntakeRPC{}
	service.recoverRespond = func(request *executionv1.RecoverOnboardingIntentRequest) (*executionv1.CreateOnboardingIntentResponse, error) {
		if service.recoverCalls == 1 {
			advanced, advanceErr := a.advanceRuntimeOnboardingAttempt(context.Background(), initial)
			if advanceErr != nil {
				t.Fatalf("advance concurrent attempt: %v", advanceErr)
			}
			if advanced.IntakeAttempt != initial.IntakeAttempt+1 || advanced.IntakeIdempotencyKey == initial.IntakeIdempotencyKey {
				t.Fatalf("advanced submission = %+v", advanced)
			}
			return &executionv1.CreateOnboardingIntentResponse{
				IntentId: "late-old-attempt-intent", AccountId: request.GetAccountId(),
				DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
				ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
			}, nil
		}
		return nil, status.Error(codes.NotFound, "newer attempt has no intent")
	}
	service.respond = func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
		return &executionv1.CreateOnboardingIntentResponse{
			IntentId: "current-attempt-intent", AccountId: request.GetAccountId(),
			DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
			ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
		}
	}

	event, err := a.requestRuntimeOnboardingWithMaterial(context.Background(), &runtimeOnboardingIntakeClient{service: service},
		"late-recover-key", runtimeTransitionRequest{
			AccountID: accountID, EventType: "account.runtime.provision_requested",
			MigrationStatus: "migrating", RuntimeStatus: "provisioning",
		}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: []byte("fresh-material")})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := a.getRuntimeOnboardingSubmission(context.Background(), "late-recover-key")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != runtimeOnboardingSubmissionQueued || stored.IntakeAttempt != initial.IntakeAttempt+1 ||
		stored.IntakeIdempotencyKey == initial.IntakeIdempotencyKey || stored.IntentID != "current-attempt-intent" ||
		event.DesiredGeneration != 1 || service.recoverCalls != 2 || service.calls != 1 {
		t.Fatalf("late recovery event/submission/calls = %+v/%+v/%d/%d", event, stored, service.recoverCalls, service.calls)
	}
}

func TestRuntimeOnboardingLateCreateResponseUsesOnlyNewerAttempt(t *testing.T) {
	for _, test := range []struct {
		name                    string
		persistSuccessorReceipt bool
		wantErr                 error
	}{
		{name: "successor receipt already durable", persistSuccessorReceipt: true},
		{name: "successor absent requires resubmission", wantErr: errRuntimeOnboardingMaterialRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			a, err := newApp(filepath.Join(t.TempDir(), "onboarding-late-create.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer a.db.Close()
			result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('late-create', '{}')`)
			if err != nil {
				t.Fatal(err)
			}
			accountID, _ := result.LastInsertId()
			seedRuntimeOnboardingSubmission(t, a, "late-create-key", accountID, 1,
				"account.runtime.provision_requested", "migrating", "session_key", "oauth")
			initial, err := a.getRuntimeOnboardingSubmission(context.Background(), "late-create-key")
			if err != nil {
				t.Fatal(err)
			}

			service := &fakeOnboardingIntakeRPC{}
			service.respond = func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
				successor, advanceErr := a.advanceRuntimeOnboardingAttempt(context.Background(), initial)
				if advanceErr != nil {
					t.Fatalf("advance concurrent attempt: %v", advanceErr)
				}
				if test.persistSuccessorReceipt {
					_, persistErr := a.persistRuntimeOnboardingReceipt(context.Background(), "late-create-key", successor,
						runtimeOnboardingIntentReceipt{
							IdempotencyKey: successor.IntakeIdempotencyKey, IntentID: "successor-durable-intent",
							AccountID: accountID, DesiredGeneration: 1, ExpiresAt: time.Now().Add(30 * time.Minute),
						}, time.Now())
					if persistErr != nil {
						t.Fatalf("persist successor receipt: %v", persistErr)
					}
				}
				return &executionv1.CreateOnboardingIntentResponse{
					IntentId: "late-old-create-intent", AccountId: request.GetAccountId(),
					DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
					ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
				}
			}

			secret := []byte("one-shot-material")
			event, err := a.requestRuntimeOnboardingWithMaterial(context.Background(), &runtimeOnboardingIntakeClient{service: service},
				"late-create-key", runtimeTransitionRequest{
					AccountID: accountID, EventType: "account.runtime.provision_requested",
					MigrationStatus: "migrating", RuntimeStatus: "provisioning",
				}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: secret})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("late create event=%+v error=%v, want %v", event, err, test.wantErr)
			}
			stored, err := a.getRuntimeOnboardingSubmission(context.Background(), "late-create-key")
			if err != nil {
				t.Fatal(err)
			}
			if service.calls != 1 || !allRuntimeOnboardingZero(secret) || stored.IntakeAttempt != initial.IntakeAttempt+1 ||
				stored.IntakeIdempotencyKey == initial.IntakeIdempotencyKey || stored.IntentID == "late-old-create-intent" {
				t.Fatalf("late create mutated/recreated state: event=%+v stored=%+v calls=%d", event, stored, service.calls)
			}
			if test.persistSuccessorReceipt {
				if stored.Status != runtimeOnboardingSubmissionQueued || stored.IntentID != "successor-durable-intent" ||
					event.DesiredGeneration != 1 || service.recoverCalls != 1 {
					t.Fatalf("durable successor event/submission/recover=%+v/%+v/%d", event, stored, service.recoverCalls)
				}
			} else if stored.Status != runtimeOnboardingSubmissionPending || stored.IntentID != "" || service.recoverCalls != 2 {
				t.Fatalf("missing successor state/recover=%+v/%d", stored, service.recoverCalls)
			}
		})
	}
}

func TestRuntimeOnboardingShortReceiptRespectsMaterialOwnership(t *testing.T) {
	t.Run("short recovered receipt rotates with retained material", func(t *testing.T) {
		a, err := newApp(filepath.Join(t.TempDir(), "onboarding-short-recover.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer a.db.Close()
		result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('short-recover', '{}')`)
		if err != nil {
			t.Fatal(err)
		}
		accountID, _ := result.LastInsertId()
		seedRuntimeOnboardingSubmission(t, a, "short-recover-key", accountID, 1,
			"account.runtime.provision_requested", "migrating", "session_key", "oauth")
		initial, err := a.getRuntimeOnboardingSubmission(context.Background(), "short-recover-key")
		if err != nil {
			t.Fatal(err)
		}
		service := &fakeOnboardingIntakeRPC{}
		service.recoverRespond = func(request *executionv1.RecoverOnboardingIntentRequest) (*executionv1.CreateOnboardingIntentResponse, error) {
			if service.recoverCalls == 1 {
				return &executionv1.CreateOnboardingIntentResponse{
					IntentId: "short-recovered-intent", AccountId: request.GetAccountId(),
					DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
					ExpiresAt: timestamppb.New(time.Now().Add(time.Second)),
				}, nil
			}
			return nil, status.Error(codes.NotFound, "new attempt")
		}
		service.respond = func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
			return &executionv1.CreateOnboardingIntentResponse{
				IntentId: "long-current-intent", AccountId: request.GetAccountId(),
				DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
				ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
			}
		}
		secret := []byte("retained-until-create")
		event, err := a.requestRuntimeOnboardingWithMaterial(context.Background(), &runtimeOnboardingIntakeClient{service: service},
			"short-recover-key", runtimeTransitionRequest{
				AccountID: accountID, EventType: "account.runtime.provision_requested",
				MigrationStatus: "migrating", RuntimeStatus: "provisioning",
			}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: secret})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := a.getRuntimeOnboardingSubmission(context.Background(), "short-recover-key")
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != runtimeOnboardingSubmissionQueued || stored.IntakeAttempt != initial.IntakeAttempt+1 ||
			stored.IntentID != "long-current-intent" || event.DesiredGeneration != 1 || service.recoverCalls != 2 ||
			service.calls != 1 || !allRuntimeOnboardingZero(secret) {
			t.Fatalf("short recovered receipt event/submission/calls=%+v/%+v/%d/%d", event, stored, service.recoverCalls, service.calls)
		}
	})

	t.Run("short created receipt requires material resubmission", func(t *testing.T) {
		a, err := newApp(filepath.Join(t.TempDir(), "onboarding-short-create.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer a.db.Close()
		result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('short-create', '{}')`)
		if err != nil {
			t.Fatal(err)
		}
		accountID, _ := result.LastInsertId()
		seedRuntimeOnboardingSubmission(t, a, "short-create-key", accountID, 1,
			"account.runtime.provision_requested", "migrating", "session_key", "oauth")
		service := &fakeOnboardingIntakeRPC{}
		service.respond = func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
			return &executionv1.CreateOnboardingIntentResponse{
				IntentId: "short-created-intent", AccountId: request.GetAccountId(),
				DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
				ExpiresAt: timestamppb.New(time.Now().Add(time.Second)),
			}
		}
		secret := []byte("consumed-once")
		_, err = a.requestRuntimeOnboardingWithMaterial(context.Background(), &runtimeOnboardingIntakeClient{service: service},
			"short-create-key", runtimeTransitionRequest{
				AccountID: accountID, EventType: "account.runtime.provision_requested",
				MigrationStatus: "migrating", RuntimeStatus: "provisioning",
			}, &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: secret})
		if !errors.Is(err, errRuntimeOnboardingMaterialRequired) {
			t.Fatalf("short created receipt error=%v", err)
		}
		stored, err := a.getRuntimeOnboardingSubmission(context.Background(), "short-create-key")
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != runtimeOnboardingSubmissionPending || stored.IntakeAttempt != 1 || stored.IntentID != "" ||
			service.recoverCalls != 1 || service.calls != 1 || !allRuntimeOnboardingZero(secret) {
			t.Fatalf("short created receipt submission/calls=%+v/%d/%d", stored, service.recoverCalls, service.calls)
		}
	})
}

func TestMigratedAccountSessionAuthQueuesExecutionOnboarding(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "migrated-session-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, auth_type, credentials_json, proxy_id, execution_migration_status, runtime_status, runtime_generation, schedulable)
		VALUES ('migrated-auth', 'oauth', '{}', ?, 'migrated', 'ready', 2, 1)`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	service := &fakeOnboardingIntakeRPC{response: &executionv1.CreateOnboardingIntentResponse{
		IntentId: "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff", AccountId: strconv.FormatInt(accountID, 10), DesiredGeneration: 3,
		Source:    executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY,
		AuthType:  executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 {
			a.onboardingIntake = nil
		}
		body := strings.NewReader(`{"session_key":"migrated-session-secret","mode":"oauth"}`)
		request := httptest.NewRequest(http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/session-auth", body)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "reauth-event-10380")
		response := httptest.NewRecorder()
		a.routes().ServeHTTP(response, request)
		if response.Code != http.StatusAccepted {
			t.Fatalf("runtime session auth attempt=%d status/body = %d/%s", attempt+1, response.Code, response.Body.String())
		}
	}
	var migrationStatus, runtimeStatus, credentialsJSON, payload string
	var generation uint64
	var schedulable int
	if err := a.db.QueryRow(`SELECT execution_migration_status, runtime_status, runtime_generation, schedulable, credentials_json
		FROM accounts WHERE id = ?`, accountID).Scan(&migrationStatus, &runtimeStatus, &generation, &schedulable, &credentialsJSON); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT payload_json FROM runtime_outbox WHERE account_id = ? AND desired_generation = 3
		AND event_type = 'account.credential.rotate_requested'`, accountID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if migrationStatus != "migrated" || runtimeStatus != "provisioning" || generation != 3 || schedulable != 0 ||
		credentialsJSON != "{}" || payload != `{"onboarding_intent_id":"bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"}` ||
		strings.Contains(payload, "migrated-session-secret") {
		t.Fatalf("runtime account/outbox = %s/%s/%d/%d/%s/%s", migrationStatus, runtimeStatus, generation, schedulable, credentialsJSON, payload)
	}
	var eventCount, grantCount, submissionCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = 'account.credential.rotate_requested'`, accountID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = ?`,
		accountID, runtimeProxyReservationGranted).Scan(&grantCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions WHERE account_id = ?`, accountID).Scan(&submissionCount); err != nil {
		t.Fatal(err)
	}
	if service.calls != 1 || eventCount != 1 || grantCount != 1 || submissionCount != 1 {
		t.Fatalf("reauthorization replay calls/events/grants/submissions=%d/%d/%d/%d", service.calls, eventCount, grantCount, submissionCount)
	}
}

func TestMigratedAccountSessionAuthMapsIntakeTimeoutAndUnavailable(t *testing.T) {
	for _, test := range []struct {
		name   string
		code   codes.Code
		status int
	}{
		{"timeout", codes.DeadlineExceeded, http.StatusGatewayTimeout},
		{"unavailable", codes.Unavailable, http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			a, err := newApp(filepath.Join(t.TempDir(), "migrated-session-auth-status.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer a.db.Close()
			proxyID := createTestForwardProxy(t, a)
			result, err := a.db.Exec(`INSERT INTO accounts
				(name, auth_type, credentials_json, proxy_id, execution_migration_status, runtime_status, runtime_generation, schedulable)
				VALUES ('migrated-auth-status', 'oauth', '{}', ?, 'migrated', 'ready', 2, 1)`, proxyID)
			if err != nil {
				t.Fatal(err)
			}
			accountID, _ := result.LastInsertId()
			a.onboardingIntake = &runtimeOnboardingIntakeClient{service: &fakeOnboardingIntakeRPC{
				err: status.Error(test.code, "internal detail must not escape"),
			}}
			request := httptest.NewRequest(http.MethodPost,
				"/api/accounts/"+strconv.FormatInt(accountID, 10)+"/session-auth",
				strings.NewReader(`{"session_key":"bounded-session-secret","mode":"oauth"}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "reauth-status-"+test.name)
			response := httptest.NewRecorder()
			a.routes().ServeHTTP(response, request)
			if response.Code != test.status || strings.Contains(response.Body.String(), "internal detail") {
				t.Fatalf("status/body = %d/%s, want %d and redacted", response.Code, response.Body.String(), test.status)
			}
			var generation uint64
			var runtimeStatus string
			if err := a.db.QueryRow(`SELECT runtime_generation, runtime_status FROM accounts WHERE id = ?`, accountID).
				Scan(&generation, &runtimeStatus); err != nil {
				t.Fatal(err)
			}
			if generation != 2 || runtimeStatus != "ready" {
				t.Fatalf("failed intake advanced account: generation/status=%d/%s", generation, runtimeStatus)
			}
		})
	}
}

func TestAccountCreateExecutionOnboardingPreservesPendingAccountAndOpaqueOutbox(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-account-create.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	service := &fakeOnboardingIntakeRPC{respond: func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
		return &executionv1.CreateOnboardingIntentResponse{
			IntentId: "cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa", AccountId: request.GetAccountId(),
			DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
			ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
		}
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	payload := map[string]any{
		"name": "execution-create", "platform": "anthropic", "auth_type": "oauth",
		"session_key": "execution-create-session", "execution_onboarding": true,
		"extra": map[string]any{}, "status": "active", "schedulable": true, "concurrency": 1,
		"priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"}, "rpm_strategy": "tiered",
		"user_msg_queue_mode": "off", "proxy_pool_id": 1, "proxy_id": proxyID,
	}
	encoded, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "create-event-10380")
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("execution account create status/body = %d/%s", response.Code, response.Body.String())
	}
	var created account
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	var migrationStatus, runtimeStatus, credentialsJSON, sourceHint, outboxPayload string
	var generation uint64
	var schedulable int
	if err := a.db.QueryRow(`SELECT execution_migration_status, runtime_status, runtime_generation, schedulable,
		credentials_json, source_sk_hint FROM accounts WHERE id = ?`, created.ID).Scan(
		&migrationStatus, &runtimeStatus, &generation, &schedulable, &credentialsJSON, &sourceHint,
	); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT payload_json FROM runtime_outbox WHERE account_id = ?
		AND event_type = 'account.runtime.provision_requested'`, created.ID).Scan(&outboxPayload); err != nil {
		t.Fatal(err)
	}
	if migrationStatus != "migrating" || runtimeStatus != "provisioning" || generation != 1 || schedulable != 0 ||
		credentialsJSON != "{}" || sourceHint != "" || outboxPayload != `{"onboarding_intent_id":"cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa"}` {
		t.Fatalf("pending execution account = %s/%s/%d/%d/%s/%s/%s", migrationStatus, runtimeStatus, generation, schedulable, credentialsJSON, sourceHint, outboxPayload)
	}

	// The queued CCMAX record is sufficient for a lost-response replay even if
	// the execution-plane intake is temporarily unavailable.
	a.onboardingIntake = nil
	replayRequest := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
	replayRequest.Header.Set("Content-Type", "application/json")
	replayRequest.Header.Set("Idempotency-Key", "create-event-10380")
	replayResponse := httptest.NewRecorder()
	a.routes().ServeHTTP(replayResponse, replayRequest)
	if replayResponse.Code != http.StatusOK {
		t.Fatalf("execution account replay status/body = %d/%s", replayResponse.Code, replayResponse.Body.String())
	}
	var replayed account
	if err := json.Unmarshal(replayResponse.Body.Bytes(), &replayed); err != nil {
		t.Fatal(err)
	}
	var accountCount, eventCount, grantCount, submissionCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE name = 'execution-create'`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ?
		AND event_type = 'account.runtime.provision_requested'`, created.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = ?`,
		created.ID, runtimeProxyReservationGranted).Scan(&grantCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_onboarding_submissions WHERE account_id = ?`, created.ID).Scan(&submissionCount); err != nil {
		t.Fatal(err)
	}
	if replayed.ID != created.ID || service.calls != 1 || accountCount != 1 || eventCount != 1 || grantCount != 1 || submissionCount != 1 {
		t.Fatalf("create replay id/calls/accounts/events/grants/submissions=%d/%d/%d/%d/%d/%d want=%d/1/1/1/1/1",
			replayed.ID, service.calls, accountCount, eventCount, grantCount, submissionCount, created.ID)
	}
}

func TestAccountCreateExecutionOnboardingSupportsEveryWorkerMaterialSource(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-account-sources.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	service := &fakeOnboardingIntakeRPC{respond: func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
		return &executionv1.CreateOnboardingIntentResponse{
			IntentId: "dddddddd-eeee-4fff-8aaa-bbbbbbbbbbbb", AccountId: request.GetAccountId(),
			DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
			ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
		}
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	tests := []struct {
		name       string
		source     string
		authType   string
		secret     string
		auxiliary  string
		wantSource executionv1.OnboardingSource
		wantAuth   executionv1.OnboardingAuthType
	}{
		{"oauth-code", "oauth_code", "oauth", "oauth-code-value", "pkce-verifier-value", executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE, executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH},
		{"setup-token", "setup_token", "setup_token", "setup-token-value", "", executionv1.OnboardingSource_ONBOARDING_SOURCE_SETUP_TOKEN, executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_SETUP_TOKEN},
		{"api-key", "api_key", "api_key", "api-key-value", "", executionv1.OnboardingSource_ONBOARDING_SOURCE_API_KEY, executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_API_KEY},
		{"cookie", "cookie", "oauth", "sessionKey=cookie-value", "", executionv1.OnboardingSource_ONBOARDING_SOURCE_COOKIE, executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH},
		{"credential-import", "credential_import", "oauth", `{"access_token":"import-value"}`, "", executionv1.OnboardingSource_ONBOARDING_SOURCE_CREDENTIAL_IMPORT, executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxyID := createTestForwardProxy(t, a)
			payload := map[string]any{
				"name": "execution-" + test.name, "platform": "anthropic", "auth_type": test.authType,
				"execution_onboarding": true, "onboarding_source": test.source,
				"onboarding_secret": test.secret, "onboarding_auxiliary": test.auxiliary,
				"extra": map[string]any{}, "status": "active", "schedulable": true, "concurrency": 1,
				"priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"}, "rpm_strategy": "tiered",
				"user_msg_queue_mode": "off", "proxy_pool_id": 1, "proxy_id": proxyID,
			}
			encoded, _ := json.Marshal(payload)
			request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", fmt.Sprintf("source-event-%d", index+1))
			response := httptest.NewRecorder()
			a.routes().ServeHTTP(response, request)
			if response.Code != http.StatusCreated {
				t.Fatalf("source %s status/body=%d/%s", test.source, response.Code, response.Body.String())
			}
			if service.request.GetSource() != test.wantSource || service.request.GetAuthType() != test.wantAuth ||
				string(service.request.GetSecret()) != test.secret || string(service.request.GetAuxiliary()) != test.auxiliary {
				t.Fatalf("source request=%+v", service.request)
			}
			var created account
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			var credentialsJSON, sourceHint, outboxPayload string
			if err := a.db.QueryRow(`SELECT credentials_json, source_sk_hint FROM accounts WHERE id = ?`, created.ID).Scan(&credentialsJSON, &sourceHint); err != nil {
				t.Fatal(err)
			}
			if err := a.db.QueryRow(`SELECT payload_json FROM runtime_outbox WHERE account_id = ?
				AND event_type = 'account.runtime.provision_requested'`, created.ID).Scan(&outboxPayload); err != nil {
				t.Fatal(err)
			}
			if credentialsJSON != "{}" || sourceHint != "" || strings.Contains(outboxPayload, test.secret) ||
				strings.Contains(outboxPayload, "access_token") || strings.Contains(outboxPayload, "api-key-value") {
				t.Fatalf("source persisted material credentials=%s hint=%s outbox=%s", credentialsJSON, sourceHint, outboxPayload)
			}
			var auditBody string
			if err := a.db.QueryRow(`SELECT request_body FROM audit_logs WHERE path = '/api/accounts' ORDER BY id DESC LIMIT 1`).Scan(&auditBody); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(auditBody, test.secret) || test.auxiliary != "" && strings.Contains(auditBody, test.auxiliary) ||
				!strings.Contains(auditBody, `"onboarding_secret":"[REDACTED]"`) ||
				!strings.Contains(auditBody, `"onboarding_auxiliary":"[REDACTED]"`) {
				t.Fatalf("source audit retained onboarding material: %s", auditBody)
			}
		})
	}
}

func TestExecutionOnboardingRejectsCredentialsJSONBeforeAccountCreation(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-credentials-rejected.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: &fakeOnboardingIntakeRPC{}}
	payload := map[string]any{
		"name": "must-reject", "auth_type": "api_key", "execution_onboarding": true,
		"onboarding_source": "api_key", "onboarding_secret": "api-key-value",
		"credentials": map[string]any{"api_key": "must-not-persist"},
		"concurrency": 1, "rate_multiplier": 1, "group_ids": []string{"a"},
	}
	encoded, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "reject-credentials-json")
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("credentials JSON status/body=%d/%s", response.Code, response.Body.String())
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE name = 'must-reject'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected account count=%d err=%v", count, err)
	}
}

func TestExecutionOnboardingRejectsNonAnthropicPlatformBeforeAccountCreation(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-platform-rejected.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	service := &fakeOnboardingIntakeRPC{}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	payload := map[string]any{
		"name": "wrong-platform", "platform": "openai", "auth_type": "oauth",
		"execution_onboarding": true, "onboarding_source": "session_key", "onboarding_secret": "session-value",
		"concurrency": 1, "rate_multiplier": 1, "group_ids": []string{"a"},
	}
	encoded, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "reject-non-anthropic")
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || service.request != nil {
		t.Fatalf("non-Anthropic status/body/request=%d/%s/%+v", response.Code, response.Body.String(), service.request)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE name = 'wrong-platform'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected account count=%d err=%v", count, err)
	}
}

func TestRuntimeOnboardingDoesNotCreateIntentForArchivedAccount(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "execution-archived-intake.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	created, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json, archived_at) VALUES ('archived-intake', '{}', ` + nowSQL + `)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := created.LastInsertId()
	service := &fakeOnboardingIntakeRPC{}
	client := &runtimeOnboardingIntakeClient{service: service}
	secret := []byte("archived-session-value")
	material := &runtimeOnboardingMaterial{Source: "session_key", AuthType: "oauth", Secret: secret}
	if _, err := a.requestRuntimeOnboardingWithMaterial(context.Background(), client, "archived-intake-event", runtimeTransitionRequest{
		AccountID: accountID, EventType: "account.runtime.provision_requested", MigrationStatus: "migrating", RuntimeStatus: "provisioning",
	}, material); !errors.Is(err, errRuntimeOnboardingIntake) {
		t.Fatalf("archived onboarding error = %v", err)
	}
	if service.request != nil || material.Secret != nil || !allRuntimeOnboardingZero(secret) {
		t.Fatalf("archived onboarding reached intake or retained material: request=%+v material=%+v", service.request, material)
	}
	assertRuntimeGenerationAndEvents(t, a, accountID, 0, 0)
}

func TestMigratedAccountOAuthCallbackQueuesWorkerCodeExchange(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-oauth-code.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	created, err := a.db.Exec(`INSERT INTO accounts
		(name, auth_type, credentials_json, proxy_id, execution_migration_status, runtime_status,
		runtime_generation, runtime_slot_id, runtime_provider, runtime_execution_epoch, schedulable)
		VALUES ('runtime-oauth', 'oauth', '{}', ?, 'migrated', 'ready', 2, 'ccmax-account-oauth', 'docker', 9, 1)`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := created.LastInsertId()
	service := &fakeOnboardingIntakeRPC{respond: func(request *executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse {
		return &executionv1.CreateOnboardingIntentResponse{
			IntentId: "ffffffff-aaaa-4bbb-8ccc-dddddddddddd", AccountId: request.GetAccountId(),
			DesiredGeneration: request.GetDesiredGeneration(), Source: request.GetSource(), AuthType: request.GetAuthType(),
			ExpiresAt: timestamppb.New(time.Now().Add(30 * time.Minute)),
		}
	}}
	a.onboardingIntake = &runtimeOnboardingIntakeClient{service: service}
	handler := a.routes()
	var authorization struct {
		AuthURL   string `json:"auth_url"`
		SessionID string `json:"session_id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/auth-url", map[string]any{
		"mode": "oauth",
	}, http.StatusOK, &authorization)
	if authorization.SessionID == "" || !strings.Contains(authorization.AuthURL, "code_challenge=") {
		t.Fatalf("runtime authorization response=%+v", authorization)
	}
	body, _ := json.Marshal(map[string]any{"session_id": authorization.SessionID, "code": "oauth-code-value#oauth-state"})
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountID, 10)+"/oauth-exchange", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "runtime-oauth-exchange-10380")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("runtime OAuth exchange status/body=%d/%s", response.Code, response.Body.String())
	}
	if service.request.GetSource() != executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE ||
		service.request.GetAuthType() != executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH ||
		string(service.request.GetSecret()) != "oauth-code-value#oauth-state" || len(service.request.GetAuxiliary()) < 32 {
		t.Fatalf("runtime OAuth intake request=%+v", service.request)
	}
	var migrationStatus, runtimeStatus, credentialsJSON, outboxPayload string
	var generation uint64
	var schedulable int
	if err := a.db.QueryRow(`SELECT execution_migration_status, runtime_status, runtime_generation, schedulable, credentials_json
		FROM accounts WHERE id = ?`, accountID).Scan(&migrationStatus, &runtimeStatus, &generation, &schedulable, &credentialsJSON); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT payload_json FROM runtime_outbox WHERE account_id = ? AND desired_generation = 3
		AND event_type IN ('account.credential.migrate_requested', 'account.credential.rotate_requested')`, accountID).Scan(&outboxPayload); err != nil {
		t.Fatal(err)
	}
	if migrationStatus != "migrated" || runtimeStatus != "provisioning" || generation != 3 || schedulable != 0 ||
		credentialsJSON != "{}" || strings.Contains(outboxPayload, "oauth-code-value") || strings.Contains(outboxPayload, "code_verifier") {
		t.Fatalf("runtime OAuth state=%s/%s/%d/%d/%s/%s", migrationStatus, runtimeStatus, generation, schedulable, credentialsJSON, outboxPayload)
	}
}

type fakeOnboardingIntakeRPC struct {
	calls           int
	request         *executionv1.CreateOnboardingIntentRequest
	response        *executionv1.CreateOnboardingIntentResponse
	respond         func(*executionv1.CreateOnboardingIntentRequest) *executionv1.CreateOnboardingIntentResponse
	err             error
	recoverCalls    int
	recoverRequest  *executionv1.RecoverOnboardingIntentRequest
	recoverResponse *executionv1.CreateOnboardingIntentResponse
	recoverRespond  func(*executionv1.RecoverOnboardingIntentRequest) (*executionv1.CreateOnboardingIntentResponse, error)
	recoverErr      error
	resultRequest   *executionv1.GetOnboardingResultRequest
	resultResponse  *executionv1.GetOnboardingResultResponse
	resultErr       error
}

type blockingOnboardingIntakeRPC struct {
	createDeadline  time.Time
	recoverDeadline time.Time
}

func (f *blockingOnboardingIntakeRPC) CreateOnboardingIntent(
	ctx context.Context,
	_ *executionv1.CreateOnboardingIntentRequest,
	_ ...grpc.CallOption,
) (*executionv1.CreateOnboardingIntentResponse, error) {
	f.createDeadline, _ = ctx.Deadline()
	<-ctx.Done()
	return nil, status.FromContextError(ctx.Err()).Err()
}

func (f *blockingOnboardingIntakeRPC) RecoverOnboardingIntent(
	ctx context.Context,
	_ *executionv1.RecoverOnboardingIntentRequest,
	_ ...grpc.CallOption,
) (*executionv1.CreateOnboardingIntentResponse, error) {
	f.recoverDeadline, _ = ctx.Deadline()
	<-ctx.Done()
	return nil, status.FromContextError(ctx.Err()).Err()
}

func (f *blockingOnboardingIntakeRPC) GetOnboardingResult(
	ctx context.Context,
	_ *executionv1.GetOnboardingResultRequest,
	_ ...grpc.CallOption,
) (*executionv1.GetOnboardingResultResponse, error) {
	<-ctx.Done()
	return nil, status.FromContextError(ctx.Err()).Err()
}

func (f *fakeOnboardingIntakeRPC) RecoverOnboardingIntent(
	_ context.Context,
	request *executionv1.RecoverOnboardingIntentRequest,
	_ ...grpc.CallOption,
) (*executionv1.CreateOnboardingIntentResponse, error) {
	f.recoverCalls++
	cloned, _ := proto.Clone(request).(*executionv1.RecoverOnboardingIntentRequest)
	f.recoverRequest = cloned
	if f.recoverRespond != nil {
		return f.recoverRespond(cloned)
	}
	if f.recoverErr != nil {
		return nil, f.recoverErr
	}
	if f.recoverResponse != nil {
		return f.recoverResponse, nil
	}
	return nil, status.Error(codes.NotFound, "not created")
}

func (f *fakeOnboardingIntakeRPC) GetOnboardingResult(
	_ context.Context,
	request *executionv1.GetOnboardingResultRequest,
	_ ...grpc.CallOption,
) (*executionv1.GetOnboardingResultResponse, error) {
	cloned, _ := proto.Clone(request).(*executionv1.GetOnboardingResultRequest)
	f.resultRequest = cloned
	return f.resultResponse, f.resultErr
}

func (f *fakeOnboardingIntakeRPC) CreateOnboardingIntent(
	_ context.Context,
	request *executionv1.CreateOnboardingIntentRequest,
	_ ...grpc.CallOption,
) (*executionv1.CreateOnboardingIntentResponse, error) {
	f.calls++
	cloned, _ := proto.Clone(request).(*executionv1.CreateOnboardingIntentRequest)
	f.request = cloned
	if f.respond != nil {
		return f.respond(cloned), f.err
	}
	return f.response, f.err
}

func allRuntimeOnboardingZero(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return false
		}
	}
	return true
}
