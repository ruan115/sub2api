package hostagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/service"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type bridgeTicketSource struct {
	issuer *ticket.Issuer
	now    time.Time
}

func (s bridgeTicketSource) Issue(_ context.Context, request TicketRequest) (string, error) {
	claims, err := ticket.NewClaims(request.AccountID, request.SlotID, request.NodeID, request.Epoch, []string{request.Scope}, s.now, time.Minute)
	if err != nil {
		return "", err
	}
	return s.issuer.Sign(claims)
}

type bridgeOnboarder struct{}

func (bridgeOnboarder) Onboard(_ context.Context, input worker.OnboardingInput) (worker.OnboardingResult, error) {
	payload, _ := json.Marshal(map[string]string{"access_token": "bridge-" + string(input.Secret)})
	return worker.OnboardingResult{AuthType: input.AuthType, CredentialJSON: payload}, nil
}

type bridgeExecutor struct{}

func (bridgeExecutor) Execute(worker.ExecutionStream) error {
	return errors.New("execution is outside secure activation bridge test")
}

func (bridgeExecutor) CountTokens(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	return nil, errors.New("count_tokens is outside secure activation bridge test")
}

type staticTicketSource struct{}

func (staticTicketSource) Issue(context.Context, TicketRequest) (string, error) {
	return "test-ticket", nil
}

type fixedVersionSink struct{}

func (fixedVersionSink) CommitSealedCredential(context.Context, worker.SealedCredentialCommitRequest) (string, error) {
	return "33333333-4444-4555-8666-777777777777", nil
}

type terminalBehaviorWorker struct {
	executionv1.UnimplementedWorkerRuntimeServiceServer
	accountBinding     string
	slotID             string
	epoch              uint64
	waitBeforeCommit   bool
	waitAfterCompleted bool
}

