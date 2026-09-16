package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func probeFixture(t *testing.T, queue, pending int) (*Server, *nodeSession) {
	t.Helper()
	server, session, _ := standaloneSession(t)
	session.outbound = make(chan *executionv1.NodeControlServiceControlResponse, queue)
	session.pendingCommands = make(map[string]pendingCommand)
	session.maxPendingCommands = pending
	t.Cleanup(session.cancelProbes)
	return server, session
}

func probeResponse(id, slot string, now time.Time) *executionv1.NodeControlServiceControlResponse {
	return &executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
		CommandId: probeTestID(id), Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, SlotId: slot, AccountId: "account-1",
		ExecutionEpoch: 1, ImageDigest: "sha256:" + strings.Repeat("a", 64), Deadline: timestamppb.New(now.Add(5 * time.Second)),
	}}}
}

// Stable synthetic nonces for table tests only. The actual scheduler uses
// crypto/rand and must never reuse an ID from an earlier round.
func probeTestID(label string) string {
	if !strings.HasPrefix(label, "probe-") {
		return label
	}
	hash := sha256.Sum256([]byte(label))
	return "probe-" + hex.EncodeToString(hash[:16])
}

func probeResult(id string, healthy bool) *executionv1.CommandResult {
	return &executionv1.CommandResult{CommandId: probeTestID(id), Succeeded: healthy, Slot: &executionv1.SlotObservation{
		SlotId: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ImageDigest: "sha256:" + strings.Repeat("a", 64), ActualState: "running", Healthy: healthy,
	}}
}

func takeProbe(t *testing.T, server *Server, session *nodeSession) *executionv1.NodeControlServiceControlResponse {
	t.Helper()
	select {
	case response := <-session.outbound:
		if !session.prepareOutbound(response, server.config.Now()) {
			t.Fatal("expected live queued probe")
		}
		return response
	default:
		t.Fatal("probe not enqueued")
		return nil
	}
}

