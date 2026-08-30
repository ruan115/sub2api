package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestVerifyRuntimeSchemaRequiresEveryCredentialPathTable(t *testing.T) {
	for _, test := range []struct {
		name         string
		tableCount   int
		columnCount  int
		indexPresent bool
		indexUnique  bool
		indexColumns string
		wantReady    bool
	}{
		{name: "complete", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns), indexPresent: true, indexUnique: true, indexColumns: "intent_id", wantReady: true},
		{name: "missing table", tableCount: len(requiredRuntimeTables) - 1},
		{name: "partial migration missing column", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns) - 1},
		{name: "partial migration missing unique index", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns)},
		{name: "named index is not unique", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns), indexPresent: true, indexColumns: "intent_id"},
		{name: "named index has wrong columns", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns), indexPresent: true, indexUnique: true, indexColumns: "intent_id,workflow_id"},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			arguments := make([]driver.Value, len(requiredRuntimeTables))
			for index, table := range requiredRuntimeTables {
				arguments[index] = table
			}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(DISTINCT table_name)")).
				WithArgs(arguments...).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(test.tableCount))
			if test.tableCount == len(requiredRuntimeTables) {
				columnArguments := make([]driver.Value, 0, len(requiredRuntimeColumns)*2)
				for _, column := range requiredRuntimeColumns {
					columnArguments = append(columnArguments, column.table, column.name)
				}
				mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*)")).
					WithArgs(columnArguments...).
					WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(test.columnCount))
				if test.columnCount == len(requiredRuntimeColumns) {
					for _, index := range requiredRuntimeUniqueIndexes {
						rows := sqlmock.NewRows([]string{"non_unique", "columns"})
						if test.indexPresent {
							nonUnique := int64(1)
							if test.indexUnique {
								nonUnique = 0
							}
							rows.AddRow(nonUnique, test.indexColumns)
						} else {
							rows.AddRow(nil, nil)
						}
						mock.ExpectQuery(regexp.QuoteMeta("SELECT MIN(non_unique), GROUP_CONCAT(column_name ORDER BY seq_in_index SEPARATOR ',')")).
							WithArgs(index.table, index.name).
							WillReturnRows(rows)
					}
				}
			}
			err = VerifyRuntimeSchema(context.Background(), db)
			if test.wantReady && err != nil {
				t.Fatal(err)
			}
			if !test.wantReady && !errors.Is(err, ErrRuntimeSchemaUnavailable) {
				t.Fatalf("missing schema error = %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
