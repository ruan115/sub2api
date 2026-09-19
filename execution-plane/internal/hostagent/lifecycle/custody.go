package lifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeregistry"
)

var ErrCustody = errors.New("authenticated runtime custody rejected")

// CustodyConfig builds the custodian. Validator is the execution lease
// authority; in production it is the coordinator that checks the fencing token
// and the durable record together, never a host-local substitute.
//
// There is deliberately no owner here. The lease owner is per slot, not per
// node, and arrives with each authenticated command; configuring one would let
// the host agent invent the claim it is supposed to prove.
type CustodyConfig struct {
	Validator          lease.Validator
	NodeID             string
	RevalidateInterval time.Duration
}

// Custody holds authenticated runtime connections for as long as their
// execution lease says they may be held.
//
// The host agent does not issue leases. It receives a slot, epoch and owner
// with the START command and proves them against the authority before taking
// custody: Adopt admits the claim through the fencer, which calls the
// validator. A claim the authority will not confirm is never taken into
// custody, so there is no host-local path that could stand in for a lease.
type Custody struct {
	registry *runtimeregistry.Registry
	fencer   *lease.Fencer
	nodeID   string
	interval time.Duration
}

func NewCustody(config CustodyConfig) (*Custody, error) {
	if config.Validator == nil || config.NodeID == "" ||
		config.RevalidateInterval <= 0 || config.RevalidateInterval > time.Minute {
		return nil, ErrCustody
	}
	fencer, err := lease.NewFencer(config.Validator)
	if err != nil {
		return nil, ErrCustody
	}
	registry, err := runtimeregistry.New(fencer)
	if err != nil {
		return nil, ErrCustody
	}
	return &Custody{
		registry: registry, fencer: fencer,
		nodeID: config.NodeID, interval: config.RevalidateInterval,
	}, nil
}

// take transfers ownership of an authenticated connection to the registry.
//
// It reports whether ownership moved. A false with no error means an identical
// instance is already held — START is replayable, so a replay must not fail —
// and the caller still owns the duplicate connection it just opened and must
// close it. Any error also leaves the connection with the caller: custody never
// closes what it did not accept.
// take accepts the instance fields and the connection separately so the claim
// it builds, and the replay comparison it makes, are testable without a live
// worker.
func (c *Custody) take(ctx context.Context, instance provider.Instance, leaseOwnerID string, connection runtimeregistry.Connection) (bool, error) {
	// The empty-owner check is deliberately redundant with Claim.Validate: a
	// claim that cannot be stated is refused at this boundary rather than
	// deeper in, so the reason stays legible.
	if c == nil || connection == nil || leaseOwnerID == "" {
		return false, ErrCustody
	}
	entry := runtimeregistry.Entry{
		Claim: lease.Claim{
			SlotID: instance.SlotID, NodeID: c.nodeID,
			ExecutionEpoch: instance.Epoch, OwnerID: leaseOwnerID,
		},
		Generation: instance.RuntimeGeneration,
		RuntimeID:  instance.RuntimeID,
		Connection: connection,
	}
	// One retry only. ErrAlreadyHeld means an incumbent exists; if it is this
	// exact instance the replay is a no-op, and if it disappeared between the
	// two calls the slot is free again and the adoption can stand. Anything
	// else, including a lease that has since ended, fails on the retry.
	for attempt := 0; attempt < 2; attempt++ {
		err := c.registry.Adopt(ctx, entry)
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, runtimeregistry.ErrAlreadyHeld):
			return false, errors.Join(ErrCustody, err)
		}
		// A different container under the same generation is ErrIdentityDrift
		// from the registry and never reaches here.
		held, exists := c.registry.Current(entry.Claim.SlotID)
		if exists {
			if held.Claim == entry.Claim && held.Generation == entry.Generation && held.RuntimeID == entry.RuntimeID {
				return false, nil
			}
			return false, errors.Join(ErrCustody, err)
		}
	}
	return false, ErrCustody
}

// Revalidate re-proves every held runtime against the lease authority and
// reclaims the ones whose lease is gone. An unreachable authority is not
// evidence that a lease is still held, so it reclaims those too.
func (c *Custody) Revalidate(ctx context.Context) error {
	if c == nil {
		return ErrCustody
	}
	return c.fencer.Revalidate(ctx)
}

// Run revalidates on an interval until the context ends, then drains. It does
// not return the revalidation error: reclaiming a runtime whose lease ended is
// the normal outcome, not a daemon failure.
func (c *Custody) Run(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrCustody
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	defer c.Drain()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			checkContext, cancel := context.WithTimeout(ctx, c.interval)
			_ = c.fencer.Revalidate(checkContext)
			cancel()
		}
	}
}

// Revoke ends a slot's custody at or below an epoch and bars it from being
// taken again. It touches one slot only: other slots on this node, and the
// node's own control connection, are untouched.
func (c *Custody) Revoke(slotID string, epoch uint64) {
	if c == nil {
		return
	}
	c.registry.Revoke(slotID, epoch)
}

// Release hands one exact generation back. A generation the fencer or a
// supersede already reclaimed reports runtimeregistry.ErrNotHeld, which is
// benign on a teardown path.
func (c *Custody) Release(slotID string, epoch, generation uint64) error {
	if c == nil {
		return ErrCustody
	}
	return c.registry.Release(slotID, epoch, generation)
}

// Drain releases everything on shutdown and reports how many runtimes it let go.
func (c *Custody) Drain() int {
	if c == nil {
		return 0
	}
	return c.registry.Drain()
}

// Held reports what the slot currently holds, for observation only.
func (c *Custody) Held(slotID string) (runtimeregistry.Held, bool) {
	if c == nil {
		return runtimeregistry.Held{}, false
	}
	return c.registry.Current(slotID)
}

// Len reports how many runtimes are under custody.
func (c *Custody) Len() int {
	if c == nil {
		return 0
	}
	return c.registry.Len()
}
