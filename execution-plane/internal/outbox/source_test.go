package outbox

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemorySourceCheckpointReplaysAndFencesOwners(t *testing.T) {
	base := time.Unix(2_000_000_000, 0).UTC()
	source, err := NewMemorySource([]Event{
		{Sequence: 1, EventID: "event-1", AccountID: 1, EventType: "account.runtime.provision_requested", DesiredGeneration: 1, PayloadJSON: []byte(`{"provider":"docker"}`), CreatedAt: base},
		{Sequence: 2, EventID: "event-2", AccountID: 1, EventType: "account.runtime.drain_requested", DesiredGeneration: 2, PayloadJSON: []byte(`{"reason_code":"archive"}`), CreatedAt: base},
	})
	if err != nil {
		t.Fatal(err)
	}
	first, ok, err := source.Claim(context.Background(), "orchestrator", "a", base, 30*time.Second)
	if err != nil || !ok || first.Sequence != 1 {
		t.Fatalf("first claim = %+v, %v, %v", first, ok, err)
	}
	if _, _, err := source.Claim(context.Background(), "orchestrator", "b", base.Add(time.Second), 30*time.Second); !errors.Is(err, ErrBusy) {
		t.Fatalf("other owner claim error = %v", err)
	}
	if err := source.Nack(context.Background(), "orchestrator", "a", 1, "dispatch_failed", base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	replay, ok, err := source.Claim(context.Background(), "orchestrator", "b", base.Add(3*time.Second), 30*time.Second)
	if err != nil || !ok || replay.EventID != first.EventID {
		t.Fatalf("replay = %+v, %v, %v", replay, ok, err)
	}
	if err := source.Ack(context.Background(), "orchestrator", "b", 1, base.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	second, ok, err := source.Claim(context.Background(), "orchestrator", "a", base.Add(5*time.Second), time.Second)
	if err != nil || !ok || second.Sequence != 2 {
		t.Fatalf("second claim = %+v, %v, %v", second, ok, err)
	}
	stolen, ok, err := source.Claim(context.Background(), "orchestrator", "b", base.Add(7*time.Second), 30*time.Second)
	if err != nil || !ok || stolen.Sequence != 2 {
		t.Fatalf("stolen claim = %+v, %v, %v", stolen, ok, err)
	}
	if err := source.Ack(context.Background(), "orchestrator", "a", 2, base.Add(8*time.Second)); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("stale owner ack error = %v", err)
	}
	if err := source.Ack(context.Background(), "orchestrator", "b", 2, base.Add(8*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestMemorySourceAllowsOneConcurrentCheckpointOwner(t *testing.T) {
	base := time.Unix(2_000_000_000, 0).UTC()
	source, _ := NewMemorySource([]Event{{
		Sequence: 1, EventID: "event-1", AccountID: 1, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: base,
	}})
	const contenders = 32
	var claimed atomic.Int32
	var wait sync.WaitGroup
	wait.Add(contenders)
	for index := range contenders {
		go func(index int) {
			defer wait.Done()
			if _, ok, err := source.Claim(context.Background(), "orchestrator", string(rune('a'+index)), base, time.Minute); err == nil && ok {
				claimed.Add(1)
			}
		}(index)
	}
	wait.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("concurrent checkpoint owners = %d", claimed.Load())
	}
}

func TestConsumerNacksStableCodeAndReplaysHandlerFailure(t *testing.T) {
	base := time.Unix(2_000_000_000, 0).UTC()
	now := base
	source, _ := NewMemorySource([]Event{{
		Sequence: 1, EventID: "event-1", AccountID: 1, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: base,
	}})
	handler := &testHandler{err: errors.New("upstream secret must not become checkpoint state")}
	consumer, err := NewConsumer(source, handler, "orchestrator", "replica-a", time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := consumer.RunOnce(context.Background()); !processed || err == nil {
		t.Fatalf("failed handler run = processed %v, err %v", processed, err)
	}
	handler.err = nil
	now = now.Add(time.Second)
	if processed, err := consumer.RunOnce(context.Background()); !processed || err != nil {
		t.Fatalf("replayed handler run = processed %v, err %v", processed, err)
	}
	if handler.calls.Load() != 2 {
		t.Fatalf("handler calls = %d", handler.calls.Load())
	}
}

func TestEventRejectsSensitivePayload(t *testing.T) {
	event := Event{Sequence: 1, EventID: "event", AccountID: 1, EventType: "account.runtime.provision_requested", DesiredGeneration: 1, PayloadJSON: []byte(`{"credentials":{"access_token":"sk-secret"}}`), CreatedAt: time.Now()}
	if err := event.Validate(); err == nil {
		t.Fatal("sensitive event payload was accepted")
	}
}

type testHandler struct {
	calls atomic.Int32
	err   error
}

func (h *testHandler) ApplyRuntimeEvent(context.Context, Event) error {
	h.calls.Add(1)
	return h.err
}
