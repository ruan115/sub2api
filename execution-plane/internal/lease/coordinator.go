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

// Renew takes the TTL rather than using the grant TTL so that a Renewer can
// drive the coordinator directly. Renewing only the fencing token would let the
// durable expires_at go stale while the token lives on, which splits authority:
// this Validate would allow the lease while ValidateCurrentProxyLease, which
// reads expires_at, would refuse it.
func (c *Coordinator) Renew(ctx context.Context, claim Claim, ttl time.Duration) error {
	if err := validateOperation(claim, ttl); err != nil {
		return err
	}
	now := c.now().UTC()
	if err := c.backend.Renew(ctx, claim, ttl); err != nil {
		return err
	}
	if err := c.durable.RenewExecutionLease(ctx, claim.SlotID, claim.ExecutionEpoch, claim.OwnerID, now.Add(ttl), now); err != nil {
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

// Validate is the conjunction of both stores, because either one alone can
// still authorise a lease the other has already ended.
//
// Revoke writes the durable record first and then drops the fencing token. If
// the token could not be dropped, the backend keeps reporting the claim as
// current until its TTL runs out, and a backend-only check would go on
// authorising a lease that was revoked. Checking the durable record closes that
// window instead of leaving it bounded by the TTL.
//
// The reverse direction is already covered: a token dropped while the durable
// write failed fails the backend check.
//
// Expiry is deliberately NOT re-derived from the durable row. The fencing token
// TTL is the expiry authority, and comparing a database timestamp against this
// process's clock would turn skew into spurious revocation.
func (c *Coordinator) Validate(ctx context.Context, claim Claim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	if err := c.backend.Validate(ctx, claim); err != nil {
		return err
	}
	durable, err := c.durable.GetExecutionLease(ctx, claim.SlotID, claim.ExecutionEpoch)
	if err != nil {
		if errors.Is(err, store.ErrExecutionLeaseNotFound) {
			return ErrLeaseNotCurrent
		}
		// An unreadable durable record is not evidence that the lease is still
		// held. This costs availability on a store outage, which is the same
		// trade the backends already make when they are unreachable.
		return fmt.Errorf("%w: read durable execution lease: %w", ErrBackendUnavailable, err)
	}
	if durable.RevokedAt != nil || durable.NodeID != claim.NodeID || durable.OwnerID != claim.OwnerID {
		return ErrLeaseNotCurrent
	}
	return nil
}
