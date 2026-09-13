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
		indexFailure string
		wantReady    bool
	}{
		{name: "complete", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns), wantReady: true},
		{name: "missing table", tableCount: len(requiredRuntimeTables) - 1},
		{name: "partial migration missing column", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns) - 1},
		{name: "partial migration missing unique index", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns), indexFailure: "missing"},
		{name: "named index is not unique", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns), indexFailure: "non-unique"},
		{name: "named index has wrong columns", tableCount: len(requiredRuntimeTables), columnCount: len(requiredRuntimeColumns), indexFailure: "wrong-columns"},
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
					for indexPosition, index := range requiredRuntimeUniqueIndexes {
						if indexPosition > 0 && test.indexFailure != "" {
							break
						}
						rows := sqlmock.NewRows([]string{"non_unique", "columns"})
						if indexPosition == 0 && test.indexFailure == "missing" {
							rows.AddRow(nil, nil)
						} else {
							nonUnique := int64(0)
							columns := index.expectedColumns
							if indexPosition == 0 && test.indexFailure == "non-unique" {
								nonUnique = 1
							}
							if indexPosition == 0 && test.indexFailure == "wrong-columns" {
								columns += ",unexpected"
							}
							rows.AddRow(nonUnique, columns)
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
