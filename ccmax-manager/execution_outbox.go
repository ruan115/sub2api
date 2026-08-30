package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	errRuntimeOutboxBusy       = errors.New("runtime outbox checkpoint is leased by another consumer")
	errRuntimeOutboxNotClaimed = errors.New("runtime outbox event is not claimed by this consumer")
	errRuntimeMigration        = errors.New("invalid runtime migration transition")
	errRuntimePlaintext        = errors.New("migrated runtime account still contains plaintext credentials")
	errRuntimeCredentialOwner  = errors.New("account credentials are owned by the execution plane")
	errRuntimeRoutingOwner     = errors.New("account runtime routing is owned by the execution plane")
)

var runtimeAccountTransitionEventTypes = map[string]bool{
	"account.runtime.provision_requested":  true,
	"account.runtime.drain_requested":      true,
	"account.runtime.destroy_requested":    true,
	"account.runtime.restore_requested":    true,
	"account.credential.migrate_requested": true,
	"account.credential.rotate_requested":  true,
	"account.proxy.change_requested":       true,
}

var runtimeOnboardingEventTypes = map[string]bool{
	"account.runtime.provision_requested":  true,
	"account.credential.migrate_requested": true,
	"account.credential.rotate_requested":  true,
}

var runtimeAuthorityEventTypes = map[string]bool{
	"account.proxy_reservation.granted": true,
	"account.proxy_reservation.revoked": true,
}

func validRuntimeOutboxEventType(eventType string) bool {
	return runtimeAccountTransitionEventTypes[eventType] || runtimeAuthorityEventTypes[eventType]
}

var runtimeOpaqueIntentIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type runtimeOutboxEvent struct {
	Sequence          int64
	EventID           string
	AccountID         int64
	EventType         string
	DesiredGeneration uint64
	PayloadJSON       string
	CreatedAt         string
}

type runtimeTransitionRequest struct {
	AccountID           int64
	EventType           string
	MigrationStatus     string
	RuntimeStatus       string
	RuntimeError        string
	ExpectedGeneration  uint64
	ExpectedProxyID     int64
	OnboardingKey       string
	OnboardingIntakeKey string
	OnboardingAttempt   uint64
	OnboardingIntentID  string
	OnboardingExpiresAt time.Time
	Payload             map[string]any
}

type runtimeOnboardingIntentReceipt struct {
	IdempotencyKey    string
	IntentID          string
	AccountID         int64
	DesiredGeneration uint64
	ExpiresAt         time.Time
}

type accountModeHealth struct {
	AccountID int64  `json:"account_id"`
	Mode      string `json:"mode"`
	Status    string `json:"status"`
	ErrorCode string `json:"error_code"`
	RecoverAt string `json:"recover_at"`
	UpdatedAt string `json:"updated_at"`
}

