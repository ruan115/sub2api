package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrExecutionLeaseConflict = errors.New("execution lease conflicts with the current assignment or owner")
	ErrExecutionLeaseNotFound = errors.New("execution lease not found")
)

type ExecutionLease struct {
	ID             string
	SlotID         string
	NodeID         string
	ExecutionEpoch uint64
	OwnerID        string
	ExpiresAt      time.Time
	RevokedAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ExecutionLeaseRepository interface {
	GrantExecutionLease(ctx context.Context, candidate ExecutionLease) (ExecutionLease, error)
	RenewExecutionLease(ctx context.Context, slotID string, executionEpoch uint64, ownerID string, expiresAt, renewedAt time.Time) error
	RevokeExecutionLease(ctx context.Context, slotID string, executionEpoch uint64, ownerID string, revokedAt time.Time) error
	GetExecutionLease(ctx context.Context, slotID string, executionEpoch uint64) (ExecutionLease, error)
}

func (r *Repository) GrantExecutionLease(ctx context.Context, candidate ExecutionLease) (ExecutionLease, error) {
	if err := validateExecutionLease(candidate); err != nil {
		return ExecutionLease{}, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ExecutionLease{}, fmt.Errorf("begin execution lease grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var assignmentNodeID string
	err = tx.QueryRowContext(ctx, `
SELECT node_id FROM slot_assignments
WHERE slot_id = ? AND execution_epoch = ? AND released_at IS NULL FOR UPDATE`,
		candidate.SlotID, candidate.ExecutionEpoch).Scan(&assignmentNodeID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && assignmentNodeID != candidate.NodeID) {
		return ExecutionLease{}, ErrExecutionLeaseConflict
	}
	if err != nil {
		return ExecutionLease{}, fmt.Errorf("lock execution lease assignment: %w", err)
	}
	existing, getErr := getExecutionLease(ctx, tx, candidate.SlotID, candidate.ExecutionEpoch, true)
	switch {
	case errors.Is(getErr, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
INSERT INTO execution_leases (
  lease_id, slot_id, node_id, execution_epoch, owner_id, expires_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, candidate.ID, candidate.SlotID, candidate.NodeID,
			candidate.ExecutionEpoch, candidate.OwnerID, candidate.ExpiresAt.UTC(), candidate.CreatedAt.UTC(), candidate.UpdatedAt.UTC())
		if err != nil {
			return ExecutionLease{}, fmt.Errorf("insert execution lease: %w", err)
		}
	case getErr != nil:
		return ExecutionLease{}, getErr
	default:
		if existing.NodeID != candidate.NodeID || existing.OwnerID != candidate.OwnerID || existing.RevokedAt != nil {
			return ExecutionLease{}, ErrExecutionLeaseConflict
		}
		_, err = tx.ExecContext(ctx, `
UPDATE execution_leases SET expires_at = ?, updated_at = ?
WHERE lease_id = ? AND revoked_at IS NULL`, candidate.ExpiresAt.UTC(), candidate.UpdatedAt.UTC(), existing.ID)
		if err != nil {
			return ExecutionLease{}, fmt.Errorf("extend idempotent execution lease: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return ExecutionLease{}, fmt.Errorf("commit execution lease grant: %w", err)
	}
	return r.GetExecutionLease(ctx, candidate.SlotID, candidate.ExecutionEpoch)
}

func (r *Repository) RenewExecutionLease(ctx context.Context, slotID string, executionEpoch uint64, ownerID string, expiresAt, renewedAt time.Time) error {
	if slotID == "" || executionEpoch == 0 || ownerID == "" || !expiresAt.After(renewedAt) || renewedAt.IsZero() {
		return errors.New("invalid execution lease renewal")
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE execution_leases SET expires_at = ?, updated_at = ?
WHERE slot_id = ? AND execution_epoch = ? AND owner_id = ? AND node_id = (
  SELECT node_id FROM slot_assignments WHERE slot_id = ? AND execution_epoch = ? AND released_at IS NULL
) AND revoked_at IS NULL`, expiresAt.UTC(), renewedAt.UTC(), slotID, executionEpoch, ownerID, slotID, executionEpoch)
	if err != nil {
		return fmt.Errorf("renew execution lease: %w", err)
	}
	if err := requireOneLease(result); err != nil {
		return err
	}
	return nil
}

func (r *Repository) RevokeExecutionLease(ctx context.Context, slotID string, executionEpoch uint64, ownerID string, revokedAt time.Time) error {
	if slotID == "" || executionEpoch == 0 || ownerID == "" || revokedAt.IsZero() {
		return errors.New("invalid execution lease revocation")
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE execution_leases SET revoked_at = ?, updated_at = ?
WHERE slot_id = ? AND execution_epoch = ? AND owner_id = ? AND revoked_at IS NULL`,
		revokedAt.UTC(), revokedAt.UTC(), slotID, executionEpoch, ownerID)
	if err != nil {
		return fmt.Errorf("revoke execution lease: %w", err)
	}
	if err := requireOneLease(result); err != nil {
		lease, getErr := r.GetExecutionLease(ctx, slotID, executionEpoch)
		if getErr == nil && lease.OwnerID == ownerID && lease.RevokedAt != nil {
			return nil
		}
		return err
	}
	return nil
}

func (r *Repository) GetExecutionLease(ctx context.Context, slotID string, executionEpoch uint64) (ExecutionLease, error) {
	lease, err := getExecutionLease(ctx, r.db, slotID, executionEpoch, false)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionLease{}, ErrExecutionLeaseNotFound
	}
	return lease, err
}

func (r *MemoryRepository) GrantExecutionLease(_ context.Context, candidate ExecutionLease) (ExecutionLease, error) {
	if err := validateExecutionLease(candidate); err != nil {
		return ExecutionLease{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	assignment, exists := activeMemoryAssignment(r.assignments[candidate.SlotID])
	if !exists || assignment.NodeID != candidate.NodeID || assignment.ExecutionEpoch != candidate.ExecutionEpoch {
		return ExecutionLease{}, ErrExecutionLeaseConflict
	}
	key := executionLeaseKey(candidate.SlotID, candidate.ExecutionEpoch)
	if existing, exists := r.executionLeases[key]; exists {
		if existing.NodeID != candidate.NodeID || existing.OwnerID != candidate.OwnerID || existing.RevokedAt != nil {
			return ExecutionLease{}, ErrExecutionLeaseConflict
		}
		existing.ExpiresAt = candidate.ExpiresAt.UTC()
		existing.UpdatedAt = candidate.UpdatedAt.UTC()
		r.executionLeases[key] = existing
		return cloneExecutionLease(existing), nil
	}
	candidate.ExpiresAt = candidate.ExpiresAt.UTC()
	candidate.CreatedAt = candidate.CreatedAt.UTC()
	candidate.UpdatedAt = candidate.UpdatedAt.UTC()
	r.executionLeases[key] = candidate
	return cloneExecutionLease(candidate), nil
}

func (r *MemoryRepository) RenewExecutionLease(_ context.Context, slotID string, executionEpoch uint64, ownerID string, expiresAt, renewedAt time.Time) error {
	if slotID == "" || executionEpoch == 0 || ownerID == "" || !expiresAt.After(renewedAt) || renewedAt.IsZero() {
		return errors.New("invalid execution lease renewal")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := executionLeaseKey(slotID, executionEpoch)
	lease, exists := r.executionLeases[key]
	assignment, assigned := activeMemoryAssignment(r.assignments[slotID])
	if !exists || lease.OwnerID != ownerID || lease.RevokedAt != nil || !assigned || assignment.ExecutionEpoch != executionEpoch || assignment.NodeID != lease.NodeID {
		return ErrExecutionLeaseNotFound
	}
	lease.ExpiresAt = expiresAt.UTC()
	lease.UpdatedAt = renewedAt.UTC()
	r.executionLeases[key] = lease
	return nil
}

func (r *MemoryRepository) RevokeExecutionLease(_ context.Context, slotID string, executionEpoch uint64, ownerID string, revokedAt time.Time) error {
	if slotID == "" || executionEpoch == 0 || ownerID == "" || revokedAt.IsZero() {
		return errors.New("invalid execution lease revocation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := executionLeaseKey(slotID, executionEpoch)
	lease, exists := r.executionLeases[key]
	if !exists || lease.OwnerID != ownerID {
		return ErrExecutionLeaseNotFound
	}
	if lease.RevokedAt == nil {
		revoked := revokedAt.UTC()
		lease.RevokedAt = &revoked
		lease.UpdatedAt = revoked
		r.executionLeases[key] = lease
	}
	return nil
}

func (r *MemoryRepository) GetExecutionLease(_ context.Context, slotID string, executionEpoch uint64) (ExecutionLease, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	lease, exists := r.executionLeases[executionLeaseKey(slotID, executionEpoch)]
	if !exists {
		return ExecutionLease{}, ErrExecutionLeaseNotFound
	}
	return cloneExecutionLease(lease), nil
}

func getExecutionLease(ctx context.Context, queryer slotQueryer, slotID string, executionEpoch uint64, forUpdate bool) (ExecutionLease, error) {
	query := `
SELECT lease_id, slot_id, node_id, execution_epoch, owner_id, expires_at, revoked_at, created_at, updated_at
FROM execution_leases WHERE slot_id = ? AND execution_epoch = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var lease ExecutionLease
	var revoked sql.NullTime
	err := queryer.QueryRowContext(ctx, query, slotID, executionEpoch).Scan(
		&lease.ID, &lease.SlotID, &lease.NodeID, &lease.ExecutionEpoch, &lease.OwnerID,
		&lease.ExpiresAt, &revoked, &lease.CreatedAt, &lease.UpdatedAt,
	)
	if err != nil {
		return ExecutionLease{}, err
	}
	if revoked.Valid {
		value := revoked.Time.UTC()
		lease.RevokedAt = &value
	}
	return lease, nil
}

func validateExecutionLease(lease ExecutionLease) error {
	if lease.ID == "" || lease.SlotID == "" || lease.NodeID == "" || lease.ExecutionEpoch == 0 || lease.OwnerID == "" ||
		lease.CreatedAt.IsZero() || lease.UpdatedAt.IsZero() || !lease.ExpiresAt.After(lease.UpdatedAt) || lease.RevokedAt != nil {
		return errors.New("invalid execution lease")
	}
	return nil
}

func requireOneLease(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrExecutionLeaseNotFound
	}
	return nil
}

func executionLeaseKey(slotID string, epoch uint64) string {
	return fmt.Sprintf("%s/%d", slotID, epoch)
}

func cloneExecutionLease(lease ExecutionLease) ExecutionLease {
	if lease.RevokedAt != nil {
		value := *lease.RevokedAt
		lease.RevokedAt = &value
	}
	return lease
}

var _ ExecutionLeaseRepository = (*Repository)(nil)
var _ ExecutionLeaseRepository = (*MemoryRepository)(nil)
