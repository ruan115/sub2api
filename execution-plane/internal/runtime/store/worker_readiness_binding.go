package store

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

// ErrWorkerReadinessBindingUnavailable deliberately exposes neither repository
// errors nor partial runtime, credential or proxy metadata.
var ErrWorkerReadinessBindingUnavailable = errors.New("worker readiness binding is unavailable")

// WorkerReadinessBinding is expected loaded-state metadata, not a worker health
// assertion or execution permission. It must not authorize first health or
// activation requests: those may legitimately have no active version/proxy yet.
// Callers separately check live authenticated control state and the Redis lease,
// and re-read authority after obtaining a worker report.
type WorkerReadinessBinding struct {
	ExecutionBinding
	CredentialVersionID                              string
	CredentialVersionNumber                          uint64
	AuthType                                         string
	ProxyLeaseID, ProxyReservationID, ProxyBindingID string
	ProxyBindingRevision                             uint64
}

type WorkerReadinessBindingRepository interface {
	ReadWorkerReadinessBinding(ctx context.Context, slotID string, checkedAt time.Time, maxAge time.Duration) (WorkerReadinessBinding, error)
}

// One consistent, read-only SELECT. In particular, version selection follows
// the account's active pointer, not MAX(version_number) or a past operation;
// proxy selection follows the unique slot/epoch binding, not a caller ID.
// The indexed equalities are additionally byte-exact to reject padded IDs.
const readWorkerReadinessBindingSQL = `
SELECT s.account_id, s.slot_id, sa.node_id, sa.provider_ref, el.owner_id,
       n.control_session_id, sa.image_digest, sa.execution_epoch, sa.desired_generation,
       sa.last_observed_at, n.last_seen_at, el.expires_at,
       sa.assigned_at, el.created_at, el.updated_at,
       v.version_id, v.version_number, cv.auth_type,
       pl.proxy_lease_id, prg.reservation_id, prg.proxy_binding_id, prg.binding_revision,
       cv.created_at, cv.updated_at, v.created_at, pl.created_at, pl.updated_at,
       prg.created_at, prg.updated_at
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id
  AND BINARY sa.slot_id = BINARY s.slot_id AND sa.released_at IS NULL
JOIN nodes n ON n.node_id = sa.node_id AND BINARY n.node_id = BINARY sa.node_id
JOIN execution_leases el ON el.slot_id = sa.slot_id AND BINARY el.slot_id = BINARY sa.slot_id
  AND el.execution_epoch = sa.execution_epoch
  AND el.node_id = sa.node_id AND BINARY el.node_id = BINARY sa.node_id
JOIN credential_vault cv ON cv.account_id = s.account_id AND BINARY cv.account_id = BINARY s.account_id
JOIN credential_versions v ON v.version_id = cv.active_version_id
  AND BINARY v.version_id = BINARY cv.active_version_id
  AND v.account_id = cv.account_id AND BINARY v.account_id = BINARY cv.account_id
JOIN proxy_leases pl ON pl.slot_id = sa.slot_id AND BINARY pl.slot_id = BINARY sa.slot_id
  AND pl.execution_epoch = sa.execution_epoch
  AND pl.account_id = s.account_id AND BINARY pl.account_id = BINARY s.account_id
  AND pl.desired_generation = s.desired_generation
JOIN proxy_reservation_grants prg ON prg.reservation_id = pl.reservation_id
  AND BINARY prg.reservation_id = BINARY pl.reservation_id
  AND prg.account_id = pl.account_id AND BINARY prg.account_id = BINARY pl.account_id
  AND prg.desired_generation = pl.desired_generation AND prg.binding_revision = pl.binding_revision
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
  AND el.expires_at > ?
  AND cv.active_version_id IS NOT NULL AND v.version_number > 0
  AND pl.revoked_at IS NULL AND prg.revoked_at IS NULL AND prg.revoke_event_id IS NULL`

