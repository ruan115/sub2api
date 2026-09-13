package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMySQLLifecycleEventApplyAtomicallyWritesSlotAndReceipt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	apply := lifecycleApplyFixture()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ? FOR UPDATE")).
		WithArgs(apply.Anchor.EventID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ? FOR UPDATE")).
		WithArgs(apply.Anchor.SourceSequence).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM slots WHERE slot_id = ? FOR UPDATE")).
		WithArgs(apply.Slot.ID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO slots (")).
		WithArgs(
			apply.Slot.ID, apply.Slot.AccountID, apply.Slot.Provider, apply.Slot.DesiredState,
			apply.Slot.DesiredGeneration, []byte(`{"failure_domain":"srv74"}`), apply.Slot.ImageDigest,
			apply.Slot.CPURequestMillis, apply.Slot.MemoryRequestBytes,
			normalizeLifecycleTime(apply.Slot.CreatedAt), normalizeLifecycleTime(apply.Slot.UpdatedAt),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO lifecycle_event_apply_receipts (")).
		WithArgs(
			apply.Anchor.SourceSequence, apply.Anchor.EventID, apply.Anchor.AccountID, apply.Anchor.EventType,
			apply.Anchor.DesiredGeneration, apply.Anchor.PayloadSHA256[:], normalizeLifecycleTime(apply.Anchor.EventCreatedAt),
			apply.Slot.ID, apply.Slot.AccountID, apply.Slot.Provider, apply.Slot.DesiredState,
			[]byte(`{"failure_domain":"srv74"}`), apply.Slot.ImageDigest, apply.Slot.CPURequestMillis,
			apply.Slot.MemoryRequestBytes, normalizeLifecycleTime(apply.AppliedAt),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	receipt, created, err := repository.ApplyLifecycleEvent(context.Background(), apply)
	if err != nil || !created {
		t.Fatalf("apply lifecycle event: created=%v receipt=%+v err=%v", created, receipt, err)
	}
	if !sameLifecycleEventAnchor(receipt.Anchor, apply.Anchor) || !sameDesiredSlot(receipt.Slot, apply.Slot) {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLifecycleEventApplyReplaysReceiptWithoutSlotMutation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	apply := lifecycleApplyFixture()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ? FOR UPDATE")).
		WithArgs(apply.Anchor.EventID).
		WillReturnRows(lifecycleReceiptRows(apply))
	mock.ExpectCommit()
	receipt, created, err := repository.ApplyLifecycleEvent(context.Background(), apply)
	if err != nil || created || receipt.Slot.ImageDigest != apply.Slot.ImageDigest {
		t.Fatalf("exact apply replay: created=%v receipt=%+v err=%v", created, receipt, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLifecycleEventApplyReplaysConcurrentCommitFoundBySequence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	apply := lifecycleApplyFixture()

	mock.ExpectBegin()
	// Models an exact writer committing after this READ COMMITTED transaction's
	// event-id locking read, but before its sequence locking read.
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ? FOR UPDATE")).
		WithArgs(apply.Anchor.EventID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ? FOR UPDATE")).
		WithArgs(apply.Anchor.SourceSequence).
		WillReturnRows(lifecycleReceiptRows(apply))
	mock.ExpectCommit()

	receipt, created, err := repository.ApplyLifecycleEvent(context.Background(), apply)
	if err != nil || created || !sameLifecycleEventAnchor(receipt.Anchor, apply.Anchor) {
		t.Fatalf("concurrent exact apply replay: created=%v receipt=%+v err=%v", created, receipt, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLifecycleEventApplyConcurrentSequenceRejectsDifferentImmutableAnchor(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	stored := lifecycleApplyFixture()
	apply := stored
	apply.Anchor.PayloadSHA256 = sha256.Sum256([]byte("different"))

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ? FOR UPDATE")).
		WithArgs(apply.Anchor.EventID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ? FOR UPDATE")).
		WithArgs(apply.Anchor.SourceSequence).
		WillReturnRows(lifecycleReceiptRows(stored))
	mock.ExpectRollback()

	if _, _, err := repository.ApplyLifecycleEvent(context.Background(), apply); !errors.Is(err, ErrLifecycleEventConflict) {
		t.Fatalf("concurrent sequence conflict error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLifecycleEventApplyRollsBackSlotWhenReceiptWriteFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	apply := lifecycleApplyFixture()
	writeErr := errors.New("receipt storage unavailable")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ? FOR UPDATE")).
		WithArgs(apply.Anchor.EventID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ? FOR UPDATE")).
		WithArgs(apply.Anchor.SourceSequence).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM slots WHERE slot_id = ? FOR UPDATE")).
		WithArgs(apply.Slot.ID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO slots (")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO lifecycle_event_apply_receipts (")).
		WillReturnError(writeErr)
	mock.ExpectRollback()
	// After releasing the write transaction the repository checks whether a
	// concurrent exact writer won. No receipt means the atomic apply failed.
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ?")).
		WithArgs(apply.Anchor.EventID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ?")).
		WithArgs(apply.Anchor.SourceSequence).
		WillReturnError(sql.ErrNoRows)

	if _, _, err := repository.ApplyLifecycleEvent(context.Background(), apply); !errors.Is(err, writeErr) {
		t.Fatalf("receipt write error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLifecycleEventApplyResolvesAmbiguousCommitFromReceipt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	apply := lifecycleApplyFixture()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ? FOR UPDATE")).
		WithArgs(apply.Anchor.EventID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ? FOR UPDATE")).
		WithArgs(apply.Anchor.SourceSequence).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM slots WHERE slot_id = ? FOR UPDATE")).
		WithArgs(apply.Slot.ID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO slots (")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO lifecycle_event_apply_receipts (")).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit response lost"))
	// The server may have committed even though the client did not receive the
	// response. An exact durable receipt resolves that ambiguity as success.
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ?")).
		WithArgs(apply.Anchor.EventID).
		WillReturnRows(lifecycleReceiptRows(apply))

	receipt, created, err := repository.ApplyLifecycleEvent(context.Background(), apply)
	if err != nil || created || !sameLifecycleEventAnchor(receipt.Anchor, apply.Anchor) {
		t.Fatalf("ambiguous commit recovery: created=%v receipt=%+v err=%v", created, receipt, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLifecycleEventLookupFailsClosedOnAnchorOrFrozenPolicyConflict(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*LifecycleEventApply)
		row    func(LifecycleEventApply) *sqlmock.Rows
	}{
		{
			name: "payload anchor changed",
			mutate: func(apply *LifecycleEventApply) {
				apply.Anchor.PayloadSHA256 = sha256.Sum256([]byte(`{"changed":true}`))
			},
			row: lifecycleReceiptRows,
		},
		{
			name: "receipt policy corrupted",
			row: func(apply LifecycleEventApply) *sqlmock.Rows {
				return lifecycleReceiptRowsWithAccount(apply, "different-account")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository, _ := NewRepository(db)
			stored := lifecycleApplyFixture()
			lookup := stored
			if test.mutate != nil {
				test.mutate(&lookup)
			}
			mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ?")).
				WithArgs(lookup.Anchor.EventID).
				WillReturnRows(test.row(stored))
			if _, _, err := repository.LookupLifecycleEventApply(context.Background(), lookup.Anchor); !errors.Is(err, ErrLifecycleEventConflict) {
				t.Fatalf("lookup conflict error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLLifecycleEventLookupChecksSequenceBeforeMutableSource(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	apply := lifecycleApplyFixture()
	conflict := apply
	conflict.Anchor.EventID = "different-event"

	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ?")).
		WithArgs(conflict.Anchor.EventID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ?")).
		WithArgs(conflict.Anchor.SourceSequence).
		WillReturnRows(lifecycleReceiptRows(apply))
	if _, _, err := repository.LookupLifecycleEventApply(context.Background(), conflict.Anchor); !errors.Is(err, ErrLifecycleEventConflict) {
		t.Fatalf("sequence conflict error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLifecycleEventLookupReplaysConcurrentCommitFoundBySequence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	apply := lifecycleApplyFixture()

	// Models a commit between the two standalone READ COMMITTED lookups.
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ?")).
		WithArgs(apply.Anchor.EventID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ?")).
		WithArgs(apply.Anchor.SourceSequence).
		WillReturnRows(lifecycleReceiptRows(apply))

	receipt, found, err := repository.LookupLifecycleEventApply(context.Background(), apply.Anchor)
	if err != nil || !found || !sameLifecycleEventAnchor(receipt.Anchor, apply.Anchor) {
		t.Fatalf("concurrent exact lookup replay: found=%v receipt=%+v err=%v", found, receipt, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLLifecycleEventLookupConcurrentSequenceRejectsDifferentImmutableAnchor(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*LifecycleEventAnchor)
	}{
		{name: "event id", mutate: func(anchor *LifecycleEventAnchor) { anchor.EventID = "different-event" }},
		{name: "account", mutate: func(anchor *LifecycleEventAnchor) { anchor.AccountID++ }},
		{name: "event type", mutate: func(anchor *LifecycleEventAnchor) { anchor.EventType = "account.runtime.drain_requested" }},
		{name: "generation", mutate: func(anchor *LifecycleEventAnchor) { anchor.DesiredGeneration++ }},
		{name: "payload", mutate: func(anchor *LifecycleEventAnchor) { anchor.PayloadSHA256 = sha256.Sum256([]byte("different")) }},
		{name: "created at", mutate: func(anchor *LifecycleEventAnchor) {
			anchor.EventCreatedAt = anchor.EventCreatedAt.Add(time.Microsecond)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository, _ := NewRepository(db)
			stored := lifecycleApplyFixture()
			lookup := stored.Anchor
			test.mutate(&lookup)

			mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE event_id = ?")).
				WithArgs(lookup.EventID).
				WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery(regexp.QuoteMeta("FROM lifecycle_event_apply_receipts WHERE source_sequence = ?")).
				WithArgs(lookup.SourceSequence).
				WillReturnRows(lifecycleReceiptRows(stored))

			if _, _, err := repository.LookupLifecycleEventApply(context.Background(), lookup); !errors.Is(err, ErrLifecycleEventConflict) {
				t.Fatalf("concurrent sequence conflict error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func lifecycleApplyFixture() LifecycleEventApply {
	now := time.Unix(2_000_000_000, 123_456_000).UTC()
	payloadDigest := sha256.Sum256([]byte(`{"reason":"restore"}`))
	return LifecycleEventApply{
		Anchor: LifecycleEventAnchor{
			SourceSequence: 41, EventID: "event-restore-41", AccountID: 7,
			EventType: "account.runtime.restore_requested", DesiredGeneration: 3,
			PayloadSHA256: payloadDigest, EventCreatedAt: now.Add(-time.Second),
		},
		Slot: Slot{
			ID: "ccmax-account-7", AccountID: "7", Provider: "docker", DesiredState: "ready",
			DesiredGeneration: 3, RequiredLabels: map[string]string{"failure_domain": "srv74"},
			ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500,
			MemoryRequestBytes: 256 << 20, CreatedAt: now, UpdatedAt: now,
		},
		AppliedAt: now,
	}
}

func lifecycleReceiptRows(apply LifecycleEventApply) *sqlmock.Rows {
	return lifecycleReceiptRowsWithAccount(apply, apply.Slot.AccountID)
}

func lifecycleReceiptRowsWithAccount(apply LifecycleEventApply, slotAccountID string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"source_sequence", "event_id", "event_account_id", "event_type", "desired_generation",
		"event_payload_sha256", "event_created_at", "slot_id", "slot_account_id", "provider",
		"desired_state", "required_labels_json", "image_digest", "cpu_request_millis",
		"memory_request_bytes", "applied_at",
	}).AddRow(
		apply.Anchor.SourceSequence, apply.Anchor.EventID, apply.Anchor.AccountID, apply.Anchor.EventType,
		apply.Anchor.DesiredGeneration, apply.Anchor.PayloadSHA256[:], normalizeLifecycleTime(apply.Anchor.EventCreatedAt),
		apply.Slot.ID, slotAccountID, apply.Slot.Provider, apply.Slot.DesiredState,
		[]byte(`{"failure_domain":"srv74"}`), apply.Slot.ImageDigest, apply.Slot.CPURequestMillis,
		apply.Slot.MemoryRequestBytes, normalizeLifecycleTime(apply.AppliedAt),
	)
}
