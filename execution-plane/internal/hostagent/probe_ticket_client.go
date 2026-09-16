package hostagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

const (
	controlProbeTicketsCapability = "probe_tickets"
	maxPendingProbeTickets        = 64
	maxProbeTicketWait            = 10 * time.Second
)

var ErrProbeTicketUnavailable = errors.New("probe ticket unavailable")

type probeCommandContextKey struct{}

// The source is installed only by the command worker. Even unsupported commands
// receive a marker when opted in, so runtime calls cannot fall back to a broader
// legacy TicketSource. Its lifetime ends before the command result is sent.
type probeCommandSource struct {
	broker    *probeTicketBroker
	ctx       context.Context
	commandID string
	scope     string
	identity  runtimeIdentity
	deadline  time.Time
	mu        sync.Mutex
	attempted bool
}

type probeTicketWaiter struct {
	source    *probeCommandSource
	ctx       context.Context
	requestID string
	sent      bool
	result    chan probeTicketResult
}

type probeTicketResult struct {
	token   string
	expires time.Time
}

type probeTicketBroker struct {
	ctx     context.Context
	cancel  context.CancelFunc
	now     func() time.Time
	queue   chan *probeTicketWaiter
	mu      sync.Mutex
	pending map[string]*probeTicketWaiter
}

func newProbeTicketBroker(ctx context.Context, now func() time.Time) *probeTicketBroker {
	live, cancel := context.WithCancel(ctx)
	return &probeTicketBroker{
		ctx: live, cancel: cancel, now: now, queue: make(chan *probeTicketWaiter, maxPendingProbeTickets),
		pending: make(map[string]*probeTicketWaiter),
	}
}

func (b *probeTicketBroker) close() {
	b.cancel()
	b.mu.Lock()
	clear(b.pending)
	b.mu.Unlock()
}

// RequestProbeTicket requests only the read-only scope authorized for the
// current control command. It cannot select a node, runtime endpoint or account.
// Returned tickets are bearer secrets and must not be logged or persisted.
func RequestProbeTicket(ctx context.Context, scope string) (string, error) {
	if ctx == nil {
		return "", ErrProbeTicketUnavailable
	}
	source, _ := ctx.Value(probeCommandContextKey{}).(*probeCommandSource)
	if source == nil {
		return "", ErrProbeTicketUnavailable
	}
	return source.request(ctx, scope)
}

func (s *probeCommandSource) request(ctx context.Context, scope string) (string, error) {
	if ctx == nil || ctx.Err() != nil || s == nil || s.broker == nil || s.ctx == nil || s.ctx.Err() != nil ||
		!validProbeScope(scope) || scope != s.scope || !s.deadline.After(s.broker.now()) || s.broker.ctx.Err() != nil {
		return "", ErrProbeTicketUnavailable
	}
	s.mu.Lock()
	if s.attempted {
		s.mu.Unlock()
		return "", ErrProbeTicketUnavailable
	}
	s.attempted = true
	s.mu.Unlock()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", ErrProbeTicketUnavailable
	}
	waitCtx, cancel := context.WithDeadline(ctx, s.deadline)
	defer cancel()
	waitCtx, cancelWait := context.WithTimeout(waitCtx, maxProbeTicketWait)
	defer cancelWait()
	w := &probeTicketWaiter{source: s, ctx: waitCtx, requestID: hex.EncodeToString(nonce[:]), result: make(chan probeTicketResult, 1)}
	b := s.broker
	b.mu.Lock()
	if len(b.pending) >= maxPendingProbeTickets || b.pending[w.requestID] != nil || !b.current(w) {
		b.mu.Unlock()
		return "", ErrProbeTicketUnavailable
	}
	b.pending[w.requestID] = w
	select {
	case b.queue <- w:
	default:
		delete(b.pending, w.requestID)
		b.mu.Unlock()
		return "", ErrProbeTicketUnavailable
	}
	b.mu.Unlock()
	defer b.remove(w)
	select {
	case result := <-w.result:
		if result.token == "" || !result.expires.After(b.now()) || !b.current(w) {
			return "", ErrProbeTicketUnavailable
		}
		return result.token, nil
	case <-waitCtx.Done():
	case <-s.ctx.Done():
	case <-b.ctx.Done():
	}
	return "", ErrProbeTicketUnavailable
}

