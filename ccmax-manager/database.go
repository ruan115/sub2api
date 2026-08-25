package main

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type databaseDialect string

const (
	dialectSQLite databaseDialect = "sqlite"
	dialectMySQL  databaseDialect = "mysql"
)

type database struct {
	*sql.DB
	dialect databaseDialect
}

type databaseTx struct {
	*sql.Tx
	dialect databaseDialect
}

func (db *database) Exec(query string, args ...any) (sql.Result, error) {
	return db.DB.Exec(db.query(query), db.args(args)...)
}

func (db *database) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.DB.ExecContext(ctx, db.query(query), db.args(args)...)
}

func (db *database) Query(query string, args ...any) (*sql.Rows, error) {
	return db.DB.Query(db.query(query), db.args(args)...)
}

func (db *database) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.DB.QueryContext(ctx, db.query(query), db.args(args)...)
}

func (db *database) QueryRow(query string, args ...any) *sql.Row {
	return db.DB.QueryRow(db.query(query), db.args(args)...)
}

func (db *database) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.DB.QueryRowContext(ctx, db.query(query), db.args(args)...)
}

func (db *database) Begin() (*databaseTx, error) {
	tx, err := db.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &databaseTx{Tx: tx, dialect: db.dialect}, nil
}

func (db *database) BeginTx(ctx context.Context, opts *sql.TxOptions) (*databaseTx, error) {
	tx, err := db.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &databaseTx{Tx: tx, dialect: db.dialect}, nil
}

func (tx *databaseTx) Exec(query string, args ...any) (sql.Result, error) {
	return tx.Tx.Exec(rewriteQuery(tx.dialect, query), normalizeQueryArgs(tx.dialect, args)...)
}

func (tx *databaseTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.Tx.ExecContext(ctx, rewriteQuery(tx.dialect, query), normalizeQueryArgs(tx.dialect, args)...)
}

func (tx *databaseTx) Query(query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.Query(rewriteQuery(tx.dialect, query), normalizeQueryArgs(tx.dialect, args)...)
}

func (tx *databaseTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return tx.Tx.QueryContext(ctx, rewriteQuery(tx.dialect, query), normalizeQueryArgs(tx.dialect, args)...)
}

func (tx *databaseTx) QueryRow(query string, args ...any) *sql.Row {
	return tx.Tx.QueryRow(rewriteQuery(tx.dialect, query), normalizeQueryArgs(tx.dialect, args)...)
}

func (tx *databaseTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.Tx.QueryRowContext(ctx, rewriteQuery(tx.dialect, query), normalizeQueryArgs(tx.dialect, args)...)
}

func (db *database) query(query string) string {
	return rewriteQuery(db.dialect, query)
}

func (db *database) args(args []any) []any {
	return normalizeQueryArgs(db.dialect, args)
}

func normalizeQueryArgs(dialect databaseDialect, args []any) []any {
	if dialect != dialectMySQL || len(args) == 0 {
		return args
	}
	normalized := make([]any, len(args))
	for index, value := range args {
		normalized[index] = value
		text, ok := value.(string)
		if !ok || text == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			normalized[index] = parsed.UTC()
		}
	}
	return normalized
}

var mysqlGroupConcatPattern = regexp.MustCompile(`GROUP_CONCAT\(([a-zA-Z0-9_.]+),\s*'([^']*)'\)`)

var mysqlGroupsTablePattern = regexp.MustCompile(`(?i)\b(FROM|JOIN|INTO|UPDATE)\s+groups\b`)

var mysqlRPMThresholdUpsertPattern = regexp.MustCompile(`ON CONFLICT\(account_id\)\s+DO UPDATE SET\s+rpm_limit = LEAST\(account_rpm_thresholds\.rpm_limit, VALUES\(rpm_limit\)\),\s+reset_at = GREATEST\(account_rpm_thresholds\.reset_at, VALUES\(reset_at\)\),\s+updated_at = UTC_TIMESTAMP\(3\)`)

var mysqlProxyHistoryUpsertPattern = regexp.MustCompile(`ON CONFLICT\(proxy_id, account_id\)\s+DO UPDATE SET\s+first_bound_at = LEAST\(proxy_account_history\.first_bound_at, VALUES\(first_bound_at\)\),\s+last_bound_at = GREATEST\(proxy_account_history\.last_bound_at, VALUES\(last_bound_at\)\),\s+bind_count = proxy_account_history\.bind_count \+ excluded\.bind_count`)

