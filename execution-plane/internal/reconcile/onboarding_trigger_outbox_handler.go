package reconcile

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
)

type OnboardingTriggerOutboxHandler struct {
	source     AccountRuntimeSource
	repository onboarding.OnboardingStartTriggerRepository
	now        func() time.Time
}

func NewOnboardingTriggerOutboxHandler(
	source AccountRuntimeSource,
	repository onboarding.OnboardingStartTriggerRepository,
	now func() time.Time,
) (*OnboardingTriggerOutboxHandler, error) {
	if source == nil || repository == nil {
		return nil, ErrOnboardingRuntimeEvent
	}
	if now == nil {
		now = time.Now
	}
	return &OnboardingTriggerOutboxHandler{source: source, repository: repository, now: now}, nil
}

// ApplyRuntimeEvent projects the ready desired slot and exact committed intent
// trigger in one worker_runtime transaction. Slot health is intentionally not
// consulted here: waiting for Docker must not block the global CCMAX outbox.
func (h *OnboardingTriggerOutboxHandler) ApplyRuntimeEvent(ctx context.Context, event outbox.Event) error {
	if h == nil || h.source == nil || h.repository == nil || h.now == nil || ctx == nil || ctx.Err() != nil ||
		event.Validate() != nil || !IsOnboardingRuntimeEvent(event.EventType) {
		return outbox.BlockingHandlerError(outbox.FailureIntegrity, "onboarding_event_invalid", ErrOnboardingRuntimeEvent)
	}
	payload, err := DecodeOnboardingRuntimePayload(event.PayloadJSON)
	if err != nil {
		return outbox.BlockingHandlerError(outbox.FailureTerminal, "onboarding_payload_schema_invalid", ErrOnboardingRuntimeEvent)
	}
	// Durable replay must precede every mutable CCMAX/account/policy lookup.
	// Otherwise an Ack loss followed by account generation advancement could
	// make an already-committed trigger impossible to acknowledge forever.
	existing, found, err := h.repository.LookupOnboardingStartTrigger(ctx, event.EventID)
	if err != nil {
		wrapped := fmt.Errorf("%w: lookup durable start trigger: %w", ErrOnboardingRuntimeEvent, err)
		if errors.Is(err, onboarding.ErrStartTriggerRejected) {
			return outbox.BlockingHandlerError(outbox.FailureIntegrity, "onboarding_replay_conflict", wrapped)
		}
		return retryRuntimeEvent(h.now, "onboarding_trigger_lookup_unavailable", wrapped)
	}
	if found {
		if !sameOnboardingTriggerEvent(existing, event, payload.IntentID) {
			return outbox.BlockingHandlerError(outbox.FailureIntegrity, "onboarding_replay_conflict", ErrOnboardingRuntimeEvent)
		}
		return nil
	}
	desired, err := h.source.LoadAccountRuntimeDesired(ctx, event.AccountID, event.DesiredGeneration)
	if err != nil {
		wrapped := fmt.Errorf("%w: load CCMAX runtime desired state: %w", ErrOnboardingRuntimeEvent, err)
		if errors.Is(err, ErrCCMAXRuntimeDesired) {
			return outbox.BlockingHandlerError(outbox.FailureIntegrity, "ccmax_runtime_identity_invalid", wrapped)
		}
		return retryRuntimeEvent(h.now, "ccmax_runtime_source_unavailable", wrapped)
	}
	if desired.AccountID != event.AccountID || desired.DesiredGeneration != event.DesiredGeneration ||
		desired.SlotID == "" || desired.Provider != "docker" {
		return outbox.BlockingHandlerError(outbox.FailureIntegrity, "ccmax_runtime_identity_mismatch", ErrOnboardingRuntimeEvent)
	}
	eventCreatedAt := event.CreatedAt.UTC().Truncate(time.Microsecond)
	if eventCreatedAt.IsZero() {
		return outbox.BlockingHandlerError(outbox.FailureIntegrity, "onboarding_event_time_invalid", ErrOnboardingRuntimeEvent)
	}
	// Authorization is anchored to the committed CCMAX event time, not replay
	// wall-clock time. A backlog may be consumed after the intent expires; the
	// trigger is still projected and the trigger queue expires it independently.
	projectedAt := eventCreatedAt
	_, _, err = h.repository.ProjectOnboardingStartTrigger(ctx, onboarding.StartTriggerProjection{
		SourceSequence: event.Sequence, EventID: event.EventID, EventType: event.EventType,
		EventCreatedAt: eventCreatedAt, IntentID: payload.IntentID,
		AccountID: strconv.FormatInt(event.AccountID, 10), DesiredGeneration: event.DesiredGeneration,
		SlotID: desired.SlotID, Provider: desired.Provider, RequiredLabels: desired.RequiredLabels,
		ImageDigest: desired.ImageDigest, CPURequestMillis: desired.CPURequestMillis,
		MemoryRequestBytes: desired.MemoryRequestBytes, ProjectedAt: projectedAt,
	})
	if err != nil {
		if errors.Is(err, onboarding.ErrStartTriggerRejected) {
			return outbox.BlockingHandlerError(outbox.FailureIntegrity, "onboarding_projection_rejected", ErrOnboardingRuntimeEvent)
		}
		return retryRuntimeEvent(
			h.now,
			"onboarding_projection_unavailable",
			fmt.Errorf("%w: project durable start trigger: %w", ErrOnboardingRuntimeEvent, err),
		)
	}
	return nil
}

func sameOnboardingTriggerEvent(
	trigger onboarding.OnboardingStartTrigger,
	event outbox.Event,
	intentID string,
) bool {
	return trigger.Validate() == nil && trigger.SourceSequence == event.Sequence &&
		trigger.EventID == event.EventID && trigger.EventType == event.EventType &&
		trigger.EventCreatedAt.Equal(event.CreatedAt.UTC().Truncate(time.Microsecond)) &&
		trigger.IntentID == intentID && trigger.AccountID == strconv.FormatInt(event.AccountID, 10) &&
		trigger.DesiredGeneration == event.DesiredGeneration
}

var _ outbox.Handler = (*OnboardingTriggerOutboxHandler)(nil)
