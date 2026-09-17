// Package runtimeenrollment composes the opt-in production runtime issuer's
// persistent dependencies. It never grants leases or substitutes memory stores.
package runtimeenrollment

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/control"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/storage"
)

var ErrDependencies = errors.New("runtime enrollment dependencies unavailable")

const startupTimeout = 5 * time.Second

type Dependencies struct {
	config control.RuntimeEnrollmentConfig
	client redisClient
	closed atomic.Bool
	once   sync.Once
	err    error
}

// New uses only durable SQL storage and an independently configured Redis
// validator. The caller has already verified the schema; no migrations or
// probe writes occur here. Ownership of database/repository remains external.
func New(ctx context.Context, c config.RuntimeEnrollmentConfig, database *sql.DB, repository *store.Repository) (*Dependencies, error) {
	return newDependencies(ctx, c, database, repository, newRedisClient)
}

func newDependencies(ctx context.Context, c config.RuntimeEnrollmentConfig, database *sql.DB, repository *store.Repository, newClient func(string) redisClient) (*Dependencies, error) {
	if ctx == nil || ctx.Err() != nil || !c.Enabled || c.Validate() != nil || database == nil || repository == nil || newClient == nil {
		return nil, ErrDependencies
	}
	receipts, err := storage.NewSQL(database)
	if err != nil {
		return nil, ErrDependencies
	}
	client := newClient(c.LeaseRedisAddr)
	if client == nil {
		return nil, ErrDependencies
	}
	d := &Dependencies{client: client}
	accepted := false
	defer func() {
		if !accepted {
			_ = d.Close()
		}
	}()
	bounded, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if bounded.Err() != nil || client.Ping(bounded).Err() != nil || bounded.Err() != nil {
		return nil, ErrDependencies
	}
	backend, err := lease.NewRedisBackend(client, config.RuntimeLeaseKeyPrefix)
	if err != nil || bounded.Err() != nil {
		return nil, ErrDependencies
	}
	d.config = control.RuntimeEnrollmentConfig{Bindings: repository, Receipts: receipts,
		Leases: leaseValidator{backend: backend, closed: &d.closed}, Timeout: c.Timeout}
	if bounded.Err() != nil {
		return nil, ErrDependencies
	}
	accepted = true
	return d, nil
}

func (d *Dependencies) ControlConfig() *control.RuntimeEnrollmentConfig {
	if d == nil || d.closed.Load() {
		return nil
	}
	c := d.config
	return &c
}

func (d *Dependencies) Close() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		d.closed.Store(true)
		if d.client != nil && d.client.Close() != nil {
			d.err = ErrDependencies
		}
	})
	return d.err
}

// Expose only validation, not Backend's Acquire/Renew/Revoke methods. A PING
// and empty Redis database must never be turned into an authorization grant.
type leaseValidator struct {
	backend *lease.RedisBackend
	closed  *atomic.Bool
}

func (v leaseValidator) Validate(ctx context.Context, claim lease.Claim) error {
	if ctx == nil || ctx.Err() != nil || v.backend == nil || v.closed == nil || v.closed.Load() {
		return lease.ErrBackendUnavailable
	}
	err := v.backend.Validate(ctx, claim)
	if ctx.Err() != nil || v.closed.Load() || errors.Is(err, lease.ErrBackendUnavailable) {
		return lease.ErrBackendUnavailable
	}
	return err
}
