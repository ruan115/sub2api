package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/go-sql-driver/mysql"
)

type healthySlotStartIntent struct {
	ID                string
	AccountID         string
	DesiredGeneration uint64
	Status            string
	ExpiresAt         time.Time
}

// StartHealthySlotOnboarding is the only write boundary for starting work on
// an intent. It deliberately reads no encrypted intent columns and does not
// claim the intent; claim/decrypt remains a later controller transition.
func (r *Repository) StartHealthySlotOnboarding(
	ctx context.Context,
	spec onboarding.HealthySlotStartSpec,
) (onboarding.Provisioning, bool, error) {
	if spec.Validate() != nil || ValidateProxyReservationOpaqueID(spec.ReservationID) != nil {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("begin healthy-slot onboarding start: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	intent, err := lockHealthySlotStartIntent(ctx, tx, spec.IntentID)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	if err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("lock healthy-slot onboarding intent: %w", err)
	}

	existing, err := getOnboardingWorkflowByIntent(ctx, tx, intent.ID, true)
	if err == nil {
		matches, replayErr := sameHealthySlotStartReplay(ctx, tx, intent, existing, spec)
		if replayErr != nil {
			existing.Destroy()
			return onboarding.Provisioning{}, false, fmt.Errorf("read healthy-slot onboarding replay binding: %w", replayErr)
		}
		if !matches {
			existing.Destroy()
			return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
		}
		if err := tx.Commit(); err != nil {
			existing.Destroy()
			return onboarding.Provisioning{}, false, fmt.Errorf("commit healthy-slot onboarding replay: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return onboarding.Provisioning{}, false, fmt.Errorf("read healthy-slot onboarding replay: %w", err)
	}
	if intent.Status != onboarding.IntentPending || intent.DesiredGeneration == 0 || !intent.ExpiresAt.After(spec.StartedAt) {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}

	var nodeID, imageDigest string
	var executionEpoch uint64
	err = tx.QueryRowContext(ctx, `
SELECT sa.node_id, sa.execution_epoch, s.image_digest
FROM slots s
JOIN slot_assignments sa ON sa.slot_id = s.slot_id AND sa.released_at IS NULL
JOIN execution_leases el
  ON el.slot_id = sa.slot_id AND el.execution_epoch = sa.execution_epoch AND el.node_id = sa.node_id
JOIN proxy_reservation_grants prg
  ON prg.reservation_id = ? AND prg.account_id = s.account_id
 AND prg.desired_generation = s.desired_generation AND prg.binding_revision = ?
WHERE s.slot_id = ? AND s.account_id = ? AND s.desired_state = 'ready'
  AND s.desired_generation = ?
  AND sa.desired_generation = s.desired_generation
  AND sa.actual_state = 'running' AND sa.healthy = TRUE
  AND sa.image_digest = s.image_digest
  AND sa.last_observed_at IS NOT NULL
  AND sa.last_observed_at >= ? AND sa.last_observed_at <= ?
  AND el.revoked_at IS NULL AND el.expires_at > ? AND el.created_at <= ?
  AND prg.revoked_at IS NULL AND prg.created_at <= ?
FOR UPDATE`,
		spec.ReservationID, spec.BindingRevision, spec.SlotID, intent.AccountID, intent.DesiredGeneration,
		spec.ObservationFreshAfter.UTC(), spec.StartedAt.UTC(), spec.StartedAt.UTC(), spec.StartedAt.UTC(), spec.StartedAt.UTC(),
	).Scan(&nodeID, &executionEpoch, &imageDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	if err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("lock healthy-slot runtime binding: %w", err)
	}

	deadline := spec.RequestedCommandDeadline.UTC()
	if intent.ExpiresAt.Before(deadline) {
		deadline = intent.ExpiresAt
	}
	workflow := onboarding.Provisioning{
		ID: spec.WorkflowID, IdempotencyKey: spec.IdempotencyKey, IntentID: intent.ID, Owner: spec.Owner,
		AccountID: intent.AccountID, DesiredGeneration: intent.DesiredGeneration,
		NodeID: nodeID, SlotID: spec.SlotID, ExecutionEpoch: executionEpoch, ImageDigest: imageDigest,
		CredentialLeaseID: spec.CredentialLeaseID, ProxyLeaseID: spec.ProxyLeaseID,
		KeyCommandID: spec.KeyCommandID, ActivationCommandID: spec.ActivationCommandID,
		CommandDeadline: deadline, Status: onboarding.ProvisioningPendingKey,
		CreatedAt: spec.StartedAt.UTC(), UpdatedAt: spec.StartedAt.UTC(),
	}
	if workflow.Validate() != nil {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	lease := ProxyLease{
		ID: spec.ProxyLeaseID, ReservationID: spec.ReservationID, AccountID: intent.AccountID,
		DesiredGeneration: intent.DesiredGeneration, BindingRevision: spec.BindingRevision,
		SlotID: spec.SlotID, ExecutionEpoch: executionEpoch,
		CreatedAt: spec.StartedAt.UTC(), UpdatedAt: spec.StartedAt.UTC(),
	}
	if validateProxyLease(lease) != nil {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}

	result, err := tx.ExecContext(ctx, `
INSERT INTO proxy_leases (
  proxy_lease_id, reservation_id, account_id, desired_generation, binding_revision,
  slot_id, execution_epoch, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		lease.ID, lease.ReservationID, lease.AccountID, lease.DesiredGeneration, lease.BindingRevision,
		lease.SlotID, lease.ExecutionEpoch, lease.CreatedAt, lease.UpdatedAt,
	)
	if err != nil {
		if isAtomicStarterDuplicate(err) {
			return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
		}
		return onboarding.Provisioning{}, false, fmt.Errorf("insert atomic onboarding proxy lease: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	result, err = tx.ExecContext(ctx, `
INSERT INTO onboarding_workflows (
  workflow_id, idempotency_key, intent_id, claim_owner, account_id, desired_generation,
  node_id, slot_id, execution_epoch, image_digest, credential_lease_id, proxy_lease_id,
  key_command_id, activation_command_id, command_deadline, status, key_id, key_public_key,
  error_code, last_command_id, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending_key', '', NULL, '', '', ?, ?)`,
		workflow.ID, workflow.IdempotencyKey, workflow.IntentID, workflow.Owner, workflow.AccountID,
		workflow.DesiredGeneration, workflow.NodeID, workflow.SlotID, workflow.ExecutionEpoch,
		workflow.ImageDigest, workflow.CredentialLeaseID, workflow.ProxyLeaseID, workflow.KeyCommandID,
		workflow.ActivationCommandID, workflow.CommandDeadline, workflow.CreatedAt, workflow.UpdatedAt,
	)
	if err != nil {
		if isAtomicStarterDuplicate(err) {
			return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
		}
		return onboarding.Provisioning{}, false, fmt.Errorf("insert atomic onboarding workflow: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	if err := tx.Commit(); err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("commit healthy-slot onboarding start: %w", err)
	}
	return workflow, true, nil
}

func lockHealthySlotStartIntent(ctx context.Context, tx *sql.Tx, intentID string) (healthySlotStartIntent, error) {
	var intent healthySlotStartIntent
	err := tx.QueryRowContext(ctx, `
SELECT intent_id, account_id, desired_generation, status, expires_at
FROM onboarding_intents WHERE intent_id = ? FOR UPDATE`, intentID).Scan(
		&intent.ID, &intent.AccountID, &intent.DesiredGeneration, &intent.Status, &intent.ExpiresAt,
	)
	intent.ExpiresAt = intent.ExpiresAt.UTC()
	return intent, err
}

func sameHealthySlotStartReplay(
	ctx context.Context,
	tx *sql.Tx,
	intent healthySlotStartIntent,
	workflow onboarding.Provisioning,
	spec onboarding.HealthySlotStartSpec,
) (bool, error) {
	if workflow.ID != spec.WorkflowID || workflow.IdempotencyKey != spec.IdempotencyKey ||
		workflow.IntentID != intent.ID || workflow.Owner != spec.Owner || workflow.AccountID != intent.AccountID ||
		workflow.DesiredGeneration != intent.DesiredGeneration || workflow.SlotID != spec.SlotID ||
		workflow.CredentialLeaseID != spec.CredentialLeaseID || workflow.ProxyLeaseID != spec.ProxyLeaseID ||
		workflow.KeyCommandID != spec.KeyCommandID || workflow.ActivationCommandID != spec.ActivationCommandID ||
		workflow.CommandDeadline.After(intent.ExpiresAt) {
		return false, nil
	}
	lease, err := getProxyLease(ctx, tx, workflow.ProxyLeaseID, true)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrProxyLeaseConflict) {
			return false, nil
		}
		return false, err
	}
	return lease.ReservationID == spec.ReservationID && lease.AccountID == workflow.AccountID &&
		lease.DesiredGeneration == workflow.DesiredGeneration && lease.BindingRevision == spec.BindingRevision &&
		lease.SlotID == workflow.SlotID && lease.ExecutionEpoch == workflow.ExecutionEpoch, nil
}

func isAtomicStarterDuplicate(err error) bool {
	var mysqlError *mysql.MySQLError
	return errors.As(err, &mysqlError) && mysqlError.Number == 1062
}

var _ onboarding.HealthySlotStartRepository = (*Repository)(nil)
