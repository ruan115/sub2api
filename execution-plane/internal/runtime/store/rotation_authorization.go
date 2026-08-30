package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

var ErrRotationAuthorizationRejected = errors.New("credential rotation authorization rejected")

type credentialRotationAuthorization struct {
	CredentialLeaseID   string
	MaterialSHA256      [32]byte
	AccountID           string
	SlotID              string
	ExecutionEpoch      uint64
	ProxyLeaseID        string
	CredentialVersionID string
	AuthorizedAt        time.Time
	CommittedAt         *time.Time
}

func (r *Repository) BeginCredentialRotation(
	ctx context.Context,
	accountBinding, slotID string,
	executionEpoch uint64,
	credentialLeaseID, proxyLeaseID string,
	materialSHA256 [32]byte,
	authorizedAt time.Time,
) (string, string, error) {
	if validateRotationAuthorizationInput(
		accountBinding, slotID, executionEpoch, credentialLeaseID, proxyLeaseID, materialSHA256, authorizedAt,
	) != nil {
		return "", "", ErrRotationAuthorizationRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return "", "", fmt.Errorf("begin credential rotation authorization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	workflow, err := getOnboardingWorkflowByCredentialLease(ctx, tx, credentialLeaseID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrRotationAuthorizationRejected
	}
	if err != nil {
		return "", "", fmt.Errorf("lock rotation onboarding workflow: %w", err)
	}
	defer workflow.Destroy()
	if provider.RuntimeAccountID(workflow.AccountID) != accountBinding || workflow.SlotID != slotID ||
		workflow.ExecutionEpoch != executionEpoch || workflow.ProxyLeaseID != proxyLeaseID {
		return "", "", ErrRotationAuthorizationRejected
	}

	existing, err := getCredentialRotationAuthorization(ctx, tx, credentialLeaseID, true)
	if err == nil {
		if !sameCredentialRotationAuthorization(existing, workflow, materialSHA256) {
			return "", "", ErrRotationAuthorizationRejected
		}
		if existing.CredentialVersionID != "" {
			if err := tx.Commit(); err != nil {
				return "", "", fmt.Errorf("commit credential rotation replay: %w", err)
			}
			return workflow.AccountID, existing.CredentialVersionID, nil
		}
		mappedVersionID, mappingErr := getCredentialOperationVersion(ctx, tx, credentialLeaseID, workflow.AccountID)
		if mappingErr == nil {
			if err := completeCredentialRotationInTx(ctx, tx, credentialLeaseID, mappedVersionID, authorizedAt); err != nil {
				return "", "", err
			}
			if err := tx.Commit(); err != nil {
				return "", "", fmt.Errorf("commit recovered credential rotation: %w", err)
			}
			return workflow.AccountID, mappedVersionID, nil
		}
		if !errors.Is(mappingErr, sql.ErrNoRows) {
			return "", "", mappingErr
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("read credential rotation authorization: %w", err)
	}

	if !rotationWorkflowCanCommit(workflow, authorizedAt) ||
		validateCurrentRotationExecution(ctx, tx, workflow, authorizedAt) != nil {
		return "", "", ErrRotationAuthorizationRejected
	}
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `
INSERT INTO credential_rotation_commits (
  credential_lease_id, material_sha256, account_id, slot_id, execution_epoch,
  proxy_lease_id, credential_version_id, authorized_at, committed_at
) VALUES (?, ?, ?, ?, ?, ?, NULL, ?, NULL)`,
			credentialLeaseID, materialSHA256[:], workflow.AccountID, slotID, executionEpoch, proxyLeaseID, authorizedAt.UTC())
		if err != nil {
			return "", "", fmt.Errorf("reserve credential rotation authorization: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("commit credential rotation authorization: %w", err)
	}
	return workflow.AccountID, "", nil
}

func (r *Repository) CompleteCredentialRotation(ctx context.Context, credentialLeaseID, versionID string, committedAt time.Time) error {
	if credential.ValidateTransportID(credentialLeaseID) != nil || credential.ValidateTransportID(versionID) != nil || committedAt.IsZero() {
		return ErrRotationAuthorizationRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin credential rotation completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := getCredentialRotationAuthorization(ctx, tx, credentialLeaseID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRotationAuthorizationRejected
	}
	if err != nil {
		return fmt.Errorf("lock credential rotation completion: %w", err)
	}
	if committedAt.Before(record.AuthorizedAt) {
		return ErrRotationAuthorizationRejected
	}
	if record.CredentialVersionID != "" {
		if record.CredentialVersionID != versionID {
			return ErrRotationAuthorizationRejected
		}
		return tx.Commit()
	}
	mappedVersionID, err := getCredentialOperationVersion(ctx, tx, credentialLeaseID, record.AccountID)
	if errors.Is(err, sql.ErrNoRows) || mappedVersionID != versionID {
		return ErrRotationAuthorizationRejected
	}
	if err != nil {
		return err
	}
	if err := completeCredentialRotationInTx(ctx, tx, credentialLeaseID, versionID, committedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit credential rotation completion: %w", err)
	}
	return nil
}

func getCredentialOperationVersion(ctx context.Context, tx *sql.Tx, credentialLeaseID, accountID string) (string, error) {
	var versionID string
	err := tx.QueryRowContext(ctx, `
SELECT cvo.version_id
FROM credential_version_operations cvo
WHERE cvo.operation_id = ? AND cvo.account_id = ? FOR UPDATE`, credentialLeaseID, accountID).Scan(&versionID)
	if err != nil {
		return "", err
	}
	if credential.ValidateTransportID(versionID) != nil {
		return "", ErrRotationAuthorizationRejected
	}
	return versionID, nil
}

func completeCredentialRotationInTx(ctx context.Context, tx *sql.Tx, credentialLeaseID, versionID string, committedAt time.Time) error {
	result, err := tx.ExecContext(ctx, `
UPDATE credential_rotation_commits
SET credential_version_id = ?, committed_at = ?
WHERE credential_lease_id = ? AND credential_version_id IS NULL`, versionID, committedAt.UTC(), credentialLeaseID)
	if err != nil {
		return fmt.Errorf("complete credential rotation authorization: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrRotationAuthorizationRejected
	}
	return nil
}

func validateRotationAuthorizationInput(
	accountBinding, slotID string,
	executionEpoch uint64,
	credentialLeaseID, proxyLeaseID string,
	materialSHA256 [32]byte,
	authorizedAt time.Time,
) error {
	for _, value := range []string{accountBinding, slotID, credentialLeaseID, proxyLeaseID} {
		if credential.ValidateTransportID(value) != nil {
			return ErrRotationAuthorizationRejected
		}
	}
	if executionEpoch == 0 || materialSHA256 == ([32]byte{}) || authorizedAt.IsZero() {
		return ErrRotationAuthorizationRejected
	}
	return nil
}

func rotationWorkflowCanCommit(workflow onboarding.Provisioning, authorizedAt time.Time) bool {
	if workflow.Validate() != nil || !workflow.CommandDeadline.After(authorizedAt) {
		return false
	}
	switch workflow.Status {
	case onboarding.ProvisioningKeyReady, onboarding.ProvisioningActivationDispatched, onboarding.ProvisioningActivationSucceeded:
		return true
	default:
		return false
	}
}

func validateCurrentRotationExecution(ctx context.Context, tx *sql.Tx, workflow onboarding.Provisioning, checkedAt time.Time) error {
	var executionLeaseID string
	err := tx.QueryRowContext(ctx, `
SELECT el.lease_id
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id AND sa.released_at IS NULL
JOIN execution_leases el ON el.slot_id = sa.slot_id AND el.execution_epoch = sa.execution_epoch
WHERE s.slot_id = ? AND s.account_id = ?
  AND s.desired_state = 'ready' AND s.desired_generation = ? AND s.image_digest = ?
  AND sa.node_id = ? AND sa.execution_epoch = ? AND sa.actual_state = 'running'
  AND sa.image_digest = ? AND sa.healthy = TRUE
  AND el.node_id = sa.node_id AND el.revoked_at IS NULL AND el.expires_at > ?
FOR UPDATE`, workflow.SlotID, workflow.AccountID, workflow.DesiredGeneration, workflow.ImageDigest,
		workflow.NodeID, workflow.ExecutionEpoch, workflow.ImageDigest, checkedAt.UTC()).Scan(&executionLeaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRotationAuthorizationRejected
	}
	if err != nil {
		return fmt.Errorf("validate current rotation execution: %w", err)
	}
	return nil
}

func getCredentialRotationAuthorization(ctx context.Context, queryer slotQueryer, credentialLeaseID string, forUpdate bool) (credentialRotationAuthorization, error) {
	query := `
SELECT credential_lease_id, material_sha256, account_id, slot_id, execution_epoch,
       proxy_lease_id, credential_version_id, authorized_at, committed_at
FROM credential_rotation_commits WHERE credential_lease_id = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var record credentialRotationAuthorization
	var material []byte
	var versionID sql.NullString
	var committedAt sql.NullTime
	err := queryer.QueryRowContext(ctx, query, credentialLeaseID).Scan(
		&record.CredentialLeaseID, &material, &record.AccountID, &record.SlotID, &record.ExecutionEpoch,
		&record.ProxyLeaseID, &versionID, &record.AuthorizedAt, &committedAt,
	)
	if err != nil {
		return credentialRotationAuthorization{}, err
	}
	if len(material) != 32 {
		return credentialRotationAuthorization{}, ErrRotationAuthorizationRejected
	}
	copy(record.MaterialSHA256[:], material)
	if versionID.Valid {
		record.CredentialVersionID = versionID.String
	}
	if committedAt.Valid {
		value := committedAt.Time.UTC()
		record.CommittedAt = &value
	}
	record.AuthorizedAt = record.AuthorizedAt.UTC()
	return record, nil
}

func sameCredentialRotationAuthorization(record credentialRotationAuthorization, workflow onboarding.Provisioning, digest [32]byte) bool {
	return record.CredentialLeaseID == workflow.CredentialLeaseID && record.MaterialSHA256 == digest &&
		record.AccountID == workflow.AccountID && record.SlotID == workflow.SlotID &&
		record.ExecutionEpoch == workflow.ExecutionEpoch && record.ProxyLeaseID == workflow.ProxyLeaseID
}
