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

type healthySlotStartRuntimeBinding struct {
	NodeID                  string
	ExecutionEpoch          uint64
	ImageDigest             string
	LastObservedAt          time.Time
	ExecutionLeaseCreatedAt time.Time
	ExecutionLeaseExpiresAt time.Time
	ReservationCreatedAt    time.Time
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

	trigger, err := lockHealthySlotStartTrigger(ctx, tx, spec)
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, onboarding.ErrHealthySlotStartRejected) {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	if err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("lock healthy-slot onboarding trigger: %w", err)
	}

	intent, err := lockHealthySlotStartIntent(ctx, tx, spec.IntentID)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	if err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("lock healthy-slot onboarding intent: %w", err)
	}

	existing, err := getOnboardingWorkflowByIntent(ctx, tx, intent.ID, true)
	if err == nil {
		matches, replayErr := sameHealthySlotStartReplay(ctx, tx, trigger, intent, existing, spec)
		if replayErr != nil {
			existing.Destroy()
			return onboarding.Provisioning{}, false, fmt.Errorf("read healthy-slot onboarding replay binding: %w", replayErr)
		}
		if !matches {
			existing.Destroy()
			return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
		}
		switch trigger.Status {
		case onboarding.StartTriggerStarted:
			if trigger.StartedWorkflowID != existing.ID {
				existing.Destroy()
				return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
			}
		case onboarding.StartTriggerClaimed:
			databaseNow, clockErr := healthySlotStartDatabaseTime(ctx, tx)
			if clockErr != nil {
				existing.Destroy()
				return onboarding.Provisioning{}, false, clockErr
			}
			if !healthySlotStartClaimCurrent(trigger, databaseNow) || !intent.ExpiresAt.After(databaseNow) {
				existing.Destroy()
				return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
			}
			if markErr := markOnboardingStartTriggerStarted(ctx, tx, trigger, existing.ID); markErr != nil {
				existing.Destroy()
				return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
			}
		default:
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
	if trigger.Status != onboarding.StartTriggerClaimed || intent.Status != onboarding.IntentPending || intent.DesiredGeneration == 0 ||
		intent.AccountID != trigger.AccountID || intent.DesiredGeneration != trigger.DesiredGeneration {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}

	var binding healthySlotStartRuntimeBinding
	err = tx.QueryRowContext(ctx, `
SELECT sa.node_id, sa.execution_epoch, s.image_digest, sa.last_observed_at,
       el.created_at, el.expires_at, prg.created_at
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
  AND el.node_id = sa.node_id AND el.revoked_at IS NULL
  AND prg.revoked_at IS NULL
FOR UPDATE`,
		trigger.ReservationID, trigger.BindingRevision, trigger.SlotID, intent.AccountID, intent.DesiredGeneration,
	).Scan(
		&binding.NodeID, &binding.ExecutionEpoch, &binding.ImageDigest, &binding.LastObservedAt,
		&binding.ExecutionLeaseCreatedAt, &binding.ExecutionLeaseExpiresAt, &binding.ReservationCreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	if err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("lock healthy-slot runtime binding: %w", err)
	}
	binding.canonicalize()
	databaseNow, err := healthySlotStartDatabaseTime(ctx, tx)
	if err != nil {
		return onboarding.Provisioning{}, false, err
	}
	if !healthySlotStartAuthorityCurrent(trigger, intent, binding, spec, databaseNow) {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}

	commandTTL := spec.RequestedCommandDeadline.Sub(spec.StartedAt)
	deadline := databaseNow.Add(commandTTL)
	if intent.ExpiresAt.Before(deadline) {
		deadline = intent.ExpiresAt
	}
	workflow := onboarding.Provisioning{
		ID: spec.WorkflowID, IdempotencyKey: spec.IdempotencyKey, IntentID: intent.ID, Owner: spec.Owner,
		AccountID: intent.AccountID, DesiredGeneration: intent.DesiredGeneration,
		NodeID: binding.NodeID, SlotID: trigger.SlotID, ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: binding.ImageDigest,
		CredentialLeaseID: spec.CredentialLeaseID, ProxyLeaseID: spec.ProxyLeaseID,
		KeyCommandID: spec.KeyCommandID, ActivationCommandID: spec.ActivationCommandID,
		CommandDeadline: deadline, Status: onboarding.ProvisioningPendingKey,
		CreatedAt: databaseNow, UpdatedAt: databaseNow,
	}
	if workflow.Validate() != nil {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	lease := ProxyLease{
		ID: spec.ProxyLeaseID, ReservationID: trigger.ReservationID, AccountID: intent.AccountID,
		DesiredGeneration: intent.DesiredGeneration, BindingRevision: trigger.BindingRevision,
		SlotID: trigger.SlotID, ExecutionEpoch: binding.ExecutionEpoch,
		CreatedAt: databaseNow, UpdatedAt: databaseNow,
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
	if err := markOnboardingStartTriggerStarted(ctx, tx, trigger, workflow.ID); err != nil {
		return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
	}
	if err := tx.Commit(); err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("commit healthy-slot onboarding start: %w", err)
	}
	return workflow, true, nil
}

func lockHealthySlotStartTrigger(
	ctx context.Context,
	tx *sql.Tx,
	spec onboarding.HealthySlotStartSpec,
) (onboarding.OnboardingStartTrigger, error) {
	trigger, err := getOnboardingStartTrigger(ctx, tx, spec.TriggerEventID, true)
	if err != nil {
		return onboarding.OnboardingStartTrigger{}, err
	}
	if trigger.EventID != spec.TriggerEventID || trigger.ClaimOwner != spec.TriggerClaimOwner ||
		trigger.ClaimVersion != spec.TriggerClaimVersion || trigger.IntentID != spec.IntentID ||
		trigger.SlotID != spec.SlotID || trigger.ReservationID != spec.ReservationID ||
		trigger.BindingRevision != spec.BindingRevision ||
		(trigger.Status != onboarding.StartTriggerClaimed && trigger.Status != onboarding.StartTriggerStarted) {
		return onboarding.OnboardingStartTrigger{}, onboarding.ErrHealthySlotStartRejected
	}
	return trigger, nil
}

func healthySlotStartClaimCurrent(trigger onboarding.OnboardingStartTrigger, checkedAt time.Time) bool {
	return trigger.Status == onboarding.StartTriggerClaimed && trigger.ClaimExpiresAt != nil &&
		trigger.ClaimExpiresAt.After(checkedAt) && trigger.IntentExpiresAt.After(checkedAt)
}

func (binding *healthySlotStartRuntimeBinding) canonicalize() {
	binding.LastObservedAt = binding.LastObservedAt.UTC()
	binding.ExecutionLeaseCreatedAt = binding.ExecutionLeaseCreatedAt.UTC()
	binding.ExecutionLeaseExpiresAt = binding.ExecutionLeaseExpiresAt.UTC()
	binding.ReservationCreatedAt = binding.ReservationCreatedAt.UTC()
}

func healthySlotStartDatabaseTime(ctx context.Context, tx *sql.Tx) (time.Time, error) {
	var databaseNow time.Time
	if err := tx.QueryRowContext(ctx, `SELECT UTC_TIMESTAMP(6)`).Scan(&databaseNow); err != nil {
		return time.Time{}, fmt.Errorf("read healthy-slot onboarding database time: %w", err)
	}
	databaseNow = canonicalRuntimeTime(databaseNow)
	if databaseNow.IsZero() {
		return time.Time{}, errors.New("read healthy-slot onboarding database time: zero timestamp")
	}
	return databaseNow, nil
}

func healthySlotStartAuthorityCurrent(
	trigger onboarding.OnboardingStartTrigger,
	intent healthySlotStartIntent,
	binding healthySlotStartRuntimeBinding,
	spec onboarding.HealthySlotStartSpec,
	databaseNow time.Time,
) bool {
	observationMaxAge := spec.StartedAt.Sub(spec.ObservationFreshAfter)
	commandTTL := spec.RequestedCommandDeadline.Sub(spec.StartedAt)
	if observationMaxAge < 0 || commandTTL <= 0 || !healthySlotStartClaimCurrent(trigger, databaseNow) ||
		!intent.ExpiresAt.After(databaseNow) {
		return false
	}
	freshAfter := databaseNow.Add(-observationMaxAge)
	return !binding.LastObservedAt.Before(freshAfter) && !binding.LastObservedAt.After(databaseNow) &&
		!binding.ExecutionLeaseCreatedAt.After(databaseNow) && binding.ExecutionLeaseExpiresAt.After(databaseNow) &&
		!binding.ReservationCreatedAt.After(databaseNow)
}

func markOnboardingStartTriggerStarted(
	ctx context.Context,
	tx *sql.Tx,
	trigger onboarding.OnboardingStartTrigger,
	workflowID string,
) error {
	result, err := tx.ExecContext(ctx, `
UPDATE onboarding_start_triggers
SET status = 'started', started_workflow_id = ?, started_at = UTC_TIMESTAMP(6), last_error_code = ''
WHERE event_id = ? AND status = 'claimed' AND claim_owner = ? AND claim_version = ?
	  AND claim_expires_at > UTC_TIMESTAMP(6) AND intent_expires_at > UTC_TIMESTAMP(6)`,
		workflowID, trigger.EventID, trigger.ClaimOwner, trigger.ClaimVersion,
	)
	if err != nil {
		return fmt.Errorf("mark onboarding start trigger started: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return onboarding.ErrHealthySlotStartRejected
	}
	return nil
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
	trigger onboarding.OnboardingStartTrigger,
	intent healthySlotStartIntent,
	workflow onboarding.Provisioning,
	spec onboarding.HealthySlotStartSpec,
) (bool, error) {
	if workflow.ID != spec.WorkflowID || workflow.IdempotencyKey != spec.IdempotencyKey ||
		workflow.IntentID != intent.ID || workflow.Owner != spec.Owner || workflow.AccountID != intent.AccountID ||
		workflow.DesiredGeneration != intent.DesiredGeneration || workflow.SlotID != trigger.SlotID ||
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
	return lease.ReservationID == trigger.ReservationID && lease.AccountID == workflow.AccountID &&
		lease.DesiredGeneration == workflow.DesiredGeneration && lease.BindingRevision == trigger.BindingRevision &&
		lease.SlotID == workflow.SlotID && lease.ExecutionEpoch == workflow.ExecutionEpoch, nil
}

func isAtomicStarterDuplicate(err error) bool {
	var mysqlError *mysql.MySQLError
	return errors.As(err, &mysqlError) && mysqlError.Number == 1062
}

var _ onboarding.HealthySlotStartRepository = (*Repository)(nil)
