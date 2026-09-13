package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestDuplicateIdentityBatchArchivesAfterDrain(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "duplicate-batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	existing, err := a.db.Exec(`INSERT INTO accounts (name, platform, auth_type, credentials_json) VALUES ('owner@example.com', 'anthropic', 'oauth', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	existingID, _ := existing.LastInsertId()
	accountID, intentID, slotID := insertPendingRuntimeOnboarding(t, a, "pending-duplicate-batch", "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff")
	reader := &fakeRuntimeOnboardingResultReader{result: runtimeOnboardingResult{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: 1, SlotID: slotID, ExecutionEpoch: 3,
		AuthType: "oauth", EmailAddress: normalizeAccountEmail("OWNER@example.com"),
		ProjectedAt: time.Now().UTC(),
	}}
	stats, err := a.reconcileRuntimeOnboardingResults(context.Background(), reader, 100)
	if err != nil || stats.Conflicted != 1 {
		t.Fatalf("duplicate stats=%+v err=%v", stats, err)
	}
	lifecycle, err := a.reconcileDuplicateIdentityLifecycle(context.Background(), 100)
	if err != nil || lifecycle.Archived != 1 {
		t.Fatalf("archive stats=%+v err=%v", lifecycle, err)
	}
	var archivedAt sql.NullString
	var proxyID, archivedProxyID sql.NullInt64
	var status string
	var schedulable int
	if err := a.db.QueryRow(`SELECT archived_at, proxy_id, archived_proxy_id, status, schedulable FROM accounts WHERE id = ?`,
		accountID).Scan(&archivedAt, &proxyID, &archivedProxyID, &status, &schedulable); err != nil {
		t.Fatal(err)
	}
	if !archivedAt.Valid || status != "disabled" || schedulable != 0 {
		t.Fatalf("archive fields at=%v proxy=%v archived_proxy=%v status=%s schedulable=%d",
			archivedAt, proxyID, archivedProxyID, status, schedulable)
	}
	var destroyEvents int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = 'account.runtime.destroy_requested'`, accountID).Scan(&destroyEvents); err != nil {
		t.Fatal(err)
	}
	if destroyEvents != 1 {
		t.Fatalf("destroy events=%d", destroyEvents)
	}
	var existingArchived sql.NullString
	if err := a.db.QueryRow(`SELECT archived_at FROM accounts WHERE id = ?`, existingID).Scan(&existingArchived); err != nil {
		t.Fatal(err)
	}
	if existingArchived.Valid {
		t.Fatal("conflicting account was archived")
	}
	lifecycle, err = a.reconcileDuplicateIdentityLifecycle(context.Background(), 100)
	if err != nil || lifecycle.Archived != 0 || lifecycle.Drained != 0 {
		t.Fatalf("replay stats=%+v err=%v", lifecycle, err)
	}
}

func TestDuplicateIdentityBatchDrainsWhenEventIsMissing(t *testing.T) {
	a, err := newApp(filepath.Join(t.TempDir(), "duplicate-drain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts
		(name, platform, auth_type, credentials_json, execution_migration_status, runtime_status, runtime_error_code, runtime_generation, schedulable)
		VALUES ('dup@example.com', 'anthropic', 'oauth', '{}', 'migrating', 'failed', 'duplicate_identity', 1, 0)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	stats, err := a.reconcileDuplicateIdentityLifecycle(context.Background(), 100)
	if err != nil || stats.Drained != 1 || stats.Archived != 0 {
		t.Fatalf("drain stats=%+v err=%v", stats, err)
	}
	var drainEvents int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox WHERE account_id = ? AND event_type = 'account.runtime.drain_requested'`, accountID).Scan(&drainEvents); err != nil {
		t.Fatal(err)
	}
	if drainEvents != 1 {
		t.Fatalf("drain events=%d", drainEvents)
	}
	stats, err = a.reconcileDuplicateIdentityLifecycle(context.Background(), 100)
	if err != nil || stats.Archived != 1 {
		t.Fatalf("archive after drain stats=%+v err=%v", stats, err)
	}
}

