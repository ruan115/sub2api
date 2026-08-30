package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

var (
	ErrStaleGeneration        = errors.New("slot desired generation is stale or conflicting")
	ErrSlotNotFound           = errors.New("runtime slot not found")
	ErrAssignmentConflict     = errors.New("slot already has an active assignment")
	ErrAssignmentNotFound     = errors.New("active slot assignment not found")
	ErrNodeCapacity           = errors.New("node is offline, changed session or lacks reserved capacity")
	runtimeImageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Slot struct {
	ID                 string
	AccountID          string
	Provider           string
	DesiredState       string
	DesiredGeneration  uint64
	NextExecutionEpoch uint64
	RequiredLabels     map[string]string
	ImageDigest        string
	CPURequestMillis   uint64
	MemoryRequestBytes uint64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type Assignment struct {
	ID                 string
	SlotID             string
	NodeID             string
	ProviderRef        string
	ExecutionEpoch     uint64
	DesiredGeneration  uint64
	ImageDigest        string
	CPURequestMillis   uint64
	MemoryRequestBytes uint64
	ActualState        string
	ActualGeneration   uint64
	Healthy            bool
	ReasonCode         string
	AssignedAt         time.Time
	LastObservedAt     *time.Time
	ReleasedAt         *time.Time
}

type AssignmentReservation struct {
	ID                    string
	SlotID                string
	NodeID                string
	ExpectedNodeSessionID string
	NodeSeenAfter         time.Time
	ReservedAt            time.Time
}

type AssignmentObservation struct {
	SlotID         string
	ExecutionEpoch uint64
	ProviderRef    string
	ActualState    string
	Healthy        bool
	ReasonCode     string
	ObservedAt     time.Time
}

type SlotRepository interface {
	PutDesiredSlot(ctx context.Context, candidate Slot) (Slot, error)
	GetSlot(ctx context.Context, slotID string) (Slot, error)
	ReserveAssignment(ctx context.Context, reservation AssignmentReservation) (Assignment, error)
	GetActiveAssignment(ctx context.Context, slotID string) (Assignment, error)
	ObserveAssignment(ctx context.Context, observation AssignmentObservation) (Assignment, error)
	ReleaseAssignment(ctx context.Context, slotID string, executionEpoch uint64, releasedAt time.Time) error
	ForceReleaseAssignment(ctx context.Context, slotID string, executionEpoch uint64, reasonCode string, releasedAt time.Time) error
}

func (r *Repository) PutDesiredSlot(ctx context.Context, candidate Slot) (Slot, error) {
	labels, err := validateDesiredSlot(candidate)
	if err != nil {
		return Slot{}, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Slot{}, fmt.Errorf("begin desired slot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, getErr := getSlot(ctx, tx, candidate.ID, true)
	switch {
	case errors.Is(getErr, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
INSERT INTO slots (
  slot_id, account_id, provider, desired_state, desired_generation, next_execution_epoch,
  required_labels_json, image_digest, cpu_request_millis, memory_request_bytes, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)`,
			candidate.ID, candidate.AccountID, candidate.Provider, candidate.DesiredState, candidate.DesiredGeneration,
			labels, candidate.ImageDigest, candidate.CPURequestMillis, candidate.MemoryRequestBytes,
			candidate.CreatedAt.UTC(), candidate.UpdatedAt.UTC(),
		)
		if err != nil {
			return Slot{}, fmt.Errorf("insert desired slot: %w", err)
		}
	case getErr != nil:
		return Slot{}, getErr
	default:
		if candidate.AccountID != existing.AccountID || candidate.Provider != existing.Provider || candidate.DesiredGeneration < existing.DesiredGeneration {
			return Slot{}, ErrStaleGeneration
		}
		if candidate.DesiredGeneration == existing.DesiredGeneration {
			existingLabels, _ := json.Marshal(existing.RequiredLabels)
			if candidate.DesiredState != existing.DesiredState || candidate.ImageDigest != existing.ImageDigest ||
				candidate.CPURequestMillis != existing.CPURequestMillis || candidate.MemoryRequestBytes != existing.MemoryRequestBytes ||
				!bytes.Equal(labels, existingLabels) {
				return Slot{}, ErrStaleGeneration
			}
			if err := tx.Commit(); err != nil {
				return Slot{}, fmt.Errorf("commit idempotent desired slot: %w", err)
			}
			return existing, nil
		}
		_, err = tx.ExecContext(ctx, `
UPDATE slots SET desired_state = ?, desired_generation = ?, required_labels_json = ?,
  image_digest = ?, cpu_request_millis = ?, memory_request_bytes = ?, updated_at = ?
WHERE slot_id = ?`, candidate.DesiredState, candidate.DesiredGeneration, labels,
			candidate.ImageDigest, candidate.CPURequestMillis, candidate.MemoryRequestBytes, candidate.UpdatedAt.UTC(), candidate.ID)
		if err != nil {
			return Slot{}, fmt.Errorf("update desired slot: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Slot{}, fmt.Errorf("commit desired slot: %w", err)
	}
	return r.GetSlot(ctx, candidate.ID)
}

func (r *Repository) GetSlot(ctx context.Context, slotID string) (Slot, error) {
	slot, err := getSlot(ctx, r.db, slotID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return Slot{}, ErrSlotNotFound
	}
	return slot, err
}

func (r *Repository) ReserveAssignment(ctx context.Context, reservation AssignmentReservation) (Assignment, error) {
	if reservation.ID == "" || reservation.SlotID == "" || reservation.NodeID == "" || reservation.ExpectedNodeSessionID == "" ||
		reservation.NodeSeenAfter.IsZero() || reservation.ReservedAt.IsZero() {
		return Assignment{}, errors.New("invalid slot assignment reservation")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Assignment{}, fmt.Errorf("begin slot assignment reservation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	slot, err := getSlot(ctx, tx, reservation.SlotID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return Assignment{}, ErrSlotNotFound
	}
	if err != nil {
		return Assignment{}, err
	}
	if slot.DesiredState != "ready" {
		return Assignment{}, errors.New("slot is not desired ready")
	}
	active, activeErr := getActiveAssignment(ctx, tx, slot.ID, true)
	if activeErr == nil {
		if active.ID == reservation.ID && active.NodeID == reservation.NodeID {
			if err := tx.Commit(); err != nil {
				return Assignment{}, fmt.Errorf("commit idempotent slot assignment: %w", err)
			}
			return active, nil
		}
		return Assignment{}, ErrAssignmentConflict
	}
	if !errors.Is(activeErr, sql.ErrNoRows) {
		return Assignment{}, activeErr
	}
	result, err := tx.ExecContext(ctx, `
UPDATE nodes SET
  reserved_slots = GREATEST(reserved_slots, allocated_slots) + 1,
  reserved_cpu_millis = GREATEST(reserved_cpu_millis, allocated_cpu_millis) + ?,
  reserved_memory_bytes = GREATEST(reserved_memory_bytes, allocated_memory_bytes) + ?,
  updated_at = ?
WHERE node_id = ? AND status = 'connected' AND control_session_id = ? AND last_seen_at >= ?
  AND GREATEST(reserved_slots, allocated_slots) < max_slots
  AND ? <= allocatable_cpu_millis - GREATEST(reserved_cpu_millis, allocated_cpu_millis)
  AND ? <= allocatable_memory_bytes - GREATEST(reserved_memory_bytes, allocated_memory_bytes)`,
		slot.CPURequestMillis, slot.MemoryRequestBytes, reservation.ReservedAt.UTC(),
		reservation.NodeID, reservation.ExpectedNodeSessionID, reservation.NodeSeenAfter.UTC(),
		slot.CPURequestMillis, slot.MemoryRequestBytes,
	)
	if err != nil {
		return Assignment{}, fmt.Errorf("reserve node slot capacity: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return Assignment{}, fmt.Errorf("read node slot reservation: %w", err)
	}
	if affected != 1 {
		return Assignment{}, ErrNodeCapacity
	}
	epoch := slot.NextExecutionEpoch
	result, err = tx.ExecContext(ctx, `UPDATE slots SET next_execution_epoch = next_execution_epoch + 1, updated_at = ? WHERE slot_id = ? AND next_execution_epoch = ?`,
		reservation.ReservedAt.UTC(), slot.ID, epoch)
	if err != nil {
		return Assignment{}, fmt.Errorf("advance slot execution epoch: %w", err)
	}
	if affected, err = result.RowsAffected(); err != nil || affected != 1 {
		return Assignment{}, ErrAssignmentConflict
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO slot_assignments (
  assignment_id, slot_id, node_id, execution_epoch, desired_generation, image_digest,
  cpu_request_millis, memory_request_bytes, actual_state, actual_generation, assigned_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'missing', 1, ?)`,
		reservation.ID, slot.ID, reservation.NodeID, epoch, slot.DesiredGeneration, slot.ImageDigest,
		slot.CPURequestMillis, slot.MemoryRequestBytes, reservation.ReservedAt.UTC(),
	)
	if err != nil {
		return Assignment{}, fmt.Errorf("insert slot assignment: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Assignment{}, fmt.Errorf("commit slot assignment: %w", err)
	}
	return r.GetActiveAssignment(ctx, slot.ID)
}

func (r *Repository) GetActiveAssignment(ctx context.Context, slotID string) (Assignment, error) {
	assignment, err := getActiveAssignment(ctx, r.db, slotID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return Assignment{}, ErrAssignmentNotFound
	}
	return assignment, err
}

func (r *Repository) ObserveAssignment(ctx context.Context, observation AssignmentObservation) (Assignment, error) {
	if err := validateObservation(observation); err != nil {
		return Assignment{}, err
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE slot_assignments SET
  provider_ref = NULLIF(?, ''),
  actual_generation = actual_generation + IF(actual_state <> ?, 1, 0),
  actual_state = ?, healthy = ?, reason_code = ?, last_observed_at = ?
WHERE slot_id = ? AND execution_epoch = ? AND released_at IS NULL`,
		observation.ProviderRef, observation.ActualState, observation.ActualState, observation.Healthy,
		observation.ReasonCode, observation.ObservedAt.UTC(), observation.SlotID, observation.ExecutionEpoch,
	)
	if err != nil {
		return Assignment{}, fmt.Errorf("observe slot assignment: %w", err)
	}
	if err := requireOneAssignment(result); err != nil {
		return Assignment{}, err
	}
	return r.GetActiveAssignment(ctx, observation.SlotID)
}

func (r *Repository) ReleaseAssignment(ctx context.Context, slotID string, executionEpoch uint64, releasedAt time.Time) error {
	if slotID == "" || executionEpoch == 0 || releasedAt.IsZero() {
		return errors.New("invalid slot assignment release")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin slot assignment release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	assignment, err := getActiveAssignment(ctx, tx, slotID, true)
	if errors.Is(err, sql.ErrNoRows) {
		var released sql.NullTime
		historyErr := tx.QueryRowContext(ctx, `
SELECT released_at FROM slot_assignments WHERE slot_id = ? AND execution_epoch = ?`, slotID, executionEpoch).Scan(&released)
		if historyErr == nil && released.Valid {
			if commitErr := tx.Commit(); commitErr != nil {
				return fmt.Errorf("commit idempotent slot assignment release: %w", commitErr)
			}
			return nil
		}
		return ErrAssignmentNotFound
	}
	if err == nil && assignment.ExecutionEpoch != executionEpoch {
		return ErrAssignmentNotFound
	}
	if err != nil {
		return err
	}
	if assignment.ActualState != "destroyed" && assignment.ActualState != "missing" {
		return errors.New("slot assignment must be destroyed before release")
	}
	result, err := tx.ExecContext(ctx, `
UPDATE slot_assignments SET released_at = ?, healthy = FALSE,
  actual_generation = actual_generation + IF(actual_state <> 'destroyed', 1, 0),
  actual_state = 'destroyed'
WHERE assignment_id = ? AND released_at IS NULL`, releasedAt.UTC(), assignment.ID)
	if err != nil {
		return fmt.Errorf("release slot assignment: %w", err)
	}
	if err := requireOneAssignment(result); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE nodes SET
  reserved_slots = IF(reserved_slots > 0, reserved_slots - 1, 0),
  reserved_cpu_millis = IF(reserved_cpu_millis >= ?, reserved_cpu_millis - ?, 0),
  reserved_memory_bytes = IF(reserved_memory_bytes >= ?, reserved_memory_bytes - ?, 0),
  updated_at = ?
WHERE node_id = ?`, assignment.CPURequestMillis, assignment.CPURequestMillis,
		assignment.MemoryRequestBytes, assignment.MemoryRequestBytes, releasedAt.UTC(), assignment.NodeID)
	if err != nil {
		return fmt.Errorf("release node slot capacity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit slot assignment release: %w", err)
	}
	return nil
}

func (r *Repository) ForceReleaseAssignment(ctx context.Context, slotID string, executionEpoch uint64, reasonCode string, releasedAt time.Time) error {
	if slotID == "" || executionEpoch == 0 || reasonCode == "" || len(reasonCode) > 64 || releasedAt.IsZero() {
		return errors.New("invalid forced slot assignment release")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin forced slot assignment release: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	assignment, err := getActiveAssignment(ctx, tx, slotID, true)
	if errors.Is(err, sql.ErrNoRows) {
		var released sql.NullTime
		historyErr := tx.QueryRowContext(ctx, `
SELECT released_at FROM slot_assignments WHERE slot_id = ? AND execution_epoch = ?`, slotID, executionEpoch).Scan(&released)
		if historyErr == nil && released.Valid {
			return tx.Commit()
		}
		return ErrAssignmentNotFound
	}
	if err != nil {
		return err
	}
	if assignment.ExecutionEpoch != executionEpoch {
		return ErrAssignmentNotFound
	}
	result, err := tx.ExecContext(ctx, `
UPDATE slot_assignments SET released_at = ?, healthy = FALSE, reason_code = ?,
  actual_generation = actual_generation + IF(actual_state <> 'fenced', 1, 0), actual_state = 'fenced'
WHERE assignment_id = ? AND released_at IS NULL`, releasedAt.UTC(), reasonCode, assignment.ID)
	if err != nil {
		return fmt.Errorf("force release slot assignment: %w", err)
	}
	if err := requireOneAssignment(result); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
UPDATE nodes SET
  reserved_slots = IF(reserved_slots > 0, reserved_slots - 1, 0),
  reserved_cpu_millis = IF(reserved_cpu_millis >= ?, reserved_cpu_millis - ?, 0),
  reserved_memory_bytes = IF(reserved_memory_bytes >= ?, reserved_memory_bytes - ?, 0),
  updated_at = ?
WHERE node_id = ?`, assignment.CPURequestMillis, assignment.CPURequestMillis,
		assignment.MemoryRequestBytes, assignment.MemoryRequestBytes, releasedAt.UTC(), assignment.NodeID)
	if err != nil {
		return fmt.Errorf("release forced node slot capacity: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit forced slot assignment release: %w", err)
	}
	return nil
}

type slotQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getSlot(ctx context.Context, queryer slotQueryer, slotID string, forUpdate bool) (Slot, error) {
	query := `
SELECT slot_id, account_id, provider, desired_state, desired_generation, next_execution_epoch,
       required_labels_json, image_digest, cpu_request_millis, memory_request_bytes, created_at, updated_at
FROM slots WHERE slot_id = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var slot Slot
	var labels []byte
	err := queryer.QueryRowContext(ctx, query, slotID).Scan(
		&slot.ID, &slot.AccountID, &slot.Provider, &slot.DesiredState, &slot.DesiredGeneration, &slot.NextExecutionEpoch,
		&labels, &slot.ImageDigest, &slot.CPURequestMillis, &slot.MemoryRequestBytes, &slot.CreatedAt, &slot.UpdatedAt,
	)
	if err != nil {
		return Slot{}, err
	}
	if err := json.Unmarshal(labels, &slot.RequiredLabels); err != nil {
		return Slot{}, fmt.Errorf("decode slot labels: %w", err)
	}
	return slot, nil
}

func getActiveAssignment(ctx context.Context, queryer slotQueryer, slotID string, forUpdate bool) (Assignment, error) {
	query := `
SELECT assignment_id, slot_id, node_id, COALESCE(provider_ref, ''), execution_epoch, desired_generation, image_digest,
       cpu_request_millis, memory_request_bytes, actual_state, actual_generation, healthy, reason_code,
       assigned_at, last_observed_at, released_at
FROM slot_assignments WHERE slot_id = ? AND released_at IS NULL`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var assignment Assignment
	var desiredGeneration sql.NullInt64
	var observed, released sql.NullTime
	err := queryer.QueryRowContext(ctx, query, slotID).Scan(
		&assignment.ID, &assignment.SlotID, &assignment.NodeID, &assignment.ProviderRef,
		&assignment.ExecutionEpoch, &desiredGeneration, &assignment.ImageDigest, &assignment.CPURequestMillis, &assignment.MemoryRequestBytes,
		&assignment.ActualState, &assignment.ActualGeneration, &assignment.Healthy, &assignment.ReasonCode,
		&assignment.AssignedAt, &observed, &released,
	)
	if err != nil {
		return Assignment{}, err
	}
	if desiredGeneration.Valid && desiredGeneration.Int64 > 0 {
		assignment.DesiredGeneration = uint64(desiredGeneration.Int64)
	}
	if observed.Valid {
		value := observed.Time.UTC()
		assignment.LastObservedAt = &value
	}
	if released.Valid {
		value := released.Time.UTC()
		assignment.ReleasedAt = &value
	}
	return assignment, nil
}

func validateDesiredSlot(slot Slot) ([]byte, error) {
	if slot.ID == "" || slot.AccountID == "" || slot.Provider != "docker" || slot.DesiredGeneration == 0 ||
		(slot.DesiredState != "ready" && slot.DesiredState != "drained" && slot.DesiredState != "absent") ||
		!runtimeImageDigestPattern.MatchString(slot.ImageDigest) || slot.CPURequestMillis == 0 || slot.MemoryRequestBytes == 0 ||
		slot.CreatedAt.IsZero() || slot.UpdatedAt.IsZero() {
		return nil, errors.New("invalid desired slot")
	}
	labels, err := json.Marshal(slot.RequiredLabels)
	if err != nil {
		return nil, errors.New("invalid desired slot labels")
	}
	return labels, nil
}

func validateObservation(observation AssignmentObservation) error {
	validState := map[string]bool{
		"missing": true, "creating": true, "created": true, "running": true, "draining": true,
		"drained": true, "stopped": true, "destroyed": true, "failed": true,
	}
	if observation.SlotID == "" || observation.ExecutionEpoch == 0 || !validState[observation.ActualState] ||
		len(observation.ProviderRef) > 255 || len(observation.ReasonCode) > 64 || observation.ObservedAt.IsZero() {
		return errors.New("invalid slot assignment observation")
	}
	return nil
}

func requireOneAssignment(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrAssignmentNotFound
	}
	return nil
}

var _ SlotRepository = (*Repository)(nil)
