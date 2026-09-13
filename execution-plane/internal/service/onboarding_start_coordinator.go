package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

const (
	defaultStartCoordinatorPollInterval = time.Second
	defaultStartCoordinatorBatchSize    = 200
	defaultStartCoordinatorClaimTTL     = 30 * time.Second
	defaultStartCoordinatorRetryDelay   = 2 * time.Second
	defaultStartCoordinatorMaxFailures  = 5
)

var ErrOnboardingStartCoordinate = errors.New("onboarding start coordinator failed")

type TriggeredOnboardingStarter interface {
	Start(ctx context.Context, request HealthySlotOnboardingStartRequest) (onboarding.Provisioning, bool, error)
}

type OnboardingStartCoordinatorConfig struct {
	Owner                  string
	PollInterval           time.Duration
	BatchSize              int
	ClaimTTL               time.Duration
	RetryDelay             time.Duration
	MaxConsecutiveFailures int
	OnError                func(eventID string, err error)
	Now                    func() time.Time
}

type OnboardingStartCoordinateResult struct {
	Claimed  int
	Started  int
	Created  int
	Replayed int
	Queued   int
	Failed   int
}

type OnboardingStartCoordinator struct {
	repository onboarding.OnboardingStartTriggerRepository
	starter    TriggeredOnboardingStarter
	config     OnboardingStartCoordinatorConfig
}

