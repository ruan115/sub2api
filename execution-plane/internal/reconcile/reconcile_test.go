package reconcile

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestPlanDesiredAndActualStateTransitions(t *testing.T) {
	tests := []struct {
		name       string
		desired    DesiredState
		assignment *Assignment
		want       ActionKind
	}{
		{name: "ready places missing assignment", desired: DesiredReady, want: ActionPlace},
		{name: "absent without assignment is settled", desired: DesiredAbsent, want: ActionNone},
		{name: "ready creates missing runtime", desired: DesiredReady, assignment: testAssignment(ActualMissing, true), want: ActionCreate},
		{name: "ready starts created runtime", desired: DesiredReady, assignment: testAssignment(ActualCreated, true), want: ActionStart},
		{name: "healthy running runtime is settled", desired: DesiredReady, assignment: testAssignment(ActualRunning, true), want: ActionNone},
		{name: "unhealthy running runtime drains", desired: DesiredReady, assignment: testAssignment(ActualRunning, false), want: ActionDrain},
		{name: "ready inspects creating runtime", desired: DesiredReady, assignment: testAssignment(ActualCreating, false), want: ActionInspect},
		{name: "drained target drains running runtime", desired: DesiredDrained, assignment: testAssignment(ActualRunning, true), want: ActionDrain},
		{name: "drained target keeps stopped runtime", desired: DesiredDrained, assignment: testAssignment(ActualStopped, false), want: ActionNone},
		{name: "absent target drains running runtime", desired: DesiredAbsent, assignment: testAssignment(ActualRunning, true), want: ActionDrain},
		{name: "absent target destroys drained runtime", desired: DesiredAbsent, assignment: testAssignment(ActualDrained, false), want: ActionDestroy},
		{name: "absent target releases destroyed assignment", desired: DesiredAbsent, assignment: testAssignment(ActualDestroyed, false), want: ActionRelease},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action, err := Plan(Input{Slot: testSlot(test.desired), Assignment: test.assignment})
			if err != nil {
				t.Fatal(err)
			}
			if action.Kind != test.want {
				t.Fatalf("action kind = %s, want %s", action.Kind, test.want)
			}
			if action.Kind != ActionNone && action.IdempotencyKey == "" {
				t.Fatal("state-changing action has no idempotency key")
			}
		})
	}
}

func TestPlanImageReplacementUsesDrainDestroyReleaseSequence(t *testing.T) {
	states := []struct {
		actual ActualState
		want   ActionKind
	}{
		{actual: ActualRunning, want: ActionDrain},
		{actual: ActualDrained, want: ActionDestroy},
		{actual: ActualDestroyed, want: ActionRelease},
	}
	for _, state := range states {
		assignment := testAssignment(state.actual, state.actual == ActualRunning)
		assignment.ImageDigest = "sha256:" + strings.Repeat("b", 64)
		action, err := Plan(Input{Slot: testSlot(DesiredReady), Assignment: assignment})
		if err != nil {
			t.Fatal(err)
		}
		if action.Kind != state.want {
			t.Fatalf("actual %s action = %s, want %s", state.actual, action.Kind, state.want)
		}
	}
}

func TestControllerClaimsConcurrentReconcileExactlyOnce(t *testing.T) {
	jobs := store.NewMemoryJobRepository()
	executor := &countingExecutor{}
	now := time.Unix(2_000_000_000, 0).UTC()
	controller, err := NewController(jobs, executor, Config{
		ClaimTTL: 30 * time.Second, RetryDelay: 5 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Slot: testSlot(DesiredReady), Assignment: testAssignment(ActualMissing, false)}
	const contenders = 32
	var claimed atomic.Int32
	var wait sync.WaitGroup
	wait.Add(contenders)
	for range contenders {
		go func() {
			defer wait.Done()
			result, err := controller.Reconcile(context.Background(), input)
			if err != nil {
				t.Errorf("reconcile: %v", err)
				return
			}
			if result.Claimed {
				claimed.Add(1)
			}
		}()
	}
	wait.Wait()
	if got := claimed.Load(); got != 1 {
		t.Fatalf("claimed reconciles = %d", got)
	}
	if got := executor.calls.Load(); got != 1 {
		t.Fatalf("executed actions = %d", got)
	}
}

func TestControllerRetriesFailedDispatchAfterBackoffWithStableCommandID(t *testing.T) {
	jobs := store.NewMemoryJobRepository()
	executor := &countingExecutor{err: errors.New("node unavailable")}
	now := time.Unix(2_000_000_000, 0).UTC()
	controller, err := NewController(jobs, executor, Config{
		ClaimTTL: 30 * time.Second, RetryDelay: 5 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Slot: testSlot(DesiredReady), Assignment: testAssignment(ActualMissing, false)}
	first, err := controller.Reconcile(context.Background(), input)
	if err == nil {
		t.Fatal("expected initial dispatch failure")
	}
	executor.err = nil
	immediate, err := controller.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if immediate.Claimed {
		t.Fatal("failed job was reclaimed before retry time")
	}
	now = now.Add(5 * time.Second)
	retried, err := controller.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !retried.Claimed || !retried.Dispatched || retried.Action.CommandID != first.Action.CommandID {
		t.Fatalf("retry result = %+v, first = %+v", retried, first)
	}
	if executor.calls.Load() != 2 {
		t.Fatalf("executor calls = %d", executor.calls.Load())
	}
}

type countingExecutor struct {
	calls atomic.Int32
	err   error
}

func (e *countingExecutor) Execute(_ context.Context, _ Action) error {
	e.calls.Add(1)
	return e.err
}

func testSlot(desired DesiredState) Slot {
	return Slot{
		ID: "slot-1", AccountID: "account-1", DesiredState: desired,
		DesiredGeneration: 3, ImageDigest: "sha256:" + strings.Repeat("a", 64),
	}
}

func testAssignment(actual ActualState, healthy bool) *Assignment {
	return &Assignment{
		ID: "assignment-1", SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 7,
		ActualGeneration: 2, ImageDigest: "sha256:" + strings.Repeat("a", 64), ActualState: actual, Healthy: healthy,
	}
}
