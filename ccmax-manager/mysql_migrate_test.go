package main

import "testing"

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
