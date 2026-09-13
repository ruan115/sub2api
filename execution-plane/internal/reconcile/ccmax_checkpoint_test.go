package reconcile

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEnsureCCMAXRuntimeConsumerCheckpointCreatesOnlyOnEmptyOutbox(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT last_sequence, claimed_sequence.*failure_state.*runtime_outbox_consumers`).
		WithArgs("execution-runtime-v1").WillReturnRows(ccmaxCheckpointRows())
	mock.ExpectQuery(regexp.QuoteMeta(`
SELECT sequence
FROM runtime_outbox
ORDER BY sequence DESC
LIMIT 1
FOR UPDATE`)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}))
	mock.ExpectExec(regexp.QuoteMeta(`
INSERT IGNORE INTO runtime_outbox_consumers (consumer_name)
VALUES (?)`)).WithArgs("execution-runtime-v1").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := EnsureCCMAXRuntimeConsumerCheckpoint(context.Background(), db, "execution-runtime-v1"); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCCMAXRuntimeConsumerCheckpointRejectsUnbootstrappedHistory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT last_sequence, claimed_sequence.*runtime_outbox_consumers`).
		WithArgs("execution-runtime-v1").WillReturnRows(
		ccmaxCheckpointRows(),
	)
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*ORDER BY sequence DESC`).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(91))
	mock.ExpectRollback()
	if err := EnsureCCMAXRuntimeConsumerCheckpoint(context.Background(), db, "execution-runtime-v1"); !errors.Is(err, ErrCCMAXRuntimeCheckpointBootstrapRequired) {
		t.Fatalf("checkpoint error = %v", err)
	}
}

func TestEnsureCCMAXRuntimeConsumerCheckpointAcceptsConsistentExistingFence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT last_sequence, claimed_sequence.*runtime_outbox_consumers`).
		WithArgs("execution-runtime-v1").WillReturnRows(ccmaxCheckpointRows().AddRow(
		90, 91, "orchestrator-srv74-1", int64(2_000_000_000_000),
		"ready", 0, "", "", 0, 0, 0,
	))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*ORDER BY sequence DESC`).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(91))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*WHERE sequence = \?.*FOR UPDATE`).
		WithArgs(int64(90)).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(90))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*WHERE sequence > \?.*ORDER BY sequence`).
		WithArgs(int64(90)).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(91))
	mock.ExpectCommit()
	if err := EnsureCCMAXRuntimeConsumerCheckpoint(context.Background(), db, "execution-runtime-v1"); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCCMAXRuntimeConsumerCheckpointRejectsImpossibleFence(t *testing.T) {
	if validCCMAXRuntimeCheckpoint(10, 12, "owner", 1, 11, true, 11, true, true) ||
		validCCMAXRuntimeCheckpoint(10, 100, "owner", 1, 11, true, 11, true, true) ||
		validCCMAXRuntimeCheckpoint(10, 0, "owner", 1, 11, true, 11, true, true) ||
		validCCMAXRuntimeCheckpoint(100, 0, "", 0, 0, false, 91, true, false) ||
		validCCMAXRuntimeCheckpoint(90, 0, "", 0, 91, true, 91, true, false) ||
		validCCMAXRuntimeCheckpoint(1, 0, "", 0, 0, false, 0, false, false) ||
		!validCCMAXRuntimeCheckpoint(0, 0, "", 0, 0, false, 0, false, true) {
		t.Fatal("checkpoint fence validation accepted an impossible state")
	}
	if err := EnsureCCMAXRuntimeConsumerCheckpoint(context.Background(), nil, "execution-runtime-v1"); !errors.Is(err, ErrCCMAXRuntimeCheckpointInvalid) {
		t.Fatalf("nil database error = %v", err)
	}
}

func TestEnsureCCMAXRuntimeConsumerCheckpointRejectsWatermarkBeyondNewestEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT last_sequence, claimed_sequence.*runtime_outbox_consumers`).
		WithArgs("execution-runtime-v1").WillReturnRows(ccmaxCheckpointRows().AddRow(
		100, 0, "", int64(0), "ready", 0, "", "", 0, 0, 0,
	))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*ORDER BY sequence DESC`).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(91))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*WHERE sequence = \?.*FOR UPDATE`).
		WithArgs(int64(100)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*WHERE sequence > \?.*ORDER BY sequence`).
		WithArgs(int64(100)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}))
	mock.ExpectRollback()
	if err := EnsureCCMAXRuntimeConsumerCheckpoint(context.Background(), db, "execution-runtime-v1"); !errors.Is(err, ErrCCMAXRuntimeCheckpointInvalid) {
		t.Fatalf("checkpoint error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureCCMAXRuntimeConsumerCheckpointRejectsBlockedStateBeforeRuntimeStarts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT last_sequence, claimed_sequence.*runtime_outbox_consumers`).
		WithArgs("execution-runtime-v1").WillReturnRows(ccmaxCheckpointRows().AddRow(
		90, 0, "", int64(0), "blocked", 91, "integrity", "lifecycle_replay_conflict", 1, 0, 7,
	))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*ORDER BY sequence DESC`).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(91))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*WHERE sequence = \?.*FOR UPDATE`).
		WithArgs(int64(90)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(90))
	mock.ExpectQuery(`(?s)SELECT sequence.*runtime_outbox.*WHERE sequence > \?.*ORDER BY sequence`).
		WithArgs(int64(90)).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(91))
	mock.ExpectRollback()
	if err := EnsureCCMAXRuntimeConsumerCheckpoint(context.Background(), db, "execution-runtime-v1"); !errors.Is(err, ErrCCMAXRuntimeCheckpointBlocked) {
		t.Fatalf("blocked checkpoint error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func ccmaxCheckpointRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"last_sequence", "claimed_sequence", "locked_by", "lease_expires_at",
		"failure_state", "failure_sequence", "failure_class", "failure_code", "failure_count",
		"next_attempt_at", "blocked_claim_version",
	})
}