func (a *app) migrateExecutionFeatures() error {
	if a.db.dialect == dialectMySQL {
		for _, column := range []struct{ table, name, definition string }{
			{"groups", "execution_policy", "VARCHAR(16) NOT NULL DEFAULT 'auto'"},
			{"groups", "worker_queue_mode", "VARCHAR(16) NOT NULL DEFAULT 'queue'"},
			{"groups", "worker_image_channel", "VARCHAR(64) NOT NULL DEFAULT 'stable'"},
			{"accounts", "execution_allowed_modes", `LONGTEXT NOT NULL DEFAULT ('["cli_native","oauth_api"]')`},
			{"accounts", "execution_preferred_mode", "VARCHAR(32) NOT NULL DEFAULT 'cli_native'"},
			{"accounts", "execution_migration_status", "VARCHAR(32) NOT NULL DEFAULT 'legacy'"},
			{"accounts", "runtime_status", "VARCHAR(32) NOT NULL DEFAULT 'legacy'"},
			{"accounts", "runtime_error_code", "VARCHAR(64) NOT NULL DEFAULT ''"},
			{"accounts", "runtime_generation", "BIGINT UNSIGNED NOT NULL DEFAULT 0"},
			{"accounts", "runtime_slot_id", "VARCHAR(128) NOT NULL DEFAULT ''"},
			{"accounts", "runtime_provider", "VARCHAR(32) NOT NULL DEFAULT ''"},
			{"accounts", "runtime_execution_epoch", "BIGINT UNSIGNED NOT NULL DEFAULT 0"},
			{"accounts", "cli_native_limit", "INT UNSIGNED NOT NULL DEFAULT 1"},
			{"accounts", "oauth_api_limit", "INT UNSIGNED NOT NULL DEFAULT 3"},
			{"accounts", "execution_total_limit", "INT UNSIGNED NOT NULL DEFAULT 3"},
		} {
			if err := ensureMySQLColumn(a.db.DB, column.table, column.name, column.definition); err != nil {
				return err
			}
		}
		for _, statement := range mysqlExecutionSchema() {
			if _, err := a.db.DB.Exec(statement); err != nil {
				return fmt.Errorf("migrate MySQL execution feature: %w", err)
			}
		}
		for _, column := range []struct{ name, definition string }{
			{"intake_idempotency_key", "VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT ''"},
			{"intake_attempt", "BIGINT UNSIGNED NOT NULL DEFAULT 0"},
			{"intent_expires_at_millis", "BIGINT NOT NULL DEFAULT 0"},
			{"request_fingerprint_version", "SMALLINT UNSIGNED NOT NULL DEFAULT 0"},
			{"request_fingerprint_sha256", "VARBINARY(32) NULL"},
		} {
			if err := ensureMySQLColumn(a.db.DB, "runtime_onboarding_submissions", column.name, column.definition); err != nil {
				return err
			}
		}
		if _, err := a.db.DB.Exec(`UPDATE runtime_onboarding_submissions
			SET intake_idempotency_key = idempotency_key WHERE intake_idempotency_key = ''`); err != nil {
			return fmt.Errorf("backfill MySQL runtime onboarding intake key: %w", err)
		}
		if err := ensureMySQLRuntimeOnboardingSubmissionSchema(a.db.DB); err != nil {
			return err
		}
		return ensureMySQLIndex(a.db.DB, "accounts", "idx_accounts_execution_dispatch", "`execution_migration_status`, `runtime_status`, `status`, `schedulable`, `priority`")
	}

	for _, column := range []struct{ table, name, definition string }{
		{"groups", "execution_policy", "TEXT NOT NULL DEFAULT 'auto'"},
		{"groups", "worker_queue_mode", "TEXT NOT NULL DEFAULT 'queue'"},
		{"groups", "worker_image_channel", "TEXT NOT NULL DEFAULT 'stable'"},
		{"accounts", "execution_allowed_modes", `TEXT NOT NULL DEFAULT '["cli_native","oauth_api"]'`},
		{"accounts", "execution_preferred_mode", "TEXT NOT NULL DEFAULT 'cli_native'"},
		{"accounts", "execution_migration_status", "TEXT NOT NULL DEFAULT 'legacy'"},
		{"accounts", "runtime_status", "TEXT NOT NULL DEFAULT 'legacy'"},
		{"accounts", "runtime_error_code", "TEXT NOT NULL DEFAULT ''"},
		{"accounts", "runtime_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "runtime_slot_id", "TEXT NOT NULL DEFAULT ''"},
		{"accounts", "runtime_provider", "TEXT NOT NULL DEFAULT ''"},
		{"accounts", "runtime_execution_epoch", "INTEGER NOT NULL DEFAULT 0"},
		{"accounts", "cli_native_limit", "INTEGER NOT NULL DEFAULT 1"},
		{"accounts", "oauth_api_limit", "INTEGER NOT NULL DEFAULT 3"},
		{"accounts", "execution_total_limit", "INTEGER NOT NULL DEFAULT 3"},
	} {
		if err := addColumnIfMissing(a.db, column.table, column.name, column.definition); err != nil {
			return fmt.Errorf("add execution column %s.%s: %w", column.table, column.name, err)
		}
	}
	for _, statement := range sqliteExecutionSchema() {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate SQLite execution feature: %w", err)
		}
	}
	if err := a.migrateSQLiteRuntimeProxyReservationAccountFK(); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"intake_idempotency_key", "TEXT NOT NULL DEFAULT ''"},
		{"intake_attempt", "INTEGER NOT NULL DEFAULT 0"},
		{"intent_expires_at_millis", "INTEGER NOT NULL DEFAULT 0"},
		{"request_fingerprint_version", "INTEGER NOT NULL DEFAULT 0"},
		{"request_fingerprint_sha256", "BLOB"},
	} {
		if err := addColumnIfMissing(a.db, "runtime_onboarding_submissions", column.name, column.definition); err != nil {
			return fmt.Errorf("add runtime onboarding submission column %s: %w", column.name, err)
		}
	}
	if _, err := a.db.Exec(`UPDATE runtime_onboarding_submissions
		SET intake_idempotency_key = idempotency_key WHERE intake_idempotency_key = ''`); err != nil {
		return fmt.Errorf("backfill SQLite runtime onboarding intake key: %w", err)
	}
	if _, err := a.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS uq_runtime_onboarding_submission_intake_key
		ON runtime_onboarding_submissions(intake_idempotency_key)`); err != nil {
		return fmt.Errorf("enforce SQLite runtime onboarding intake-key uniqueness: %w", err)
	}
	return nil
}

func (a *app) migrateSQLiteRuntimeProxyReservationAccountFK() error {
	if a == nil || a.db == nil || a.db.dialect != dialectSQLite {
		return nil
	}
	var schema string
	if err := a.db.QueryRow(`SELECT COALESCE(sql, '') FROM sqlite_master
		WHERE type = 'table' AND name = 'runtime_proxy_reservations'`).Scan(&schema); err != nil {
		return fmt.Errorf("inspect runtime proxy reservation schema: %w", err)
	}
	legacy := "account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE"
	if !strings.Contains(schema, legacy) {
		return nil
	}
	rewritten := strings.Replace(schema, legacy, "account_id INTEGER NOT NULL REFERENCES accounts(id)", 1)
	if err := a.rebuildTableWithSchema("runtime_proxy_reservations", rewritten); err != nil {
		return fmt.Errorf("remove cascading runtime proxy reservation account foreign key: %w", err)
	}
	if _, err := a.db.Exec(`CREATE INDEX IF NOT EXISTS idx_runtime_proxy_reservations_proxy_status
		ON runtime_proxy_reservations(proxy_id, status)`); err != nil {
		return fmt.Errorf("restore runtime proxy reservation proxy-status index: %w", err)
	}
	return nil
}

func sqliteExecutionSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS account_mode_health (
			account_id INTEGER NOT NULL REFERENCES accounts(id),
			mode TEXT NOT NULL,
			status TEXT NOT NULL,
			error_code TEXT NOT NULL DEFAULT '',
			recover_at TEXT,
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			PRIMARY KEY (account_id, mode)
		)`,
		`CREATE TABLE IF NOT EXISTS runtime_outbox (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL UNIQUE,
			account_id INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			desired_generation INTEGER NOT NULL,
			payload_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_outbox_account_generation ON runtime_outbox(account_id, desired_generation, sequence)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_outbox_created ON runtime_outbox(created_at, sequence)`,
		`CREATE TABLE IF NOT EXISTS runtime_proxy_reservations (
			reservation_id TEXT PRIMARY KEY,
			account_id INTEGER NOT NULL REFERENCES accounts(id),
			proxy_id INTEGER NOT NULL REFERENCES proxies(id),
			desired_generation INTEGER NOT NULL,
			binding_revision INTEGER NOT NULL,
			grant_event_id TEXT NOT NULL UNIQUE,
			revoke_event_id TEXT UNIQUE,
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
			revoked_at TEXT,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			UNIQUE (account_id, desired_generation),
			CHECK ((status = 'active' AND revoke_event_id IS NULL AND revoked_at IS NULL) OR
				(status = 'revoked' AND revoke_event_id IS NOT NULL AND revoked_at IS NOT NULL))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_proxy_reservations_proxy_status ON runtime_proxy_reservations(proxy_id, status)`,
		`CREATE TABLE IF NOT EXISTS runtime_outbox_consumers (
			consumer_name TEXT PRIMARY KEY,
			last_sequence INTEGER NOT NULL DEFAULT 0,
			claimed_sequence INTEGER NOT NULL DEFAULT 0,
			locked_by TEXT NOT NULL DEFAULT '',
			lease_expires_at INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE TABLE IF NOT EXISTS runtime_onboarding_result_cursors (
			cursor_name TEXT PRIMARY KEY,
			last_sequence INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE TABLE IF NOT EXISTS runtime_onboarding_submissions (
			idempotency_key TEXT PRIMARY KEY,
			intake_idempotency_key TEXT NOT NULL DEFAULT '',
			intake_attempt INTEGER NOT NULL DEFAULT 0,
			operation_type TEXT NOT NULL,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			desired_generation INTEGER NOT NULL,
			event_type TEXT NOT NULL,
			migration_status TEXT NOT NULL,
			source_type TEXT NOT NULL,
			auth_type TEXT NOT NULL,
			proxy_id INTEGER REFERENCES proxies(id),
			status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'queued')),
			intent_id TEXT NOT NULL DEFAULT '',
			intent_expires_at_millis INTEGER NOT NULL DEFAULT 0,
			event_id TEXT NOT NULL DEFAULT '',
			request_fingerprint_version INTEGER NOT NULL DEFAULT 0,
			request_fingerprint_sha256 BLOB,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_runtime_onboarding_submission_account_generation ON runtime_onboarding_submissions(account_id, desired_generation)`,
		`CREATE TABLE IF NOT EXISTS runtime_operation_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL,
			account_id INTEGER NOT NULL,
			operation TEXT NOT NULL,
			status TEXT NOT NULL,
			error_code TEXT NOT NULL DEFAULT '',
			detail_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_runtime_operation_account ON runtime_operation_audit(account_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_execution_dispatch ON accounts(execution_migration_status, runtime_status, status, schedulable, priority) WHERE deleted_at IS NULL`,
	}
}

