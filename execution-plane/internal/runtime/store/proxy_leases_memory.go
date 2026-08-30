package store

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

func (r *MemoryRepository) GrantProxyLease(_ context.Context, candidate ProxyLease) (ProxyLease, error) {
	candidate.CreatedAt = canonicalRuntimeTime(candidate.CreatedAt)
	candidate.UpdatedAt = canonicalRuntimeTime(candidate.UpdatedAt)
	if validateProxyLease(candidate) != nil {
		return ProxyLease{}, ErrProxyLeaseConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot, slotExists := r.slots[candidate.SlotID]
	assignment, assignmentExists := activeMemoryAssignment(r.assignments[candidate.SlotID])
	executionLease, executionLeaseExists := r.executionLeases[executionLeaseKey(candidate.SlotID, candidate.ExecutionEpoch)]
	reservation, reservationExists := r.proxyReservations[candidate.ReservationID]
	if !slotExists || slot.AccountID != candidate.AccountID || slot.DesiredState != "ready" ||
		slot.DesiredGeneration != candidate.DesiredGeneration || !assignmentExists || assignment.ExecutionEpoch != candidate.ExecutionEpoch ||
		assignment.DesiredGeneration != slot.DesiredGeneration || assignment.ActualState != "running" ||
		!assignment.Healthy || assignment.ImageDigest != slot.ImageDigest ||
		!executionLeaseExists || executionLease.NodeID != assignment.NodeID || executionLease.RevokedAt != nil ||
		!executionLease.ExpiresAt.After(candidate.CreatedAt) || executionLease.CreatedAt.After(candidate.CreatedAt) || !reservationExists ||
		reservation.AccountID != candidate.AccountID || reservation.DesiredGeneration != candidate.DesiredGeneration ||
		reservation.BindingRevision != candidate.BindingRevision || reservation.RevokedAt != nil ||
		reservation.CreatedAt.After(candidate.CreatedAt) {
		return ProxyLease{}, ErrProxyLeaseConflict
	}
	if existing, exists := r.proxyLeases[candidate.ID]; exists {
		if !sameProxyLeaseBinding(existing, candidate) || existing.RevokedAt != nil {
			return ProxyLease{}, ErrProxyLeaseConflict
		}
		return cloneProxyLease(existing), nil
	}
	key := executionLeaseKey(candidate.SlotID, candidate.ExecutionEpoch)
	if existingID := r.proxyLeaseIDsByEpoch[key]; existingID != "" {
		return ProxyLease{}, ErrProxyLeaseConflict
	}
	r.proxyLeases[candidate.ID] = candidate
	r.proxyLeaseIDsByEpoch[key] = candidate.ID
	return cloneProxyLease(candidate), nil
}

func (r *MemoryRepository) RevokeProxyLease(_ context.Context, proxyLeaseID string, revokedAt time.Time) error {
	revokedAt = canonicalRuntimeTime(revokedAt)
	if credential.ValidateTransportID(proxyLeaseID) != nil || revokedAt.IsZero() {
		return ErrProxyLeaseNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lease, exists := r.proxyLeases[proxyLeaseID]
	if !exists {
		return ErrProxyLeaseNotFound
	}
	if revokedAt.Before(lease.CreatedAt) {
		return ErrProxyLeaseNotFound
	}
	if lease.RevokedAt == nil {
		revoked := revokedAt.UTC()
		lease.RevokedAt = &revoked
		lease.UpdatedAt = revoked
		r.proxyLeases[proxyLeaseID] = lease
	}
	return nil
}

func (r *MemoryRepository) GetProxyLease(_ context.Context, proxyLeaseID string) (ProxyLease, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	lease, exists := r.proxyLeases[proxyLeaseID]
	if !exists {
		return ProxyLease{}, ErrProxyLeaseNotFound
	}
	return cloneProxyLease(lease), nil
}

func (r *MemoryRepository) ValidateCurrentProxyLease(
	_ context.Context,
	accountBinding, slotID string,
	executionEpoch uint64,
	proxyLeaseID string,
	checkedAt time.Time,
) error {
	checkedAt = canonicalRuntimeTime(checkedAt)
	if validateCurrentProxyLeaseInput(accountBinding, slotID, executionEpoch, proxyLeaseID, checkedAt) != nil {
		return ErrProxyLeaseNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	proxyLease, exists := r.proxyLeases[proxyLeaseID]
	slot, slotExists := r.slots[slotID]
	assignment, assignmentExists := activeMemoryAssignment(r.assignments[slotID])
	executionLease, executionLeaseExists := r.executionLeases[executionLeaseKey(slotID, executionEpoch)]
	reservation, reservationExists := r.proxyReservations[proxyLease.ReservationID]
	if !exists || proxyLease.RevokedAt != nil || proxyLease.SlotID != slotID || proxyLease.ExecutionEpoch != executionEpoch ||
		proxyLease.CreatedAt.After(checkedAt) ||
		!slotExists || slot.AccountID != proxyLease.AccountID || slot.DesiredState != "ready" ||
		slot.DesiredGeneration != proxyLease.DesiredGeneration || provider.RuntimeAccountID(proxyLease.AccountID) != accountBinding ||
		!assignmentExists || assignment.ExecutionEpoch != executionEpoch || assignment.DesiredGeneration != slot.DesiredGeneration ||
		assignment.ActualState != "running" ||
		!assignment.Healthy || assignment.ImageDigest != slot.ImageDigest || !executionLeaseExists ||
		executionLease.NodeID != assignment.NodeID || executionLease.RevokedAt != nil || !executionLease.ExpiresAt.After(checkedAt) ||
		executionLease.CreatedAt.After(checkedAt) ||
		!reservationExists || reservation.AccountID != proxyLease.AccountID ||
		reservation.DesiredGeneration != proxyLease.DesiredGeneration || reservation.BindingRevision != proxyLease.BindingRevision ||
		reservation.RevokedAt != nil || reservation.CreatedAt.After(checkedAt) {
		return ErrProxyLeaseNotFound
	}
	return nil
}

func cloneProxyLease(lease ProxyLease) ProxyLease {
	if lease.RevokedAt != nil {
		value := *lease.RevokedAt
		lease.RevokedAt = &value
	}
	return lease
}

var _ ProxyLeaseRepository = (*MemoryRepository)(nil)
