package control

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type probeTicketReaderFunc func(context.Context, string, time.Time, time.Duration) (store.ProbeBinding, error)

func (f probeTicketReaderFunc) ReadProbeBinding(c context.Context, s string, n time.Time, age time.Duration) (store.ProbeBinding, error) {
	return f(c, s, n, age)
}

type probeTicketLeaseFunc func(context.Context, lease.Claim) error

func (f probeTicketLeaseFunc) Validate(c context.Context, claim lease.Claim) error {
	return f(c, claim)
}

type ticketFixture struct {
	server   *Server
	session  *nodeSession
	bound    store.ProbeBinding
	verifier *ticket.Verifier
	reads    atomic.Int64
	leases   atomic.Int64
}

func newTicketFixture(t *testing.T) *ticketFixture {
	t.Helper()
	s, session := probeFixture(t, 64, 128)
	now := s.config.Now()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, _ := ticket.NewIssuer(private)
	verifier, _ := ticket.NewVerifier(pub)
	f := &ticketFixture{server: s, session: session, verifier: verifier, bound: store.ProbeBinding{
		AccountID: "account-1", SlotID: "slot-1", NodeID: "node-1", ProviderRef: "container-1", LeaseOwnerID: "owner-1",
		ControlSessionID: session.id, ImageDigest: "sha256:" + strings.Repeat("a", 64), ExecutionEpoch: 1, RouteGeneration: 2,
		NodeSeenAt: now, LeaseExpiresAt: now.Add(time.Minute),
	}}
	session.capabilities = map[string]struct{}{probeTicketsCapability: {}}
	s.config.ProbeTickets, err = normalizeProbeTicketConfig(&ProbeTicketConfig{Issuer: issuer,
		Repository: probeTicketReaderFunc(func(_ context.Context, slot string, _ time.Time, age time.Duration) (store.ProbeBinding, error) {
			f.reads.Add(1)
			if slot != f.bound.SlotID || age != 45*time.Second {
				t.Error("wrong metadata query")
			}
			return f.bound, nil
		}),
		Leases: probeTicketLeaseFunc(func(_ context.Context, c lease.Claim) error {
			f.leases.Add(1)
			if c != (lease.Claim{SlotID: f.bound.SlotID, NodeID: f.bound.NodeID, ExecutionEpoch: f.bound.ExecutionEpoch, OwnerID: f.bound.LeaseOwnerID}) {
				t.Error("wrong lease claim")
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *ticketFixture) command(scope string) *executionv1.NodeControlServiceControlResponse {
	deadline := timestamppb.New(f.server.config.Now().Add(30 * time.Second))
	if scope == "credential_key" {
		return &executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_CredentialKeyCommand{CredentialKeyCommand: &executionv1.CredentialKeyCommand{
			CommandId: "diagnostic-command", SlotId: f.bound.SlotID, AccountId: f.bound.AccountID, ExecutionEpoch: 1,
			ImageDigest: f.bound.ImageDigest, Deadline: deadline, DesiredGeneration: 2,
		}}}
	}
	return &executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
		CommandId: "diagnostic-command", SlotId: f.bound.SlotID, AccountId: f.bound.AccountID, ExecutionEpoch: 1,
		ImageDigest: f.bound.ImageDigest, Deadline: deadline, Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT,
		Metadata: map[string]string{"desired_generation": "2"},
	}}}
}

func (f *ticketFixture) reserve(t *testing.T, response *executionv1.NodeControlServiceControlResponse, sending bool) {
	t.Helper()
	if !f.session.reserveCommand(controlCommandID(response), pendingFromResponse(response), f.server.config.Now()) {
		t.Fatal("reserve failed")
	}
	if sending && !f.session.prepareOutbound(response, f.server.config.Now()) {
		t.Fatal("sending authorization failed")
	}
}

func ticketRequest(scope string, round int) *executionv1.ControlProbeTicketRequest {
	return &executionv1.ControlProbeTicketRequest{CommandId: "diagnostic-command", Scope: scope, RequestId: fmt.Sprintf("%032x", round)}
}

func (f *ticketFixture) request(t *testing.T, request *executionv1.ControlProbeTicketRequest) *executionv1.ControlProbeTicketResponse {
	t.Helper()
	if err := f.server.recordProbeTicketRequest(context.Background(), "node-1", f.session, request); err != nil {
		t.Fatal(err)
	}
	select {
	case response := <-f.session.outbound:
		if !f.session.prepareOutbound(response, f.server.config.Now()) {
			t.Fatal("response not current at dequeue")
		}
		return response.GetProbeTicketResponse()
	default:
		t.Fatal("missing correlated response")
		return nil
	}
}

func requireProbeTicketDenied(t *testing.T, response *executionv1.ControlProbeTicketResponse) {
	t.Helper()
	if response == nil || response.GetErrorCode() != probeTicketUnavailable || response.GetExecutionTicket() != "" || response.GetExpiresAt() != nil {
		t.Fatalf("unsafe denial: %v", response)
	}
}

func TestProbeTicketsOnlyReadScopesAndSingleAttempt(t *testing.T) {
	for _, scope := range []string{"health", "credential_key"} {
		t.Run(scope, func(t *testing.T) {
			f := newTicketFixture(t)
			f.reserve(t, f.command(scope), true)
			request := ticketRequest(scope, 1)
			response := f.request(t, request)
			claims, err := f.verifier.Verify(response.GetExecutionTicket(), f.server.config.Now())
			if err != nil || response.GetErrorCode() != "" || response.GetRequestId() != request.GetRequestId() || claims.AccountID != provider.RuntimeAccountID(f.bound.AccountID) ||
				claims.SlotID != f.bound.SlotID || claims.NodeID != f.bound.NodeID || claims.Epoch != f.bound.ExecutionEpoch || len(claims.Scopes) != 1 || claims.Scopes[0] != scope ||
				claims.ExpiresAt != response.GetExpiresAt().AsTime().Unix() || claims.ExpiresAt > f.server.config.Now().Add(5*time.Second).Unix() {
				t.Fatal("wrong diagnostic claims", err)
			}
			if f.reads.Load() != 2 || f.leases.Load() != 2 {
				t.Fatal("missing double authority validation")
			}
			response = f.request(t, ticketRequest(scope, 2))
			requireProbeTicketDenied(t, response)
			if response.GetRequestId() != ticketRequest(scope, 2).GetRequestId() || f.reads.Load() != 2 {
				t.Fatal("duplicate request reissued or lost correlation")
			}
		})
	}
}

func TestProbeTicketsPreconditionsDoNotReadAuthority(t *testing.T) {
	for _, fault := range []string{"disabled", "no capability", "missing", "queued", "expired", "destroy", "wrong account", "no generation", "padded generation", "overflow generation", "key no generation", "wrong scope", "messages", "activate", "mixed scope"} {
		t.Run(fault, func(t *testing.T) {
			f := newTicketFixture(t)
			scope := "health"
			response := f.command(scope)
			sending := true
			switch fault {
			case "disabled":
				f.server.config.ProbeTickets = nil
			case "no capability":
				delete(f.session.capabilities, probeTicketsCapability)
			case "queued":
				sending = false
			case "expired":
				response.GetSlotCommand().Deadline = timestamppb.New(f.server.config.Now())
			case "destroy":
				response.GetSlotCommand().Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DESTROY
			case "wrong account":
				response.GetSlotCommand().AccountId = "another-account"
			case "no generation":
				response.GetSlotCommand().Metadata = nil
			case "padded generation":
				response.GetSlotCommand().Metadata["desired_generation"] = "02"
			case "overflow generation":
				response.GetSlotCommand().Metadata["desired_generation"] = "18446744073709551616"
			case "key no generation":
				scope = "credential_key"
				response = f.command(scope)
				response.GetCredentialKeyCommand().DesiredGeneration = 0
			case "wrong scope":
				scope = "credential_key"
			case "messages":
				scope = "messages"
			case "activate":
				scope = "secure_activate"
			case "mixed scope":
				scope = "health.messages"
			}
			if fault != "missing" {
				f.reserve(t, response, sending)
			}
			requireProbeTicketDenied(t, f.request(t, ticketRequest(scope, 1)))
			if fault != "wrong account" && (f.reads.Load() != 0 || f.leases.Load() != 0) {
				t.Fatal("precondition rejection reached authority")
			}
		})
	}
}

func TestProbeTicketsRejectMalformedRequestWithoutConsumingAttempt(t *testing.T) {
	for _, bad := range []string{"", "short", strings.Repeat("A", 32), strings.Repeat("g", 32), strings.Repeat("a", 32) + "\n"} {
		f := newTicketFixture(t)
		f.reserve(t, f.command("health"), true)
		r := ticketRequest("health", 1)
		r.RequestId = bad
		if err := f.server.recordProbeTicketRequest(context.Background(), "node-1", f.session, r); status.Code(err) != codes.InvalidArgument {
			t.Fatal("malformed request accepted", err)
		}
		if f.reads.Load() != 0 || len(f.session.outbound) != 0 {
			t.Fatal("malformed request changed state")
		}
		if result := f.request(t, ticketRequest("health", 2)); result.GetExecutionTicket() == "" {
			t.Fatal("malformed request consumed valid attempt")
		}
	}
}

func TestProbeTicketsRejectBindingChangesAndDependencies(t *testing.T) {
	for _, field := range []string{"account", "slot", "node", "session", "epoch", "generation", "image", "ref", "owner", "lease expiry", "stale node", "future node", "first read error", "second read error", "first lease", "second lease", "certificate revoked"} {
		t.Run(field, func(t *testing.T) {
			f := newTicketFixture(t)
			f.reserve(t, f.command("health"), true)
			reads := 0
			f.server.config.ProbeTickets.Repository = probeTicketReaderFunc(func(context.Context, string, time.Time, time.Duration) (store.ProbeBinding, error) {
				reads++
				b := f.bound
				if field == "first read error" || field == "second read error" && reads == 2 {
					return b, errors.New("private repository detail")
				}
				if reads == 2 {
					switch field {
					case "account":
						b.AccountID = "account-2"
					case "slot":
						b.SlotID += "x"
					case "node":
						b.NodeID += "x"
					case "session":
						b.ControlSessionID = strings.Repeat("b", 32)
					case "epoch":
						b.ExecutionEpoch++
					case "generation":
						b.RouteGeneration++
					case "image":
						b.ImageDigest = "sha256:" + strings.Repeat("b", 64)
					case "ref":
						b.ProviderRef += "x"
					case "owner":
						b.LeaseOwnerID += "x"
					case "lease expiry":
						b.LeaseExpiresAt = f.server.config.Now()
					case "stale node":
						b.NodeSeenAt = f.server.config.Now().Add(-45 * time.Second)
					case "future node":
						b.NodeSeenAt = f.server.config.Now().Add(time.Second)
					}
				}
				return b, nil
			})
			calls := 0
			f.server.config.ProbeTickets.Leases = probeTicketLeaseFunc(func(context.Context, lease.Claim) error {
				calls++
				if field == "first lease" || field == "second lease" && calls == 2 {
					return errors.New("private lease detail")
				}
				return nil
			})
			if field == "certificate revoked" {
				r := f.server.repository.(sessionRepository)
				r.validate = func(context.Context, string, string, time.Time) error { return store.ErrCertificateNotActive }
				f.server.repository = r
			}
			requireProbeTicketDenied(t, f.request(t, ticketRequest("health", 1)))
		})
	}
}

func TestProbeTicketsClampExpiryAndRejectSubsecondWindow(t *testing.T) {
	for _, bound := range []string{"command", "lease", "node", "certificate", "original lease", "original node", "subsecond"} {
		t.Run(bound, func(t *testing.T) {
			f := newTicketFixture(t)
			now := f.server.config.Now()
			ceiling := now.Add(2*time.Second + 500*time.Millisecond)
			response := f.command("health")
			switch bound {
			case "command":
				response.GetSlotCommand().Deadline = timestamppb.New(ceiling)
			case "lease":
				f.bound.LeaseExpiresAt = ceiling
			case "node":
				f.bound.NodeSeenAt = ceiling.Add(-45 * time.Second)
			case "certificate":
				remote, _ := peer.FromContext(f.session.context)
				info := remote.AuthInfo.(credentials.TLSInfo)
				info.State.PeerCertificates[0].NotAfter = ceiling
			case "original lease", "original node":
				reads := 0
				f.server.config.ProbeTickets.Repository = probeTicketReaderFunc(func(context.Context, string, time.Time, time.Duration) (store.ProbeBinding, error) {
					reads++
					b := f.bound
					if reads == 1 {
						if bound == "original lease" {
							b.LeaseExpiresAt = ceiling
						} else {
							b.NodeSeenAt = ceiling.Add(-45 * time.Second)
						}
					}
					return b, nil
				})
			case "subsecond":
				ceiling = now.Add(500 * time.Millisecond)
				f.bound.LeaseExpiresAt = ceiling
			}
			f.reserve(t, response, true)
			result := f.request(t, ticketRequest("health", 1))
			if bound == "subsecond" {
				requireProbeTicketDenied(t, result)
				return
			}
			if result.GetExecutionTicket() == "" || result.GetExpiresAt().AsTime().Unix() != ceiling.Unix() {
				t.Fatal("expiry cap was rounded up or ignored")
			}
		})
	}
}

func TestProbeTicketsConcurrentSingleAttemptAndABA(t *testing.T) {
	f := newTicketFixture(t)
	command := f.command("health")
	f.reserve(t, command, true)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.server.config.ProbeTickets.Repository = probeTicketReaderFunc(func(ctx context.Context, _ string, _ time.Time, _ time.Duration) (store.ProbeBinding, error) {
		once.Do(func() { close(entered) })
		select {
		case <-release:
			return f.bound, nil
		case <-ctx.Done():
			return store.ProbeBinding{}, ctx.Err()
		}
	})
	done := make(chan error, 1)
	go func() {
		done <- f.server.recordProbeTicketRequest(context.Background(), "node-1", f.session, ticketRequest("health", 1))
	}()
	<-entered
	var wg sync.WaitGroup
	for i := 2; i <= 17; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if err := f.server.recordProbeTicketRequest(context.Background(), "node-1", f.session, ticketRequest("health", n)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	// Same command ID is now a different outbound/pending instance. The old
	// attempt must not mint a ticket even when all binding values are identical.
	f.session.releaseCommand("diagnostic-command")
	replacement := proto.Clone(command).(*executionv1.NodeControlServiceControlResponse)
	f.reserve(t, replacement, true)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for len(f.session.outbound) > 0 {
		requireProbeTicketDenied(t, (<-f.session.outbound).GetProbeTicketResponse())
	}
	// A genuinely new command instance gets a fresh attempt; old request IDs
	// are not retained as an unbounded server history.
	if response := f.request(t, ticketRequest("health", 18)); response.GetExecutionTicket() == "" {
		t.Fatal("new command instance could not authorize")
	}
}

func TestProbeTicketsQueueDropsStaleOrCompletedProof(t *testing.T) {
	for _, fault := range []string{"expired", "released", "same ID replaced", "closed", "full"} {
		t.Run(fault, func(t *testing.T) {
			f := newTicketFixture(t)
			command := f.command("health")
			f.reserve(t, command, true)
			if fault == "full" {
				for i := 0; i < cap(f.session.outbound); i++ {
					f.session.outbound <- &executionv1.NodeControlServiceControlResponse{}
				}
			}
			if err := f.server.recordProbeTicketRequest(context.Background(), "node-1", f.session, ticketRequest("health", 1)); err != nil {
				t.Fatal(err)
			}
			if fault == "full" {
				if len(f.session.queuedTickets) != 0 {
					t.Fatal("full queue retained ticket marker")
				}
				return
			}
			response := <-f.session.outbound
			now := f.server.config.Now()
			switch fault {
			case "expired":
				now = response.GetProbeTicketResponse().GetExpiresAt().AsTime()
			case "released":
				f.session.releaseCommand("diagnostic-command")
			case "same ID replaced":
				f.session.releaseCommand("diagnostic-command")
				f.reserve(t, proto.Clone(command).(*executionv1.NodeControlServiceControlResponse), true)
			case "closed":
				close(f.session.done)
			}
			if f.session.prepareOutbound(response, now) || len(f.session.queuedTickets) != 0 {
				t.Fatal("stale ticket sent or marker retained")
			}
		})
	}
}

func TestProbeTicketConfigurationDefaultsAndRejectsInvalid(t *testing.T) {
	f := newTicketFixture(t)
	input := *f.server.config.ProbeTickets
	input.TTL, input.Timeout = 0, 0
	got, err := normalizeProbeTicketConfig(&input)
	if err != nil || got.TTL != 5*time.Second || got.Timeout != 2*time.Second {
		t.Fatal(got, err)
	}
	input.TTL = time.Hour
	if got.TTL != 5*time.Second {
		t.Fatal("configuration aliases caller")
	}
	if got, err := normalizeProbeTicketConfig(nil); err != nil || got != nil {
		t.Fatal("nil did not disable")
	}
	for _, edit := range []func(*ProbeTicketConfig){func(c *ProbeTicketConfig) { c.Repository = nil }, func(c *ProbeTicketConfig) { c.Leases = nil }, func(c *ProbeTicketConfig) { c.Issuer = nil }, func(c *ProbeTicketConfig) { c.TTL = -time.Second }, func(c *ProbeTicketConfig) { c.TTL = 10*time.Second + 1 }, func(c *ProbeTicketConfig) { c.Timeout = -time.Second }, func(c *ProbeTicketConfig) { c.Timeout = 5*time.Second + 1 }} {
		c := *f.server.config.ProbeTickets
		edit(&c)
		if got, err := normalizeProbeTicketConfig(&c); err == nil || got != nil {
			t.Fatal("invalid enabled config accepted")
		}
	}
}

func TestProbeTicketsDoNotRetainNonDiagnosticControlPayloads(t *testing.T) {
	f := newTicketFixture(t)
	for _, response := range []*executionv1.NodeControlServiceControlResponse{
		{Event: &executionv1.NodeControlServiceControlResponse_SecureActivationCommand{SecureActivationCommand: &executionv1.SecureActivationCommand{
			CommandId: "activate", SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: 1,
			ImageDigest: f.bound.ImageDigest, Deadline: timestamppb.New(f.server.config.Now().Add(time.Minute)),
			CredentialLeaseId: "credential-lease", ProxyLeaseId: "proxy-1", EncryptedCredentialBundle: make([]byte, maxCredentialBundleBytes),
		}}},
		{Event: &executionv1.NodeControlServiceControlResponse_RevokeEpoch{RevokeEpoch: &executionv1.RevokeEpochCommand{CommandId: "revoke", SlotId: "slot-1", ExecutionEpoch: 1}}},
		{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
			CommandId: "start", SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: 1,
			Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START, Deadline: timestamppb.New(f.server.config.Now().Add(time.Minute)),
		}}},
	} {
		pending := pendingFromResponse(response)
		if pending.outbound != nil || pending.ticketAttempt != nil || pending.permitsProbeScope("health") || pending.permitsProbeScope("credential_key") {
			t.Fatal("non-diagnostic command retained response or acquired diagnostic scope")
		}
		if !f.session.prepareOutbound(response, f.server.config.Now()) {
			t.Fatal("diagnostic marker changed legacy non-diagnostic dequeue semantics")
		}
	}
}

