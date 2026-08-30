package store

import (
	"strings"
	"testing"
)

func TestRuntimeCoreMigrationContainsRequiredBoundaries(t *testing.T) {
	migrations, err := Migrations("up")
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 10 {
		t.Fatalf("unexpected migrations: %+v", migrations)
	}
	for _, migration := range migrations {
		if migration.Checksum == ([32]byte{}) {
			t.Fatalf("migration has an empty checksum: %+v", migration)
		}
	}
	var schemaBuilder strings.Builder
	for _, migration := range migrations {
		schemaBuilder.WriteString(migration.SQL)
		schemaBuilder.WriteByte('\n')
	}
	schema := schemaBuilder.String()
	for _, table := range []string{
		"nodes", "node_enrollments", "node_certificates", "slots", "slot_assignments",
		"execution_leases", "credential_vault", "credential_versions", "credential_leases",
		"runtime_sessions", "provisioning_jobs", "image_releases", "node_drain_jobs",
		"node_command_results", "reconciliation_runs",
		"credential_security_events",
		"onboarding_intents",
		"onboarding_workflows",
		"onboarding_results",
		"credential_version_operations",
		"credential_rotation_commits",
		"proxy_reservation_grants",
		"proxy_leases",
	} {
		if !strings.Contains(schema, "CREATE TABLE "+table+" (") {
			t.Errorf("missing table %s", table)
		}
	}
	for _, forbidden := range []string{
		"credentials_json", "access_token", "refresh_token", "api_key", "ccmax.", "accounts(",
		"delete from proxy_leases",
	} {
		if strings.Contains(strings.ToLower(schema), forbidden) {
			t.Errorf("migration contains forbidden cross-boundary/plaintext term %q", forbidden)
		}
	}
	if !strings.Contains(schema, "token_sha256 BINARY(32)") || strings.Contains(schema, "enrollment_token") {
		t.Error("enrollment schema must store only a token digest")
	}
	if !strings.Contains(schema, "uq_slot_assignments_active") || !strings.Contains(schema, "execution_epoch BIGINT UNSIGNED") {
		t.Error("slot assignment uniqueness/epoch fencing schema is missing")
	}
	for _, boundary := range []string{
		"control_session_id VARCHAR(32)",
		"allocatable_cpu_millis BIGINT UNSIGNED",
		"reserved_cpu_millis BIGINT UNSIGNED",
		"next_execution_epoch BIGINT UNSIGNED",
		"actual_generation BIGINT UNSIGNED",
		"uq_execution_leases_epoch",
		"uq_onboarding_intents_idempotency",
		"claim_expires_at DATETIME(6)",
		"uq_onboarding_workflows_key_command",
		"uq_onboarding_workflows_activation_command",
		"uq_onboarding_workflows_credential_lease",
		"uq_onboarding_workflows_intent",
		"uq_onboarding_results_intent",
		"uq_credential_version_operations_version",
		"uq_credential_rotation_commits_version",
		"uq_proxy_reservation_account_generation",
		"uq_proxy_reservation_runtime_binding",
		"ADD COLUMN desired_generation BIGINT UNSIGNED NULL AFTER execution_epoch",
		"ADD COLUMN reservation_id VARCHAR(128) NULL AFTER proxy_lease_id",
		"ADD COLUMN binding_revision BIGINT UNSIGNED NULL AFTER desired_generation",
		"fk_proxy_leases_reservation_binding",
		"uq_proxy_leases_slot_epoch",
	} {
		if !strings.Contains(schema, boundary) {
			t.Errorf("migration is missing runtime fencing/capacity boundary %q", boundary)
		}
	}
}

func TestMigrationsRejectUnknownDirection(t *testing.T) {
	if _, err := Migrations("sideways"); err == nil {
		t.Fatal("expected migration direction validation")
	}
}
