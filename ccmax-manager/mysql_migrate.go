package main

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

type mysqlMigrationTableReport struct {
	Table        string `json:"table"`
	SourceRows   int64  `json:"source_rows"`
	ImportedRows int64  `json:"imported_rows"`
	TargetRows   int64  `json:"target_rows"`
}

type mysqlMigrationReport struct {
	Tables []mysqlMigrationTableReport `json:"tables"`
}

var mysqlMigrationTables = []string{
	"dispatch_strategies",
	"groups",
	"proxy_pools",
	"proxies",
	"users",
	"accounts",
	"account_groups",
	"purposes",
	"model_prices",
	"panel_sessions",
	"api_keys",
	"feature_migrations",
	"usage_logs",
	"account_fingerprints",
	"proxy_account_history",
	"audit_logs",
	"authorization_logs",
	"gateway_error_logs",
	"account_lifecycle_events",
	"reserve_activation_logs",
	"pricing_sync_state",
}

func migrateSQLiteToMySQL(sourcePath string, target *database, resetTarget bool) (mysqlMigrationReport, error) {
	report := mysqlMigrationReport{Tables: make([]mysqlMigrationTableReport, 0, len(mysqlMigrationTables))}
	if target == nil || target.dialect != dialectMySQL {
		return report, fmt.Errorf("migration target must be MySQL")
	}
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return report, fmt.Errorf("resolve SQLite migration source: %w", err)
	}
	source, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(sourcePath)+"?mode=ro&_foreign_keys=on")
	if err != nil {
		return report, fmt.Errorf("open SQLite migration source: %w", err)
	}
	defer source.Close()
	if err := source.Ping(); err != nil {
		return report, fmt.Errorf("ping SQLite migration source: %w", err)
	}

	if !resetTarget {
		var rows int64
		if err := target.DB.QueryRow(`SELECT
			(SELECT COUNT(*) FROM accounts) +
			(SELECT COUNT(*) FROM users) +
			(SELECT COUNT(*) FROM usage_logs)`).Scan(&rows); err != nil {
			return report, fmt.Errorf("inspect MySQL migration target: %w", err)
		}
		if rows > 0 {
			return report, fmt.Errorf("MySQL migration target contains %d durable rows; set CCMAX_MIGRATE_RESET_TARGET=1 only for a disposable shadow database", rows)
		}
	}
	if err := resetMySQLMigrationTarget(target); err != nil {
		return report, err
	}

	for _, table := range mysqlMigrationTables {
		exists, err := sqliteTableExists(source, table)
		if err != nil {
			return report, err
		}
		if !exists {
			continue
		}
		columns, err := commonMigrationColumns(source, target.DB, table)
		if err != nil {
			return report, err
		}
		if len(columns) == 0 {
			return report, fmt.Errorf("no common columns for migration table %s", table)
		}
		sourceRows, importedRows, err := copyMigrationTable(source, target, table, columns)
		if err != nil {
			return report, err
		}
		var targetRows int64
		if err := target.DB.QueryRow("SELECT COUNT(*) FROM " + quoteIdentifier(table)).Scan(&targetRows); err != nil {
			return report, fmt.Errorf("count migrated MySQL table %s: %w", table, err)
		}
		if sourceRows != importedRows || sourceRows != targetRows {
			return report, fmt.Errorf("migration row count mismatch for %s: source=%d imported=%d target=%d", table, sourceRows, importedRows, targetRows)
		}
		report.Tables = append(report.Tables, mysqlMigrationTableReport{
			Table: table, SourceRows: sourceRows, ImportedRows: importedRows, TargetRows: targetRows,
		})
	}
	if err := verifyMySQLMigrationAggregates(source, target.DB); err != nil {
		return report, err
	}
	return report, nil
}

func resetMySQLMigrationTarget(target *database) error {
	conn, err := target.DB.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("reserve MySQL migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		return fmt.Errorf("disable MySQL foreign keys for migration: %w", err)
	}
	defer conn.ExecContext(context.Background(), `SET FOREIGN_KEY_CHECKS = 1`)
	for index := len(mysqlMigrationTables) - 1; index >= 0; index-- {
		if _, err := conn.ExecContext(context.Background(), "TRUNCATE TABLE "+quoteIdentifier(mysqlMigrationTables[index])); err != nil {
			return fmt.Errorf("reset MySQL migration table %s: %w", mysqlMigrationTables[index], err)
		}
	}
	return nil
}

func sqliteTableExists(source *sql.DB, table string) (bool, error) {
	var count int
	if err := source.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
		return false, fmt.Errorf("inspect SQLite table %s: %w", table, err)
	}
	return count > 0, nil
}