type workerReadinessHistory struct {
	assignedAt, leaseCreatedAt, leaseUpdatedAt       time.Time
	vaultCreatedAt, vaultUpdatedAt, versionCreatedAt time.Time
	proxyCreatedAt, proxyUpdatedAt                   time.Time
	reservationCreatedAt, reservationUpdatedAt       time.Time
}

func (r *Repository) ReadWorkerReadinessBinding(ctx context.Context, slotID string, checkedAt time.Time, maxAge time.Duration) (WorkerReadinessBinding, error) {
	if r == nil || r.db == nil || !validBindingRead(ctx, slotID, checkedAt, maxAge) {
		return WorkerReadinessBinding{}, ErrWorkerReadinessBindingUnavailable
	}
	checkedAt = checkedAt.UTC()
	cutoff := checkedAt.Add(-maxAge)
	var binding WorkerReadinessBinding
	var history workerReadinessHistory
	err := r.db.QueryRowContext(ctx, readWorkerReadinessBindingSQL,
		slotID, cutoff, checkedAt, cutoff, checkedAt, checkedAt, checkedAt, checkedAt, checkedAt,
	).Scan(&binding.AccountID, &binding.SlotID, &binding.NodeID, &binding.ProviderRef,
		&binding.LeaseOwnerID, &binding.ControlSessionID, &binding.ImageDigest,
		&binding.ExecutionEpoch, &binding.RouteGeneration,
		&binding.ObservedAt, &binding.NodeSeenAt, &binding.LeaseExpiresAt,
		&history.assignedAt, &history.leaseCreatedAt, &history.leaseUpdatedAt,
		&binding.CredentialVersionID, &binding.CredentialVersionNumber, &binding.AuthType,
		&binding.ProxyLeaseID, &binding.ProxyReservationID, &binding.ProxyBindingID, &binding.ProxyBindingRevision,
		&history.vaultCreatedAt, &history.vaultUpdatedAt, &history.versionCreatedAt,
		&history.proxyCreatedAt, &history.proxyUpdatedAt,
		&history.reservationCreatedAt, &history.reservationUpdatedAt)
	if err != nil || ctx.Err() != nil || binding.SlotID != slotID ||
		!validWorkerReadinessBinding(binding, checkedAt, maxAge) || !validWorkerReadinessHistory(history, checkedAt) {
		return WorkerReadinessBinding{}, ErrWorkerReadinessBindingUnavailable
	}
	binding.ExecutionBinding = utcExecutionBinding(binding.ExecutionBinding)
	return binding, nil
}

