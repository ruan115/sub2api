package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type diagnosticProjectionFunc func(context.Context, string, time.Time, time.Duration) (store.ProbeBinding, error)

func (f diagnosticProjectionFunc) ReadProbeBinding(ctx context.Context, slotID string, now time.Time, maxAge time.Duration) (store.ProbeBinding, error) {
	return f(ctx, slotID, now, maxAge)
}

type diagnosticNoopObserver struct{}

func (diagnosticNoopObserver) ObserveCommandResult(context.Context, string, *executionv1.CommandResult) error {
	return nil
}

type diagnosticUnusedModel struct{ calls atomic.Int32 }

func (m *diagnosticUnusedModel) Execute(worker.ExecutionStream) error {
	m.calls.Add(1)
	return errors.New("model execution must not be reached")
}
func (m *diagnosticUnusedModel) CountTokens(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	m.calls.Add(1)
	return nil, errors.New("model execution must not be reached")
}

type diagnosticRound struct {
	commandID, scope, nonce string
	denied, successful      bool
	key                     *executionv1.CredentialTransportKeyOutput
}

// This is a synthetic diagnostic executor, not the production provider/runtime
// registry. Its ticket bridge and worker RPCs are the actual implementations.
type diagnosticCommandExecutor struct {
	client   executionv1.WorkerRuntimeServiceClient
	verifier *ticket.Verifier
	identity worker.Identity
	now      func() time.Time
	rounds   chan diagnosticRound
}

func (*diagnosticCommandExecutor) Snapshot() hostagent.NodeSnapshot { return hostagent.NodeSnapshot{} }
func (*diagnosticCommandExecutor) RevokeEpoch(context.Context, *executionv1.RevokeEpochCommand) *executionv1.CommandResult {
	return &executionv1.CommandResult{Succeeded: false, ErrorCode: "unexpected_revoke"}
}
func (*diagnosticCommandExecutor) SecureActivate(context.Context, *executionv1.SecureActivationCommand, worker.SealedCredentialSink) *executionv1.CommandResult {
	return &executionv1.CommandResult{Succeeded: false, ErrorCode: "activation_not_allowed"}
}
func (e *diagnosticCommandExecutor) ExecuteSlotCommand(ctx context.Context, command *executionv1.SlotCommand) *executionv1.CommandResult {
	return e.run(ctx, command.GetCommandId(), "health", command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest())
}
func (e *diagnosticCommandExecutor) CredentialTransportKey(ctx context.Context, command *executionv1.CredentialKeyCommand) *executionv1.CommandResult {
	return e.run(ctx, command.GetCommandId(), "credential_key", command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest())
}

