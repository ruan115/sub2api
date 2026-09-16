package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

var ErrProbeBindingUnavailable = errors.New("probe binding is unavailable")

// ProbeBinding authorizes no execution. It is only a candidate for an INSPECT
// of an existing assignment; observations may be absent, stale or unhealthy.
// Callers must separately verify the live control session and independent lease.
type ProbeBinding struct {
	AccountID, SlotID, NodeID, ProviderRef, LeaseOwnerID, ControlSessionID, ImageDigest string
	ExecutionEpoch, RouteGeneration                                                     uint64
	NodeSeenAt, LeaseExpiresAt                                                          time.Time
	LastObservedAt                                                                      *time.Time
	ObservedControlSessionID                                                            string
}

// A page's cursor advances over scanned rows, including invalid projections.
// An empty Bindings does not mean completion when NextAfterSlotID is nonempty.
type ProbeBindingPage struct {
	Bindings        []ProbeBinding
	NextAfterSlotID string
}

type ProbeBindingRepository interface {
	ListProbeBindings(ctx context.Context, afterSlotID string, checkedAt time.Time, maxNodeAge time.Duration, limit int) (ProbeBindingPage, error)
	ReadProbeBinding(ctx context.Context, slotID string, checkedAt time.Time, maxNodeAge time.Duration) (ProbeBinding, error)
}

// The core eligibility is filtered before LIMIT. Format/history validation is
// repeated in Go, without allowing malformed rows to stall valid later pages.
// Ordinary join predicates preserve index opportunities; BINARY rejects padded
// VARCHAR aliases. Neither worker readiness nor observation freshness is required.
const probeBindingSQL = `
SELECT s.account_id, s.slot_id, sa.node_id, sa.provider_ref, el.owner_id,
       n.control_session_id, sa.image_digest, sa.execution_epoch, sa.desired_generation,
       n.last_seen_at, el.expires_at, sa.last_observed_at, sa.observed_control_session_id,
       sa.assigned_at, el.created_at, el.updated_at
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id
  AND BINARY sa.slot_id = BINARY s.slot_id AND sa.released_at IS NULL
JOIN nodes n ON n.node_id = sa.node_id AND BINARY n.node_id = BINARY sa.node_id
JOIN execution_leases el ON el.slot_id = sa.slot_id AND BINARY el.slot_id = BINARY sa.slot_id
  AND el.execution_epoch = sa.execution_epoch
  AND el.node_id = sa.node_id AND BINARY el.node_id = BINARY sa.node_id
WHERE s.desired_state = 'ready'
  AND s.desired_generation > 0 AND sa.desired_generation = s.desired_generation
  AND sa.execution_epoch > 0 AND BINARY sa.image_digest = BINARY s.image_digest
  AND sa.provider_ref IS NOT NULL AND sa.provider_ref <> ''
  AND n.status = 'connected' AND n.control_session_id IS NOT NULL AND n.control_session_id <> ''
  AND n.last_seen_at > ? AND n.last_seen_at <= ?
  AND (sa.last_observed_at IS NULL OR sa.last_observed_at <= ?)
  AND sa.assigned_at <= ?
  AND el.revoked_at IS NULL AND el.created_at <= ? AND el.updated_at <= ?
  AND el.expires_at > ?`

type probeBindingRecord struct {
	binding                                    ProbeBinding
	assignedAt, leaseCreatedAt, leaseUpdatedAt time.Time
}

func (r *Repository) ReadProbeBinding(ctx context.Context, slotID string, checkedAt time.Time, maxNodeAge time.Duration) (ProbeBinding, error) {
	if r == nil || r.db == nil || !validBindingRead(ctx, slotID, checkedAt, maxNodeAge) {
		return ProbeBinding{}, ErrProbeBindingUnavailable
	}
	args := append(probeBindingTimeArgs(checkedAt, maxNodeAge), slotID)
	record, err := scanProbeBinding(r.db.QueryRowContext(ctx, probeBindingSQL+` AND s.slot_id = ?`, args...))
	if err != nil || ctx.Err() != nil || record.binding.SlotID != slotID || !validProbeBinding(record, checkedAt, maxNodeAge) {
		return ProbeBinding{}, ErrProbeBindingUnavailable
	}
	return cloneProbeBinding(record.binding), nil
}

