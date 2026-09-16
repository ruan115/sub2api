package control

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
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

const probeTicketsCapability = "probe_tickets"
const probeTicketUnavailable = "probe_ticket_unavailable"
const probeTicketMaxNodeAge = 45 * time.Second

var probeTicketRequestIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type ProbeTicketRepository interface {
	ReadProbeBinding(context.Context, string, time.Time, time.Duration) (store.ProbeBinding, error)
}

// Validate proves current ownership only, not future Redis PTTL or immediate
// revocation of an already issued diagnostic ticket.
type ProbeTicketLeases interface {
	Validate(context.Context, lease.Claim) error
}

// Nil disables diagnostic issuance. Dependencies must be explicitly injected;
// this config exposes no credential read, provisioning or lease-write method.
type ProbeTicketConfig struct {
	Repository ProbeTicketRepository
	Leases     ProbeTicketLeases
	Issuer     *ticket.Issuer
	TTL        time.Duration
	Timeout    time.Duration
}

func normalizeProbeTicketConfig(input *ProbeTicketConfig) (*ProbeTicketConfig, error) {
	if input == nil {
		return nil, nil
	}
	config := *input
	if config.TTL == 0 {
		config.TTL = 5 * time.Second
	}
	if config.Timeout == 0 {
		config.Timeout = 2 * time.Second
	}
	if config.Repository == nil || config.Leases == nil || config.Issuer == nil ||
		config.TTL <= 0 || config.TTL > 10*time.Second || config.Timeout <= 0 || config.Timeout > 5*time.Second {
		return nil, errors.New("probe ticket configuration is invalid")
	}
	return &config, nil
}

// At most one attempt belongs to each eligible command instance. Keeping its
// outbound identity prevents release/reuse of a command ID from reviving it.
type probeTicketAttempt struct {
	commandID string
	requestID string
	scope     string
	outbound  *executionv1.NodeControlServiceControlResponse
	cancel    context.CancelFunc
}

func (p pendingCommand) cancelTicket() {
	if p.ticketAttempt != nil {
		p.ticketAttempt.cancel()
	}
}

func probeTicketGeneration(raw string) uint64 {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != raw {
		return 0
	}
	return value
}

func (p pendingCommand) permitsProbeScope(scope string) bool {
	return scope == "health" && p.kind == pendingSlotCommand && p.action == executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT ||
		scope == "credential_key" && p.kind == pendingCredentialKey
}

func pendingProbeTicketLive(p pendingCommand, now time.Time) bool {
	return p.outbound != nil && p.sending && p.deadline.After(now) && p.desiredGeneration > 0 &&
		credential.ValidateTransportID(p.slotID) == nil && credential.ValidateTransportID(p.accountBinding) == nil &&
		p.executionEpoch > 0 && imageDigestPattern.MatchString(p.imageDigest) && (p.probe == nil || p.probe.ctx.Err() == nil)
}

func (s *Server) recordProbeTicketRequest(ctx context.Context, nodeID string, session *nodeSession, request *executionv1.ControlProbeTicketRequest) error {
	if request == nil || credential.ValidateTransportID(request.GetCommandId()) != nil ||
		!probeTicketRequestIDPattern.MatchString(request.GetRequestId()) ||
		credential.ValidateTransportID(request.GetScope()) != nil || len(request.GetScope()) > 32 || proto.Size(request) > 256 {
		return status.Error(codes.InvalidArgument, "probe ticket request is invalid")
	}
	response := &executionv1.ControlProbeTicketResponse{CommandId: request.GetCommandId(), Scope: request.GetScope(), RequestId: request.GetRequestId(), ErrorCode: probeTicketUnavailable}
	var attempt *probeTicketAttempt
	if s.config.ProbeTickets != nil && session != nil {
		if _, enabled := session.capabilities[probeTicketsCapability]; enabled {
			attempt, response = s.authorizeProbeTicket(ctx, nodeID, session, request, response)
		}
	}
	// Denials are ordinary correlated responses, not a control-stream failure.
	// A saturated queue is dropped rather than blocking heartbeats/control work.
	s.enqueueProbeTicket(ctx, nodeID, session, attempt, response)
	return nil
}