func (s *terminalBehaviorWorker) SecureActivate(stream grpc.BidiStreamingServer[executionv1.SecureActivateRequest, executionv1.SecureActivateResponse]) error {
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	begin := request.GetBegin()
	if begin == nil {
		return errors.New("missing begin")
	}
	if s.waitBeforeCommit {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	if err := stream.Send(&executionv1.SecureActivateResponse{Event: &executionv1.SecureActivateResponse_CredentialCommit{CredentialCommit: &executionv1.SealedCredentialCommit{
		AccountBinding: s.accountBinding, SlotId: s.slotID, ExecutionEpoch: s.epoch,
		CredentialLeaseId: begin.GetCredentialLeaseId(), ProxyLeaseId: begin.GetProxyLeaseId(),
		SealedCredentialBundle: []byte("sealed-test-bundle"),
	}}}); err != nil {
		return err
	}
	ack, err := stream.Recv()
	if err != nil || ack.GetCredentialCommitAck() == nil {
		return err
	}
	if err := stream.Send(&executionv1.SecureActivateResponse{Event: &executionv1.SecureActivateResponse_Completed{Completed: &executionv1.SecureActivationCompleted{
		SlotId: s.slotID, ExecutionEpoch: s.epoch,
		HealthyModes: []executionv1.ExecutionMode{executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API},
	}}}); err != nil {
		return err
	}
	if s.waitAfterCompleted {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	return nil
}

func newTerminalBehaviorRuntime(t *testing.T, server *terminalBehaviorWorker) (*Runtime, func()) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executionv1.RegisterWorkerRuntimeServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	connection, err := grpc.NewClient(
		"passthrough:///terminal-behavior-bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		client: executionv1.NewWorkerRuntimeServiceClient(connection), connection: connection, ticketSource: staticTicketSource{},
		identity: runtimeIdentity{AccountID: server.accountBinding, SlotID: server.slotID, NodeID: "srv74", Epoch: server.epoch},
	}
	return runtime, func() {
		_ = connection.Close()
		grpcServer.Stop()
		_ = listener.Close()
	}
}

type bridgeRotationAuthorizer struct {
	accountID string
	claim     service.RotationClaim
}

func (a *bridgeRotationAuthorizer) CommitAuthorizedRotation(ctx context.Context, claim service.RotationClaim, rotate func(context.Context, string) (string, error)) (string, error) {
	a.claim = claim
	return rotate(ctx, a.accountID)
}

type bridgeRotator struct {
	mu        sync.Mutex
	plaintext []byte
	authType  string
	calls     int
	err       error
}

func (r *bridgeRotator) RotateIdempotent(_ context.Context, _, accountID, authType, _ string, plaintext []byte) (credential.VersionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.authType = authType
	r.plaintext = append([]byte(nil), plaintext...)
	if r.err != nil {
		return credential.VersionRecord{}, r.err
	}
	return credential.VersionRecord{
		ID: "22222222-3333-4444-8555-666666666666", AccountID: accountID, AuthType: authType,
	}, nil
}

func (r *bridgeRotator) destroy() {
	r.mu.Lock()
	defer r.mu.Unlock()
	zero(r.plaintext)
}

type secureHostFixture struct {
	runtime           *Runtime
	identity          worker.Identity
	rawAccountID      string
	workerRecipient   *credential.Recipient
	rotationRecipient *credential.Recipient
	activator         *worker.SecureActivator
	close             func()
}

func newSecureHostFixture(t *testing.T) secureHostFixture {
	t.Helper()
	now := time.Unix(2_000_000_200, 0)
	rawAccountID := "account-10380"
	identity := worker.Identity{AccountID: provider.RuntimeAccountID(rawAccountID), SlotID: "slot-10380", NodeID: "srv74", Epoch: 23}
	publicKey, privateKey, err := ed25519.GenerateKey(strings.NewReader(strings.Repeat("h", ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	issuer, _ := ticket.NewIssuer(privateKey)
	verifier, _ := ticket.NewVerifier(publicKey)
	guard, err := worker.NewGuard(verifier, identity, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	workerRecipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x51}, 32)))
	rotationRecipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x52}, 32)))
	activator, err := worker.NewSecureActivator(worker.SecureActivatorConfig{
		Identity: identity, Recipient: workerRecipient, Onboarder: bridgeOnboarder{},
	})
	if err != nil {
		t.Fatal(err)
	}
	runtimeServer, err := worker.NewRuntimeServer(worker.RuntimeServerConfig{
		Guard: guard, Identity: identity, Activator: activator, Executor: bridgeExecutor{}, HealthSource: activator,
		ImageDigest: "registry/worker@sha256:" + strings.Repeat("c", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(3 << 20)
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(3<<20), grpc.MaxSendMsgSize(3<<20))
	runtimeServer.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	connection, err := grpc.NewClient(
		"passthrough:///host-secure-bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(3<<20), grpc.MaxCallSendMsgSize(3<<20)),
	)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		client: executionv1.NewWorkerRuntimeServiceClient(connection), connection: connection,
		ticketSource: bridgeTicketSource{issuer: issuer, now: now},
		identity:     runtimeIdentity{AccountID: identity.AccountID, SlotID: identity.SlotID, NodeID: identity.NodeID, Epoch: identity.Epoch},
	}
	return secureHostFixture{
		runtime: runtime, identity: identity, rawAccountID: rawAccountID, workerRecipient: workerRecipient,
		rotationRecipient: rotationRecipient, activator: activator,
		close: func() {
			_ = connection.Close()
			grpcServer.Stop()
			_ = listener.Close()
			workerRecipient.Destroy()
			rotationRecipient.Destroy()
		},
	}
}

func (f secureHostFixture) activation(t *testing.T, leaseID, proxyLeaseID, secret string) ActivationLease {
	t.Helper()
	transportKey, err := f.runtime.CredentialTransportKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rotationKeyID, rotationPublicKey, _ := f.rotationRecipient.PublicKey()
	payload, err := worker.EncodeActivationPackage(worker.ActivationPackage{
		Input:                  worker.OnboardingInput{Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth, Secret: []byte(secret)},
		RotationRecipientKeyID: rotationKeyID, RotationRecipientPublicKey: rotationPublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer zero(payload)
	sealed, err := credential.SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x53}, 256)), transportKey.KeyID, transportKey.PublicKey, credential.TransportContext{
		AccountBinding: f.identity.AccountID, SlotID: f.identity.SlotID, ExecutionEpoch: f.identity.Epoch,
		LeaseID: leaseID, ProxyLeaseID: proxyLeaseID, Purpose: "onboarding",
	}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return ActivationLease{CredentialLeaseID: leaseID, ProxyLeaseID: proxyLeaseID, EncryptedCredentialBundle: sealed}
}

