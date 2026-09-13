package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrRuntimeSchemaUnavailable = errors.New("worker runtime schema is unavailable")

var requiredRuntimeTables = []string{
	"nodes",
	"node_enrollments",
	"node_certificates",
	"slots",
	"slot_assignments",
	"execution_leases",
	"credential_vault",
	"credential_versions",
	"credential_leases",
	"credential_version_operations",
	"credential_rotation_commits",
	"proxy_reservation_grants",
	"proxy_leases",
	"credential_security_events",
	"onboarding_intents",
	"onboarding_start_triggers",
	"onboarding_workflows",
	"onboarding_results",
	"lifecycle_event_apply_receipts",
}

type runtimeSchemaColumn struct {
	table string
	name  string
}

// These post-core columns are checked separately so a partially applied
// additive migration cannot be mistaken for a usable runtime schema.
var requiredRuntimeColumns = []runtimeSchemaColumn{
	{table: "slot_assignments", name: "desired_generation"},
	{table: "proxy_leases", name: "reservation_id"},
	{table: "proxy_leases", name: "desired_generation"},
	{table: "proxy_leases", name: "binding_revision"},
	{table: "onboarding_start_triggers", name: "status"},
	{table: "onboarding_start_triggers", name: "claim_version"},
	{table: "onboarding_start_triggers", name: "intent_expires_at"},
	{table: "onboarding_start_triggers", name: "started_workflow_id"},
	{table: "onboarding_start_triggers", name: "started_at"},
	{table: "lifecycle_event_apply_receipts", name: "receipt_id"},
	{table: "lifecycle_event_apply_receipts", name: "source_sequence"},
	{table: "lifecycle_event_apply_receipts", name: "event_id"},
	{table: "lifecycle_event_apply_receipts", name: "event_account_id"},
	{table: "lifecycle_event_apply_receipts", name: "event_type"},
	{table: "lifecycle_event_apply_receipts", name: "desired_generation"},
	{table: "lifecycle_event_apply_receipts", name: "event_payload_sha256"},
	{table: "lifecycle_event_apply_receipts", name: "event_created_at"},
	{table: "lifecycle_event_apply_receipts", name: "slot_id"},
	{table: "lifecycle_event_apply_receipts", name: "slot_account_id"},
	{table: "lifecycle_event_apply_receipts", name: "provider"},
	{table: "lifecycle_event_apply_receipts", name: "desired_state"},
	{table: "lifecycle_event_apply_receipts", name: "required_labels_json"},
	{table: "lifecycle_event_apply_receipts", name: "image_digest"},
	{table: "lifecycle_event_apply_receipts", name: "cpu_request_millis"},
	{table: "lifecycle_event_apply_receipts", name: "memory_request_bytes"},
	{table: "lifecycle_event_apply_receipts", name: "applied_at"},
}

type runtimeSchemaIndex struct {
	table           string
	name            string
	expectedColumns string
}

var requiredRuntimeUniqueIndexes = []runtimeSchemaIndex{
	{table: "onboarding_workflows", name: "uq_onboarding_workflows_intent", expectedColumns: "intent_id"},
	{table: "onboarding_start_triggers", name: "uq_onboarding_start_triggers_event", expectedColumns: "event_id"},
	{table: "onboarding_start_triggers", name: "uq_onboarding_start_triggers_sequence", expectedColumns: "source_sequence"},
	{table: "onboarding_start_triggers", name: "uq_onboarding_start_triggers_intent", expectedColumns: "intent_id"},
	{table: "onboarding_start_triggers", name: "uq_onboarding_start_triggers_account_generation", expectedColumns: "account_id,desired_generation"},
	{table: "lifecycle_event_apply_receipts", name: "PRIMARY", expectedColumns: "receipt_id"},
	{table: "lifecycle_event_apply_receipts", name: "uq_lifecycle_event_apply_receipts_event", expectedColumns: "event_id"},
	{table: "lifecycle_event_apply_receipts", name: "uq_lifecycle_event_apply_receipts_sequence", expectedColumns: "source_sequence"},
}

// VerifyRuntimeSchema is intentionally read-only. Production migrations are an
// explicit deployment step; the orchestrator refuses to listen when any table
// required by its credential path is absent.
func VerifyRuntimeSchema(ctx context.Context, db *sql.DB) error {
	if ctx == nil || ctx.Err() != nil || db == nil {
		return ErrRuntimeSchemaUnavailable
	}
	arguments := make([]any, 0, len(requiredRuntimeTables))
	placeholders := ""
	for index, table := range requiredRuntimeTables {
		if index > 0 {
			placeholders += ", "
		}
		placeholders += "?"
		arguments = append(arguments, table)
	}
	query := `
SELECT COUNT(DISTINCT table_name)
FROM information_schema.tables
WHERE table_schema = DATABASE() AND table_name IN (` + placeholders + `)`
	var count int
	if err := db.QueryRowContext(ctx, query, arguments...).Scan(&count); err != nil {
		return fmt.Errorf("verify worker runtime schema: %w", err)
	}
	if count != len(requiredRuntimeTables) {
		return ErrRuntimeSchemaUnavailable
	}
	columnArguments := make([]any, 0, len(requiredRuntimeColumns)*2)
	columnPredicates := ""
	for index, column := range requiredRuntimeColumns {
		if index > 0 {
			columnPredicates += " OR "
		}
		columnPredicates += "(table_name = ? AND column_name = ?)"
		columnArguments = append(columnArguments, column.table, column.name)
	}
	columnQuery := `
SELECT COUNT(*)
FROM information_schema.columns
WHERE table_schema = DATABASE() AND (` + columnPredicates + `)`
	if err := db.QueryRowContext(ctx, columnQuery, columnArguments...).Scan(&count); err != nil {
		return fmt.Errorf("verify worker runtime schema columns: %w", err)
	}
	if count != len(requiredRuntimeColumns) {
		return ErrRuntimeSchemaUnavailable
	}
	for _, required := range requiredRuntimeUniqueIndexes {
		var nonUnique sql.NullInt64
		var columns sql.NullString
		err := db.QueryRowContext(ctx, `
SELECT MIN(non_unique), GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')
FROM information_schema.statistics
WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?`,
			required.table, required.name,
		).Scan(&nonUnique, &columns)
		if err != nil {
			return fmt.Errorf("verify worker runtime schema index %s.%s: %w", required.table, required.name, err)
		}
		if !nonUnique.Valid || nonUnique.Int64 != 0 || !columns.Valid || columns.String != required.expectedColumns {
			return ErrRuntimeSchemaUnavailable
		}
	}
	return nil
}
