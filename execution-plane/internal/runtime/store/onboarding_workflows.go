package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

func (r *Repository) ObserveProvisioningCommand(ctx context.Context, observation onboarding.ProvisioningCommandObservation) (onboarding.Provisioning, error) {
	if observation.Validate() != nil {
		return onboarding.Provisioning{}, onboarding.ErrProvisioningRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return onboarding.Provisioning{}, fmt.Errorf("begin onboarding workflow observation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := getOnboardingWorkflowByCommand(ctx, tx, observation.CommandID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.Provisioning{}, onboarding.ErrProvisioningRejected
	}
	if err != nil {
		return onboarding.Provisioning{}, err
	}
	defer current.Destroy()
	updated, err := onboarding.ApplyProvisioningObservation(current, observation)
	if err != nil {
		return onboarding.Provisioning{}, err
	}
	if err := updateOnboardingWorkflowState(ctx, tx, updated); err != nil {
		updated.Destroy()
		return onboarding.Provisioning{}, err
	}
	if err := tx.Commit(); err != nil {
		updated.Destroy()
		return onboarding.Provisioning{}, fmt.Errorf("commit onboarding workflow observation: %w", err)
	}
	return updated, nil
}

func (r *Repository) MarkProvisioningActivationDispatched(ctx context.Context, workflowID string, dispatchedAt time.Time) error {
	return r.transitionOnboardingWorkflow(ctx, workflowID, dispatchedAt, func(record *onboarding.Provisioning) bool {
		switch record.Status {
		case onboarding.ProvisioningKeyReady:
			record.Status = onboarding.ProvisioningActivationDispatched
			record.LastCommandID = record.ActivationCommandID
			if dispatchedAt.After(record.UpdatedAt) {
				record.UpdatedAt = dispatchedAt.UTC()
			}
			return true
		case onboarding.ProvisioningActivationDispatched:
			if dispatchedAt.After(record.UpdatedAt) {
				record.UpdatedAt = dispatchedAt.UTC()
			}
			return true
		case onboarding.ProvisioningActivationSucceeded, onboarding.ProvisioningCompleted:
			return true
		default:
			return false
		}
	})
}

func (r *Repository) MarkProvisioningKeyDispatched(ctx context.Context, workflowID string, dispatchedAt time.Time) error {
	return r.transitionOnboardingWorkflow(ctx, workflowID, dispatchedAt, func(record *onboarding.Provisioning) bool {
		switch record.Status {
		case onboarding.ProvisioningPendingKey:
			record.Status = onboarding.ProvisioningKeyDispatched
			record.LastCommandID = record.KeyCommandID
			if dispatchedAt.After(record.UpdatedAt) {
				record.UpdatedAt = dispatchedAt.UTC()
			}
			return true
		case onboarding.ProvisioningKeyDispatched:
			if dispatchedAt.After(record.UpdatedAt) {
				record.UpdatedAt = dispatchedAt.UTC()
			}
			return true
		case onboarding.ProvisioningKeyReady,
			onboarding.ProvisioningActivationDispatched, onboarding.ProvisioningActivationSucceeded,
			onboarding.ProvisioningCompleted:
			return true
		default:
			return false
		}
	})
}

func (r *Repository) CompleteProvisioning(ctx context.Context, workflowID string, completedAt time.Time) error {
	return r.transitionOnboardingWorkflow(ctx, workflowID, completedAt, func(record *onboarding.Provisioning) bool {
		if record.Status == onboarding.ProvisioningCompleted {
			return true
		}
		if record.Status != onboarding.ProvisioningActivationSucceeded {
			return false
		}
		record.Status = onboarding.ProvisioningCompleted
		if completedAt.After(record.UpdatedAt) {
			record.UpdatedAt = completedAt.UTC()
		}
		return true
	})
}

func (r *Repository) FailProvisioning(ctx context.Context, workflowID, errorCode string, failedAt time.Time) error {
	return r.transitionOnboardingWorkflow(ctx, workflowID, failedAt, func(record *onboarding.Provisioning) bool {
		if record.Status == onboarding.ProvisioningFailed {
			return record.ErrorCode == errorCode
		}
		switch record.Status {
		case onboarding.ProvisioningPendingKey, onboarding.ProvisioningKeyDispatched:
			record.LastCommandID = record.KeyCommandID
		case onboarding.ProvisioningKeyReady, onboarding.ProvisioningActivationDispatched,
			onboarding.ProvisioningActivationSucceeded:
			record.LastCommandID = record.ActivationCommandID
		default:
			return false
		}
		record.Status = onboarding.ProvisioningFailed
		record.ErrorCode = errorCode
		if failedAt.After(record.UpdatedAt) {
			record.UpdatedAt = failedAt.UTC()
		}
		return true
	})
}

func (r *Repository) transitionOnboardingWorkflow(
	ctx context.Context,
	workflowID string,
	at time.Time,
	transition func(*onboarding.Provisioning) bool,
) error {
	if credential.ValidateTransportID(workflowID) != nil || at.IsZero() {
		return onboarding.ErrProvisioningRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin onboarding workflow transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	record, err := getOnboardingWorkflowByID(ctx, tx, workflowID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.ErrProvisioningRejected
	}
	if err != nil {
		return err
	}
	defer record.Destroy()
	previousStatus, previousCommand, previousError, previousUpdated := record.Status, record.LastCommandID, record.ErrorCode, record.UpdatedAt
	if !transition(&record) {
		return onboarding.ErrProvisioningRejected
	}
	if record.UpdatedAt.Equal(previousUpdated) &&
		(record.Status != previousStatus || record.LastCommandID != previousCommand || record.ErrorCode != previousError) {
		record.UpdatedAt = at.UTC()
	}
	if record.Validate() != nil {
		return onboarding.ErrProvisioningRejected
	}
	if err := updateOnboardingWorkflowState(ctx, tx, record); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit onboarding workflow transition: %w", err)
	}
	return nil
}

func (r *Repository) GetProvisioning(ctx context.Context, workflowID string) (onboarding.Provisioning, error) {
	if credential.ValidateTransportID(workflowID) != nil {
		return onboarding.Provisioning{}, onboarding.ErrProvisioningRejected
	}
	return getOnboardingWorkflowByID(ctx, r.db, workflowID, false)
}

func (r *Repository) ListActiveProvisioningIDs(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, onboarding.ErrProvisioningRejected
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT workflow_id
FROM onboarding_workflows
WHERE status NOT IN ('completed', 'failed')
ORDER BY updated_at, workflow_id
LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list active onboarding workflows: %w", err)
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var workflowID string
		if err := rows.Scan(&workflowID); err != nil || credential.ValidateTransportID(workflowID) != nil {
			return nil, onboarding.ErrProvisioningRejected
		}
		ids = append(ids, workflowID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active onboarding workflows: %w", err)
	}
	return ids, nil
}

func (r *Repository) DeferProvisioningRetry(ctx context.Context, workflowID string, retryAt time.Time) error {
	if credential.ValidateTransportID(workflowID) != nil || retryAt.IsZero() {
		return onboarding.ErrProvisioningRejected
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE onboarding_workflows
SET updated_at = GREATEST(updated_at, ?)
WHERE workflow_id = ? AND status NOT IN ('completed', 'failed')`, retryAt.UTC(), workflowID)
	if err != nil {
		return fmt.Errorf("defer onboarding workflow retry: %w", err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read onboarding retry deferral result: %w", err)
	}
	return nil
}

type onboardingWorkflowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getOnboardingWorkflowByID(ctx context.Context, queryer onboardingWorkflowQueryer, workflowID string, forUpdate bool) (onboarding.Provisioning, error) {
	query := onboardingWorkflowSelect + " WHERE workflow_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanOnboardingWorkflow(queryer.QueryRowContext(ctx, query, workflowID))
}

func getOnboardingWorkflowByIntent(ctx context.Context, queryer onboardingWorkflowQueryer, intentID string, forUpdate bool) (onboarding.Provisioning, error) {
	query := onboardingWorkflowSelect + " WHERE intent_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanOnboardingWorkflow(queryer.QueryRowContext(ctx, query, intentID))
}

func getOnboardingWorkflowByCommand(ctx context.Context, queryer onboardingWorkflowQueryer, commandID string, forUpdate bool) (onboarding.Provisioning, error) {
	query := onboardingWorkflowSelect + " WHERE key_command_id = ? OR activation_command_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanOnboardingWorkflow(queryer.QueryRowContext(ctx, query, commandID, commandID))
}

const onboardingWorkflowSelect = `
SELECT workflow_id, idempotency_key, intent_id, claim_owner, account_id, desired_generation,
       node_id, slot_id, execution_epoch, image_digest, credential_lease_id, proxy_lease_id,
       key_command_id, activation_command_id, command_deadline, status, key_id, key_public_key,
       error_code, last_command_id, created_at, updated_at
FROM onboarding_workflows`

func scanOnboardingWorkflow(row *sql.Row) (onboarding.Provisioning, error) {
	var record onboarding.Provisioning
	var publicKey []byte
	err := row.Scan(
		&record.ID, &record.IdempotencyKey, &record.IntentID, &record.Owner, &record.AccountID, &record.DesiredGeneration,
		&record.NodeID, &record.SlotID, &record.ExecutionEpoch, &record.ImageDigest, &record.CredentialLeaseID,
		&record.ProxyLeaseID, &record.KeyCommandID, &record.ActivationCommandID, &record.CommandDeadline,
		&record.Status, &record.KeyID, &publicKey, &record.ErrorCode, &record.LastCommandID, &record.CreatedAt, &record.UpdatedAt,
	)
	if err != nil {
		return onboarding.Provisioning{}, err
	}
	record.KeyPublicKey = append([]byte(nil), publicKey...)
	for index := range publicKey {
		publicKey[index] = 0
	}
	record.CommandDeadline = record.CommandDeadline.UTC()
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	if record.Validate() != nil {
		record.Destroy()
		return onboarding.Provisioning{}, onboarding.ErrProvisioningRejected
	}
	return record, nil
}

func updateOnboardingWorkflowState(ctx context.Context, tx *sql.Tx, record onboarding.Provisioning) error {
	var publicKey any
	if len(record.KeyPublicKey) > 0 {
		publicKey = record.KeyPublicKey
	}
	result, err := tx.ExecContext(ctx, `
UPDATE onboarding_workflows SET
  status = ?, key_id = ?, key_public_key = ?, error_code = ?, last_command_id = ?, updated_at = ?
WHERE workflow_id = ?`,
		record.Status, record.KeyID, publicKey, record.ErrorCode, record.LastCommandID, record.UpdatedAt.UTC(), record.ID,
	)
	if err != nil {
		return fmt.Errorf("update onboarding workflow state: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return onboarding.ErrProvisioningRejected
	}
	return nil
}

var _ onboarding.ProvisioningRepository = (*Repository)(nil)
var _ onboarding.ActiveProvisioningRepository = (*Repository)(nil)
