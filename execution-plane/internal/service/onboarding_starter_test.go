package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

type recordingHealthySlotStartRepository struct {
	mu        sync.Mutex
	workflows map[string]onboarding.Provisioning
	specs     []onboarding.HealthySlotStartSpec
	err       error
	creates   atomic.Int32
}

func (r *recordingHealthySlotStartRepository) StartHealthySlotOnboarding(
	_ context.Context,
	spec onboarding.HealthySlotStartSpec,
) (onboarding.Provisioning, bool, error) {
	if r.err != nil {
		return onboarding.Provisioning{}, false, r.err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.specs = append(r.specs, spec)
	if existing, ok := r.workflows[spec.IntentID]; ok {
		return existing, false, nil
	}
	workflow := onboarding.Provisioning{
		ID: spec.WorkflowID, IdempotencyKey: spec.IdempotencyKey, IntentID: spec.IntentID, Owner: spec.Owner,
		AccountID: "account-10380", DesiredGeneration: 7, NodeID: "srv74", SlotID: spec.SlotID,
		ExecutionEpoch: 19, ImageDigest: "sha256:" + strings.Repeat("a", 64),
		CredentialLeaseID: spec.CredentialLeaseID, ProxyLeaseID: spec.ProxyLeaseID,
		KeyCommandID: spec.KeyCommandID, ActivationCommandID: spec.ActivationCommandID,
		CommandDeadline: spec.RequestedCommandDeadline, Status: onboarding.ProvisioningPendingKey,
		CreatedAt: spec.StartedAt, UpdatedAt: spec.StartedAt,
	}
	r.workflows[spec.IntentID] = workflow
	r.creates.Add(1)
	return workflow, true, nil
}

func TestHealthySlotOnboardingStarterDerivesStableSecretFreeIdentity(t *testing.T) {
	clock := time.Unix(2_000_000_000, 987_654_321).UTC()
	repository := &recordingHealthySlotStartRepository{workflows: make(map[string]onboarding.Provisioning)}
	starter, err := NewHealthySlotOnboardingStarter(repository, HealthySlotOnboardingStarterConfig{
		Now: func() time.Time { return clock }, ObservationMaxAge: 30 * time.Second, CommandTTL: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := HealthySlotOnboardingStartRequest{
		Trigger: testHealthySlotOnboardingStartTrigger(clock, onboarding.StartTriggerClaimed),
	}
	first, created, err := starter.Start(context.Background(), request)
	if err != nil || !created {
		t.Fatalf("first start = %+v/%t/%v", first, created, err)
	}
	clock = clock.Add(time.Minute)
	replayed, created, err := starter.Start(context.Background(), request)
	if err != nil || created || replayed.ID != first.ID {
		t.Fatalf("replayed start = %+v/%t/%v", replayed, created, err)
	}
	if repository.creates.Load() != 1 || len(repository.specs) != 2 {
		t.Fatalf("creates/specs = %d/%d", repository.creates.Load(), len(repository.specs))
	}
	firstSpec, replaySpec := repository.specs[0], repository.specs[1]
	if firstSpec.TriggerEventID != request.Trigger.EventID || firstSpec.TriggerClaimOwner != request.Trigger.ClaimOwner ||
		firstSpec.TriggerClaimVersion != request.Trigger.ClaimVersion || firstSpec.IntentID != request.Trigger.IntentID ||
		firstSpec.SlotID != request.Trigger.SlotID || firstSpec.ReservationID != request.Trigger.ReservationID ||
		firstSpec.BindingRevision != request.Trigger.BindingRevision {
		t.Fatalf("trigger fields were not preserved: trigger=%+v spec=%+v", request.Trigger, firstSpec)
	}
	if firstSpec.WorkflowID != replaySpec.WorkflowID || firstSpec.IdempotencyKey != replaySpec.IdempotencyKey ||
		firstSpec.CredentialLeaseID != replaySpec.CredentialLeaseID || firstSpec.ProxyLeaseID != replaySpec.ProxyLeaseID ||
		firstSpec.KeyCommandID != replaySpec.KeyCommandID || firstSpec.ActivationCommandID != replaySpec.ActivationCommandID ||
		firstSpec.Owner != firstSpec.WorkflowID ||
		!firstSpec.ObservationFreshAfter.Equal(firstSpec.StartedAt.Add(-30*time.Second)) {
		t.Fatalf("unstable starter identity/bounds: first=%+v replay=%+v", firstSpec, replaySpec)
	}
	if err := firstSpec.Validate(); err != nil {
		t.Fatalf("derived start spec is invalid: %v", err)
	}
	if firstSpec.StartedAt.Nanosecond()%1000 != 0 {
		t.Fatalf("starter time was not canonicalized to DATETIME(6): %s", firstSpec.StartedAt)
	}
}

func TestHealthySlotOnboardingStarterConcurrentCallsHaveOneLogicalCreate(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := &recordingHealthySlotStartRepository{workflows: make(map[string]onboarding.Provisioning)}
	starter, _ := NewHealthySlotOnboardingStarter(repository, HealthySlotOnboardingStarterConfig{
		Now: func() time.Time { return now }, ObservationMaxAge: time.Minute, CommandTTL: 5 * time.Minute,
	})
	request := HealthySlotOnboardingStartRequest{
		Trigger: testHealthySlotOnboardingStartTrigger(now, onboarding.StartTriggerClaimed),
	}
	var wait sync.WaitGroup
	var created atomic.Int32
	var failed atomic.Int32
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, wasCreated, err := starter.Start(context.Background(), request)
			if err != nil {
				failed.Add(1)
			}
			if wasCreated {
				created.Add(1)
			}
		}()
	}
	wait.Wait()
	if failed.Load() != 0 || created.Load() != 1 || repository.creates.Load() != 1 {
		t.Fatalf("failed/created/repository creates = %d/%d/%d", failed.Load(), created.Load(), repository.creates.Load())
	}
}

func TestHealthySlotOnboardingStarterRejectsInvalidInputAndPreservesRepositoryFailure(t *testing.T) {
	if _, err := NewHealthySlotOnboardingStarter(nil, HealthySlotOnboardingStarterConfig{}); !errors.Is(err, ErrHealthySlotOnboardingStart) {
		t.Fatalf("invalid config error = %v", err)
	}
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := &recordingHealthySlotStartRepository{workflows: make(map[string]onboarding.Provisioning)}
	starter, _ := NewHealthySlotOnboardingStarter(repository, HealthySlotOnboardingStarterConfig{
		Now:               func() time.Time { return now },
		ObservationMaxAge: time.Minute, CommandTTL: time.Minute,
	})
	if _, _, err := starter.Start(context.Background(), HealthySlotOnboardingStartRequest{}); !errors.Is(err, ErrHealthySlotOnboardingStart) {
		t.Fatalf("invalid request error = %v", err)
	}
	pendingTrigger := testHealthySlotOnboardingStartTrigger(now, onboarding.StartTriggerPending)
	if err := pendingTrigger.Validate(); err != nil {
		t.Fatalf("pending trigger fixture is invalid: %v", err)
	}
	if _, _, err := starter.Start(context.Background(), HealthySlotOnboardingStartRequest{Trigger: pendingTrigger}); !errors.Is(err, ErrHealthySlotOnboardingStart) {
		t.Fatalf("pending trigger error = %v", err)
	}
	if len(repository.specs) != 0 {
		t.Fatalf("repository received %d invalid requests", len(repository.specs))
	}

	repositoryFailure := errors.New("repository start failed")
	repository.err = repositoryFailure
	for _, status := range []string{onboarding.StartTriggerClaimed, onboarding.StartTriggerStarted} {
		t.Run(status, func(t *testing.T) {
			trigger := testHealthySlotOnboardingStartTrigger(now, status)
			if err := trigger.Validate(); err != nil {
				t.Fatalf("%s trigger fixture is invalid: %v", status, err)
			}
			_, _, err := starter.Start(context.Background(), HealthySlotOnboardingStartRequest{Trigger: trigger})
			if !errors.Is(err, ErrHealthySlotOnboardingStart) || !errors.Is(err, repositoryFailure) {
				t.Fatalf("%s repository failure = %v", status, err)
			}
		})
	}
}

func testHealthySlotOnboardingStartTrigger(now time.Time, status string) onboarding.OnboardingStartTrigger {
	now = now.UTC().Truncate(time.Microsecond)
	trigger := onboarding.OnboardingStartTrigger{
		StartTriggerProjection: onboarding.StartTriggerProjection{
			SourceSequence: 10380, EventID: "event-10380", EventType: "account.runtime.provision_requested",
			EventCreatedAt: now.Add(-time.Second), IntentID: "11111111-2222-4333-8444-555555555555",
			AccountID: "account-10380", DesiredGeneration: 7, SlotID: "slot-10380", Provider: "docker",
			ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500,
			MemoryRequestBytes: 256 << 20, ProjectedAt: now,
		},
		IntentExpiresAt: now.Add(10 * time.Minute), ReservationID: "reservation-10380",
		BindingRevision: 7, Status: status, NextAttemptAt: now,
	}
	if status == onboarding.StartTriggerClaimed || status == onboarding.StartTriggerStarted {
		claimExpiresAt := now.Add(5 * time.Minute)
		trigger.ClaimOwner = "starter-1"
		trigger.ClaimVersion = 3
		trigger.ClaimExpiresAt = &claimExpiresAt
	}
	if status == onboarding.StartTriggerStarted {
		startedAt := now
		trigger.StartedWorkflowID = healthySlotStarterID("workflow", trigger.IntentID)
		trigger.StartedAt = &startedAt
	}
	return trigger
}

var _ onboarding.HealthySlotStartRepository = (*recordingHealthySlotStartRepository)(nil)
