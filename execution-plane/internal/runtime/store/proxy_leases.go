package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/go-sql-driver/mysql"
)

var (
	ErrProxyLeaseConflict = errors.New("proxy lease conflicts with the current runtime binding")
	ErrProxyLeaseNotFound = errors.New("proxy lease is not current")
)

// ProxyLease is an opaque runtime grant for a fixed proxy reservation owned by
// CCMAX. It intentionally contains no proxy endpoint or credentials. Current
// validity also requires the exact slot epoch's execution lease to be current.
type ProxyLease struct {
	ID                string
	ReservationID     string
	AccountID         string
	DesiredGeneration uint64
	BindingRevision   uint64
	SlotID            string
	ExecutionEpoch    uint64
	RevokedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ProxyLeaseRepository interface {
	GrantProxyLease(ctx context.Context, candidate ProxyLease) (ProxyLease, error)
	RevokeProxyLease(ctx context.Context, proxyLeaseID string, revokedAt time.Time) error
	GetProxyLease(ctx context.Context, proxyLeaseID string) (ProxyLease, error)
	ValidateCurrentProxyLease(
		ctx context.Context,
		accountBinding, slotID string,
		executionEpoch uint64,
		proxyLeaseID string,
		checkedAt time.Time,
	) error
}

func (r *Repository) GrantProxyLease(ctx context.Context, candidate ProxyLease) (ProxyLease, error) {
	candidate.CreatedAt = canonicalRuntimeTime(candidate.CreatedAt)
	candidate.UpdatedAt = canonicalRuntimeTime(candidate.UpdatedAt)
	if validateProxyLease(candidate) != nil {
		return ProxyLease{}, ErrProxyLeaseConflict
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ProxyLease{}, fmt.Errorf("begin proxy lease grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var accountID string
	err = tx.QueryRowContext(ctx, `
SELECT s.account_id
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id AND sa.released_at IS NULL
JOIN execution_leases el ON el.slot_id = sa.slot_id AND el.execution_epoch = sa.execution_epoch
JOIN proxy_reservation_grants prg
  ON prg.reservation_id = ? AND prg.account_id = s.account_id
 AND prg.desired_generation = s.desired_generation AND prg.binding_revision = ?
WHERE s.slot_id = ? AND s.account_id = ? AND s.desired_state = 'ready' AND s.desired_generation = ?
  AND sa.execution_epoch = ? AND sa.actual_state = 'running' AND sa.healthy = TRUE
  AND sa.desired_generation = s.desired_generation AND sa.image_digest = s.image_digest
  AND el.node_id = sa.node_id AND el.revoked_at IS NULL AND el.expires_at > ? AND el.created_at <= ?
  AND prg.revoked_at IS NULL AND prg.created_at <= ?
FOR UPDATE`, candidate.ReservationID, candidate.BindingRevision, candidate.SlotID, candidate.AccountID,
		candidate.DesiredGeneration, candidate.ExecutionEpoch, candidate.CreatedAt.UTC(), candidate.CreatedAt.UTC(),
		candidate.CreatedAt.UTC()).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && accountID != candidate.AccountID) {
		return ProxyLease{}, ErrProxyLeaseConflict
	}
	if err != nil {
		return ProxyLease{}, fmt.Errorf("validate proxy lease execution binding: %w", err)
	}
	existing, err := getProxyLease(ctx, tx, candidate.ID, true)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
INSERT INTO proxy_leases (
  proxy_lease_id, reservation_id, account_id, desired_generation, binding_revision,
  slot_id, execution_epoch, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, candidate.ID, candidate.ReservationID, candidate.AccountID,
			candidate.DesiredGeneration, candidate.BindingRevision, candidate.SlotID, candidate.ExecutionEpoch,
			candidate.CreatedAt.UTC(), candidate.UpdatedAt.UTC())
		if err != nil {
			var mysqlError *mysql.MySQLError
			if errors.As(err, &mysqlError) && mysqlError.Number == 1062 {
				return ProxyLease{}, ErrProxyLeaseConflict
			}
			return ProxyLease{}, fmt.Errorf("insert proxy lease: %w", err)
		}
	case err != nil:
		return ProxyLease{}, fmt.Errorf("read proxy lease during grant: %w", err)
	default:
		if !sameProxyLeaseBinding(existing, candidate) || existing.RevokedAt != nil {
			return ProxyLease{}, ErrProxyLeaseConflict
		}
	}
	if err := tx.Commit(); err != nil {
		return ProxyLease{}, fmt.Errorf("commit proxy lease grant: %w", err)
	}
	return r.GetProxyLease(ctx, candidate.ID)
}

func (r *Repository) RevokeProxyLease(ctx context.Context, proxyLeaseID string, revokedAt time.Time) error {
	revokedAt = canonicalRuntimeTime(revokedAt)
	if credential.ValidateTransportID(proxyLeaseID) != nil || revokedAt.IsZero() {
		return ErrProxyLeaseNotFound
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE proxy_leases SET revoked_at = ?, updated_at = ?
WHERE proxy_lease_id = ? AND revoked_at IS NULL AND created_at <= ?`,
		revokedAt.UTC(), revokedAt.UTC(), proxyLeaseID, revokedAt.UTC())
	if err != nil {
		return fmt.Errorf("revoke proxy lease: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read proxy lease revocation result: %w", err)
	} else if affected == 1 {
		return nil
	}
	existing, getErr := r.GetProxyLease(ctx, proxyLeaseID)
	if getErr == nil && existing.RevokedAt != nil {
		return nil
	}
	return ErrProxyLeaseNotFound
}

func (r *Repository) GetProxyLease(ctx context.Context, proxyLeaseID string) (ProxyLease, error) {
	if credential.ValidateTransportID(proxyLeaseID) != nil {
		return ProxyLease{}, ErrProxyLeaseNotFound
	}
	lease, err := getProxyLease(ctx, r.db, proxyLeaseID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return ProxyLease{}, ErrProxyLeaseNotFound
	}
	return lease, err
}

func (r *Repository) ValidateCurrentProxyLease(
	ctx context.Context,
	accountBinding, slotID string,
	executionEpoch uint64,
	proxyLeaseID string,
	checkedAt time.Time,
) error {
	checkedAt = canonicalRuntimeTime(checkedAt)
	if validateCurrentProxyLeaseInput(accountBinding, slotID, executionEpoch, proxyLeaseID, checkedAt) != nil {
		return ErrProxyLeaseNotFound
	}
	var accountID string
	err := r.db.QueryRowContext(ctx, `
SELECT pl.account_id
FROM proxy_leases pl
JOIN slots s ON s.slot_id = pl.slot_id AND s.account_id = pl.account_id
  AND s.desired_state = 'ready' AND s.desired_generation = pl.desired_generation
JOIN slot_assignments sa ON sa.slot_id = pl.slot_id AND sa.execution_epoch = pl.execution_epoch AND sa.released_at IS NULL
JOIN execution_leases el ON el.slot_id = sa.slot_id AND el.execution_epoch = sa.execution_epoch
JOIN proxy_reservation_grants prg
  ON prg.reservation_id = pl.reservation_id AND prg.account_id = pl.account_id
 AND prg.desired_generation = pl.desired_generation AND prg.binding_revision = pl.binding_revision
WHERE pl.proxy_lease_id = ? AND pl.slot_id = ? AND pl.execution_epoch = ? AND pl.revoked_at IS NULL
  AND pl.created_at <= ?
  AND sa.actual_state = 'running' AND sa.healthy = TRUE
  AND sa.desired_generation = s.desired_generation AND sa.image_digest = s.image_digest
  AND el.node_id = sa.node_id AND el.revoked_at IS NULL AND el.expires_at > ? AND el.created_at <= ?
  AND prg.revoked_at IS NULL AND prg.created_at <= ?`,
		proxyLeaseID, slotID, executionEpoch, checkedAt, checkedAt, checkedAt, checkedAt).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProxyLeaseNotFound
	}
	if err != nil {
		return fmt.Errorf("validate current proxy lease: %w", err)
	}
	if provider.RuntimeAccountID(accountID) != accountBinding {
		return ErrProxyLeaseNotFound
	}
	return nil
}

func getProxyLease(ctx context.Context, queryer slotQueryer, proxyLeaseID string, forUpdate bool) (ProxyLease, error) {
	query := `
SELECT proxy_lease_id, reservation_id, account_id, desired_generation, binding_revision,
       slot_id, execution_epoch, revoked_at, created_at, updated_at
FROM proxy_leases WHERE proxy_lease_id = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var lease ProxyLease
	var revokedAt sql.NullTime
	err := queryer.QueryRowContext(ctx, query, proxyLeaseID).Scan(
		&lease.ID, &lease.ReservationID, &lease.AccountID, &lease.DesiredGeneration, &lease.BindingRevision,
		&lease.SlotID, &lease.ExecutionEpoch, &revokedAt, &lease.CreatedAt, &lease.UpdatedAt,
	)
	if err != nil {
		return ProxyLease{}, err
	}
	if revokedAt.Valid {
		value := revokedAt.Time.UTC()
		lease.RevokedAt = &value
	}
	lease.CreatedAt = lease.CreatedAt.UTC()
	lease.UpdatedAt = lease.UpdatedAt.UTC()
	if validateProxyLeaseStored(lease) != nil {
		return ProxyLease{}, ErrProxyLeaseConflict
	}
	return lease, nil
}

func validateProxyLease(lease ProxyLease) error {
	if credential.ValidateTransportID(lease.ID) != nil || ValidateProxyReservationOpaqueID(lease.ReservationID) != nil ||
		ValidateProxyReservationOpaqueID(lease.AccountID) != nil || credential.ValidateTransportID(lease.SlotID) != nil ||
		lease.DesiredGeneration == 0 || lease.BindingRevision == 0 || lease.ExecutionEpoch == 0 || lease.RevokedAt != nil ||
		lease.CreatedAt.IsZero() || lease.UpdatedAt.IsZero() || lease.UpdatedAt.Before(lease.CreatedAt) {
		return ErrProxyLeaseConflict
	}
	return nil
}

func validateProxyLeaseStored(lease ProxyLease) error {
	revokedAt := lease.RevokedAt
	lease.RevokedAt = nil
	if validateProxyLease(lease) != nil || revokedAt != nil && revokedAt.Before(lease.CreatedAt) {
		return ErrProxyLeaseConflict
	}
	return nil
}

func validateCurrentProxyLeaseInput(accountBinding, slotID string, executionEpoch uint64, proxyLeaseID string, checkedAt time.Time) error {
	for _, value := range []string{accountBinding, slotID, proxyLeaseID} {
		if credential.ValidateTransportID(value) != nil {
			return ErrProxyLeaseNotFound
		}
	}
	if executionEpoch == 0 || checkedAt.IsZero() {
		return ErrProxyLeaseNotFound
	}
	return nil
}

func sameProxyLeaseBinding(left, right ProxyLease) bool {
	return left.ID == right.ID && left.ReservationID == right.ReservationID && left.AccountID == right.AccountID &&
		left.DesiredGeneration == right.DesiredGeneration && left.BindingRevision == right.BindingRevision &&
		left.SlotID == right.SlotID && left.ExecutionEpoch == right.ExecutionEpoch && left.CreatedAt.Equal(right.CreatedAt)
}

// MySQL DATETIME(6) is the durable precision boundary. Canonicalizing before
// validation and persistence keeps SQL and memory exact-replay semantics
// identical when callers pass time.Now() values containing nanoseconds.
func canonicalRuntimeTime(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC().Truncate(time.Microsecond)
}

var _ ProxyLeaseRepository = (*Repository)(nil)
