package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

type CredentialVaultRepository interface {
	credential.VaultRepository
	ListCredentialSecurityEvents(ctx context.Context, accountID string, limit int) ([]credential.SecurityEvent, error)
}

func (r *Repository) NextCredentialVersionNumber(ctx context.Context, accountID string) (uint64, error) {
	if err := validateCredentialAccountID(accountID); err != nil {
		return 0, err
	}
	var current uint64
	if err := r.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_number), 0) FROM credential_versions WHERE account_id = ?`, accountID).Scan(&current); err != nil {
		return 0, fmt.Errorf("read next credential version: %w", err)
	}
	if current == math.MaxUint64 {
		return 0, credential.ErrCredentialVersionConflict
	}
	return current + 1, nil
}

func (r *Repository) CommitCredentialVersion(ctx context.Context, version credential.VersionRecord) error {
	if err := version.Validate(); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin credential version transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
INSERT INTO credential_vault (account_id, active_version_id, auth_type, created_at, updated_at)
VALUES (?, NULL, ?, ?, ?)
ON DUPLICATE KEY UPDATE account_id = account_id`,
		version.AccountID, version.AuthType, version.CreatedAt.UTC(), version.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("ensure credential vault: %w", err)
	}
	var activeVersion sql.NullString
	var currentAuthType string
	if err := tx.QueryRowContext(ctx, `SELECT active_version_id, auth_type FROM credential_vault WHERE account_id = ? FOR UPDATE`, version.AccountID).
		Scan(&activeVersion, &currentAuthType); err != nil {
		return fmt.Errorf("lock credential vault: %w", err)
	}
	var currentVersion uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_number), 0) FROM credential_versions WHERE account_id = ?`, version.AccountID).
		Scan(&currentVersion); err != nil {
		return fmt.Errorf("read current credential version: %w", err)
	}
	if currentVersion == math.MaxUint64 || version.VersionNumber != currentVersion+1 {
		return credential.ErrCredentialVersionConflict
	}

	_, err = tx.ExecContext(ctx, `
INSERT INTO credential_versions (
  version_id, account_id, version_number, ciphertext, encrypted_dek, nonce, aad_json,
  kms_key_id, kms_key_version, credential_hint, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		version.ID, version.AccountID, version.VersionNumber, version.Envelope.Ciphertext,
		version.Envelope.EncryptedDEK, version.Envelope.Nonce, version.Envelope.AADJSON,
		version.Envelope.KMSKeyID, version.Envelope.KMSKeyVersion, version.Hint, version.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("insert credential version: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE credential_vault SET active_version_id = ?, auth_type = ?, updated_at = ?
WHERE account_id = ?`, version.ID, version.AuthType, version.CreatedAt.UTC(), version.AccountID)
	if err != nil {
		return fmt.Errorf("activate credential version: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return credential.ErrCredentialVersionConflict
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE credential_leases SET revoked_at = ?
WHERE account_id = ? AND version_id <> ? AND consumed_at IS NULL AND revoked_at IS NULL`,
		version.CreatedAt.UTC(), version.AccountID, version.ID); err != nil {
		return fmt.Errorf("revoke superseded credential leases: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit credential version: %w", err)
	}
	return nil
}

func (r *Repository) GetActiveCredentialVersion(ctx context.Context, accountID string) (credential.VersionRecord, error) {
	if err := validateCredentialAccountID(accountID); err != nil {
		return credential.VersionRecord{}, err
	}
	record, err := getActiveCredentialVersion(ctx, r.db, accountID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return credential.VersionRecord{}, credential.ErrCredentialVaultNotFound
	}
	return record, err
}

