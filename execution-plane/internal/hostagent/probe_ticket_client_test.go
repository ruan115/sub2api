package hostagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type probeCallResult struct {
	token string
	err   error
}

func probeCommand(id, scope string, now time.Time) controlCommandEnvelope {
	if scope == "credential_key" {
		return controlCommandEnvelope{key: &executionv1.CredentialKeyCommand{
			CommandId: id, AccountId: "account-probe", SlotId: "slot-probe", ExecutionEpoch: 4,
			ImageDigest: "sha256:" + strings.Repeat("a", 64), Deadline: timestamppb.New(now.Add(10 * time.Second)), DesiredGeneration: 2,
		}}
	}
	return controlCommandEnvelope{slot: &executionv1.SlotCommand{
		CommandId: id, Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT,
		AccountId: "account-probe", SlotId: "slot-probe", ExecutionEpoch: 4,
		ImageDigest: "sha256:" + strings.Repeat("a", 64), Deadline: timestamppb.New(now.Add(10 * time.Second)),
		Metadata: map[string]string{"desired_generation": "2"},
	}}
}

func newTestProbeBroker(t *testing.T, now time.Time) *probeTicketBroker {
	t.Helper()
	b := newProbeTicketBroker(context.Background(), func() time.Time { return now })
	t.Cleanup(b.close)
	return b
}

func beginProbeCall(ctx context.Context, scope string) <-chan probeCallResult {
	done := make(chan probeCallResult, 1)
	go func() { raw, err := RequestProbeTicket(ctx, scope); done <- probeCallResult{token: raw, err: err} }()
	return done
}

func awaitProbeOutbound(t *testing.T, b *probeTicketBroker) *executionv1.ControlProbeTicketRequest {
	t.Helper()
	select {
	case w := <-b.queue:
		out := b.outbound(w)
		if out == nil {
			t.Fatal("live probe request was not emitted")
		}
		return out.GetProbeTicketRequest()
	case <-time.After(time.Second):
		t.Fatal("probe request was not queued")
		return nil
	}
}

func awaitProbeCall(t *testing.T, done <-chan probeCallResult, success bool) {
	t.Helper()
	select {
	case result := <-done:
		if success {
			if result.err != nil || result.token == "" {
				t.Fatal("expected a successful probe ticket")
			}
		} else if result.token != "" || !errors.Is(result.err, ErrProbeTicketUnavailable) {
			t.Fatal("probe ticket was not denied with a fixed error")
		}
	case <-time.After(time.Second):
		t.Fatal("probe request did not finish")
	}
}

