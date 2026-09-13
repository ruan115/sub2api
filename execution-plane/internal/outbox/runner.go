package outbox

import (
	"context"
	"errors"
	"time"
)

const (
	defaultPollInterval           = time.Second
	defaultBatchSize              = 100
	defaultMaxConsecutiveFailures = 5
)

var ErrRun = errors.New("runtime outbox runner failed")

type RunnerConfig struct {
	PollInterval           time.Duration
	BatchSize              int
	MaxConsecutiveFailures int
	OnError                func(error)
	OnBlocked              func(BlockedError)
	OnRetryBudgetExceeded  func(RetryBudgetError)
}

type RunResult struct {
	Processed int
	Failed    int
}

type Runner struct {
	consumer *Consumer
	config   RunnerConfig
}

func NewRunner(consumer *Consumer, config RunnerConfig) (*Runner, error) {
	if consumer == nil {
		return nil, ErrRun
	}
	if config.PollInterval == 0 {
		config.PollInterval = defaultPollInterval
	}
	if config.BatchSize == 0 {
		config.BatchSize = defaultBatchSize
	}
	if config.MaxConsecutiveFailures == 0 {
		config.MaxConsecutiveFailures = defaultMaxConsecutiveFailures
	}
	if config.PollInterval <= 0 || config.PollInterval > time.Minute || config.BatchSize < 1 || config.BatchSize > 1000 ||
		config.MaxConsecutiveFailures < 1 || config.MaxConsecutiveFailures > 1000 {
		return nil, ErrRun
	}
	if config.OnError == nil {
		config.OnError = func(error) {}
	}
	if config.OnBlocked == nil {
		config.OnBlocked = func(BlockedError) {}
	}
	if config.OnRetryBudgetExceeded == nil {
		config.OnRetryBudgetExceeded = func(RetryBudgetError) {}
	}
	return &Runner{consumer: consumer, config: config}, nil
}

// Step drains at most one bounded ordered page. The first failed event stops
// the page: advancing beyond it would violate the single CCMAX checkpoint's
// grant-before-onboarding ordering.
func (r *Runner) Step(ctx context.Context) (RunResult, error) {
	if r == nil || r.consumer == nil || ctx == nil || ctx.Err() != nil {
		return RunResult{}, ErrRun
	}
	var result RunResult
	for result.Processed < r.config.BatchSize {
		processed, err := r.consumer.RunOnce(ctx)
		if errors.Is(err, ErrBusy) {
			return result, nil
		}
		if err != nil {
			if processed {
				result.Processed++
				result.Failed++
			}
			r.config.OnError(ErrRun)
			return result, errors.Join(ErrRun, err)
		}
		if !processed {
			break
		}
		result.Processed++
	}
	return result, nil
}

func (r *Runner) Run(ctx context.Context) error {
	if r == nil || ctx == nil || ctx.Err() != nil {
		return ErrRun
	}
	consecutiveFailures := 0
	for {
		_, err := r.Step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, ErrBlocked) {
			var blocked *BlockedError
			if errors.As(err, &blocked) && blocked != nil {
				r.config.OnBlocked(*blocked)
			}
			return err
		}
		if errors.Is(err, ErrRetryBudgetExceeded) {
			var exhausted *RetryBudgetError
			if errors.As(err, &exhausted) && exhausted != nil {
				r.config.OnRetryBudgetExceeded(*exhausted)
			}
			return err
		}
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= r.config.MaxConsecutiveFailures {
				return err
			}
		} else {
			consecutiveFailures = 0
		}
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
