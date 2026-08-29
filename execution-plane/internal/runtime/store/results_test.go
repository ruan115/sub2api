package store

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestApplyCommandResultCommitsObservationAndJobAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	result := CommandResult{
		CommandID: "job-1", NodeID: "srv74", Succeeded: true,
		SlotObservationJSON: []byte(`{"slot_id":"slot-1"}`),
		Observation: &AssignmentObservation{
			SlotID: "slot-1", ExecutionEpoch: 7, ProviderRef: "container-1",
			ActualState: "created", Healthy: true, ObservedAt: now,
		},
		ReceivedAt: now, RetryAt: now.Add(5 * time.Second),
	}
	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO node_command_results.*ON DUPLICATE KEY UPDATE`).
		WithArgs(result.CommandID, result.NodeID, true, "", "", result.SlotObservationJSON, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT node_id FROM node_command_results WHERE command_id = \?`).
		WithArgs(result.CommandID).
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}).AddRow(result.NodeID))
	mock.ExpectQuery(`SELECT slot_id FROM provisioning_jobs WHERE job_id = \? FOR UPDATE`).
		WithArgs(result.CommandID).
		WillReturnRows(sqlmock.NewRows([]string{"slot_id"}).AddRow("slot-1"))
	mock.ExpectExec(`(?s)UPDATE slot_assignments SET.*actual_generation.*WHERE slot_id = \? AND node_id = \? AND execution_epoch = \?`).
		WithArgs("container-1", "created", "created", true, "", now, "slot-1", "srv74", uint64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE provisioning_jobs SET status = 'completed'.*next_attempt_at = NULL`).
		WithArgs(now, result.CommandID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := repository.ApplyCommandResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
