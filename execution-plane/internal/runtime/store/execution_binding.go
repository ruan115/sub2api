package store

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

// ErrExecutionBindingUnavailable never includes repository errors or binding
// metadata. A missing, inconsistent, stale or inaccessible projection is denied.
var ErrExecutionBindingUnavailable = errors.New("execution binding is unavailable")

// ExecutionBinding is a read-only control-plane projection, not a route-cache
// entry or an assertion that worker mode/credentials are ready. Callers must
// separately validate the active authenticated control session and Redis lease.
type ExecutionBinding struct {
	AccountID, SlotID, NodeID, ProviderRef, LeaseOwnerID, ControlSessionID, ImageDigest string
	ExecutionEpoch, RouteGeneration                                                     uint64
	ObservedAt, NodeSeenAt, LeaseExpiresAt                                              time.Time
}

type ExecutionBindingRepository interface {
	ReadExecutionBinding(ctx context.Context, slotID string, checkedAt time.Time, maxAge time.Duration) (ExecutionBinding, error)
}

// A single consistent SELECT prevents independently read account/assignment/
// node/lease versions from being assembled into authority. No credential or
// route-cache column is read, and observation timestamps are never refreshed.
const readExecutionBindingSQL = `
SELECT s.account_id, s.slot_id, sa.node_id, sa.provider_ref, el.owner_id,
       n.control_session_id, sa.image_digest, sa.execution_epoch, sa.desired_generation,
       sa.last_observed_at, n.last_seen_at, el.expires_at,
       sa.assigned_at, el.created_at, el.updated_at
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id
  AND BINARY sa.slot_id = BINARY s.slot_id AND sa.released_at IS NULL
JOIN nodes n ON n.node_id = sa.node_id AND BINARY n.node_id = BINARY sa.node_id
JOIN execution_leases el ON el.slot_id = sa.slot_id AND BINARY el.slot_id = BINARY sa.slot_id
  AND el.execution_epoch = sa.execution_epoch
  AND el.node_id = sa.node_id AND BINARY el.node_id = BINARY sa.node_id
WHERE s.slot_id = ?
  AND s.desired_state = 'ready'
  AND s.desired_generation > 0 AND sa.desired_generation = s.desired_generation
  AND sa.execution_epoch > 0
  AND BINARY sa.image_digest = BINARY s.image_digest
  AND sa.healthy = 1 AND sa.actual_state IN ('ready', 'running', 'busy')
  AND sa.provider_ref IS NOT NULL AND sa.provider_ref <> ''
  AND n.status = 'connected'
  AND n.control_session_id IS NOT NULL AND n.control_session_id <> ''
  AND sa.observed_control_session_id IS NOT NULL
  AND BINARY sa.observed_control_session_id = BINARY n.control_session_id
  AND sa.last_observed_at > ? AND sa.last_observed_at <= ?
  AND n.last_seen_at > ? AND n.last_seen_at <= ?
  AND sa.assigned_at <= ?
  AND el.revoked_at IS NULL AND el.created_at <= ? AND el.updated_at <= ?
  AND el.expires_at > ?`

