package store

import (
	"context"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func sessionAssignmentRows(now time.Time, session driver.Value) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"assignment_id", "slot_id", "node_id", "provider_ref", "execution_epoch", "desired_generation", "image_digest",
		"cpu_request_millis", "memory_request_bytes", "actual_state", "actual_generation", "healthy", "reason_code",
		"assigned_at", "last_observed_at", "released_at", "observed_control_session_id",
	}).AddRow("assignment-1", "slot-1", "srv74", "container-1", 1, 1, "image-digest",
		500, 1<<30, "running", 2, true, "", now, now, nil, session)
}

func TestGetActiveAssignmentScansObservationSessionAndHistoricalNull(t *testing.T) {
	for _, session := range []driver.Value{nil, observedSessionA} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		repository, _ := NewRepository(db)
		now := time.Unix(2_000_000_000, 0).UTC()
		mock.ExpectQuery(`(?s)SELECT assignment_id,.*released_at, observed_control_session_id.*FROM slot_assignments WHERE slot_id = \? AND released_at IS NULL`).
			WithArgs("slot-1").WillReturnRows(sessionAssignmentRows(now, session))
		assignment, err := repository.GetActiveAssignment(context.Background(), "slot-1")
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if session != nil {
			want = session.(string)
		}
		if assignment.ObservedControlSessionID != want || assignment.LastObservedAt == nil || !assignment.LastObservedAt.Equal(now) {
			t.Fatalf("assignment = %+v", assignment)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestSQLLegacyObserveAssignmentExplicitlyClearsSessionProof(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	observation := *sessionResult(now).Observation
	mock.ExpectExec(`(?s)UPDATE slot_assignments SET.*observed_control_session_id = NULL.*WHERE slot_id = \? AND execution_epoch = \? AND released_at IS NULL`).
		WithArgs(observation.ProviderRef, observation.ActualState, observation.ActualState, observation.Healthy,
			observation.ReasonCode, now, observation.SlotID, observation.ExecutionEpoch).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT assignment_id,.*observed_control_session_id.*FROM slot_assignments WHERE slot_id = \? AND released_at IS NULL`).
		WithArgs("slot-1").WillReturnRows(sessionAssignmentRows(now, nil))
	assignment, err := repository.ObserveAssignment(context.Background(), observation)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.ObservedControlSessionID != "" {
		t.Fatalf("unproven observation retained proof: %+v", assignment)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
