package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

type startCoordinatorRepository struct {
	mu         sync.Mutex
	claimed    []onboarding.OnboardingStartTrigger
	claimErr   error
	claimCalls int
	claimFn    func(int) ([]onboarding.OnboardingStartTrigger, error)
	retries    []onboarding.StartTriggerRetry
	retryErr   error
}

func (r *startCoordinatorRepository) LookupOnboardingStartTrigger(context.Context, string) (onboarding.OnboardingStartTrigger, bool, error) {
	return onboarding.OnboardingStartTrigger{}, false, nil
}

func (r *startCoordinatorRepository) ProjectOnboardingStartTrigger(context.Context, onboarding.StartTriggerProjection) (onboarding.OnboardingStartTrigger, bool, error) {
	return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
}

func (r *startCoordinatorRepository) ClaimDueOnboardingStartTriggers(_ context.Context, _ onboarding.StartTriggerClaimQuery) ([]onboarding.OnboardingStartTrigger, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.claimCalls++
	if r.claimFn != nil {
		claimed, err := r.claimFn(r.claimCalls)
		return append([]onboarding.OnboardingStartTrigger(nil), claimed...), err
	}
	return append([]onboarding.OnboardingStartTrigger(nil), r.claimed...), r.claimErr
}

func (r *startCoordinatorRepository) RetryOnboardingStartTrigger(_ context.Context, retry onboarding.StartTriggerRetry) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.retries = append(r.retries, retry)
	return r.retryErr
}

type startCoordinatorStarter struct {
	results map[string]struct {
		created bool
		err     error
	}
}

func (s *startCoordinatorStarter) Start(_ context.Context, request HealthySlotOnboardingStartRequest) (onboarding.Provisioning, bool, error) {
	result := s.results[request.Trigger.EventID]
	return onboarding.Provisioning{ID: "workflow-" + request.Trigger.EventID}, result.created, result.err
}