func (r *Repository) ListProbeBindings(ctx context.Context, afterSlotID string, checkedAt time.Time, maxNodeAge time.Duration, limit int) (ProbeBindingPage, error) {
	if r == nil || r.db == nil || !validProbeList(ctx, afterSlotID, checkedAt, maxNodeAge, limit) {
		return ProbeBindingPage{}, ErrProbeBindingUnavailable
	}
	args := append(probeBindingTimeArgs(checkedAt, maxNodeAge), afterSlotID, limit)
	rows, err := r.db.QueryContext(ctx, probeBindingSQL+`
  AND BINARY s.slot_id > BINARY ? ORDER BY BINARY s.slot_id LIMIT ?`, args...)
	if err != nil {
		return ProbeBindingPage{}, ErrProbeBindingUnavailable
	}
	defer rows.Close()
	records := make([]probeBindingRecord, 0, limit)
	for rows.Next() {
		if len(records) >= limit || ctx.Err() != nil {
			return ProbeBindingPage{}, ErrProbeBindingUnavailable
		}
		record, err := scanProbeBinding(rows)
		if err != nil {
			return ProbeBindingPage{}, ErrProbeBindingUnavailable
		}
		records = append(records, record)
	}
	if rows.Err() != nil || rows.Close() != nil || ctx.Err() != nil {
		return ProbeBindingPage{}, ErrProbeBindingUnavailable
	}
	return buildProbePage(records, afterSlotID, checkedAt, maxNodeAge, limit)
}

