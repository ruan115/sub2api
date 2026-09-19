package main

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func testSchemaSource(statements ...string) []byte {
	var source strings.Builder
	source.WriteString("package fixture\nfunc mysqlExecutionSchema() []string { return []string{")
	for _, statement := range statements {
		source.WriteString(strconv.Quote(statement) + ",")
	}
	source.WriteString("} }")
	return []byte(source.String())
}

func testSchemaStatements() []string {
	return []string{
		"CREATE TABLE IF NOT EXISTS runtime_outbox_commit_lock (singleton INT PRIMARY KEY, lock_epoch BIGINT)",
		commitLockInsert,
		"CREATE TABLE IF NOT EXISTS runtime_outbox (sequence BIGINT PRIMARY KEY)",
		"CREATE TABLE IF NOT EXISTS runtime_outbox_consumers (consumer_name VARCHAR(128) PRIMARY KEY)",
	}
}

func TestExtractUsesRepositorySchemaAndOnlyWhitelistedStatements(t *testing.T) {
	source, err := os.ReadFile("../../../../ccmax-manager/execution_outbox.go")
	if err != nil {
		t.Fatal(err)
	}
	statements, err := extractCCMAXSchema(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 4 {
		t.Fatalf("selected %d statements, want 4", len(statements))
	}
	joined := strings.Join(statements, "\n")
	for _, required := range []string{"uq_runtime_outbox_event", "claim_version", "blocked_claim_version", "chk_runtime_outbox_consumer_failure_state", commitLockInsert} {
		if !strings.Contains(joined, required) {
			t.Errorf("repository schema lost required marker %s", required)
		}
	}
	for _, forbidden := range []string{"CREATE TABLE IF NOT EXISTS accounts", "runtime_proxy_reservations", "account_mode_health", "runtime_operation_audit"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("selected unrelated schema %s", forbidden)
		}
	}
	for _, statement := range statements {
		if !strings.Contains(string(source), strings.TrimSuffix(statement, ";")) {
			t.Fatal("statement is not an exact source literal")
		}
	}
}

func TestExtractRejectsMissingAndDuplicateStatements(t *testing.T) {
	base := testSchemaStatements()
	for index := range base {
		remaining := append(append([]string(nil), base[:index]...), base[index+1:]...)
		if _, err := extractCCMAXSchema(testSchemaSource(remaining...)); err == nil {
			t.Errorf("accepted missing statement %d", index)
		}
	}
	if _, err := extractCCMAXSchema(testSchemaSource(append(base, base[0])...)); err == nil {
		t.Fatal("accepted duplicate selected statement")
	}
}

func TestExtractIgnoresUnrelatedSQLButRejectsAppendedCommands(t *testing.T) {
	base := testSchemaStatements()
	input := append(base, "CREATE TABLE IF NOT EXISTS runtime_outbox_lookalike (id INT)", "DELETE FROM accounts")
	got, err := extractCCMAXSchema(testSchemaSource(input...))
	if err != nil || len(got) != 4 {
		t.Fatalf("whitelist extraction: count=%d err=%v", len(got), err)
	}
	for _, suffix := range []string{"; DROP DATABASE production", " -- comment", " /* comment */"} {
		input := testSchemaStatements()
		input[2] += suffix
		if _, err := extractCCMAXSchema(testSchemaSource(input...)); err == nil {
			t.Errorf("accepted appended SQL or comment %q", suffix)
		}
	}
}

func TestExtractRejectsDynamicOrWrongSource(t *testing.T) {
	for _, source := range []string{
		"package fixture",
		"package fixture; func mysqlExecutionSchema() []string { return buildSchema() }",
		"package fixture; func mysqlExecutionSchema() []string { return []string{buildSchema()} }",
		"package fixture; func mysqlExecutionSchema() []string { return []string{\"a\" + \"b\"} }",
		"package fixture; func mysqlExecutionSchema() []string { return []string{} }; func mysqlExecutionSchema() []string { return []string{} }",
	} {
		if _, err := extractCCMAXSchema([]byte(source)); err == nil {
			t.Errorf("accepted nonliteral or incomplete schema")
		}
	}
}
