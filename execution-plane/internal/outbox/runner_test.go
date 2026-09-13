package outbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunnerDrainsBoundedOrderedPage(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	events := make([]Event, 3)
	for index := range events {
		events[index] = Event{
			Sequence: int64(index + 1), EventID: "event-" + string(rune('a'+index)), AccountID: 7,
			EventType: "account.runtime.provision_requested", DesiredGeneration: uint64(index + 1),
			PayloadJSON: []byte(`{}`), CreatedAt: now,
		}
	}
	source, _ := NewMemorySource(events)
	var calls []string
	router, _ := NewRouter(map[string]Handler{
		"account.runtime.provision_requested": recordingHandler{name: "runtime", calls: &calls},
	})
	consumer, _ := NewConsumer(source, router, "runtime", "owner-a", time.Minute, func() time.Time { return now })
	runner, err := NewRunner(consumer, RunnerConfig{BatchSize: 2, PollInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Step(context.Background())
	if err != nil || result.Processed != 2 || result.Failed != 0 || len(calls) != 2 {
		t.Fatalf("first Step() = %+v calls=%v err=%v", result, calls, err)
	}
	result, err = runner.Step(context.Background())
	if err != nil || result.Processed != 1 || len(calls) != 3 {
		t.Fatalf("second Step() = %+v calls=%v err=%v", result, calls, err)
	}
}

func TestRunnerStopsAtFailedEventAndRetriesSameSequence(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	clock := now
	source, _ := NewMemorySource([]Event{
		{Sequence: 1, EventID: "event-1", AccountID: 7, EventType: "runtime", DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: now},
		{Sequence: 2, EventID: "event-2", AccountID: 7, EventType: "runtime", DesiredGeneration: 2, PayloadJSON: []byte(`{}`), CreatedAt: now},
	})
	var calls []string
	handlerErr := errors.New("projection unavailable")
	router, _ := NewRouter(map[string]Handler{"runtime": retryableRecordingHandler{
		calls: &calls, now: func() time.Time { return clock }, err: handlerErr,
	}})
	consumer, _ := NewConsumer(source, router, "runtime", "owner-a", time.Minute, func() time.Time { return clock })
	var errorsSeen int
	runner, _ := NewRunner(consumer, RunnerConfig{BatchSize: 10, OnError: func(error) { errorsSeen++ }})
	result, err := runner.Step(context.Background())
	if !errors.Is(err, ErrRun) || result.Processed != 1 || result.Failed != 1 {
		t.Fatalf("Step(0) = %+v, %v", result, err)
	}
	if result, err = runner.Step(context.Background()); err != nil || result.Processed != 0 {
		t.Fatalf("retry-wait Step = %+v, %v", result, err)
	}
	clock = clock.Add(time.Second)
	result, err = runner.Step(context.Background())
	if !errors.Is(err, ErrRun) || result.Processed != 1 || result.Failed != 1 {
		t.Fatalf("Step(1) = %+v, %v", result, err)
	}
	if len(calls) != 2 || calls[0] != "runtime:event-1" || calls[1] != "runtime:event-1" || errorsSeen != 2 {
		t.Fatalf("calls=%v errors=%d", calls, errorsSeen)
	}
}

func TestRunnerRunPropagatesDurableBlockedCheckpoint(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	source, _ := NewMemorySource([]Event{{
		Sequence: 1, EventID: "event-1", AccountID: 7, EventType: "runtime", DesiredGeneration: 1,
		PayloadJSON: []byte(`{}`), CreatedAt: now,
	}})
	router, _ := NewRouter(map[string]Handler{
		"runtime": recordingHandler{name: "runtime", calls: &[]string{}, err: errors.New("unclassified corruption")},
	})
	consumer, _ := NewConsumer(source, router, "runtime", "owner-a", time.Minute, func() time.Time { return now })
	var blockedSeen BlockedError
	runner, _ := NewRunner(consumer, RunnerConfig{
		PollInterval: time.Millisecond,
		OnBlocked:    func(blocked BlockedError) { blockedSeen = blocked },
	})
	if err := runner.Run(context.Background()); !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked runner error = %v", err)
	}
	if blockedSeen.ConsumerName != "runtime" || blockedSeen.Sequence != 1 || blockedSeen.Code != "handler_unclassified" ||
		blockedSeen.BlockedClaimVersion == 0 {
		t.Fatalf("blocked callback = %+v", blockedSeen)
	}
}

func TestRunnerRunPropagatesRetryBudgetWithSafeCallback(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	clock := now
	source, _ := NewMemorySource([]Event{{
		Sequence: 1, EventID: "event-1", AccountID: 7, EventType: "runtime", DesiredGeneration: 1,
		PayloadJSON: []byte(`{}`), CreatedAt: now,
	}})
	var calls []string
	router, _ := NewRouter(map[string]Handler{"runtime": retryableRecordingHandler{
		calls: &calls, now: func() time.Time { return clock }, err: errors.New("storage unavailable"),
	}})
	consumer, _ := NewConsumer(source, router, "runtime", "owner-a", time.Minute, func() time.Time { return clock }, 1)
	var exhaustedSeen RetryBudgetError
	runner, _ := NewRunner(consumer, RunnerConfig{
		PollInterval: time.Millisecond,
		OnRetryBudgetExceeded: func(exhausted RetryBudgetError) {
			exhaustedSeen = exhausted
		},
	})
	if err := runner.Run(context.Background()); !errors.Is(err, ErrRetryBudgetExceeded) {
		t.Fatalf("retry budget runner error = %v", err)
	}
	if exhaustedSeen.ConsumerName != "runtime" || exhaustedSeen.Sequence != 1 ||
		exhaustedSeen.Code != "projection_unavailable" || exhaustedSeen.FailureCount != 1 {
		t.Fatalf("retry budget callback = %+v", exhaustedSeen)
	}
}

type retryableRecordingHandler struct {
	calls *[]string
	now   func() time.Time
	err   error
}

func (h retryableRecordingHandler) ApplyRuntimeEvent(_ context.Context, event Event) error {
	*h.calls = append(*h.calls, "runtime:"+event.EventID)
	return RetryableHandlerError("projection_unavailable", h.now().Add(time.Second), h.err)
}

func TestRunnerStopsCleanly(t *testing.T) {
	source, _ := NewMemorySource(nil)
	var calls []string
	router, _ := NewRouter(map[string]Handler{"runtime": recordingHandler{name: "runtime", calls: &calls}})
	consumer, _ := NewConsumer(source, router, "runtime", "owner-a", time.Minute, time.Now)
	runner, _ := NewRunner(consumer, RunnerConfig{PollInterval: time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
	if _, err := NewRunner(nil, RunnerConfig{}); !errors.Is(err, ErrRun) {
		t.Fatalf("nil consumer error = %v", err)
	}
}

func TestRunnerTreatsCancellationDuringStepAsCleanShutdown(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	source, _ := NewMemorySource([]Event{{
		Sequence: 1, EventID: "event-1", AccountID: 7, EventType: "runtime", DesiredGeneration: 1,
		PayloadJSON: []byte(`{}`), CreatedAt: now,
	}})
	ctx, cancel := context.WithCancel(context.Background())
	handler := cancelingHandler{cancel: cancel}
	consumer, _ := NewConsumer(source, handler, "runtime", "owner-a", time.Minute, func() time.Time { return now })
	runner, _ := NewRunner(consumer, RunnerConfig{PollInterval: time.Millisecond, MaxConsecutiveFailures: 1})
	if err := runner.Run(ctx); err != nil {
		t.Fatalf("cancellation during Step = %v", err)
	}
}

type cancelingHandler struct {
	cancel context.CancelFunc
}

func (h cancelingHandler) ApplyRuntimeEvent(context.Context, Event) error {
	h.cancel()
	return context.Canceled
}
