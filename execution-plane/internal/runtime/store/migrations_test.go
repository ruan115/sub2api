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
	if len(migrations) != 2 || migrations[0].Checksum == ([32]byte{}) || migrations[1].Checksum == ([32]byte{}) {
		t.Fatalf("unexpected migrations: %+v", migrations)
	}
	schema := migrations[0].SQL + "\n" + migrations[1].SQL
	for _, table := range []string{
		"nodes", "node_enrollments", "node_certificates", "slots", "slot_assignments",
		"execution_leases", "credential_vault", "credential_versions", "credential_leases",
		"runtime_sessions", "provisioning_jobs", "image_releases", "node_drain_jobs",
		"node_command_results", "reconciliation_runs",
		"credential_security_events",
	} {
		if !strings.Contains(schema, "CREATE TABLE "+table+" (") {
			t.Errorf("missing table %s", table)
		}
	}
	for _, forbidden := range []string{"credentials_json", "access_token", "refresh_token", "api_key", "ccmax.", "accounts("} {
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
