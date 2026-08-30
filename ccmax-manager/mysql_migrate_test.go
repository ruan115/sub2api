//go:build sqlite_migrate

package main

import (
	"slices"
	"testing"
)

type migrationKeyScanner []any

func (values migrationKeyScanner) Scan(dest ...any) error {
	for index := range dest {
		*dest[index].(*any) = values[index]
	}
	return nil
}

func TestScanMigrationKeyNormalizesDriverNumericTypes(t *testing.T) {
	_, sqliteKey, err := scanMigrationKey(migrationKeyScanner{int64(42), "a"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	_, mysqlKey, err := scanMigrationKey(migrationKeyScanner{[]byte("42"), []byte("a")}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if sqliteKey != mysqlKey {
		t.Fatalf("SQLite key %q does not match MySQL key %q", sqliteKey, mysqlKey)
	}
}

func TestComparableMigrationValuePreservesIntegerTrailingZero(t *testing.T) {
	if got := comparableMigrationValue([]byte("1923854870")); got != "1923854870" {
		t.Fatalf("integer comparable value = %q", got)
	}
	if got := comparableMigrationValue([]byte("100.12000000")); got != "100.12" {
		t.Fatalf("decimal comparable value = %q", got)
	}
}

func TestExecutionTablesParticipateInMigrationWithoutInvalidSequenceWatermark(t *testing.T) {
	for _, table := range []string{
		"account_mode_health",
		"runtime_outbox",
		"runtime_proxy_reservations",
		"runtime_onboarding_result_cursors",
		"runtime_onboarding_submissions",
		"runtime_operation_audit",
	} {
		if !slices.Contains(mysqlMigrationTables, table) {
			t.Fatalf("execution table %s is missing from SQLite to MySQL migration", table)
		}
	}
	if mysqlAppendOnlyMigrationTables["runtime_outbox"] {
		t.Fatal("runtime_outbox cannot use the id-only append watermark because its primary key is sequence")
	}
	if mysqlAppendOnlyMigrationTables["runtime_proxy_reservations"] {
		t.Fatal("runtime_proxy_reservations is mutable and cannot use an append-only watermark")
	}
}