func (r *MemoryRepository) ReadWorkerReadinessBinding(ctx context.Context, slotID string, checkedAt time.Time, maxAge time.Duration) (WorkerReadinessBinding, error) {
	if r == nil || !validBindingRead(ctx, slotID, checkedAt, maxAge) {
		return WorkerReadinessBinding{}, ErrWorkerReadinessBindingUnavailable
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	execution, err := r.readExecutionBindingLocked(ctx, slotID, checkedAt, maxAge)
	if err != nil {
		return WorkerReadinessBinding{}, ErrWorkerReadinessBindingUnavailable
	}
	vault, vaultExists := r.credentialVaults[execution.AccountID]
	version, versionExists := r.credentialVersions[vault.ActiveVersionID]
	proxyID := r.proxyLeaseIDsByEpoch[executionLeaseKey(slotID, execution.ExecutionEpoch)]
	proxy, proxyExists := r.proxyLeases[proxyID]
	reservation, reservationExists := r.proxyReservations[proxy.ReservationID]
	if !vaultExists || !versionExists || version.ID != vault.ActiveVersionID || version.AccountID != execution.AccountID ||
		!proxyExists || proxy.ID != proxyID || proxy.SlotID != slotID || proxy.ExecutionEpoch != execution.ExecutionEpoch ||
		proxy.AccountID != execution.AccountID || proxy.DesiredGeneration != execution.RouteGeneration || proxy.RevokedAt != nil ||
		!reservationExists || reservation.ReservationID != proxy.ReservationID || reservation.AccountID != execution.AccountID ||
		reservation.DesiredGeneration != execution.RouteGeneration || reservation.BindingRevision != proxy.BindingRevision ||
		reservation.RevokedAt != nil || reservation.RevokeEventID != "" {
		return WorkerReadinessBinding{}, ErrWorkerReadinessBindingUnavailable
	}
	binding := WorkerReadinessBinding{
		ExecutionBinding: execution, CredentialVersionID: version.ID,
		CredentialVersionNumber: version.VersionNumber, AuthType: vault.AuthType,
		ProxyLeaseID: proxy.ID, ProxyReservationID: reservation.ReservationID,
		ProxyBindingID: reservation.ProxyBindingID, ProxyBindingRevision: reservation.BindingRevision,
	}
	// The B1 helper already checked assignment/execution-lease history. This
	// projection never inspects or clones VersionRecord.Envelope or Hint.
	if ctx.Err() != nil || !validWorkerReadinessBinding(binding, checkedAt, maxAge) ||
		!validReadinessMetadataHistory(workerReadinessHistory{
			vaultCreatedAt: vault.CreatedAt, vaultUpdatedAt: vault.UpdatedAt, versionCreatedAt: version.CreatedAt,
			proxyCreatedAt: proxy.CreatedAt, proxyUpdatedAt: proxy.UpdatedAt,
			reservationCreatedAt: reservation.CreatedAt, reservationUpdatedAt: reservation.UpdatedAt,
		}, checkedAt) {
		return WorkerReadinessBinding{}, ErrWorkerReadinessBindingUnavailable
	}
	return binding, nil
}

func validWorkerReadinessBinding(binding WorkerReadinessBinding, checkedAt time.Time, maxAge time.Duration) bool {
	if !validExecutionBinding(binding.ExecutionBinding, checkedAt, maxAge) ||
		credential.ValidateTransportID(binding.CredentialVersionID) != nil || binding.CredentialVersionNumber == 0 ||
		credential.ValidateTransportID(binding.ProxyLeaseID) != nil ||
		ValidateProxyReservationOpaqueID(binding.AccountID) != nil || ValidateProxyReservationOpaqueID(binding.ProxyReservationID) != nil ||
		ValidateProxyBindingID(binding.ProxyBindingID) != nil || binding.ProxyBindingRevision == 0 {
		return false
	}
	switch binding.AuthType {
	case "oauth", "setup_token", "api_key":
		return true
	default:
		return false
	}
}

func validWorkerReadinessHistory(history workerReadinessHistory, checkedAt time.Time) bool {
	return validBindingHistory(history.assignedAt, history.leaseCreatedAt, history.leaseUpdatedAt, checkedAt) &&
		validReadinessMetadataHistory(history, checkedAt)
}

func validReadinessMetadataHistory(history workerReadinessHistory, checkedAt time.Time) bool {
	return validReadinessRecordHistory(history.vaultCreatedAt, history.vaultUpdatedAt, checkedAt) &&
		!history.versionCreatedAt.IsZero() && !history.versionCreatedAt.Before(history.vaultCreatedAt) &&
		!history.versionCreatedAt.After(history.vaultUpdatedAt) &&
		validReadinessRecordHistory(history.proxyCreatedAt, history.proxyUpdatedAt, checkedAt) &&
		validReadinessRecordHistory(history.reservationCreatedAt, history.reservationUpdatedAt, checkedAt) &&
		!history.reservationCreatedAt.After(history.proxyCreatedAt)
}

func validReadinessRecordHistory(createdAt, updatedAt, checkedAt time.Time) bool {
	return !createdAt.IsZero() && !updatedAt.IsZero() && !createdAt.After(checkedAt) &&
		!updatedAt.Before(createdAt) && !updatedAt.After(checkedAt)
}

var _ WorkerReadinessBindingRepository = (*Repository)(nil)
var _ WorkerReadinessBindingRepository = (*MemoryRepository)(nil)