func rewriteQuery(dialect databaseDialect, query string) string {
	if dialect != dialectMySQL {
		return query
	}

	replacements := []struct {
		old string
		new string
	}{
		{"INSERT OR IGNORE", "INSERT IGNORE"},
		{"MAX(0, CAST(strftime('%s', invalidated_at) AS INTEGER) - CAST(strftime('%s', onboarded_at) AS INTEGER))", "GREATEST(0, TIMESTAMPDIFF(SECOND, onboarded_at, invalidated_at))"},
		{"MAX(0, CAST(strftime('%s','now') AS INTEGER) - CAST(strftime('%s', onboarded_at) AS INTEGER))", "GREATEST(0, TIMESTAMPDIFF(SECOND, onboarded_at, UTC_TIMESTAMP(3)))"},
		{"strftime('%Y-%m-%dT%H:%M:%fZ','now','-90 days')", "(UTC_TIMESTAMP(3) - INTERVAL 90 DAY)"},
		{"strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')", "(UTC_TIMESTAMP(3) - INTERVAL 60 SECOND)"},
		{"strftime('%Y-%m-%dT%H:%M:%fZ','now','-1 second')", "(UTC_TIMESTAMP(3) - INTERVAL 1 SECOND)"},
		{"strftime('%Y-%m-%dT%H:%M:%fZ','now')", "UTC_TIMESTAMP(3)"},
		{"CAST(strftime('%s','now') AS INTEGER)", "UNIX_TIMESTAMP(UTC_TIMESTAMP(3))"},
		{"COALESCE(json_extract(credentials_json, '$.refresh_token'), '')", "COALESCE(JSON_UNQUOTE(JSON_EXTRACT(credentials_json, '$.refresh_token')), '')"},
		{"CASE WHEN COALESCE(px.id, archived_px.id) IS NULL THEN '' ELSE COALESCE(px.protocol, archived_px.protocol) || '://' || COALESCE(px.host, archived_px.host) || ':' || COALESCE(px.port, archived_px.port) END", "CASE WHEN COALESCE(px.id, archived_px.id) IS NULL THEN '' ELSE CONCAT(COALESCE(px.protocol, archived_px.protocol), '://', COALESCE(px.host, archived_px.host), ':', COALESCE(px.port, archived_px.port)) END"},
		{"MIN(account_rpm_thresholds.rpm_limit, excluded.rpm_limit)", "LEAST(account_rpm_thresholds.rpm_limit, VALUES(rpm_limit))"},
		{"MAX(account_rpm_thresholds.reset_at, excluded.reset_at)", "GREATEST(account_rpm_thresholds.reset_at, VALUES(reset_at))"},
		{"MIN(proxy_account_history.first_bound_at, excluded.first_bound_at)", "LEAST(proxy_account_history.first_bound_at, VALUES(first_bound_at))"},
		{"MAX(proxy_account_history.last_bound_at, excluded.last_bound_at)", "GREATEST(proxy_account_history.last_bound_at, VALUES(last_bound_at))"},
	}
	for _, replacement := range replacements {
		query = strings.ReplaceAll(query, replacement.old, replacement.new)
	}
	query = strings.ReplaceAll(query, "p.key", "p.`key`")
	query = strings.ReplaceAll(query, "purposes (key,", "purposes (`key`,")
	query = strings.ReplaceAll(query, "purposes SET key =", "purposes SET `key` =")
	query = strings.ReplaceAll(query, "SELECT key FROM purposes", "SELECT `key` FROM purposes")
	query = strings.ReplaceAll(query, "WHERE key =", "WHERE `key` =")
	query = mysqlGroupConcatPattern.ReplaceAllString(query, "GROUP_CONCAT($1 SEPARATOR '$2')")
	query = mysqlGroupsTablePattern.ReplaceAllString(query, "$1 `groups`")

	query = rewriteMySQLUpsert(query)
	return query
}

