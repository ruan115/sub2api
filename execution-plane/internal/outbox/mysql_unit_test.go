package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestStrictMySQLSourceRejectsMissingPreflightCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, err := NewStrictMySQLSource(db)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT last_sequence, claimed_sequence.*runtime_outbox_consumers`).
		WithArgs("execution-runtime-v1").WillReturnRows(sqlmock.NewRows([]string{
		"last_sequence", "claimed_sequence", "locked_by", "lease_expires_at", "claim_version",
		"failure_state", "failure_sequence", "failure_class", "failure_code", "failure_count", "next_attempt_at", "blocked_claim_version",
	}))
	mock.ExpectRollback()
	_, claimed, err := source.Claim(
		context.Background(), "execution-runtime-v1", "orchestrator-srv74-1", time.Now().UTC(), time.Minute,
	)
	if claimed || !errors.Is(err, ErrCheckpointMissing) {
		t.Fatalf("strict claim = claimed=%t error=%v", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLSourceRejectsCorruptRetryCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, _ := NewStrictMySQLSource(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT last_sequence, claimed_sequence.*runtime_outbox_consumers`).
		WithArgs("execution-runtime-v1").WillReturnRows(mysqlCheckpointRows().AddRow(
		1, 0, "", int64(0), uint64(7), "retry_wait", 1, "retryable", "storage_unavailable",
		uint64(1), now.Add(time.Minute).UnixMilli(), uint64(0),
	))
	mock.ExpectRollback()
	if _, claimed, err := source.Claim(
		context.Background(), "execution-runtime-v1", "orchestrator-srv74-1", now, time.Minute,
	); claimed || err == nil {
		t.Fatalf("corrupt retry checkpoint = claimed %t, err %v", claimed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLSourceRetryBudgetExitKeepsCheckpointRecoverable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, _ := NewStrictMySQLSource(db)
	base := time.Unix(2_000_000_000, 0).UTC()
	claim := ClaimedEvent{
		Event: Event{
			Sequence: 2, EventID: "event-2", AccountID: 7, EventType: "runtime",
			DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: base,
		},
		ConsumerName: "execution-runtime-v1", Owner: "orchestrator-srv74-1", ClaimVersion: 7,
		LeaseExpiresAt: base.Add(time.Minute),
	}
	failure := Failure{
		Class: FailureRetryable, Code: "storage_unavailable",
		FailedAt: base.Add(time.Second), RetryAfter: base.Add(2 * time.Second), RetryLimit: 5,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT claimed_sequence, locked_by, lease_expires_at, claim_version, failure_count.*runtime_outbox_consumers`).
		WithArgs(claim.ConsumerName).WillReturnRows(sqlmock.NewRows([]string{
		"claimed_sequence", "locked_by", "lease_expires_at", "claim_version", "failure_count",
	}).AddRow(claim.Sequence, claim.Owner, claim.LeaseExpiresAt.UnixMilli(), claim.ClaimVersion, uint64(4)))
	mock.ExpectExec(`(?s)UPDATE runtime_outbox_consumers SET.*failure_state = \?.*blocked_claim_version = \?.*WHERE consumer_name = \?`).
		WithArgs(
			"retry_wait", claim.Sequence, string(FailureRetryable), failure.Code,
			failure.FailedAt.UnixMilli(), failure.FailedAt.UnixMilli(), failure.RetryAfter.UnixMilli(), uint64(0),
			failure.Code, claim.ConsumerName, claim.Sequence, claim.Owner, claim.ClaimVersion,
		).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err = source.Fail(context.Background(), claim, failure)
	var exhausted *RetryBudgetError
	if !errors.As(err, &exhausted) || exhausted.FailureCount != 5 || exhausted.Sequence != claim.Sequence {
		t.Fatalf("retry budget error = %#v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLSourceBlockedRetryReturnsCanonicalReadyCheckpoint(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	source, _ := NewStrictMySQLSource(db)
	mock.ExpectExec(`(?s)UPDATE runtime_outbox_consumers SET.*failure_state = 'ready'.*failure_sequence = 0.*failure_count = 0.*blocked_claim_version = 0.*WHERE consumer_name = \?.*failure_sequence = \?.*blocked_claim_version = \?`).
		WithArgs("execution-runtime-v1", int64(91), uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := source.retryBlocked(context.Background(), "execution-runtime-v1", 91, 7); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func mysqlCheckpointRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"last_sequence", "claimed_sequence", "locked_by", "lease_expires_at", "claim_version",
		"failure_state", "failure_sequence", "failure_class", "failure_code", "failure_count",
		"next_attempt_at", "blocked_claim_version",
	})
}
