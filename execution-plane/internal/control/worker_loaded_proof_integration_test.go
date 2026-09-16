package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/workerproof"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type loadedProofOnboarder struct{}

func (loadedProofOnboarder) Onboard(_ context.Context, input worker.OnboardingInput) (worker.OnboardingResult, error) {
	value, _ := json.Marshal(map[string]string{"access_token": "normalized-" + string(input.Secret)})
	return worker.OnboardingResult{AuthType: input.AuthType, CredentialJSON: value}, nil
}

// The test does not execute a model request. Any unexpected execution method
// call hits an absent embedded implementation rather than an external API.
type loadedProofUnusedExecutor struct{ worker.Executor }
type loadedProofHealthReader struct {
	client executionv1.WorkerRuntimeServiceClient
	ticket func(string) string
	last   *executionv1.HealthResponse
	hook   func()
}

func (r *loadedProofHealthReader) ReadHealth(ctx context.Context, _ store.WorkerReadinessBinding, challenge string) (*executionv1.HealthResponse, error) {
	response, err := r.client.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: r.ticket("health"), Challenge: challenge})
	r.last = response
	if r.hook != nil {
		r.hook()
	}
	return response, err
}

type loadedProofCachedReader struct{ response *executionv1.HealthResponse }

func (r loadedProofCachedReader) ReadHealth(context.Context, store.WorkerReadinessBinding, string) (*executionv1.HealthResponse, error) {
	return proto.Clone(r.response).(*executionv1.HealthResponse), nil
}

