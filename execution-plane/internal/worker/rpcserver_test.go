package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type recordingActivator struct {
	mu         sync.Mutex
	activation Activation
}

func (a *recordingActivator) Activate(_ context.Context, activation Activation) ([]executionv1.ExecutionMode, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.activation = activation
	a.activation.EncryptedCredentialBundle = append([]byte(nil), activation.EncryptedCredentialBundle...)
	return []executionv1.ExecutionMode{executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API}, nil
}

func (a *recordingActivator) CredentialTransportKey() (string, []byte, error) {
	return "wck_test", []byte("12345678901234567890123456789012"), nil
}

type deterministicExecutor struct{}

func (deterministicExecutor) Execute(stream ExecutionStream) error {
	return stream.Send(&executionv1.ExecuteResponse{
		Event: &executionv1.ExecuteResponse_Completed{
			Completed: &executionv1.ExecutionCompleted{UpstreamRequestId: "msg_fake_worker"},
		},
	})
}

func (deterministicExecutor) CountTokens(_ context.Context, _ *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	return &executionv1.CountTokensResponse{
		StatusCode:            200,
		AnthropicResponseJson: []byte(`{"input_tokens":7}`),
		UpstreamRequestId:     "req_fake_count",
	}, nil
}

type leakingExecutor struct{}

func (leakingExecutor) Execute(ExecutionStream) error {
	return errors.New("upstream leaked Authorization: Bearer secret-value")
}

func (leakingExecutor) CountTokens(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	return nil, errors.New("upstream leaked refresh_token=secret-value")
}

type staticHealth struct{}

func (staticHealth) ModeHealth(context.Context) []ModeHealth {
	return []ModeHealth{
		{Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE, Healthy: false, ReasonCode: "not_activated"},
		{Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, Healthy: true},
	}
}

type rpcFixture struct {
	client    executionv1.WorkerRuntimeServiceClient
	issuer    *ticket.Issuer
	identity  Identity
	activator *recordingActivator
	now       time.Time
	close     func()
}

func newRPCFixture(t *testing.T) rpcFixture {
	return newRPCFixtureWithExecutor(t, deterministicExecutor{})
}

