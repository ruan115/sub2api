package control

import (
	"context"
	"errors"
	"regexp"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"google.golang.org/protobuf/proto"
)

// MaxProbeDeadline bounds an observation round; it is not an execution lease.
const MaxProbeDeadline = 10 * time.Second

// ErrProbeDispatchUnavailable deliberately does not disclose node or slot state.
var ErrProbeDispatchUnavailable = errors.New("probe dispatch unavailable")

// The reserved namespace prevents a reclaimed probe from being reinterpreted
// as a lifecycle command. The trusted scheduler supplies a fresh random nonce
// each round; the server deliberately does not retain an unbounded ID history.
var probeCommandIDPattern = regexp.MustCompile(`^probe-[a-f0-9]{32}$`)

type pendingProbe struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// DispatchToSession only dispatches bounded, read-only INSPECT commands to the
// exact authenticated session supplied by the caller. It never waits for queue
// space and never switches to a replacement session during authentication I/O.
func (s *Server) DispatchToSession(ctx context.Context, nodeID, expectedSessionID string, response *executionv1.NodeControlServiceControlResponse) error {
	if s == nil || s.config.Now == nil || ctx == nil || ctx.Err() != nil || response == nil ||
		!nodeIDPattern.MatchString(nodeID) || len(expectedSessionID) != 32 {
		return ErrProbeDispatchUnavailable
	}
	cloned, ok := proto.Clone(response).(*executionv1.NodeControlServiceControlResponse)
	if !ok || validateControlResponse(cloned) != nil {
		return ErrProbeDispatchUnavailable
	}
	command := cloned.GetSlotCommand()
	now := s.config.Now().UTC()
	if command == nil || command.GetAction() != executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT ||
		!probeCommandIDPattern.MatchString(command.GetCommandId()) || !imageDigestPattern.MatchString(command.GetImageDigest()) ||
		!validProbeDeadline(controlDeadline(cloned), now) {
		return ErrProbeDispatchUnavailable
	}
	s.mu.RLock()
	pinned := s.sessions[nodeID]
	live := liveSession(pinned, expectedSessionID)
	s.mu.RUnlock()
	if !live {
		return ErrProbeDispatchUnavailable
	}
	if err := s.ValidateControlSession(ctx, nodeID, expectedSessionID); err != nil {
		return ErrProbeDispatchUnavailable
	}
	// Keep the map pin through the non-blocking insertion. Never hold it while
	// validating durable state or doing any other I/O.
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.sessions[nodeID] != pinned || !liveSession(pinned, expectedSessionID) || ctx.Err() != nil {
		return ErrProbeDispatchUnavailable
	}
	if !pinned.enqueueProbe(ctx, cloned, s.config.Now().UTC()) {
		return ErrProbeDispatchUnavailable
	}
	return nil
}

func validProbeDeadline(deadline, now time.Time) bool {
	return deadline.After(now) && !deadline.After(now.Add(MaxProbeDeadline))
}