func TestWorkerLoadedProofRealRPCVaultAckRotationAndDrain(t *testing.T) {
	ctx := context.Background()
	h := newControlHarness(t, 10*time.Second)
	controlStream := enrollAndOpenControl(t, h, helloEvent("srv74"))
	node, _ := h.repository.GetNode(ctx, "srv74")
	eventually(t, func() bool { return h.server.ValidateControlSession(ctx, "srv74", node.ControlSessionID) == nil })
	image := "sha256:" + strings.Repeat("a", 64)
	assignment := reserveControlTestAssignment(t, h, image)
	if _, err := h.repository.GrantExecutionLease(ctx, store.ExecutionLease{ID: "lease-1", SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: assignment.ExecutionEpoch, OwnerID: "owner-1", CreatedAt: h.now, UpdatedAt: h.now, ExpiresAt: h.now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	// Establish a synthetic provider observation on the actual authenticated
	// NodeControl stream. Actual provider INSPECT behavior is covered by B2a.
	command := &executionv1.SlotCommand{CommandId: "loaded-initial-inspect", Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: assignment.ExecutionEpoch, ImageDigest: image, Deadline: timestamppb.New(h.now.Add(time.Minute))}
	if err := h.server.Dispatch(ctx, "srv74", &executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: command}}); err != nil {
		t.Fatal(err)
	}
	if response, err := controlStream.Recv(); err != nil || response.GetSlotCommand().GetCommandId() != command.CommandId {
		t.Fatal(response, err)
	}
	if err := controlStream.Send(&executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_CommandResult{CommandResult: &executionv1.CommandResult{CommandId: command.CommandId, Succeeded: true, Slot: &executionv1.SlotObservation{SlotId: "slot-1", ExecutionEpoch: assignment.ExecutionEpoch, ProviderRef: "container-1", ImageDigest: image, ActualState: "running", Healthy: true}}}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, ok := h.repository.GetCommandResult(command.CommandId); return ok })
	if _, err := h.repository.GrantProxyReservation(ctx, store.ProxyReservationGrant{ReservationID: "reservation-1", AccountID: "account-1", DesiredGeneration: 1, ProxyBindingID: "123", BindingRevision: 1, GrantEventID: "grant-1", CreatedAt: h.now, UpdatedAt: h.now}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repository.GrantProxyLease(ctx, store.ProxyLease{ID: "proxy-1", ReservationID: "reservation-1", AccountID: "account-1", DesiredGeneration: 1, BindingRevision: 1, SlotID: "slot-1", ExecutionEpoch: assignment.ExecutionEpoch, CreatedAt: h.now, UpdatedAt: h.now}); err != nil {
		t.Fatal(err)
	}
	backend := lease.NewMemoryBackend(func() time.Time { return h.now })
	if err := backend.Acquire(ctx, lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: assignment.ExecutionEpoch, OwnerID: "owner-1"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	kms, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x41}, 32), "kms-fixture", "v1")
	if err != nil {
		t.Fatal(err)
	}
	crypto, err := credential.NewService(kms)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := credential.NewVault(crypto, h.repository, credential.VaultConfig{Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	workerRecipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer workerRecipient.Destroy()
	rotationRecipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer rotationRecipient.Destroy()
	identity := worker.Identity{AccountID: provider.RuntimeAccountID("account-1"), SlotID: "slot-1", NodeID: "srv74", Epoch: assignment.ExecutionEpoch}
	activator, err := worker.NewSecureActivator(worker.SecureActivatorConfig{Identity: identity, Recipient: workerRecipient, Onboarder: loadedProofOnboarder{}})
	if err != nil {
		t.Fatal(err)
	}
	defer activator.Drain()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ticket.NewIssuer(private)
	if err != nil {
		t.Fatal(err)
	}
	ticketVerifier, err := ticket.NewVerifier(public)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := worker.NewGuard(ticketVerifier, identity, func() time.Time { return h.now })
	if err != nil {
		t.Fatal(err)
	}
	issue := func(scope string) string {
		t.Helper()
		claims, err := ticket.NewClaims(identity.AccountID, identity.SlotID, identity.NodeID, identity.Epoch, []string{scope}, h.now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := issuer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	runtimeServer, err := worker.NewRuntimeServer(worker.RuntimeServerConfig{Guard: guard, Identity: identity, Activator: activator, HealthSource: activator, Executor: loadedProofUnusedExecutor{}, ImageDigest: image})
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(3 << 20)
	defer listener.Close()
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(3<<20), grpc.MaxSendMsgSize(3<<20))
	runtimeServer.Register(grpcServer)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	connection, err := grpc.NewClient("passthrough:///loaded-proof-worker", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := executionv1.NewWorkerRuntimeServiceClient(connection)
	healthReader := &loadedProofHealthReader{client: client, ticket: issue}
	config := workerproof.Config{Repository: h.repository, Sessions: h.server, Leases: backend, Health: healthReader, Now: func() time.Time { return h.now }}
	checker, err := workerproof.New(config)
	if err != nil {
		t.Fatal(err)
	}
	want := dataplane.Binding{AccountID: "account-1", SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: assignment.ExecutionEpoch, RouteGeneration: 1}
	check := func(allowed bool) workerproof.Receipt {
		t.Helper()
		receipt, err := checker.Check(ctx, want, executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API)
		if allowed {
			if err != nil || receipt.ActivationRevision == 0 {
				t.Fatal(receipt, err)
			}
		} else if err != workerproof.ErrUnavailable || receipt != (workerproof.Receipt{}) {
			t.Fatal(receipt, err)
		}
		return receipt
	}
	check(false)
	activate := func(leaseID, material string, beforeAck func(credential.VersionRecord)) credential.VersionRecord {
		t.Helper()
		keyID, publicKey, err := rotationRecipient.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		payload, err := worker.EncodeActivationPackage(worker.ActivationPackage{Input: worker.OnboardingInput{Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth, Secret: []byte(material)}, RotationRecipientKeyID: keyID, RotationRecipientPublicKey: publicKey})
		if err != nil {
			t.Fatal(err)
		}
		defer clear(payload)
		workerKey, workerPublic, err := workerRecipient.PublicKey()
		if err != nil {
			t.Fatal(err)
		}
		bundle, err := credential.SealForRecipient(ctx, rand.Reader, workerKey, workerPublic, credential.TransportContext{AccountBinding: identity.AccountID, SlotID: identity.SlotID, ExecutionEpoch: identity.Epoch, LeaseID: leaseID, ProxyLeaseID: "proxy-1", Purpose: "onboarding"}, payload)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(bundle)
		stream, err := client.SecureActivate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&executionv1.SecureActivateRequest{Event: &executionv1.SecureActivateRequest_Begin{Begin: &executionv1.SecureActivateBegin{ExecutionTicket: issue("secure_activate"), CredentialLeaseId: leaseID, EncryptedCredentialBundle: bundle, ProxyLeaseId: "proxy-1"}}}); err != nil {
			t.Fatal(err)
		}
		response, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		commit := response.GetCredentialCommit()
		if commit == nil || commit.AccountBinding != identity.AccountID || commit.SlotId != identity.SlotID || commit.ExecutionEpoch != identity.Epoch || commit.CredentialLeaseId != leaseID || commit.ProxyLeaseId != "proxy-1" {
			t.Fatal("incorrect sealed commit binding")
		}
		plaintext, err := rotationRecipient.Open(ctx, commit.SealedCredentialBundle, credential.TransportContext{AccountBinding: identity.AccountID, SlotID: identity.SlotID, ExecutionEpoch: identity.Epoch, LeaseID: leaseID, ProxyLeaseID: "proxy-1", Purpose: "rotation"})
		if err != nil {
			t.Fatal(err)
		}
		defer clear(plaintext)
		rotation, err := credential.DecodeRotationMaterial(plaintext)
		if err != nil {
			t.Fatal(err)
		}
		defer rotation.Destroy()
		version, err := vault.Rotate(ctx, "account-1", rotation.AuthType, "", rotation.Plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if beforeAck != nil {
			beforeAck(version)
		}
		if err := stream.Send(&executionv1.SecureActivateRequest{Event: &executionv1.SecureActivateRequest_CredentialCommitAck{CredentialCommitAck: &executionv1.CredentialCommitAck{VersionId: version.ID}}}); err != nil {
			t.Fatal(err)
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if response, err := stream.Recv(); err != nil || response.GetCompleted() == nil {
			t.Fatal("activation not completed", err)
		}
		return version
	}
	first := activate("activation-lease-1", "fixture-material-one", func(credential.VersionRecord) {
		health, err := client.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: issue("health"), Challenge: strings.Repeat("c", 32)})
		if err != nil || health.GetLoadedState() != nil {
			t.Fatal("worker published loaded state before vault ack", err)
		}
		check(false) // durable active pointer exists, but worker has not installed it.
	})
	proof := check(true)
	if proof.Authority.CredentialVersionID != first.ID || proof.Authority.CredentialVersionNumber != 1 || proof.ActivationRevision != 1 {
		t.Fatal(proof)
	}
	encoded, err := proto.Marshal(healthReader.last)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"fixture-material-one", "normalized-fixture-material-one", "access_token", "ciphertext"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatal("health response contains secret material")
		}
	}
	cached := config
	cached.Health = loadedProofCachedReader{response: healthReader.last}
	cachedChecker, _ := workerproof.New(cached)
	if _, err := cachedChecker.Check(ctx, want, executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API); err != workerproof.ErrUnavailable {
		t.Fatal("cached health report satisfied a new challenge")
	}
	if _, err := vault.Rotate(ctx, "account-1", "oauth", "", []byte(`{"access_token":"fixture-out-of-band"}`)); err != nil {
		t.Fatal(err)
	}
	check(false) // active control version changed; old worker remains loaded.
	third := activate("activation-lease-2", "fixture-material-two", nil)
	proof = check(true)
	if proof.Authority.CredentialVersionID != third.ID || proof.Authority.CredentialVersionNumber != 3 || proof.ActivationRevision != 2 {
		t.Fatal("worker revision was confused with vault sequence", proof)
	}
	healthReader.hook = func() {
		if _, err := vault.Rotate(ctx, "account-1", "oauth", "", []byte(`{"access_token":"fixture-concurrent"}`)); err != nil {
			t.Fatal(err)
		}
	}
	check(false) // mutation after the actual Health reply is caught by re-read.
	healthReader.hook = nil
	activate("activation-lease-3", "fixture-material-three", nil)
	check(true)
	activator.Drain()
	check(false)
	if health, err := client.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: issue("health"), Challenge: strings.Repeat("d", 32)}); err != nil || health.GetLoadedState() != nil {
		t.Fatal("drain retained a loaded-state report", err)
	}
	if err := h.repository.RevokeProxyLease(ctx, "proxy-1", h.now); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repository.ReadWorkerReadinessBinding(ctx, "slot-1", h.now, 45*time.Second); !errors.Is(err, store.ErrWorkerReadinessBindingUnavailable) {
		t.Fatal("revoked proxy remained a candidate", err)
	}
}
