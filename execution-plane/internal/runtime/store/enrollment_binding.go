package store

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/contracts"
)

var ErrRuntimeEnrollmentBindingUnavailable = errors.New("runtime enrollment binding unavailable")

// Bootstrap authority deliberately excludes provider_ref, observation and
// worker health: the instance cannot serve TLS before receiving its first leaf.
// desired_state='ready' is control intent, not a worker readiness assertion.
const runtimeEnrollmentBindingSQL = `SELECT sa.assignment_id, s.account_id, s.slot_id, sa.node_id, sa.image_digest,
 n.control_session_id, el.owner_id, sa.execution_epoch, sa.desired_generation,
 n.last_seen_at, el.expires_at, sa.assigned_at, el.created_at, el.updated_at
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id AND BINARY sa.slot_id = BINARY s.slot_id AND sa.released_at IS NULL
JOIN nodes n ON n.node_id = sa.node_id AND BINARY n.node_id = BINARY sa.node_id
JOIN execution_leases el ON el.slot_id = sa.slot_id AND BINARY el.slot_id = BINARY sa.slot_id
 AND el.execution_epoch = sa.execution_epoch AND el.node_id = sa.node_id AND BINARY el.node_id = BINARY sa.node_id
WHERE BINARY s.slot_id = BINARY ? AND s.desired_state = 'ready'
 AND s.desired_generation > 0 AND sa.desired_generation = s.desired_generation AND sa.execution_epoch > 0
 AND BINARY sa.image_digest = BINARY s.image_digest
 AND n.status = 'connected' AND n.control_session_id IS NOT NULL
 AND n.last_seen_at > ? AND n.last_seen_at <= ? AND sa.assigned_at <= ?
 AND el.revoked_at IS NULL AND el.created_at <= ? AND el.updated_at <= ? AND el.expires_at > ?`

func (r *Repository) ReadRuntimeEnrollmentBinding(ctx context.Context, slotID string, now time.Time, maxNodeAge time.Duration) (contracts.Grant, error) {
	if r == nil || r.db == nil || !validBindingRead(ctx, slotID, now, maxNodeAge) {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	now = now.UTC()
	var g contracts.Grant
	var assigned, created, updated time.Time
	err := r.db.QueryRowContext(ctx, runtimeEnrollmentBindingSQL, slotID, now.Add(-maxNodeAge), now, now, now, now, now).
		Scan(&g.AssignmentID, &g.AccountID, &g.SlotID, &g.NodeID, &g.ImageDigest, &g.ControlSessionID, &g.LeaseOwnerID,
			&g.Epoch, &g.Generation, &g.NodeSeenAt, &g.LeaseExpiresAt, &assigned, &created, &updated)
	if err != nil || ctx.Err() != nil || g.SlotID != slotID || !validRuntimeEnrollmentGrant(g, now, maxNodeAge) || !validBindingHistory(assigned, created, updated, now) {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	g.NodeSeenAt = g.NodeSeenAt.UTC()
	g.LeaseExpiresAt = g.LeaseExpiresAt.UTC()
	return g, nil
}

func (r *MemoryRepository) ReadRuntimeEnrollmentBinding(ctx context.Context, slotID string, now time.Time, maxNodeAge time.Duration) (contracts.Grant, error) {
	if r == nil || !validBindingRead(ctx, slotID, now, maxNodeAge) {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ctx.Err() != nil {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	s, ok := r.slots[slotID]
	if !ok || s.ID != slotID || s.DesiredState != "ready" || s.DesiredGeneration == 0 {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	var a Assignment
	found := false
	for _, candidate := range r.assignments[slotID] {
		if candidate.ReleasedAt == nil {
			if found {
				return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
			}
			a = candidate
			found = true
		}
	}
	if !found || a.SlotID != slotID || a.DesiredGeneration != s.DesiredGeneration || a.ImageDigest != s.ImageDigest {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	n, ok := r.nodes[a.NodeID]
	if !ok || n.ID != a.NodeID || n.Status != "connected" || n.LastSeenAt == nil {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	l, ok := r.executionLeases[executionLeaseKey(slotID, a.ExecutionEpoch)]
	if !ok || l.SlotID != slotID || l.NodeID != a.NodeID || l.ExecutionEpoch != a.ExecutionEpoch || l.RevokedAt != nil || !validBindingHistory(a.AssignedAt, l.CreatedAt, l.UpdatedAt, now) {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	g := contracts.Grant{AssignmentID: a.ID, AccountID: s.AccountID, SlotID: slotID, NodeID: n.ID, ImageDigest: a.ImageDigest, ControlSessionID: n.ControlSessionID,
		LeaseOwnerID: l.OwnerID, Epoch: a.ExecutionEpoch, Generation: a.DesiredGeneration, NodeSeenAt: n.LastSeenAt.UTC(), LeaseExpiresAt: l.ExpiresAt.UTC()}
	if ctx.Err() != nil || !validRuntimeEnrollmentGrant(g, now, maxNodeAge) {
		return contracts.Grant{}, ErrRuntimeEnrollmentBindingUnavailable
	}
	return g, nil
}

func validRuntimeEnrollmentGrant(g contracts.Grant, now time.Time, maxNodeAge time.Duration) bool {
	for _, id := range []string{g.AssignmentID, g.AccountID, g.LeaseOwnerID} {
		if credential.ValidateTransportID(id) != nil {
			return false
		}
	}
	return g.RuntimeBinding().Validate() == nil && controlSessionIDPattern.MatchString(g.ControlSessionID) && runtimeImageDigestPattern.MatchString(g.ImageDigest) &&
		!g.NodeSeenAt.IsZero() && !g.NodeSeenAt.After(now) && g.NodeSeenAt.After(now.Add(-maxNodeAge)) && g.LeaseExpiresAt.After(now)
}
