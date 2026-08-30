package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

const (
	defaultProvisioningPollInterval = time.Second
	defaultProvisioningBatchSize    = 100
)

var ErrProvisioningRun = errors.New("secure onboarding provisioning runner failed")

type ProvisioningAdvancer interface {
	Advance(ctx context.Context, workflowID string) (string, error)
}

type ProvisioningRunnerConfig struct {
	PollInterval time.Duration
	BatchSize    int
	OnError      func(workflowID string, err error)
	Now          func() time.Time
}

type ProvisioningRunResult struct {
	Scanned int
	Failed  int
}

type ProvisioningRunner struct {
	repository onboarding.ActiveProvisioningRepository
	advancer   ProvisioningAdvancer
	config     ProvisioningRunnerConfig
}

func NewProvisioningRunner(
	repository onboarding.ActiveProvisioningRepository,
	advancer ProvisioningAdvancer,
	config ProvisioningRunnerConfig,
) (*ProvisioningRunner, error) {
	if repository == nil || advancer == nil {
		return nil, ErrProvisioningRun
	}
	if config.PollInterval == 0 {
		config.PollInterval = defaultProvisioningPollInterval
	}
	if config.BatchSize == 0 {
		config.BatchSize = defaultProvisioningBatchSize
	}
	if config.PollInterval <= 0 || config.BatchSize < 1 || config.BatchSize > 1000 {
		return nil, ErrProvisioningRun
	}
	if config.OnError == nil {
		config.OnError = func(string, error) {}
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ProvisioningRunner{repository: repository, advancer: advancer, config: config}, nil
}

// Step scans one bounded page and advances each workflow once. A bad workflow
// is isolated from the rest of the page; a repository scan failure is returned
// because no work identity can be trusted in that case.
func (r *ProvisioningRunner) Step(ctx context.Context) (ProvisioningRunResult, error) {
	if r == nil || r.repository == nil || r.advancer == nil || ctx == nil || ctx.Err() != nil {
		return ProvisioningRunResult{}, ErrProvisioningRun
	}
	ids, err := r.repository.ListActiveProvisioningIDs(ctx, r.config.BatchSize)
	if err != nil {
		r.config.OnError("", ErrProvisioningRun)
		return ProvisioningRunResult{}, ErrProvisioningRun
	}
	result := ProvisioningRunResult{Scanned: len(ids)}
	for _, workflowID := range ids {
		if ctx.Err() != nil {
			return result, ErrProvisioningRun
		}
		if credential.ValidateTransportID(workflowID) != nil {
			result.Failed++
			r.config.OnError("", ErrProvisioningRun)
			continue
		}
		if _, err := r.advancer.Advance(ctx, workflowID); err != nil {
			result.Failed++
			r.config.OnError(workflowID, ErrProvisioningAdvance)
			if err := r.repository.DeferProvisioningRetry(ctx, workflowID, r.config.Now().UTC()); err != nil {
				r.config.OnError(workflowID, ErrProvisioningRun)
			}
		}
	}
	return result, nil
}

func (r *ProvisioningRunner) Run(ctx context.Context) error {
	if r == nil || ctx == nil || ctx.Err() != nil {
		return ErrProvisioningRun
	}
	for {
		_, _ = r.Step(ctx)
		timer := time.NewTimer(r.config.PollInterval)
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

var _ ProvisioningAdvancer = (*SecureOnboardingController)(nil)
