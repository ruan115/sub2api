package store

import (
	"context"
	"strconv"
	"time"
)

func (r *MemoryRepository) GrantProxyReservation(_ context.Context, candidate ProxyReservationGrant) (ProxyReservationGrant, error) {
	if validateProxyReservationGrant(candidate) != nil {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, exists := r.proxyReservations[candidate.ReservationID]; exists {
		if !sameProxyReservationGrantIdentity(existing, candidate) {
			return ProxyReservationGrant{}, ErrProxyReservationConflict
		}
		return cloneProxyReservationGrant(existing), nil
	}
	scope := proxyReservationScope(candidate.AccountID, candidate.DesiredGeneration)
	if existingID := r.proxyReservationByScope[scope]; existingID != "" {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	if existingID := r.proxyReservationByGrant[candidate.GrantEventID]; existingID != "" {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	candidate.CreatedAt = candidate.CreatedAt.UTC()
	candidate.UpdatedAt = candidate.UpdatedAt.UTC()
	r.proxyReservations[candidate.ReservationID] = candidate
	r.proxyReservationByScope[scope] = candidate.ReservationID
	r.proxyReservationByGrant[candidate.GrantEventID] = candidate.ReservationID
	return cloneProxyReservationGrant(candidate), nil
}

func (r *MemoryRepository) RevokeProxyReservation(_ context.Context, candidate ProxyReservationRevocation) (ProxyReservationGrant, error) {
	if validateProxyReservationRevocation(candidate) != nil {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	stored, exists := r.proxyReservations[candidate.ReservationID]
	if !exists {
		return ProxyReservationGrant{}, ErrProxyReservationNotFound
	}
	if !sameProxyReservationRevocationBinding(stored, candidate) || candidate.RevokedAt.Before(stored.CreatedAt) {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	if stored.RevokedAt != nil {
		if stored.RevokeEventID != candidate.RevokeEventID || !stored.RevokedAt.Equal(candidate.RevokedAt) {
			return ProxyReservationGrant{}, ErrProxyReservationConflict
		}
		return cloneProxyReservationGrant(stored), nil
	}
	if existingID := r.proxyReservationByRevoke[candidate.RevokeEventID]; existingID != "" {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	revokedAt := candidate.RevokedAt.UTC()
	stored.RevokeEventID = candidate.RevokeEventID
	stored.RevokedAt = &revokedAt
	stored.UpdatedAt = revokedAt
	r.proxyReservations[candidate.ReservationID] = stored
	r.proxyReservationByRevoke[candidate.RevokeEventID] = candidate.ReservationID
	return cloneProxyReservationGrant(stored), nil
}

func (r *MemoryRepository) GetProxyReservation(_ context.Context, reservationID string) (ProxyReservationGrant, error) {
	if ValidateProxyReservationOpaqueID(reservationID) != nil {
		return ProxyReservationGrant{}, ErrProxyReservationNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	grant, exists := r.proxyReservations[reservationID]
	if !exists {
		return ProxyReservationGrant{}, ErrProxyReservationNotFound
	}
	return cloneProxyReservationGrant(grant), nil
}

func (r *MemoryRepository) ValidateCurrentProxyReservation(
	_ context.Context,
	accountID string,
	desiredGeneration uint64,
	reservationID string,
	bindingRevision uint64,
	checkedAt time.Time,
) error {
	if validateCurrentProxyReservationInput(accountID, desiredGeneration, reservationID, bindingRevision, checkedAt) != nil {
		return ErrProxyReservationNotFound
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	grant, exists := r.proxyReservations[reservationID]
	if !exists || grant.AccountID != accountID || grant.DesiredGeneration != desiredGeneration ||
		grant.BindingRevision != bindingRevision || grant.RevokedAt != nil || grant.CreatedAt.After(checkedAt) {
		return ErrProxyReservationNotFound
	}
	return nil
}

func cloneProxyReservationGrant(grant ProxyReservationGrant) ProxyReservationGrant {
	if grant.RevokedAt != nil {
		value := *grant.RevokedAt
		grant.RevokedAt = &value
	}
	return grant
}

func proxyReservationScope(accountID string, desiredGeneration uint64) string {
	return accountID + "\x00" + strconv.FormatUint(desiredGeneration, 10)
}

var _ ProxyReservationGrantRepository = (*MemoryRepository)(nil)