func TestHostAgentSecureActivationStreamsCiphertextThroughVaultAck(t *testing.T) {
	fixture := newSecureHostFixture(t)
	defer fixture.close()
	authorizer := &bridgeRotationAuthorizer{accountID: fixture.rawAccountID}
	rotator := &bridgeRotator{}
	defer rotator.destroy()
	sink, err := service.NewCredentialRotationSink(service.CredentialRotationSinkConfig{
		Recipient: fixture.rotationRecipient, Authorizer: authorizer, Vault: rotator,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := "host-agent-stream-secret"
	modes, err := fixture.runtime.ActivateSecure(context.Background(), fixture.activation(t, "lease-host", "proxy-host", secret), sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(modes) != 1 || modes[0] != executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API {
		t.Fatalf("secure activation modes = %v", modes)
	}
	rotator.mu.Lock()
	calls, authType := rotator.calls, rotator.authType
	plaintext := append([]byte(nil), rotator.plaintext...)
	rotator.mu.Unlock()
	defer zero(plaintext)
	if calls != 1 || authType != worker.AuthTypeOAuth || !bytes.Contains(plaintext, []byte("bridge-"+secret)) {
		t.Fatalf("vault rotation calls/auth/plaintext = %d/%q/%q", calls, authType, plaintext)
	}
	if authorizer.claim.AccountBinding != fixture.identity.AccountID || authorizer.claim.SlotID != fixture.identity.SlotID ||
		authorizer.claim.ExecutionEpoch != fixture.identity.Epoch || authorizer.claim.CredentialLeaseID != "lease-host" ||
		authorizer.claim.ProxyLeaseID != "proxy-host" {
		t.Fatalf("rotation authorization claim = %+v", authorizer.claim)
	}
	active, err := fixture.activator.ActiveCredential()
	if err != nil {
		t.Fatal(err)
	}
	defer active.Destroy()
	if active.VersionID != "22222222-3333-4444-8555-666666666666" || !bytes.Contains(active.CredentialJSON, []byte("bridge-"+secret)) {
		t.Fatalf("worker active credential = %+v", active)
	}
}

func TestHostAgentSecureActivationDoesNotAckFailedVaultCommit(t *testing.T) {
	fixture := newSecureHostFixture(t)
	defer fixture.close()
	authorizer := &bridgeRotationAuthorizer{accountID: fixture.rawAccountID}
	rotator := &bridgeRotator{err: errors.New("kms leaked failed-vault-secret")}
	defer rotator.destroy()
	sink, _ := service.NewCredentialRotationSink(service.CredentialRotationSinkConfig{
		Recipient: fixture.rotationRecipient, Authorizer: authorizer, Vault: rotator,
	})
	_, err := fixture.runtime.ActivateSecure(context.Background(), fixture.activation(t, "lease-failed", "proxy-failed", "failed-vault-secret"), sink)
	if err == nil || strings.Contains(err.Error(), "failed-vault-secret") {
		t.Fatalf("failed vault activation error = %v", err)
	}
	if _, err := fixture.activator.ActiveCredential(); !errors.Is(err, worker.ErrActivationRejected) {
		t.Fatalf("failed vault commit activated worker: %v", err)
	}
}

func TestHostAgentSecureActivationTreatsCompletedAsTerminal(t *testing.T) {
	server := &terminalBehaviorWorker{
		accountBinding: "95a7c9f1f7654af7a836061a6561b839", slotID: "slot-terminal", epoch: 7,
		waitAfterCompleted: true,
	}
	runtime, closeRuntime := newTerminalBehaviorRuntime(t, server)
	defer closeRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	modes, err := runtime.ActivateSecure(ctx, ActivationLease{
		CredentialLeaseID: "lease-terminal", ProxyLeaseID: "proxy-terminal", EncryptedCredentialBundle: []byte("encrypted"),
	}, fixedVersionSink{})
	if err != nil {
		t.Fatal(err)
	}
	if len(modes) != 1 || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("terminal activation modes/elapsed = %v/%s", modes, time.Since(started))
	}
}

func TestHostAgentSecureActivationPreservesDeadline(t *testing.T) {
	server := &terminalBehaviorWorker{
		accountBinding: "95a7c9f1f7654af7a836061a6561b839", slotID: "slot-timeout", epoch: 8,
		waitBeforeCommit: true,
	}
	runtime, closeRuntime := newTerminalBehaviorRuntime(t, server)
	defer closeRuntime()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := runtime.ActivateSecure(ctx, ActivationLease{
		CredentialLeaseID: "lease-timeout", ProxyLeaseID: "proxy-timeout", EncryptedCredentialBundle: []byte("encrypted"),
	}, fixedVersionSink{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline activation error = %v", err)
	}
}
