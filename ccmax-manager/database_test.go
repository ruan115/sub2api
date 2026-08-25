package main

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestMySQLQueryRewriteCoversRuntimeSQLiteSyntax(t *testing.T) {
	queries := []string{
		`SELECT COUNT(*) FROM account_rpm_events WHERE created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`,
		`SELECT MAX(0, CAST(strftime('%s','now') AS INTEGER) - CAST(strftime('%s', onboarded_at) AS INTEGER)) FROM accounts`,
		`SELECT COALESCE((SELECT GROUP_CONCAT(ag.group_id, ',') FROM account_groups ag), '')`,
		`SELECT CASE WHEN COALESCE(px.id, archived_px.id) IS NULL THEN '' ELSE COALESCE(px.protocol, archived_px.protocol) || '://' || COALESCE(px.host, archived_px.host) || ':' || COALESCE(px.port, archived_px.port) END`,
		`SELECT COALESCE(json_extract(credentials_json, '$.refresh_token'), '') FROM accounts`,
		`INSERT OR IGNORE INTO feature_migrations (name) VALUES ('migration')`,
		`INSERT INTO account_model_cooldowns (account_id, model, reset_at) VALUES (?, ?, ?) ON CONFLICT(account_id, model) DO UPDATE SET reset_at = excluded.reset_at, updated_at = ` + nowSQL,
		`INSERT INTO account_fingerprints (account_id, fingerprint_json) VALUES (?, ?) ON CONFLICT(account_id) DO UPDATE SET fingerprint_json = excluded.fingerprint_json, updated_at = ` + nowSQL,
		`INSERT INTO account_inflight (account_id, requests) VALUES (?, 1) ON CONFLICT(account_id) DO UPDATE SET requests = requests + 1`,
		`INSERT INTO dispatch_sessions (session_hash, api_key_id, account_id, expires_at) VALUES (?, ?, ?, ?) ON CONFLICT(session_hash, api_key_id) DO UPDATE SET account_id = excluded.account_id, expires_at = excluded.expires_at`,
		`INSERT INTO account_rpm_thresholds (account_id, rpm_limit, reset_at) VALUES (?, ?, ?)
			ON CONFLICT(account_id) DO UPDATE SET
				rpm_limit = MIN(account_rpm_thresholds.rpm_limit, excluded.rpm_limit),
				reset_at = MAX(account_rpm_thresholds.reset_at, excluded.reset_at),
				updated_at = ` + nowSQL,
		`INSERT INTO model_prices (model, input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million, source, source_hash) VALUES (?, ?, ?, ?, ?, 'manual', '') ON CONFLICT(model) DO UPDATE SET input_per_million = excluded.input_per_million, output_per_million = excluded.output_per_million, cache_creation_per_million = excluded.cache_creation_per_million, cache_read_per_million = excluded.cache_read_per_million, source = 'manual', source_hash = '', updated_at = ` + nowSQL,
		`SELECT p.id, p.key FROM purposes p WHERE key = ?`,
		// migrateSharedData runs on MySQL too, and there is no MySQL
		// integration suite — this rewrite check is the only guard that its
		// SQLite-flavoured SQL survives translation.
		`INSERT OR IGNORE INTO proxy_pools (id, name, source_type, default_protocol) VALUES (1, 'default', 'manual', 'socks5')`,
		`INSERT OR IGNORE INTO proxy_account_history (proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
			SELECT proxy_id, id, created_at, updated_at, 1 FROM accounts WHERE proxy_id IS NOT NULL`,
		`INSERT OR IGNORE INTO proxy_account_history (proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
			SELECT archived_proxy_id, id, created_at, updated_at, 1 FROM accounts WHERE archived_proxy_id IS NOT NULL`,
		`UPDATE accounts SET survival_seconds_total = MAX(0, CAST(strftime('%s', invalidated_at) AS INTEGER) - CAST(strftime('%s', onboarded_at) AS INTEGER)) WHERE survival_seconds_total = 0 AND onboarded_at IS NOT NULL AND invalidated_at IS NOT NULL`,
		accumulateAccountSurvivalSQL,
		`INSERT INTO proxy_account_history
			(proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
			SELECT ?, account_id, first_bound_at, last_bound_at, bind_count
			FROM proxy_account_history WHERE proxy_id = ?
			ON CONFLICT(proxy_id, account_id) DO UPDATE SET
				first_bound_at = MIN(proxy_account_history.first_bound_at, excluded.first_bound_at),
				last_bound_at = MAX(proxy_account_history.last_bound_at, excluded.last_bound_at),
				bind_count = proxy_account_history.bind_count + excluded.bind_count`,
		// Rate-limit bookkeeping.
		`UPDATE accounts SET rate_limit_reset_at = NULL, rate_limit_window = '', rate_limit_reason = '',
			consecutive_429 = 0, last_429_at = NULL,
			error_message = CASE WHEN auth_error = '' THEN '' ELSE error_message END, updated_at = ` + nowSQL + `
			WHERE rate_limit_reason IN ('429_cooling', 'quota_exhausted')
			AND rate_limit_reset_at IS NOT NULL AND rate_limit_reset_at <= ` + nowSQL,
		`UPDATE accounts SET rate_limit_reason = '', consecutive_429 = 0, last_429_at = NULL,
			rate_limit_downweight_until = NULL, quota_refreshed_at = ` + nowSQL + `,
			error_message = CASE WHEN auth_error = '' THEN '' ELSE error_message END, updated_at = ` + nowSQL + `
			WHERE rate_limit_downweight_until IS NOT NULL AND rate_limit_downweight_until <= ` + nowSQL + `
			AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at <= ` + nowSQL + `)`,
		// Group strategy traffic shares.
		`SELECT ds.id, ds.name,
			COALESCE((SELECT s.weight FROM group_strategy_shares s WHERE s.group_id = ? AND s.strategy_id = ds.id), 0),
			COUNT(DISTINCT a.id),
			COALESCE(SUM((SELECT COUNT(*) FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds'))), 0)
			FROM account_groups ag
			JOIN accounts a ON a.id = ag.account_id AND a.deleted_at IS NULL AND a.archived_at IS NULL
			LEFT JOIN groups g ON g.id = ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, g.strategy_id) AND ds.deleted_at IS NULL
			WHERE ag.group_id = ?
			GROUP BY ds.id, ds.name
			ORDER BY ds.id`,
	}
	for _, query := range queries {
		rewritten := rewriteQuery(dialectMySQL, query)
		for _, forbidden := range []string{"strftime(", "ON CONFLICT", "INSERT OR", " || ", "json_extract", "p.key", "WHERE key ="} {
			if strings.Contains(rewritten, forbidden) {
				t.Fatalf("MySQL query still contains %q:\n%s", forbidden, rewritten)
			}
		}
	}
}

