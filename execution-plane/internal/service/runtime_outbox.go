package service

import (
	"database/sql"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/reconcile"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

var ErrRuntimeOutboxComposition = errors.New("runtime outbox composition is invalid")

type RuntimeOutboxConfig struct {
	ConsumerName          string
	Owner                 string
	LeaseTTL              time.Duration
	PollInterval          time.Duration
	BatchSize             int
	MaxRetryFailures      int
	Defaults              reconcile.CCMAXRuntimeDefaults
	OnError               func(error)
	OnBlocked             func(outbox.BlockedError)
	OnRetryBudgetExceeded func(outbox.RetryBudgetError)
	Now                   func() time.Time
}

// NewRuntimeOutboxRunner composes the only ordered CCMAX runtime checkpoint.
// Authority, onboarding and lifecycle events must all use this runner so an
// onboarding trigger cannot overtake the reservation grant that authorizes it.
func NewRuntimeOutboxRunner(
	ccmaxDatabase *sql.DB,
	runtimeRepository *store.Repository,
	config RuntimeOutboxConfig,
) (*outbox.Runner, error) {
	if ccmaxDatabase == nil || runtimeRepository == nil || config.ConsumerName == "" || config.Owner == "" ||
		config.LeaseTTL <= 0 {
		return nil, ErrRuntimeOutboxComposition
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaxRetryFailures == 0 {
		config.MaxRetryFailures = int(outbox.DefaultMaxRetryFailures)
	}
	if config.MaxRetryFailures < 1 || config.MaxRetryFailures > 1000 {
		return nil, ErrRuntimeOutboxComposition
	}
	desiredSource, err := reconcile.NewMySQLAccountRuntimeSource(ccmaxDatabase, config.Defaults)
	if err != nil {
		return nil, errors.Join(ErrRuntimeOutboxComposition, err)
	}
	router, err := reconcile.NewRuntimeEventRouter(
		desiredSource,
		runtimeRepository,
		runtimeRepository,
		runtimeRepository,
		config.Now,
	)
	if err != nil {
		return nil, errors.Join(ErrRuntimeOutboxComposition, err)
	}
	source, err := outbox.NewStrictMySQLSource(ccmaxDatabase)
	if err != nil {
		return nil, errors.Join(ErrRuntimeOutboxComposition, err)
	}
	consumer, err := outbox.NewConsumer(
		source,
		router,
		config.ConsumerName,
		config.Owner,
		config.LeaseTTL,
		config.Now,
		uint64(config.MaxRetryFailures),
	)
	if err != nil {
		return nil, errors.Join(ErrRuntimeOutboxComposition, err)
	}
	runner, err := outbox.NewRunner(consumer, outbox.RunnerConfig{
		PollInterval:           config.PollInterval,
		BatchSize:              config.BatchSize,
		MaxConsecutiveFailures: config.MaxRetryFailures,
		OnError:                config.OnError,
		OnBlocked:              config.OnBlocked,
		OnRetryBudgetExceeded:  config.OnRetryBudgetExceeded,
	})
	if err != nil {
		return nil, errors.Join(ErrRuntimeOutboxComposition, err)
	}
	return runner, nil
}
