package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func (r *Repository) ApplyCommandResult(ctx context.Context, result CommandResult) error {
	if err := validateAppliedCommandResult(result); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin command result transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var observation any
	if len(result.SlotObservationJSON) > 0 {
		observation = result.SlotObservationJSON
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO node_command_results (
  command_id, node_id, succeeded, error_code, error_message, slot_observation_json, received_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE command_id = command_id`,
		result.CommandID, result.NodeID, result.Succeeded, result.ErrorCode, result.ErrorMessage,
		observation, result.ReceivedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("record applied command result: %w", err)
	}
	var recordedNodeID string
	if err := tx.QueryRowContext(ctx, `SELECT node_id FROM node_command_results WHERE command_id = ?`, result.CommandID).Scan(&recordedNodeID); err != nil {
		return fmt.Errorf("verify command result owner: %w", err)
	}
	if recordedNodeID != result.NodeID {
		return errors.New("command result belongs to a different node")
	}

	var jobSlotID string
	jobErr := tx.QueryRowContext(ctx, `SELECT slot_id FROM provisioning_jobs WHERE job_id = ? FOR UPDATE`, result.CommandID).Scan(&jobSlotID)
	jobExists := jobErr == nil
	if jobErr != nil && !errors.Is(jobErr, sql.ErrNoRows) {
		return fmt.Errorf("lock command provisioning job: %w", jobErr)
	}
	if result.Observation != nil {
		if jobExists && jobSlotID != result.Observation.SlotID {
			return errors.New("command result slot does not match provisioning job")
		}
		update, err := tx.ExecContext(ctx, `
UPDATE slot_assignments SET
  provider_ref = NULLIF(?, ''),
  actual_generation = actual_generation + IF(actual_state <> ?, 1, 0),
  actual_state = ?, healthy = ?, reason_code = ?, last_observed_at = ?
WHERE slot_id = ? AND node_id = ? AND execution_epoch = ? AND released_at IS NULL`,
			result.Observation.ProviderRef, result.Observation.ActualState, result.Observation.ActualState,
			result.Observation.Healthy, result.Observation.ReasonCode, result.Observation.ObservedAt.UTC(),
			result.Observation.SlotID, result.NodeID, result.Observation.ExecutionEpoch,
		)
		if err != nil {
			return fmt.Errorf("apply command slot observation: %w", err)
		}
		if jobExists {
			affected, err := update.RowsAffected()
			if err != nil || affected != 1 {
				return ErrAssignmentNotFound
			}
		}
	}
	if len(result.SlotObservationJSON) > 0 && !json.Valid(result.SlotObservationJSON) {
		return errors.New("applied command slot observation must be valid JSON")
	}
	if jobExists {
		if result.Succeeded {
			_, err = tx.ExecContext(ctx, `
UPDATE provisioning_jobs SET status = 'completed', error_code = '', next_attempt_at = NULL, updated_at = ?
WHERE job_id = ? AND status IN ('running', 'dispatched', 'failed')`, result.ReceivedAt.UTC(), result.CommandID)
		} else {
			_, err = tx.ExecContext(ctx, `
UPDATE provisioning_jobs SET status = 'failed', error_code = ?, next_attempt_at = ?, updated_at = ?
WHERE job_id = ? AND status IN ('running', 'dispatched')`,
				result.ErrorCode, result.RetryAt.UTC(), result.ReceivedAt.UTC(), result.CommandID)
		}
		if err != nil {
			return fmt.Errorf("apply command provisioning job result: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit command result: %w", err)
	}
	return nil
}

func (r *MemoryRepository) ApplyCommandResult(ctx context.Context, result CommandResult) error {
	if err := validateAppliedCommandResult(result); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, exists := r.commandResults[result.CommandID]; exists {
		if existing.NodeID != result.NodeID {
			return errors.New("command result belongs to a different node")
		}
	} else {
		result.SlotObservationJSON = append([]byte(nil), result.SlotObservationJSON...)
		r.commandResults[result.CommandID] = result
	}
	if result.Observation == nil {
		return r.applyMemoryJobResult(ctx, result)
	}
	assignments := r.assignments[result.Observation.SlotID]
	for index := range assignments {
		assignment := &assignments[index]
		if assignment.ReleasedAt == nil && assignment.NodeID == result.NodeID && assignment.ExecutionEpoch == result.Observation.ExecutionEpoch {
			if assignment.ActualState != result.Observation.ActualState {
				assignment.ActualGeneration++
			}
			assignment.ProviderRef = result.Observation.ProviderRef
			assignment.ActualState = result.Observation.ActualState
			assignment.Healthy = result.Observation.Healthy
			assignment.ReasonCode = result.Observation.ReasonCode
			observed := result.Observation.ObservedAt.UTC()
			assignment.LastObservedAt = &observed
			r.assignments[result.Observation.SlotID] = assignments
			break
		}
	}
	return r.applyMemoryJobResult(ctx, result)
}

func (r *MemoryRepository) applyMemoryJobResult(ctx context.Context, result CommandResult) error {
	var err error
	if result.Succeeded {
		err = r.jobs.CompleteProvisioningJob(ctx, result.CommandID, result.ReceivedAt)
	} else {
		err = r.jobs.FailProvisioningJob(ctx, result.CommandID, result.ErrorCode, result.ReceivedAt, result.RetryAt)
	}
	if errors.Is(err, ErrJobNotClaimed) {
		return nil
	}
	return err
}

func validateAppliedCommandResult(result CommandResult) error {
	if result.CommandID == "" || result.NodeID == "" || result.ReceivedAt.IsZero() ||
		!result.RetryAt.After(result.ReceivedAt) || len(result.ErrorCode) > 64 || len(result.ErrorMessage) > 1024 {
		return errors.New("invalid applied command result")
	}
	if !result.Succeeded && result.ErrorCode == "" {
		return errors.New("failed command result requires an error code")
	}
	if result.Observation != nil {
		if err := validateObservation(*result.Observation); err != nil {
			return err
		}
	}
	return nil
}
