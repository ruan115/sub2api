package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/go-sql-driver/mysql"
)

type onboardingStartTriggerIntent struct {
	ID                string
	AccountID         string
	DesiredGeneration uint64
	Status            string
	ExpiresAt         time.Time
}

// ProjectOnboardingStartTrigger atomically projects a ready slot and records
// the exact CCMAX onboarding event that authorized it. The intent query is
// deliberately metadata-only; encrypted onboarding material is never loaded.
func (r *Repository) ProjectOnboardingStartTrigger(
	ctx context.Context,
	projection onboarding.StartTriggerProjection,
) (onboarding.OnboardingStartTrigger, bool, error) {
	if projection.ValidateAnchor() != nil {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("begin onboarding start trigger projection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := getOnboardingStartTrigger(ctx, tx, projection.EventID, true)
	if err == nil {
		if !sameOnboardingStartTriggerAnchor(existing, projection) {
			return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
		}
		if err := tx.Commit(); err != nil {
			return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("commit onboarding start trigger replay: %w", err)
		}
		return existing, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("read onboarding start trigger replay: %w", err)
	}
	if projection.Validate() != nil {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}
	labelsJSON, err := json.Marshal(projection.RequiredLabels)
	if err != nil {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}

	intent, err := lockOnboardingStartTriggerIntent(ctx, tx, projection.IntentID)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}
	if err != nil {
		return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("lock onboarding start trigger intent metadata: %w", err)
	}
	if intent.AccountID != projection.AccountID || intent.DesiredGeneration != projection.DesiredGeneration ||
		intent.Status != onboarding.IntentPending || !intent.ExpiresAt.After(projection.ProjectedAt) {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}

	if err := projectOnboardingReadySlot(ctx, tx, projection, labelsJSON); err != nil {
		return onboarding.OnboardingStartTrigger{}, false, err
	}
	reservationID, bindingRevision, err := lockCurrentOnboardingProxyReservation(
		ctx, tx, projection.AccountID, projection.DesiredGeneration, projection.ProjectedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}
	if err != nil {
		return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("lock onboarding start trigger proxy reservation: %w", err)
	}

	trigger := onboarding.OnboardingStartTrigger{
		StartTriggerProjection: projection,
		IntentExpiresAt:        intent.ExpiresAt,
		ReservationID:          reservationID,
		BindingRevision:        bindingRevision,
		Status:                 onboarding.StartTriggerPending,
		NextAttemptAt:          projection.ProjectedAt,
	}
	if trigger.Validate() != nil {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO onboarding_start_triggers (
  source_sequence, event_id, event_type, event_created_at,
  intent_id, intent_expires_at, account_id, desired_generation,
  slot_id, provider, required_labels_json, image_digest, cpu_request_millis, memory_request_bytes,
  reservation_id, binding_revision, status, next_attempt_at, projected_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		trigger.SourceSequence, trigger.EventID, trigger.EventType, trigger.EventCreatedAt,
		trigger.IntentID, trigger.IntentExpiresAt, trigger.AccountID, trigger.DesiredGeneration,
		trigger.SlotID, trigger.Provider, labelsJSON, trigger.ImageDigest,
		trigger.CPURequestMillis, trigger.MemoryRequestBytes, trigger.ReservationID,
		trigger.BindingRevision, trigger.NextAttemptAt, trigger.ProjectedAt,
	)
	if err != nil {
		if isOnboardingStartTriggerDuplicate(err) {
			replayed, replayErr := getOnboardingStartTrigger(ctx, tx, projection.EventID, true)
			if replayErr == nil && sameOnboardingStartTriggerAnchor(replayed, projection) {
				if commitErr := tx.Commit(); commitErr != nil {
					return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("commit concurrent onboarding start trigger replay: %w", commitErr)
				}
				return replayed, false, nil
			}
			return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
		}
		return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("insert onboarding start trigger: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}
	if err := tx.Commit(); err != nil {
		return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("commit onboarding start trigger projection: %w", err)
	}
	return trigger, true, nil
}

// ClaimDueOnboardingStartTriggers claims short-lived queue ownership without
// consulting mutable slot health. The atomic starter is the sole authority for
// deciding whether the frozen trigger currently has a usable runtime binding.
func (r *Repository) ClaimDueOnboardingStartTriggers(
	ctx context.Context,
	query onboarding.StartTriggerClaimQuery,
) ([]onboarding.OnboardingStartTrigger, error) {
	if query.Validate() != nil {
		return nil, onboarding.ErrStartTriggerRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin onboarding start trigger claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
UPDATE onboarding_start_triggers
SET status = 'expired', claim_owner = '', claim_expires_at = NULL
WHERE intent_expires_at <= ?
  AND (status = 'pending' OR (status = 'claimed' AND claim_expires_at <= ?))
ORDER BY next_attempt_at, source_sequence, intent_id
LIMIT ?`,
		query.ClaimedAt, query.ClaimedAt, query.Limit,
	); err != nil {
		return nil, fmt.Errorf("expire onboarding start triggers: %w", err)
	}

	rows, err := tx.QueryContext(ctx, onboardingStartTriggerClaimSelect,
		query.ClaimedAt, query.ClaimedAt, query.ClaimedAt, query.Limit,
	)
	if err != nil {
		return nil, fmt.Errorf("select due onboarding start triggers: %w", err)
	}
	triggers := make([]onboarding.OnboardingStartTrigger, 0, query.Limit)
	for rows.Next() {
		trigger, scanErr := scanOnboardingStartTrigger(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan due onboarding start trigger: %w", scanErr)
		}
		triggers = append(triggers, trigger)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate due onboarding start triggers: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close due onboarding start triggers: %w", err)
	}

	claimExpiresAt := query.ClaimedAt.Add(query.ClaimTTL)
	for index := range triggers {
		trigger := &triggers[index]
		result, err := tx.ExecContext(ctx, `
UPDATE onboarding_start_triggers
SET status = 'claimed', claim_owner = ?, claim_version = claim_version + 1, claim_expires_at = ?
WHERE event_id = ? AND claim_version = ? AND intent_expires_at > ?
  AND ((status = 'pending' AND next_attempt_at <= ?)
    OR (status = 'claimed' AND claim_expires_at <= ?))`,
			query.Owner, claimExpiresAt, trigger.EventID, trigger.ClaimVersion,
			query.ClaimedAt, query.ClaimedAt, query.ClaimedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("claim onboarding start trigger: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return nil, onboarding.ErrStartTriggerClaimLost
		}
		trigger.Status = onboarding.StartTriggerClaimed
		trigger.ClaimOwner = query.Owner
		trigger.ClaimVersion++
		expiresAt := claimExpiresAt.UTC()
		trigger.ClaimExpiresAt = &expiresAt
		if trigger.Validate() != nil {
			return nil, onboarding.ErrStartTriggerRejected
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit onboarding start trigger claim: %w", err)
	}
	return triggers, nil
}

func (r *Repository) RetryOnboardingStartTrigger(ctx context.Context, retry onboarding.StartTriggerRetry) error {
	if retry.Validate() != nil {
		return onboarding.ErrStartTriggerRejected
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE onboarding_start_triggers
SET status = 'pending', claim_owner = '', claim_expires_at = NULL,
    next_attempt_at = ?, attempt_count = attempt_count + 1, last_error_code = ?
WHERE event_id = ? AND status = 'claimed' AND claim_owner = ? AND claim_version = ?
  AND claim_expires_at > ?`,
		retry.NextAttemptAt, retry.ErrorCode, retry.EventID, retry.Owner, retry.ClaimVersion, retry.RetriedAt,
	)
	if err != nil {
		return fmt.Errorf("retry onboarding start trigger: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return onboarding.ErrStartTriggerClaimLost
	}
	return nil
}

func (r *Repository) LookupOnboardingStartTrigger(
	ctx context.Context,
	eventID string,
) (onboarding.OnboardingStartTrigger, bool, error) {
	if r == nil || r.db == nil || ctx == nil || ctx.Err() != nil || credential.ValidateTransportID(eventID) != nil {
		return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
	}
	trigger, err := getOnboardingStartTrigger(ctx, r.db, eventID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.OnboardingStartTrigger{}, false, nil
	}
	if err != nil {
		return onboarding.OnboardingStartTrigger{}, false, fmt.Errorf("lookup onboarding start trigger: %w", err)
	}
	return trigger, true, nil
}

func lockOnboardingStartTriggerIntent(
	ctx context.Context,
	tx *sql.Tx,
	intentID string,
) (onboardingStartTriggerIntent, error) {
	var intent onboardingStartTriggerIntent
	err := tx.QueryRowContext(ctx, onboardingStartTriggerIntentMetadataSelect, intentID).Scan(
		&intent.ID, &intent.AccountID, &intent.DesiredGeneration, &intent.Status, &intent.ExpiresAt,
	)
	intent.ExpiresAt = intent.ExpiresAt.UTC()
	return intent, err
}

func projectOnboardingReadySlot(
	ctx context.Context,
	tx *sql.Tx,
	projection onboarding.StartTriggerProjection,
	labelsJSON []byte,
) error {
	candidate := Slot{
		ID: projection.SlotID, AccountID: projection.AccountID, Provider: projection.Provider,
		DesiredState: "ready", DesiredGeneration: projection.DesiredGeneration,
		RequiredLabels: projection.RequiredLabels, ImageDigest: projection.ImageDigest,
		CPURequestMillis: projection.CPURequestMillis, MemoryRequestBytes: projection.MemoryRequestBytes,
		CreatedAt: projection.ProjectedAt, UpdatedAt: projection.ProjectedAt,
	}
	if _, err := validateDesiredSlot(candidate); err != nil {
		return onboarding.ErrStartTriggerRejected
	}
	existing, err := getSlot(ctx, tx, candidate.ID, true)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
INSERT INTO slots (
  slot_id, account_id, provider, desired_state, desired_generation, next_execution_epoch,
  required_labels_json, image_digest, cpu_request_millis, memory_request_bytes, created_at, updated_at
) VALUES (?, ?, ?, 'ready', ?, 1, ?, ?, ?, ?, ?, ?)`,
			candidate.ID, candidate.AccountID, candidate.Provider, candidate.DesiredGeneration,
			labelsJSON, candidate.ImageDigest, candidate.CPURequestMillis, candidate.MemoryRequestBytes,
			candidate.CreatedAt, candidate.UpdatedAt,
		)
		if err != nil {
			if isOnboardingStartTriggerDuplicate(err) {
				return onboarding.ErrStartTriggerRejected
			}
			return fmt.Errorf("insert onboarding start trigger ready slot: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("lock onboarding start trigger ready slot: %w", err)
	}
	existingLabels, err := json.Marshal(existing.RequiredLabels)
	if err != nil {
		return onboarding.ErrStartTriggerRejected
	}
	if candidate.AccountID != existing.AccountID || candidate.Provider != existing.Provider ||
		candidate.DesiredGeneration < existing.DesiredGeneration {
		return onboarding.ErrStartTriggerRejected
	}
	if candidate.DesiredGeneration == existing.DesiredGeneration {
		if existing.DesiredState != "ready" || candidate.ImageDigest != existing.ImageDigest ||
			candidate.CPURequestMillis != existing.CPURequestMillis ||
			candidate.MemoryRequestBytes != existing.MemoryRequestBytes ||
			!bytes.Equal(labelsJSON, existingLabels) {
			return onboarding.ErrStartTriggerRejected
		}
		return nil
	}
	result, err := tx.ExecContext(ctx, `
UPDATE slots SET desired_state = 'ready', desired_generation = ?, required_labels_json = ?,
  image_digest = ?, cpu_request_millis = ?, memory_request_bytes = ?, updated_at = ?
WHERE slot_id = ? AND desired_generation = ?`,
		candidate.DesiredGeneration, labelsJSON, candidate.ImageDigest, candidate.CPURequestMillis,
		candidate.MemoryRequestBytes, candidate.UpdatedAt, candidate.ID, existing.DesiredGeneration,
	)
	if err != nil {
		return fmt.Errorf("update onboarding start trigger ready slot: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return onboarding.ErrStartTriggerRejected
	}
	return nil
}

func lockCurrentOnboardingProxyReservation(
	ctx context.Context,
	tx *sql.Tx,
	accountID string,
	desiredGeneration uint64,
	checkedAt time.Time,
) (string, uint64, error) {
	var reservationID string
	var bindingRevision uint64
	err := tx.QueryRowContext(ctx, `
SELECT reservation_id, binding_revision
FROM proxy_reservation_grants
WHERE account_id = ? AND desired_generation = ? AND revoked_at IS NULL AND created_at <= ?
FOR UPDATE`, accountID, desiredGeneration, checkedAt).Scan(&reservationID, &bindingRevision)
	return reservationID, bindingRevision, err
}

func getOnboardingStartTrigger(
	ctx context.Context,
	queryer slotQueryer,
	eventID string,
	forUpdate bool,
) (onboarding.OnboardingStartTrigger, error) {
	query := onboardingStartTriggerSelect + " WHERE event_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanOnboardingStartTrigger(queryer.QueryRowContext(ctx, query, eventID))
}

type onboardingStartTriggerScanner interface {
	Scan(dest ...any) error
}

func scanOnboardingStartTrigger(scanner onboardingStartTriggerScanner) (onboarding.OnboardingStartTrigger, error) {
	var trigger onboarding.OnboardingStartTrigger
	var labelsJSON []byte
	var claimExpiresAt, startedAt sql.NullTime
	err := scanner.Scan(
		&trigger.SourceSequence, &trigger.EventID, &trigger.EventType, &trigger.EventCreatedAt,
		&trigger.IntentID, &trigger.IntentExpiresAt, &trigger.AccountID, &trigger.DesiredGeneration,
		&trigger.SlotID, &trigger.Provider, &labelsJSON, &trigger.ImageDigest,
		&trigger.CPURequestMillis, &trigger.MemoryRequestBytes, &trigger.ReservationID,
		&trigger.BindingRevision, &trigger.Status, &trigger.ClaimOwner, &trigger.ClaimVersion,
		&claimExpiresAt, &trigger.NextAttemptAt, &trigger.AttemptCount, &trigger.LastErrorCode,
		&trigger.StartedWorkflowID, &startedAt, &trigger.ProjectedAt,
	)
	if err != nil {
		return onboarding.OnboardingStartTrigger{}, err
	}
	if err := json.Unmarshal(labelsJSON, &trigger.RequiredLabels); err != nil {
		return onboarding.OnboardingStartTrigger{}, onboarding.ErrStartTriggerRejected
	}
	trigger.EventCreatedAt = trigger.EventCreatedAt.UTC()
	trigger.IntentExpiresAt = trigger.IntentExpiresAt.UTC()
	trigger.NextAttemptAt = trigger.NextAttemptAt.UTC()
	trigger.ProjectedAt = trigger.ProjectedAt.UTC()
	if claimExpiresAt.Valid {
		value := claimExpiresAt.Time.UTC()
		trigger.ClaimExpiresAt = &value
	}
	if startedAt.Valid {
		value := startedAt.Time.UTC()
		trigger.StartedAt = &value
	}
	if trigger.Validate() != nil {
		return onboarding.OnboardingStartTrigger{}, onboarding.ErrStartTriggerRejected
	}
	return trigger, nil
}

func sameOnboardingStartTriggerAnchor(
	trigger onboarding.OnboardingStartTrigger,
	projection onboarding.StartTriggerProjection,
) bool {
	return trigger.SourceSequence == projection.SourceSequence && trigger.EventID == projection.EventID &&
		trigger.EventType == projection.EventType && trigger.EventCreatedAt.Equal(projection.EventCreatedAt) &&
		trigger.IntentID == projection.IntentID && trigger.AccountID == projection.AccountID &&
		trigger.DesiredGeneration == projection.DesiredGeneration && trigger.SlotID == projection.SlotID &&
		trigger.Provider == projection.Provider
}

func isOnboardingStartTriggerDuplicate(err error) bool {
	var mysqlError *mysql.MySQLError
	return errors.As(err, &mysqlError) && mysqlError.Number == 1062
}

const onboardingStartTriggerIntentMetadataSelect = `
SELECT intent_id, account_id, desired_generation, status, expires_at
FROM onboarding_intents WHERE intent_id = ? FOR UPDATE`

const onboardingStartTriggerSelect = `
SELECT source_sequence, event_id, event_type, event_created_at,
       intent_id, intent_expires_at, account_id, desired_generation,
       slot_id, provider, required_labels_json, image_digest, cpu_request_millis, memory_request_bytes,
       reservation_id, binding_revision, status, claim_owner, claim_version, claim_expires_at,
       next_attempt_at, attempt_count, last_error_code, started_workflow_id, started_at, projected_at
FROM onboarding_start_triggers`

const onboardingStartTriggerClaimSelect = onboardingStartTriggerSelect + `
WHERE intent_expires_at > ?
  AND ((status = 'pending' AND next_attempt_at <= ?)
    OR (status = 'claimed' AND claim_expires_at <= ?))
ORDER BY next_attempt_at, source_sequence, intent_id
LIMIT ?
FOR UPDATE SKIP LOCKED`

var _ onboarding.OnboardingStartTriggerRepository = (*Repository)(nil)