func NewOnboardingStartCoordinator(
	repository onboarding.OnboardingStartTriggerRepository,
	starter TriggeredOnboardingStarter,
	config OnboardingStartCoordinatorConfig,
) (*OnboardingStartCoordinator, error) {
	if repository == nil || starter == nil || credential.ValidateTransportID(config.Owner) != nil {
		return nil, ErrOnboardingStartCoordinate
	}
	if config.PollInterval == 0 {
		config.PollInterval = defaultStartCoordinatorPollInterval
	}
	if config.BatchSize == 0 {
		config.BatchSize = defaultStartCoordinatorBatchSize
	}
	if config.ClaimTTL == 0 {
		config.ClaimTTL = defaultStartCoordinatorClaimTTL
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = defaultStartCoordinatorRetryDelay
	}
	if config.MaxConsecutiveFailures == 0 {
		config.MaxConsecutiveFailures = defaultStartCoordinatorMaxFailures
	}
	if config.PollInterval <= 0 || config.PollInterval > time.Minute || config.BatchSize < 1 || config.BatchSize > 1000 ||
		config.ClaimTTL <= 0 || config.ClaimTTL > 24*time.Hour || config.RetryDelay <= 0 || config.RetryDelay > time.Hour ||
		config.MaxConsecutiveFailures < 1 || config.MaxConsecutiveFailures > 1000 {
		return nil, ErrOnboardingStartCoordinate
	}
	if config.OnError == nil {
		config.OnError = func(string, error) {}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &OnboardingStartCoordinator{repository: repository, starter: starter, config: config}, nil
}

// Step claims one bounded page in a short trigger-only transaction, then
// invokes the atomic starter sequentially. An unavailable healthy binding is
// queued with a fenced retry; it never blocks the global CCMAX outbox.
func (c *OnboardingStartCoordinator) Step(ctx context.Context) (OnboardingStartCoordinateResult, error) {
	if c == nil || c.repository == nil || c.starter == nil || ctx == nil || ctx.Err() != nil {
		return OnboardingStartCoordinateResult{}, ErrOnboardingStartCoordinate
	}
	claimedAt := canonicalCoordinatorTime(c.config.Now())
	claimed, err := c.repository.ClaimDueOnboardingStartTriggers(ctx, onboarding.StartTriggerClaimQuery{
		Owner: c.config.Owner, ClaimedAt: claimedAt, ClaimTTL: c.config.ClaimTTL, Limit: c.config.BatchSize,
	})
	if err != nil {
		c.config.OnError("", ErrOnboardingStartCoordinate)
		return OnboardingStartCoordinateResult{}, errors.Join(ErrOnboardingStartCoordinate, err)
	}
	result := OnboardingStartCoordinateResult{Claimed: len(claimed)}
	var pageErr error
	for _, trigger := range claimed {
		if ctx.Err() != nil {
			return result, ErrOnboardingStartCoordinate
		}
		if trigger.Validate() != nil || trigger.Status != onboarding.StartTriggerClaimed ||
			trigger.ClaimOwner != c.config.Owner {
			result.Failed++
			c.config.OnError("", ErrOnboardingStartCoordinate)
			pageErr = errors.Join(pageErr, ErrOnboardingStartCoordinate)
			continue
		}
		_, created, startErr := c.starter.Start(ctx, HealthySlotOnboardingStartRequest{Trigger: trigger})
		if startErr == nil {
			result.Started++
			if created {
				result.Created++
			} else {
				result.Replayed++
			}
			continue
		}
		retriedAt := canonicalCoordinatorTime(c.config.Now())
		nextAttemptAt := retriedAt.Add(c.config.RetryDelay).UTC().Truncate(time.Microsecond)
		if trigger.IntentExpiresAt.After(retriedAt) && nextAttemptAt.After(trigger.IntentExpiresAt) {
			nextAttemptAt = trigger.IntentExpiresAt
		}
		retryErr := c.repository.RetryOnboardingStartTrigger(ctx, onboarding.StartTriggerRetry{
			EventID: trigger.EventID, Owner: trigger.ClaimOwner, ClaimVersion: trigger.ClaimVersion,
			RetriedAt: retriedAt, NextAttemptAt: nextAttemptAt, ErrorCode: "runtime_binding_unavailable",
		})
		if retryErr != nil {
			result.Failed++
			c.config.OnError(trigger.EventID, ErrOnboardingStartCoordinate)
			if !errors.Is(retryErr, onboarding.ErrStartTriggerClaimLost) {
				pageErr = errors.Join(pageErr, ErrOnboardingStartCoordinate, retryErr)
			}
			continue
		}
		result.Queued++
		if !errors.Is(startErr, onboarding.ErrHealthySlotStartRejected) {
			c.config.OnError(trigger.EventID, ErrHealthySlotOnboardingStart)
			// A healthy-binding rejection is expected while slot state converges,
			// but a repository/transaction/internal starter failure is a service
			// failure even when the trigger was safely requeued. Surface only fixed
			// sentinels so the supervisor can apply its consecutive-failure budget
			// without leaking storage details through logs.
			pageErr = errors.Join(pageErr, ErrOnboardingStartCoordinate, ErrHealthySlotOnboardingStart)
		}
	}
	return result, pageErr
}

func (c *OnboardingStartCoordinator) Run(ctx context.Context) error {
	if c == nil || ctx == nil || ctx.Err() != nil {
		return ErrOnboardingStartCoordinate
	}
	// Scan/storage failures and unexpected starter failures have different
	// notions of recovery. A successful empty scan proves the trigger store is
	// healthy, but it does not prove that a previously failing starter path has
	// recovered. Reset that streak only after a real claimed page completes
	// without an unexpected starter failure (including an expected binding
	// rejection that was safely requeued).
	scanFailures := 0
	starterFailures := 0
	for {
		result, err := c.Step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case err != nil && errors.Is(err, ErrHealthySlotOnboardingStart):
			// Claiming succeeded; retain an independent streak for the starter
			// across empty polls caused by RetryDelay > PollInterval.
			scanFailures = 0
			starterFailures++
			if starterFailures >= c.config.MaxConsecutiveFailures {
				return errors.Join(ErrOnboardingStartCoordinate, err)
			}
		case err != nil:
			scanFailures++
			if scanFailures >= c.config.MaxConsecutiveFailures {
				return errors.Join(ErrOnboardingStartCoordinate, err)
			}
		case result.Claimed > 0:
			scanFailures = 0
			starterFailures = 0
		default:
			// An empty page is a successful scan only.
			scanFailures = 0
		}
		timer := time.NewTimer(c.config.PollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil
		case <-timer.C:
		}
	}
}

func canonicalCoordinatorTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC().Truncate(time.Microsecond)
}

var _ TriggeredOnboardingStarter = (*HealthySlotOnboardingStarter)(nil)
