package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

var ErrJobNotClaimed = errors.New("provisioning job is not claimed")

type ProvisioningJob struct {
	ID                string
	SlotID            string
	IdempotencyKey    string
	DesiredGeneration uint64
	Step              string
	Status            string
	RetryCount        uint32
	ErrorCode         string
	NextAttemptAt     *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type JobRepository interface {
	ClaimProvisioningJob(ctx context.Context, candidate ProvisioningJob, now time.Time, claimTTL time.Duration) (ProvisioningJob, bool, error)
	MarkProvisioningJobDispatched(ctx context.Context, jobID string, dispatchedAt time.Time) error
	CompleteProvisioningJob(ctx context.Context, jobID string, completedAt time.Time) error
	FailProvisioningJob(ctx context.Context, jobID, errorCode string, failedAt, retryAt time.Time) error
}

func (r *Repository) ClaimProvisioningJob(ctx context.Context, candidate ProvisioningJob, now time.Time, claimTTL time.Duration) (ProvisioningJob, bool, error) {
	if err := validateJobCandidate(candidate, now, claimTTL); err != nil {
		return ProvisioningJob{}, false, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ProvisioningJob{}, false, fmt.Errorf("begin provisioning job claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
INSERT INTO provisioning_jobs (
  job_id, slot_id, idempotency_key, desired_generation, step, status,
  retry_count, error_code, next_attempt_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'pending', 0, '', ?, ?, ?)
ON DUPLICATE KEY UPDATE idempotency_key = VALUES(idempotency_key)`,
		candidate.ID, candidate.SlotID, candidate.IdempotencyKey, candidate.DesiredGeneration,
		candidate.Step, now.UTC(), now.UTC(), now.UTC(),
	)
	if err != nil {
		return ProvisioningJob{}, false, fmt.Errorf("create idempotent provisioning job: %w", err)
	}
	claimUntil := now.Add(claimTTL).UTC()
	result, err := tx.ExecContext(ctx, `
UPDATE provisioning_jobs SET
  status = 'running',
  retry_count = retry_count + CASE WHEN status = 'pending' THEN 0 ELSE 1 END,
  error_code = '', next_attempt_at = ?, updated_at = ?
WHERE idempotency_key = ? AND (
  status = 'pending' OR
  (status IN ('failed', 'running', 'dispatched') AND next_attempt_at <= ?)
)`, claimUntil, now.UTC(), candidate.IdempotencyKey, now.UTC())
	if err != nil {
		return ProvisioningJob{}, false, fmt.Errorf("claim provisioning job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ProvisioningJob{}, false, fmt.Errorf("read provisioning job claim: %w", err)
	}
	job, err := getProvisioningJob(ctx, tx, candidate.IdempotencyKey)
	if err != nil {
		return ProvisioningJob{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProvisioningJob{}, false, fmt.Errorf("commit provisioning job claim: %w", err)
	}
	return job, affected == 1, nil
}

func (r *Repository) MarkProvisioningJobDispatched(ctx context.Context, jobID string, dispatchedAt time.Time) error {
	if jobID == "" || dispatchedAt.IsZero() {
		return errors.New("invalid provisioning job dispatch")
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE provisioning_jobs SET status = 'dispatched', updated_at = ?
WHERE job_id = ? AND status = 'running'`, dispatchedAt.UTC(), jobID)
	if err != nil {
		return fmt.Errorf("mark provisioning job dispatched: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read provisioning job dispatch: %w", err)
	}
	if affected != 1 {
		return ErrJobNotClaimed
	}
	return nil
}

func (r *Repository) CompleteProvisioningJob(ctx context.Context, jobID string, completedAt time.Time) error {
	return r.transitionJob(ctx, jobID, "completed", "", completedAt, time.Time{}, "running", "dispatched")
}

func (r *Repository) FailProvisioningJob(ctx context.Context, jobID, errorCode string, failedAt, retryAt time.Time) error {
	if errorCode == "" || len(errorCode) > 64 || !retryAt.After(failedAt) {
		return errors.New("invalid provisioning job failure")
	}
	return r.transitionJob(ctx, jobID, "failed", errorCode, failedAt, retryAt, "running", "dispatched")
}

func (r *Repository) transitionJob(ctx context.Context, jobID, targetStatus, errorCode string, updatedAt, nextAttemptAt time.Time, allowedStatuses ...string) error {
	if jobID == "" || updatedAt.IsZero() || len(allowedStatuses) == 0 {
		return errors.New("invalid provisioning job transition")
	}
	placeholders := "?"
	for range allowedStatuses[1:] {
		placeholders += ", ?"
	}
	query := `UPDATE provisioning_jobs SET status = ?, error_code = ?, next_attempt_at = ?, updated_at = ? WHERE job_id = ? AND status IN (` + placeholders + `)`
	var next any
	if !nextAttemptAt.IsZero() {
		next = nextAttemptAt.UTC()
	}
	arguments := []any{targetStatus, errorCode, next, updatedAt.UTC(), jobID}
	for _, allowed := range allowedStatuses {
		arguments = append(arguments, allowed)
	}
	result, err := r.db.ExecContext(ctx, query, arguments...)
	if err != nil {
		return fmt.Errorf("transition provisioning job: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read provisioning job transition: %w", err)
	}
	if affected != 1 {
		return ErrJobNotClaimed
	}
	return nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getProvisioningJob(ctx context.Context, queryer queryRower, idempotencyKey string) (ProvisioningJob, error) {
	var job ProvisioningJob
	var next sql.NullTime
	err := queryer.QueryRowContext(ctx, `
SELECT job_id, slot_id, idempotency_key, desired_generation, step, status,
       retry_count, error_code, next_attempt_at, created_at, updated_at
FROM provisioning_jobs WHERE idempotency_key = ?`, idempotencyKey).Scan(
		&job.ID, &job.SlotID, &job.IdempotencyKey, &job.DesiredGeneration, &job.Step, &job.Status,
		&job.RetryCount, &job.ErrorCode, &next, &job.CreatedAt, &job.UpdatedAt,
	)
	if err != nil {
		return ProvisioningJob{}, fmt.Errorf("get provisioning job: %w", err)
	}
	if next.Valid {
		value := next.Time.UTC()
		job.NextAttemptAt = &value
	}
	return job, nil
}

func validateJobCandidate(job ProvisioningJob, now time.Time, claimTTL time.Duration) error {
	if job.ID == "" || job.SlotID == "" || job.IdempotencyKey == "" || len(job.IdempotencyKey) > 255 ||
		job.DesiredGeneration == 0 || job.Step == "" || len(job.Step) > 64 || now.IsZero() || claimTTL <= 0 {
		return errors.New("invalid provisioning job candidate")
	}
	return nil
}

// MemoryJobRepository is a concurrency-safe test implementation of the job
// claim and retry contract.
type MemoryJobRepository struct {
	mu      sync.Mutex
	byKey   map[string]ProvisioningJob
	keyByID map[string]string
}

func NewMemoryJobRepository() *MemoryJobRepository {
	return &MemoryJobRepository{byKey: make(map[string]ProvisioningJob), keyByID: make(map[string]string)}
}

func (r *MemoryRepository) ClaimProvisioningJob(ctx context.Context, candidate ProvisioningJob, now time.Time, claimTTL time.Duration) (ProvisioningJob, bool, error) {
	return r.jobs.ClaimProvisioningJob(ctx, candidate, now, claimTTL)
}

func (r *MemoryRepository) MarkProvisioningJobDispatched(ctx context.Context, jobID string, dispatchedAt time.Time) error {
	return r.jobs.MarkProvisioningJobDispatched(ctx, jobID, dispatchedAt)
}

func (r *MemoryRepository) CompleteProvisioningJob(ctx context.Context, jobID string, completedAt time.Time) error {
	return r.jobs.CompleteProvisioningJob(ctx, jobID, completedAt)
}

func (r *MemoryRepository) FailProvisioningJob(ctx context.Context, jobID, errorCode string, failedAt, retryAt time.Time) error {
	return r.jobs.FailProvisioningJob(ctx, jobID, errorCode, failedAt, retryAt)
}

func (r *MemoryJobRepository) ClaimProvisioningJob(_ context.Context, candidate ProvisioningJob, now time.Time, claimTTL time.Duration) (ProvisioningJob, bool, error) {
	if err := validateJobCandidate(candidate, now, claimTTL); err != nil {
		return ProvisioningJob{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	job, exists := r.byKey[candidate.IdempotencyKey]
	if !exists {
		job = candidate
		job.Status = "pending"
		job.CreatedAt = now.UTC()
		job.UpdatedAt = now.UTC()
		r.keyByID[job.ID] = job.IdempotencyKey
	}
	claimable := job.Status == "pending" ||
		((job.Status == "failed" || job.Status == "running" || job.Status == "dispatched") && job.NextAttemptAt != nil && !job.NextAttemptAt.After(now))
	if claimable {
		if job.Status != "pending" {
			job.RetryCount++
		}
		job.Status = "running"
		job.ErrorCode = ""
		next := now.Add(claimTTL).UTC()
		job.NextAttemptAt = &next
		job.UpdatedAt = now.UTC()
	}
	r.byKey[job.IdempotencyKey] = job
	return cloneJob(job), claimable, nil
}

func (r *MemoryJobRepository) MarkProvisioningJobDispatched(_ context.Context, jobID string, dispatchedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, exists := r.keyByID[jobID]
	if !exists {
		return ErrJobNotClaimed
	}
	job := r.byKey[key]
	if job.Status != "running" {
		return ErrJobNotClaimed
	}
	job.Status = "dispatched"
	job.UpdatedAt = dispatchedAt.UTC()
	r.byKey[key] = job
	return nil
}

func (r *MemoryJobRepository) CompleteProvisioningJob(_ context.Context, jobID string, completedAt time.Time) error {
	return r.transition(jobID, "completed", "", completedAt, time.Time{}, "running", "dispatched")
}

func (r *MemoryJobRepository) FailProvisioningJob(_ context.Context, jobID, errorCode string, failedAt, retryAt time.Time) error {
	if errorCode == "" || len(errorCode) > 64 || !retryAt.After(failedAt) {
		return errors.New("invalid provisioning job failure")
	}
	return r.transition(jobID, "failed", errorCode, failedAt, retryAt, "running", "dispatched")
}

func (r *MemoryJobRepository) transition(jobID, targetStatus, errorCode string, updatedAt, nextAttemptAt time.Time, allowed ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key, exists := r.keyByID[jobID]
	if !exists {
		return ErrJobNotClaimed
	}
	job := r.byKey[key]
	allowedCurrent := false
	for _, status := range allowed {
		allowedCurrent = allowedCurrent || job.Status == status
	}
	if !allowedCurrent {
		return ErrJobNotClaimed
	}
	job.Status = targetStatus
	job.ErrorCode = errorCode
	job.UpdatedAt = updatedAt.UTC()
	job.NextAttemptAt = nil
	if !nextAttemptAt.IsZero() {
		next := nextAttemptAt.UTC()
		job.NextAttemptAt = &next
	}
	r.byKey[key] = job
	return nil
}

func cloneJob(job ProvisioningJob) ProvisioningJob {
	if job.NextAttemptAt != nil {
		value := *job.NextAttemptAt
		job.NextAttemptAt = &value
	}
	return job
}

var _ JobRepository = (*Repository)(nil)
var _ JobRepository = (*MemoryJobRepository)(nil)
var _ JobRepository = (*MemoryRepository)(nil)