func TestOnboardingStartCoordinatorStartsAndQueuesClaimedTriggersIndependently(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := &startCoordinatorRepository{claimed: []onboarding.OnboardingStartTrigger{
		coordinatorTrigger("event-created", now, 1),
		coordinatorTrigger("event-replayed", now, 2),
		coordinatorTrigger("event-queued", now, 3),
	}}
	starter := &startCoordinatorStarter{results: map[string]struct {
		created bool
		err     error
	}{
		"event-created":  {created: true},
		"event-replayed": {created: false},
		"event-queued":   {err: errors.Join(ErrHealthySlotOnboardingStart, onboarding.ErrHealthySlotStartRejected)},
	}}
	coordinator, err := NewOnboardingStartCoordinator(repository, starter, OnboardingStartCoordinatorConfig{
		Owner: "coordinator-a", PollInterval: time.Second, BatchSize: 200,
		ClaimTTL: 30 * time.Second, RetryDelay: 2 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.Step(context.Background())
	if err != nil || result.Claimed != 3 || result.Started != 2 || result.Created != 1 ||
		result.Replayed != 1 || result.Queued != 1 || result.Failed != 0 {
		t.Fatalf("Step() = %+v, %v", result, err)
	}
	if len(repository.retries) != 1 || repository.retries[0].EventID != "event-queued" ||
		repository.retries[0].ClaimVersion != 3 || !repository.retries[0].NextAttemptAt.Equal(now.Add(2*time.Second)) {
		t.Fatalf("retries = %+v", repository.retries)
	}
}

func TestOnboardingStartCoordinatorFencesClaimAndRepositoryFailures(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := &startCoordinatorRepository{claimErr: errors.New("database unavailable")}
	starter := &startCoordinatorStarter{results: make(map[string]struct {
		created bool
		err     error
	})}
	var errorsSeen int
	coordinator, _ := NewOnboardingStartCoordinator(repository, starter, OnboardingStartCoordinatorConfig{
		Owner: "coordinator-a", Now: func() time.Time { return now }, OnError: func(string, error) { errorsSeen++ },
	})
	if _, err := coordinator.Step(context.Background()); !errors.Is(err, ErrOnboardingStartCoordinate) || errorsSeen != 1 {
		t.Fatalf("claim failure = %v errors=%d", err, errorsSeen)
	}
	repository.claimErr = nil
	repository.claimed = []onboarding.OnboardingStartTrigger{coordinatorTrigger("event-1", now, 1)}
	repository.retryErr = onboarding.ErrStartTriggerClaimLost
	starter.results["event-1"] = struct {
		created bool
		err     error
	}{err: onboarding.ErrHealthySlotStartRejected}
	result, err := coordinator.Step(context.Background())
	if err != nil || result.Failed != 1 || result.Queued != 0 || errorsSeen != 2 {
		t.Fatalf("retry fence = %+v err=%v errors=%d", result, err, errorsSeen)
	}
	if _, err := NewOnboardingStartCoordinator(repository, starter, OnboardingStartCoordinatorConfig{}); !errors.Is(err, ErrOnboardingStartCoordinate) {
		t.Fatalf("missing owner error = %v", err)
	}
}

func TestOnboardingStartCoordinatorStopsCleanly(t *testing.T) {
	repository := &startCoordinatorRepository{}
	starter := &startCoordinatorStarter{results: make(map[string]struct {
		created bool
		err     error
	})}
	coordinator, _ := NewOnboardingStartCoordinator(repository, starter, OnboardingStartCoordinatorConfig{
		Owner: "coordinator-a", PollInterval: time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- coordinator.Run(ctx) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator did not stop")
	}
}

func TestOnboardingStartCoordinatorRunStopsAfterConsecutiveScanFailures(t *testing.T) {
	repository := &startCoordinatorRepository{claimErr: errors.New("database unavailable")}
	starter := &startCoordinatorStarter{results: make(map[string]struct {
		created bool
		err     error
	})}
	coordinator, err := NewOnboardingStartCoordinator(repository, starter, OnboardingStartCoordinatorConfig{
		Owner: "coordinator-a", PollInterval: time.Millisecond, MaxConsecutiveFailures: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- coordinator.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrOnboardingStartCoordinate) {
			t.Fatalf("coordinator run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator kept serving after persistent scan failure")
	}
}

func TestOnboardingStartCoordinatorRunStopsAfterPersistentStarterFailures(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	trigger := coordinatorTrigger("event-internal-failure", now, 1)
	repository := &startCoordinatorRepository{claimFn: func(call int) ([]onboarding.OnboardingStartTrigger, error) {
		// Model RetryDelay > PollInterval: a failed attempt is followed by a
		// healthy empty poll before the same durable trigger becomes due.
		if call%2 == 0 {
			return nil, nil
		}
		return []onboarding.OnboardingStartTrigger{trigger}, nil
	}}
	starter := &startCoordinatorStarter{results: map[string]struct {
		created bool
		err     error
	}{
		"event-internal-failure": {err: errors.New("persistent starter database failure")},
	}}
	var errorsSeen int
	coordinator, err := NewOnboardingStartCoordinator(repository, starter, OnboardingStartCoordinatorConfig{
		Owner: "coordinator-a", PollInterval: time.Millisecond, MaxConsecutiveFailures: 2,
		Now: func() time.Time { return now }, OnError: func(string, error) { errorsSeen++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- coordinator.Run(context.Background()) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrOnboardingStartCoordinate) ||
			!errors.Is(err, ErrHealthySlotOnboardingStart) {
			t.Fatalf("coordinator run error = %v", err)
		}
		if errorsSeen != 2 || len(repository.retries) != 2 {
			t.Fatalf("persistent starter failures errors=%d retries=%d", errorsSeen, len(repository.retries))
		}
	case <-time.After(time.Second):
		t.Fatal("coordinator kept serving after persistent starter failure")
	}
}

func coordinatorTrigger(eventID string, now time.Time, claimVersion uint64) onboarding.OnboardingStartTrigger {
	claimExpiresAt := now.Add(time.Minute)
	return onboarding.OnboardingStartTrigger{
		StartTriggerProjection: onboarding.StartTriggerProjection{
			SourceSequence: int64(claimVersion), EventID: eventID,
			EventType: "account.runtime.provision_requested", EventCreatedAt: now,
			IntentID:  "11111111-2222-4333-8444-" + strings.Repeat(string(rune('0'+claimVersion)), 12),
			AccountID: "10380", DesiredGeneration: claimVersion, SlotID: "ccmax-account-10380",
			Provider: "docker", ImageDigest: "sha256:" + strings.Repeat("a", 64),
			CPURequestMillis: 500, MemoryRequestBytes: 128 << 20, ProjectedAt: now,
		},
		IntentExpiresAt: now.Add(10 * time.Minute), ReservationID: "reservation-" + eventID,
		BindingRevision: claimVersion, Status: onboarding.StartTriggerClaimed,
		ClaimOwner: "coordinator-a", ClaimVersion: claimVersion, ClaimExpiresAt: &claimExpiresAt,
		NextAttemptAt: now,
	}
}