// The account proxy-history triggers are deliberately SQLite-only: MySQL keeps
// the same table current through recordProxyAssignment instead. rewriteQuery has
// no rule for their upsert, so feeding one to MySQL is a bug worth failing loudly.
func TestProxyHistoryTriggerUpsertIsSQLiteOnly(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("trigger upsert was translated instead of rejected")
		}
	}()
	rewriteQuery(dialectMySQL, `INSERT INTO proxy_account_history (proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
		VALUES (NEW.proxy_id, NEW.id, NEW.created_at, NEW.updated_at, 1)
		ON CONFLICT(proxy_id, account_id) DO UPDATE SET
			last_bound_at = excluded.last_bound_at,
			bind_count = proxy_account_history.bind_count + 1`)
}

func TestSQLiteLockErrorsAreRetryable(t *testing.T) {
	for _, message := range []string{
		"database is locked",
		"database table is locked: proxies",
		"database is busy",
	} {
		if !isSQLiteLockError(errors.New(message)) {
			t.Fatalf("%q was not classified as a SQLite lock error", message)
		}
	}
	if isSQLiteLockError(errors.New("constraint failed")) {
		t.Fatal("non-lock database error was classified as retryable")
	}
}

func TestSQLiteWriteRetriesUntilLockClears(t *testing.T) {
	attempts := 0
	_, err := retryDatabaseWrite(t.Context(), dialectSQLite, func() (sql.Result, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("database is locked")
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("write attempts = %d, want 3", attempts)
	}
}

func TestMySQLQueryArgumentsNormalizeRFC3339Times(t *testing.T) {
	const value = "2026-08-26T01:02:03.456Z"
	args := normalizeQueryArgs(dialectMySQL, []any{value, "not-a-time", int64(7)})
	parsed, ok := args[0].(time.Time)
	if !ok || parsed.Location() != time.UTC || parsed.Format(time.RFC3339Nano) != value {
		t.Fatalf("normalized timestamp = %#v", args[0])
	}
	if args[1] != "not-a-time" || args[2] != int64(7) {
		t.Fatalf("non-time arguments changed: %#v", args)
	}
}

func TestMySQLQueryRewriteQuotesGroupsTable(t *testing.T) {
	input := `SELECT g.id FROM groups g JOIN groups parent ON parent.id = g.id WHERE g.id IN (SELECT group_id FROM account_groups)`
	want := "SELECT g.id FROM `groups` g JOIN `groups` parent ON parent.id = g.id WHERE g.id IN (SELECT group_id FROM account_groups)"
	if got := rewriteQuery(dialectMySQL, input); got != want {
		t.Fatalf("rewriteQuery() = %q, want %q", got, want)
	}

	alreadyQuoted := "INSERT INTO `groups` (id) VALUES (?)"
	if got := rewriteQuery(dialectMySQL, alreadyQuoted); got != alreadyQuoted {
		t.Fatalf("rewriteQuery() changed an already quoted identifier: %q", got)
	}
}

func TestMySQLQueryRewriteErrorLogConcatenations(t *testing.T) {
	input := `SELECT (',' || group_ids || ','), al.action || ' · ' || al.method || ' ' || al.path || ' 返回 HTTP ' || al.status_code FROM groups`
	want := "SELECT CONCAT(',', group_ids, ','), CONCAT(al.action, ' · ', al.method, ' ', al.path, ' 返回 HTTP ', al.status_code) FROM `groups`"
	if got := rewriteQuery(dialectMySQL, input); got != want {
		t.Fatalf("rewriteQuery() = %q, want %q", got, want)
	}
}

func TestMySQLQueryRewriteAccountLimitResetDifference(t *testing.T) {
	input := `SELECT ABS(strftime('%s', a.rate_limit_reset_at) - strftime('%s', a.quota_5h_reset_at)) FROM accounts a`
	want := `SELECT ABS(TIMESTAMPDIFF(SECOND, a.rate_limit_reset_at, a.quota_5h_reset_at)) FROM accounts a`
	if got := rewriteQuery(dialectMySQL, input); got != want {
		t.Fatalf("rewriteQuery() = %q, want %q", got, want)
	}
}