func (r *Repository) ReadExecutionBinding(ctx context.Context, slotID string, checkedAt time.Time, maxAge time.Duration) (ExecutionBinding, error) {
	if r == nil || r.db == nil || !validBindingRead(ctx, slotID, checkedAt, maxAge) {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	checkedAt = checkedAt.UTC()
	cutoff := checkedAt.Add(-maxAge)
	var binding ExecutionBinding
	var assignedAt, leaseCreatedAt, leaseUpdatedAt time.Time
	err := r.db.QueryRowContext(ctx, readExecutionBindingSQL,
		slotID, cutoff, checkedAt, cutoff, checkedAt, checkedAt, checkedAt, checkedAt, checkedAt,
	).Scan(&binding.AccountID, &binding.SlotID, &binding.NodeID, &binding.ProviderRef,
		&binding.LeaseOwnerID, &binding.ControlSessionID, &binding.ImageDigest,
		&binding.ExecutionEpoch, &binding.RouteGeneration,
		&binding.ObservedAt, &binding.NodeSeenAt, &binding.LeaseExpiresAt,
		&assignedAt, &leaseCreatedAt, &leaseUpdatedAt)
	if err != nil || ctx.Err() != nil || binding.SlotID != slotID ||
		!validExecutionBinding(binding, checkedAt, maxAge) ||
		!validBindingHistory(assignedAt, leaseCreatedAt, leaseUpdatedAt, checkedAt) {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	return utcExecutionBinding(binding), nil
}

func (r *MemoryRepository) ReadExecutionBinding(ctx context.Context, slotID string, checkedAt time.Time, maxAge time.Duration) (ExecutionBinding, error) {
	if r == nil || !validBindingRead(ctx, slotID, checkedAt, maxAge) {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ctx.Err() != nil {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	slot, exists := r.slots[slotID]
	if !exists || slot.ID != slotID || slot.DesiredState != "ready" || slot.DesiredGeneration == 0 {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	var assignment Assignment
	active := false
	for _, candidate := range r.assignments[slotID] {
		if candidate.ReleasedAt == nil {
			if active { // Match the SQL unique-active-assignment invariant.
				return ExecutionBinding{}, ErrExecutionBindingUnavailable
			}
			assignment, active = candidate, true
		}
	}
	if !active || assignment.SlotID != slotID || assignment.DesiredGeneration != slot.DesiredGeneration ||
		assignment.ImageDigest != slot.ImageDigest || !assignment.Healthy ||
		(assignment.ActualState != "ready" && assignment.ActualState != "running" && assignment.ActualState != "busy") ||
		assignment.LastObservedAt == nil {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	node, exists := r.nodes[assignment.NodeID]
	if !exists || node.ID != assignment.NodeID || node.Status != "connected" || node.ControlSessionID == "" ||
		assignment.ObservedControlSessionID != node.ControlSessionID || node.LastSeenAt == nil {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	lease, exists := r.executionLeases[executionLeaseKey(slotID, assignment.ExecutionEpoch)]
	if !exists || lease.SlotID != slotID || lease.NodeID != node.ID || lease.ExecutionEpoch != assignment.ExecutionEpoch ||
		lease.RevokedAt != nil || !validBindingHistory(assignment.AssignedAt, lease.CreatedAt, lease.UpdatedAt, checkedAt) {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	binding := ExecutionBinding{
		AccountID: slot.AccountID, SlotID: slot.ID, NodeID: node.ID,
		ProviderRef: assignment.ProviderRef, LeaseOwnerID: lease.OwnerID,
		ControlSessionID: node.ControlSessionID, ImageDigest: assignment.ImageDigest,
		ExecutionEpoch: assignment.ExecutionEpoch, RouteGeneration: assignment.DesiredGeneration,
		ObservedAt: *assignment.LastObservedAt, NodeSeenAt: *node.LastSeenAt, LeaseExpiresAt: lease.ExpiresAt,
	}
	if ctx.Err() != nil || !validExecutionBinding(binding, checkedAt, maxAge) {
		return ExecutionBinding{}, ErrExecutionBindingUnavailable
	}
	return utcExecutionBinding(binding), nil
}

func validBindingRead(ctx context.Context, slotID string, checkedAt time.Time, maxAge time.Duration) bool {
	return ctx != nil && ctx.Err() == nil && credential.ValidateTransportID(slotID) == nil &&
		!checkedAt.IsZero() && maxAge > 0 && maxAge <= 45*time.Second
}

func validExecutionBinding(binding ExecutionBinding, checkedAt time.Time, maxAge time.Duration) bool {
	for _, id := range []string{binding.AccountID, binding.SlotID, binding.NodeID, binding.LeaseOwnerID, binding.ControlSessionID} {
		if credential.ValidateTransportID(id) != nil {
			return false
		}
	}
	if !controlSessionIDPattern.MatchString(binding.ControlSessionID) || binding.ExecutionEpoch == 0 || binding.RouteGeneration == 0 ||
		!runtimeImageDigestPattern.MatchString(binding.ImageDigest) || !validBindingProviderRef(binding.ProviderRef) {
		return false
	}
	cutoff := checkedAt.Add(-maxAge)
	return !binding.ObservedAt.IsZero() && binding.ObservedAt.After(cutoff) && !binding.ObservedAt.After(checkedAt) &&
		!binding.NodeSeenAt.IsZero() && binding.NodeSeenAt.After(cutoff) && !binding.NodeSeenAt.After(checkedAt) &&
		binding.LeaseExpiresAt.After(checkedAt)
}

func validBindingHistory(assignedAt, leaseCreatedAt, leaseUpdatedAt, checkedAt time.Time) bool {
	return !assignedAt.IsZero() && !assignedAt.After(checkedAt) &&
		!leaseCreatedAt.IsZero() && !leaseCreatedAt.After(checkedAt) &&
		!leaseUpdatedAt.IsZero() && !leaseUpdatedAt.Before(leaseCreatedAt) && !leaseUpdatedAt.After(checkedAt)
}

func validBindingProviderRef(ref string) bool {
	if ref == "" || len(ref) > 255 || strings.TrimSpace(ref) != ref || !utf8.ValidString(ref) {
		return false
	}
	for _, char := range ref {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func utcExecutionBinding(binding ExecutionBinding) ExecutionBinding {
	binding.ObservedAt = binding.ObservedAt.UTC()
	binding.NodeSeenAt = binding.NodeSeenAt.UTC()
	binding.LeaseExpiresAt = binding.LeaseExpiresAt.UTC()
	return binding
}

var _ ExecutionBindingRepository = (*Repository)(nil)
var _ ExecutionBindingRepository = (*MemoryRepository)(nil)
