package store

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestClaimProvisioningJobIsTransactionalAndLeased(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	claimUntil := now.Add(30 * time.Second)
	candidate := ProvisioningJob{
		ID: "job-1", SlotID: "slot-1", IdempotencyKey: "slot/1/create",
		DesiredGeneration: 1, Step: "create",
	}

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO provisioning_jobs.*ON DUPLICATE KEY UPDATE`).
		WithArgs(candidate.ID, candidate.SlotID, candidate.IdempotencyKey, candidate.DesiredGeneration, candidate.Step, now, now, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)UPDATE provisioning_jobs SET.*status = 'running'.*next_attempt_at = \?.*status IN \('failed', 'running', 'dispatched'\)`).
		WithArgs(claimUntil, now, candidate.IdempotencyKey, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT job_id.*FROM provisioning_jobs WHERE idempotency_key = \?`).
		WithArgs(candidate.IdempotencyKey).
		WillReturnRows(sqlmock.NewRows([]string{
			"job_id", "slot_id", "idempotency_key", "desired_generation", "step", "status",
			"retry_count", "error_code", "next_attempt_at", "created_at", "updated_at",
		}).AddRow(candidate.ID, candidate.SlotID, candidate.IdempotencyKey, 1, "create", "running", 0, "", claimUntil, now, now))
	mock.ExpectCommit()

	job, claimed, err := repository.ClaimProvisioningJob(context.Background(), candidate, now, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !claimed || job.Status != "running" || job.NextAttemptAt == nil || !job.NextAttemptAt.Equal(claimUntil) {
		t.Fatalf("claimed job = %+v, claimed = %v", job, claimed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkProvisioningJobDispatchedPreservesClaimDeadline(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	mock.ExpectExec(`(?s)UPDATE provisioning_jobs SET status = 'dispatched', updated_at = \?.*status = 'running'`).
		WithArgs(now, "job-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.MarkProvisioningJobDispatched(context.Background(), "job-1", now); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
