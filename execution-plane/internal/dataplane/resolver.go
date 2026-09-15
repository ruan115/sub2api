package dataplane

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

var ErrBindingUnavailable = errors.New("execution binding is unavailable")

// Snapshot is a trusted, internally consistent control-plane projection, not
// caller input or a Redis route cache entry. ProviderRef is never sent to CCMAX.
// Its source must advance generation/epoch before replacing the assignment.
type Snapshot struct {
	Binding
	ProviderRef  string
	LeaseOwnerID string
	// Session-bound sources populate this control-plane-only fence. A
	// reconnect during runtime lookup must not return the old connection,
	// even if a new inspection has already confirmed the same assignment.
	ControlSessionID string
	Ready            bool
	ObservedAt       time.Time
}

type SnapshotSource interface {
	Snapshot(context.Context, string) (Snapshot, error)
}

// RuntimeLookup only finds an already provisioned runtime. Implementations must
// not create/start slots, derive endpoints from caller input, or mint tickets.
type RuntimeLookup interface {
	LookupRuntime(context.Context, Binding, string) (Runtime, error)
}

type LeaseValidator interface {
	Validate(context.Context, lease.Claim) error
}

type FencedResolverConfig struct {
	NodeID         string
	Source         SnapshotSource
	Leases         LeaseValidator
	Runtimes       RuntimeLookup
	MaxSnapshotAge time.Duration
	Now            func() time.Time
}

type FencedResolver struct{ config FencedResolverConfig }

func NewFencedResolver(config FencedResolverConfig) (*FencedResolver, error) {
	if credential.ValidateTransportID(config.NodeID) != nil || config.Source == nil || config.Leases == nil || config.Runtimes == nil {
		return nil, ErrBindingUnavailable
	}
	if config.MaxSnapshotAge == 0 {
		config.MaxSnapshotAge = 45 * time.Second
	}
	if config.MaxSnapshotAge <= 0 || config.MaxSnapshotAge > 45*time.Second {
		return nil, ErrBindingUnavailable
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &FencedResolver{config: config}, nil
}

func (r *FencedResolver) Resolve(ctx context.Context, binding Binding) (Runtime, error) {
	before, err := r.current(ctx, binding)
	if err != nil {
		return nil, err
	}
	runtime, err := r.config.Runtimes.LookupRuntime(ctx, binding, before.ProviderRef)
	if err != nil || runtime == nil {
		return nil, ErrBindingUnavailable
	}
	// A lookup must not resurrect an assignment replaced while it was running.
	after, err := r.current(ctx, binding)
	if err != nil || after.ProviderRef != before.ProviderRef || after.LeaseOwnerID != before.LeaseOwnerID || after.ControlSessionID != before.ControlSessionID {
		return nil, ErrBindingUnavailable
	}
	return runtime, nil
}

func (r *FencedResolver) Validate(ctx context.Context, binding Binding) error {
	_, err := r.current(ctx, binding)
	return err
}

func (r *FencedResolver) current(ctx context.Context, binding Binding) (Snapshot, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || binding.NodeID != r.config.NodeID ||
		credential.ValidateTransportID(binding.AccountID) != nil || credential.ValidateTransportID(binding.SlotID) != nil ||
		binding.ExecutionEpoch == 0 || binding.RouteGeneration == 0 {
		return Snapshot{}, ErrBindingUnavailable
	}
	current, err := r.config.Source.Snapshot(ctx, binding.SlotID)
	now := r.config.Now().UTC()
	if err != nil || current.Binding != binding || !current.Ready || current.ProviderRef == "" || len(current.ProviderRef) > 256 ||
		credential.ValidateTransportID(current.LeaseOwnerID) != nil || current.ObservedAt.IsZero() ||
		current.ObservedAt.After(now) || !current.ObservedAt.Add(r.config.MaxSnapshotAge).After(now) {
		return Snapshot{}, ErrBindingUnavailable
	}
	if err := r.config.Leases.Validate(ctx, lease.Claim{
		SlotID: binding.SlotID, NodeID: binding.NodeID, ExecutionEpoch: binding.ExecutionEpoch, OwnerID: current.LeaseOwnerID,
	}); err != nil || ctx.Err() != nil {
		return Snapshot{}, ErrBindingUnavailable
	}
	// Lease validation may block on storage. Do not return a snapshot that
	// crossed its freshness boundary while that I/O was in flight.
	now = r.config.Now().UTC()
	if current.ObservedAt.After(now) || !current.ObservedAt.Add(r.config.MaxSnapshotAge).After(now) {
		return Snapshot{}, ErrBindingUnavailable
	}
	return current, nil
}

var _ Resolver = (*FencedResolver)(nil)