func TestDuplicateIdentityBatchSkipsArchivedHistory(t *testing.T) {
	const batchSize = 3
	for _, pendingCount := range []int{0, 2} {
		t.Run(fmt.Sprintf("pending_%d", pendingCount), func(t *testing.T) {
			a, err := newApp(filepath.Join(t.TempDir(), "duplicate-history.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer a.db.Close()

			// These lower-ID historical rows outnumber a whole scan page. They
			// must not consume its limit or starve later unarchived accounts.
			for index := range batchSize + 1 {
				insertDuplicateIdentityLifecycleAccount(t, a, fmt.Sprintf("history-%d", index), true)
			}
			for index := range pendingCount {
				insertDuplicateIdentityLifecycleAccount(t, a, fmt.Sprintf("pending-%d", index), false)
			}

			stats, err := a.reconcileDuplicateIdentityLifecycle(context.Background(), batchSize)
			if err != nil || stats != (duplicateIdentityLifecycleStats{Scanned: pendingCount, Drained: pendingCount}) {
				t.Fatalf("drain after archived history: stats=%+v err=%v", stats, err)
			}
			stats, err = a.reconcileDuplicateIdentityLifecycle(context.Background(), batchSize)
			if err != nil || stats != (duplicateIdentityLifecycleStats{Scanned: pendingCount, Archived: pendingCount}) {
				t.Fatalf("archive after archived history: stats=%+v err=%v", stats, err)
			}
			stats, err = a.reconcileDuplicateIdentityLifecycle(context.Background(), batchSize)
			if err != nil || stats != (duplicateIdentityLifecycleStats{}) {
				t.Fatalf("all archived must be an empty no-op: stats=%+v err=%v", stats, err)
			}

			var historicalUnchanged, pendingArchived, outboxEvents, auditRows int
			if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts
				WHERE name LIKE 'history-%' AND archived_at IS NOT NULL AND runtime_generation = 1
				  AND runtime_status = 'archived' AND status = 'disabled'
				  AND runtime_error_code = 'duplicate_identity' AND schedulable = 0`).Scan(&historicalUnchanged); err != nil {
				t.Fatal(err)
			}
			if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts
				WHERE name LIKE 'pending-%' AND archived_at IS NOT NULL AND runtime_generation = 3
				  AND runtime_status = 'archived' AND status = 'disabled'
				  AND runtime_error_code = 'duplicate_identity' AND schedulable = 0`).Scan(&pendingArchived); err != nil {
				t.Fatal(err)
			}
			if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_outbox`).Scan(&outboxEvents); err != nil {
				t.Fatal(err)
			}
			if err := a.db.QueryRow(`SELECT COUNT(*) FROM runtime_operation_audit`).Scan(&auditRows); err != nil {
				t.Fatal(err)
			}
			if historicalUnchanged != batchSize+1 || pendingArchived != pendingCount ||
				outboxEvents != 2*pendingCount || auditRows != pendingCount {
				t.Fatalf("history=%d archived=%d events=%d audit=%d, pending=%d",
					historicalUnchanged, pendingArchived, outboxEvents, auditRows, pendingCount)
			}
		})
	}
}

func insertDuplicateIdentityLifecycleAccount(t *testing.T, a *app, name string, archived bool) {
	t.Helper()
	var archivedAt any
	runtimeStatus, status := "failed", "active"
	if archived {
		archivedAt = "2026-09-01T00:00:00Z"
		runtimeStatus, status = "archived", "disabled"
	}
	if _, err := a.db.Exec(`INSERT INTO accounts
		(name, platform, auth_type, credentials_json, execution_migration_status,
		 runtime_status, runtime_error_code, runtime_generation, schedulable, status, archived_at)
		VALUES (?, 'anthropic', 'oauth', '{}', 'migrating', ?, 'duplicate_identity', 1, 0, ?, ?)`,
		name, runtimeStatus, status, archivedAt); err != nil {
		t.Fatal(err)
	}
}