func (s *nodeSession) enqueueProbe(caller context.Context, response *executionv1.NodeControlServiceControlResponse, now time.Time) bool {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	s.reapProbesLocked(now)
	command := pendingFromResponse(response)
	commandID := controlCommandID(response)
	if caller.Err() != nil || s.context == nil || s.context.Err() != nil || !validProbeDeadline(command.deadline, now) ||
		len(s.pendingCommands) >= s.maxPendingCommands || s.maxPendingCommands < 2 || cap(s.outbound) < 2 ||
		len(s.outbound) >= cap(s.outbound)-1 || len(s.queuedProbes) >= cap(s.outbound)/2 || s.queuedProbeIDLocked(commandID) {
		return false
	}
	probeCount := 0
	for id, pending := range s.pendingCommands {
		if id == commandID || (pending.slotID == command.slotID && pending.executionEpoch == command.executionEpoch) {
			return false
		}
		if pending.probe != nil {
			probeCount++
		}
	}
	if probeCount >= s.maxPendingCommands/2 {
		return false
	}
	// The caller's short authorization context may be cancelled as soon as we
	// return. A queued probe instead belongs to this pinned control session.
	probeCtx, cancel := context.WithDeadline(s.context, command.deadline)
	if probeCtx.Err() != nil {
		cancel()
		return false
	}
	command.probe = &pendingProbe{ctx: probeCtx, cancel: cancel}
	if s.pendingCommands == nil {
		s.pendingCommands = make(map[string]pendingCommand)
	}
	if s.queuedProbes == nil {
		s.queuedProbes = make(map[*executionv1.NodeControlServiceControlResponse]*pendingProbe)
	}
	s.pendingCommands[commandID] = command
	s.queuedProbes[response] = command.probe
	select {
	case s.outbound <- response:
		return true
	default:
		delete(s.pendingCommands, commandID)
		delete(s.queuedProbes, response)
		cancel()
		return false
	}
}

func (s *nodeSession) queuedProbeIDLocked(commandID string) bool {
	for response := range s.queuedProbes {
		if controlCommandID(response) == commandID {
			return true
		}
	}
	return false
}

func (s *nodeSession) reapProbesLocked(now time.Time) {
	for id, pending := range s.pendingCommands {
		if pending.probe != nil && (pending.probe.ctx.Err() != nil || !pending.deadline.After(now)) {
			pending.cancelTicket()
			pending.probe.cancel()
			delete(s.pendingCommands, id)
		}
	}
}

// Queue markers outlive pending invalidation, but only until their bounded
// queue item is consumed. This makes stale queued probes skippable without an
// ever-growing history of command IDs.
func (s *nodeSession) prepareOutbound(response *executionv1.NodeControlServiceControlResponse, now time.Time) bool {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if response.GetProbeTicketResponse() != nil {
		return s.prepareProbeTicketLocked(response, now)
	}
	probe, marked := s.queuedProbes[response]
	if marked {
		delete(s.queuedProbes, response)
		s.reapProbesLocked(now)
		pending, exists := s.pendingCommands[controlCommandID(response)]
		if !exists || pending.probe != probe || probe.ctx.Err() != nil {
			return false
		}
	}
	if id := controlCommandID(response); response.GetCredentialKeyCommand() != nil ||
		response.GetSlotCommand().GetAction() == executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT {
		pending, exists := s.pendingCommands[id]
		if !exists || pending.outbound != response {
			return false
		}
		// The authorization point is handing this exact object to stream.Send,
		// not reserving/queueing it and not a claim of remote delivery.
		pending.sending = true
		s.pendingCommands[id] = pending
	}
	return true
}

func (s *nodeSession) probeCurrent(commandID string, probe *pendingProbe, now time.Time) bool {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	s.reapProbesLocked(now)
	pending, exists := s.pendingCommands[commandID]
	return exists && pending.probe == probe && probe.ctx.Err() == nil
}

func (s *nodeSession) probeResultContext(caller context.Context, commandID string, probe *pendingProbe, now time.Time) (context.Context, context.CancelFunc, bool) {
	if caller.Err() != nil || !s.probeCurrent(commandID, probe, now) {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(probe.ctx)
	stop := context.AfterFunc(caller, cancel)
	return ctx, func() { stop(); cancel() }, true
}

func (s *nodeSession) releaseProbe(commandID string, probe *pendingProbe) {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	if pending, exists := s.pendingCommands[commandID]; exists && pending.probe == probe {
		pending.cancelTicket()
		probe.cancel()
		delete(s.pendingCommands, commandID)
	}
}

func (s *nodeSession) cancelProbes() {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	for id, pending := range s.pendingCommands {
		pending.cancelTicket()
		if pending.probe != nil {
			pending.probe.cancel()
			delete(s.pendingCommands, id)
		}
	}
}
