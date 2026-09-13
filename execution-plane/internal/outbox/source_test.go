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
	if err := source.Fail(context.Background(), first, Failure{
		Class: FailureRetryable, Code: "dispatch_failed", FailedAt: base.Add(2 * time.Second), RetryAfter: base.Add(3 * time.Second),
		RetryLimit: DefaultMaxRetryFailures,
	}); err != nil {
		t.Fatal(err)
	}
	replay, ok, err := source.Claim(context.Background(), "orchestrator", "b", base.Add(3*time.Second), 30*time.Second)
	if err != nil || !ok || replay.EventID != first.EventID {
		t.Fatalf("replay = %+v, %v, %v", replay, ok, err)
	}
	if err := source.Ack(context.Background(), replay, base.Add(4*time.Second)); err != nil {
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
	if err := source.Ack(context.Background(), second, base.Add(8*time.Second)); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("stale owner ack error = %v", err)
	}
	if err := source.Ack(context.Background(), stolen, base.Add(8*time.Second)); err != nil {
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

func TestConsumerPersistsRetryableCodeAndReplaysHandlerFailure(t *testing.T) {
	base := time.Unix(2_000_000_000, 0).UTC()
	now := base
	source, _ := NewMemorySource([]Event{{
		Sequence: 1, EventID: "event-1", AccountID: 1, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: base,
	}})
	cause := errors.New("upstream secret must not become checkpoint state")
	handler := &testHandler{err: RetryableHandlerError("storage_unavailable", base.Add(time.Second), cause)}
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

func TestMemorySourceBlocksUnclassifiedFailureAndFencesSameOwnerABA(t *testing.T) {
	base := time.Unix(2_000_000_000, 0).UTC()
	source, _ := NewMemorySource([]Event{{
		Sequence: 1, EventID: "event-1", AccountID: 1, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: base,
	}})
	first, ok, err := source.Claim(context.Background(), "orchestrator", "replica-a", base, time.Second)
	if err != nil || !ok {
		t.Fatal(err)
	}
	second, ok, err := source.Claim(context.Background(), "orchestrator", "replica-a", base.Add(2*time.Second), time.Minute)
	if err != nil || !ok || second.ClaimVersion <= first.ClaimVersion {
		t.Fatalf("second claim = %+v/%t/%v", second, ok, err)
	}
	if err := source.Ack(context.Background(), first, base.Add(3*time.Second)); !errors.Is(err, ErrNotClaimed) {
		t.Fatalf("same-owner stale ack = %v", err)
	}
	failure := Failure{Class: FailureIndeterminate, Code: "handler_unclassified", FailedAt: base.Add(3 * time.Second)}
	if err := source.Fail(context.Background(), second, failure); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := source.Claim(context.Background(), "orchestrator", "replica-b", base.Add(4*time.Second), time.Minute); ok || !errors.Is(err, ErrBlocked) {
		t.Fatalf("blocked claim = %t/%v", ok, err)
	}
	if err := source.retryBlocked("orchestrator", 1, second.ClaimVersion); err != nil {
		t.Fatal(err)
	}
	retried, ok, err := source.Claim(context.Background(), "orchestrator", "replica-b", base.Add(5*time.Second), time.Minute)
	if err != nil || !ok || retried.Sequence != 1 {
		t.Fatalf("operator retry claim = %+v/%t/%v", retried, ok, err)
	}
}

func TestConsumerExitsRetryBudgetWithoutPermanentlyBlockingCheckpoint(t *testing.T) {
	base := time.Unix(2_000_000_000, 0).UTC()
	clock := base
	source, _ := NewMemorySource([]Event{{
		Sequence: 1, EventID: "event-1", AccountID: 1, EventType: "runtime",
		DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: base,
	}})
	var calls []string
	handler := retryableRecordingHandler{
		calls: &calls, now: func() time.Time { return clock }, err: errors.New("runtime database unavailable"),
	}
	consumer, err := NewConsumer(
		source, handler, "orchestrator", "replica-a", time.Minute, func() time.Time { return clock }, 2,
	)
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := consumer.RunOnce(context.Background()); !processed || err == nil || errors.Is(err, ErrBlocked) {
		t.Fatalf("first retry = processed %t, err %v", processed, err)
	}
	clock = clock.Add(time.Second)
	if processed, err := consumer.RunOnce(context.Background()); !processed || !errors.Is(err, ErrRetryBudgetExceeded) {
		t.Fatalf("exhausted retry = processed %t, err %v", processed, err)
	}
	if len(calls) != 2 {
		t.Fatalf("handler calls = %d", len(calls))
	}
	if replay, claimed, err := source.Claim(context.Background(), "orchestrator", "replica-b", clock.Add(time.Second), time.Minute); !claimed || err != nil || replay.Sequence != 1 {
		t.Fatalf("recoverable retry = claim %+v, claimed %t, err %v", replay, claimed, err)
	}
}

func TestConsumerCanPersistBlockForMalformedClaimedEvent(t *testing.T) {
	base := time.Unix(2_000_000_000, 0).UTC()
	source := &poisonClaimSource{claim: ClaimedEvent{
		Event: Event{
			Sequence: 1, EventID: "event-1", AccountID: 1, EventType: "runtime",
			DesiredGeneration: 1, PayloadJSON: []byte(`not-json`), CreatedAt: base,
		},
		ConsumerName: "orchestrator", Owner: "replica-a", ClaimVersion: 1,
		LeaseExpiresAt: base.Add(time.Minute),
	}}
	consumer, err := NewConsumer(source, &testHandler{}, "orchestrator", "replica-a", time.Minute, func() time.Time { return base })
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := consumer.RunOnce(context.Background()); !processed || !errors.Is(err, ErrBlocked) {
		t.Fatalf("poison event = processed %t, err %v", processed, err)
	}
	if source.failed.Class != FailureSecurity || source.failed.Code != "invalid_event" || source.acks != 0 {
		t.Fatalf("persisted failure = %+v, acks=%d", source.failed, source.acks)
	}
}

func TestEventRejectsSensitivePayload(t *testing.T) {
	event := Event{Sequence: 1, EventID: "event", AccountID: 1, EventType: "account.runtime.provision_requested", DesiredGeneration: 1, PayloadJSON: []byte(`{"credentials":{"access_token":"sk-secret"}}`), CreatedAt: time.Now()}
	if err := event.Validate(); err == nil {
		t.Fatal("sensitive event payload was accepted")
	}
}

func TestEventRejectsJSONNullPayload(t *testing.T) {
	event := Event{
		Sequence: 1, EventID: "event", AccountID: 1, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`null`), CreatedAt: time.Now(),
	}
	if err := event.Validate(); err == nil {
		t.Fatal("JSON null payload was accepted as an object")
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

type poisonClaimSource struct {
	claim  ClaimedEvent
	failed Failure
	acks   int
}

func (s *poisonClaimSource) Claim(context.Context, string, string, time.Time, time.Duration) (ClaimedEvent, bool, error) {
	return s.claim, true, nil
}

func (s *poisonClaimSource) Ack(context.Context, ClaimedEvent, time.Time) error {
	s.acks++
	return nil
}

func (s *poisonClaimSource) Fail(_ context.Context, claim ClaimedEvent, failure Failure) error {
	if claim.ValidateToken() != nil || failure.Validate() != nil {
		return errors.New("invalid poison failure")
	}
	s.failed = failure
	return nil
}