func (r *MemoryRepository) ReadProbeBinding(ctx context.Context, slotID string, checkedAt time.Time, maxNodeAge time.Duration) (ProbeBinding, error) {
	if r == nil || !validBindingRead(ctx, slotID, checkedAt, maxNodeAge) {
		return ProbeBinding{}, ErrProbeBindingUnavailable
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	record, exists := r.probeBindingLocked(slotID, checkedAt, maxNodeAge)
	if !exists || ctx.Err() != nil || !validProbeBinding(record, checkedAt, maxNodeAge) {
		return ProbeBinding{}, ErrProbeBindingUnavailable
	}
	return cloneProbeBinding(record.binding), nil
}

func (r *MemoryRepository) ListProbeBindings(ctx context.Context, afterSlotID string, checkedAt time.Time, maxNodeAge time.Duration, limit int) (ProbeBindingPage, error) {
	if r == nil || !validProbeList(ctx, afterSlotID, checkedAt, maxNodeAge, limit) {
		return ProbeBindingPage{}, ErrProbeBindingUnavailable
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	// Memory has no sorted index. Keep only the first limit eligible rows,
	// bounding the page allocation even when the repository is much larger.
	records := make([]probeBindingRecord, 0, limit)
	for slotID := range r.slots {
		if ctx.Err() != nil {
			return ProbeBindingPage{}, ErrProbeBindingUnavailable
		}
		if slotID <= afterSlotID {
			continue
		}
		record, exists := r.probeBindingLocked(slotID, checkedAt, maxNodeAge)
		if !exists {
			continue
		}
		index := sort.Search(len(records), func(i int) bool { return records[i].binding.SlotID >= slotID })
		if index >= limit {
			continue
		}
		if len(records) < limit {
			records = append(records, probeBindingRecord{})
		}
		copy(records[index+1:], records[index:len(records)-1])
		records[index] = record
	}
	if ctx.Err() != nil {
		return ProbeBindingPage{}, ErrProbeBindingUnavailable
	}
	return buildProbePage(records, afterSlotID, checkedAt, maxNodeAge, limit)
}

// Called with r.mu held; this mirrors the SQL predicates before format filtering.
func (r *MemoryRepository) probeBindingLocked(slotID string, checkedAt time.Time, maxNodeAge time.Duration) (probeBindingRecord, bool) {
	slot, exists := r.slots[slotID]
	if !exists || slot.ID != slotID || slot.DesiredState != "ready" || slot.DesiredGeneration == 0 {
		return probeBindingRecord{}, false
	}
	var assignment Assignment
	active := false
	for _, candidate := range r.assignments[slotID] {
		if candidate.ReleasedAt == nil {
			if active {
				return probeBindingRecord{}, false
			}
			assignment, active = candidate, true
		}
	}
	if !active || assignment.SlotID != slotID || assignment.ExecutionEpoch == 0 || assignment.ProviderRef == "" ||
		assignment.DesiredGeneration != slot.DesiredGeneration || assignment.ImageDigest != slot.ImageDigest || assignment.AssignedAt.After(checkedAt) ||
		assignment.LastObservedAt != nil && assignment.LastObservedAt.After(checkedAt) {
		return probeBindingRecord{}, false
	}
	node, exists := r.nodes[assignment.NodeID]
	if !exists || node.ID != assignment.NodeID || node.Status != "connected" || node.ControlSessionID == "" || node.LastSeenAt == nil ||
		!node.LastSeenAt.After(checkedAt.Add(-maxNodeAge)) || node.LastSeenAt.After(checkedAt) {
		return probeBindingRecord{}, false
	}
	lease, exists := r.executionLeases[executionLeaseKey(slotID, assignment.ExecutionEpoch)]
	if !exists || lease.SlotID != slotID || lease.NodeID != assignment.NodeID || lease.ExecutionEpoch != assignment.ExecutionEpoch ||
		lease.RevokedAt != nil || !lease.ExpiresAt.After(checkedAt) || lease.CreatedAt.After(checkedAt) || lease.UpdatedAt.After(checkedAt) {
		return probeBindingRecord{}, false
	}
	return probeBindingRecord{binding: ProbeBinding{
		AccountID: slot.AccountID, SlotID: slotID, NodeID: node.ID, ProviderRef: assignment.ProviderRef,
		LeaseOwnerID: lease.OwnerID, ControlSessionID: node.ControlSessionID, ImageDigest: assignment.ImageDigest,
		ExecutionEpoch: assignment.ExecutionEpoch, RouteGeneration: assignment.DesiredGeneration,
		NodeSeenAt: *node.LastSeenAt, LeaseExpiresAt: lease.ExpiresAt,
		LastObservedAt: assignment.LastObservedAt, ObservedControlSessionID: assignment.ObservedControlSessionID,
	}, assignedAt: assignment.AssignedAt, leaseCreatedAt: lease.CreatedAt, leaseUpdatedAt: lease.UpdatedAt}, true
}

func probeBindingTimeArgs(checkedAt time.Time, maxNodeAge time.Duration) []any {
	now := checkedAt.UTC()
	return []any{now.Add(-maxNodeAge), now, now, now, now, now, now}
}

func scanProbeBinding(row rowScanner) (probeBindingRecord, error) {
	var record probeBindingRecord
	var observedAt sql.NullTime
	var observedSession sql.NullString
	binding := &record.binding
	err := row.Scan(&binding.AccountID, &binding.SlotID, &binding.NodeID, &binding.ProviderRef, &binding.LeaseOwnerID,
		&binding.ControlSessionID, &binding.ImageDigest, &binding.ExecutionEpoch, &binding.RouteGeneration,
		&binding.NodeSeenAt, &binding.LeaseExpiresAt, &observedAt, &observedSession,
		&record.assignedAt, &record.leaseCreatedAt, &record.leaseUpdatedAt)
	if observedAt.Valid {
		value := observedAt.Time.UTC()
		binding.LastObservedAt = &value
	}
	binding.ObservedControlSessionID = observedSession.String
	return record, err
}

func validProbeList(ctx context.Context, afterSlotID string, checkedAt time.Time, maxNodeAge time.Duration, limit int) bool {
	return ctx != nil && ctx.Err() == nil && !checkedAt.IsZero() && maxNodeAge > 0 && maxNodeAge <= 45*time.Second &&
		limit > 0 && limit <= 100 && (afterSlotID == "" || credential.ValidateTransportID(afterSlotID) == nil)
}

func validProbeBinding(record probeBindingRecord, checkedAt time.Time, maxNodeAge time.Duration) bool {
	binding := record.binding
	for _, id := range []string{binding.AccountID, binding.SlotID, binding.NodeID, binding.LeaseOwnerID} {
		if credential.ValidateTransportID(id) != nil {
			return false
		}
	}
	if !controlSessionIDPattern.MatchString(binding.ControlSessionID) ||
		(binding.ObservedControlSessionID != "" && !controlSessionIDPattern.MatchString(binding.ObservedControlSessionID)) ||
		binding.ExecutionEpoch == 0 || binding.RouteGeneration == 0 || !runtimeImageDigestPattern.MatchString(binding.ImageDigest) ||
		!validBindingProviderRef(binding.ProviderRef) || !validBindingHistory(record.assignedAt, record.leaseCreatedAt, record.leaseUpdatedAt, checkedAt) {
		return false
	}
	return !binding.NodeSeenAt.IsZero() && binding.NodeSeenAt.After(checkedAt.Add(-maxNodeAge)) && !binding.NodeSeenAt.After(checkedAt) &&
		binding.LeaseExpiresAt.After(checkedAt) &&
		(binding.LastObservedAt == nil || !binding.LastObservedAt.IsZero() && !binding.LastObservedAt.After(checkedAt))
}

func buildProbePage(records []probeBindingRecord, afterSlotID string, checkedAt time.Time, maxNodeAge time.Duration, limit int) (ProbeBindingPage, error) {
	page := ProbeBindingPage{Bindings: make([]ProbeBinding, 0, len(records))}
	last := afterSlotID
	for _, record := range records {
		if record.binding.SlotID <= last {
			return ProbeBindingPage{}, ErrProbeBindingUnavailable
		}
		last = record.binding.SlotID
		if validProbeBinding(record, checkedAt, maxNodeAge) {
			page.Bindings = append(page.Bindings, cloneProbeBinding(record.binding))
		}
	}
	if len(records) == limit {
		if credential.ValidateTransportID(last) != nil {
			return ProbeBindingPage{}, ErrProbeBindingUnavailable
		}
		page.NextAfterSlotID = last
	}
	return page, nil
}

func cloneProbeBinding(binding ProbeBinding) ProbeBinding {
	binding.NodeSeenAt = binding.NodeSeenAt.UTC()
	binding.LeaseExpiresAt = binding.LeaseExpiresAt.UTC()
	if binding.LastObservedAt != nil {
		value := binding.LastObservedAt.UTC()
		binding.LastObservedAt = &value
	}
	return binding
}

var _ ProbeBindingRepository = (*Repository)(nil)
var _ ProbeBindingRepository = (*MemoryRepository)(nil)
