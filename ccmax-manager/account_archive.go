package main

import (
	"errors"
	"net/http"
	"strings"
)

type accountBatchArchiveInput struct {
	IDs []int64 `json:"ids"`
}

type accountArchiveResult struct {
	Matched  int64 `json:"matched"`
	Archived int64 `json:"archived"`
	Skipped  int64 `json:"skipped"`
}

func (a *app) handleAccountArchive(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	result, err := a.archiveAccounts([]int64{id})
	if err != nil {
		writeDBError(w, err)
		return
	}
	if result.Matched == 0 {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if result.Archived == 0 {
		writeError(w, http.StatusConflict, "only dead accounts can be archived")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) handleAccountBatchArchive(w http.ResponseWriter, r *http.Request) {
	var input accountBatchArchiveInput
	if !decodeJSON(w, r, &input) {
		return
	}
	ids := uniquePositiveIDs(input.IDs, 501)
	if len(ids) == 0 || len(ids) > 500 {
		writeError(w, http.StatusBadRequest, "select between 1 and 500 accounts")
		return
	}
	result, err := a.archiveAccounts(ids)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if result.Matched == 0 {
		writeError(w, http.StatusNotFound, "no selected accounts were found")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) handleAccountRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()
	// Restoring clears archived_proxy_id, which for accounts archived before the
	// history table existed is the last record that the address was consumed.
	// Preserve it first so a single-use address stays burned.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO proxy_account_history (proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
		SELECT archived_proxy_id, id, created_at, updated_at, 1 FROM accounts
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NOT NULL AND archived_proxy_id IS NOT NULL`, id); err != nil {
		writeDBError(w, err)
		return
	}
	result, err := tx.Exec(`UPDATE accounts SET
		archived_at = NULL,
		archived_proxy_id = NULL,
		proxy_id = NULL,
		auto_proxy = 0,
		schedulable = 0,
		status = CASE WHEN status = 'disabled' THEN 'error' ELSE status END,
		rate_limit_reset_at = NULL,
		rate_limit_window = '',
		rate_limit_reason = '',
		consecutive_429 = 0,
		rate_limit_downweight_until = NULL,
		quota_refreshed_at = NULL,
		last_429_at = NULL,
		updated_at = `+nowSQL+`
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NOT NULL`, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "archived account not found")
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "restored": true})
}

func (a *app) archiveAccounts(ids []int64) (accountArchiveResult, error) {
	result := accountArchiveResult{}
	if len(ids) == 0 {
		return result, errors.New("no accounts selected")
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	tx, err := a.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := tx.QueryRow(`SELECT COUNT(*) FROM accounts WHERE deleted_at IS NULL AND archived_at IS NULL AND id IN (`+placeholders+`)`, args...).Scan(&result.Matched); err != nil {
		return result, err
	}
	deadCondition := accountStatePredicate("accounts", "error")
	if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type, reason)
		SELECT id, 'invalidated', COALESCE(NULLIF(auth_error, ''), error_message) FROM accounts
		WHERE deleted_at IS NULL AND archived_at IS NULL AND onboarded_at IS NOT NULL AND invalidated_at IS NULL
		AND id IN (`+placeholders+`) AND `+deadCondition, args...); err != nil {
		return result, err
	}
	updateResult, err := tx.Exec(`UPDATE accounts SET
		`+accumulateAccountSurvivalSQL+`,
		invalidated_at = CASE WHEN onboarded_at IS NULL THEN invalidated_at ELSE COALESCE(invalidated_at, `+nowSQL+`) END,
		archived_proxy_id = proxy_id,
		proxy_id = NULL,
		auto_proxy = 0,
		status = 'disabled',
		schedulable = 0,
		archived_at = `+nowSQL+`,
		updated_at = `+nowSQL+`
		WHERE deleted_at IS NULL AND archived_at IS NULL AND id IN (`+placeholders+`) AND `+deadCondition, args...)
	if err != nil {
		return result, err
	}
	result.Archived, err = updateResult.RowsAffected()
	if err != nil {
		return result, err
	}
	result.Skipped = result.Matched - result.Archived
	archivedAccountSubquery := `SELECT id FROM accounts WHERE archived_at IS NOT NULL AND id IN (` + placeholders + `)`
	rows, err := tx.Query(`SELECT DISTINCT archived_proxy_id FROM accounts
		WHERE archived_at IS NOT NULL AND archived_proxy_id IS NOT NULL AND id IN (`+placeholders+`)`, args...)
	if err != nil {
		return result, err
	}
	proxyIDs := []int64{}
	for rows.Next() {
		var proxyID int64
		if err := rows.Scan(&proxyID); err != nil {
			rows.Close()
			return result, err
		}
		proxyIDs = append(proxyIDs, proxyID)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if err := quarantineProxyIDs(tx, proxyIDs); err != nil {
		return result, err
	}
	if _, err := tx.Exec(`DELETE FROM account_inflight WHERE account_id IN (`+archivedAccountSubquery+`)`, args...); err != nil {
		return result, err
	}
	if _, err := tx.Exec(`DELETE FROM dispatch_sessions WHERE account_id IN (`+archivedAccountSubquery+`)`, args...); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