func (e *diagnosticCommandExecutor) run(ctx context.Context, commandID, scope, slotID string, epoch uint64, image string) *executionv1.CommandResult {
	round := diagnosticRound{commandID: commandID, scope: scope}
	result := &executionv1.CommandResult{CommandId: commandID, ErrorCode: "diagnostic_failed", Slot: &executionv1.SlotObservation{
		SlotId: slotID, ExecutionEpoch: epoch, ProviderRef: "container-1", ImageDigest: image, ActualState: "running",
	}}
	defer func() {
		select {
		case e.rounds <- round:
		case <-ctx.Done():
		}
	}()
	// A disallowed permission must fail locally without consuming the allowed
	// diagnostic attempt or falling back to an injected legacy ticket source.
	if raw, err := hostagent.RequestProbeTicket(ctx, "secure_activate"); err == nil || raw != "" {
		return result
	}
	raw, err := hostagent.RequestProbeTicket(ctx, scope)
	if err != nil {
		round.denied = raw == ""
		return result
	}
	claims, err := e.verifier.Verify(raw, e.now())
	if err != nil || claims.AccountID != e.identity.AccountID || claims.NodeID != e.identity.NodeID || claims.SlotID != slotID || claims.Epoch != epoch ||
		len(claims.Scopes) != 1 || claims.Scopes[0] != scope || claims.ExpiresAt > e.now().Add(5*time.Second).Unix() {
		return result
	}
	round.nonce = claims.Nonce
	otherIdentity := e.identity
	otherIdentity.AccountID = provider.RuntimeAccountID("account-other")
	other, err := worker.NewGuard(e.verifier, otherIdentity, e.now)
	if err != nil {
		return result
	}
	if _, err := other.Authorize(raw, scope); !errors.Is(err, worker.ErrIdentityMismatch) {
		return result
	}
	if _, err := e.client.CountTokens(ctx, &executionv1.WorkerRuntimeServiceCountTokensRequest{
		ExecutionTicket: raw, Request: &executionv1.CountTokensRequest{},
	}); status.Code(err) != codes.PermissionDenied {
		return result
	}
	if scope == "health" {
		challenge := strings.Repeat("a", 32)
		response, err := e.client.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: raw, Challenge: challenge})
		if err != nil || response.GetSlotId() != slotID || response.GetExecutionEpoch() != epoch || response.GetImageDigest() != image || response.GetChallenge() != challenge || response.GetLoadedState() != nil {
			return result
		}
		if _, err := e.client.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: raw, Challenge: challenge}); status.Code(err) != codes.AlreadyExists {
			return result
		}
	} else {
		response, err := e.client.CredentialTransportKey(ctx, &executionv1.CredentialTransportKeyRequest{ExecutionTicket: raw})
		if err != nil || response.GetSlotId() != slotID || response.GetExecutionEpoch() != epoch || credential.ValidateRecipientKey(response.GetKeyId(), response.GetPublicKey()) != nil {
			return result
		}
		round.key = &executionv1.CredentialTransportKeyOutput{KeyId: response.GetKeyId(), PublicKey: response.GetPublicKey()}
		if _, err := e.client.CredentialTransportKey(ctx, &executionv1.CredentialTransportKeyRequest{ExecutionTicket: raw}); status.Code(err) != codes.AlreadyExists {
			return result
		}
	}
	if duplicate, err := hostagent.RequestProbeTicket(ctx, scope); err == nil || duplicate != "" {
		return result
	}
	round.successful = true
	result.Succeeded, result.ErrorCode, result.Slot.Healthy = true, "", true
	result.CredentialTransportKey = round.key
	return result
}

