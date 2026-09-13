package migrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

type catalogDefinition struct {
	Columns     []map[string]any `json:"columns"`
	Constraints []map[string]any `json:"constraints"`
	Indexes     []map[string]any `json:"indexes"`
	Extensions  []map[string]any `json:"extensions"`
}

func recoveryFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate catalog evidence")
	}
	path := filepath.Join(append([]string{filepath.Dir(filename), "..", "..", "..", "..", "recovery"}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func baseline(t *testing.T) catalogDefinition {
	t.Helper()
	var result catalogDefinition
	if err := json.Unmarshal(recoveryFile(t, "baselines", "portunex", "postgres", "identity-definition.json"), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func compact(value string) string { return strings.Join(strings.Fields(value), " ") }

func relocated(value string) string {
	for _, table := range []string{"users", "auth_sessions", "api_keys"} {
		value = strings.ReplaceAll(value, "public."+table, "portunex_identity."+table)
	}
	return value
}

// This is a drift check against evidence, not a PostgreSQL parser or semantics
// test. The tagged integration test independently compares the actual catalog.
func TestSchemaTextMatchesObservedDefinitions(t *testing.T) {
	b := baseline(t)
	if len(b.Columns) != 30 || len(b.Indexes) != 17 || len(b.Constraints) != 14 {
		t.Fatal("baseline scope changed; explicitly review the recovery schema")
	}
	ddl := SQL()
	tables := regexp.MustCompile(`(?s)CREATE TABLE portunex_identity\.(\w+) \(\n(.*?)\n\);`).FindAllStringSubmatch(ddl, -1)
	if len(tables) != 3 {
		t.Fatalf("got %d table definitions, want 3", len(tables))
	}
	columns := make(map[string][]string)
	constraints := make(map[string]string)
	for _, match := range tables {
		for _, raw := range strings.Split(match[2], "\n") {
			line := compact(strings.TrimSuffix(strings.TrimSpace(raw), ","))
			if strings.HasPrefix(line, "CONSTRAINT ") {
				fields := strings.SplitN(line, " ", 3)
				constraints[fields[1]] = fields[2]
			} else {
				columns[match[1]] = append(columns[match[1]], line)
			}
		}
	}
	seen := make(map[string]int)
	for _, col := range b.Columns {
		table := col["table"].(string)
		definition := col["name"].(string) + " " + col["type"].(string)
		if col["column_not_null"].(bool) {
			definition += " NOT NULL"
		}
		if col["has_default"].(bool) {
			definition += " DEFAULT " + col["default_expression"].(string)
		}
		position := int(col["position"].(float64)) - 1
		if position >= len(columns[table]) || columns[table][position] != definition {
			t.Errorf("%s column %d should be %q", table, position+1, definition)
		}
		seen[table]++
	}
	for table, actual := range columns {
		if len(actual) != seen[table] {
			t.Errorf("%s has unexpected columns", table)
		}
	}
	for _, constraint := range b.Constraints {
		if constraint["type"] == "n" { // Inline NOT NULL checked above.
			continue
		}
		name := constraint["name"].(string)
		if constraints[name] != relocated(constraint["definition"].(string)) {
			t.Errorf("constraint %s differs from evidence", name)
		}
		delete(constraints, name)
	}
	if len(constraints) != 0 {
		t.Errorf("unexpected constraints: %v", constraints)
	}
	indexes := regexp.MustCompile(`(?m)^CREATE (?:UNIQUE )?INDEX .*;$`).FindAllString(ddl, -1)
	if len(indexes) != len(b.Indexes)-3 {
		t.Fatalf("got %d explicit indexes, want 14 plus 3 primary keys", len(indexes))
	}
	for _, index := range b.Indexes {
		if !index["primary"].(bool) && !strings.Contains(ddl, relocated(index["definition"].(string))+";") {
			t.Errorf("index %s differs from evidence", index["name"])
		}
	}
}

func TestSchemaHasNoImplicitMigrationOrPolicy(t *testing.T) {
	ddl := SQL()
	if !strings.Contains(ddl, "CREATE SCHEMA portunex_identity;\nCREATE EXTENSION citext WITH SCHEMA public VERSION '1.8';") {
		t.Fatal("fresh-schema and pinned-extension requirements changed")
	}
	for _, forbidden := range []string{"IF NOT EXISTS", "CREATE SEQUENCE", "GENERATED", "SERIAL", "CREATE TRIGGER", "DROP ", "DELETE FROM", "INSERT INTO", "UPDATE ", "BEGIN;", "COMMIT;"} {
		if strings.Contains(strings.ToUpper(ddl), forbidden) {
			t.Errorf("schema contains unexpected policy/implicit application: %s", forbidden)
		}
	}
}