func mysqlExecutionSchema() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS account_mode_health (
			account_id BIGINT NOT NULL,
			mode VARCHAR(32) NOT NULL,
			status VARCHAR(32) NOT NULL,
			error_code VARCHAR(64) NOT NULL DEFAULT '',
			recover_at DATETIME(3) NULL,
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			PRIMARY KEY (account_id, mode),
			CONSTRAINT fk_account_mode_health_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS runtime_outbox (
			sequence BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			event_id CHAR(36) NOT NULL,
			account_id BIGINT NOT NULL,
			event_type VARCHAR(96) NOT NULL,
			desired_generation BIGINT UNSIGNED NOT NULL,
			payload_json LONGTEXT NOT NULL DEFAULT ('{}'),
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			UNIQUE KEY uq_runtime_outbox_event (event_id),
			KEY idx_runtime_outbox_account_generation (account_id, desired_generation, sequence),
			KEY idx_runtime_outbox_created (created_at, sequence)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS runtime_proxy_reservations (
			reservation_id VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
			account_id BIGINT NOT NULL,
			proxy_id BIGINT NOT NULL,
			desired_generation BIGINT UNSIGNED NOT NULL,
			binding_revision BIGINT UNSIGNED NOT NULL,
			grant_event_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
			revoke_event_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			revoked_at DATETIME(3) NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			UNIQUE KEY uq_runtime_proxy_reservation_account_generation (account_id, desired_generation),
			UNIQUE KEY uq_runtime_proxy_reservation_grant_event (grant_event_id),
			UNIQUE KEY uq_runtime_proxy_reservation_revoke_event (revoke_event_id),
			KEY idx_runtime_proxy_reservations_proxy_status (proxy_id, status),
			CONSTRAINT fk_runtime_proxy_reservation_account FOREIGN KEY (account_id) REFERENCES accounts(id),
			CONSTRAINT fk_runtime_proxy_reservation_proxy FOREIGN KEY (proxy_id) REFERENCES proxies(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS runtime_outbox_consumers (
			consumer_name VARCHAR(128) NOT NULL PRIMARY KEY,
			last_sequence BIGINT NOT NULL DEFAULT 0,
			claimed_sequence BIGINT NOT NULL DEFAULT 0,
			locked_by VARCHAR(128) NOT NULL DEFAULT '',
			lease_expires_at BIGINT NOT NULL DEFAULT 0,
			last_error VARCHAR(512) NOT NULL DEFAULT '',
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS runtime_onboarding_result_cursors (
			cursor_name VARCHAR(128) NOT NULL PRIMARY KEY,
			last_sequence BIGINT NOT NULL DEFAULT 0,
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS runtime_onboarding_submissions (
			idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
			intake_idempotency_key VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
			intake_attempt BIGINT UNSIGNED NOT NULL DEFAULT 0,
			operation_type VARCHAR(32) NOT NULL,
			account_id BIGINT NOT NULL,
			desired_generation BIGINT UNSIGNED NOT NULL,
			event_type VARCHAR(96) NOT NULL,
			migration_status VARCHAR(32) NOT NULL,
			source_type VARCHAR(32) NOT NULL,
			auth_type VARCHAR(32) NOT NULL,
			proxy_id BIGINT NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'pending',
			intent_id VARCHAR(128) NOT NULL DEFAULT '',
			intent_expires_at_millis BIGINT NOT NULL DEFAULT 0,
			event_id CHAR(36) NOT NULL DEFAULT '',
			request_fingerprint_version SMALLINT UNSIGNED NOT NULL DEFAULT 0,
			request_fingerprint_sha256 VARBINARY(32) NULL,
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			updated_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			UNIQUE KEY uq_runtime_onboarding_submission_intake_key (intake_idempotency_key),
			UNIQUE KEY uq_runtime_onboarding_submission_account_generation (account_id, desired_generation),
			CONSTRAINT fk_runtime_onboarding_submission_account FOREIGN KEY (account_id) REFERENCES accounts(id) ON DELETE CASCADE,
			CONSTRAINT fk_runtime_onboarding_submission_proxy FOREIGN KEY (proxy_id) REFERENCES proxies(id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS runtime_operation_audit (
			id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			event_id CHAR(36) NOT NULL,
			account_id BIGINT NOT NULL,
			operation VARCHAR(96) NOT NULL,
			status VARCHAR(32) NOT NULL,
			error_code VARCHAR(64) NOT NULL DEFAULT '',
			detail_json LONGTEXT NOT NULL DEFAULT ('{}'),
			created_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
			KEY idx_runtime_operation_account (account_id, created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}

func ensureMySQLRuntimeOnboardingSubmissionSchema(db *sql.DB) error {
	if db == nil {
		return errors.New("ensure MySQL runtime onboarding submission schema: nil database")
	}
	if err := ensureMySQLRuntimeOnboardingIdempotencyCollation(db); err != nil {
		return err
	}
	if err := ensureMySQLRuntimeOnboardingIntakeUniqueness(db); err != nil {
		return err
	}
	return ensureMySQLRuntimeOnboardingGenerationUniqueness(db)
}

func ensureMySQLRuntimeOnboardingIdempotencyCollation(db *sql.DB) error {
	ensureColumn := func(column string) error {
		const inspect = `SELECT COALESCE(CHARACTER_SET_NAME, ''), COALESCE(COLLATION_NAME, ''),
		COALESCE(CHARACTER_MAXIMUM_LENGTH, 0), IS_NULLABLE
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'runtime_onboarding_submissions'
		  AND COLUMN_NAME = ?`
		isExpected := func() (bool, error) {
			var characterSet, collation, nullable string
			var maximumLength int64
			if err := db.QueryRow(inspect, column).Scan(&characterSet, &collation, &maximumLength, &nullable); err != nil {
				return false, fmt.Errorf("inspect MySQL runtime onboarding %s: %w", column, err)
			}
			return characterSet == "ascii" && collation == "ascii_bin" && maximumLength == 128 && nullable == "NO", nil
		}
		expected, err := isExpected()
		if err != nil || expected {
			return err
		}
		if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE runtime_onboarding_submissions
			MODIFY COLUMN %s VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL`, column)); err != nil {
			// Concurrent replicas can race during startup DDL. Treat the other
			// replica's completed, exact definition as success; never ignore a
			// conversion failure or a partially upgraded definition.
			if expected, inspectErr := isExpected(); inspectErr == nil && expected {
				return nil
			}
			return fmt.Errorf("enforce MySQL runtime onboarding %s collation: %w", column, err)
		}
		return nil
	}
	if err := ensureColumn("idempotency_key"); err != nil {
		return err
	}
	return ensureColumn("intake_idempotency_key")
}

func ensureMySQLRuntimeOnboardingIntakeUniqueness(db *sql.DB) error {
	const indexName = "uq_runtime_onboarding_submission_intake_key"
	inspect := func() (bool, bool, []string, error) {
		rows, err := db.Query(`SELECT NON_UNIQUE, COLUMN_NAME
			FROM information_schema.STATISTICS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'runtime_onboarding_submissions'
			  AND INDEX_NAME = ?
			ORDER BY SEQ_IN_INDEX`, indexName)
		if err != nil {
			return false, false, nil, fmt.Errorf("inspect MySQL runtime onboarding intake-key index: %w", err)
		}
		defer rows.Close()
		exists := false
		unique := true
		columns := []string{}
		for rows.Next() {
			var nonUnique int
			var column string
			if err := rows.Scan(&nonUnique, &column); err != nil {
				return false, false, nil, fmt.Errorf("scan MySQL runtime onboarding intake-key index: %w", err)
			}
			exists = true
			unique = unique && nonUnique == 0
			columns = append(columns, column)
		}
		if err := rows.Err(); err != nil {
			return false, false, nil, fmt.Errorf("iterate MySQL runtime onboarding intake-key index: %w", err)
		}
		return exists, unique, columns, nil
	}
	isExpected := func() (bool, error) {
		exists, unique, columns, err := inspect()
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
		if !exactMySQLUniqueIndexDefinition(unique, columns, "intake_idempotency_key") {
			return false, fmt.Errorf("MySQL runtime onboarding intake-key index %s has an incompatible definition", indexName)
		}
		return true, nil
	}
	expected, err := isExpected()
	if err != nil || expected {
		return err
	}
	if _, err := db.Exec(`ALTER TABLE runtime_onboarding_submissions
		ADD UNIQUE INDEX uq_runtime_onboarding_submission_intake_key (intake_idempotency_key)`); err != nil {
		if expected, inspectErr := isExpected(); inspectErr == nil && expected {
			return nil
		}
		return fmt.Errorf("enforce MySQL runtime onboarding intake-key uniqueness: %w", err)
	}
	return nil
}

func exactMySQLUniqueIndexDefinition(unique bool, columns []string, expected ...string) bool {
	if !unique || len(columns) != len(expected) {
		return false
	}
	for index := range expected {
		if columns[index] != expected[index] {
			return false
		}
	}
	return true
}

func ensureMySQLRuntimeOnboardingGenerationUniqueness(db *sql.DB) error {
	const indexName = "uq_runtime_onboarding_submission_account_generation"
	inspect := func() (bool, bool, []string, error) {
		rows, err := db.Query(`SELECT NON_UNIQUE, COLUMN_NAME
			FROM information_schema.STATISTICS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'runtime_onboarding_submissions'
			  AND INDEX_NAME = ?
			ORDER BY SEQ_IN_INDEX`, indexName)
		if err != nil {
			return false, false, nil, fmt.Errorf("inspect MySQL runtime onboarding generation index: %w", err)
		}
		defer rows.Close()
		exists := false
		unique := true
		columns := []string{}
		for rows.Next() {
			var nonUnique int
			var column string
			if err := rows.Scan(&nonUnique, &column); err != nil {
				return false, false, nil, fmt.Errorf("scan MySQL runtime onboarding generation index: %w", err)
			}
			exists = true
			unique = unique && nonUnique == 0
			columns = append(columns, column)
		}
		if err := rows.Err(); err != nil {
			return false, false, nil, fmt.Errorf("iterate MySQL runtime onboarding generation index: %w", err)
		}
		return exists, unique, columns, nil
	}
	isExpected := func() (bool, error) {
		exists, unique, columns, err := inspect()
		if err != nil {
			return false, err
		}
		if !exists {
			return false, nil
		}
		if !unique || len(columns) != 2 || columns[0] != "account_id" || columns[1] != "desired_generation" {
			return false, fmt.Errorf("MySQL runtime onboarding generation index %s has an incompatible definition", indexName)
		}
		return true, nil
	}
	expected, err := isExpected()
	if err != nil || expected {
		return err
	}
	var duplicateAccountID int64
	var duplicateGeneration uint64
	err = db.QueryRow(`SELECT account_id, desired_generation
		FROM runtime_onboarding_submissions
		GROUP BY account_id, desired_generation
		HAVING COUNT(*) > 1
		LIMIT 1`).Scan(&duplicateAccountID, &duplicateGeneration)
	if err == nil {
		return fmt.Errorf("cannot enforce MySQL runtime onboarding generation uniqueness: account %d generation %d has duplicate submissions",
			duplicateAccountID, duplicateGeneration)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("inspect MySQL runtime onboarding generation duplicates: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE runtime_onboarding_submissions
		ADD UNIQUE INDEX uq_runtime_onboarding_submission_account_generation (account_id, desired_generation)`); err != nil {
		if expected, inspectErr := isExpected(); inspectErr == nil && expected {
			return nil
		}
		return fmt.Errorf("enforce MySQL runtime onboarding generation uniqueness: %w", err)
	}
	return nil
}

const runtimeOnboardingReceiptCommitMargin = 5 * time.Second

func runtimeOnboardingReceiptHasCommitMargin(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() || now.IsZero() {
		return false
	}
	return !expiresAt.UTC().Before(now.UTC().Add(runtimeOnboardingReceiptCommitMargin))
}

func (a *app) requestRuntimeTransition(ctx context.Context, request runtimeTransitionRequest) (runtimeOutboxEvent, error) {
	if request.AccountID <= 0 || !runtimeAccountTransitionEventTypes[request.EventType] ||
		!validRuntimeStatus(request.RuntimeStatus) || len(request.RuntimeError) > 64 {
		return runtimeOutboxEvent{}, errors.New("invalid runtime transition request")
	}
	if request.OnboardingKey != "" && (!runtimeOpaqueIntentIDPattern.MatchString(request.OnboardingKey) || runtimeSecretString(request.OnboardingKey) ||
		!runtimeOnboardingEventTypes[request.EventType] ||
		!runtimeOpaqueIntentIDPattern.MatchString(request.OnboardingIntakeKey) || runtimeSecretString(request.OnboardingIntakeKey) ||
		request.OnboardingAttempt > runtimeOnboardingMaxIntakeAttempt ||
		!runtimeOpaqueIntentIDPattern.MatchString(request.OnboardingIntentID) || runtimeSecretString(request.OnboardingIntentID) || request.ExpectedProxyID <= 0 ||
		!runtimeOnboardingReceiptHasCommitMargin(request.OnboardingExpiresAt, time.Now())) {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	payload, err := safeRuntimePayload(request.Payload)
	if err != nil {
		return runtimeOutboxEvent{}, err
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("begin runtime transition: %w", err)
	}
	defer tx.Rollback()
	query := `SELECT execution_migration_status, runtime_generation, credentials_json, proxy_id FROM accounts
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`
	if a.db.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var currentMigration, credentialsJSON string
	var currentGeneration uint64
	var currentProxyID sql.NullInt64
	if err := tx.QueryRowContext(ctx, query, request.AccountID).Scan(&currentMigration, &currentGeneration, &credentialsJSON, &currentProxyID); err != nil {
		return runtimeOutboxEvent{}, err
	}
	if request.ExpectedProxyID > 0 && (!currentProxyID.Valid || currentProxyID.Int64 != request.ExpectedProxyID) {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	targetMigration := strings.TrimSpace(request.MigrationStatus)
	if targetMigration == "" {
		targetMigration = currentMigration
	}
	if !allowedMigrationTransition(currentMigration, targetMigration) {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	if targetMigration == "migrated" && !emptyJSONObject(credentialsJSON) {
		return runtimeOutboxEvent{}, errRuntimePlaintext
	}
	nextGeneration := currentGeneration + 1
	if request.ExpectedGeneration != 0 && request.ExpectedGeneration != nextGeneration {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	// Only a successor proxy grant transfers this authority. Ordinary
	// drain/destroy/restore generations retain the durable business reservation
	// for the later lifecycle workflow. Lock and validate the successor binding
	// before revoking the predecessor so the handoff remains fail-closed.
	if request.OnboardingKey != "" {
		if err := validateRuntimeProxyBindingForGrantTx(ctx, tx, request.AccountID, currentProxyID.Int64); err != nil {
			return runtimeOutboxEvent{}, err
		}
		if _, err := revokeActiveRuntimeProxyReservationsTx(ctx, tx, request.AccountID, currentGeneration); err != nil {
			return runtimeOutboxEvent{}, err
		}
	}
	slotID := fmt.Sprintf("ccmax-account-%d", request.AccountID)
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET
		execution_migration_status = ?, runtime_status = ?, runtime_error_code = ?,
		runtime_generation = ?, runtime_slot_id = CASE WHEN runtime_slot_id = '' THEN ? ELSE runtime_slot_id END,
		runtime_provider = CASE WHEN runtime_provider = '' THEN 'docker' ELSE runtime_provider END,
		schedulable = 0, updated_at = `+nowSQL+`
		WHERE id = ? AND runtime_generation = ? AND deleted_at IS NULL AND archived_at IS NULL`,
		targetMigration, request.RuntimeStatus, request.RuntimeError, nextGeneration, slotID, request.AccountID, currentGeneration)
	if err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("update runtime desired state: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	if request.OnboardingKey != "" {
		if _, _, err := ensureRuntimeProxyReservationTx(ctx, tx, request.AccountID, currentProxyID.Int64, nextGeneration); err != nil {
			return runtimeOutboxEvent{}, err
		}
	}
	event, err := enqueueRuntimeEventTx(ctx, tx, runtimeOutboxEvent{
		EventID: newRuntimeEventID(), AccountID: request.AccountID, EventType: request.EventType,
		DesiredGeneration: nextGeneration, PayloadJSON: payload,
	})
	if err != nil {
		return runtimeOutboxEvent{}, err
	}
	if request.OnboardingKey != "" {
		result, err := tx.ExecContext(ctx, `UPDATE runtime_onboarding_submissions SET
			status = 'queued', event_id = ?, updated_at = `+nowSQL+`
			WHERE idempotency_key = ? AND intake_idempotency_key = ? AND intake_attempt = ?
			  AND account_id = ? AND desired_generation = ?
			  AND event_type = ? AND proxy_id = ? AND status = 'pending' AND intent_id = ?
			  AND intent_expires_at_millis = ? AND event_id = ''`,
			event.EventID, request.OnboardingKey, request.OnboardingIntakeKey, request.OnboardingAttempt, request.AccountID,
			event.DesiredGeneration, request.EventType, request.ExpectedProxyID, request.OnboardingIntentID,
			request.OnboardingExpiresAt.UTC().UnixMilli())
		if err != nil {
			return runtimeOutboxEvent{}, fmt.Errorf("complete runtime onboarding submission: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return runtimeOutboxEvent{}, errRuntimeMigration
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_operation_audit
		(event_id, account_id, operation, status, error_code, detail_json)
		VALUES (?, ?, ?, 'requested', ?, ?)`, event.EventID, request.AccountID, request.EventType, request.RuntimeError, payload); err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("record runtime operation audit: %w", err)
	}
	// The receipt is a short-lived execution-plane authorization. Recheck it
	// inside the same transaction immediately before commit so time spent
	// waiting on the account row lock cannot turn an expired receipt into a
	// durable generation change. Five seconds leaves room for commit latency.
	if request.OnboardingKey != "" && !runtimeOnboardingReceiptHasCommitMargin(request.OnboardingExpiresAt, time.Now()) {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	if err := tx.Commit(); err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("commit runtime transition: %w", err)
	}
	return event, nil
}

// requestRuntimeOnboardingTransition is the only CCMAX transition allowed to
// reference onboarding material. The material itself has already crossed the
// mTLS intake boundary and only its opaque intent id may enter CCMAX MySQL.
func (a *app) requestRuntimeOnboardingTransition(
	ctx context.Context,
	request runtimeTransitionRequest,
	receipt runtimeOnboardingIntentReceipt,
) (runtimeOutboxEvent, error) {
	if request.AccountID <= 0 || receipt.AccountID != request.AccountID || receipt.DesiredGeneration == 0 ||
		!runtimeOpaqueIntentIDPattern.MatchString(receipt.IdempotencyKey) || runtimeSecretString(receipt.IdempotencyKey) ||
		!runtimeOpaqueIntentIDPattern.MatchString(receipt.IntentID) || runtimeSecretString(receipt.IntentID) ||
		!runtimeOnboardingReceiptHasCommitMargin(receipt.ExpiresAt, time.Now()) {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	if !runtimeOnboardingEventTypes[request.EventType] {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	externalKey := strings.TrimSpace(request.OnboardingKey)
	if externalKey == "" {
		externalKey = receipt.IdempotencyKey
	}
	submission, err := a.getRuntimeOnboardingSubmission(ctx, externalKey)
	if err != nil || submission.Status != runtimeOnboardingSubmissionPending ||
		submission.AccountID != request.AccountID || submission.DesiredGeneration != receipt.DesiredGeneration ||
		submission.EventType != request.EventType || !submission.ProxyID.Valid || submission.ProxyID.Int64 <= 0 {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	storedReceipt, ok := runtimeOnboardingReceiptFromSubmission(submission)
	if !ok || storedReceipt.IdempotencyKey != receipt.IdempotencyKey || storedReceipt.IntentID != receipt.IntentID ||
		storedReceipt.AccountID != receipt.AccountID || storedReceipt.DesiredGeneration != receipt.DesiredGeneration ||
		storedReceipt.ExpiresAt.UnixMilli() != receipt.ExpiresAt.UTC().UnixMilli() {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	if err := a.validateRuntimeOnboardingSubmissionAccount(ctx, submission); err != nil {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	request.ExpectedGeneration = receipt.DesiredGeneration
	request.ExpectedProxyID = submission.ProxyID.Int64
	request.OnboardingKey = submission.IdempotencyKey
	request.OnboardingIntakeKey = submission.IntakeIdempotencyKey
	request.OnboardingAttempt = submission.IntakeAttempt
	request.OnboardingIntentID = receipt.IntentID
	request.OnboardingExpiresAt = receipt.ExpiresAt
	request.Payload = map[string]any{"onboarding_intent_id": receipt.IntentID}
	return a.requestRuntimeTransition(ctx, request)
}

func enqueueRuntimeEventTx(ctx context.Context, tx *databaseTx, event runtimeOutboxEvent) (runtimeOutboxEvent, error) {
	if ctx == nil || ctx.Err() != nil || tx == nil || !validRuntimeOpaqueID(event.EventID) ||
		event.AccountID <= 0 || !validRuntimeOutboxEventType(event.EventType) || event.DesiredGeneration == 0 {
		return runtimeOutboxEvent{}, errors.New("invalid runtime outbox event")
	}
	payload, err := safeRuntimePayloadJSON(event.PayloadJSON)
	if err != nil {
		return runtimeOutboxEvent{}, err
	}
	event.PayloadJSON = payload
	var currentGeneration uint64
	if err := tx.QueryRowContext(ctx, `SELECT runtime_generation FROM accounts WHERE id = ?`, event.AccountID).Scan(&currentGeneration); err != nil {
		return runtimeOutboxEvent{}, err
	}
	if runtimeAccountTransitionEventTypes[event.EventType] {
		if currentGeneration != event.DesiredGeneration {
			return runtimeOutboxEvent{}, errRuntimeMigration
		}
	} else if err := validateRuntimeProxyAuthorityEventTx(ctx, tx, event, currentGeneration); err != nil {
		return runtimeOutboxEvent{}, err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO runtime_outbox
		(event_id, account_id, event_type, desired_generation, payload_json)
		VALUES (?, ?, ?, ?, ?)`, event.EventID, event.AccountID, event.EventType, event.DesiredGeneration, event.PayloadJSON)
	if err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("insert runtime outbox event: %w", err)
	}
	event.Sequence, err = result.LastInsertId()
	if err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("read runtime outbox sequence: %w", err)
	}
	return event, nil
}

func (a *app) claimRuntimeOutboxEvent(ctx context.Context, consumerName, owner string, now time.Time, leaseTTL time.Duration) (runtimeOutboxEvent, bool, error) {
	if strings.TrimSpace(consumerName) == "" || strings.TrimSpace(owner) == "" || len(consumerName) > 128 || len(owner) > 128 || now.IsZero() || leaseTTL <= 0 {
		return runtimeOutboxEvent{}, false, errors.New("invalid runtime outbox claim")
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return runtimeOutboxEvent{}, false, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runtime_outbox_consumers (consumer_name) VALUES (?)`, consumerName); err != nil {
		return runtimeOutboxEvent{}, false, fmt.Errorf("create runtime outbox checkpoint: %w", err)
	}
	query := `SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at FROM runtime_outbox_consumers WHERE consumer_name = ?`
	if a.db.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var lastSequence, claimedSequence, leaseExpiresAt int64
	var lockedBy string
	if err := tx.QueryRowContext(ctx, query, consumerName).Scan(&lastSequence, &claimedSequence, &lockedBy, &leaseExpiresAt); err != nil {
		return runtimeOutboxEvent{}, false, err
	}
	nowMillis := now.UTC().UnixMilli()
	if lockedBy != "" && lockedBy != owner && leaseExpiresAt > nowMillis {
		return runtimeOutboxEvent{}, false, errRuntimeOutboxBusy
	}
	sequence := claimedSequence
	if sequence <= lastSequence {
		err = tx.QueryRowContext(ctx, `SELECT sequence FROM runtime_outbox WHERE sequence > ? ORDER BY sequence LIMIT 1`, lastSequence).Scan(&sequence)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.Commit(); err != nil {
				return runtimeOutboxEvent{}, false, err
			}
			return runtimeOutboxEvent{}, false, nil
		}
		if err != nil {
			return runtimeOutboxEvent{}, false, err
		}
	}
	var event runtimeOutboxEvent
	if err := tx.QueryRowContext(ctx, `SELECT sequence, event_id, account_id, event_type, desired_generation, payload_json, created_at
		FROM runtime_outbox WHERE sequence = ?`, sequence).Scan(
		&event.Sequence, &event.EventID, &event.AccountID, &event.EventType, &event.DesiredGeneration, &event.PayloadJSON, &event.CreatedAt,
	); err != nil {
		return runtimeOutboxEvent{}, false, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET claimed_sequence = ?, locked_by = ?, lease_expires_at = ?, last_error = '', updated_at = `+nowSQL+`
		WHERE consumer_name = ? AND last_sequence = ?`, sequence, owner, now.Add(leaseTTL).UTC().UnixMilli(), consumerName, lastSequence)
	if err != nil {
		return runtimeOutboxEvent{}, false, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return runtimeOutboxEvent{}, false, errRuntimeOutboxBusy
	}
	if err := tx.Commit(); err != nil {
		return runtimeOutboxEvent{}, false, err
	}
	return event, true, nil
}

func (a *app) ackRuntimeOutboxEvent(ctx context.Context, consumerName, owner string, sequence int64, now time.Time) error {
	return a.finishRuntimeOutboxEvent(ctx, consumerName, owner, sequence, "", now, true)
}

func (a *app) nackRuntimeOutboxEvent(ctx context.Context, consumerName, owner string, sequence int64, errorCode string, now time.Time) error {
	if strings.TrimSpace(errorCode) == "" || len(errorCode) > 512 || runtimeSecretString(errorCode) {
		return errors.New("invalid runtime outbox failure code")
	}
	return a.finishRuntimeOutboxEvent(ctx, consumerName, owner, sequence, errorCode, now, false)
}

func (a *app) finishRuntimeOutboxEvent(ctx context.Context, consumerName, owner string, sequence int64, errorCode string, now time.Time, acknowledge bool) error {
	if consumerName == "" || owner == "" || sequence <= 0 || now.IsZero() {
		return errors.New("invalid runtime outbox completion")
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := `SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at FROM runtime_outbox_consumers WHERE consumer_name = ?`
	if a.db.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var lastSequence, claimedSequence, leaseExpiresAt int64
	var lockedBy string
	if err := tx.QueryRowContext(ctx, query, consumerName).Scan(&lastSequence, &claimedSequence, &lockedBy, &leaseExpiresAt); err != nil {
		return err
	}
	if acknowledge && lastSequence >= sequence {
		return tx.Commit()
	}
	if claimedSequence != sequence || lockedBy != owner || (acknowledge && leaseExpiresAt <= now.UTC().UnixMilli()) {
		return errRuntimeOutboxNotClaimed
	}
	if acknowledge {
		_, err = tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET last_sequence = ?, claimed_sequence = 0, locked_by = '', lease_expires_at = 0, last_error = '', updated_at = `+nowSQL+` WHERE consumer_name = ?`, sequence, consumerName)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET claimed_sequence = 0, locked_by = '', lease_expires_at = 0, last_error = ?, updated_at = `+nowSQL+` WHERE consumer_name = ?`, errorCode, consumerName)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (a *app) setAccountModeHealth(ctx context.Context, health accountModeHealth) error {
	if health.AccountID <= 0 || !validExecutionMode(health.Mode) || !validModeHealthStatus(health.Status) || len(health.ErrorCode) > 64 || runtimeSecretString(health.ErrorCode) {
		return errors.New("invalid account execution mode health")
	}
	var recoverAt any
	if strings.TrimSpace(health.RecoverAt) != "" {
		parsed, err := time.Parse(time.RFC3339Nano, health.RecoverAt)
		if err != nil {
			return errors.New("invalid account execution mode recovery time")
		}
		recoverAt = parsed.UTC().Format(time.RFC3339Nano)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO account_mode_health (account_id, mode, status) VALUES (?, ?, 'unavailable')`, health.AccountID, health.Mode); err != nil {
		return fmt.Errorf("create account mode health: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE account_mode_health SET status = ?, error_code = ?, recover_at = ?, updated_at = `+nowSQL+` WHERE account_id = ? AND mode = ?`,
		health.Status, health.ErrorCode, recoverAt, health.AccountID, health.Mode)
	if err != nil {
		return fmt.Errorf("update account mode health: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return errors.New("account execution mode health was not updated")
	}
	return tx.Commit()
}

func (a *app) getAccountModeHealth(ctx context.Context, accountID int64, mode string) (accountModeHealth, error) {
	if accountID <= 0 || !validExecutionMode(mode) {
		return accountModeHealth{}, errors.New("invalid account execution mode health query")
	}
	var health accountModeHealth
	var recoverAt sql.NullString
	err := a.db.QueryRowContext(ctx, `SELECT account_id, mode, status, error_code, recover_at, updated_at FROM account_mode_health WHERE account_id = ? AND mode = ?`, accountID, mode).Scan(
		&health.AccountID, &health.Mode, &health.Status, &health.ErrorCode, &recoverAt, &health.UpdatedAt,
	)
	if recoverAt.Valid {
		health.RecoverAt = recoverAt.String
	}
	return health, err
}

func safeRuntimePayload(payload map[string]any) (string, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", errors.New("runtime outbox payload is not JSON serializable")
	}
	return safeRuntimePayloadJSON(string(encoded))
}

func safeRuntimePayloadJSON(payload string) (string, error) {
	if len(payload) == 0 || len(payload) > 64<<10 {
		return "", errors.New("runtime outbox payload size is invalid")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return "", errors.New("runtime outbox payload must be a JSON object")
	}
	if runtimePayloadContainsSecret(decoded) {
		return "", errors.New("runtime outbox payload contains a sensitive field")
	}
	canonical, _ := json.Marshal(decoded)
	return string(canonical), nil
}

func runtimePayloadContainsSecret(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			lower := strings.ToLower(key)
			for _, sensitive := range []string{"authorization", "credential", "password", "cookie", "secret", "session_key", "access_token", "refresh_token", "api_key", "proxy_url"} {
				if strings.Contains(lower, sensitive) {
					return true
				}
			}
			if runtimePayloadContainsSecret(child) {
				return true
			}
		}
	case []any:
		for _, child := range current {
			if runtimePayloadContainsSecret(child) {
				return true
			}
		}
	case string:
		return runtimeSecretString(current)
	}
	return false
}

func runtimeSecretString(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "bearer ") || strings.Contains(lower, "sk-ant-") || strings.Contains(lower, "sk-")
}

func validRuntimeStatus(status string) bool {
	switch status {
	case "legacy", "provisioning", "ready", "draining", "destroying", "failed", "archived", "deleted":
		return true
	default:
		return false
	}
}

func legacyExecutionPredicate(alias string) string {
	if alias == "" {
		alias = "accounts"
	}
	return alias + ".execution_migration_status = 'legacy'"
}

func (a *app) requireLegacyCredentialOwner(ctx context.Context, accountID int64) error {
	if accountID <= 0 {
		return sql.ErrNoRows
	}
	var migrationStatus string
	if err := a.db.QueryRowContext(ctx, `SELECT execution_migration_status FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&migrationStatus); err != nil {
		return err
	}
	if migrationStatus != "legacy" {
		return errRuntimeCredentialOwner
	}
	return nil
}

func writeRuntimeCredentialOwnerError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, errRuntimeCredentialOwner) {
		writeError(w, http.StatusConflict, errRuntimeCredentialOwner.Error())
		return true
	}
	return false
}

func writeRuntimeRoutingOwnerError(w http.ResponseWriter, err error) bool {
	if errors.Is(err, errRuntimeRoutingOwner) {
		writeError(w, http.StatusConflict, errRuntimeRoutingOwner.Error())
		return true
	}
	return false
}

func validExecutionMode(mode string) bool {
	return mode == "cli_native" || mode == "oauth_api"
}

func validModeHealthStatus(status string) bool {
	switch status {
	case "healthy", "cooling", "billing_blocked", "auth_failed", "unavailable":
		return true
	default:
		return false
	}
}

func allowedMigrationTransition(current, target string) bool {
	if current == target {
		return current == "legacy" || current == "migrating" || current == "migrated" || current == "failed"
	}
	switch current {
	case "legacy":
		return target == "migrating"
	case "migrating":
		return target == "migrated" || target == "failed"
	case "failed":
		return target == "migrating"
	case "migrated":
		return false
	default:
		return false
	}
}

func emptyJSONObject(value string) bool {
	var decoded map[string]any
	return json.Unmarshal([]byte(value), &decoded) == nil && len(decoded) == 0
}

func newRuntimeEventID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(fmt.Sprintf("generate runtime event ID: %v", err))
	}
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
