package lease

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

var (
	ErrFailoverTooEarly  = errors.New("execution failover grace period has not elapsed")
	ErrLeaseStillCurrent = errors.New("old execution lease is still current")
)

type FailoverController struct {
	coordinator *Coordinator
	slots       store.SlotRepository
	timing      Timing
	now         func() time.Time
}

func NewFailoverController(coordinator *Coordinator, slots store.SlotRepository, timing Timing, now func() time.Time) (*FailoverController, error) {
	if coordinator == nil || slots == nil || timing.Validate() != nil {
		return nil, errors.New("failover coordinator, slot repository and timing are required")
	}
	if now == nil {
		now = time.Now
	}
	return &FailoverController{coordinator: coordinator, slots: slots, timing: timing, now: now}, nil
}

// FenceAndRelease permits reassignment only after the failover window and only
// when the authoritative lease backend proves the old claim is no longer
// current. Backend unavailability therefore blocks failover instead of risking
// two active epochs.
func (c *FailoverController) FenceAndRelease(ctx context.Context, claim Claim, lastRenewedAt time.Time) error {
	now := c.now().UTC()
	availability, err := EvaluateAvailability(lastRenewedAt, now, c.timing)
	if err != nil {
		return err
	}
	if !availability.CanFailover {
		return ErrFailoverTooEarly
	}
	if err := c.coordinator.Validate(ctx, claim); err == nil {
		return ErrLeaseStillCurrent
	} else if !errors.Is(err, ErrLeaseNotCurrent) {
		return err
	}
	if err := c.coordinator.Revoke(ctx, claim); err != nil {
		return err
	}
	return c.slots.ForceReleaseAssignment(ctx, claim.SlotID, claim.ExecutionEpoch, "node_failover_fenced", now)
}
