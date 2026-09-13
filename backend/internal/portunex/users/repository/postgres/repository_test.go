package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

var userColumns = []string{"id", "email", "password_phc", "points", "role", "created_at", "updated_at", "deleted_at", "can_purchase_subscription", "daily_recharge_limit"}

func mockRepository(t *testing.T) (*Repository, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		db.Close()
	})
	repository, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	return repository, mock
}

func TestFindByIDPreservesNulls(t *testing.T) {
	repository, mock := mockRepository(t)
	mock.ExpectQuery(findByIDSQL).WithArgs(int64(math.MaxInt64)).WillReturnRows(
		sqlmock.NewRows(userColumns).AddRow(int64(math.MaxInt64), nil, nil, nil, nil, nil, nil, nil, false, "0.000000000000000001"),
	)
	record, err := repository.FindByID(context.Background(), math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != math.MaxInt64 || record.Email.Valid || record.PasswordPHC.Valid || record.Points.Valid || record.Role.Valid || record.CreatedAt.Valid || record.UpdatedAt.Valid || record.DeletedAt.Valid {
		t.Fatal("bigint or SQL NULL was not preserved")
	}
	if record.DailyRechargeLimit.String() != "0.000000000000000001" {
		t.Fatal("decimal precision was lost")
	}
}

func TestFindByEmailParametersAndExactValues(t *testing.T) {
	repository, mock := mockRepository(t)
	// Treat this as data without local trimming, SQL interpolation or normalization.
	email := " Demo' OR 1=1 --@Example.invalid "
	now := time.Date(2026, 9, 14, 12, 34, 56, 123456000, time.FixedZone("test", 8*3600))
	mock.ExpectQuery(findByEmailSQL).WithArgs(email).WillReturnRows(sqlmock.NewRows(userColumns).
		AddRow(int64(-7), email, "synthetic-phc", "999999999999.123456789012345678", "admin", now, now, nil, true, "123.987654321098765432"))
	record, err := repository.FindByEmail(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != -7 || record.Email.String != email || record.PasswordPHC.String != "synthetic-phc" || record.Role.String != "admin" || !record.CanPurchaseSubscription {
		t.Fatal("database values were transformed")
	}
	if !record.Points.Valid || record.Points.Decimal.String() != "999999999999.123456789012345678" || record.DailyRechargeLimit.String() != "123.987654321098765432" {
		t.Fatal("NUMERIC lost precision")
	}
	if !record.CreatedAt.Valid || !record.CreatedAt.Time.Equal(now) || !record.UpdatedAt.Time.Equal(now) {
		t.Fatal("timestamp instant changed")
	}
}

func TestReadErrorsAreSafeAndDiscardPartialRecord(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"missing", sql.ErrNoRows, ErrNotFound},
		{"unique", &pq.Error{Code: "23505", Message: "secret-phc", Detail: "secret-email"}, ErrConflict},
		{"constraint", &pq.Error{Code: "23514", Message: "secret-phc"}, ErrConstraint},
		{"retryable", &pq.Error{Code: "40001", Detail: "secret-email"}, ErrRetryable},
		{"query_canceled", &pq.Error{Code: "57014", Detail: "secret-email"}, ErrCanceled},
		{"unavailable", errors.New("secret-phc secret-email connection string"), ErrUnavailable},
		{"wrapped_cancel", fmt.Errorf("secret-email: %w", context.Canceled), context.Canceled},
		{"wrapped_deadline", fmt.Errorf("secret-phc: %w", context.DeadlineExceeded), context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, mock := mockRepository(t)
			mock.ExpectQuery(findByEmailSQL).WithArgs("synthetic@example.invalid").WillReturnError(tc.err)
			record, err := repository.FindByEmail(context.Background(), "synthetic@example.invalid")
			if record != nil || err != tc.want {
				t.Fatalf("unexpected result or error category: %v", err)
			}
			assertSafeError(t, err)
		})
	}
	t.Run("scan", func(t *testing.T) {
		repository, mock := mockRepository(t)
		mock.ExpectQuery(findByIDSQL).WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows(userColumns).
			AddRow(int64(1), "secret-email", "secret-phc", "secret-bad-decimal", "user", nil, nil, nil, false, "0"))
		record, err := repository.FindByID(context.Background(), 1)
		if record != nil || err != ErrUnavailable {
			t.Fatal("scan failure exposed a partial record or driver error")
		}
		assertSafeError(t, err)
	})
	t.Run("unsupported_numeric_nan", func(t *testing.T) {
		repository, mock := mockRepository(t)
		mock.ExpectQuery(findByIDSQL).WithArgs(int64(1)).WillReturnRows(sqlmock.NewRows(userColumns).
			AddRow(int64(1), nil, nil, "NaN", "user", nil, nil, nil, false, "0"))
		record, err := repository.FindByID(context.Background(), 1)
		if record != nil || err != ErrUnavailable {
			t.Fatal("unsupported PostgreSQL NaN must fail safely, not become zero")
		}
	})
}

func TestCancellationAndInvalidRepository(t *testing.T) {
	repository, _ := mockRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.FindByID(ctx, 1); err != context.Canceled {
		t.Fatal("pre-canceled call reached database")
	}
	if _, err := New(nil); err != ErrUnavailable {
		t.Fatal("nil database was accepted")
	}
	for _, repository := range []*Repository{nil, {}} {
		if _, err := repository.FindByID(context.Background(), 1); err != ErrUnavailable {
			t.Fatal("zero repository should fail safely")
		}
	}
	t.Run("during_query", func(t *testing.T) {
		repository, mock := mockRepository(t)
		mock.ExpectQuery(findByIDSQL).WithArgs(int64(1)).WillDelayFor(time.Second).
			WillReturnRows(sqlmock.NewRows(userColumns))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if _, err := repository.FindByID(ctx, 1); err != context.DeadlineExceeded {
			t.Fatalf("query cancellation not preserved: %v", err)
		}
	})
}

func assertSafeError(t *testing.T, err error) {
	t.Helper()
	var driverError *pq.Error
	if errors.As(err, &driverError) || errors.Unwrap(err) != nil || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "secret") {
		t.Fatal("unsafe error crossed the repository boundary")
	}
}