func commonMigrationColumns(source *sql.DB, target *sql.DB, table string) ([]string, error) {
	sourceColumns := map[string]bool{}
	sourceColumnOrder := []string{}
	rows, err := source.Query("PRAGMA table_info(" + quoteIdentifier(table) + ")")
	if err != nil {
		return nil, fmt.Errorf("inspect SQLite columns for %s: %w", table, err)
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan SQLite columns for %s: %w", table, err)
		}
		sourceColumns[name] = true
		sourceColumnOrder = append(sourceColumnOrder, name)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close SQLite columns for %s: %w", table, err)
	}

	targetRows, err := target.Query(`SELECT COLUMN_NAME
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COALESCE(GENERATION_EXPRESSION, '') = ''
		ORDER BY ORDINAL_POSITION`, table)
	if err != nil {
		return nil, fmt.Errorf("inspect MySQL columns for %s: %w", table, err)
	}
	defer targetRows.Close()
	columns := []string{}
	targetColumns := map[string]bool{}
	for targetRows.Next() {
		var name string
		if err := targetRows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan MySQL columns for %s: %w", table, err)
		}
		targetColumns[name] = true
		if sourceColumns[name] {
			columns = append(columns, name)
		}
	}
	if err := targetRows.Err(); err != nil {
		return nil, err
	}
	missing := []string{}
	for _, name := range sourceColumnOrder {
		if !targetColumns[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("MySQL migration table %s is missing SQLite columns: %s", table, strings.Join(missing, ", "))
	}
	return columns, nil
}

func copyMigrationTable(source *sql.DB, target *database, table string, columns []string) (int64, int64, error) {
	quotedColumns := make([]string, len(columns))
	for index, column := range columns {
		quotedColumns[index] = quoteIdentifier(column)
	}
	query := "SELECT " + strings.Join(quotedColumns, ", ") + " FROM " + quoteIdentifier(table)
	rows, err := source.Query(query)
	if err != nil {
		return 0, 0, fmt.Errorf("read SQLite migration table %s: %w", table, err)
	}
	defer rows.Close()

	const batchSize = 250
	batch := make([][]any, 0, batchSize)
	var sourceRows, importedRows int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		placeholders := make([]string, len(batch))
		args := make([]any, 0, len(batch)*len(columns))
		rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + ")"
		for index, values := range batch {
			placeholders[index] = rowPlaceholder
			args = append(args, values...)
		}
		statement := "INSERT INTO " + quoteIdentifier(table) + " (" + strings.Join(quotedColumns, ", ") + ") VALUES " + strings.Join(placeholders, ",")
		result, err := target.Exec(statement, args...)
		if err != nil {
			return fmt.Errorf("write MySQL migration table %s: %w", table, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count imported MySQL rows for %s: %w", table, err)
		}
		importedRows += affected
		batch = batch[:0]
		return nil
	}

	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for index := range values {
			destinations[index] = &values[index]
		}
		if err := rows.Scan(destinations...); err != nil {
			return sourceRows, importedRows, fmt.Errorf("scan SQLite migration table %s: %w", table, err)
		}
		for index, value := range values {
			if bytes, ok := value.([]byte); ok {
				value = string(bytes)
			}
			if isMigrationTimeColumn(columns[index]) && value == "" {
				value = nil
			}
			values[index] = value
		}
		batch = append(batch, values)
		sourceRows++
		if len(batch) == batchSize {
			if err := flush(); err != nil {
				return sourceRows, importedRows, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return sourceRows, importedRows, fmt.Errorf("iterate SQLite migration table %s: %w", table, err)
	}
	if err := flush(); err != nil {
		return sourceRows, importedRows, err
	}
	return sourceRows, importedRows, nil
}

func isMigrationTimeColumn(column string) bool {
	return strings.HasSuffix(column, "_at") || column == "created_at" || column == "updated_at" || column == "expires_at" || column == "reset_at"
}

func verifyMySQLMigrationAggregates(source *sql.DB, target *sql.DB) error {
	checks := []string{
		`SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_creation_tokens), 0), COALESCE(SUM(cache_read_tokens), 0), ROUND(COALESCE(SUM(billed_cost), 0), 8), ROUND(COALESCE(SUM(actual_cost), 0), 8) FROM usage_logs`,
		`SELECT COUNT(*), ROUND(COALESCE(SUM(quota_used), 0), 8) FROM api_keys`,
		`SELECT COUNT(*), ROUND(COALESCE(SUM(balance), 0), 8) FROM users`,
	}
	for _, query := range checks {
		sourceValues, err := queryComparableRow(source, query)
		if err != nil {
			return fmt.Errorf("verify SQLite aggregate: %w", err)
		}
		targetValues, err := queryComparableRow(target, query)
		if err != nil {
			return fmt.Errorf("verify MySQL aggregate: %w", err)
		}
		if strings.Join(sourceValues, "|") != strings.Join(targetValues, "|") {
			return fmt.Errorf("migration aggregate mismatch for %q: SQLite=%v MySQL=%v", query, sourceValues, targetValues)
		}
	}
	return nil
}

func queryComparableRow(db *sql.DB, query string) ([]string, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	values := make([]any, len(columns))
	destinations := make([]any, len(columns))
	for index := range values {
		destinations[index] = &values[index]
	}
	if err := rows.Scan(destinations...); err != nil {
		return nil, err
	}
	result := make([]string, len(values))
	for index, value := range values {
		switch typed := value.(type) {
		case []byte:
			result[index] = strings.TrimRight(strings.TrimRight(string(typed), "0"), ".")
		default:
			result[index] = fmt.Sprint(value)
		}
	}
	return result, nil
}

func quoteIdentifier(identifier string) string {
	if identifier == "" || strings.Contains(identifier, "`") {
		panic("invalid SQL identifier")
	}
	return "`" + identifier + "`"
}

func sortedMigrationTables() []string {
	tables := append([]string(nil), mysqlMigrationTables...)
	sort.Strings(tables)
	return tables
}
