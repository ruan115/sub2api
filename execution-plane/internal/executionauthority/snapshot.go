// Package executionauthority projects durable execution state for the control
// plane. It owns no credentials, ticket keys, runtime provisioning or listener.
package executionauthority

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

type SessionValidator interface {
	ValidateControlSession(context.Context, string, string) error
}

type Config struct {
	NodeID         string
	Repository     store.ExecutionBindingRepository
	Sessions       SessionValidator
	MaxSnapshotAge time.Duration
	Now            func() time.Time
}

type Source struct{ config Config }

func NewSource(config Config) (*Source, error) {
	if credential.ValidateTransportID(config.NodeID) != nil || config.Repository == nil || config.Sessions == nil {
		return nil, dataplane.ErrBindingUnavailable
	}
	if config.MaxSnapshotAge == 0 {
		config.MaxSnapshotAge = 45 * time.Second
	}
	if config.MaxSnapshotAge <= 0 || config.MaxSnapshotAge > 45*time.Second {
		return nil, dataplane.ErrBindingUnavailable
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Source{config: config}, nil
}

// Snapshot does not renew or cache authority. The persisted observation time
// survives unchanged; node heartbeat and query time cannot extend it. Ready
// here describes a scheduling candidate, not active worker mode/credentials.
// FencedResolver must still verify the independent lease backend.
func (s *Source) Snapshot(ctx context.Context, slotID string) (dataplane.Snapshot, error) {
	if s == nil || ctx == nil || ctx.Err() != nil || credential.ValidateTransportID(slotID) != nil {
		return dataplane.Snapshot{}, dataplane.ErrBindingUnavailable
	}
	before := s.config.Now().UTC()
	binding, err := s.config.Repository.ReadExecutionBinding(ctx, slotID, before, s.config.MaxSnapshotAge)
	if err != nil || binding.SlotID != slotID || binding.NodeID != s.config.NodeID ||
		credential.ValidateTransportID(binding.AccountID) != nil || credential.ValidateTransportID(binding.LeaseOwnerID) != nil ||
		binding.ExecutionEpoch == 0 || binding.RouteGeneration == 0 || binding.ProviderRef == "" || len(binding.ProviderRef) > 256 ||
		len(binding.ControlSessionID) != 32 || !s.current(binding, before) || ctx.Err() != nil {
		return dataplane.Snapshot{}, dataplane.ErrBindingUnavailable
	}
	if s.config.Sessions.ValidateControlSession(ctx, binding.NodeID, binding.ControlSessionID) != nil ||
		ctx.Err() != nil || !s.current(binding, s.config.Now().UTC()) {
		return dataplane.Snapshot{}, dataplane.ErrBindingUnavailable
	}
	return dataplane.Snapshot{
		Binding: dataplane.Binding{AccountID: binding.AccountID, SlotID: binding.SlotID, NodeID: binding.NodeID,
			ExecutionEpoch: binding.ExecutionEpoch, RouteGeneration: binding.RouteGeneration},
		ProviderRef: binding.ProviderRef, LeaseOwnerID: binding.LeaseOwnerID, ControlSessionID: binding.ControlSessionID,
		Ready: true, ObservedAt: binding.ObservedAt,
	}, nil
}

func (s *Source) current(binding store.ExecutionBinding, now time.Time) bool {
	return !binding.ObservedAt.IsZero() && !binding.ObservedAt.After(now) && binding.ObservedAt.Add(s.config.MaxSnapshotAge).After(now) &&
		!binding.NodeSeenAt.IsZero() && !binding.NodeSeenAt.After(now) && binding.NodeSeenAt.Add(s.config.MaxSnapshotAge).After(now) &&
		binding.LeaseExpiresAt.After(now)
}

var _ dataplane.SnapshotSource = (*Source)(nil)
