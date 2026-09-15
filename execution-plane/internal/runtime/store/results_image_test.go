package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSQLSessionProofRequiresExactLockedAssignmentImageWithoutJob(t *testing.T) {
	for _, withJob := range []bool{false, true} {
		for _, mismatch := range []string{"different image", "trailing space", "uppercase", "assignment missing"} {
			t.Run(mismatch+map[bool]string{false: "/without job", true: "/with job"}[withJob], func(t *testing.T) {
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
				jobQuery := mock.ExpectQuery(`SELECT slot_id FROM provisioning_jobs WHERE job_id = \? FOR UPDATE`).WithArgs(result.CommandID)
				if withJob {
					jobQuery.WillReturnRows(sqlmock.NewRows([]string{"slot_id"}).AddRow(result.Observation.SlotID))
				} else {
					jobQuery.WillReturnError(sql.ErrNoRows)
				}
				imageQuery := expectResultImageLock(mock, result)
				switch mismatch {
				case "different image":
					imageQuery.WillReturnRows(sqlmock.NewRows([]string{"image_digest"}).AddRow("sha256:" + strings.Repeat("b", 64)))
				case "trailing space":
					imageQuery.WillReturnRows(sqlmock.NewRows([]string{"image_digest"}).AddRow(result.ExpectedImageDigest + " "))
				case "uppercase":
					imageQuery.WillReturnRows(sqlmock.NewRows([]string{"image_digest"}).AddRow(strings.ToUpper(result.ExpectedImageDigest)))
				case "assignment missing":
					imageQuery.WillReturnError(sql.ErrNoRows)
				}
				mock.ExpectRollback()
				if err := repository.ApplyCommandResult(context.Background(), result); !errors.Is(err, ErrAssignmentNotFound) {
					t.Fatalf("unmatched image proof = %v", err)
				}
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestSessionProofRejectsMissingOrInvalidExpectedImageBeforeWrites(t *testing.T) {
	for _, expectedImage := range []string{"", "latest", "sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 64) + " "} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		sqlRepository, _ := NewRepository(db)
		memory, result := memorySessionFixture(t)
		before, _ := memory.GetActiveAssignment(context.Background(), "slot-1")
		result.ExpectedImageDigest = expectedImage
		for _, repository := range []interface {
			ApplyCommandResult(context.Context, CommandResult) error
		}{sqlRepository, memory} {
			if err := repository.ApplyCommandResult(context.Background(), result); err == nil {
				t.Fatal("invalid expected image created session proof")
			}
		}
		if _, exists := memory.GetCommandResult(result.CommandID); exists {
			t.Fatal("invalid image partially recorded command")
		}
		after, _ := memory.GetActiveAssignment(context.Background(), "slot-1")
		if !reflect.DeepEqual(before, after) {
			t.Fatal("invalid image partially changed assignment")
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestMemorySessionProofRequiresCurrentAssignmentAndImageEvenWithoutJob(t *testing.T) {
	for _, withJob := range []bool{false, true} {
		for _, mismatch := range []string{"different image", "assignment missing", "old epoch", "wrong node", "released"} {
			t.Run(mismatch+map[bool]string{false: "/without job", true: "/with job"}[withJob], func(t *testing.T) {
				repository, result := memorySessionFixture(t)
				// Preserve a real prior proof: a failed attempt must not refresh
				// it, replace it or partially record the new command/job result.
				seed := result
				seed.CommandID = "previous-inspect"
				if err := repository.ApplyCommandResult(context.Background(), seed); err != nil {
					t.Fatal(err)
				}
				job := ProvisioningJob{ID: result.CommandID, SlotID: "slot-1", IdempotencyKey: "inspect-key", DesiredGeneration: 1, Step: "inspect"}
				var beforeJob ProvisioningJob
				if withJob {
					var err error
					beforeJob, _, err = repository.ClaimProvisioningJob(context.Background(), job, result.ReceivedAt, time.Minute)
					if err != nil {
						t.Fatal(err)
					}
				}
				result.Observation.ProviderRef = "different-runtime"
				result.Observation.ObservedAt = result.Observation.ObservedAt.Add(time.Second)
				repository.mu.Lock()
				switch mismatch {
				case "different image":
					result.ExpectedImageDigest = "sha256:" + strings.Repeat("b", 64)
				case "assignment missing":
					delete(repository.assignments, "slot-1")
				case "old epoch":
					result.Observation.ExecutionEpoch++
				case "wrong node":
					repository.assignments["slot-1"][0].NodeID = "other-node"
				case "released":
					at := result.ReceivedAt
					repository.assignments["slot-1"][0].ReleasedAt = &at
				}
				before := append([]Assignment(nil), repository.assignments["slot-1"]...)
				repository.mu.Unlock()
				if err := repository.ApplyCommandResult(context.Background(), result); !errors.Is(err, ErrAssignmentNotFound) {
					t.Fatalf("unmatched image proof = %v", err)
				}
				if _, exists := repository.GetCommandResult(result.CommandID); exists {
					t.Fatal("unmatched image partially recorded command")
				}
				repository.mu.RLock()
				after := append([]Assignment(nil), repository.assignments["slot-1"]...)
				repository.mu.RUnlock()
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("unmatched image changed assignment: before=%+v after=%+v", before, after)
				}
				if withJob {
					repository.jobs.mu.Lock()
					afterJob := cloneJob(repository.jobs.byKey[job.IdempotencyKey])
					repository.jobs.mu.Unlock()
					if !reflect.DeepEqual(beforeJob, afterJob) {
						t.Fatal("unmatched image partially changed job")
					}
				}
			})
		}
	}
}

func TestUnprovenResultsStillClearProofWithoutExpectedImage(t *testing.T) {
	for _, kind := range []string{"legacy", "failed", "unhealthy"} {
		t.Run(kind, func(t *testing.T) {
			repository, result := memorySessionFixture(t)
			if err := repository.ApplyCommandResult(context.Background(), result); err != nil {
				t.Fatal(err)
			}
			result.CommandID, result.ExpectedImageDigest = "unproven-inspect", ""
			switch kind {
			case "legacy":
				result.ControlSessionID = ""
			case "failed":
				result.Succeeded, result.ErrorCode = false, "inspect_failed"
			case "unhealthy":
				result.Observation.Healthy = false
			}
			if err := repository.ApplyCommandResult(context.Background(), result); err != nil {
				t.Fatal(err)
			}
			assignment, _ := repository.GetActiveAssignment(context.Background(), "slot-1")
			if assignment.ObservedControlSessionID != "" {
				t.Fatal("unproven result retained old session proof")
			}
		})
	}
}
