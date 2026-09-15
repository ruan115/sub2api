package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const observedSessionA = "0123456789abcdef0123456789abcdef"
const observedSessionB = "abcdef0123456789abcdef0123456789"

func sessionResult(now time.Time) CommandResult {
	return CommandResult{
		CommandID: "inspect-1", NodeID: "srv74", ControlSessionID: observedSessionA, Succeeded: true,
		ExpectedImageDigest: "sha256:" + strings.Repeat("a", 64),
		SlotObservationJSON: []byte(`{"slot_id":"slot-1"}`),
		Observation: &AssignmentObservation{
			SlotID: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1",
			ActualState: "running", Healthy: true, ObservedAt: now,
		},
		ReceivedAt: now, RetryAt: now.Add(5 * time.Second),
	}
}

func expectSessionLock(mock sqlmock.Sqlmock, result CommandResult, session any) {
	mock.ExpectQuery(`SELECT control_session_id FROM nodes WHERE node_id = \? AND status = 'connected' FOR UPDATE`).
		WithArgs(result.NodeID).
		WillReturnRows(sqlmock.NewRows([]string{"control_session_id"}).AddRow(session))
}

func expectResultRecord(mock sqlmock.Sqlmock, result CommandResult) {
	mock.ExpectExec(`(?s)INSERT INTO node_command_results.*ON DUPLICATE KEY UPDATE`).
		WithArgs(result.CommandID, result.NodeID, result.Succeeded, result.ErrorCode, result.ErrorMessage,
			result.SlotObservationJSON, result.ReceivedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`SELECT node_id FROM node_command_results WHERE command_id = \?`).
		WithArgs(result.CommandID).
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}).AddRow(result.NodeID))
}

func expectResultAssignment(mock sqlmock.Sqlmock, result CommandResult, healthy bool, session string) *sqlmock.ExpectedExec {
	if session != "" {
		expectResultImageLock(mock, result).WillReturnRows(sqlmock.NewRows([]string{"image_digest"}).AddRow(result.ExpectedImageDigest))
	}
	return mock.ExpectExec(`(?s)UPDATE slot_assignments SET.*observed_control_session_id = NULLIF\(\?, ''\).*WHERE slot_id = \? AND node_id = \? AND execution_epoch = \? AND released_at IS NULL`).
		WithArgs(result.Observation.ProviderRef, result.Observation.ActualState, result.Observation.ActualState,
			healthy, result.Observation.ReasonCode, result.Observation.ObservedAt, session,
			result.Observation.SlotID, result.NodeID, result.Observation.ExecutionEpoch)
}

func expectResultImageLock(mock sqlmock.Sqlmock, result CommandResult) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`(?s)SELECT image_digest FROM slot_assignments WHERE slot_id = \? AND node_id = \? AND execution_epoch = \? AND released_at IS NULL FOR UPDATE`).
		WithArgs(result.Observation.SlotID, result.NodeID, result.Observation.ExecutionEpoch)
}