func TestProbeTicketTLSControlClientToActualWorker(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		name := "default_closed"
		if enabled {
			name = "explicitly_enabled"
		}
		t.Run(name, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			issuer, _ := ticket.NewIssuer(private)
			verifier, _ := ticket.NewVerifier(public)
			backend := lease.NewMemoryBackend(now)
			var repository *store.MemoryRepository
			h := newControlHarnessWithConfig(t, 10*time.Second, func(config *Config) {
				config.Now, config.CommandObserver = now, diagnosticNoopObserver{}
				if enabled {
					config.ProbeTickets = &ProbeTicketConfig{Issuer: issuer, Leases: backend, Repository: diagnosticProjectionFunc(func(ctx context.Context, slotID string, at time.Time, maxAge time.Duration) (store.ProbeBinding, error) {
						return repository.ReadProbeBinding(ctx, slotID, at, maxAge)
					})}
				}
			})
			repository = h.repository
			clock.Store(h.now.UnixNano())
			identity := worker.Identity{AccountID: provider.RuntimeAccountID("account-1"), SlotID: "slot-1", NodeID: "srv74", Epoch: 1}
			guard, _ := worker.NewGuard(verifier, identity, now)
			recipient, err := credential.NewRecipient(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			defer recipient.Destroy()
			activator, err := worker.NewSecureActivator(worker.SecureActivatorConfig{Identity: identity, Recipient: recipient, Onboarder: loadedProofOnboarder{}})
			if err != nil {
				t.Fatal(err)
			}
			defer activator.Drain()
			model := &diagnosticUnusedModel{}
			image := "sha256:" + strings.Repeat("a", 64)
			workerServer, err := worker.NewRuntimeServer(worker.RuntimeServerConfig{Guard: guard, Identity: identity, Activator: activator, HealthSource: activator, Executor: model, ImageDigest: image})
			if err != nil {
				t.Fatal(err)
			}
			listener := bufconn.Listen(1 << 20)
			defer listener.Close()
			server := grpc.NewServer()
			workerServer.Register(server)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			connection, err := grpc.NewClient("passthrough:///diagnostic-worker", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			executor := &diagnosticCommandExecutor{client: executionv1.NewWorkerRuntimeServiceClient(connection), verifier: verifier, identity: identity, now: now, rounds: make(chan diagnosticRound, 4)}
			client := diagnosticEnrolledClient(t, h)
			controlClient, err := hostagent.NewControlClient(hostagent.ControlClientConfig{
				Client: client, Executor: executor, ActivationExecutor: executor, NodeID: "srv74", EnableProbeTickets: true,
				Capabilities: []string{"probe_tickets", "secure_activation", "oauth_api"}, Capacity: helloEvent("srv74").GetHello().GetCapacity(),
				HeartbeatInterval: 50 * time.Millisecond, ReconnectMin: time.Second, ReconnectMax: 2 * time.Second, MaxConcurrentCommands: 2, CommandQueue: 4, Now: now,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- controlClient.Run(ctx) }()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Error("control client did not stop")
				}
			}()
			eventually(t, func() bool {
				node, err := h.repository.GetNode(ctx, "srv74")
				return err == nil && h.server.ValidateControlSession(ctx, "srv74", node.ControlSessionID) == nil
			})
			assignment := reserveControlTestAssignment(t, h, image)
			if assignment.ExecutionEpoch != identity.Epoch {
				t.Fatal("fixture epoch differs from worker")
			}
			if _, err := h.repository.ObserveAssignment(ctx, store.AssignmentObservation{SlotID: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ActualState: "running", Healthy: false, ObservedAt: now()}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.repository.GrantExecutionLease(ctx, store.ExecutionLease{ID: "diagnostic-lease", SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 1, OwnerID: "owner-1", CreatedAt: now(), UpdatedAt: now(), ExpiresAt: now().Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			claim := lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 1, OwnerID: "owner-1"}
			if err := backend.Acquire(ctx, claim, time.Minute); err != nil {
				t.Fatal(err)
			}
			run := func(id, scope string, allowed bool) diagnosticRound {
				t.Helper()
				response := &executionv1.NodeControlServiceControlResponse{}
				if scope == "health" {
					response.Event = &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{CommandId: id, Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: 1, ImageDigest: image, Deadline: timestamppb.New(now().Add(10 * time.Second)), Metadata: map[string]string{"desired_generation": "1"}}}
				} else {
					response.Event = &executionv1.NodeControlServiceControlResponse_CredentialKeyCommand{CredentialKeyCommand: &executionv1.CredentialKeyCommand{CommandId: id, SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: 1, ImageDigest: image, Deadline: timestamppb.New(now().Add(10 * time.Second)), DesiredGeneration: 1}}
				}
				if err := h.server.Dispatch(ctx, "srv74", response); err != nil {
					t.Fatal(err)
				}
				var round diagnosticRound
				select {
				case round = <-executor.rounds:
				case <-time.After(3 * time.Second):
					t.Fatal("diagnostic round timed out")
				}
				if round.commandID != id || round.scope != scope || round.successful != allowed || round.denied == allowed {
					t.Fatalf("unexpected diagnostic result: %+v", round)
				}
				eventually(t, func() bool { _, found := h.repository.GetCommandResult(id); return found })
				return round
			}
			first := run("diagnostic-health-1", "health", enabled)
			second := run("diagnostic-key-1", "credential_key", enabled)
			if enabled && (first.nonce == second.nonce || second.key == nil) {
				t.Fatal("diagnostic permissions did not get independent nonces/public key")
			}
			backend.SetAvailable(false)
			run("diagnostic-offline", "health", false)
			if model.calls.Load() != 0 || activator.Ready() {
				t.Fatal("diagnostic tickets reached model execution or activated credentials")
			}
			if raw, err := hostagent.RequestProbeTicket(context.Background(), "health"); err == nil || raw != "" {
				t.Fatal("diagnostic ticket escaped its command context")
			}
		})
	}
}

func diagnosticEnrolledClient(t *testing.T, h *controlHarness) executionv1.NodeControlServiceClient {
	t.Helper()
	enrollmentToken, err := h.server.CreateEnrollment(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	_, private, public := generateNodeKey(t)
	anonymous := executionv1.NewNodeControlServiceClient(h.dial(t, nil))
	enrollment, err := anonymous.EnrollNode(context.Background(), &executionv1.EnrollNodeRequest{
		EnrollmentToken: enrollmentToken.Token, NodeId: "srv74", PublicKeyPem: string(public), ProtocolVersion: &executionv1.ProtocolVersion{Major: CurrentProtocolMajor, Minor: CurrentProtocolMinor},
	})
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair([]byte(enrollment.GetCertificatePem()), private)
	if err != nil {
		t.Fatal(err)
	}
	return executionv1.NewNodeControlServiceClient(h.dial(t, &certificate))
}
