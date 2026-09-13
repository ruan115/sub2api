package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRuntimeOutboxCommitOrderSchemaStaysInSyncAcrossDialects(t *testing.T) {
	for dialect, statements := range map[string][]string{
		"sqlite": sqliteExecutionSchema(),
		"mysql":  mysqlExecutionSchema(),
	} {
		schema := strings.Join(statements, "\n")
		for _, fragment := range []string{
			"CREATE TABLE IF NOT EXISTS runtime_outbox_commit_lock",
			"singleton",
			"lock_epoch",
			"runtime_outbox_commit_lock (singleton, lock_epoch) VALUES (1, 0)",
		} {
			if !strings.Contains(schema, fragment) {
				t.Fatalf("%s execution schema is missing %q", dialect, fragment)
			}
		}
	}
}

func TestMigrateExecutionFeaturesRepairsMissingRuntimeOutboxCommitLockRow(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-outbox-commit-lock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	if _, err := a.db.Exec(`DELETE FROM runtime_outbox_commit_lock WHERE singleton = 1`); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateExecutionFeatures(); err != nil {
		t.Fatal(err)
	}
	var singleton, epoch int64
	if err := a.db.QueryRow(`SELECT singleton, lock_epoch FROM runtime_outbox_commit_lock`).Scan(&singleton, &epoch); err != nil {
		t.Fatal(err)
	}
	if singleton != 1 || epoch != 0 {
		t.Fatalf("repaired commit-order lock = %d/%d, want 1/0", singleton, epoch)
	}
}