func TestSQLCommandResultLocksSessionBeforeAnyWrites(t *testing.T) {
	for _, test := range []struct {
		name    string
		session any
		absent  bool
	}{
		{name: "replaced", session: observedSessionB},
		{name: "missing proof", session: nil},
		{name: "case differs", session: strings.ToUpper(observedSessionA)},
		{name: "trailing space differs", session: observedSessionA + " "},
		{name: "disconnected or absent node", absent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository, _ := NewRepository(db)
			result := sessionResult(time.Unix(2_000_000_000, 0).UTC())
			mock.ExpectBegin()
			if test.absent {
				mock.ExpectQuery(`SELECT control_session_id FROM nodes WHERE node_id = \? AND status = 'connected' FOR UPDATE`).
					WithArgs(result.NodeID).WillReturnError(sql.ErrNoRows)
			} else {
				expectSessionLock(mock, result, test.session)
			}
			mock.ExpectRollback()
			if err := repository.ApplyCommandResult(context.Background(), result); !errors.Is(err, ErrNodeNotFound) {
				t.Fatalf("session rejection = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLCommandResultProofRequiresSuccessfulHealthyObservation(t *testing.T) {
	for _, test := range []struct {
		name, session, wantSession      string
		succeeded, healthy, wantHealthy bool
	}{
		{name: "current successful healthy", session: observedSessionA, succeeded: true, healthy: true, wantHealthy: true, wantSession: observedSessionA},
		{name: "successful unhealthy clears", session: observedSessionA, succeeded: true},
		{name: "failed claimed healthy clears", session: observedSessionA, healthy: true},
		{name: "legacy healthy clears", succeeded: true, healthy: true, wantHealthy: true},
		{name: "legacy failed clears", healthy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository, _ := NewRepository(db)
			result := sessionResult(time.Unix(2_000_000_000, 0).UTC())
			result.ControlSessionID, result.Succeeded, result.Observation.Healthy = test.session, test.succeeded, test.healthy
			if !result.Succeeded {
				result.ErrorCode = "inspect_failed"
			}
			mock.ExpectBegin()
			if result.ControlSessionID != "" {
				expectSessionLock(mock, result, result.ControlSessionID)
			}
			expectResultRecord(mock, result)
			mock.ExpectQuery(`SELECT slot_id FROM provisioning_jobs WHERE job_id = \? FOR UPDATE`).
				WithArgs(result.CommandID).WillReturnError(sql.ErrNoRows)
			expectResultAssignment(mock, result, test.wantHealthy, test.wantSession).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			if err := repository.ApplyCommandResult(context.Background(), result); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSQLCommandResultRollsBackProofWhenJobUpdateFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	result := sessionResult(time.Unix(2_000_000_000, 0).UTC())
	mock.ExpectBegin()
	expectSessionLock(mock, result, observedSessionA)
	expectResultRecord(mock, result)
	mock.ExpectQuery(`SELECT slot_id FROM provisioning_jobs WHERE job_id = \? FOR UPDATE`).
		WithArgs(result.CommandID).WillReturnRows(sqlmock.NewRows([]string{"slot_id"}).AddRow(result.Observation.SlotID))
	expectResultAssignment(mock, result, true, observedSessionA).WillReturnResult(sqlmock.NewResult(0, 1))
	failure := errors.New("synthetic job write failure")
	mock.ExpectExec(`(?s)UPDATE provisioning_jobs SET status = 'completed'`).
		WithArgs(result.ReceivedAt, result.CommandID).WillReturnError(failure)
	mock.ExpectRollback()
	if err := repository.ApplyCommandResult(context.Background(), result); !errors.Is(err, failure) {
		t.Fatalf("apply = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func memorySessionFixture(t *testing.T) (*MemoryRepository, CommandResult) {
	t.Helper()
	repository, now := connectedMemoryRepository(t)
	if _, err := repository.PutDesiredSlot(context.Background(), desiredSlot("slot-1", "account-1", 1, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ReserveAssignment(context.Background(), AssignmentReservation{
		ID: "assignment-1", SlotID: "slot-1", NodeID: "srv74", ExpectedNodeSessionID: "session-1",
		NodeSeenAfter: now.Add(-45 * time.Second), ReservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	acceptMemorySession(t, repository, observedSessionA, now)
	result := sessionResult(now)
	return repository, result
}

func acceptMemorySession(t *testing.T, repository *MemoryRepository, session string, now time.Time) {
	t.Helper()
	node, err := repository.GetNode(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.AcceptHello(context.Background(), Hello{
		NodeID: node.ID, SessionID: session, ProtocolMajor: 1, Capacity: node.Capacity, ReceivedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCommandObservationProofIsClearedByUnprovenUpdates(t *testing.T) {
	for _, name := range []string{"legacy apply", "legacy observe", "failed healthy", "successful unhealthy"} {
		t.Run(name, func(t *testing.T) {
			repository, result := memorySessionFixture(t)
			if err := repository.ApplyCommandResult(context.Background(), result); err != nil {
				t.Fatal(err)
			}
			proven, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
			if proven.ObservedControlSessionID != observedSessionA || !proven.Healthy {
				t.Fatalf("initial proof = %+v", proven)
			}
			result.CommandID = "inspect-2"
			result.Observation.ObservedAt = result.Observation.ObservedAt.Add(time.Second)
			switch name {
			case "legacy apply":
				result.ControlSessionID = ""
			case "failed healthy":
				result.Succeeded, result.ErrorCode = false, "inspect_failed"
			case "successful unhealthy":
				result.Observation.Healthy = false
			}
			if name == "legacy observe" {
				if _, err := repository.ObserveAssignment(context.Background(), *result.Observation); err != nil {
					t.Fatal(err)
				}
			} else if err := repository.ApplyCommandResult(context.Background(), result); err != nil {
				t.Fatal(err)
			}
			updated, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
			if updated.ObservedControlSessionID != "" {
				t.Fatalf("retained old proof: %+v", updated)
			}
			if (name == "failed healthy" || name == "successful unhealthy") && updated.Healthy {
				t.Fatal("unhealthy result left assignment healthy")
			}
		})
	}
}

func TestMemorySessionReplacementNeedsFreshCommandProof(t *testing.T) {
	repository, result := memorySessionFixture(t)
	if err := repository.ApplyCommandResult(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	before, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
	if err := repository.MarkDisconnected(context.Background(), "srv74", observedSessionA, result.ReceivedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	late := result
	late.CommandID = "late-inspect"
	if err := repository.ApplyCommandResult(context.Background(), late); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("disconnected result = %v", err)
	}
	acceptMemorySession(t, repository, observedSessionB, result.ReceivedAt.Add(2*time.Second))
	if err := repository.RecordHeartbeat(context.Background(), Heartbeat{
		NodeID: "srv74", SessionID: observedSessionB, ReceivedAt: result.ReceivedAt.Add(3 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ApplyCommandResult(context.Background(), late); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("old session result = %v", err)
	}
	unchanged, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
	if !reflect.DeepEqual(before, unchanged) {
		t.Fatalf("hello/heartbeat/late result changed observation: before=%+v after=%+v", before, unchanged)
	}
	if _, exists := repository.GetCommandResult(late.CommandID); exists {
		t.Fatal("late result was partly recorded")
	}
	fresh := sessionResult(result.ReceivedAt.Add(4 * time.Second))
	fresh.CommandID, fresh.ControlSessionID = "fresh-inspect", observedSessionB
	if err := repository.ApplyCommandResult(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	updated, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
	if updated.ObservedControlSessionID != observedSessionB || !updated.LastObservedAt.Equal(fresh.Observation.ObservedAt) {
		t.Fatalf("new proof = %+v", updated)
	}
}

func TestMemoryCommandResultValidationHasNoPartialWrites(t *testing.T) {
	for _, name := range []string{"wrong job slot", "wrong assignment epoch", "wrong assignment node", "invalid JSON", "invalid session", "replaced session"} {
		t.Run(name, func(t *testing.T) {
			repository, result := memorySessionFixture(t)
			job := ProvisioningJob{ID: result.CommandID, SlotID: "slot-1", IdempotencyKey: "inspect-key", DesiredGeneration: 1, Step: "inspect"}
			beforeJob, _, err := repository.ClaimProvisioningJob(context.Background(), job, result.ReceivedAt, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			before, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
			switch name {
			case "wrong job slot":
				result.Observation.SlotID = "other-slot"
			case "wrong assignment epoch":
				result.Observation.ExecutionEpoch++
			case "wrong assignment node":
				repository.mu.Lock()
				node := repository.nodes["srv74"]
				node.ID = "other-node"
				repository.nodes[node.ID] = node
				repository.mu.Unlock()
				result.NodeID = "other-node"
			case "invalid JSON":
				result.SlotObservationJSON = []byte(`{broken`)
			case "invalid session":
				result.ControlSessionID = "not-a-session"
			case "replaced session":
				acceptMemorySession(t, repository, observedSessionB, result.ReceivedAt.Add(time.Second))
			}
			if err := repository.ApplyCommandResult(context.Background(), result); err == nil {
				t.Fatal("invalid result accepted")
			}
			if _, exists := repository.GetCommandResult(result.CommandID); exists {
				t.Fatal("invalid result was partly recorded")
			}
			after, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("assignment mutated: before=%+v after=%+v", before, after)
			}
			repository.jobs.mu.Lock()
			afterJob := cloneJob(repository.jobs.byKey[job.IdempotencyKey])
			repository.jobs.mu.Unlock()
			if !reflect.DeepEqual(beforeJob, afterJob) {
				t.Fatalf("job mutated: before=%+v after=%+v", beforeJob, afterJob)
			}
		})
	}
}

func TestCommandResultRejectsMalformedSessionBeforeSQL(t *testing.T) {
	for _, session := range []string{"short", strings.Repeat("a", 33), strings.Repeat("g", 32), strings.ToUpper(observedSessionA), observedSessionA + " "} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		repository, _ := NewRepository(db)
		result := sessionResult(time.Unix(2_000_000_000, 0).UTC())
		result.ControlSessionID = session
		if err := repository.ApplyCommandResult(context.Background(), result); err == nil {
			t.Fatal("malformed session accepted")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestMemoryCommandResultCommitsJobAndProofTogether(t *testing.T) {
	for _, succeeded := range []bool{true, false} {
		repository, result := memorySessionFixture(t)
		job := ProvisioningJob{ID: result.CommandID, SlotID: "slot-1", IdempotencyKey: "inspect-key", DesiredGeneration: 1, Step: "inspect"}
		if _, _, err := repository.ClaimProvisioningJob(context.Background(), job, result.ReceivedAt, time.Minute); err != nil {
			t.Fatal(err)
		}
		result.Succeeded = succeeded
		if !succeeded {
			result.ErrorCode = "inspect_failed"
		}
		if err := repository.ApplyCommandResult(context.Background(), result); err != nil {
			t.Fatal(err)
		}
		assignment, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
		storedResult, exists := repository.GetCommandResult(result.CommandID)
		repository.jobs.mu.Lock()
		storedJob := cloneJob(repository.jobs.byKey[job.IdempotencyKey])
		repository.jobs.mu.Unlock()
		if !exists || storedResult.Succeeded != succeeded {
			t.Fatalf("recorded result = %+v, exists %v", storedResult, exists)
		}
		if succeeded {
			if assignment.ObservedControlSessionID != observedSessionA || !assignment.Healthy || storedJob.Status != "completed" || storedJob.NextAttemptAt != nil {
				t.Fatalf("success assignment/job = %+v / %+v", assignment, storedJob)
			}
		} else if assignment.ObservedControlSessionID != "" || assignment.Healthy || storedJob.Status != "failed" || storedJob.ErrorCode != result.ErrorCode || storedJob.NextAttemptAt == nil || !storedJob.NextAttemptAt.Equal(result.RetryAt) {
			t.Fatalf("failure assignment/job = %+v / %+v", assignment, storedJob)
		}
	}
}

func TestSQLCommandResultAssignmentMismatchRollsBack(t *testing.T) {
	for _, name := range []string{"job slot mismatch", "assignment missing"} {
		t.Run(name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository, _ := NewRepository(db)
			result := sessionResult(time.Unix(2_000_000_000, 0).UTC())
			mock.ExpectBegin()
			expectSessionLock(mock, result, observedSessionA)
			expectResultRecord(mock, result)
			jobSlot := "slot-1"
			if name == "job slot mismatch" {
				jobSlot = "other-slot"
			}
			mock.ExpectQuery(`SELECT slot_id FROM provisioning_jobs WHERE job_id = \? FOR UPDATE`).
				WithArgs(result.CommandID).WillReturnRows(sqlmock.NewRows([]string{"slot_id"}).AddRow(jobSlot))
			if name == "assignment missing" {
				expectResultAssignment(mock, result, true, observedSessionA).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			mock.ExpectRollback()
			if err := repository.ApplyCommandResult(context.Background(), result); err == nil {
				t.Fatal("mismatched assignment accepted")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The first Err call occurs with the repository lock held. Capture its return
// value before notifying the test, so late cancellation cannot be caught by
// this first check: the regression needs the later jobs-lock check.
type firstErrObservedContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *firstErrObservedContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.observed) })
	return err
}

func TestMemoryCommandResultCancelledWhileWaitingForJobLockHasNoWrites(t *testing.T) {
	repository, result := memorySessionFixture(t)
	job := ProvisioningJob{ID: result.CommandID, SlotID: "slot-1", IdempotencyKey: "inspect-key", DesiredGeneration: 1, Step: "inspect"}
	beforeJob, _, err := repository.ClaimProvisioningJob(context.Background(), job, result.ReceivedAt, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &firstErrObservedContext{Context: parent, observed: make(chan struct{})}
	finished := make(chan error, 1)
	repository.jobs.mu.Lock()
	locked := true
	defer func() {
		if locked {
			repository.jobs.mu.Unlock()
		}
	}()
	go func() { finished <- repository.ApplyCommandResult(ctx, result) }()
	select {
	case <-ctx.observed:
	case <-time.After(time.Second):
		t.Fatal("Apply did not reach its pre-jobs-lock context check")
	}
	cancel()
	repository.jobs.mu.Unlock()
	locked = false
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled apply = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Apply did not finish after the job lock was released")
	}
	if _, exists := repository.GetCommandResult(result.CommandID); exists {
		t.Fatal("cancelled command result was recorded")
	}
	after, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("cancelled apply changed assignment: before=%+v after=%+v", before, after)
	}
	repository.jobs.mu.Lock()
	afterJob := cloneJob(repository.jobs.byKey[job.IdempotencyKey])
	repository.jobs.mu.Unlock()
	if !reflect.DeepEqual(beforeJob, afterJob) {
		t.Fatalf("cancelled apply changed job: before=%+v after=%+v", beforeJob, afterJob)
	}
}
