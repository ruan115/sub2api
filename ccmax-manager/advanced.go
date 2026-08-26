package main

import (
	"context"
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
	// dispatch_strategies must exist before groups/accounts add strategy_id
	// columns that reference it.
	if _, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS dispatch_strategies (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			rpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (rpm_limit >= 0),
			tpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (tpm_limit >= 0),
			itpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (itpm_limit >= 0),
			concurrency_limit INTEGER NOT NULL DEFAULT 0 CHECK (concurrency_limit >= 0),
			rpm_strategy TEXT NOT NULL DEFAULT 'fixed' CHECK (rpm_strategy IN ('tiered', 'sticky_exempt', 'fixed')),
			rpm_sticky_buffer INTEGER NOT NULL DEFAULT 0 CHECK (rpm_sticky_buffer >= 0),
			dispatch_mode TEXT NOT NULL DEFAULT '' CHECK (dispatch_mode IN (` + dispatchModeCheckList + `)),
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			deleted_at TEXT
		)`); err != nil {
		return fmt.Errorf("create dispatch strategies table: %w", err)
	}
	// Added before the rebuild below so an existing table gains the column and
	// the rebuild's INSERT ... SELECT can carry it across.
	if err := addColumnIfMissing(a.db, "dispatch_strategies", "itpm_limit", "INTEGER NOT NULL DEFAULT 0 CHECK (itpm_limit >= 0)"); err != nil {
		return err
	}
	if err := a.migrateDispatchStrategyModes(); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "normal_request_mode", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "claude_code_identity_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "stream_hedge_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "adaptive_hedge_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "rpm_dispatch_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "mcp_tool_names_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "service_tier_passthrough_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "inference_geo_passthrough_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "speed_passthrough_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "anthropic_beta_passthrough_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "reject_anthropic_downgrade_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "reject_distillation_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "request_format_filter_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Off by default: enabling it hides the pooled account's unified ratelimit
	// headers from clients, so Claude Code stops falling back to Opus 4.8 when
	// the account crosses the advertised fallback percentage.
	if err := addColumnIfMissing(a.db, "groups", "quota_header_masking_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Off by default: enabling it stamps the real cache_creation 5m/1h bucket
	// split (tracked from message_start) into distilled message_delta usage
	// instead of backfilling zeros.
	if err := addColumnIfMissing(a.db, "groups", "cache_creation_detail_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "overload_cooldown_seconds", "INTEGER NOT NULL DEFAULT 10 CHECK (overload_cooldown_seconds BETWEEN 1 AND 600)"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "rate_limit_downweight_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "rate_limit_cooling_threshold", "INTEGER NOT NULL DEFAULT 3 CHECK (rate_limit_cooling_threshold BETWEEN 1 AND 10)"); err != nil {
		return err
	}
	// Reuse the former 429 wait-seconds column as the maximum duration for the
	// switch-controlled transient cooldown. Values from the old 1-600 second
	// retry setting are normalised into the new 60-120 second safety range.
	if err := addColumnIfMissing(a.db, "groups", "rate_limit_wait_seconds", "INTEGER NOT NULL DEFAULT 120 CHECK (rate_limit_wait_seconds BETWEEN 60 AND 120)"); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE groups SET rate_limit_wait_seconds = ? WHERE rate_limit_wait_seconds < ? OR rate_limit_wait_seconds > ?`, defaultRateLimitCooldownSeconds, minRateLimitCooldownSeconds, maxRateLimitCooldownSeconds); err != nil {
		return fmt.Errorf("normalise group 429 cooldown seconds: %w", err)
	}
	if err := addColumnIfMissing(a.db, "groups", "rate_limit_stepped_cooldown_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "rate_limit_cooldown_step_seconds", "INTEGER NOT NULL DEFAULT 30 CHECK (rate_limit_cooldown_step_seconds BETWEEN 1 AND 60)"); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE groups SET rate_limit_cooldown_step_seconds = ? WHERE rate_limit_cooldown_step_seconds < 1 OR rate_limit_cooldown_step_seconds > ?`, defaultRateLimitCooldownStepSeconds, maxRateLimitCooldownStepSeconds); err != nil {
		return fmt.Errorf("normalise group 429 cooldown step seconds: %w", err)
	}
	if err := addColumnIfMissing(a.db, "groups", "rate_limit_downweight_stepped_cooldown_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "rate_limit_downweight_base_minutes", "INTEGER NOT NULL DEFAULT 60 CHECK (rate_limit_downweight_base_minutes BETWEEN 1 AND 315)"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "rate_limit_downweight_step_minutes", "INTEGER NOT NULL DEFAULT 60 CHECK (rate_limit_downweight_step_minutes BETWEEN 1 AND 315)"); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE groups SET rate_limit_downweight_base_minutes = ? WHERE rate_limit_downweight_base_minutes < ? OR rate_limit_downweight_base_minutes > ?`, defaultRateLimitDownweightBaseMinutes, minRateLimitDownweightMinutes, maxRateLimitDownweightMinutes); err != nil {
		return fmt.Errorf("normalise group 429 downweight base minutes: %w", err)
	}
	if _, err := a.db.Exec(`UPDATE groups SET rate_limit_downweight_step_minutes = ? WHERE rate_limit_downweight_step_minutes < ? OR rate_limit_downweight_step_minutes > ?`, defaultRateLimitDownweightStepMinutes, minRateLimitDownweightMinutes, maxRateLimitDownweightMinutes); err != nil {
		return fmt.Errorf("normalise group 429 downweight step minutes: %w", err)
	}
	if err := addColumnIfMissing(a.db, "groups", "five_hour_release_stagger_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "five_hour_release_stagger_min_minutes", "INTEGER NOT NULL DEFAULT 15 CHECK (five_hour_release_stagger_min_minutes BETWEEN 0 AND 315)"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "five_hour_release_stagger_max_minutes", "INTEGER NOT NULL DEFAULT 30 CHECK (five_hour_release_stagger_max_minutes BETWEEN 0 AND 315)"); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE groups SET five_hour_release_stagger_min_minutes = ?, five_hour_release_stagger_max_minutes = ? WHERE five_hour_release_stagger_min_minutes < 0 OR five_hour_release_stagger_max_minutes < five_hour_release_stagger_min_minutes OR five_hour_release_stagger_max_minutes > ?`, defaultFiveHourReleaseStaggerMinMinutes, defaultFiveHourReleaseStaggerMaxMinutes, maxFiveHourReleaseStaggerMinutes); err != nil {
		return fmt.Errorf("normalise group 5h release stagger minutes: %w", err)
	}
	if err := addColumnIfMissing(a.db, "groups", "strategy_id", "INTEGER REFERENCES dispatch_strategies(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "accounts", "strategy_id", "INTEGER REFERENCES dispatch_strategies(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "capacity_queue_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "capacity_queue_timeout_seconds", "INTEGER NOT NULL DEFAULT 30 CHECK (capacity_queue_timeout_seconds BETWEEN 1 AND 600)"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "groups", "reserve_pool_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Off by default: turning it on restricts the group to accounts that resolve
	// a dispatch strategy, so existing unbound pools keep serving traffic.
	if err := addColumnIfMissing(a.db, "groups", "strategy_required_enabled", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Existing hedge groups keep their explicit dispatch strategy after this feature is introduced.
	if _, err := a.db.Exec(`UPDATE groups SET rpm_dispatch_enabled = 0 WHERE stream_hedge_enabled = 1 OR adaptive_hedge_enabled = 1`); err != nil {
		return fmt.Errorf("migrate RPM dispatch compatibility: %w", err)
	}
	if err := a.migrateDynamicGroups(); err != nil {
		return err
	}
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
		`CREATE TABLE IF NOT EXISTS gateway_error_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL DEFAULT '',
			api_key_id INTEGER REFERENCES api_keys(id) ON DELETE SET NULL,
			user_id INTEGER REFERENCES users(id) ON DELETE SET NULL,
			account_id INTEGER REFERENCES accounts(id) ON DELETE SET NULL,
			group_id TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL,
			category TEXT NOT NULL DEFAULT 'gateway_request',
			method TEXT NOT NULL,
			path TEXT NOT NULL,
			message TEXT NOT NULL DEFAULT '',
			client_ip TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			rpm_snapshot INTEGER NOT NULL DEFAULT -1,
			tpm_snapshot INTEGER NOT NULL DEFAULT -1,
			total_requests INTEGER NOT NULL DEFAULT -1,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_errors_created ON gateway_error_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_errors_user_created ON gateway_error_logs(user_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS account_lifecycle_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			event_type TEXT NOT NULL CHECK (event_type IN ('onboarded', 'invalidated')),
			reason TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_lifecycle_created ON account_lifecycle_events(event_type, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_account_lifecycle_account ON account_lifecycle_events(account_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS account_token_leases (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			owner TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_token_leases_expiry ON account_token_leases(expires_at, account_id)`,
		`CREATE TABLE IF NOT EXISTS account_rpm_thresholds (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			rpm_limit INTEGER NOT NULL CHECK (rpm_limit > 0),
			reset_at TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_rpm_thresholds_reset ON account_rpm_thresholds(reset_at, account_id)`,
		`CREATE TABLE IF NOT EXISTS reserve_activation_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			source_group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			target_group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			reason TEXT NOT NULL CHECK (reason IN ('capacity', 'rate_limit')),
			requested_model TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_reserve_activation_target_created ON reserve_activation_logs(target_group_id, created_at DESC)`,
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
	if _, err := a.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_groups_single_reserve_pool ON groups(reserve_pool_enabled) WHERE reserve_pool_enabled = 1`); err != nil {
		return fmt.Errorf("enforce single reserve pool: %w", err)
	}
	if err := addColumnIfMissing(a.db, "gateway_error_logs", "account_id", "INTEGER REFERENCES accounts(id) ON DELETE SET NULL"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "gateway_error_logs", "category", "TEXT NOT NULL DEFAULT 'gateway_request'"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "gateway_error_logs", "duration_ms", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	for _, column := range []string{"rpm_snapshot", "tpm_snapshot", "total_requests"} {
		if err := addColumnIfMissing(a.db, "gateway_error_logs", column, "INTEGER NOT NULL DEFAULT -1"); err != nil {
			return err
		}
	}
	for _, column := range []struct{ name, definition string }{
		{"client_request_id", "TEXT NOT NULL DEFAULT ''"},
		{"trace_id", "TEXT NOT NULL DEFAULT ''"},
		{"upstream_request_id", "TEXT NOT NULL DEFAULT ''"},
		{"dispatch_diagnostics", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := addColumnIfMissing(a.db, "gateway_error_logs", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := a.db.Exec(`CREATE INDEX IF NOT EXISTS idx_gateway_errors_account_created ON gateway_error_logs(account_id, created_at DESC)`); err != nil {
		return fmt.Errorf("migrate gateway error account index: %w", err)
	}
	// The invalidation reason is pinned onto the lifecycle event so the
	// de-authorization monitor stays accurate after an account is re-authorized
	// and accounts.auth_error is cleared.
	if err := addColumnIfMissing(a.db, "account_lifecycle_events", "reason", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	accountColumns := []struct{ name, definition string }{
		{"source_sk_hint", "TEXT NOT NULL DEFAULT ''"},
		{"auth_status", "TEXT NOT NULL DEFAULT 'unknown'"},
		{"auth_error", "TEXT NOT NULL DEFAULT ''"},
		{"auth_checked_at", "TEXT"},
		{"token_expires_at", "TEXT"},
		{"quota_5h_utilization", "REAL NOT NULL DEFAULT 0"},
		{"quota_5h_reset_at", "TEXT"},
		{"quota_5h_threshold_enabled", "INTEGER NOT NULL DEFAULT 0"},
		{"quota_5h_threshold_percent", "INTEGER NOT NULL DEFAULT 80 CHECK (quota_5h_threshold_percent BETWEEN 1 AND 100)"},
		{"quota_7d_utilization", "REAL NOT NULL DEFAULT 0"},
		{"quota_7d_reset_at", "TEXT"},
		{"quota_sampled_at", "TEXT"},
		{"subscription_type", "TEXT NOT NULL DEFAULT ''"},
		{"rate_limit_tier", "TEXT NOT NULL DEFAULT ''"},
		{"account_price", "REAL NOT NULL DEFAULT 0"},
		{"onboarded_at", "TEXT"},
		{"reauthorized_at", "TEXT"},
		{"reauthorization_count", "INTEGER NOT NULL DEFAULT 0"},
		{"invalidated_at", "TEXT"},
		{"survival_seconds_total", "INTEGER NOT NULL DEFAULT 0"},
		{"archived_at", "TEXT"},
		{"archived_proxy_id", "INTEGER REFERENCES proxies(id) ON DELETE SET NULL"},
		// Which unified window ('5h' / '7d') caused the current cooldown; empty
		// when the upstream did not say or the cooldown came from a bare 429.
		{"rate_limit_window", "TEXT NOT NULL DEFAULT ''"},
		// Automatic 429 state is separate from schedulable, which remains an
		// administrator-controlled switch.
		{"rate_limit_reason", "TEXT NOT NULL DEFAULT ''"},
		{"consecutive_429", "INTEGER NOT NULL DEFAULT 0"},
		{"last_429_at", "TEXT"},
		// The penalty lasts until the quota window in force when the limit was
		// hit rolls over; quota_refreshed_at then earns a short priority boost.
		{"rate_limit_downweight_until", "TEXT"},
		{"quota_refreshed_at", "TEXT"},
		// The upstream's own remaining-ITPM report, sampled from response
		// headers. Authoritative, so the dispatcher never has to estimate a
		// request's token cost before sending it.
		{"itpm_remaining", "INTEGER"},
		{"itpm_reset_at", "TEXT"},
		{"itpm_sampled_at", "TEXT"},
	}
	for _, column := range accountColumns {
		if err := addColumnIfMissing(a.db, "accounts", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := a.db.Exec(`CREATE INDEX IF NOT EXISTS idx_accounts_quota_5h_threshold ON accounts(quota_5h_threshold_enabled, quota_5h_utilization)`); err != nil {
		return fmt.Errorf("index account 5h threshold: %w", err)
	}
	priceColumns := []struct{ name, definition string }{
		{"source", "TEXT NOT NULL DEFAULT 'manual'"},
		{"source_hash", "TEXT NOT NULL DEFAULT ''"},
	}
	usageColumns := []struct{ name, definition string }{
		{"user_id", "INTEGER REFERENCES users(id) ON DELETE SET NULL"},
		{"api_key_id", "INTEGER REFERENCES api_keys(id) ON DELETE SET NULL"},
		{"account_sk_hint", "TEXT NOT NULL DEFAULT ''"},
		{"client_request_id", "TEXT NOT NULL DEFAULT ''"},
		{"trace_id", "TEXT NOT NULL DEFAULT ''"},
		{"upstream_request_id", "TEXT NOT NULL DEFAULT ''"},
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
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_usage_client_request ON usage_logs(client_request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_trace ON usage_logs(trace_id)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_errors_client_request ON gateway_error_logs(client_request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_gateway_errors_trace ON gateway_error_logs(trace_id)`,
	} {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("index gateway request correlation: %w", err)
		}
	}
	for _, column := range priceColumns {
		if err := addColumnIfMissing(a.db, "model_prices", column.name, column.definition); err != nil {
			return err
		}
	}
	if _, err := a.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_proxy_exclusive ON accounts(proxy_id) WHERE proxy_id IS NOT NULL AND deleted_at IS NULL`); err != nil {
		return fmt.Errorf("enforce one account per proxy: %w", err)
	}
	// Account lifecycle and proxy history backfills live in migrateSharedData
	// so MySQL runs them too.
	return nil
}

// migrateDispatchStrategyModes rebuilds dispatch_strategies whenever its
// dispatch_mode CHECK predates a mode the code now accepts. SQLite cannot alter
// a CHECK in place, so the table is copied. Adding a future mode only requires
// extending dispatchModes.
func (a *app) migrateDispatchStrategyModes() error {
	var schema string
	if err := a.db.QueryRow(`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'table' AND name = 'dispatch_strategies'`).Scan(&schema); err != nil {
		return fmt.Errorf("inspect dispatch strategies schema: %w", err)
	}
	stale := false
	for _, mode := range dispatchModes {
		if mode != "" && !strings.Contains(schema, "'"+mode+"'") {
			stale = true
			break
		}
	}
	if !stale {
		return nil
	}

	ctx := context.Background()
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open dispatch strategies migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for dispatch strategies migration: %w", err)
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin dispatch strategies migration: %w", err)
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE dispatch_strategies_migrated (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			rpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (rpm_limit >= 0),
			tpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (tpm_limit >= 0),
			itpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (itpm_limit >= 0),
			concurrency_limit INTEGER NOT NULL DEFAULT 0 CHECK (concurrency_limit >= 0),
			rpm_strategy TEXT NOT NULL DEFAULT 'fixed' CHECK (rpm_strategy IN ('tiered', 'sticky_exempt', 'fixed')),
			rpm_sticky_buffer INTEGER NOT NULL DEFAULT 0 CHECK (rpm_sticky_buffer >= 0),
			dispatch_mode TEXT NOT NULL DEFAULT '' CHECK (dispatch_mode IN (` + dispatchModeCheckList + `)),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			deleted_at TEXT
		)`,
		`INSERT INTO dispatch_strategies_migrated (id, name, description, rpm_limit, tpm_limit, itpm_limit, concurrency_limit, rpm_strategy, rpm_sticky_buffer, dispatch_mode, created_at, updated_at, deleted_at)
		 SELECT id, name, description, rpm_limit, tpm_limit, itpm_limit, concurrency_limit, rpm_strategy, rpm_sticky_buffer, dispatch_mode, created_at, updated_at, deleted_at FROM dispatch_strategies`,
		`DROP TABLE dispatch_strategies`,
		`ALTER TABLE dispatch_strategies_migrated RENAME TO dispatch_strategies`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate dispatch strategies round robin: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dispatch strategies round robin migration: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("restore foreign keys after dispatch strategies migration: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check dispatch strategies migration foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("dispatch strategies migration left invalid foreign keys")
	}
	return nil
}