func TestEnqueueRuntimeEventTakesCommitOrderLockBeforeInsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	rawTx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	tx := &databaseTx{Tx: rawTx, dialect: dialectMySQL}
	eventID := "11111111-2222-4333-8444-555555555555"
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT runtime_generation FROM accounts WHERE id = ?`)).
		WithArgs(int64(91)).
		WillReturnRows(sqlmock.NewRows([]string{"runtime_generation"}).AddRow(uint64(7)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE runtime_outbox_commit_lock
		SET lock_epoch = lock_epoch + 1 WHERE singleton = 1`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO runtime_outbox
		(event_id, account_id, event_type, desired_generation, payload_json)
		VALUES (?, ?, ?, ?, ?)`)).
		WithArgs(eventID, int64(91), "account.runtime.provision_requested", uint64(7), `{}`).
		WillReturnResult(sqlmock.NewResult(42, 1))
	mock.ExpectRollback()

	got, err := enqueueRuntimeEventTx(context.Background(), tx, runtimeOutboxEvent{
		EventID: eventID, AccountID: 91, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 7, PayloadJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sequence != 42 {
		t.Fatalf("outbox sequence = %d, want 42", got.Sequence)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRuntimeOutboxCommitOrderFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		result driver.Result
		err    error
	}{
		{name: "missing singleton", result: sqlmock.NewResult(0, 0)},
		{name: "database error", err: errors.New("write failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			rawTx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			tx := &databaseTx{Tx: rawTx, dialect: dialectMySQL}
			expectation := mock.ExpectExec(regexp.QuoteMeta(`UPDATE runtime_outbox_commit_lock
		SET lock_epoch = lock_epoch + 1 WHERE singleton = 1`))
			if test.err != nil {
				expectation.WillReturnError(test.err)
			} else {
				expectation.WillReturnResult(test.result)
			}
			mock.ExpectRollback()

			if err := acquireRuntimeOutboxCommitOrderTx(context.Background(), tx); err == nil {
				t.Fatal("commit-order lock failure was accepted")
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLRuntimeOutboxCommitOrderAcrossConnections(t *testing.T) {
	dsn := os.Getenv("CCMAX_EXECUTION_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set CCMAX_EXECUTION_MYSQL_TEST_DSN to run the CCMAX MySQL commit-order integration")
	}
	t.Setenv("CCMAX_MYSQL_DSN", dsn)
	a, err := newApp("")
	if err != nil {
		t.Fatal(err)
	}
	a.db.SetMaxOpenConns(4)
	a.db.SetMaxIdleConns(4)
	t.Cleanup(func() { _ = a.db.Close() })

	suffix := newRuntimeEventID()
	accountIDs := make([]int64, 0, 2)
	for _, name := range []string{"commit-order-first-" + suffix, "commit-order-second-" + suffix} {
		result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES (?, '{}')`, name)
		if err != nil {
			t.Fatal(err)
		}
		accountID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		accountIDs = append(accountIDs, accountID)
	}
	t.Cleanup(func() {
		_, _ = a.db.Exec(`DELETE FROM runtime_outbox WHERE account_id IN (?, ?)`, accountIDs[0], accountIDs[1])
		_, _ = a.db.Exec(`DELETE FROM accounts WHERE id IN (?, ?)`, accountIDs[0], accountIDs[1])
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstTx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer firstTx.Rollback()
	if _, err := firstTx.ExecContext(ctx, `UPDATE accounts SET runtime_generation = 1,
		runtime_status = 'provisioning' WHERE id = ?`, accountIDs[0]); err != nil {
		t.Fatal(err)
	}
	first, err := enqueueRuntimeEventTx(ctx, firstTx, runtimeOutboxEvent{
		EventID: newRuntimeEventID(), AccountID: accountIDs[0],
		EventType: "account.runtime.provision_requested", DesiredGeneration: 1, PayloadJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	type producerResult struct {
		event runtimeOutboxEvent
		err   error
	}
	secondAtLock := make(chan struct{})
	secondDone := make(chan producerResult, 1)
	go func() {
		secondTx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			secondDone <- producerResult{err: err}
			return
		}
		defer secondTx.Rollback()
		if _, err := secondTx.ExecContext(ctx, `UPDATE accounts SET runtime_generation = 1,
			runtime_status = 'provisioning' WHERE id = ?`, accountIDs[1]); err != nil {
			secondDone <- producerResult{err: err}
			return
		}
		close(secondAtLock)
		event, err := enqueueRuntimeEventTx(ctx, secondTx, runtimeOutboxEvent{
			EventID: newRuntimeEventID(), AccountID: accountIDs[1],
			EventType: "account.runtime.provision_requested", DesiredGeneration: 1, PayloadJSON: `{}`,
		})
		if err == nil {
			err = secondTx.Commit()
		}
		secondDone <- producerResult{event: event, err: err}
	}()

	select {
	case <-secondAtLock:
	case result := <-secondDone:
		t.Fatalf("second producer failed before reaching commit-order lock: %v", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case result := <-secondDone:
		_ = firstTx.Rollback()
		t.Fatalf("second producer passed the commit-order lock before first transaction completed: %v", result.err)
	case <-time.After(250 * time.Millisecond):
	}

	if err := firstTx.Commit(); err != nil {
		t.Fatal(err)
	}
	var second producerResult
	select {
	case second = <-secondDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	if first.Sequence >= second.event.Sequence {
		t.Fatalf("commit-ordered sequences = %d then %d", first.Sequence, second.event.Sequence)
	}

	var committedEvents int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id IN (?, ?)`,
		accountIDs[0], accountIDs[1]).Scan(&committedEvents); err != nil {
		t.Fatal(err)
	}
	if committedEvents != 2 {
		t.Fatalf("committed event count = %d, want 2", committedEvents)
	}
}

func TestMySQLRuntimeOutboxCommitOrderRollbackLeavesClaimableGap(t *testing.T) {
	dsn := os.Getenv("CCMAX_EXECUTION_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set CCMAX_EXECUTION_MYSQL_TEST_DSN to run the CCMAX MySQL commit-order rollback integration")
	}
	t.Setenv("CCMAX_MYSQL_DSN", dsn)
	a, err := newApp("")
	if err != nil {
		t.Fatal(err)
	}
	a.db.SetMaxOpenConns(4)
	a.db.SetMaxIdleConns(4)
	t.Cleanup(func() { _ = a.db.Close() })

	suffix := newRuntimeEventID()
	accountIDs := make([]int64, 0, 2)
	for _, name := range []string{"commit-rollback-first-" + suffix, "commit-rollback-second-" + suffix} {
		result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES (?, '{}')`, name)
		if err != nil {
			t.Fatal(err)
		}
		accountID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		accountIDs = append(accountIDs, accountID)
	}
	consumerName := "commit-rollback-consumer-" + suffix
	t.Cleanup(func() {
		_, _ = a.db.Exec(`DELETE FROM runtime_outbox_consumers WHERE consumer_name = ?`, consumerName)
		_, _ = a.db.Exec(`DELETE FROM runtime_outbox WHERE account_id IN (?, ?)`, accountIDs[0], accountIDs[1])
		_, _ = a.db.Exec(`DELETE FROM accounts WHERE id IN (?, ?)`, accountIDs[0], accountIDs[1])
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	firstTx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer firstTx.Rollback()
	if _, err := firstTx.ExecContext(ctx, `UPDATE accounts SET runtime_generation = 1,
		runtime_status = 'provisioning' WHERE id = ?`, accountIDs[0]); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := enqueueRuntimeEventTx(ctx, firstTx, runtimeOutboxEvent{
		EventID: newRuntimeEventID(), AccountID: accountIDs[0],
		EventType: "account.runtime.provision_requested", DesiredGeneration: 1, PayloadJSON: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}

	type producerResult struct {
		event runtimeOutboxEvent
		err   error
	}
	secondAtLock := make(chan struct{})
	secondDone := make(chan producerResult, 1)
	go func() {
		secondTx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			secondDone <- producerResult{err: err}
			return
		}
		defer secondTx.Rollback()
		if _, err := secondTx.ExecContext(ctx, `UPDATE accounts SET runtime_generation = 1,
			runtime_status = 'provisioning' WHERE id = ?`, accountIDs[1]); err != nil {
			secondDone <- producerResult{err: err}
			return
		}
		close(secondAtLock)
		event, err := enqueueRuntimeEventTx(ctx, secondTx, runtimeOutboxEvent{
			EventID: newRuntimeEventID(), AccountID: accountIDs[1],
			EventType: "account.runtime.provision_requested", DesiredGeneration: 1, PayloadJSON: `{}`,
		})
		if err == nil {
			err = secondTx.Commit()
		}
		secondDone <- producerResult{event: event, err: err}
	}()

	select {
	case <-secondAtLock:
	case result := <-secondDone:
		t.Fatalf("second producer failed before reaching commit-order lock: %v", result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case result := <-secondDone:
		_ = firstTx.Rollback()
		t.Fatalf("second producer passed the commit-order lock before first rollback: %v", result.err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := firstTx.Rollback(); err != nil {
		t.Fatal(err)
	}

	var second producerResult
	select {
	case second = <-secondDone:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if second.err != nil {
		t.Fatal(second.err)
	}
	if second.event.Sequence != rolledBack.Sequence+1 {
		t.Fatalf("rollback gap = allocated %d then committed %d, want N+1", rolledBack.Sequence, second.event.Sequence)
	}
	var rolledBackRows int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE event_id = ?`,
		rolledBack.EventID).Scan(&rolledBackRows); err != nil {
		t.Fatal(err)
	}
	if rolledBackRows != 0 {
		t.Fatalf("rolled-back outbox event persisted: count=%d", rolledBackRows)
	}

	if _, err := a.db.Exec(`INSERT INTO runtime_outbox_consumers (consumer_name, last_sequence)
		VALUES (?, ?)`, consumerName, rolledBack.Sequence-1); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	claimed, ok, err := a.claimRuntimeOutboxEvent(ctx, consumerName, "rollback-gap-owner", now, time.Minute)
	if err != nil || !ok || claimed.EventID != second.event.EventID {
		t.Fatalf("claim after rollback gap: event=%+v ok=%v err=%v", claimed, ok, err)
	}
}