func newRPCFixtureWithExecutor(t *testing.T, executor Executor) rpcFixture {
	t.Helper()
	now := time.Unix(2_000_000_000, 0)
	identity := Identity{AccountID: "account-1", SlotID: "slot-1", NodeID: "srv74", Epoch: 12}
	publicKey, privateKey, err := ed25519.GenerateKey(strings.NewReader(strings.Repeat("w", ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	issuer, _ := ticket.NewIssuer(privateKey)
	verifier, _ := ticket.NewVerifier(publicKey)
	guard, err := NewGuard(verifier, identity, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	activator := &recordingActivator{}
	runtimeServer, err := NewRuntimeServer(RuntimeServerConfig{
		Guard: guard, Identity: identity, Activator: activator,
		Executor: executor, HealthSource: staticHealth{},
		ImageDigest: "registry/worker@sha256:" + strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}

	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	runtimeServer.Register(grpcServer)
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	connection, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		grpcServer.Stop()
		_ = listener.Close()
		t.Fatal(err)
	}
	return rpcFixture{
		client: executionv1.NewWorkerRuntimeServiceClient(connection),
		issuer: issuer, identity: identity, activator: activator, now: now,
		close: func() {
			_ = connection.Close()
			grpcServer.Stop()
			_ = listener.Close()
		},
	}
}

func TestWorkerGRPCRedactsUnclassifiedExecutorErrors(t *testing.T) {
	fixture := newRPCFixtureWithExecutor(t, leakingExecutor{})
	defer fixture.close()
	ctx := context.Background()

	_, err := fixture.client.CountTokens(ctx, &executionv1.WorkerRuntimeServiceCountTokensRequest{
		ExecutionTicket: fixture.ticket(t, "count_tokens"),
		Request:         &executionv1.CountTokensRequest{AccountId: fixture.identity.AccountID},
	})
	if status.Code(err) != codes.Internal || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("count_tokens leaked executor error: %v", err)
	}

	stream, err := fixture.client.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{
		Event: &executionv1.WorkerRuntimeServiceExecuteRequest_Begin{
			Begin: &executionv1.WorkerBeginExecution{
				ExecutionTicket: fixture.ticket(t, "messages"),
				Request: &executionv1.BeginExecution{
					RequestId: "request-redaction", AccountId: fixture.identity.AccountID,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Internal || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("execute leaked executor error: %v", err)
	}
}

func (f rpcFixture) ticket(t *testing.T, scope string) string {
	t.Helper()
	claims, err := ticket.NewClaims(f.identity.AccountID, f.identity.SlotID, f.identity.NodeID, f.identity.Epoch, []string{scope}, f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rawTicket, err := f.issuer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	return rawTicket
}

func TestWorkerGRPCActivationHealthAndCountTokens(t *testing.T) {
	fixture := newRPCFixture(t)
	defer fixture.close()
	ctx := context.Background()
	transportKey, err := fixture.client.CredentialTransportKey(ctx, &executionv1.CredentialTransportKeyRequest{
		ExecutionTicket: fixture.ticket(t, "credential_key"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if transportKey.GetSlotId() != fixture.identity.SlotID || transportKey.GetExecutionEpoch() != fixture.identity.Epoch ||
		transportKey.GetKeyId() != "wck_test" || len(transportKey.GetPublicKey()) != 32 {
		t.Fatalf("unexpected credential transport key: %+v", transportKey)
	}

	activation, err := fixture.client.Activate(ctx, &executionv1.ActivateRequest{
		ExecutionTicket:           fixture.ticket(t, "activate"),
		CredentialLeaseId:         "credential-lease-1",
		EncryptedCredentialBundle: []byte("ciphertext-only"),
		ProxyLeaseId:              "proxy-lease-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if activation.GetSlotId() != fixture.identity.SlotID || activation.GetExecutionEpoch() != fixture.identity.Epoch {
		t.Fatalf("unexpected activation response: %+v", activation)
	}
	if got := string(fixture.activator.activation.EncryptedCredentialBundle); got != "ciphertext-only" {
		t.Fatalf("activator did not receive bundle: %q", got)
	}

	healthTicket := fixture.ticket(t, "health")
	health, err := fixture.client.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: healthTicket})
	if err != nil {
		t.Fatal(err)
	}
	if len(health.GetModes()) != 2 || health.GetExecutionEpoch() != fixture.identity.Epoch {
		t.Fatalf("unexpected health response: %+v", health)
	}
	if _, err := fixture.client.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: healthTicket}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected replay rejection, got %v", err)
	}

	count, err := fixture.client.CountTokens(ctx, &executionv1.WorkerRuntimeServiceCountTokensRequest{
		ExecutionTicket: fixture.ticket(t, "count_tokens"),
		Request: &executionv1.CountTokensRequest{
			AccountId: fixture.identity.AccountID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if count.GetResponse().GetStatusCode() != 200 || count.GetResponse().GetUpstreamRequestId() != "req_fake_count" {
		t.Fatalf("unexpected count response: %+v", count)
	}
}

func TestWorkerGRPCExecuteRequiresAuthorizedBegin(t *testing.T) {
	fixture := newRPCFixture(t)
	defer fixture.close()
	ctx := context.Background()

	stream, err := fixture.client.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{
		Event: &executionv1.WorkerRuntimeServiceExecuteRequest_Begin{
			Begin: &executionv1.WorkerBeginExecution{
				ExecutionTicket: fixture.ticket(t, "messages"),
				Request: &executionv1.BeginExecution{
					RequestId: "request-1", AccountId: fixture.identity.AccountID,
					Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API,
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if response.GetResponse().GetCompleted().GetUpstreamRequestId() != "msg_fake_worker" {
		t.Fatalf("unexpected execute response: %+v", response)
	}

	badStream, err := fixture.client.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := badStream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{
		Event: &executionv1.WorkerRuntimeServiceExecuteRequest_ToolResult{
			ToolResult: &executionv1.ToolResult{ToolUseId: "tool-1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := badStream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected begin validation error, got %v", err)
	}
}

func TestWorkerGRPCRejectsWrongEpoch(t *testing.T) {
	fixture := newRPCFixture(t)
	defer fixture.close()
	wrong := fixture.identity
	wrong.Epoch++
	claims, _ := ticket.NewClaims(wrong.AccountID, wrong.SlotID, wrong.NodeID, wrong.Epoch, []string{"health"}, fixture.now, time.Minute)
	rawTicket, _ := fixture.issuer.Sign(claims)
	if _, err := fixture.client.Health(context.Background(), &executionv1.HealthRequest{ExecutionTicket: rawTicket}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected epoch rejection, got %v", err)
	}
}

type secureRPCFixture struct {
	client            executionv1.WorkerRuntimeServiceClient
	issuer            *ticket.Issuer
	identity          Identity
	now               time.Time
	workerRecipient   *credential.Recipient
	rotationRecipient *credential.Recipient
	activator         *SecureActivator
	close             func()
}

func newSecureRPCFixture(t *testing.T) secureRPCFixture {
	t.Helper()
	now := time.Unix(2_000_000_100, 0)
	identity := Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-secure", NodeID: "srv74", Epoch: 19}
	publicKey, privateKey, err := ed25519.GenerateKey(strings.NewReader(strings.Repeat("s", ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	issuer, _ := ticket.NewIssuer(privateKey)
	verifier, _ := ticket.NewVerifier(publicKey)
	guard, err := NewGuard(verifier, identity, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	workerRecipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x6a}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	rotationRecipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x6b}, 32)))
	if err != nil {
		workerRecipient.Destroy()
		t.Fatal(err)
	}
	activator, err := NewSecureActivator(SecureActivatorConfig{
		Identity: identity, Recipient: workerRecipient, Onboarder: &fakeOnboardingEngine{},
	})
	if err != nil {
		workerRecipient.Destroy()
		rotationRecipient.Destroy()
		t.Fatal(err)
	}
	runtimeServer, err := NewRuntimeServer(RuntimeServerConfig{
		Guard: guard, Identity: identity, Activator: activator, Executor: deterministicExecutor{},
		HealthSource: activator, ImageDigest: "registry/worker@sha256:" + strings.Repeat("b", 64),
	})
	if err != nil {
		workerRecipient.Destroy()
		rotationRecipient.Destroy()
		t.Fatal(err)
	}
	listener := bufconn.Listen(2 << 20)
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(3<<20), grpc.MaxSendMsgSize(3<<20))
	runtimeServer.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	connection, err := grpc.NewClient(
		"passthrough:///secure-bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(3<<20), grpc.MaxCallSendMsgSize(3<<20)),
	)
	if err != nil {
		grpcServer.Stop()
		_ = listener.Close()
		workerRecipient.Destroy()
		rotationRecipient.Destroy()
		t.Fatal(err)
	}
	return secureRPCFixture{
		client: executionv1.NewWorkerRuntimeServiceClient(connection), issuer: issuer, identity: identity, now: now,
		workerRecipient: workerRecipient, rotationRecipient: rotationRecipient, activator: activator,
		close: func() {
			_ = connection.Close()
			grpcServer.Stop()
			_ = listener.Close()
			workerRecipient.Destroy()
			rotationRecipient.Destroy()
		},
	}
}

func (f secureRPCFixture) ticket(t *testing.T, scope string) string {
	t.Helper()
	claims, err := ticket.NewClaims(f.identity.AccountID, f.identity.SlotID, f.identity.NodeID, f.identity.Epoch, []string{scope}, f.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rawTicket, err := f.issuer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	return rawTicket
}

func (f secureRPCFixture) activationBundle(t *testing.T, leaseID, proxyLeaseID, secret string) []byte {
	t.Helper()
	rotationKeyID, rotationPublicKey, err := f.rotationRecipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := EncodeActivationPackage(ActivationPackage{
		Input:                  OnboardingInput{Source: OnboardingSessionKey, AuthType: AuthTypeOAuth, Secret: []byte(secret)},
		RotationRecipientKeyID: rotationKeyID, RotationRecipientPublicKey: rotationPublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer zero(payload)
	workerKeyID, workerPublicKey, err := f.workerRecipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := credential.SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x6c}, 256)), workerKeyID, workerPublicKey, credential.TransportContext{
		AccountBinding: f.identity.AccountID, SlotID: f.identity.SlotID, ExecutionEpoch: f.identity.Epoch,
		LeaseID: leaseID, ProxyLeaseID: proxyLeaseID, Purpose: "onboarding",
	}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestWorkerSecureActivationStreamCommitsBeforeReady(t *testing.T) {
	fixture := newSecureRPCFixture(t)
	defer fixture.close()
	leaseID, proxyLeaseID, secret := "credential-lease-secure", "proxy-lease-secure", "stream-source-secret"
	bundle := fixture.activationBundle(t, leaseID, proxyLeaseID, secret)
	if bytes.Contains(bundle, []byte(secret)) {
		t.Fatal("secure activation bundle contains onboarding plaintext")
	}
	stream, err := fixture.client.SecureActivate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&executionv1.SecureActivateRequest{Event: &executionv1.SecureActivateRequest_Begin{Begin: &executionv1.SecureActivateBegin{
		ExecutionTicket: fixture.ticket(t, "secure_activate"), CredentialLeaseId: leaseID,
		EncryptedCredentialBundle: bundle, ProxyLeaseId: proxyLeaseID,
	}}}); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	commit := response.GetCredentialCommit()
	if commit == nil || commit.GetAccountBinding() != fixture.identity.AccountID || commit.GetSlotId() != fixture.identity.SlotID ||
		commit.GetExecutionEpoch() != fixture.identity.Epoch || commit.GetCredentialLeaseId() != leaseID || commit.GetProxyLeaseId() != proxyLeaseID {
		t.Fatalf("unexpected secure credential commit: %+v", commit)
	}
	if bytes.Contains(commit.GetSealedCredentialBundle(), []byte(secret)) || bytes.Contains(commit.GetSealedCredentialBundle(), []byte("normalized-"+secret)) {
		t.Fatal("secure credential commit contains plaintext")
	}
	rotationPlaintext, err := fixture.rotationRecipient.Open(context.Background(), commit.GetSealedCredentialBundle(), credential.TransportContext{
		AccountBinding: fixture.identity.AccountID, SlotID: fixture.identity.SlotID, ExecutionEpoch: fixture.identity.Epoch,
		LeaseID: leaseID, ProxyLeaseID: proxyLeaseID, Purpose: "rotation",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer zero(rotationPlaintext)
	material, err := credential.DecodeRotationMaterial(rotationPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	defer material.Destroy()
	if material.AuthType != AuthTypeOAuth || !bytes.Contains(material.Plaintext, []byte("normalized-"+secret)) {
		t.Fatalf("unexpected rotation material: %+v", material)
	}
	versionID := "11111111-2222-4333-8444-555555555555"
	if err := stream.Send(&executionv1.SecureActivateRequest{Event: &executionv1.SecureActivateRequest_CredentialCommitAck{CredentialCommitAck: &executionv1.CredentialCommitAck{VersionId: versionID}}}); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	response, err = stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	completed := response.GetCompleted()
	if completed == nil || completed.GetSlotId() != fixture.identity.SlotID || completed.GetExecutionEpoch() != fixture.identity.Epoch || len(completed.GetHealthyModes()) != 1 {
		t.Fatalf("unexpected secure activation completion: %+v", completed)
	}
	active, err := fixture.activator.ActiveCredential()
	if err != nil {
		t.Fatal(err)
	}
	defer active.Destroy()
	if active.VersionID != versionID || !bytes.Contains(active.CredentialJSON, []byte("normalized-"+secret)) {
		t.Fatalf("active credential changed incorrectly: %+v", active)
	}
}

func TestWorkerSecureActivationStreamRejectsInvalidCommitAck(t *testing.T) {
	fixture := newSecureRPCFixture(t)
	defer fixture.close()
	leaseID, proxyLeaseID := "credential-lease-bad-ack", "proxy-lease-bad-ack"
	stream, err := fixture.client.SecureActivate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&executionv1.SecureActivateRequest{Event: &executionv1.SecureActivateRequest_Begin{Begin: &executionv1.SecureActivateBegin{
		ExecutionTicket: fixture.ticket(t, "secure_activate"), CredentialLeaseId: leaseID,
		EncryptedCredentialBundle: fixture.activationBundle(t, leaseID, proxyLeaseID, "bad-ack-secret"), ProxyLeaseId: proxyLeaseID,
	}}}); err != nil {
		t.Fatal(err)
	}
	if response, err := stream.Recv(); err != nil || response.GetCredentialCommit() == nil {
		t.Fatalf("secure commit response/error = %+v / %v", response, err)
	}
	if err := stream.Send(&executionv1.SecureActivateRequest{Event: &executionv1.SecureActivateRequest_CredentialCommitAck{CredentialCommitAck: &executionv1.CredentialCommitAck{VersionId: " bad-version"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Internal || strings.Contains(err.Error(), "bad-ack-secret") {
		t.Fatalf("invalid commit ack error = %v", err)
	}
	if _, err := fixture.activator.ActiveCredential(); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("invalid ack activated credential: %v", err)
	}
}
