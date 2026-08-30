package service

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

func TestOnboardingIntakeCreatesOpaqueIntentAndErasesRequest(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x41}, 32), "kms-test", "v1")
	cryptoService, _ := credential.NewService(kms)
	vault, _ := onboarding.NewVault(onboarding.VaultConfig{
		Crypto: cryptoService, Repository: onboarding.NewMemoryRepository(), Random: rand.Reader,
		Now: func() time.Time { return now }, IntentTTL: 30 * time.Minute, ClaimTTL: 5 * time.Minute,
	})
	server, err := NewOnboardingIntakeServer(vault, onboarding.NewMemoryProvisioningRepository(), intakeAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	secret := []byte("intake-session-secret")
	secretAlias := secret
	request := &executionv1.CreateOnboardingIntentRequest{
		IdempotencyKey: "event-intake", AccountId: "account-intake", DesiredGeneration: 3,
		Source:   executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY,
		AuthType: executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		Secret:   secret,
	}
	response, err := server.CreateOnboardingIntent(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetIntentId() == "" || response.GetAccountId() != "account-intake" || response.GetDesiredGeneration() != 3 ||
		response.GetExpiresAt() == nil || !response.GetExpiresAt().AsTime().Equal(now.Add(30*time.Minute)) {
		t.Fatalf("intake response = %+v", response)
	}
	if request.Secret != nil || !allIntakeZero(secretAlias) || bytes.Contains([]byte(response.String()), []byte("intake-session-secret")) {
		t.Fatal("intake request/response retained temporary credential material")
	}
	opened, err := vault.ClaimAndOpen(context.Background(), response.GetIntentId(), "account-intake", 3, "workflow-intake")
	if err != nil || string(opened.Secret) != "intake-session-secret" {
		t.Fatalf("opened intake intent = %+v/%v", opened, err)
	}
	opened.Destroy()
}

func TestOnboardingIntakeRejectsUnauthorizedPeerAndStillErasesRequest(t *testing.T) {
	server, _ := NewOnboardingIntakeServer(&recordingIntentCreator{}, onboarding.NewMemoryProvisioningRepository(), intakeAuthorizer{err: errors.New("denied")})
	secret := []byte("must-be-erased")
	request := &executionv1.CreateOnboardingIntentRequest{Secret: secret}
	_, err := server.CreateOnboardingIntent(context.Background(), request)
	if status.Code(err).String() != "PermissionDenied" || request.Secret != nil || !allIntakeZero(secret) {
		t.Fatalf("unauthorized intake error/request = %v/%+v", err, request)
	}
}

func TestOnboardingIntentRecoveryRoundTripsExactSecretFreeReceiptOverGRPC(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	var clockUnixNano atomic.Int64
	clockUnixNano.Store(now.UnixNano())
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x43}, 32), "kms-recover", "v1")
	cryptoService, _ := credential.NewService(kms)
	vault, _ := onboarding.NewVault(onboarding.VaultConfig{
		Crypto: cryptoService, Repository: onboarding.NewMemoryRepository(), Random: bytes.NewReader(bytes.Repeat([]byte{0x65}, 2048)),
		Now: func() time.Time { return time.Unix(0, clockUnixNano.Load()).UTC() }, IntentTTL: 30 * time.Minute, ClaimTTL: 5 * time.Minute,
	})
	secret := []byte("durable-recovery-secret")
	receipt, err := vault.Create(context.Background(), onboarding.CreateRequest{
		IdempotencyKey: "event-recovery-rpc", AccountID: "account-recovery", DesiredGeneration: 9,
		Input: &worker.OnboardingInput{Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth, Secret: secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewOnboardingIntakeServer(vault, onboarding.NewMemoryProvisioningRepository(), intakeAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	rpcServer := grpc.NewServer()
	executionv1.RegisterOnboardingIntakeServiceServer(rpcServer, server)
	go func() { _ = rpcServer.Serve(listener) }()
	t.Cleanup(func() {
		rpcServer.Stop()
		_ = listener.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(ctx, "bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	request := &executionv1.RecoverOnboardingIntentRequest{
		IdempotencyKey: "event-recovery-rpc", AccountId: "account-recovery", DesiredGeneration: 9,
		Source:   executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY,
		AuthType: executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
	}
	if fields := request.ProtoReflect().Descriptor().Fields(); fields.ByName("secret") != nil || fields.ByName("auxiliary") != nil {
		t.Fatal("recovery request schema contains credential material fields")
	}
	response, err := executionv1.NewOnboardingIntakeServiceClient(connection).RecoverOnboardingIntent(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetIntentId() != receipt.IntentID || response.GetAccountId() != receipt.AccountID ||
		response.GetDesiredGeneration() != receipt.DesiredGeneration || response.GetExpiresAt() == nil ||
		!response.GetExpiresAt().AsTime().Equal(receipt.ExpiresAt) {
		t.Fatalf("recovered gRPC receipt = %+v, want %+v", response, receipt)
	}
	if fields := response.ProtoReflect().Descriptor().Fields(); fields.ByName("secret") != nil || fields.ByName("auxiliary") != nil {
		t.Fatal("recovery response schema contains credential material fields")
	}
	if bytes.Contains([]byte(request.String()), []byte("durable-recovery-secret")) ||
		bytes.Contains([]byte(response.String()), []byte("durable-recovery-secret")) {
		t.Fatal("recovery RPC exposed credential material")
	}

	wrongIdentity := proto.Clone(request).(*executionv1.RecoverOnboardingIntentRequest)
	wrongIdentity.AccountId = "account-other"
	_, err = executionv1.NewOnboardingIntakeServiceClient(connection).RecoverOnboardingIntent(ctx, wrongIdentity)
	if status.Code(err) != codes.FailedPrecondition || status.Convert(err).Message() != "onboarding intent receipt is not recoverable" {
		t.Fatalf("wrong identity recovery error = %v", err)
	}
	missing := proto.Clone(request).(*executionv1.RecoverOnboardingIntentRequest)
	missing.IdempotencyKey = "event-recovery-missing"
	_, missingErr := executionv1.NewOnboardingIntakeServiceClient(connection).RecoverOnboardingIntent(ctx, missing)
	if status.Code(missingErr) != codes.NotFound || status.Convert(missingErr).Message() != "onboarding intent receipt was not found" {
		t.Fatalf("missing recovery error = %v", missingErr)
	}
	clockUnixNano.Store(receipt.ExpiresAt.UnixNano())
	_, expiredErr := executionv1.NewOnboardingIntakeServiceClient(connection).RecoverOnboardingIntent(ctx, request)
	if status.Code(expiredErr) != codes.Aborted || status.Convert(expiredErr).Message() != "onboarding intent receipt has expired" {
		t.Fatalf("expired recovery error = %v", expiredErr)
	}
	_, expiredWrongIdentityErr := executionv1.NewOnboardingIntakeServiceClient(connection).RecoverOnboardingIntent(ctx, wrongIdentity)
	if status.Code(expiredWrongIdentityErr) != codes.FailedPrecondition ||
		status.Convert(expiredWrongIdentityErr).Message() != "onboarding intent receipt is not recoverable" {
		t.Fatalf("expired wrong-identity recovery error = %v", expiredWrongIdentityErr)
	}
}

func TestOnboardingIntentRecoveryReusesIntakeAuthorization(t *testing.T) {
	server, err := NewOnboardingIntakeServer(
		&recordingIntentCreator{}, onboarding.NewMemoryProvisioningRepository(), intakeAuthorizer{err: errors.New("denied")},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = server.RecoverOnboardingIntent(context.Background(), &executionv1.RecoverOnboardingIntentRequest{
		IdempotencyKey: "event-recovery", AccountId: "account-recovery", DesiredGeneration: 1,
		Source:   executionv1.OnboardingSource_ONBOARDING_SOURCE_API_KEY,
		AuthType: executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_API_KEY,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unauthorized recovery error = %v", err)
	}
}

func TestOnboardingIntakeReturnsOnlyCompletedSafeProjection(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	result := onboarding.ProvisioningResult{
		WorkflowID: "workflow-10380", IntentID: "intent-10380", AccountID: "10380", DesiredGeneration: 7,
		SlotID: "ccmax-account-10380", ExecutionEpoch: 19,
		CredentialLeaseID: "lease-10380", CredentialVersionID: "version-10380", CreatedAt: now,
		Projection: onboarding.ResultProjection{
			AuthType: "oauth", EmailAddress: "owner@example.com", OrganizationID: "org-10380",
			UpstreamAccountID: "account-upstream", Scope: "user:inference", SubscriptionType: "max", RateLimitTier: "tier-1",
			ExpiresAt: now.Add(time.Hour),
		},
	}
	outcome, err := onboarding.NewSucceededProvisioningOutcome(result, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	reader := &recordingResultReader{outcome: outcome}
	server, err := NewOnboardingIntakeServer(&recordingIntentCreator{}, reader, intakeAuthorizer{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := server.GetOnboardingResult(context.Background(), &executionv1.GetOnboardingResultRequest{
		IntentId: "intent-10380", AccountId: "10380", DesiredGeneration: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.GetEmailAddress() != "owner@example.com" || response.GetSubscriptionType() != "max" ||
		response.GetAuthType() != executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH ||
		response.GetStatus() != executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_SUCCEEDED ||
		response.GetSlotId() != "ccmax-account-10380" || response.GetExecutionEpoch() != 19 ||
		response.GetProjectedAt() == nil || response.GetExpiresAt() == nil || response.GetFinishedAt() == nil ||
		!response.GetFinishedAt().AsTime().Equal(now.Add(time.Second)) {
		t.Fatalf("onboarding result = %+v", response)
	}
	if bytes.Contains([]byte(response.String()), []byte("access_token")) || bytes.Contains([]byte(response.String()), []byte("refresh_token")) {
		t.Fatal("onboarding result exposed credential fields")
	}
	if reader.intentID != "intent-10380" || reader.accountID != "10380" || reader.generation != 7 {
		t.Fatalf("result lookup = %q/%q/%d", reader.intentID, reader.accountID, reader.generation)
	}
}

func TestOnboardingIntakeReturnsSafeTerminalFailureAndExpiry(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000_000_000, 0).UTC()
	tests := []struct {
		name      string
		outcome   onboarding.ProvisioningOutcome
		status    executionv1.OnboardingResultStatus
		errorCode string
		errorText string
	}{
		{
			name: "failed",
			outcome: onboarding.ProvisioningOutcome{
				Status: onboarding.ProvisioningOutcomeFailed, WorkflowID: "workflow-10380",
				IntentID: "intent-10380", AccountID: "10380", DesiredGeneration: 7,
				SlotID: "ccmax-account-10380", ExecutionEpoch: 19,
				ErrorCode: onboarding.ProvisioningErrorFailed, ErrorSummary: onboarding.ProvisioningSummaryFailed,
				FinishedAt: now,
			},
			status:    executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_FAILED,
			errorCode: onboarding.ProvisioningErrorFailed, errorText: onboarding.ProvisioningSummaryFailed,
		},
		{
			name: "expired without workflow",
			outcome: onboarding.ProvisioningOutcome{
				Status: onboarding.ProvisioningOutcomeExpired, IntentID: "intent-10380", AccountID: "10380",
				DesiredGeneration: 7, ErrorCode: onboarding.ProvisioningErrorExpired,
				ErrorSummary: onboarding.ProvisioningSummaryExpired, FinishedAt: now,
			},
			status:    executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_EXPIRED,
			errorCode: onboarding.ProvisioningErrorExpired, errorText: onboarding.ProvisioningSummaryExpired,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.outcome.Validate(); err != nil {
				t.Fatal(err)
			}
			server, err := NewOnboardingIntakeServer(
				&recordingIntentCreator{}, &recordingResultReader{outcome: test.outcome}, intakeAuthorizer{},
			)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.GetOnboardingResult(context.Background(), &executionv1.GetOnboardingResultRequest{
				IntentId: "intent-10380", AccountId: "10380", DesiredGeneration: 7,
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.GetStatus() != test.status || response.GetErrorCode() != test.errorCode ||
				response.GetErrorSummary() != test.errorText || response.GetFinishedAt() == nil ||
				response.GetProjectedAt() != nil || response.GetExpiresAt() != nil ||
				response.GetAuthType() != executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_UNSPECIFIED {
				t.Fatalf("terminal onboarding result = %+v", response)
			}
			if bytes.Contains([]byte(response.String()), []byte("worker_key_failed")) ||
				bytes.Contains([]byte(response.String()), []byte("provider")) {
				t.Fatal("terminal result exposed internal failure details")
			}
		})
	}
}

func TestOnboardingIntakeTerminalOutcomeRoundTripsOverGRPC(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	outcome := onboarding.ProvisioningOutcome{
		Status: onboarding.ProvisioningOutcomeFailed, WorkflowID: "workflow-10380",
		IntentID: "intent-10380", AccountID: "10380", DesiredGeneration: 7,
		SlotID: "ccmax-account-10380", ExecutionEpoch: 19,
		ErrorCode: onboarding.ProvisioningErrorFailed, ErrorSummary: onboarding.ProvisioningSummaryFailed,
		FinishedAt: now,
	}
	server, err := NewOnboardingIntakeServer(
		&recordingIntentCreator{}, &recordingResultReader{outcome: outcome}, intakeAuthorizer{},
	)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	rpcServer := grpc.NewServer()
	executionv1.RegisterOnboardingIntakeServiceServer(rpcServer, server)
	go func() { _ = rpcServer.Serve(listener) }()
	t.Cleanup(func() {
		rpcServer.Stop()
		_ = listener.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(ctx, "bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	response, err := executionv1.NewOnboardingIntakeServiceClient(connection).GetOnboardingResult(
		ctx,
		&executionv1.GetOnboardingResultRequest{IntentId: "intent-10380", AccountId: "10380", DesiredGeneration: 7},
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetStatus() != executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_FAILED ||
		response.GetErrorCode() != onboarding.ProvisioningErrorFailed ||
		response.GetErrorSummary() != onboarding.ProvisioningSummaryFailed ||
		response.GetFinishedAt() == nil || !response.GetFinishedAt().AsTime().Equal(now) ||
		response.GetProjectedAt() != nil {
		t.Fatalf("gRPC terminal outcome = %+v", response)
	}
}

func TestOnboardingIntakeResultErrorMappingDoesNotMaskFailuresAsPending(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		code codes.Code
	}{
		{name: "pending", err: onboarding.ErrResultPending, code: codes.FailedPrecondition},
		{name: "canceled", err: context.Canceled, code: codes.Canceled},
		{name: "deadline", err: context.DeadlineExceeded, code: codes.DeadlineExceeded},
		{name: "corrupt", err: onboarding.ErrResultProjectionRejected, code: codes.Internal},
		{name: "storage", err: errors.New("database unavailable"), code: codes.Unavailable},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, err := NewOnboardingIntakeServer(
				&recordingIntentCreator{}, &recordingResultReader{err: test.err}, intakeAuthorizer{},
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = server.GetOnboardingResult(context.Background(), &executionv1.GetOnboardingResultRequest{
				IntentId: "intent-10380", AccountId: "10380", DesiredGeneration: 7,
			})
			if status.Code(err) != test.code {
				t.Fatalf("result error code = %s, want %s (%v)", status.Code(err), test.code, err)
			}
		})
	}
}

func TestMTLSServiceAuthorizerAcceptsOnlyExpectedServiceIdentity(t *testing.T) {
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	privateKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	publicKeyPEM, _ := pki.PublicKeyPEM(privateKey.Public())
	issued, err := authority.IssueServiceClient("ccmax", publicKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{issued.Certificate}, VerifiedChains: [][]*x509.Certificate{{issued.Certificate}},
	}}})
	if err := (MTLSServiceAuthorizer{ExpectedServiceID: "ccmax"}).AuthorizeOnboardingIntake(ctx); err != nil {
		t.Fatal(err)
	}
	if err := (MTLSServiceAuthorizer{ExpectedServiceID: "other"}).AuthorizeOnboardingIntake(ctx); err == nil {
		t.Fatal("wrong service identity was authorized")
	}
}

type intakeAuthorizer struct{ err error }

func (a intakeAuthorizer) AuthorizeOnboardingIntake(context.Context) error { return a.err }

type recordingIntentCreator struct{}

func (*recordingIntentCreator) Create(context.Context, onboarding.CreateRequest) (onboarding.Receipt, error) {
	return onboarding.Receipt{}, errors.New("must not be called")
}

func (*recordingIntentCreator) Recover(context.Context, onboarding.RecoverRequest) (onboarding.Receipt, error) {
	return onboarding.Receipt{}, errors.New("must not be called")
}

type recordingResultReader struct {
	outcome    onboarding.ProvisioningOutcome
	err        error
	intentID   string
	accountID  string
	generation uint64
}

func (r *recordingResultReader) GetProvisioningResult(_ context.Context, intentID, accountID string, desiredGeneration uint64) (onboarding.ProvisioningOutcome, error) {
	r.intentID, r.accountID, r.generation = intentID, accountID, desiredGeneration
	return r.outcome, r.err
}

func allIntakeZero(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return false
		}
	}
	return true
}
