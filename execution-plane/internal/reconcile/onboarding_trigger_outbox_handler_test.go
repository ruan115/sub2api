package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
)

type recordingStartTriggerRepository struct {
	projection onboarding.StartTriggerProjection
	calls      int
	err        error
	lookup     onboarding.OnboardingStartTrigger
	found      bool
	lookupErr  error
}

func (r *recordingStartTriggerRepository) LookupOnboardingStartTrigger(
	_ context.Context,
	_ string,
) (onboarding.OnboardingStartTrigger, bool, error) {
	return r.lookup, r.found, r.lookupErr
}

func (r *recordingStartTriggerRepository) ProjectOnboardingStartTrigger(_ context.Context, projection onboarding.StartTriggerProjection) (onboarding.OnboardingStartTrigger, bool, error) {
	r.calls++
	r.projection = projection
	return onboarding.OnboardingStartTrigger{StartTriggerProjection: projection}, true, r.err
}

func (r *recordingStartTriggerRepository) ClaimDueOnboardingStartTriggers(context.Context, onboarding.StartTriggerClaimQuery) ([]onboarding.OnboardingStartTrigger, error) {
	return nil, onboarding.ErrStartTriggerRejected
}

func (r *recordingStartTriggerRepository) RetryOnboardingStartTrigger(context.Context, onboarding.StartTriggerRetry) error {
	return onboarding.ErrStartTriggerRejected
}

func TestOnboardingTriggerOutboxHandlerProjectsExactCommittedIntent(t *testing.T) {
	eventTime := time.Unix(2_000_000_000, 0).UTC()
	projectedAt := eventTime.Add(time.Second)
	source := &runtimeDesiredSource{desired: AccountRuntimeDesired{
		AccountID: 10380, SlotID: "ccmax-account-10380", Provider: "docker", DesiredGeneration: 7,
		RequiredLabels: map[string]string{"region": "ap-shanghai"},
		ImageDigest:    "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500, MemoryRequestBytes: 128 << 20,
	}}
	repository := &recordingStartTriggerRepository{}
	handler, err := NewOnboardingTriggerOutboxHandler(source, repository, func() time.Time { return projectedAt })
	if err != nil {
		t.Fatal(err)
	}
	event := outbox.Event{
		Sequence: 91, EventID: "event-91", AccountID: 10380,
		EventType: "account.runtime.provision_requested", DesiredGeneration: 7,
		PayloadJSON: []byte(`{"onboarding_intent_id":"11111111-2222-4333-8444-555555555555"}`), CreatedAt: eventTime,
	}
	if err := handler.ApplyRuntimeEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	projection := repository.projection
	if repository.calls != 1 || projection.SourceSequence != event.Sequence || projection.EventID != event.EventID ||
		projection.IntentID != "11111111-2222-4333-8444-555555555555" || projection.AccountID != "10380" ||
		projection.DesiredGeneration != 7 || projection.SlotID != "ccmax-account-10380" ||
		!projection.ProjectedAt.Equal(eventTime) {
		t.Fatalf("projection = %+v calls=%d", projection, repository.calls)
	}
}

func TestOnboardingTriggerOutboxHandlerReplaysBeforeMutableAccountLookup(t *testing.T) {
	eventTime := time.Unix(2_000_000_000, 0).UTC()
	projectedAt := eventTime.Add(time.Second)
	intentID := "11111111-2222-4333-8444-555555555555"
	event := outbox.Event{
		Sequence: 91, EventID: "event-91", AccountID: 10380,
		EventType: "account.runtime.provision_requested", DesiredGeneration: 7,
		PayloadJSON: []byte(`{"onboarding_intent_id":"` + intentID + `"}`), CreatedAt: eventTime,
	}
	trigger := onboarding.OnboardingStartTrigger{
		StartTriggerProjection: onboarding.StartTriggerProjection{
			SourceSequence: event.Sequence, EventID: event.EventID, EventType: event.EventType,
			EventCreatedAt: eventTime, IntentID: intentID, AccountID: "10380", DesiredGeneration: 7,
			SlotID: "ccmax-account-10380", Provider: "docker",
			ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500,
			MemoryRequestBytes: 128 << 20, ProjectedAt: projectedAt,
		},
		IntentExpiresAt: projectedAt.Add(10 * time.Minute), ReservationID: "reservation-10380",
		BindingRevision: 9, Status: onboarding.StartTriggerPending, NextAttemptAt: projectedAt,
	}
	source := &runtimeDesiredSource{err: errors.New("mutable account was deleted")}
	repository := &recordingStartTriggerRepository{lookup: trigger, found: true}
	handler, err := NewOnboardingTriggerOutboxHandler(source, repository, func() time.Time {
		return projectedAt.Add(time.Hour)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.ApplyRuntimeEvent(context.Background(), event); err != nil {
		t.Fatalf("durable replay = %v", err)
	}
	if source.calls != 0 || repository.calls != 0 {
		t.Fatalf("durable replay consulted mutable source=%d or projected=%d", source.calls, repository.calls)
	}
}

func TestOnboardingTriggerOutboxHandlerRejectsBeforeProjection(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	source := &runtimeDesiredSource{desired: AccountRuntimeDesired{
		AccountID: 7, SlotID: "ccmax-account-7", Provider: "docker", DesiredGeneration: 1,
		ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500, MemoryRequestBytes: 128 << 20,
	}}
	repository := &recordingStartTriggerRepository{}
	handler, _ := NewOnboardingTriggerOutboxHandler(source, repository, func() time.Time { return now })
	base := outbox.Event{
		Sequence: 1, EventID: "event-1", AccountID: 7,
		EventType: "account.runtime.provision_requested", DesiredGeneration: 1,
		PayloadJSON: []byte(`{"onboarding_intent_id":"intent-1"}`), CreatedAt: now,
	}
	for _, mutate := range []func(*outbox.Event){
		func(event *outbox.Event) {
			event.PayloadJSON = []byte(`{"onboarding_intent_id":"intent-1","extra":true}`)
		},
		func(event *outbox.Event) { event.EventType = "account.runtime.restore_requested" },
		func(event *outbox.Event) { event.CreatedAt = time.Time{} },
	} {
		event := base
		mutate(&event)
		if err := handler.ApplyRuntimeEvent(context.Background(), event); !errors.Is(err, ErrOnboardingRuntimeEvent) {
			t.Fatalf("invalid event error = %v", err)
		}
	}
	if repository.calls != 0 {
		t.Fatalf("invalid events projected %d triggers", repository.calls)
	}
}
