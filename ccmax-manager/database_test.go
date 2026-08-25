package main

import (
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