func (r *Repository) IssueCredentialLease(ctx context.Context, candidate credential.LeaseRecord) (credential.LeaseRecord, error) {
	if err := candidate.ValidateForIssue(); err != nil {
		return credential.LeaseRecord{}, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return credential.LeaseRecord{}, fmt.Errorf("begin credential lease issue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var versionID string
	err = tx.QueryRowContext(ctx, `SELECT active_version_id FROM credential_vault
WHERE account_id = ? AND active_version_id IS NOT NULL FOR UPDATE`, candidate.AccountID).Scan(&versionID)
	if errors.Is(err, sql.ErrNoRows) {
		return credential.LeaseRecord{}, credential.ErrCredentialLeaseRejected
	}
	if err != nil {
		return credential.LeaseRecord{}, fmt.Errorf("lock credential vault for lease: %w", err)
	}
	var assignmentNodeID string
	err = tx.QueryRowContext(ctx, `
SELECT sa.node_id
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id AND sa.released_at IS NULL
JOIN execution_leases el ON el.slot_id = sa.slot_id AND el.execution_epoch = sa.execution_epoch
WHERE s.slot_id = ? AND s.account_id = ? AND sa.execution_epoch = ?
  AND el.node_id = sa.node_id AND el.revoked_at IS NULL AND el.expires_at > ?
FOR UPDATE`, candidate.SlotID, candidate.AccountID, candidate.ExecutionEpoch, candidate.CreatedAt.UTC()).Scan(&assignmentNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return credential.LeaseRecord{}, credential.ErrCredentialLeaseRejected
	}
	if err != nil {
		return credential.LeaseRecord{}, fmt.Errorf("validate credential lease binding: %w", err)
	}
	candidate.VersionID = versionID
	_, err = tx.ExecContext(ctx, `
INSERT INTO credential_leases (
  lease_id, token_sha256, account_id, version_id, slot_id, execution_epoch, expires_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.ID, candidate.TokenSHA256[:], candidate.AccountID, candidate.VersionID,
		candidate.SlotID, candidate.ExecutionEpoch, candidate.ExpiresAt.UTC(), candidate.CreatedAt.UTC())
	if err != nil {
		return credential.LeaseRecord{}, fmt.Errorf("insert credential lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return credential.LeaseRecord{}, fmt.Errorf("commit credential lease issue: %w", err)
	}
	return cloneCredentialLease(candidate), nil
}

func (r *Repository) ConsumeCredentialLease(ctx context.Context, claim credential.LeaseClaim) (credential.VersionRecord, error) {
	if err := claim.Validate(); err != nil {
		return credential.VersionRecord{}, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return credential.VersionRecord{}, fmt.Errorf("begin credential lease consume: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var activeVersionID sql.NullString
	vaultErr := tx.QueryRowContext(ctx, `SELECT active_version_id FROM credential_vault WHERE account_id = ? FOR UPDATE`, claim.AccountID).
		Scan(&activeVersionID)
	if vaultErr != nil && !errors.Is(vaultErr, sql.ErrNoRows) {
		return credential.VersionRecord{}, fmt.Errorf("lock credential vault for consume: %w", vaultErr)
	}
	lease, err := getCredentialLeaseByToken(ctx, tx, claim.TokenSHA256, true)
	if errors.Is(err, sql.ErrNoRows) {
		return credential.VersionRecord{}, rejectCredentialLease(ctx, tx, claim, credential.LeaseRecord{}, "unknown_token")
	}
	if err != nil {
		return credential.VersionRecord{}, fmt.Errorf("lock credential lease: %w", err)
	}
	reason := rejectedCredentialLeaseReason(lease, claim)
	if reason != "" {
		return credential.VersionRecord{}, rejectCredentialLease(ctx, tx, claim, lease, reason)
	}
	if !activeVersionID.Valid || activeVersionID.String != lease.VersionID {
		return credential.VersionRecord{}, rejectCredentialLease(ctx, tx, claim, lease, "version_inactive")
	}

	var assignmentNodeID string
	err = tx.QueryRowContext(ctx, `
SELECT sa.node_id
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id AND sa.released_at IS NULL
JOIN execution_leases el ON el.slot_id = sa.slot_id AND el.execution_epoch = sa.execution_epoch
WHERE s.slot_id = ? AND s.account_id = ? AND sa.execution_epoch = ?
  AND el.node_id = sa.node_id AND el.revoked_at IS NULL AND el.expires_at > ?
FOR UPDATE`, lease.SlotID, lease.AccountID, lease.ExecutionEpoch, claim.ConsumedAt.UTC()).Scan(&assignmentNodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return credential.VersionRecord{}, rejectCredentialLease(ctx, tx, claim, lease, "epoch_or_version_inactive")
	}
	if err != nil {
		return credential.VersionRecord{}, fmt.Errorf("revalidate credential lease epoch: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
UPDATE credential_leases SET consumed_at = ?
WHERE lease_id = ? AND consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		claim.ConsumedAt.UTC(), lease.ID, claim.ConsumedAt.UTC())
	if err != nil {
		return credential.VersionRecord{}, fmt.Errorf("consume credential lease: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return credential.VersionRecord{}, rejectCredentialLease(ctx, tx, claim, lease, "concurrent_replay")
	}
	version, err := getCredentialVersion(ctx, tx, lease.VersionID)
	if err != nil {
		return credential.VersionRecord{}, fmt.Errorf("read leased credential version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return credential.VersionRecord{}, fmt.Errorf("commit credential lease consume: %w", err)
	}
	return version, nil
}

func (r *Repository) ListCredentialSecurityEvents(ctx context.Context, accountID string, limit int) ([]credential.SecurityEvent, error) {
	if err := validateCredentialAccountID(accountID); err != nil || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid credential security event query")
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT event_id, event_type, reason_code, account_id, slot_id, execution_epoch, COALESCE(lease_id, ''), created_at
FROM credential_security_events WHERE account_id = ? ORDER BY created_at DESC, event_id DESC LIMIT ?`, accountID, limit)
	if err != nil {
		return nil, fmt.Errorf("list credential security events: %w", err)
	}
	defer rows.Close()
	events := make([]credential.SecurityEvent, 0)
	for rows.Next() {
		var event credential.SecurityEvent
		if err := rows.Scan(&event.ID, &event.EventType, &event.ReasonCode, &event.AccountID, &event.SlotID,
			&event.ExecutionEpoch, &event.LeaseID, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan credential security event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate credential security events: %w", err)
	}
	return events, nil
}

func getActiveCredentialVersion(ctx context.Context, queryer slotQueryer, accountID string, forUpdate bool) (credential.VersionRecord, error) {
	query := `
SELECT v.version_id, v.account_id, v.version_number, cv.auth_type, v.ciphertext, v.encrypted_dek,
       v.nonce, v.aad_json, v.kms_key_id, v.kms_key_version, v.credential_hint, v.created_at
FROM credential_vault cv
JOIN credential_versions v ON v.version_id = cv.active_version_id AND v.account_id = cv.account_id
WHERE cv.account_id = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanCredentialVersion(queryer.QueryRowContext(ctx, query, accountID))
}

func getCredentialVersion(ctx context.Context, queryer slotQueryer, versionID string) (credential.VersionRecord, error) {
	return scanCredentialVersion(queryer.QueryRowContext(ctx, `
SELECT v.version_id, v.account_id, v.version_number, cv.auth_type, v.ciphertext, v.encrypted_dek,
       v.nonce, v.aad_json, v.kms_key_id, v.kms_key_version, v.credential_hint, v.created_at
FROM credential_versions v
JOIN credential_vault cv ON cv.account_id = v.account_id
WHERE v.version_id = ?`, versionID))
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCredentialVersion(row rowScanner) (credential.VersionRecord, error) {
	var record credential.VersionRecord
	err := row.Scan(
		&record.ID, &record.AccountID, &record.VersionNumber, &record.AuthType,
		&record.Envelope.Ciphertext, &record.Envelope.EncryptedDEK, &record.Envelope.Nonce,
		&record.Envelope.AADJSON, &record.Envelope.KMSKeyID, &record.Envelope.KMSKeyVersion,
		&record.Hint, &record.CreatedAt,
	)
	if err != nil {
		return credential.VersionRecord{}, err
	}
	if err := record.NormalizeAndValidate(); err != nil {
		return credential.VersionRecord{}, fmt.Errorf("stored credential version is invalid: %w", err)
	}
	return record, nil
}

func getCredentialLeaseByToken(ctx context.Context, queryer slotQueryer, digest [32]byte, forUpdate bool) (credential.LeaseRecord, error) {
	query := `
SELECT lease_id, token_sha256, account_id, version_id, slot_id, execution_epoch,
       expires_at, consumed_at, revoked_at, created_at
FROM credential_leases WHERE token_sha256 = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var lease credential.LeaseRecord
	var token []byte
	var consumed, revoked sql.NullTime
	err := queryer.QueryRowContext(ctx, query, digest[:]).Scan(
		&lease.ID, &token, &lease.AccountID, &lease.VersionID, &lease.SlotID, &lease.ExecutionEpoch,
		&lease.ExpiresAt, &consumed, &revoked, &lease.CreatedAt,
	)
	if err != nil {
		return credential.LeaseRecord{}, err
	}
	if len(token) != sha256Size {
		return credential.LeaseRecord{}, errors.New("stored credential lease digest is invalid")
	}
	copy(lease.TokenSHA256[:], token)
	if consumed.Valid {
		value := consumed.Time.UTC()
		lease.ConsumedAt = &value
	}
	if revoked.Valid {
		value := revoked.Time.UTC()
		lease.RevokedAt = &value
	}
	return lease, nil
}

const sha256Size = 32

func rejectedCredentialLeaseReason(lease credential.LeaseRecord, claim credential.LeaseClaim) string {
	if lease.AccountID != claim.AccountID || lease.SlotID != claim.SlotID || lease.ExecutionEpoch != claim.ExecutionEpoch {
		return "binding_mismatch"
	}
	if lease.ConsumedAt != nil {
		return "replayed"
	}
	if lease.RevokedAt != nil {
		return "revoked"
	}
	if claim.ConsumedAt.Before(lease.CreatedAt) {
		return "clock_invalid"
	}
	if !lease.ExpiresAt.After(claim.ConsumedAt) {
		return "expired"
	}
	return ""
}

func rejectCredentialLease(ctx context.Context, tx *sql.Tx, claim credential.LeaseClaim, lease credential.LeaseRecord, reason string) error {
	eventAccountID, eventSlotID, eventLeaseID, eventEpoch := claim.AccountID, claim.SlotID, "", claim.ExecutionEpoch
	if lease.ID != "" {
		eventAccountID, eventSlotID, eventLeaseID, eventEpoch = lease.AccountID, lease.SlotID, lease.ID, lease.ExecutionEpoch
	}
	var nullableLeaseID any
	if eventLeaseID != "" {
		nullableLeaseID = eventLeaseID
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO credential_security_events (
  event_id, event_type, reason_code, account_id, slot_id, execution_epoch, lease_id, created_at
) VALUES (?, 'credential_lease_rejected', ?, ?, ?, ?, ?, ?)`,
		claim.SecurityEventID, reason, eventAccountID, eventSlotID, eventEpoch, nullableLeaseID, claim.ConsumedAt.UTC())
	if err != nil {
		return fmt.Errorf("record credential lease security event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit credential lease security event: %w", err)
	}
	return credential.ErrCredentialLeaseRejected
}

func validateCredentialAccountID(accountID string) error {
	if accountID == "" || len(accountID) > 128 {
		return errors.New("credential account id is invalid")
	}
	return nil
}

func cloneCredentialLease(lease credential.LeaseRecord) credential.LeaseRecord {
	if lease.ConsumedAt != nil {
		value := *lease.ConsumedAt
		lease.ConsumedAt = &value
	}
	if lease.RevokedAt != nil {
		value := *lease.RevokedAt
		lease.RevokedAt = &value
	}
	return lease
}

var _ CredentialVaultRepository = (*Repository)(nil)
