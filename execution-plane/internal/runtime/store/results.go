package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

var controlSessionIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func (r *Repository) ApplyCommandResult(ctx context.Context, result CommandResult) error {
	if err := validateAppliedCommandResult(result); err != nil {
		return err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin command result transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if result.ControlSessionID != "" {
		// Serialize the proof with Hello/disconnect updates to this same node.
		// A late result must not establish proof for a replacement session.
		var currentSessionID sql.NullString
		err := tx.QueryRowContext(ctx, `
SELECT control_session_id FROM nodes WHERE node_id = ? AND status = 'connected' FOR UPDATE`,
			result.NodeID).Scan(&currentSessionID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNodeNotFound
		}
		if err != nil {
			return fmt.Errorf("lock command result node session: %w", err)
		}
		if !currentSessionID.Valid || currentSessionID.String != result.ControlSessionID {
			return ErrNodeNotFound
		}
	}
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
		healthy, observedSessionID := commandObservationProof(result)
		if observedSessionID != "" {
			// The command/reply image match alone is insufficient: the durable
			// assignment must still name that image, including commands without
			// a provisioning job. Keep this lock through the observation write.
			var assignedImage string
			err := tx.QueryRowContext(ctx, `
SELECT image_digest FROM slot_assignments
WHERE slot_id = ? AND node_id = ? AND execution_epoch = ? AND released_at IS NULL FOR UPDATE`,
				result.Observation.SlotID, result.NodeID, result.Observation.ExecutionEpoch).Scan(&assignedImage)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && assignedImage != result.ExpectedImageDigest) {
				return ErrAssignmentNotFound
			}
			if err != nil {
				return fmt.Errorf("lock command result assignment image: %w", err)
			}
		}
		update, err := tx.ExecContext(ctx, `
UPDATE slot_assignments SET
  provider_ref = NULLIF(?, ''),
  actual_generation = actual_generation + IF(actual_state <> ?, 1, 0),
  actual_state = ?, healthy = ?, reason_code = ?, last_observed_at = ?,
  observed_control_session_id = NULLIF(?, '')
WHERE slot_id = ? AND node_id = ? AND execution_epoch = ? AND released_at IS NULL`,
			result.Observation.ProviderRef, result.Observation.ActualState, result.Observation.ActualState,
			healthy, result.Observation.ReasonCode, result.Observation.ObservedAt.UTC(), observedSessionID,
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.ControlSessionID != "" {
		node, exists := r.nodes[result.NodeID]
		if !exists || node.Status != "connected" || node.ControlSessionID != result.ControlSessionID {
			return ErrNodeNotFound
		}
	}
	if existing, exists := r.commandResults[result.CommandID]; exists {
		if existing.NodeID != result.NodeID {
			return errors.New("command result belongs to a different node")
		}
	}
	// Hold both stores until every validation has passed, matching the SQL
	// transaction. No job-only path acquires r.mu while holding jobs.mu.
	r.jobs.mu.Lock()
	defer r.jobs.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	jobKey, jobExists := r.jobs.keyByID[result.CommandID]
	job := r.jobs.byKey[jobKey]
	assignmentIndex := -1
	var assignments []Assignment
	if result.Observation != nil {
		if jobExists && job.SlotID != result.Observation.SlotID {
			return errors.New("command result slot does not match provisioning job")
		}
		assignments = r.assignments[result.Observation.SlotID]
		for index, assignment := range assignments {
			if assignment.ReleasedAt == nil && assignment.NodeID == result.NodeID && assignment.ExecutionEpoch == result.Observation.ExecutionEpoch {
				assignmentIndex = index
				break
			}
		}
		_, observedSessionID := commandObservationProof(result)
		if (jobExists || observedSessionID != "") && assignmentIndex < 0 {
			return ErrAssignmentNotFound
		}
		if observedSessionID != "" && assignments[assignmentIndex].ImageDigest != result.ExpectedImageDigest {
			return ErrAssignmentNotFound
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, exists := r.commandResults[result.CommandID]; !exists {
		storedResult := result
		storedResult.SlotObservationJSON = append([]byte(nil), result.SlotObservationJSON...)
		if result.Observation != nil {
			observation := *result.Observation
			storedResult.Observation = &observation
		}
		r.commandResults[result.CommandID] = storedResult
	}
	if assignmentIndex >= 0 {
		assignment := &assignments[assignmentIndex]
		if assignment.ActualState != result.Observation.ActualState {
			assignment.ActualGeneration++
		}
		assignment.ProviderRef = result.Observation.ProviderRef
		assignment.ActualState = result.Observation.ActualState
		assignment.Healthy, assignment.ObservedControlSessionID = commandObservationProof(result)
		assignment.ReasonCode = result.Observation.ReasonCode
		observed := result.Observation.ObservedAt.UTC()
		assignment.LastObservedAt = &observed
		r.assignments[result.Observation.SlotID] = assignments
	}
	if jobExists && (job.Status == "running" || job.Status == "dispatched" || (result.Succeeded && job.Status == "failed")) {
		job.UpdatedAt = result.ReceivedAt.UTC()
		if result.Succeeded {
			job.Status, job.ErrorCode, job.NextAttemptAt = "completed", "", nil
		} else {
			job.Status, job.ErrorCode = "failed", result.ErrorCode
			retryAt := result.RetryAt.UTC()
			job.NextAttemptAt = &retryAt
		}
		r.jobs.byKey[jobKey] = job
	}
	return nil
}

// A failed command cannot turn a claimed healthy observation into authority.
// Empty legacy sessions always clear proof, even for a healthy observation.
func commandObservationProof(result CommandResult) (bool, string) {
	healthy := result.Succeeded && result.Observation.Healthy
	if healthy {
		return true, result.ControlSessionID
	}
	return false, ""
}

func validateAppliedCommandResult(result CommandResult) error {
	if result.CommandID == "" || result.NodeID == "" || result.ReceivedAt.IsZero() ||
		!result.RetryAt.After(result.ReceivedAt) || len(result.ErrorCode) > 64 || len(result.ErrorMessage) > 1024 {
		return errors.New("invalid applied command result")
	}
	if result.ControlSessionID != "" && !controlSessionIDPattern.MatchString(result.ControlSessionID) {
		return errors.New("invalid command result control session")
	}
	if len(result.SlotObservationJSON) > 0 && !json.Valid(result.SlotObservationJSON) {
		return errors.New("applied command slot observation must be valid JSON")
	}
	if !result.Succeeded && result.ErrorCode == "" {
		return errors.New("failed command result requires an error code")
	}
	if result.Observation != nil {
		if err := validateObservation(*result.Observation); err != nil {
			return err
		}
		if _, session := commandObservationProof(result); session != "" && !runtimeImageDigestPattern.MatchString(result.ExpectedImageDigest) {
			return errors.New("invalid command result expected image")
		}
	}
	return nil
}