func TestProbeDispatchRestrictedAndBounded(t *testing.T) {
	for _, test := range []string{"valid", "nil", "wrong action", "revoke", "no image", "wrong image", "expired", "over deadline", "wrong session", "caller cancelled", "closed", "queue one", "no namespace", "short nonce", "uppercase nonce", "nonhex nonce"} {
		t.Run(test, func(t *testing.T) {
			server, session := probeFixture(t, 4, 8)
			response := probeResponse("probe-1", "slot-1", server.config.Now())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := session.id
			switch test {
			case "nil":
				response = nil
			case "wrong action":
				response.GetSlotCommand().Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DESTROY
			case "revoke":
				response.Event = &executionv1.NodeControlServiceControlResponse_RevokeEpoch{RevokeEpoch: &executionv1.RevokeEpochCommand{CommandId: "probe-1", SlotId: "slot-1", ExecutionEpoch: 1}}
			case "no image":
				response.GetSlotCommand().ImageDigest = ""
			case "wrong image":
				response.GetSlotCommand().ImageDigest = "private image detail"
			case "expired":
				response.GetSlotCommand().Deadline = timestamppb.New(server.config.Now())
			case "over deadline":
				response.GetSlotCommand().Deadline = timestamppb.New(server.config.Now().Add(MaxProbeDeadline + time.Nanosecond))
			case "wrong session":
				expected = strings.Repeat("b", 32)
			case "caller cancelled":
				cancel()
			case "closed":
				close(session.done)
			case "queue one":
				session.outbound = make(chan *executionv1.NodeControlServiceControlResponse, 1)
			case "no namespace":
				response.GetSlotCommand().CommandId = strings.Repeat("a", 32)
			case "short nonce":
				response.GetSlotCommand().CommandId = "probe-1"
			case "uppercase nonce":
				response.GetSlotCommand().CommandId = "probe-" + strings.Repeat("A", 32)
			case "nonhex nonce":
				response.GetSlotCommand().CommandId = "probe-" + strings.Repeat("g", 32)
			}
			err := server.DispatchToSession(ctx, "node-1", expected, response)
			if test != "valid" {
				if err != ErrProbeDispatchUnavailable || len(session.pendingCommands) != 0 || len(session.outbound) != 0 {
					t.Fatalf("error=%v pending=%d queued=%d", err, len(session.pendingCommands), len(session.outbound))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cancel() // the caller finishing authorization must not revoke a queued round.
			pending, _ := session.command(probeTestID("probe-1"))
			if pending.probe == nil || pending.probe.ctx.Err() != nil {
				t.Fatal("probe lifetime was attached to its short dispatch caller")
			}
			response.GetSlotCommand().SlotId = "mutated"
			if got := takeProbe(t, server, session); got.GetSlotCommand().GetSlotId() != "slot-1" {
				t.Fatal("dispatch retained caller-owned message")
			}
		})
	}
}

func TestProbeDispatchPinsSessionAcrossAuthenticationIO(t *testing.T) {
	for _, change := range []string{"replace", "detach", "caller cancel", "deadline"} {
		t.Run(change, func(t *testing.T) {
			server, session := probeFixture(t, 4, 8)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			now := server.config.Now()
			response := probeResponse("probe-1", "slot-1", now)
			repository := server.repository.(sessionRepository)
			var replacement *nodeSession
			repository.validate = func(context.Context, string, string, time.Time) error {
				switch change {
				case "replace":
					// Even the same session ID cannot replace the pinned transport.
					replacement = &nodeSession{id: session.id, accepted: true, context: session.context, done: make(chan struct{}), outbound: make(chan *executionv1.NodeControlServiceControlResponse, 4)}
					server.mu.Lock()
					server.sessions["node-1"] = replacement
					server.mu.Unlock()
				case "detach":
					server.detach("node-1", session.id)
				case "caller cancel":
					cancel()
				case "deadline":
					server.config.Now = func() time.Time { return now.Add(5 * time.Second) }
				}
				return nil
			}
			server.repository = repository
			if err := server.DispatchToSession(ctx, "node-1", session.id, response); err != ErrProbeDispatchUnavailable {
				t.Fatalf("error=%v", err)
			}
			if len(session.outbound) != 0 || len(session.pendingCommands) != 0 || (replacement != nil && len(replacement.outbound) != 0) {
				t.Fatal("probe reached changed or expired session")
			}
		})
	}
}

func TestProbeReservesHalfOfQueueAndPendingCapacity(t *testing.T) {
	for _, quota := range []string{"queue", "pending", "one reserved queue place"} {
		t.Run(quota, func(t *testing.T) {
			server, session := probeFixture(t, 4, 4)
			if quota == "one reserved queue place" {
				for i := 0; i < 3; i++ {
					session.outbound <- probeResponse(fmt.Sprint("ordinary-", i), "other", server.config.Now())
				}
			} else {
				for i := 0; i < 2; i++ {
					if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse(fmt.Sprint("probe-", i), fmt.Sprint("slot-", i), server.config.Now())); err != nil {
						t.Fatal(err)
					}
					if quota == "pending" {
						takeProbe(t, server, session)
					}
				}
			}
			if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-over-quota", "slot-3", server.config.Now())); err != ErrProbeDispatchUnavailable {
				t.Fatal("probe exceeded reserved capacity", err)
			}
			if err := server.Dispatch(context.Background(), "node-1", probeResponse("ordinary", "slot-4", server.config.Now())); err != nil {
				t.Fatal("probe starved ordinary dispatch", err)
			}
		})
	}
}

func TestProbeSingleFlightAndPendingCommandConflict(t *testing.T) {
	for _, kind := range []pendingCommandKind{pendingSlotCommand, pendingEpochRevocation, pendingCredentialKey, pendingSecureActivation} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			server, session := probeFixture(t, 4, 8)
			session.pendingCommands["ordinary"] = pendingCommand{kind: kind, slotID: "slot-1", executionEpoch: 1}
			if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-1", "slot-1", server.config.Now())); err != ErrProbeDispatchUnavailable {
				t.Fatalf("kind=%d error=%v", kind, err)
			}
			delete(session.pendingCommands, "ordinary")
			if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-1", "slot-1", server.config.Now())); err != nil {
				t.Fatal(err)
			}
			takeProbe(t, server, session)
			if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-2", "slot-1", server.config.Now())); err != ErrProbeDispatchUnavailable {
				t.Fatal("duplicate probe accepted", err)
			}
		})
	}
}

func TestProbeConcurrentRoundsRemainSingleFlight(t *testing.T) {
	server, session := probeFixture(t, 8, 16)
	results := make(chan error, 32)
	for i := range 32 {
		go func() {
			results <- server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse(fmt.Sprint("probe-", i), "slot-1", server.config.Now()))
		}()
	}
	accepted := 0
	for range 32 {
		err := <-results
		if err == nil {
			accepted++
		} else if err != ErrProbeDispatchUnavailable {
			t.Fatal(err)
		}
	}
	if accepted != 1 || len(session.pendingCommands) != 1 || len(session.outbound) != 1 {
		t.Fatalf("accepted=%d pending=%d queued=%d", accepted, len(session.pendingCommands), len(session.outbound))
	}
}

