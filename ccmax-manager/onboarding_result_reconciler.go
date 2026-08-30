package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
)

var (
	errRuntimeOnboardingCandidate = errors.New("runtime onboarding candidate is invalid")
	errRuntimeOnboardingStale     = errors.New("runtime onboarding result is stale")
	errRuntimeOnboardingDuplicate = errors.New("runtime onboarding identity conflicts with another account")
)

const runtimeOnboardingResultCursorName = "ccmax-runtime-onboarding-results"

type runtimeOnboardingResultReader interface {
	GetResult(ctx context.Context, intentID string, accountID int64, desiredGeneration uint64) (runtimeOnboardingResult, error)
}

type runtimeOnboardingCandidate struct {
	Sequence          int64
	AccountID         int64
	DesiredGeneration uint64
	SlotID            string
	IntentID          string
	EventID           string
}

type runtimeOnboardingCandidatePage struct {
	StartSequence int64
	Candidates    []runtimeOnboardingCandidate
}

type runtimeOnboardingReconcileStats struct {
	Checked    int
	Pending    int
	Completed  int
	Terminated int
	Conflicted int
	Failed     int
}

func (a *app) startRuntimeOnboardingResultScheduler() func() {
	if a == nil || a.onboardingIntake == nil {
		return func() {}
	}
	interval := time.Duration(envInt("CCMAX_EXECUTION_ONBOARDING_RESULT_SECONDS", 2)) * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		run := func() {
			cycle, cycleCancel := context.WithTimeout(ctx, 30*time.Second)
			defer cycleCancel()
			stats, err := a.reconcileRuntimeOnboardingResults(cycle, a.onboardingIntake, 100)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("runtime onboarding result reconciliation failed")
				return
			}
			if stats.Completed > 0 || stats.Terminated > 0 || stats.Conflicted > 0 || stats.Failed > 0 {
				log.Printf("runtime onboarding results: checked=%d pending=%d completed=%d terminated=%d conflicted=%d failed=%d",
					stats.Checked, stats.Pending, stats.Completed, stats.Terminated, stats.Conflicted, stats.Failed)
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

func (a *app) reconcileRuntimeOnboardingResults(
	ctx context.Context,
	reader runtimeOnboardingResultReader,
	limit int,
) (runtimeOnboardingReconcileStats, error) {
	var stats runtimeOnboardingReconcileStats
	if a == nil || a.db == nil || reader == nil || ctx == nil || ctx.Err() != nil || limit < 1 || limit > 1000 {
		return stats, errRuntimeOnboardingCandidate
	}
	page, err := a.listRuntimeOnboardingCandidatePage(ctx, limit)
	if err != nil {
		return stats, err
	}
	lastCheckedSequence := page.StartSequence
	for _, candidate := range page.Candidates {
		if ctx.Err() != nil {
			return stats, ctx.Err()
		}
		stats.Checked++
		lastCheckedSequence = candidate.Sequence
		if candidate.Sequence <= 0 || candidate.AccountID <= 0 || candidate.DesiredGeneration == 0 ||
			!runtimeOpaqueIntentIDPattern.MatchString(candidate.SlotID) || runtimeSecretString(candidate.SlotID) ||
			!runtimeOpaqueIntentIDPattern.MatchString(candidate.EventID) || runtimeSecretString(candidate.EventID) ||
			!runtimeOpaqueIntentIDPattern.MatchString(candidate.IntentID) || runtimeSecretString(candidate.IntentID) {
			stats.Failed++
			continue
		}
		result, resultErr := reader.GetResult(ctx, candidate.IntentID, candidate.AccountID, candidate.DesiredGeneration)
		if errors.Is(resultErr, errRuntimeOnboardingResultPending) {
			stats.Pending++
			continue
		}
		if resultErr != nil {
			stats.Failed++
			continue
		}
		if normalizedRuntimeOnboardingResultStatus(result) != runtimeOnboardingResultSucceeded {
			if err := a.applyRuntimeOnboardingTerminalResult(ctx, candidate, result); err != nil {
				stats.Failed++
				continue
			}
			stats.Terminated++
			continue
		}
		if err := a.applyRuntimeOnboardingResult(ctx, candidate, result); err != nil {
			if errors.Is(err, errRuntimeOnboardingDuplicate) {
				stats.Conflicted++
			} else {
				stats.Failed++
			}
			continue
		}
		stats.Completed++
	}
	if len(page.Candidates) > 0 {
		if err := a.advanceRuntimeOnboardingResultCursor(ctx, page.StartSequence, lastCheckedSequence); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

func (a *app) listRuntimeOnboardingCandidates(ctx context.Context, limit int) ([]runtimeOnboardingCandidate, error) {
	page, err := a.listRuntimeOnboardingCandidatePage(ctx, limit)
	if err != nil {
		return nil, err
	}
	return page.Candidates, nil
}

func (a *app) listRuntimeOnboardingCandidatePage(ctx context.Context, limit int) (runtimeOnboardingCandidatePage, error) {
	var page runtimeOnboardingCandidatePage
	if a == nil || a.db == nil || ctx == nil || ctx.Err() != nil || limit < 1 || limit > 1000 {
		return page, errRuntimeOnboardingCandidate
	}
	cursor, err := a.loadRuntimeOnboardingResultCursor(ctx)
	if err != nil {
		return page, err
	}
	page.StartSequence = cursor
	page.Candidates, err = a.queryRuntimeOnboardingCandidates(ctx, cursor, false, limit)
	if err != nil {
		return runtimeOnboardingCandidatePage{}, err
	}
	if cursor > 0 && len(page.Candidates) < limit {
		wrapped, err := a.queryRuntimeOnboardingCandidates(ctx, cursor, true, limit-len(page.Candidates))
		if err != nil {
			return runtimeOnboardingCandidatePage{}, err
		}
		page.Candidates = append(page.Candidates, wrapped...)
	}
	return page, nil
}

func (a *app) loadRuntimeOnboardingResultCursor(ctx context.Context) (int64, error) {
	if _, err := a.db.ExecContext(ctx, `INSERT OR IGNORE INTO runtime_onboarding_result_cursors (cursor_name) VALUES (?)`, runtimeOnboardingResultCursorName); err != nil {
		return 0, fmt.Errorf("initialize runtime onboarding result cursor: %w", err)
	}
	var sequence int64
	if err := a.db.QueryRowContext(ctx, `SELECT last_sequence FROM runtime_onboarding_result_cursors WHERE cursor_name = ?`, runtimeOnboardingResultCursorName).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("load runtime onboarding result cursor: %w", err)
	}
	if sequence < 0 {
		return 0, fmt.Errorf("load runtime onboarding result cursor: %w", errRuntimeOnboardingCandidate)
	}
	return sequence, nil
}

func (a *app) advanceRuntimeOnboardingResultCursor(ctx context.Context, expectedSequence, nextSequence int64) error {
	if expectedSequence < 0 || nextSequence <= 0 {
		return errRuntimeOnboardingCandidate
	}
	result, err := a.db.ExecContext(ctx, `UPDATE runtime_onboarding_result_cursors
		SET last_sequence = ?, updated_at = `+nowSQL+`
		WHERE cursor_name = ? AND last_sequence = ?`, nextSequence, runtimeOnboardingResultCursorName, expectedSequence)
	if err != nil {
		return fmt.Errorf("advance runtime onboarding result cursor: %w", err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read runtime onboarding result cursor update: %w", err)
	}
	// A zero-row compare-and-swap means another replica advanced the shared
	// cursor first. Duplicate checks are acceptable; the winner determines the
	// next keyset page, so no replica can pin the scan to a permanent prefix.
	return nil
}

func (a *app) queryRuntimeOnboardingCandidates(
	ctx context.Context,
	cursor int64,
	throughCursor bool,
	limit int,
) ([]runtimeOnboardingCandidate, error) {
	comparison := `o.sequence > ?`
	if throughCursor {
		comparison = `o.sequence <= ?`
	}
	rows, err := a.db.QueryContext(ctx, `SELECT o.sequence, a.id, a.runtime_generation, a.runtime_slot_id, o.event_id, o.payload_json
		FROM accounts a
		JOIN runtime_outbox o ON o.account_id = a.id AND o.desired_generation = a.runtime_generation
		WHERE a.deleted_at IS NULL AND a.archived_at IS NULL
		  AND a.execution_migration_status IN ('migrating', 'migrated')
		  AND a.runtime_status = 'provisioning'
		  AND o.event_type IN ('account.runtime.provision_requested', 'account.credential.migrate_requested', 'account.credential.rotate_requested')
		  AND `+comparison+`
		ORDER BY o.sequence LIMIT ?`, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("list runtime onboarding candidates: %w", err)
	}
	defer rows.Close()
	result := make([]runtimeOnboardingCandidate, 0, limit)
	for rows.Next() {
		var candidate runtimeOnboardingCandidate
		var payload string
		if err := rows.Scan(&candidate.Sequence, &candidate.AccountID, &candidate.DesiredGeneration, &candidate.SlotID, &candidate.EventID, &payload); err != nil {
			return nil, fmt.Errorf("scan runtime onboarding candidate: %w", err)
		}
		intentID, err := runtimeOnboardingIntentID(payload)
		if err == nil {
			candidate.IntentID = intentID
		}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate runtime onboarding candidates: %w", err)
	}
	return result, nil
}

func runtimeOnboardingIntentID(payload string) (string, error) {
	decoder := json.NewDecoder(bytes.NewBufferString(payload))
	decoder.DisallowUnknownFields()
	var decoded struct {
		IntentID string `json:"onboarding_intent_id"`
	}
	if err := decoder.Decode(&decoded); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return "", errRuntimeOnboardingCandidate
	}
	decoded.IntentID = strings.TrimSpace(decoded.IntentID)
	if !runtimeOpaqueIntentIDPattern.MatchString(decoded.IntentID) || runtimeSecretString(decoded.IntentID) {
		return "", errRuntimeOnboardingCandidate
	}
	return decoded.IntentID, nil
}

func (a *app) applyRuntimeOnboardingResult(
	ctx context.Context,
	candidate runtimeOnboardingCandidate,
	result runtimeOnboardingResult,
) error {
	if result.AccountID != candidate.AccountID || result.DesiredGeneration != candidate.DesiredGeneration ||
		result.IntentID != candidate.IntentID || result.SlotID != candidate.SlotID ||
		normalizedRuntimeOnboardingResultStatus(result) != runtimeOnboardingResultSucceeded || !validRuntimeOnboardingResult(result) {
		return errRuntimeOnboardingStale
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `SELECT execution_migration_status, runtime_status, runtime_generation, runtime_slot_id,
		credentials_json, onboarded_at FROM accounts
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`
	if a.db.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var migrationStatus, runtimeStatus, slotID, credentialsJSON string
	var generation uint64
	var onboardedAt sql.NullString
	if err := tx.QueryRowContext(ctx, query, candidate.AccountID).Scan(
		&migrationStatus, &runtimeStatus, &generation, &slotID, &credentialsJSON, &onboardedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errRuntimeOnboardingStale
		}
		return err
	}
	if (migrationStatus != "migrating" && migrationStatus != "migrated") || runtimeStatus != "provisioning" ||
		generation != candidate.DesiredGeneration || slotID != candidate.SlotID || !emptyJSONObject(credentialsJSON) {
		return errRuntimeOnboardingStale
	}
	conflictID, err := runtimeOnboardingEmailConflict(ctx, tx, candidate.AccountID, result.EmailAddress)
	if err != nil {
		return err
	}
	if conflictID != 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET runtime_status = 'failed', runtime_error_code = 'duplicate_identity',
			auth_status = 'invalid', auth_error = 'runtime onboarding identity conflict', schedulable = 0, updated_at = `+nowSQL+`
			WHERE id = ? AND runtime_generation = ? AND runtime_status = 'provisioning'`, candidate.AccountID, generation); err != nil {
			return err
		}
		detail, _ := json.Marshal(map[string]any{"conflict_account_id": conflictID})
		if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_operation_audit
			(event_id, account_id, operation, status, error_code, detail_json)
			VALUES (?, ?, 'account.runtime.result_projected', 'blocked', 'duplicate_identity', ?)`, candidate.EventID, candidate.AccountID, string(detail)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return errRuntimeOnboardingDuplicate
	}

	expiresAt := any(nil)
	if !result.ExpiresAt.IsZero() {
		expiresAt = result.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	isReauthorization := onboardedAt.Valid
	// A newly-created execution account starts with a provisional label, so its
	// authenticated email becomes the account name. Reauthorization must not
	// overwrite an administrator-assigned name on an already migrated account.
	name := ""
	if !onboardedAt.Valid {
		name = result.EmailAddress
	}
	update, err := tx.ExecContext(ctx, `UPDATE accounts SET
		name = CASE WHEN ? = '' THEN name ELSE ? END,
		auth_type = ?, auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, token_expires_at = ?,
		subscription_type = ?, rate_limit_tier = ?,
		execution_migration_status = 'migrated', runtime_status = 'ready', runtime_error_code = '',
		runtime_execution_epoch = ?, schedulable = 1, status = 'active', error_message = '',
		onboarded_at = CASE WHEN onboarded_at IS NULL OR invalidated_at IS NOT NULL THEN `+nowSQL+` ELSE onboarded_at END,
		invalidated_at = NULL,
		reauthorized_at = CASE WHEN ? THEN `+nowSQL+` ELSE reauthorized_at END,
		reauthorization_count = reauthorization_count + CASE WHEN ? THEN 1 ELSE 0 END,
		updated_at = `+nowSQL+`
		WHERE id = ? AND runtime_generation = ? AND runtime_status = 'provisioning'
		  AND execution_migration_status IN ('migrating', 'migrated')`,
		name, name, result.AuthType, expiresAt, result.SubscriptionType, result.RateLimitTier,
		result.ExecutionEpoch, isReauthorization, isReauthorization, candidate.AccountID, generation)
	if err != nil {
		return err
	}
	if affected, err := update.RowsAffected(); err != nil || affected != 1 {
		return errRuntimeOnboardingStale
	}
	for _, mode := range []string{executionModeCLINative, executionModeOAuthAPI} {
		status, errorCode := "healthy", ""
		if result.AuthType == "api_key" && mode == executionModeCLINative {
			status, errorCode = "unavailable", "auth_type_unsupported"
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO account_mode_health (account_id, mode, status) VALUES (?, ?, 'unavailable')`, candidate.AccountID, mode); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_mode_health SET status = ?, error_code = ?, recover_at = NULL, updated_at = `+nowSQL+`
			WHERE account_id = ? AND mode = ?`, status, errorCode, candidate.AccountID, mode); err != nil {
			return err
		}
	}
	// Existing lifecycle analytics model reauthorization as a subsequent
	// "onboarded" event; the table deliberately accepts only onboarded and
	// invalidated. The dedicated account counters above retain the distinction.
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, 'onboarded')`, candidate.AccountID); err != nil {
		return err
	}
	detail, _ := json.Marshal(map[string]any{
		"auth_type": result.AuthType, "email_address": result.EmailAddress,
		"organization_id": result.OrganizationID, "upstream_account_id": result.UpstreamAccountID,
		"scope": result.Scope, "subscription_type": result.SubscriptionType, "rate_limit_tier": result.RateLimitTier,
		"slot_id": result.SlotID, "execution_epoch": result.ExecutionEpoch,
	})
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_operation_audit
		(event_id, account_id, operation, status, error_code, detail_json)
		VALUES (?, ?, 'account.runtime.result_projected', 'completed', '', ?)`, candidate.EventID, candidate.AccountID, string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) applyRuntimeOnboardingTerminalResult(
	ctx context.Context,
	candidate runtimeOnboardingCandidate,
	result runtimeOnboardingResult,
) error {
	resultStatus := normalizedRuntimeOnboardingResultStatus(result)
	if result.AccountID != candidate.AccountID || result.DesiredGeneration != candidate.DesiredGeneration ||
		result.IntentID != candidate.IntentID ||
		(resultStatus != runtimeOnboardingResultFailed && resultStatus != runtimeOnboardingResultExpired) ||
		(result.SlotID != "" && result.SlotID != candidate.SlotID) || !validRuntimeOnboardingResult(result) {
		return errRuntimeOnboardingStale
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `SELECT execution_migration_status, runtime_status, runtime_generation, runtime_slot_id,
		credentials_json, onboarded_at, invalidated_at FROM accounts
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`
	if a.db.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var migrationStatus, runtimeStatus, slotID, credentialsJSON string
	var generation uint64
	var onboardedAt, invalidatedAt sql.NullString
	if err := tx.QueryRowContext(ctx, query, candidate.AccountID).Scan(
		&migrationStatus, &runtimeStatus, &generation, &slotID, &credentialsJSON, &onboardedAt, &invalidatedAt,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errRuntimeOnboardingStale
		}
		return err
	}
	if (migrationStatus != "migrating" && migrationStatus != "migrated") || runtimeStatus != "provisioning" ||
		generation != candidate.DesiredGeneration || slotID != candidate.SlotID || !emptyJSONObject(credentialsJSON) {
		return errRuntimeOnboardingStale
	}
	update, err := tx.ExecContext(ctx, `UPDATE accounts SET
		runtime_status = 'failed', runtime_error_code = ?, schedulable = 0,
		auth_status = 'invalid', auth_error = ?,
		status = CASE WHEN status = 'disabled' THEN status ELSE 'error' END,
		error_message = ?,
		invalidated_at = CASE WHEN onboarded_at IS NULL THEN invalidated_at ELSE COALESCE(invalidated_at, `+nowSQL+`) END,
		updated_at = `+nowSQL+`
		WHERE id = ? AND runtime_generation = ? AND runtime_status = 'provisioning'
		  AND execution_migration_status IN ('migrating', 'migrated')`,
		result.ErrorCode, result.ErrorSummary, result.ErrorSummary, candidate.AccountID, generation)
	if err != nil {
		return err
	}
	if affected, err := update.RowsAffected(); err != nil || affected != 1 {
		return errRuntimeOnboardingStale
	}
	for _, mode := range []string{executionModeCLINative, executionModeOAuthAPI} {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO account_mode_health (account_id, mode, status) VALUES (?, ?, 'unavailable')`, candidate.AccountID, mode); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE account_mode_health SET status = 'unavailable', error_code = ?, recover_at = NULL, updated_at = `+nowSQL+`
			WHERE account_id = ? AND mode = ?`, result.ErrorCode, candidate.AccountID, mode); err != nil {
			return err
		}
	}
	if onboardedAt.Valid && !invalidatedAt.Valid {
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_lifecycle_events (account_id, event_type, reason)
			VALUES (?, 'invalidated', ?)`, candidate.AccountID, result.ErrorSummary); err != nil {
			return err
		}
	}
	detail, _ := json.Marshal(map[string]any{
		"result_status": resultStatus, "error_summary": result.ErrorSummary,
		"finished_at": result.FinishedAt.UTC().Format(time.RFC3339Nano),
		"slot_id":     result.SlotID, "execution_epoch": result.ExecutionEpoch,
	})
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_operation_audit
		(event_id, account_id, operation, status, error_code, detail_json)
		VALUES (?, ?, 'account.runtime.result_projected', 'failed', ?, ?)`,
		candidate.EventID, candidate.AccountID, result.ErrorCode, string(detail)); err != nil {
		return err
	}
	return tx.Commit()
}

func normalizedRuntimeOnboardingResultStatus(result runtimeOnboardingResult) string {
	if result.Status == "" {
		return runtimeOnboardingResultSucceeded
	}
	return result.Status
}

func runtimeOnboardingEmailConflict(ctx context.Context, tx *databaseTx, accountID int64, email string) (int64, error) {
	target := normalizeAccountEmail(email)
	if target == "" {
		return 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, name, credentials_json FROM accounts
		WHERE id <> ? AND platform = 'anthropic' AND deleted_at IS NULL AND archived_at IS NULL ORDER BY id`, accountID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var otherID int64
		var name, credentialsJSON string
		if err := rows.Scan(&otherID, &name, &credentialsJSON); err != nil {
			return 0, err
		}
		credentialEmail, _ := decodeObject(credentialsJSON)["email_address"].(string)
		if normalizeAccountEmail(name) == target || normalizeAccountEmail(credentialEmail) == target {
			return otherID, nil
		}
	}
	return 0, rows.Err()
}
