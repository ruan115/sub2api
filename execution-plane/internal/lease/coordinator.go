package lease

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

type Coordinator struct {
	backend Backend
	durable store.ExecutionLeaseRepository
	ttl     time.Duration
	now     func() time.Time
}

func NewCoordinator(backend Backend, durable store.ExecutionLeaseRepository, ttl time.Duration, now func() time.Time) (*Coordinator, error) {
	if backend == nil || durable == nil || ttl <= 0 {
		return nil, errors.New("execution lease backend, durable store and TTL are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Coordinator{backend: backend, durable: durable, ttl: ttl, now: now}, nil
}

func (c *Coordinator) Grant(ctx context.Context, leaseID string, claim Claim) error {
	if leaseID == "" || claim.Validate() != nil {
		return errors.New("execution lease grant is invalid")
	}
	now := c.now().UTC()
	if err := c.backend.Acquire(ctx, claim, c.ttl); err != nil {
		return fmt.Errorf("activate execution lease: %w", err)
	}
	_, err := c.durable.GrantExecutionLease(ctx, store.ExecutionLease{
		ID: leaseID, SlotID: claim.SlotID, NodeID: claim.NodeID, ExecutionEpoch: claim.ExecutionEpoch,
		OwnerID: claim.OwnerID, ExpiresAt: now.Add(c.ttl), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		_ = c.backend.Revoke(context.Background(), claim)
		return fmt.Errorf("persist execution lease grant: %w", err)
	}
	return nil
}

func (c *Coordinator) Renew(ctx context.Context, claim Claim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	now := c.now().UTC()
	if err := c.backend.Renew(ctx, claim, c.ttl); err != nil {
		return err
	}
	if err := c.durable.RenewExecutionLease(ctx, claim.SlotID, claim.ExecutionEpoch, claim.OwnerID, now.Add(c.ttl), now); err != nil {
		_ = c.backend.Revoke(context.Background(), claim)
		return fmt.Errorf("persist execution lease renewal: %w", err)
	}
	return nil
}

func (c *Coordinator) Revoke(ctx context.Context, claim Claim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	now := c.now().UTC()
	durableErr := c.durable.RevokeExecutionLease(ctx, claim.SlotID, claim.ExecutionEpoch, claim.OwnerID, now)
	backendErr := c.backend.Revoke(ctx, claim)
	if durableErr != nil || backendErr != nil {
		return errors.Join(durableErr, backendErr)
	}
	return nil
}

func (c *Coordinator) Validate(ctx context.Context, claim Claim) error {
	return c.backend.Validate(ctx, claim)
}