func (b *probeTicketBroker) current(w *probeTicketWaiter) bool {
	current := w.source.deadline.After(b.now())
	return current && w.ctx.Err() == nil && w.source.ctx.Err() == nil && b.ctx.Err() == nil
}

func (b *probeTicketBroker) remove(w *probeTicketWaiter) {
	b.mu.Lock()
	if b.pending[w.requestID] == w {
		delete(b.pending, w.requestID)
	}
	b.mu.Unlock()
}

// outbound is called only by the existing control-session Send loop.
func (b *probeTicketBroker) outbound(w *probeTicketWaiter) *executionv1.NodeControlServiceControlRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending[w.requestID] != w || w.sent || !b.current(w) {
		return nil
	}
	w.sent = true
	return &executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_ProbeTicketRequest{
		ProbeTicketRequest: &executionv1.ControlProbeTicketRequest{CommandId: w.source.commandID, Scope: w.source.scope, RequestId: w.requestID},
	}}
}

func (b *probeTicketBroker) deliver(response *executionv1.ControlProbeTicketResponse) error {
	claims, expires, err := parseProbeTicketResponse(response)
	if err != nil {
		return ErrProbeTicketUnavailable
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.pending[response.GetRequestId()]
	if w == nil {
		// A syntactically valid late reply cannot satisfy a new same-command
		// waiter: request_id is fresh per attempt, and no history is retained.
		return nil
	}
	if !w.sent || response.GetCommandId() != w.source.commandID || response.GetScope() != w.source.scope {
		return ErrProbeTicketUnavailable
	}
	if !b.current(w) {
		delete(b.pending, response.GetRequestId())
		return nil
	}
	if response.GetErrorCode() == "" {
		i := w.source.identity
		now := b.now()
		if claims.AccountID != i.AccountID || claims.SlotID != i.SlotID || claims.NodeID != i.NodeID || claims.Epoch != i.Epoch ||
			claims.IssuedAt > now.Unix() || !expires.After(now) || expires.After(now.Add(maxProbeTicketWait)) || expires.After(w.source.deadline) {
			return ErrProbeTicketUnavailable
		}
	}
	delete(b.pending, response.GetRequestId())
	if b.current(w) {
		w.result <- probeTicketResult{token: response.GetExecutionTicket(), expires: expires}
	}
	return nil
}

func (b *probeTicketBroker) commandContext(parent context.Context, nodeID string, command controlCommandEnvelope) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	source := &probeCommandSource{broker: b, ctx: ctx}
	var account, slotID, image string
	var epoch, generation uint64
	if slot := command.slot; slot != nil && slot.GetAction() == executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT {
		source.commandID, source.scope = slot.GetCommandId(), "health"
		account, slotID, image, epoch = slot.GetAccountId(), slot.GetSlotId(), slot.GetImageDigest(), slot.GetExecutionEpoch()
		raw := slot.GetMetadata()["desired_generation"]
		generation, _ = strconv.ParseUint(raw, 10, 64)
		if strconv.FormatUint(generation, 10) != raw {
			generation = 0
		}
		if deadline := slot.GetDeadline(); deadline != nil && deadline.CheckValid() == nil {
			source.deadline = deadline.AsTime()
		}
	} else if key := command.key; key != nil {
		source.commandID, source.scope = key.GetCommandId(), "credential_key"
		account, slotID, image, epoch, generation = key.GetAccountId(), key.GetSlotId(), key.GetImageDigest(), key.GetExecutionEpoch(), key.GetDesiredGeneration()
		if deadline := key.GetDeadline(); deadline != nil && deadline.CheckValid() == nil {
			source.deadline = deadline.AsTime()
		}
	}
	if credential.ValidateTransportID(source.commandID) != nil || credential.ValidateTransportID(account) != nil ||
		credential.ValidateTransportID(slotID) != nil || !hostNodeIDPattern.MatchString(nodeID) || epoch == 0 || generation == 0 ||
		!commandImageDigestPattern.MatchString(image) || source.deadline.IsZero() {
		source.scope = ""
	}
	source.identity = runtimeIdentity{AccountID: provider.RuntimeAccountID(account), SlotID: slotID, NodeID: nodeID, Epoch: epoch}
	return context.WithValue(ctx, probeCommandContextKey{}, source), cancel
}
