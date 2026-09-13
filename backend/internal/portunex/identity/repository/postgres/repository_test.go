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

var sessionColumns = []string{"id", "user_id", "token", "expires_at", "ip_address", "user_agent", "created_at", "last_used_at", "deleted_at"}

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

func syntheticParams() CreateParams {
	return CreateParams{
		ID: math.MaxInt64, UserID: math.MinInt64,
		StorageToken: StorageToken("synthetic' storage value; --"),
		ExpiresAt:    time.Date(2030, 1, 2, 3, 4, 5, 123456000, time.UTC),
		CreatedAt:    time.Date(2026, 9, 14, 1, 2, 3, 654321000, time.UTC),
		IPAddress:    sql.NullString{}, UserAgent: sql.NullString{String: "", Valid: true},
	}
}

func TestCreateParametersAndNulls(t *testing.T) {
	repository, mock := mockRepository(t)
	params := syntheticParams()
	mock.ExpectExec(createSQL).WithArgs(params.ID, params.UserID, string(params.StorageToken), params.ExpiresAt, nil, "", params.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.Create(context.Background(), params); err != nil {
		t.Fatal(err)
	}
}

func TestFindActivePreservesRecord(t *testing.T) {
	repository, mock := mockRepository(t)
	params := syntheticParams()
	mock.ExpectQuery(findActiveSQL).WithArgs(string(params.StorageToken)).WillReturnRows(sqlmock.NewRows(sessionColumns).
		AddRow(params.ID, params.UserID, string(params.StorageToken), params.ExpiresAt, nil, "", nil, nil, nil))
	record, err := repository.FindActiveByStorageToken(context.Background(), params.StorageToken)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != math.MaxInt64 || record.UserID != math.MinInt64 || record.StorageToken != params.StorageToken || !record.ExpiresAt.Equal(params.ExpiresAt) {
		t.Fatal("session values were transformed")
	}
	if record.IPAddress.Valid || !record.UserAgent.Valid || record.UserAgent.String != "" || record.CreatedAt.Valid || record.LastUsedAt.Valid || record.DeletedAt.Valid {
		t.Fatal("SQL NULL and empty values were conflated")
	}
}

func TestTouchAndRevokeUseExplicitTimes(t *testing.T) {
	repository, mock := mockRepository(t)
	now := syntheticParams().CreatedAt
	mock.ExpectExec(touchSQL).WithArgs(now, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(revokeSQL).WithArgs(now, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(revokeSQL).WithArgs(now, int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repository.Touch(context.Background(), 1, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.Revoke(context.Background(), 1, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.Revoke(context.Background(), 1, now); err != ErrNotFound {
		t.Fatal("missing/already revoked row should report no match")
	}
}

func TestReadAndWriteErrorsAreSafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"missing", sql.ErrNoRows, ErrNotFound},
		{"unique", &pq.Error{Code: "23505", Message: "secret-token", Detail: "secret-email"}, ErrConflict},
		{"foreign_key", &pq.Error{Code: "23503", Detail: "secret-token"}, ErrConstraint},
		{"not_null", &pq.Error{Code: "23502", Detail: "secret-token"}, ErrConstraint},
		{"retryable", &pq.Error{Code: "40001", Detail: "secret-token"}, ErrRetryable},
		{"deadlock", &pq.Error{Code: "40P01", Detail: "secret-token"}, ErrRetryable},
		{"query_canceled", &pq.Error{Code: "57014", Detail: "secret-token"}, ErrCanceled},
		{"unavailable", errors.New("secret-token connection string"), ErrUnavailable},
		{"wrapped_cancel", fmt.Errorf("secret-token: %w", context.Canceled), context.Canceled},
		{"wrapped_deadline", fmt.Errorf("secret-token: %w", context.DeadlineExceeded), context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, mock := mockRepository(t)
			mock.ExpectExec(createSQL).WillReturnError(tc.err)
			mock.ExpectQuery(findActiveSQL).WithArgs("synthetic").WillReturnError(tc.err)
			if err := repository.Create(context.Background(), syntheticParams()); err != tc.want {
				t.Fatalf("wrong write error category: %v", err)
			} else {
				assertSafeError(t, err)
			}
			record, err := repository.FindActiveByStorageToken(context.Background(), StorageToken("synthetic"))
			if record != nil || err != tc.want {
				t.Fatalf("wrong read result/error category: %v", err)
			}
			assertSafeError(t, err)
		})
	}
	t.Run("scan", func(t *testing.T) {
		repository, mock := mockRepository(t)
		mock.ExpectQuery(findActiveSQL).WithArgs("synthetic").WillReturnRows(sqlmock.NewRows(sessionColumns).
			AddRow(int64(1), int64(2), "secret-token", "secret-invalid-time", nil, nil, nil, nil, nil))
		record, err := repository.FindActiveByStorageToken(context.Background(), StorageToken("synthetic"))
		if record != nil || err != ErrUnavailable {
			t.Fatal("scan failure exposed partial record or driver error")
		}
		assertSafeError(t, err)
	})
	t.Run("rows_affected", func(t *testing.T) {
		repository, mock := mockRepository(t)
		mock.ExpectExec(createSQL).WillReturnResult(sqlmock.NewErrorResult(errors.New("secret-token")))
		if err := repository.Create(context.Background(), syntheticParams()); err != ErrUnavailable {
			t.Fatal("RowsAffected driver error escaped")
		}
		mock.ExpectExec(createSQL).WillReturnResult(sqlmock.NewResult(0, 2))
		if err := repository.Create(context.Background(), syntheticParams()); err != ErrUnavailable {
			t.Fatal("unexpected affected row count accepted")
		}
	})
}

func TestCancellationAndInvalidRepository(t *testing.T) {
	repository, _ := mockRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := repository.Create(ctx, syntheticParams()); err != context.Canceled {
		t.Fatal("pre-canceled write reached database")
	}
	if _, err := repository.FindActiveByStorageToken(ctx, StorageToken("synthetic")); err != context.Canceled {
		t.Fatal("pre-canceled read reached database")
	}
	if _, err := New(nil); err != ErrUnavailable {
		t.Fatal("nil database accepted")
	}
	for _, repository := range []*Repository{nil, {}} {
		if err := repository.Create(context.Background(), syntheticParams()); err != ErrUnavailable {
			t.Fatal("zero repository write did not fail safely")
		}
		if _, err := repository.FindActiveByStorageToken(context.Background(), StorageToken("synthetic")); err != ErrUnavailable {
			t.Fatal("zero repository read did not fail safely")
		}
	}
	t.Run("during_write", func(t *testing.T) {
		repository, mock := mockRepository(t)
		mock.ExpectExec(createSQL).WillDelayFor(time.Second).WillReturnResult(sqlmock.NewResult(0, 1))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		if err := repository.Create(ctx, syntheticParams()); err != context.DeadlineExceeded {
			t.Fatalf("write cancellation not preserved: %v", err)
		}
	})
}

func TestStorageTokenCommonFormattingIsRedacted(t *testing.T) {
	token := StorageToken("secret-token")
	if strings.Contains(fmt.Sprintf("%s %v %+v %#v", token, token, token, token), "secret-token") {
		t.Fatal("storage token escaped through Stringer/GoStringer formatting")
	}
}

func assertSafeError(t *testing.T, err error) {
	t.Helper()
	var driverError *pq.Error
	if errors.As(err, &driverError) || errors.Unwrap(err) != nil || strings.Contains(fmt.Sprintf("%+v %#v", err, err), "secret") {
		t.Fatal("unsafe error crossed the repository boundary")
	}
}