func (s *Server) authorizeProbeTicket(parent context.Context, nodeID string, session *nodeSession, request *executionv1.ControlProbeTicketRequest, denied *executionv1.ControlProbeTicketResponse) (*probeTicketAttempt, *executionv1.ControlProbeTicketResponse) {
	config := s.config.ProbeTickets
	if parent == nil || parent.Err() != nil {
		return nil, denied
	}
	started := s.config.Now().UTC()
	s.mu.RLock()
	if s.sessions[nodeID] != session || !liveSession(session, session.id) {
		s.mu.RUnlock()
		return nil, denied
	}
	session.commandMu.Lock()
	session.reapProbesLocked(started)
	pending, exists := session.pendingCommands[request.GetCommandId()]
	if !exists || !pending.permitsProbeScope(request.GetScope()) || !pendingProbeTicketLive(pending, started) || pending.ticketAttempt != nil {
		session.commandMu.Unlock()
		s.mu.RUnlock()
		return nil, denied
	}
	ctx, cancel := context.WithDeadline(session.context, minProbeTicketTime(started.Add(config.Timeout), pending.deadline))
	stop := context.AfterFunc(parent, cancel)
	attempt := &probeTicketAttempt{commandID: request.GetCommandId(), requestID: request.GetRequestId(), scope: request.GetScope(), outbound: pending.outbound, cancel: cancel}
	pending.ticketAttempt = attempt
	session.pendingCommands[attempt.commandID] = pending
	session.commandMu.Unlock()
	s.mu.RUnlock()
	defer func() { stop(); cancel() }()
	if ctx.Err() != nil || s.ValidateControlSession(ctx, nodeID, session.id) != nil {
		return attempt, denied
	}
	certificateExpiry := probeTicketCertificateExpiry(session)
	before, err := config.Repository.ReadProbeBinding(ctx, pending.slotID, s.config.Now().UTC(), probeTicketMaxNodeAge)
	if err != nil || ctx.Err() != nil || !probeTicketBindingMatches(before, pending, nodeID, session.id, s.config.Now().UTC()) {
		return attempt, denied
	}
	claim := lease.Claim{SlotID: before.SlotID, NodeID: before.NodeID, ExecutionEpoch: before.ExecutionEpoch, OwnerID: before.LeaseOwnerID}
	if config.Leases.Validate(ctx, claim) != nil || ctx.Err() != nil {
		return attempt, denied
	}
	after, err := config.Repository.ReadProbeBinding(ctx, pending.slotID, s.config.Now().UTC(), probeTicketMaxNodeAge)
	if err != nil || ctx.Err() != nil || !sameProbeTicketBinding(before, after) ||
		!probeTicketBindingMatches(after, pending, nodeID, session.id, s.config.Now().UTC()) {
		return attempt, denied
	}
	if config.Leases.Validate(ctx, claim) != nil || ctx.Err() != nil ||
		s.ValidateControlSession(ctx, nodeID, session.id) != nil || ctx.Err() != nil {
		return attempt, denied
	}
	// Freeze both original and re-read ceilings; a refresh cannot extend this
	// attempt's lifetime. Unix seconds round the final cap down, never up.
	ceiling := minProbeTicketTime(started.Add(config.TTL), pending.deadline, certificateExpiry,
		before.LeaseExpiresAt, after.LeaseExpiresAt, before.NodeSeenAt.Add(probeTicketMaxNodeAge), after.NodeSeenAt.Add(probeTicketMaxNodeAge))
	s.mu.RLock()
	defer s.mu.RUnlock()
	session.commandMu.Lock()
	defer session.commandMu.Unlock()
	now := s.config.Now().UTC()
	if s.sessions[nodeID] != session || !liveSession(session, session.id) || !session.probeTicketAttemptLiveLocked(attempt, now) ||
		ctx.Err() != nil || parent.Err() != nil || now.Before(started) || !started.Add(config.Timeout).After(now) || ceiling.Unix() <= now.Unix() ||
		!probeTicketBindingMatches(before, pending, nodeID, session.id, now) || !probeTicketBindingMatches(after, pending, nodeID, session.id, now) {
		return attempt, denied
	}
	claims, err := ticket.NewClaims(pending.accountBinding, pending.slotID, nodeID, pending.executionEpoch, []string{attempt.scope}, now, ceiling.Sub(now))
	if err != nil || ctx.Err() != nil || parent.Err() != nil {
		return attempt, denied
	}
	raw, err := config.Issuer.Sign(claims)
	if err != nil || ctx.Err() != nil || parent.Err() != nil {
		return attempt, denied
	}
	return attempt, &executionv1.ControlProbeTicketResponse{CommandId: attempt.commandID, Scope: attempt.scope,
		RequestId: attempt.requestID, ExecutionTicket: raw, ExpiresAt: timestamppb.New(time.Unix(claims.ExpiresAt, 0).UTC())}
}

