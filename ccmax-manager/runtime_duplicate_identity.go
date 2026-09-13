package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

const runtimeDuplicateIdentityError = "duplicate_identity"

type duplicateIdentityLifecycleStats struct {
	Scanned   int
	Drained   int
	Archived  int
	Unchanged int
	Failed    int
}

func (a *app) startDuplicateIdentityLifecycleScheduler() func() {
	if a == nil || a.db == nil {
		return func() {}
	}
	interval := time.Duration(envInt("CCMAX_EXECUTION_DUPLICATE_IDENTITY_SECONDS", 5)) * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		run := func() {
			cycle, cycleCancel := context.WithTimeout(ctx, 30*time.Second)
			defer cycleCancel()
			stats, err := a.reconcileDuplicateIdentityLifecycle(cycle, 100)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("duplicate identity lifecycle reconciliation failed")
				return
			}
			if stats.Drained > 0 || stats.Archived > 0 || stats.Failed > 0 {
				log.Printf("duplicate identity lifecycle: scanned=%d drained=%d archived=%d unchanged=%d failed=%d",
					stats.Scanned, stats.Drained, stats.Archived, stats.Unchanged, stats.Failed)
			}
		}
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				run()
			case <-ctx.Done():
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wait.Wait()
		})
	}
}

func enqueueDuplicateIdentityDrainTx(ctx context.Context, tx *databaseTx, accountID, conflictID int64, desiredGeneration uint64) error {
	if ctx == nil || ctx.Err() != nil || tx == nil || accountID <= 0 || desiredGeneration == 0 {
		return errRuntimeOnboardingCandidate
	}
	if conflictID < 0 || conflictID == accountID {
		return errRuntimeOnboardingCandidate
	}
	payload := map[string]any{"reason": runtimeDuplicateIdentityError}
	if conflictID > 0 {
		payload["conflict_account_id"] = conflictID
	}
	encoded, err := safeRuntimePayload(payload)
	if err != nil {
		return err
	}
	var existing int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_outbox
		WHERE account_id = ? AND event_type = 'account.runtime.drain_requested' AND desired_generation = ?`,
		accountID, desiredGeneration).Scan(&existing); err != nil {
		return fmt.Errorf("inspect duplicate identity drain event: %w", err)
	}
	if existing > 0 {
		return nil
	}
	_, err = enqueueRuntimeEventTx(ctx, tx, runtimeOutboxEvent{
		EventID: newRuntimeEventID(), AccountID: accountID, EventType: "account.runtime.drain_requested",
		DesiredGeneration: desiredGeneration, PayloadJSON: encoded,
	})
	return err
}

func (a *app) reconcileDuplicateIdentityLifecycle(ctx context.Context, limit int) (duplicateIdentityLifecycleStats, error) {
	var stats duplicateIdentityLifecycleStats
	if a == nil || a.db == nil || ctx == nil || ctx.Err() != nil || limit < 1 || limit > 1000 {
		return stats, errRuntimeOnboardingCandidate
	}
	rows, err := a.db.QueryContext(ctx, `SELECT id FROM accounts
		WHERE deleted_at IS NULL AND archived_at IS NULL
		  AND runtime_error_code = ? AND execution_migration_status != 'legacy'
		ORDER BY id LIMIT ?`, runtimeDuplicateIdentityError, limit)
	if err != nil {
		return stats, fmt.Errorf("list duplicate identity accounts: %w", err)
	}
	defer rows.Close()
	var accountIDs []int64
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return stats, err
		}
		accountIDs = append(accountIDs, accountID)
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}
	for _, accountID := range accountIDs {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		stats.Scanned++
		changed, archived, err := a.advanceDuplicateIdentityAccount(ctx, accountID)
		if err != nil {
			stats.Failed++
			continue
		}
		if archived {
			stats.Archived++
			continue
		}
		if changed {
			stats.Drained++
			continue
		}
		stats.Unchanged++
	}
	return stats, nil
}

func (a *app) advanceDuplicateIdentityAccount(ctx context.Context, accountID int64) (drained bool, archived bool, err error) {
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	query := `SELECT runtime_generation, runtime_error_code, execution_migration_status, archived_at
		FROM accounts WHERE id = ? AND deleted_at IS NULL`
	if a.db.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var generation uint64
	var errorCode, migrationStatus string
	var archivedAt sql.NullString
	if err := tx.QueryRowContext(ctx, query, accountID).Scan(&generation, &errorCode, &migrationStatus, &archivedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	if errorCode != runtimeDuplicateIdentityError || migrationStatus == "legacy" {
		return false, false, nil
	}
	if archivedAt.Valid {
		return false, false, tx.Commit()
	}
	var drainCount, destroyCount int
	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN event_type = 'account.runtime.drain_requested' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN event_type = 'account.runtime.destroy_requested' THEN 1 ELSE 0 END), 0)
		FROM runtime_outbox WHERE account_id = ?`, accountID).Scan(&drainCount, &destroyCount); err != nil {
		return false, false, err
	}
	if drainCount == 0 {
		nextGeneration := generation + 1
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET runtime_generation = ?,
			runtime_status = 'failed', runtime_error_code = ?, schedulable = 0, updated_at = `+nowSQL+`
			WHERE id = ? AND runtime_generation = ? AND deleted_at IS NULL AND archived_at IS NULL`,
			nextGeneration, runtimeDuplicateIdentityError, accountID, generation)
		if err != nil {
			return false, false, err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return false, false, errRuntimeOnboardingStale
		}
		if err := enqueueDuplicateIdentityDrainTx(ctx, tx, accountID, 0, nextGeneration); err != nil {
			return false, false, err
		}
		if err := tx.Commit(); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	if destroyCount > 0 {
		return false, false, tx.Commit()
	}
	if err := archiveDuplicateIdentityAccountTx(ctx, tx, accountID, generation); err != nil {
		return false, false, err
	}
	if err := tx.Commit(); err != nil {
		return false, false, err
	}
	return false, true, nil
}

func archiveDuplicateIdentityAccountTx(ctx context.Context, tx *databaseTx, accountID int64, currentGeneration uint64) error {
	nextGeneration := currentGeneration + 1
	payload, err := safeRuntimePayload(map[string]any{"reason": runtimeDuplicateIdentityError})
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET
		runtime_generation = ?, runtime_status = 'archived', runtime_error_code = ?,
		schedulable = 0, status = 'disabled',
		archived_at = COALESCE(archived_at, `+nowSQL+`),
		archived_proxy_id = COALESCE(archived_proxy_id, proxy_id),
		updated_at = `+nowSQL+`
		WHERE id = ? AND runtime_generation = ? AND deleted_at IS NULL AND archived_at IS NULL
		  AND runtime_error_code = ? AND execution_migration_status != 'legacy'`,
		nextGeneration, runtimeDuplicateIdentityError, accountID, currentGeneration, runtimeDuplicateIdentityError)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return errRuntimeOnboardingStale
	}
	if _, err := enqueueRuntimeEventTx(ctx, tx, runtimeOutboxEvent{
		EventID: newRuntimeEventID(), AccountID: accountID, EventType: "account.runtime.destroy_requested",
		DesiredGeneration: nextGeneration, PayloadJSON: payload,
	}); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{"reason": runtimeDuplicateIdentityError})
	_, err = tx.ExecContext(ctx, `INSERT INTO runtime_operation_audit
		(event_id, account_id, operation, status, error_code, detail_json)
		VALUES (?, ?, 'account.runtime.duplicate_archived', 'completed', ?, ?)`,
		newRuntimeEventID(), accountID, runtimeDuplicateIdentityError, string(detail))
	return err
}