func rewriteMySQLUpsert(query string) string {
	query = mysqlRPMThresholdUpsertPattern.ReplaceAllString(query, `ON DUPLICATE KEY UPDATE rpm_limit = LEAST(account_rpm_thresholds.rpm_limit, VALUES(rpm_limit)), reset_at = GREATEST(account_rpm_thresholds.reset_at, VALUES(reset_at)), updated_at = UTC_TIMESTAMP(3)`)
	query = mysqlProxyHistoryUpsertPattern.ReplaceAllString(query, `ON DUPLICATE KEY UPDATE first_bound_at = LEAST(proxy_account_history.first_bound_at, VALUES(first_bound_at)), last_bound_at = GREATEST(proxy_account_history.last_bound_at, VALUES(last_bound_at)), bind_count = proxy_account_history.bind_count + VALUES(bind_count)`)
	rules := []struct {
		conflict string
		update   string
	}{
		{
			conflict: "ON CONFLICT(account_id, model) DO UPDATE SET reset_at = excluded.reset_at, updated_at = " + mysqlNowSQL(),
			update:   "ON DUPLICATE KEY UPDATE reset_at = VALUES(reset_at), updated_at = " + mysqlNowSQL(),
		},
		{
			conflict: "ON CONFLICT(account_id) DO UPDATE SET fingerprint_json = excluded.fingerprint_json, updated_at = " + mysqlNowSQL(),
			update:   "ON DUPLICATE KEY UPDATE fingerprint_json = VALUES(fingerprint_json), updated_at = " + mysqlNowSQL(),
		},
		{
			conflict: "ON CONFLICT(account_id) DO UPDATE SET requests = requests + 1",
			update:   "ON DUPLICATE KEY UPDATE requests = requests + 1",
		},
		{
			conflict: "ON CONFLICT(session_hash, api_key_id) DO UPDATE SET account_id = excluded.account_id, expires_at = excluded.expires_at",
			update:   "ON DUPLICATE KEY UPDATE account_id = VALUES(account_id), expires_at = VALUES(expires_at)",
		},
	}
	for _, rule := range rules {
		query = strings.Replace(query, rule.conflict, rule.update, 1)
	}

	manualPriceConflict := "ON CONFLICT(model) DO UPDATE SET input_per_million = excluded.input_per_million, output_per_million = excluded.output_per_million, cache_creation_per_million = excluded.cache_creation_per_million, cache_read_per_million = excluded.cache_read_per_million, source = 'manual', source_hash = '', updated_at = " + mysqlNowSQL()
	manualPriceUpdate := "ON DUPLICATE KEY UPDATE input_per_million = VALUES(input_per_million), output_per_million = VALUES(output_per_million), cache_creation_per_million = VALUES(cache_creation_per_million), cache_read_per_million = VALUES(cache_read_per_million), source = 'manual', source_hash = '', updated_at = " + mysqlNowSQL()
	query = strings.Replace(query, manualPriceConflict, manualPriceUpdate, 1)

	remotePriceConflict := "ON CONFLICT(model) DO UPDATE SET input_per_million = excluded.input_per_million, output_per_million = excluded.output_per_million, cache_creation_per_million = excluded.cache_creation_per_million, cache_read_per_million = excluded.cache_read_per_million, source_hash = excluded.source_hash, updated_at = " + mysqlNowSQL() + " WHERE model_prices.source = 'remote'"
	remotePriceUpdate := "ON DUPLICATE KEY UPDATE input_per_million = IF(source = 'remote', VALUES(input_per_million), input_per_million), output_per_million = IF(source = 'remote', VALUES(output_per_million), output_per_million), cache_creation_per_million = IF(source = 'remote', VALUES(cache_creation_per_million), cache_creation_per_million), cache_read_per_million = IF(source = 'remote', VALUES(cache_read_per_million), cache_read_per_million), source_hash = IF(source = 'remote', VALUES(source_hash), source_hash), updated_at = IF(source = 'remote', " + mysqlNowSQL() + ", updated_at)"
	query = strings.Replace(query, remotePriceConflict, remotePriceUpdate, 1)

	leaseConflict := "ON CONFLICT(account_id) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at, updated_at = " + mysqlNowSQL() + "\n\t\t\tWHERE account_token_leases.expires_at <= UNIX_TIMESTAMP(UTC_TIMESTAMP(3)) OR account_token_leases.owner = excluded.owner"
	leaseUpdate := "ON DUPLICATE KEY UPDATE owner = IF(expires_at <= UNIX_TIMESTAMP(UTC_TIMESTAMP(3)) OR owner = VALUES(owner), VALUES(owner), owner), expires_at = IF(expires_at <= UNIX_TIMESTAMP(UTC_TIMESTAMP(3)) OR owner = VALUES(owner), VALUES(expires_at), expires_at), updated_at = IF(expires_at <= UNIX_TIMESTAMP(UTC_TIMESTAMP(3)) OR owner = VALUES(owner), " + mysqlNowSQL() + ", updated_at)"
	query = strings.Replace(query, leaseConflict, leaseUpdate, 1)

	if strings.Contains(query, "ON CONFLICT") {
		panic(fmt.Sprintf("untranslated SQLite upsert in MySQL query: %s", query))
	}
	return query
}

func mysqlNowSQL() string {
	return "UTC_TIMESTAMP(3)"
}