func (s *Server) enqueueProbeTicket(ctx context.Context, nodeID string, session *nodeSession, attempt *probeTicketAttempt, ticketResponse *executionv1.ControlProbeTicketResponse) {
	if session == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	response := &executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_ProbeTicketResponse{ProbeTicketResponse: ticketResponse}}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.sessions[nodeID] != session || !liveSession(session, session.id) {
		return
	}
	session.commandMu.Lock()
	defer session.commandMu.Unlock()
	now := s.config.Now().UTC()
	if ctx.Err() != nil || len(session.outbound) >= cap(session.outbound) || len(session.queuedTickets) >= cap(session.outbound) {
		return
	}
	if ticketResponse.GetExecutionTicket() != "" && (!session.probeTicketAttemptLiveLocked(attempt, now) ||
		ticketResponse.GetExpiresAt() == nil || !ticketResponse.GetExpiresAt().AsTime().After(now)) {
		return
	}
	if session.queuedTickets == nil {
		session.queuedTickets = make(map[*executionv1.NodeControlServiceControlResponse]*probeTicketAttempt)
	}
	session.queuedTickets[response] = attempt
	select {
	case session.outbound <- response:
	default:
		delete(session.queuedTickets, response)
	}
}

// Caller holds commandMu. Completed/reused commands and expired tickets are
// removed at the bounded outbound dequeue, without retaining tombstones.
func (s *nodeSession) prepareProbeTicketLocked(response *executionv1.NodeControlServiceControlResponse, now time.Time) bool {
	attempt, tracked := s.queuedTickets[response]
	delete(s.queuedTickets, response)
	if !tracked || s.context == nil || s.context.Err() != nil {
		return false
	}
	select {
	case <-s.done:
		return false
	default:
	}
	result := response.GetProbeTicketResponse()
	if result.GetErrorCode() != "" {
		return result.GetErrorCode() == probeTicketUnavailable && result.GetExecutionTicket() == "" && result.GetExpiresAt() == nil
	}
	return result.GetExecutionTicket() != "" && result.GetExpiresAt() != nil && result.GetExpiresAt().CheckValid() == nil &&
		result.GetExpiresAt().AsTime().After(now) && s.probeTicketAttemptLiveLocked(attempt, now) &&
		result.GetRequestId() == attempt.requestID && result.GetCommandId() == attempt.commandID && result.GetScope() == attempt.scope
}

func (s *nodeSession) probeTicketAttemptLiveLocked(attempt *probeTicketAttempt, now time.Time) bool {
	if attempt == nil {
		return false
	}
	pending, exists := s.pendingCommands[attempt.commandID]
	return exists && pending.ticketAttempt == attempt && pending.outbound == attempt.outbound &&
		pending.permitsProbeScope(attempt.scope) && pendingProbeTicketLive(pending, now)
}

func probeTicketBindingMatches(b store.ProbeBinding, p pendingCommand, nodeID, sessionID string, now time.Time) bool {
	for _, id := range []string{b.AccountID, b.SlotID, b.NodeID, b.LeaseOwnerID} {
		if credential.ValidateTransportID(id) != nil {
			return false
		}
	}
	if b.ProviderRef == "" || len(b.ProviderRef) > 255 || strings.TrimSpace(b.ProviderRef) != b.ProviderRef || !utf8.ValidString(b.ProviderRef) {
		return false
	}
	for _, r := range b.ProviderRef {
		if unicode.IsControl(r) {
			return false
		}
	}
	return !now.IsZero() && b.SlotID == p.slotID && b.NodeID == nodeID && b.ControlSessionID == sessionID &&
		provider.RuntimeAccountID(b.AccountID) == p.accountBinding && b.ExecutionEpoch == p.executionEpoch &&
		b.RouteGeneration == p.desiredGeneration && b.ImageDigest == p.imageDigest &&
		!b.NodeSeenAt.IsZero() && !b.NodeSeenAt.After(now) && b.NodeSeenAt.Add(probeTicketMaxNodeAge).After(now) && b.LeaseExpiresAt.After(now) &&
		(b.LastObservedAt == nil || !b.LastObservedAt.IsZero() && !b.LastObservedAt.After(now))
}

func sameProbeTicketBinding(a, b store.ProbeBinding) bool {
	return a.AccountID == b.AccountID && a.SlotID == b.SlotID && a.NodeID == b.NodeID && a.ControlSessionID == b.ControlSessionID &&
		a.ExecutionEpoch == b.ExecutionEpoch && a.RouteGeneration == b.RouteGeneration && a.ProviderRef == b.ProviderRef &&
		a.LeaseOwnerID == b.LeaseOwnerID && a.ImageDigest == b.ImageDigest
}

func probeTicketCertificateExpiry(session *nodeSession) time.Time {
	remote, ok := peer.FromContext(session.context)
	if !ok {
		return time.Time{}
	}
	info, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 || info.State.PeerCertificates[0] == nil {
		return time.Time{}
	}
	return info.State.PeerCertificates[0].NotAfter
}

func minProbeTicketTime(first time.Time, rest ...time.Time) time.Time {
	for _, value := range rest {
		if value.Before(first) {
			first = value
		}
	}
	return first
}