func probeResponse(t *testing.T, request *executionv1.ControlProbeTicketRequest, now time.Time) *executionv1.ControlProbeTicketResponse {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, _ := ticket.NewIssuer(key)
	claims, err := ticket.NewClaims(provider.RuntimeAccountID("account-probe"), "slot-probe", "node-probe", 4, []string{request.GetScope()}, now, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := issuer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	return &executionv1.ControlProbeTicketResponse{
		CommandId: request.GetCommandId(), Scope: request.GetScope(), RequestId: request.GetRequestId(), ExecutionTicket: raw,
		ExpiresAt: timestamppb.New(time.Unix(claims.ExpiresAt, 0)),
	}
}

func TestProbeTicketBrokerSuccessAndSingleAttempt(t *testing.T) {
	for _, scope := range []string{"health", "credential_key"} {
		t.Run(scope, func(t *testing.T) {
			now := time.Now().UTC()
			b := newTestProbeBroker(t, now)
			ctx, cancel := b.commandContext(b.ctx, "node-probe", probeCommand("command-probe", scope, now))
			defer cancel()
			done := beginProbeCall(ctx, scope)
			request := awaitProbeOutbound(t, b)
			if !probeRequestIDPattern.MatchString(request.GetRequestId()) {
				t.Fatal("missing canonical random correlation")
			}
			awaitProbeCall(t, beginProbeCall(ctx, scope), false)
			if err := b.deliver(probeResponse(t, request, now)); err != nil {
				t.Fatal(err)
			}
			awaitProbeCall(t, done, true)
			awaitProbeCall(t, beginProbeCall(ctx, scope), false)
			if len(b.pending) != 0 || len(b.queue) != 0 {
				t.Fatal("completed request retained broker entries")
			}
		})
	}
}

func TestProbeTicketBrokerCancellationLateReplyAndSameCommandABA(t *testing.T) {
	now := time.Now().UTC()
	b := newTestProbeBroker(t, now)
	command := probeCommand("same-command", "health", now)
	ctx, cancel := b.commandContext(b.ctx, "node-probe", command)
	done := beginProbeCall(ctx, "health")
	oldRequest := awaitProbeOutbound(t, b)
	cancel()
	awaitProbeCall(t, done, false)
	newCtx, cancelNew := b.commandContext(b.ctx, "node-probe", command)
	defer cancelNew()
	newDone := beginProbeCall(newCtx, "health")
	newRequest := awaitProbeOutbound(t, b)
	if oldRequest.GetRequestId() == newRequest.GetRequestId() {
		t.Fatal("correlation reused for a different command attempt")
	}
	// The old ticket may already be expired when its reply finally arrives.
	if err := b.deliver(probeResponse(t, oldRequest, now.Add(-10*time.Second))); err != nil {
		t.Fatal("valid late response was not discarded")
	}
	select {
	case <-newDone:
		t.Fatal("old reply completed the new command instance")
	default:
	}
	if err := b.deliver(probeResponse(t, newRequest, now)); err != nil {
		t.Fatal(err)
	}
	awaitProbeCall(t, newDone, true)
	// Retained context values cannot resurrect a completed command.
	cancelNew()
	awaitProbeCall(t, beginProbeCall(context.WithoutCancel(newCtx), "health"), false)
}

func TestProbeTicketBrokerDisconnectAndDeadlineReleaseWaiters(t *testing.T) {
	for _, reason := range []string{"disconnect", "deadline", "caller_cancel", "queued_cancel"} {
		t.Run(reason, func(t *testing.T) {
			now := time.Now().UTC()
			b := newTestProbeBroker(t, now)
			command := probeCommand("command-probe", "health", now)
			if reason == "deadline" {
				command.slot.Deadline = timestamppb.New(now.Add(30 * time.Millisecond))
			}
			commandCtx, cancelCommand := b.commandContext(b.ctx, "node-probe", command)
			defer cancelCommand()
			ctx, cancel := context.WithCancel(commandCtx)
			defer cancel()
			done := beginProbeCall(ctx, "health")
			if reason == "queued_cancel" {
				select {
				case w := <-b.queue:
					cancel()
					awaitProbeCall(t, done, false)
					if b.outbound(w) != nil {
						t.Fatal("cancelled queued request was sent")
					}
				case <-time.After(time.Second):
					t.Fatal("request not queued")
				}
				return
			}
			awaitProbeOutbound(t, b)
			switch reason {
			case "disconnect":
				b.close()
			case "caller_cancel":
				cancel()
			}
			awaitProbeCall(t, done, false)
			if len(b.pending) != 0 {
				t.Fatal("aborted request retained a waiter")
			}
		})
	}
}

func TestProbeTicketBrokerCapacityAndQueueAreBounded(t *testing.T) {
	now := time.Now().UTC()
	b := newTestProbeBroker(t, now)
	dones := make([]<-chan probeCallResult, 0, maxPendingProbeTickets)
	for index := range maxPendingProbeTickets {
		ctx, cancel := b.commandContext(b.ctx, "node-probe", probeCommand(fmt.Sprintf("cmd-%d", index), "health", now))
		defer cancel()
		dones = append(dones, beginProbeCall(ctx, "health"))
		awaitProbeOutbound(t, b)
	}
	ctx, cancel := b.commandContext(b.ctx, "node-probe", probeCommand("over-capacity", "health", now))
	defer cancel()
	awaitProbeCall(t, beginProbeCall(ctx, "health"), false)
	if len(b.pending) != maxPendingProbeTickets {
		t.Fatal("pending capacity was not enforced")
	}
	b.close()
	for _, done := range dones {
		awaitProbeCall(t, done, false)
	}
	b = newTestProbeBroker(t, now)
	for range maxPendingProbeTickets {
		b.queue <- &probeTicketWaiter{}
	}
	ctx, cancel = b.commandContext(b.ctx, "node-probe", probeCommand("queue-full", "health", now))
	defer cancel()
	awaitProbeCall(t, beginProbeCall(ctx, "health"), false)
	if len(b.pending) != 0 || len(b.queue) != maxPendingProbeTickets {
		t.Fatal("full queue retained a waiter or exceeded its bound")
	}
}

func TestProbeTicketContextRejectsUnsupportedCommandsAndGeneration(t *testing.T) {
	now := time.Now().UTC()
	b := newTestProbeBroker(t, now)
	for _, ctx := range []context.Context{nil, context.Background()} {
		awaitProbeCall(t, beginProbeCall(ctx, "health"), false)
	}
	for _, mutate := range []func(*controlCommandEnvelope){
		func(c *controlCommandEnvelope) {
			c.slot.Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START
		},
		func(c *controlCommandEnvelope) { c.slot.Metadata = nil },
		func(c *controlCommandEnvelope) { c.slot.Metadata["desired_generation"] = "+2" },
		func(c *controlCommandEnvelope) { c.slot.Metadata["desired_generation"] = "02" },
		func(c *controlCommandEnvelope) { c.slot.Metadata["desired_generation"] = "0" },
		func(c *controlCommandEnvelope) { c.slot.Metadata["desired_generation"] = "18446744073709551616" },
		func(c *controlCommandEnvelope) { c.slot.Deadline = nil },
		func(c *controlCommandEnvelope) { c.slot.ExecutionEpoch = 0 },
		func(c *controlCommandEnvelope) { c.slot.AccountId = "bad\naccount" },
		func(c *controlCommandEnvelope) { c.slot.ImageDigest = "latest" },
	} {
		command := probeCommand("command-probe", "health", now)
		mutate(&command)
		ctx, cancel := b.commandContext(b.ctx, "node-probe", command)
		awaitProbeCall(t, beginProbeCall(ctx, "health"), false)
		cancel()
	}
	key := probeCommand("command-key", "credential_key", now)
	key.key.DesiredGeneration = 0
	ctx, cancel := b.commandContext(b.ctx, "node-probe", key)
	defer cancel()
	awaitProbeCall(t, beginProbeCall(ctx, "credential_key"), false)
	ctx, cancel = b.commandContext(b.ctx, "node-probe", probeCommand("command-probe", "health", now))
	defer cancel()
	for _, scope := range []string{"credential_key", "activate", "secure_activate", "messages", "count_tokens", "health messages", ""} {
		awaitProbeCall(t, beginProbeCall(ctx, scope), false)
	}
	if len(b.queue) != 0 || len(b.pending) != 0 {
		t.Fatal("denied scope or command reached the wire")
	}
}

type probeFallbackSource struct{ calls atomic.Int32 }

func (s *probeFallbackSource) Issue(context.Context, TicketRequest) (string, error) {
	s.calls.Add(1)
	return "legacy-fixture", nil
}

func TestRuntimeProbeSourceBindsIdentityAndNeverFallsBack(t *testing.T) {
	now := time.Now().UTC()
	b := newTestProbeBroker(t, now)
	ctx, cancel := b.commandContext(b.ctx, "node-probe", probeCommand("command-probe", "health", now))
	defer cancel()
	fallback := &probeFallbackSource{}
	identity := runtimeIdentity{AccountID: provider.RuntimeAccountID("account-probe"), SlotID: "slot-probe", NodeID: "node-probe", Epoch: 4}
	for _, mutate := range []func(*runtimeIdentity){
		func(i *runtimeIdentity) { i.AccountID = "account-probe" }, func(i *runtimeIdentity) { i.SlotID = "other-slot" },
		func(i *runtimeIdentity) { i.NodeID = "other-node" }, func(i *runtimeIdentity) { i.Epoch++ },
	} {
		wrong := identity
		mutate(&wrong)
		r := &Runtime{identity: wrong, ticketSource: fallback}
		if raw, err := r.issue(ctx, "health"); raw != "" || !errors.Is(err, ErrProbeTicketUnavailable) {
			t.Fatal("cross-runtime command context accepted")
		}
	}
	r := &Runtime{identity: identity, ticketSource: fallback}
	for _, scope := range []string{"messages", "count_tokens", "activate", "secure_activate", "credential_key"} {
		if raw, err := r.issue(ctx, scope); raw != "" || !errors.Is(err, ErrProbeTicketUnavailable) {
			t.Fatal("diagnostic command obtained another scope")
		}
	}
	done := make(chan probeCallResult, 1)
	go func() { raw, err := r.issue(ctx, "health"); done <- probeCallResult{raw, err} }()
	if err := b.deliver(probeResponse(t, awaitProbeOutbound(t, b), now)); err != nil {
		t.Fatal(err)
	}
	awaitProbeCall(t, done, true)
	if fallback.calls.Load() != 0 {
		t.Fatal("command-scoped ticket failure fell back to legacy source")
	}
	if raw, err := r.issue(context.Background(), "messages"); raw != "legacy-fixture" || err != nil || fallback.calls.Load() != 1 {
		t.Fatal("non-opted-in legacy behavior changed")
	}
}

func TestProbeTicketResponseRejectsContradictionsAndWrongCorrelation(t *testing.T) {
	now := time.Now().UTC()
	b := newTestProbeBroker(t, now)
	ctx, cancel := b.commandContext(b.ctx, "node-probe", probeCommand("command-probe", "health", now))
	defer cancel()
	done := beginProbeCall(ctx, "health")
	request := awaitProbeOutbound(t, b)
	valid := probeResponse(t, request, now)
	for _, mutate := range []func(*executionv1.ControlProbeTicketResponse){
		func(r *executionv1.ControlProbeTicketResponse) { r.CommandId = "other-command" },
		func(r *executionv1.ControlProbeTicketResponse) { r.Scope = "credential_key" },
		func(r *executionv1.ControlProbeTicketResponse) { r.RequestId = "bad-request-id" },
		func(r *executionv1.ControlProbeTicketResponse) { r.ErrorCode = "probe_ticket_unavailable" },
		func(r *executionv1.ControlProbeTicketResponse) {
			r.ErrorCode = "arbitrary-error"
			r.ExecutionTicket = ""
			r.ExpiresAt = nil
		},
		func(r *executionv1.ControlProbeTicketResponse) { r.ExecutionTicket = strings.Repeat("x", 4097) },
		func(r *executionv1.ControlProbeTicketResponse) { r.ExecutionTicket = "not.a.ticket" },
		func(r *executionv1.ControlProbeTicketResponse) { r.ExpiresAt = nil },
		func(r *executionv1.ControlProbeTicketResponse) { r.ExpiresAt.Nanos = 1 },
		func(r *executionv1.ControlProbeTicketResponse) { r.ExpiresAt.Seconds++ },
	} {
		response := proto.Clone(valid).(*executionv1.ControlProbeTicketResponse)
		mutate(response)
		if !errors.Is(b.deliver(response), ErrProbeTicketUnavailable) {
			t.Fatal("invalid response was accepted")
		}
	}
	denied := &executionv1.ControlProbeTicketResponse{CommandId: request.GetCommandId(), Scope: request.GetScope(), RequestId: request.GetRequestId(), ErrorCode: "probe_ticket_unavailable"}
	if err := b.deliver(denied); err != nil {
		t.Fatal("fixed denial was not accepted")
	}
	awaitProbeCall(t, done, false)
}

func TestProbeTicketResponseChecksIdentityExpiryAndClaimShape(t *testing.T) {
	now := time.Now().UTC()
	b := newTestProbeBroker(t, now)
	ctx, cancel := b.commandContext(b.ctx, "node-probe", probeCommand("command-probe", "health", now))
	defer cancel()
	done := beginProbeCall(ctx, "health")
	request := awaitProbeOutbound(t, b)
	valid := probeResponse(t, request, now)
	for _, mutate := range []func(*ticket.Claims){
		func(c *ticket.Claims) { c.AccountID = provider.RuntimeAccountID("other-account") },
		func(c *ticket.Claims) { c.SlotID = "other-slot" },
		func(c *ticket.Claims) { c.NodeID = "other-node" },
		func(c *ticket.Claims) { c.Epoch++ },
		func(c *ticket.Claims) { c.Nonce = "noncanonical-nonce" },
		func(c *ticket.Claims) { c.Scopes = []string{"health", "messages"} },
		func(c *ticket.Claims) { c.Version++ },
		func(c *ticket.Claims) { c.ExpiresAt = c.IssuedAt + 11 },
		func(c *ticket.Claims) { c.IssuedAt += 20; c.ExpiresAt += 20 },
		func(c *ticket.Claims) { c.IssuedAt -= 20; c.ExpiresAt -= 20 },
	} {
		r := proto.Clone(valid).(*executionv1.ControlProbeTicketResponse)
		parts := strings.Split(r.ExecutionTicket, ".")
		payload, _ := base64.RawURLEncoding.DecodeString(parts[0])
		var claims ticket.Claims
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		mutate(&claims)
		payload, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		// Shape/binding validation must reject these before the worker's real
		// signature verification; this test does not assert token authenticity.
		r.ExecutionTicket = base64.RawURLEncoding.EncodeToString(payload) + "." + parts[1]
		r.ExpiresAt = timestamppb.New(time.Unix(claims.ExpiresAt, 0))
		if !errors.Is(b.deliver(r), ErrProbeTicketUnavailable) {
			t.Fatal("invalid ticket metadata was accepted")
		}
	}
	if err := b.deliver(valid); err != nil {
		t.Fatal(err)
	}
	awaitProbeCall(t, done, true)
}

func TestProbeTicketResponseBeforeSendAndCancelledDuringFinalClockRead(t *testing.T) {
	for _, edge := range []string{"before-send", "final-cancel"} {
		t.Run(edge, func(t *testing.T) {
			now := time.Now().UTC()
			b := newTestProbeBroker(t, now)
			ctx, cancel := b.commandContext(b.ctx, "node-probe", probeCommand("command-probe", "health", now))
			defer cancel()
			done := beginProbeCall(ctx, "health")
			var w *probeTicketWaiter
			select {
			case w = <-b.queue:
			case <-time.After(time.Second):
				t.Fatal("request not queued")
			}
			request := &executionv1.ControlProbeTicketRequest{CommandId: "command-probe", Scope: "health", RequestId: w.requestID}
			response := probeResponse(t, request, now)
			if edge == "before-send" {
				if !errors.Is(b.deliver(response), ErrProbeTicketUnavailable) {
					t.Fatal("unissued request accepted a ticket")
				}
				cancel()
			} else {
				b.outbound(w)
				// Only deliver consults this hook while the waiter is blocked.
				calls := 0
				b.now = func() time.Time {
					calls++
					if calls == 3 {
						cancel()
					}
					return now
				}
				if err := b.deliver(response); err != nil || calls != 3 {
					t.Fatal("final cancellation boundary was not exercised")
				}
			}
			awaitProbeCall(t, done, false)
		})
	}
}

func TestControlClientProbeTicketCapabilityRequiresExplicitOptIn(t *testing.T) {
	now := time.Now().UTC()
	base := ControlClientConfig{
		Client: executionv1.NewNodeControlServiceClient(nil), Executor: newTestSlotCommandExecutor(t, providerfake.New(), now), NodeID: "node-probe",
		Capacity:          &executionv1.Capacity{MaxSlots: 1, MaxActiveCli: 1, MaxActiveApi: 1, MaxActiveTotal: 1, AllocatableCpuMillis: 1, AllocatableMemoryBytes: 1},
		HeartbeatInterval: time.Second, ReconnectMin: time.Second, ReconnectMax: time.Second, MaxConcurrentCommands: 1, CommandQueue: 1,
	}
	for _, enabled := range []bool{false, true} {
		for _, advertised := range []bool{false, true} {
			config := base
			config.EnableProbeTickets = enabled
			if advertised {
				config.Capabilities = []string{controlProbeTicketsCapability}
			}
			client, err := NewControlClient(config)
			if enabled != advertised {
				if err == nil || client != nil {
					t.Fatal("mismatched diagnostic capability accepted")
				}
			} else if err != nil || client.enableProbeTickets != enabled {
				t.Fatal("matching diagnostic capability rejected")
			}
		}
	}
}