func TestProbeExpiryReclaimsOnlyProbesAndSkipsQueuedCommands(t *testing.T) {
	server, session := probeFixture(t, 4, 8)
	now := server.config.Now()
	session.pendingCommands["credential-commit"] = pendingCommand{kind: pendingSecureActivation, slotID: "other", executionEpoch: 1, deadline: now.Add(-time.Second), commitStarted: true}
	if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-expired", "slot-1", now)); err != nil {
		t.Fatal(err)
	}
	old := <-session.outbound
	now = now.Add(5 * time.Second)
	server.config.Now = func() time.Time { return now }
	if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-new", "slot-1", now)); err != nil {
		t.Fatal("expired probe blocked new round", err)
	}
	if _, exists := session.command("credential-commit"); !exists {
		t.Fatal("probe cleanup reclaimed credential commit")
	}
	if session.prepareOutbound(old, now) {
		t.Fatal("expired probe was sent")
	}
	takeProbe(t, server, session)
	if len(session.queuedProbes) != 0 {
		t.Fatal("consumed queue marker retained")
	}
}

func TestOrdinaryCommandInvalidatesSameSlotProbeBeforeCapacityCheck(t *testing.T) {
	server, session := probeFixture(t, 4, 2)
	now := server.config.Now()
	if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-1", "slot-1", now)); err != nil {
		t.Fatal(err)
	}
	probe, _ := session.command(probeTestID("probe-1"))
	session.pendingCommands["other-command"] = pendingCommand{kind: pendingSecureActivation, slotID: "other", executionEpoch: 1}
	ordinary := probeResponse("ordinary", "slot-1", now)
	ordinary.GetSlotCommand().ExecutionEpoch = 2 // mutation of another epoch still supersedes old observation.
	if err := server.Dispatch(context.Background(), "node-1", ordinary); err != nil {
		t.Fatal("probe blocked superseding ordinary command", err)
	}
	if probe.probe.ctx.Err() == nil {
		t.Fatal("superseded probe context remains live")
	}
	if session.prepareOutbound(<-session.outbound, now) {
		t.Fatal("invalidated queued probe was sent")
	}
	if got := <-session.outbound; !session.prepareOutbound(got, now) || got.GetSlotCommand().GetCommandId() != "ordinary" {
		t.Fatal("ordinary command was skipped")
	}
}

func TestOrdinaryCommandCannotReplaceProbeUsingItsID(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			server, session := probeFixture(t, 4, 8)
			now := server.config.Now()
			if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-same-id", "slot-1", now)); err != nil {
				t.Fatal(err)
			}
			original, _ := session.command(probeTestID("probe-same-id"))
			takeProbe(t, server, session)
			if expired {
				now = now.Add(5 * time.Second)
				server.config.Now = func() time.Time { return now }
			}
			if err := server.Dispatch(context.Background(), "node-1", probeResponse("probe-same-id", "slot-1", now)); err == nil {
				t.Fatal("ordinary command replaced probe under its ID")
			}
			current, exists := session.command(probeTestID("probe-same-id"))
			if !exists || current.probe != original.probe || current.probe == nil || len(session.outbound) != 0 {
				t.Fatal("ID collision changed pending probe authority")
			}
		})
	}
}

func TestReapedProbeIDCannotBeReclassifiedAsOrdinaryCommand(t *testing.T) {
	server, session := probeFixture(t, 4, 8)
	now := server.config.Now()
	writes := 0
	server.repository = proofRepository{NodeRepository: server.repository, apply: func(context.Context, store.CommandResult) error { writes++; return nil }}
	if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-old", "slot-1", now)); err != nil {
		t.Fatal(err)
	}
	takeProbe(t, server, session)
	now = now.Add(5 * time.Second)
	server.config.Now = func() time.Time { return now }
	// A subsequent round reclaims the old pending ID, so a mere duplicate-map
	// check would no longer prevent reclassifying the old result as ordinary.
	if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-next", "slot-1", now)); err != nil {
		t.Fatal(err)
	}
	if _, exists := session.command(probeTestID("probe-old")); exists {
		t.Fatal("fixture did not reclaim expired probe")
	}
	if err := server.Dispatch(context.Background(), "node-1", probeResponse("probe-old", "slot-1", now)); err == nil {
		t.Fatal("ordinary command adopted a reclaimed probe ID")
	}
	if err := server.recordCommandResult(context.Background(), "node-1", session, probeResult("probe-old", true)); err == nil || writes != 0 {
		t.Fatalf("late probe result=%v writes=%d", err, writes)
	}
	if current, exists := session.command(probeTestID("probe-next")); !exists || current.probe == nil || current.probe.ctx.Err() != nil {
		t.Fatal("rejected ID reuse cancelled the valid new probe")
	}
}

