package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrCCMAXRuntimeSchemaUnavailable = errors.New("CCMAX runtime outbox schema is unavailable")

var requiredCCMAXRuntimeColumns = []struct {
	table string
	name  string
}{
	{table: "accounts", name: "runtime_generation"},
	{table: "accounts", name: "runtime_slot_id"},
	{table: "accounts", name: "runtime_provider"},
	{table: "runtime_outbox", name: "sequence"},
	{table: "runtime_outbox", name: "event_id"},
	{table: "runtime_outbox", name: "account_id"},
	{table: "runtime_outbox", name: "event_type"},
	{table: "runtime_outbox", name: "desired_generation"},
	{table: "runtime_outbox", name: "payload_json"},
	{table: "runtime_outbox", name: "created_at"},
	{table: "runtime_outbox_commit_lock", name: "singleton"},
	{table: "runtime_outbox_commit_lock", name: "lock_epoch"},
	{table: "runtime_outbox_consumers", name: "consumer_name"},
	{table: "runtime_outbox_consumers", name: "last_sequence"},
	{table: "runtime_outbox_consumers", name: "claimed_sequence"},
	{table: "runtime_outbox_consumers", name: "locked_by"},
	{table: "runtime_outbox_consumers", name: "lease_expires_at"},
	{table: "runtime_outbox_consumers", name: "claim_version"},
	{table: "runtime_outbox_consumers", name: "failure_state"},
	{table: "runtime_outbox_consumers", name: "failure_sequence"},
	{table: "runtime_outbox_consumers", name: "failure_class"},
	{table: "runtime_outbox_consumers", name: "failure_code"},
	{table: "runtime_outbox_consumers", name: "failure_count"},
	{table: "runtime_outbox_consumers", name: "first_failed_at"},
	{table: "runtime_outbox_consumers", name: "last_failed_at"},
	{table: "runtime_outbox_consumers", name: "next_attempt_at"},
	{table: "runtime_outbox_consumers", name: "blocked_claim_version"},
	{table: "runtime_outbox_consumers", name: "last_error"},
	{table: "runtime_outbox_consumers", name: "updated_at"},
}

// VerifyCCMAXRuntimeSchema is read-only. A production orchestrator must not
// initialize cloud/KMS or listen until both the worker_runtime schema and this
// separate CCMAX outbox boundary have been verified.
func VerifyCCMAXRuntimeSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil || ctx.Err() != nil || db == nil {
		return ErrCCMAXRuntimeSchemaUnavailable
	}
	var tableCount int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT table_name)
FROM information_schema.tables
WHERE table_schema = DATABASE()
  AND table_name IN ('accounts', 'runtime_outbox', 'runtime_outbox_commit_lock', 'runtime_outbox_consumers')`).Scan(&tableCount); err != nil {
		return fmt.Errorf("verify CCMAX runtime tables: %w", err)
	}
	if tableCount != 4 {
		return ErrCCMAXRuntimeSchemaUnavailable
	}
	arguments := make([]any, 0, len(requiredCCMAXRuntimeColumns)*2)
	predicates := ""
	for index, column := range requiredCCMAXRuntimeColumns {
		if index > 0 {
			predicates += " OR "
		}
		predicates += "(table_name = ? AND column_name = ?)"
		arguments = append(arguments, column.table, column.name)
	}
	var columnCount int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM information_schema.columns
WHERE table_schema = DATABASE() AND (`+predicates+`)`, arguments...).Scan(&columnCount); err != nil {
		return fmt.Errorf("verify CCMAX runtime columns: %w", err)
	}
	if columnCount != len(requiredCCMAXRuntimeColumns) {
		return ErrCCMAXRuntimeSchemaUnavailable
	}
	for _, index := range []struct {
		table   string
		name    string
		columns string
	}{
		{table: "runtime_outbox", name: "PRIMARY", columns: "sequence"},
		{table: "runtime_outbox", name: "uq_runtime_outbox_event", columns: "event_id"},
		{table: "runtime_outbox_commit_lock", name: "PRIMARY", columns: "singleton"},
		{table: "runtime_outbox_consumers", name: "PRIMARY", columns: "consumer_name"},
	} {
		var nonUnique sql.NullInt64
		var columns sql.NullString
		err := db.QueryRowContext(ctx, `
SELECT MIN(non_unique), GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`, index.table, index.name).Scan(&nonUnique, &columns)
		if err != nil {
			return fmt.Errorf("verify CCMAX runtime index %s.%s: %w", index.table, index.name, err)
		}
		if !nonUnique.Valid || nonUnique.Int64 != 0 || !columns.Valid || columns.String != index.columns {
			return ErrCCMAXRuntimeSchemaUnavailable
		}
	}
	var singleton int
	var lockEpoch uint64
	if err := db.QueryRowContext(ctx, `
SELECT singleton, lock_epoch
FROM runtime_outbox_commit_lock
WHERE singleton = 1`).Scan(&singleton, &lockEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCCMAXRuntimeSchemaUnavailable
		}
		return fmt.Errorf("verify CCMAX runtime outbox commit-order lock: %w", err)
	}
	if singleton != 1 {
		return ErrCCMAXRuntimeSchemaUnavailable
	}
	return nil
}
