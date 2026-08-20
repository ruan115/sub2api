package main

import (
	"fmt"
	"net/http"
	"strings"
)

type accountSummary struct {
	Accounts       int64   `json:"accounts"`
	ActiveAccounts int64   `json:"active_accounts"`
	Requests       int64   `json:"requests"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	BilledCost     float64 `json:"billed_cost"`
	ActualCost     float64 `json:"actual_cost"`
}

func (a *app) migrateAdvancedFeatures() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			actor_user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
			actor_username TEXT NOT NULL DEFAULT '',
			actor_role TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			method TEXT NOT NULL,
			path TEXT NOT NULL,
			target_type TEXT NOT NULL DEFAULT '',
			target_id TEXT NOT NULL DEFAULT '',
			request_body TEXT NOT NULL DEFAULT '{}',
			client_ip TEXT NOT NULL DEFAULT '',
			user_agent TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_actor_created ON audit_logs(actor_user_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS authorization_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
			account_name TEXT NOT NULL DEFAULT '',
			proxy_id INTEGER REFERENCES proxies(id) ON DELETE SET NULL,
			proxy_ip TEXT NOT NULL DEFAULT '',
			method TEXT NOT NULL,
			success INTEGER NOT NULL DEFAULT 0,
			status_message TEXT NOT NULL DEFAULT '',
			subscription_type TEXT NOT NULL DEFAULT '',
			client_ip TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_authorization_created ON authorization_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_authorization_account_created ON authorization_logs(account_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS account_lifecycle_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			event_type TEXT NOT NULL CHECK (event_type IN ('onboarded', 'invalidated')),
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_lifecycle_created ON account_lifecycle_events(event_type, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_account_lifecycle_account ON account_lifecycle_events(account_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS pricing_sync_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			remote_url TEXT NOT NULL,
			hash_url TEXT NOT NULL,
			remote_hash TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'idle',
			model_count INTEGER NOT NULL DEFAULT 0,
			last_synced_at TEXT,
			last_checked_at TEXT,
			last_error TEXT NOT NULL DEFAULT ''
		)`,
		`INSERT OR IGNORE INTO pricing_sync_state (id, remote_url, hash_url) VALUES (1,
			'https://raw.githubusercontent.com/Wei-Shaw/model-price-repo/main/model_prices_and_context_window.json',
			'https://raw.githubusercontent.com/Wei-Shaw/model-price-repo/main/model_prices_and_context_window.sha256')`,
	}
	for _, statement := range statements {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate advanced features: %w", err)
		}
	}
	accountColumns := []struct{ name, definition string }{
		{"auth_status", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"auth_error", "TEXT NOT NULL DEFAULT ''"},
		{"auth_checked_at", "TEXT"},
		{"token_expires_at", "TEXT"},
		{"quota_5h_utilization", "REAL NOT NULL DEFAULT 0"},
		{"quota_5h_reset_at", "TEXT"},
		{"quota_7d_utilization", "REAL NOT NULL DEFAULT 0"},
		{"quota_7d_reset_at", "TEXT"},
		{"quota_sampled_at", "TEXT"},
		{"subscription_type", "TEXT NOT NULL DEFAULT ''"},
		{"account_price", "REAL NOT NULL DEFAULT 0"},
		{"onboarded_at", "TEXT"},
		{"invalidated_at", "TEXT"},
		{"survival_seconds_total", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range accountColumns {
		if err := addColumnIfMissing(a.db, "accounts", column.name, column.definition); err != nil {
			return err
		}
	}
	priceColumns := []struct{ name, definition string }{
		{"source", "TEXT NOT NULL DEFAULT 'manual'"},
		{"source_hash", "TEXT NOT NULL DEFAULT ''"},
	}
	usageColumns := []struct{ name, definition string }{
		{"user_id", "INTEGER REFERENCES users(id) ON DELETE SET NULL"},
		{"api_key_id", "INTEGER REFERENCES api_keys(id) ON DELETE SET NULL"},
	}
	for _, column := range usageColumns {
		if err := addColumnIfMissing(a.db, "usage_logs", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := a.db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_user_created ON usage_logs(user_id, created_at DESC)`); err != nil {
		return fmt.Errorf("index usage logs by user: %w", err)
	}
	if _, err := a.db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_api_key_created ON usage_logs(api_key_id, created_at DESC)`); err != nil {
		return fmt.Errorf("index usage logs by API key: %w", err)
	}
	for _, column := range priceColumns {
		if err := addColumnIfMissing(a.db, "model_prices", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := a.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_proxy_exclusive ON accounts(proxy_id) WHERE proxy_id IS NOT NULL AND deleted_at IS NULL`); err != nil {
		return fmt.Errorf("enforce one account per proxy: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET auth_status = 'valid' WHERE auth_status = 'unknown' AND credentials_json != '{}'`); err != nil {
		return fmt.Errorf("initialize account authorization state: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET onboarded_at = COALESCE(auth_checked_at, created_at) WHERE onboarded_at IS NULL AND auth_status = 'valid'`); err != nil {
		return fmt.Errorf("initialize account onboarding time: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET invalidated_at = NULL WHERE onboarded_at IS NULL AND status != 'error'`); err != nil {
		return fmt.Errorf("clear invalid pending-account timestamps: %w", err)
	}
	if _, err := a.db.Exec(`DELETE FROM account_lifecycle_events WHERE event_type = 'invalidated' AND account_id IN (SELECT id FROM accounts WHERE onboarded_at IS NULL AND status != 'error')`); err != nil {
		return fmt.Errorf("clear invalid pending-account events: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET invalidated_at = COALESCE(auth_checked_at, updated_at) WHERE invalidated_at IS NULL AND (status = 'error' OR (auth_status = 'reauth_required' AND onboarded_at IS NOT NULL))`); err != nil {
		return fmt.Errorf("initialize account invalidation time: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET survival_seconds_total = MAX(0, CAST(strftime('%s', invalidated_at) AS INTEGER) - CAST(strftime('%s', onboarded_at) AS INTEGER)) WHERE survival_seconds_total = 0 AND onboarded_at IS NOT NULL AND invalidated_at IS NOT NULL`); err != nil {
		return fmt.Errorf("initialize account accumulated survival: %w", err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type, created_at)
		SELECT id, 'onboarded', onboarded_at FROM accounts a WHERE onboarded_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM account_lifecycle_events e WHERE e.account_id = a.id AND e.event_type = 'onboarded')`); err != nil {
		return fmt.Errorf("initialize account onboarding events: %w", err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type, created_at)
		SELECT id, 'invalidated', invalidated_at FROM accounts a WHERE invalidated_at IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM account_lifecycle_events e WHERE e.account_id = a.id AND e.event_type = 'invalidated')`); err != nil {
		return fmt.Errorf("initialize account invalidation events: %w", err)
	}
	if err := a.backfillAccountSubscriptions(); err != nil {
		return err
	}
	return nil
}

func (a *app) backfillAccountSubscriptions() error {
	rows, err := a.db.Query(`SELECT id, credentials_json FROM accounts WHERE subscription_type = '' AND credentials_json != '{}'`)
	if err != nil {
		return fmt.Errorf("query account subscriptions: %w", err)
	}
	type candidate struct {
		id          int64
		credentials string
	}
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.credentials); err != nil {
			rows.Close()
			return fmt.Errorf("scan account subscription: %w", err)
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate account subscriptions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close account subscription rows: %w", err)
	}
	for _, item := range candidates {
		subscription := subscriptionTypeFromCredentials(decodeObject(item.credentials))
		if subscription == "" {
			continue
		}
		if _, err := a.db.Exec(`UPDATE accounts SET subscription_type = ? WHERE id = ? AND subscription_type = ''`, subscription, item.id); err != nil {
			return fmt.Errorf("backfill account subscription: %w", err)
		}
	}
	return nil
}

func (a *app) handleAccountSummary(w http.ResponseWriter, r *http.Request) {
	where := []string{"a.deleted_at IS NULL"}
	whereArgs := []any{}
	if user := currentUser(r); user.Role == "user" {
		condition, args := scopedAccountCondition(user, "a")
		where = append(where, condition)
		whereArgs = append(whereArgs, args...)
	}
	if groupID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))); groupID == "a" || groupID == "b" {
		where = append(where, `EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = ?)`)
		whereArgs = append(whereArgs, groupID)
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		where = append(where, accountStatePredicate("a", status))
	}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		where = append(where, "(a.name LIKE ? OR a.notes LIKE ? OR a.credential_hint LIKE ?)")
		term := "%" + search + "%"
		whereArgs = append(whereArgs, term, term, term)
	}
	join := "LEFT JOIN usage_logs u ON u.account_id = a.id"
	joinArgs := []any{}
	if user := currentUser(r); user.Role == "user" {
		join += " AND u.user_id = ?"
		joinArgs = append(joinArgs, user.ID)
	}
	if from := normalizeDateStart(strings.TrimSpace(r.URL.Query().Get("from"))); from != "" {
		join += " AND u.created_at >= ?"
		joinArgs = append(joinArgs, from)
	}
	if to := normalizeDateEnd(strings.TrimSpace(r.URL.Query().Get("to"))); to != "" {
		join += " AND u.created_at < ?"
		joinArgs = append(joinArgs, to)
	}
	args := append(joinArgs, whereArgs...)
	var item accountSummary
	query := `SELECT COUNT(DISTINCT a.id), COUNT(DISTINCT CASE WHEN ` + accountStatePredicate("a", "normal") + ` THEN a.id END), COUNT(u.id), COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0), COALESCE(SUM(u.billed_cost), 0), COALESCE(SUM(u.actual_cost), 0) FROM accounts a ` + join + ` WHERE ` + strings.Join(where, " AND ")
	if err := a.db.QueryRow(query, args...).Scan(&item.Accounts, &item.ActiveAccounts, &item.Requests, &item.InputTokens, &item.OutputTokens, &item.BilledCost, &item.ActualCost); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}
