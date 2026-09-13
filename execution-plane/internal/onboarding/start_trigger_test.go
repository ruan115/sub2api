package onboarding

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOnboardingStartTriggerQueueStateValidation(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	trigger := validOnboardingStartTrigger(now)
	if err := trigger.Validate(); err != nil {
		t.Fatal(err)
	}
	claimExpiry := now.Add(time.Minute)
	trigger.Status = StartTriggerClaimed
	trigger.ClaimOwner = "starter-1"
	trigger.ClaimVersion = 1
	trigger.ClaimExpiresAt = &claimExpiry
	if err := trigger.Validate(); err != nil {
		t.Fatalf("claimed trigger: %v", err)
	}
	trigger.Status = StartTriggerExpired
	trigger.ClaimOwner = ""
	trigger.ClaimExpiresAt = nil
	if err := trigger.Validate(); err != nil {
		t.Fatalf("expired trigger: %v", err)
	}
	trigger.Status = StartTriggerStarted
	if err := trigger.Validate(); !errors.Is(err, ErrStartTriggerRejected) {
		t.Fatalf("started without claim/workflow error = %v", err)
	}
}

func TestStartTriggerClaimAndRetryRequireMicrosecondFencing(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	claim := StartTriggerClaimQuery{Owner: "starter-1", ClaimedAt: now, ClaimTTL: time.Minute, Limit: 10}
	if err := claim.Validate(); err != nil {
		t.Fatal(err)
	}
	claim.ClaimTTL = time.Minute + time.Nanosecond
	if err := claim.Validate(); !errors.Is(err, ErrStartTriggerRejected) {
		t.Fatalf("sub-microsecond claim expiry error = %v", err)
	}
	retry := StartTriggerRetry{
		EventID: "event-1", Owner: "starter-1", ClaimVersion: 1,
		RetriedAt: now, NextAttemptAt: now.Add(time.Second), ErrorCode: "slot_not_healthy",
	}
	if err := retry.Validate(); err != nil {
		t.Fatal(err)
	}
	retry.NextAttemptAt = now.Add(-time.Second)
	if err := retry.Validate(); !errors.Is(err, ErrStartTriggerRejected) {
		t.Fatalf("backwards retry error = %v", err)
	}
}

func validOnboardingStartTrigger(now time.Time) OnboardingStartTrigger {
	return OnboardingStartTrigger{
		StartTriggerProjection: StartTriggerProjection{
			SourceSequence: 1, EventID: "event-1", EventType: "account.runtime.provision_requested",
			EventCreatedAt: now.Add(-time.Second), IntentID: "11111111-2222-4333-8444-555555555555",
			AccountID: "10380", DesiredGeneration: 1, SlotID: "slot-10380", Provider: "docker",
			ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500,
			MemoryRequestBytes: 256 << 20, ProjectedAt: now,
		},
		IntentExpiresAt: now.Add(10 * time.Minute), ReservationID: "reservation-10380",
		BindingRevision: 1, Status: StartTriggerPending, NextAttemptAt: now,
	}
}