func TestOrdinaryDispatchRejectsEntireProbeNamespace(t *testing.T) {
	server, session := probeFixture(t, 4, 8)
	for _, id := range []string{"probe-", "probe-invalid", "probe-" + strings.Repeat("a", 32)} {
		response := probeResponse("ordinary", "slot-1", server.config.Now())
		response.GetSlotCommand().CommandId = id
		if err := server.Dispatch(context.Background(), "node-1", response); err == nil || len(session.pendingCommands) != 0 || len(session.outbound) != 0 {
			t.Fatal("reserved probe namespace entered ordinary pending state", err)
		}
	}
}

func TestExpiredOrInvalidatedProbeResultsNeverReachStorage(t *testing.T) {
	for _, state := range []string{"expired", "invalidated", "detached"} {
		for _, healthy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/healthy=%t", state, healthy), func(t *testing.T) {
				server, session := probeFixture(t, 4, 8)
				now := server.config.Now()
				writes := 0
				server.repository = proofRepository{NodeRepository: server.repository, apply: func(context.Context, store.CommandResult) error { writes++; return nil }}
				if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-1", "slot-1", now)); err != nil {
					t.Fatal(err)
				}
				takeProbe(t, server, session)
				switch state {
				case "expired":
					server.config.Now = func() time.Time { return now.Add(5 * time.Second) }
				case "invalidated":
					if err := server.Dispatch(context.Background(), "node-1", probeResponse("ordinary", "slot-1", now)); err != nil {
						t.Fatal(err)
					}
				case "detached":
					server.detach("node-1", session.id)
				}
				if err := server.recordCommandResult(context.Background(), "node-1", session, probeResult("probe-1", healthy)); err == nil || writes != 0 {
					t.Fatalf("error=%v writes=%d", err, writes)
				}
			})
		}
	}
}

func TestProbeResultStorageContextCancelledBySupersedingCommand(t *testing.T) {
	for _, cause := range []string{"mutation", "detach", "stream cancelled"} {
		t.Run(cause, func(t *testing.T) {
			server, session := probeFixture(t, 4, 8)
			stream, cancelStream := context.WithCancel(session.context)
			defer cancelStream()
			session.context = stream
			entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			server.repository = proofRepository{NodeRepository: server.repository, apply: func(ctx context.Context, _ store.CommandResult) error {
				close(entered)
				<-ctx.Done()
				close(cancelled)
				<-release
				return ctx.Err()
			}}
			if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-1", "slot-1", server.config.Now())); err != nil {
				t.Fatal(err)
			}
			takeProbe(t, server, session)
			result := make(chan error, 1)
			go func() {
				result <- server.recordCommandResult(context.Background(), "node-1", session, probeResult("probe-1", false))
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("result did not enter storage")
			}
			switch cause {
			case "mutation":
				if err := server.Dispatch(context.Background(), "node-1", probeResponse("ordinary", "slot-1", server.config.Now())); err != nil {
					t.Fatal(err)
				}
			case "detach":
				server.detach("node-1", session.id)
			case "stream cancelled":
				cancelStream()
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("storage context was not cancelled")
			}
			close(release)
			if err := <-result; err == nil {
				t.Fatal("cancelled result reported success")
			}
		})
	}
}

func TestProbeSuccessfulResultReleasesPendingWithoutHistory(t *testing.T) {
	server, session := probeFixture(t, 4, 8)
	writes := 0
	server.repository = proofRepository{NodeRepository: server.repository, apply: func(ctx context.Context, result store.CommandResult) error {
		if ctx.Err() != nil || result.ControlSessionID != session.id || result.ExpectedImageDigest != probeResult("", true).GetSlot().GetImageDigest() {
			return errors.New("missing session or image binding")
		}
		writes++
		return nil
	}}
	if err := server.DispatchToSession(context.Background(), "node-1", session.id, probeResponse("probe-1", "slot-1", server.config.Now())); err != nil {
		t.Fatal(err)
	}
	pending, _ := session.command(probeTestID("probe-1"))
	takeProbe(t, server, session)
	if err := server.recordCommandResult(context.Background(), "node-1", session, probeResult("probe-1", true)); err != nil {
		t.Fatal(err)
	}
	if writes != 1 || len(session.pendingCommands) != 0 || len(session.queuedProbes) != 0 || pending.probe.ctx.Err() == nil {
		t.Fatal("completed probe retained live pending state")
	}
	if err := server.recordCommandResult(context.Background(), "node-1", session, probeResult("probe-1", true)); err == nil || writes != 1 {
		t.Fatal("duplicate result was accepted")
	}
}
