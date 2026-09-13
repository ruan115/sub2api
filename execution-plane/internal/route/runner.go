package route

import (
	"context"
	"errors"
	"time"
)

var ErrRouteRunner = errors.New("execution route publisher is invalid")

type Runner struct {
	reconciler *Reconciler
	interval   time.Duration
	onError    func(error)
}

func NewRunner(reconciler *Reconciler, interval time.Duration, onError func(error)) (*Runner, error) {
	if reconciler == nil || interval <= 0 || interval > time.Minute {
		return nil, ErrRouteRunner
	}
	return &Runner{reconciler: reconciler, interval: interval, onError: onError}, nil
}

func (r *Runner) Run(ctx context.Context) error {
	if r == nil || r.reconciler == nil || r.interval <= 0 || ctx == nil {
		return ErrRouteRunner
	}
	if err := r.sync(ctx); err != nil && ctx.Err() != nil {
		return nil
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.sync(ctx); err != nil && ctx.Err() != nil {
				return nil
			}
		}
	}
}

func (r *Runner) sync(ctx context.Context) error {
	if err := r.reconciler.Sync(ctx); err != nil {
		if r.onError != nil && ctx.Err() == nil {
			r.onError(err)
		}
		return err
	}
	return nil
}
