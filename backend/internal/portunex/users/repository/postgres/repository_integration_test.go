//go:build portunex_integration

package postgres

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/portunex/migrations"
	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/postgres/testcluster"
)

func TestIntegrationUserRepository(t *testing.T) {
	cluster := testcluster.Start(t)
	db := cluster.DB
	applySchema(t, db)
	repository, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 1, 2, 3, 456789000, time.FixedZone("test", -7*3600))
	execSynthetic(t, db, `INSERT INTO portunex_identity.users
		(id,email,password_phc,points,role,created_at,updated_at,can_purchase_subscription,daily_recharge_limit)
		VALUES ($1,$2,$3,$4,$5,$6,$6,$7,$8)`, int64(math.MaxInt64), "Case@Example.invalid", "synthetic-phc",
		"999999999999.123456789012345678", "admin", now, true, "0.000000000000000001")
	execSynthetic(t, db, `INSERT INTO portunex_identity.users
		(id,email,password_phc,points,role,created_at,updated_at,daily_recharge_limit)
		VALUES ($1,NULL,NULL,NULL,NULL,NULL,NULL,0)`, int64(math.MinInt64))

	t.Run("bigint_decimal_time_and_citext", func(t *testing.T) {
		record, err := repository.FindByEmail(ctx, "cASE@eXAMPLE.INVALID")
		if err != nil {
			t.Fatal(err)
		}
		if record.ID != math.MaxInt64 || record.Email.String != "Case@Example.invalid" || record.PasswordPHC.String != "synthetic-phc" || record.Role.String != "admin" || !record.CanPurchaseSubscription {
			t.Fatal("case-insensitive lookup or stored values changed")
		}
		if !record.Points.Valid || record.Points.Decimal.String() != "999999999999.123456789012345678" || record.DailyRechargeLimit.String() != "0.000000000000000001" {
			t.Fatal("NUMERIC precision changed")
		}
		if !record.CreatedAt.Valid || !record.CreatedAt.Time.Equal(now) || !record.UpdatedAt.Time.Equal(now) {
			t.Fatal("timestamptz instant changed")
		}
		if _, err := repository.FindByEmail(ctx, " Case@Example.invalid "); err != ErrNotFound {
			t.Fatal("repository silently trimmed email")
		}
	})

	t.Run("nullable_values", func(t *testing.T) {
		record, err := repository.FindByID(ctx, math.MinInt64)
		if err != nil {
			t.Fatal(err)
		}
		if record.ID != math.MinInt64 || record.Email.Valid || record.PasswordPHC.Valid || record.Points.Valid || record.Role.Valid || record.CreatedAt.Valid || record.UpdatedAt.Valid || record.DeletedAt.Valid {
			t.Fatal("SQL NULLs were replaced with defaults")
		}
	})

	t.Run("unsupported_numeric_nan_fails_safely", func(t *testing.T) {
		execSynthetic(t, db, "INSERT INTO portunex_identity.users (id,points) VALUES ($1,'NaN')", int64(-2))
		record, err := repository.FindByID(ctx, -2)
		if record != nil || err != ErrUnavailable {
			t.Fatal("unsupported PostgreSQL NaN became a value or escaped as a driver error")
		}
		assertSafeError(t, err)
	})

	t.Run("fixed_schema_operator_and_new_connection", func(t *testing.T) {
		// A one-connection pool keeps this test's session setting predictable.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		t.Cleanup(func() {
			db.SetMaxOpenConns(4)
			db.SetMaxIdleConns(1)
		})
		execSynthetic(t, db, "SET search_path = pg_catalog")
		if _, err := repository.FindByEmail(ctx, "CASE@example.invalid"); err != nil {
			t.Fatalf("repository depends on search_path: %v", err)
		}
		// Same ID/email, but distinguishable values in a higher-priority schema.
		execSynthetic(t, db, "CREATE TABLE public.users (LIKE portunex_identity.users)")
		execSynthetic(t, db, `INSERT INTO public.users
			(id,email,password_phc,points,role,can_purchase_subscription,daily_recharge_limit)
			VALUES ($1,$2,$3,1,'user',false,9)`, int64(math.MaxInt64), "Case@Example.invalid", "synthetic-shadow-phc")
		execSynthetic(t, db, "SET search_path = public, pg_catalog")
		byID, err := repository.FindByID(ctx, math.MaxInt64)
		if err != nil || byID.Role.String != "admin" || byID.PasswordPHC.String != "synthetic-phc" {
			t.Fatal("ID lookup read the public shadow instead of the recovery schema")
		}
		byEmail, err := repository.FindByEmail(ctx, "CASE@example.invalid")
		if err != nil || byEmail.ID != math.MaxInt64 || byEmail.Role.String != "admin" {
			t.Fatal("email lookup read the public shadow instead of the recovery schema")
		}
		execSynthetic(t, db, "RESET search_path")
		if stats := db.Stats(); stats.OpenConnections != 1 || stats.Idle != 1 {
			t.Fatal("expected one idle connection before reconnection check")
		}
		db.SetMaxIdleConns(0) // Synchronously close all existing pool connections.
		if stats := db.Stats(); stats.OpenConnections != 0 {
			t.Fatal("old connection remained open")
		}
		// With no existing connection, this must read committed data on a new
		// connection. The PostgreSQL process is NOT restarted by this test.
		reconnected, err := repository.FindByID(ctx, math.MaxInt64)
		if err != nil || reconnected.Role.String != "admin" || reconnected.Points.Decimal.String() != "999999999999.123456789012345678" {
			t.Fatal("committed user record did not survive a connection change")
		}
	})

	t.Run("soft_deleted_hidden", func(t *testing.T) {
		execSynthetic(t, db, "UPDATE portunex_identity.users SET deleted_at = $1 WHERE id = $2", now, int64(math.MaxInt64))
		if _, err := repository.FindByID(ctx, math.MaxInt64); err != ErrNotFound {
			t.Fatal("soft-deleted user returned by ID")
		}
		if _, err := repository.FindByEmail(ctx, "case@example.invalid"); err != ErrNotFound {
			t.Fatal("soft-deleted user returned by email")
		}
		execSynthetic(t, db, "INSERT INTO portunex_identity.users (id,email) VALUES ($1,$2)", int64(1), "CASE@example.invalid")
		record, err := repository.FindByEmail(ctx, "case@example.invalid")
		if err != nil || record.ID != 1 {
			t.Fatal("active email was not reusable after soft deletion")
		}
	})

	t.Run("real_query_cancellation", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "LOCK TABLE portunex_identity.users IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		if record, err := repository.FindByID(deadline, 1); record != nil || err != context.DeadlineExceeded {
			t.Fatalf("blocked query did not return a safe context error: %v", err)
		}
	})

	t.Run("closed_database_error_is_safe", func(t *testing.T) {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		record, err := repository.FindByEmail(ctx, "secret-email@example.invalid")
		if record != nil || err != ErrUnavailable {
			t.Fatal("closed database error escaped repository boundary")
		}
		assertSafeError(t, err)
	})
}

func applySchema(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(migrations.SQL()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func execSynthetic(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
