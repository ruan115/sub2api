package reconcile

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestVerifyCCMAXRuntimeSchemaRequiresExactOutboxBoundary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(`(?s)SELECT COUNT\(DISTINCT table_name\).*information_schema.tables`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	columnExpectation := mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*information_schema.columns`)
	columnArguments := make([]driver.Value, 0, len(requiredCCMAXRuntimeColumns)*2)
	for _, column := range requiredCCMAXRuntimeColumns {
		columnArguments = append(columnArguments, column.table, column.name)
	}
	columnExpectation.WithArgs(columnArguments...).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(len(requiredCCMAXRuntimeColumns)))
	mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
		WithArgs("runtime_outbox", "PRIMARY").
		WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "sequence"))
	mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
		WithArgs("runtime_outbox", "uq_runtime_outbox_event").
		WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "event_id"))
	mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
		WithArgs("runtime_outbox_commit_lock", "PRIMARY").
		WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "singleton"))
	mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
		WithArgs("runtime_outbox_consumers", "PRIMARY").
		WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "consumer_name"))
	mock.ExpectQuery(`(?s)SELECT singleton, lock_epoch.*runtime_outbox_commit_lock`).
		WillReturnRows(sqlmock.NewRows([]string{"singleton", "lock_epoch"}).AddRow(1, 8))
	if err := VerifyCCMAXRuntimeSchema(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyCCMAXRuntimeSchemaFailsClosedOnPartialOrWrongIndex(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(sqlmock.Sqlmock)
	}{
		{name: "missing table", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`(?s)SELECT COUNT\(DISTINCT table_name\).*information_schema.tables`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
		}},
		{name: "missing consumer write column", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`(?s)SELECT COUNT\(DISTINCT table_name\).*information_schema.tables`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
			mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*information_schema.columns`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(len(requiredCCMAXRuntimeColumns) - 1))
		}},
		{name: "wrong primary sequence", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`(?s)SELECT COUNT\(DISTINCT table_name\).*information_schema.tables`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
			mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*information_schema.columns`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(len(requiredCCMAXRuntimeColumns)))
			mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
				WithArgs("runtime_outbox", "PRIMARY").
				WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "sequence,event_id"))
		}},
		{name: "wrong unique index", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`(?s)SELECT COUNT\(DISTINCT table_name\).*information_schema.tables`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
			mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*information_schema.columns`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(len(requiredCCMAXRuntimeColumns)))
			mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
				WithArgs("runtime_outbox", "PRIMARY").
				WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "sequence"))
			mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
				WithArgs("runtime_outbox", "uq_runtime_outbox_event").
				WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(1, "event_id"))
		}},
		{name: "missing commit lock row", setup: func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(`(?s)SELECT COUNT\(DISTINCT table_name\).*information_schema.tables`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
			mock.ExpectQuery(`(?s)SELECT COUNT\(\*\).*information_schema.columns`).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(len(requiredCCMAXRuntimeColumns)))
			mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
				WithArgs("runtime_outbox", "PRIMARY").
				WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "sequence"))
			mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
				WithArgs("runtime_outbox", "uq_runtime_outbox_event").
				WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "event_id"))
			mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
				WithArgs("runtime_outbox_commit_lock", "PRIMARY").
				WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "singleton"))
			mock.ExpectQuery(`(?s)SELECT MIN\(non_unique\).*information_schema.statistics`).
				WithArgs("runtime_outbox_consumers", "PRIMARY").
				WillReturnRows(sqlmock.NewRows([]string{"non_unique", "columns"}).AddRow(0, "consumer_name"))
			mock.ExpectQuery(`(?s)SELECT singleton, lock_epoch.*runtime_outbox_commit_lock`).
				WillReturnError(sql.ErrNoRows)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			test.setup(mock)
			if err := VerifyCCMAXRuntimeSchema(context.Background(), db); !errors.Is(err, ErrCCMAXRuntimeSchemaUnavailable) {
				t.Fatalf("schema error = %v", err)
			}
		})
	}
	if err := VerifyCCMAXRuntimeSchema(context.Background(), nil); !errors.Is(err, ErrCCMAXRuntimeSchemaUnavailable) {
		t.Fatalf("nil database error = %v", err)
	}
}