func TestProbeTicketsCancellationSessionReplacementAndTimeout(t *testing.T) {
	for _, fault := range []string{"parent cancelled", "command released", "session replaced", "metadata timeout", "lease timeout", "final clock cancelled"} {
		t.Run(fault, func(t *testing.T) {
			f := newTicketFixture(t)
			f.reserve(t, f.command("health"), true)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			reads, leases := 0, 0
			if strings.HasSuffix(fault, "timeout") {
				f.server.config.ProbeTickets.Timeout = time.Millisecond
			}
			f.server.config.ProbeTickets.Repository = probeTicketReaderFunc(func(ctx context.Context, _ string, _ time.Time, _ time.Duration) (store.ProbeBinding, error) {
				reads++
				switch fault {
				case "parent cancelled":
					cancel()
				case "command released":
					f.session.releaseCommand("diagnostic-command")
				case "session replaced":
					f.server.mu.Lock()
					f.server.sessions["node-1"] = &nodeSession{id: strings.Repeat("b", 32)}
					f.server.mu.Unlock()
				case "metadata timeout":
					<-ctx.Done()
				}
				return f.bound, nil
			})
			f.server.config.ProbeTickets.Leases = probeTicketLeaseFunc(func(ctx context.Context, _ lease.Claim) error {
				leases++
				if fault == "lease timeout" {
					<-ctx.Done()
				}
				return nil
			})
			if fault == "final clock cancelled" {
				now := f.server.config.Now()
				// Last certificate verification also uses the clock; cancelling
				// after both lease reads must never produce a bearer ticket.
				f.server.config.Now = func() time.Time {
					if reads == 2 && leases == 2 {
						cancel()
					}
					return now
				}
			}
			if err := f.server.recordProbeTicketRequest(parent, "node-1", f.session, ticketRequest("health", 1)); err != nil {
				t.Fatal(err)
			}
			for len(f.session.outbound) > 0 {
				requireProbeTicketDenied(t, (<-f.session.outbound).GetProbeTicketResponse())
			}
			if fault == "metadata timeout" || fault == "lease timeout" {
				pending, _ := f.session.command("diagnostic-command")
				if pending.ticketAttempt == nil {
					t.Fatal("failed authorization did not consume its bounded attempt")
				}
			}
		})
	}
}