func (a *app) migrateDynamicGroups() error {
	var schema string
	if err := a.db.QueryRow(`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'table' AND name = 'groups'`).Scan(&schema); err != nil {
		return fmt.Errorf("inspect groups schema: %w", err)
	}
	compact := strings.NewReplacer(" ", "", "\n", "", "\t", "").Replace(strings.ToLower(schema))
	if !strings.Contains(compact, "check(idin('a','b'))") {
		return nil
	}

	ctx := context.Background()
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("open groups migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys for groups migration: %w", err)
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin groups migration: %w", err)
	}
	defer tx.Rollback()
	statements := []string{
		`CREATE TABLE groups_dynamic (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			rate_multiplier REAL NOT NULL DEFAULT 1 CHECK (rate_multiplier >= 0),
			daily_limit_usd REAL,
			monthly_limit_usd REAL,
			normal_request_mode INTEGER NOT NULL DEFAULT 0,
			claude_code_identity_enabled INTEGER NOT NULL DEFAULT 0,
			stream_hedge_enabled INTEGER NOT NULL DEFAULT 0,
			adaptive_hedge_enabled INTEGER NOT NULL DEFAULT 0,
			rpm_dispatch_enabled INTEGER NOT NULL DEFAULT 1,
			mcp_tool_names_enabled INTEGER NOT NULL DEFAULT 0,
			service_tier_passthrough_enabled INTEGER NOT NULL DEFAULT 0,
			inference_geo_passthrough_enabled INTEGER NOT NULL DEFAULT 0,
			speed_passthrough_enabled INTEGER NOT NULL DEFAULT 0,
			anthropic_beta_passthrough_enabled INTEGER NOT NULL DEFAULT 0,
			reject_anthropic_downgrade_enabled INTEGER NOT NULL DEFAULT 0,
			reject_distillation_enabled INTEGER NOT NULL DEFAULT 0,
			request_format_filter_enabled INTEGER NOT NULL DEFAULT 0,
			quota_header_masking_enabled INTEGER NOT NULL DEFAULT 0,
			cache_creation_detail_enabled INTEGER NOT NULL DEFAULT 0,
			overload_cooldown_seconds INTEGER NOT NULL DEFAULT 10 CHECK (overload_cooldown_seconds BETWEEN 1 AND 600),
			rate_limit_downweight_enabled INTEGER NOT NULL DEFAULT 1,
			rate_limit_cooling_threshold INTEGER NOT NULL DEFAULT 3 CHECK (rate_limit_cooling_threshold BETWEEN 1 AND 10),
			rate_limit_wait_seconds INTEGER NOT NULL DEFAULT 120 CHECK (rate_limit_wait_seconds BETWEEN 60 AND 120),
			rate_limit_stepped_cooldown_enabled INTEGER NOT NULL DEFAULT 0,
			rate_limit_cooldown_step_seconds INTEGER NOT NULL DEFAULT 30 CHECK (rate_limit_cooldown_step_seconds BETWEEN 1 AND 60),
			rate_limit_downweight_stepped_cooldown_enabled INTEGER NOT NULL DEFAULT 0,
			rate_limit_downweight_base_minutes INTEGER NOT NULL DEFAULT 60 CHECK (rate_limit_downweight_base_minutes BETWEEN 1 AND 315),
			rate_limit_downweight_step_minutes INTEGER NOT NULL DEFAULT 60 CHECK (rate_limit_downweight_step_minutes BETWEEN 1 AND 315),
			five_hour_release_stagger_enabled INTEGER NOT NULL DEFAULT 1,
			five_hour_release_stagger_min_minutes INTEGER NOT NULL DEFAULT 15 CHECK (five_hour_release_stagger_min_minutes BETWEEN 0 AND 315),
			five_hour_release_stagger_max_minutes INTEGER NOT NULL DEFAULT 30 CHECK (five_hour_release_stagger_max_minutes BETWEEN 0 AND 315),
			capacity_queue_enabled INTEGER NOT NULL DEFAULT 0,
			capacity_queue_timeout_seconds INTEGER NOT NULL DEFAULT 30 CHECK (capacity_queue_timeout_seconds BETWEEN 1 AND 600),
			strategy_required_enabled INTEGER NOT NULL DEFAULT 0,
			strategy_id INTEGER REFERENCES dispatch_strategies(id) ON DELETE SET NULL,
			reserve_pool_enabled INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`INSERT INTO groups_dynamic (id, name, description, rate_multiplier, daily_limit_usd, monthly_limit_usd, normal_request_mode, claude_code_identity_enabled, stream_hedge_enabled, adaptive_hedge_enabled, rpm_dispatch_enabled, mcp_tool_names_enabled, service_tier_passthrough_enabled, inference_geo_passthrough_enabled, speed_passthrough_enabled, anthropic_beta_passthrough_enabled, reject_anthropic_downgrade_enabled, reject_distillation_enabled, request_format_filter_enabled, quota_header_masking_enabled, cache_creation_detail_enabled, overload_cooldown_seconds, rate_limit_downweight_enabled, rate_limit_cooling_threshold, rate_limit_wait_seconds, rate_limit_stepped_cooldown_enabled, rate_limit_cooldown_step_seconds, rate_limit_downweight_stepped_cooldown_enabled, rate_limit_downweight_base_minutes, rate_limit_downweight_step_minutes, five_hour_release_stagger_enabled, five_hour_release_stagger_min_minutes, five_hour_release_stagger_max_minutes, capacity_queue_enabled, capacity_queue_timeout_seconds, strategy_required_enabled, strategy_id, reserve_pool_enabled, status, created_at, updated_at)
		 SELECT id, name, description, rate_multiplier, daily_limit_usd, monthly_limit_usd, normal_request_mode, claude_code_identity_enabled, stream_hedge_enabled, adaptive_hedge_enabled, rpm_dispatch_enabled, mcp_tool_names_enabled, service_tier_passthrough_enabled, inference_geo_passthrough_enabled, speed_passthrough_enabled, anthropic_beta_passthrough_enabled, reject_anthropic_downgrade_enabled, reject_distillation_enabled, request_format_filter_enabled, quota_header_masking_enabled, cache_creation_detail_enabled, overload_cooldown_seconds, rate_limit_downweight_enabled, rate_limit_cooling_threshold, rate_limit_wait_seconds, rate_limit_stepped_cooldown_enabled, rate_limit_cooldown_step_seconds, rate_limit_downweight_stepped_cooldown_enabled, rate_limit_downweight_base_minutes, rate_limit_downweight_step_minutes, five_hour_release_stagger_enabled, five_hour_release_stagger_min_minutes, five_hour_release_stagger_max_minutes, capacity_queue_enabled, capacity_queue_timeout_seconds, strategy_required_enabled, strategy_id, reserve_pool_enabled, status, created_at, updated_at FROM groups`,
		`DROP TABLE groups`,
		`ALTER TABLE groups_dynamic RENAME TO groups`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate dynamic groups: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dynamic groups migration: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("restore foreign keys after groups migration: %w", err)
	}
	rows, err := conn.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("check groups migration foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("groups migration left invalid foreign key references")
	}
	return rows.Err()
}

func (a *app) backfillAccountSubscriptions() error {
	rows, err := a.db.Query(`SELECT id, credentials_json FROM accounts WHERE (subscription_type = '' OR rate_limit_tier = '') AND credentials_json != '{}'`)
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
		credentials := decodeObject(item.credentials)
		subscription := subscriptionTypeFromCredentials(credentials)
		rateLimitTier := rateLimitTierFromCredentials(credentials)
		if subscription == "" && rateLimitTier == "" {
			continue
		}
		if _, err := a.db.Exec(`UPDATE accounts SET subscription_type = CASE WHEN subscription_type = '' THEN ? ELSE subscription_type END, rate_limit_tier = CASE WHEN rate_limit_tier = '' THEN ? ELSE rate_limit_tier END WHERE id = ?`, subscription, rateLimitTier, item.id); err != nil {
			return fmt.Errorf("backfill account subscription: %w", err)
		}
	}
	return nil
}

func (a *app) handleAccountSummary(w http.ResponseWriter, r *http.Request) {
	where := []string{"a.deleted_at IS NULL", "a.archived_at IS NULL"}
	whereArgs := []any{}
	if user := currentUser(r); user.Role == "user" {
		condition, args := scopedAccountCondition(user, "a")
		where = append(where, condition)
		whereArgs = append(whereArgs, args...)
	}
	if groupID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))); groupIDPattern.MatchString(groupID) {
		where = append(where, `EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = ?)`)
		whereArgs = append(whereArgs, groupID)
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		where = append(where, accountStatePredicate("a", status))
	}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		where = append(where, `(CAST(a.id AS CHAR) LIKE ? OR a.name LIKE ? OR a.notes LIKE ? OR a.credential_hint LIKE ? OR a.source_sk_hint LIKE ? OR EXISTS (
			SELECT 1 FROM proxies p_search
			WHERE p_search.id IN (a.proxy_id, a.archived_proxy_id)
			AND (p_search.name LIKE ? OR p_search.host LIKE ? OR p_search.exit_ip LIKE ?)
		))`)
		term := "%" + search + "%"
		whereArgs = append(whereArgs, term, term, term, term, term, term, term, term)
	}
	quotaConditions, quotaArgs, err := accountQuotaFilterConditions(r, "a")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	where = append(where, quotaConditions...)
	whereArgs = append(whereArgs, quotaArgs...)
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
