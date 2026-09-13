//go:build portunex_integration

package postgres

import (
	"context"
	"database/sql"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/portunex/migrations"
	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/postgres/testcluster"
)

func TestIntegrationSessionRepository(t *testing.T) {
	cluster := testcluster.Start(t)
	db := cluster.DB
	applySchema(t, db)
	repository, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const userID = int64(math.MaxInt64 - 1)
	execSynthetic(t, db, "INSERT INTO portunex_identity.users (id) VALUES ($1)", userID)
	var databaseNow time.Time
	if err := db.QueryRow("SELECT pg_catalog.now()").Scan(&databaseNow); err != nil {
		t.Fatal(err)
	}
	params := CreateParams{
		ID: math.MaxInt64, UserID: userID, StorageToken: StorageToken("synthetic-storage-value"),
		ExpiresAt: databaseNow.Add(time.Hour), CreatedAt: databaseNow.Add(-time.Hour),
		IPAddress: sql.NullString{}, UserAgent: sql.NullString{String: "", Valid: true},
	}

	t.Run("create_lookup_and_explicit_touch", func(t *testing.T) {
		if err := repository.Create(ctx, params); err != nil {
			t.Fatal(err)
		}
		record, err := repository.FindActiveByStorageToken(ctx, params.StorageToken)
		if err != nil {
			t.Fatal(err)
		}
		if record.ID != math.MaxInt64 || record.UserID != userID || record.StorageToken != params.StorageToken || !record.ExpiresAt.Equal(params.ExpiresAt) || record.IPAddress.Valid || !record.UserAgent.Valid || record.UserAgent.String != "" {
			t.Fatal("stored session values changed")
		}
		if !record.CreatedAt.Valid || !record.CreatedAt.Time.Equal(params.CreatedAt) || !record.LastUsedAt.Valid || !record.LastUsedAt.Time.Equal(params.CreatedAt) || record.DeletedAt.Valid {
			t.Fatal("created/last-used initialization did not match explicit input")
		}
		usedAt := databaseNow.Add(-time.Minute)
		if err := repository.Touch(ctx, params.ID, usedAt); err != nil {
			t.Fatal(err)
		}
		record, err = repository.FindActiveByStorageToken(ctx, params.StorageToken)
		if err != nil || !record.LastUsedAt.Time.Equal(usedAt) || !record.ExpiresAt.Equal(params.ExpiresAt) {
			t.Fatal("touch did not preserve expiry or explicit last-used time")
		}
	})

	t.Run("null_timestamps_and_expiration", func(t *testing.T) {
		execSynthetic(t, db, `INSERT INTO portunex_identity.auth_sessions
			(id,user_id,token,expires_at,created_at,last_used_at)
			VALUES ($1,$2,$3,$4,NULL,NULL)`, int64(1), userID, "synthetic-null-times", params.ExpiresAt)
		record, err := repository.FindActiveByStorageToken(ctx, StorageToken("synthetic-null-times"))
		if err != nil || record.CreatedAt.Valid || record.LastUsedAt.Valid || record.IPAddress.Valid || record.UserAgent.Valid {
			t.Fatal("nullable session values did not survive database scan")
		}
		expired := params
		expired.ID, expired.StorageToken, expired.ExpiresAt = 2, StorageToken("synthetic-expired"), databaseNow.Add(-time.Second)
		if err := repository.Create(ctx, expired); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.FindActiveByStorageToken(ctx, expired.StorageToken); err != ErrNotFound {
			t.Fatal("expired session was returned as active")
		}
		if err := repository.Touch(ctx, expired.ID, databaseNow); err != nil {
			t.Fatal("storage touch incorrectly added an expiration policy")
		}
	})

	t.Run("fixed_schema_and_new_connection", func(t *testing.T) {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		t.Cleanup(func() {
			db.SetMaxOpenConns(6)
			db.SetMaxIdleConns(2)
		})
		// A public table contains the same ID/token with distinguishable values.
		execSynthetic(t, db, "CREATE TABLE public.auth_sessions (LIKE portunex_identity.auth_sessions)")
		execSynthetic(t, db, `INSERT INTO public.auth_sessions
			(id,user_id,token,expires_at,ip_address,user_agent)
			VALUES ($1,$2,$3,$4,'synthetic-shadow-ip','synthetic-shadow-agent')`,
			params.ID, params.UserID, string(params.StorageToken), params.ExpiresAt)
		execSynthetic(t, db, "SET search_path = public, pg_catalog")
		record, err := repository.FindActiveByStorageToken(ctx, params.StorageToken)
		if err != nil || record.ID != params.ID || record.IPAddress.Valid || !record.UserAgent.Valid || record.UserAgent.String != "" {
			t.Fatal("session lookup read the public shadow instead of the recovery schema")
		}
		execSynthetic(t, db, "RESET search_path")
		if stats := db.Stats(); stats.OpenConnections != 1 || stats.Idle != 1 {
			t.Fatal("expected one idle connection before reconnection check")
		}
		db.SetMaxIdleConns(0)
		if stats := db.Stats(); stats.OpenConnections != 0 {
			t.Fatal("old connection remained open")
		}
		// Proves committed storage remains readable on a new connection only;
		// this is not a PostgreSQL process-restart or crash-recovery gate.
		reconnected, err := repository.FindActiveByStorageToken(ctx, params.StorageToken)
		if err != nil || reconnected.ID != params.ID || !reconnected.LastUsedAt.Valid || !reconnected.LastUsedAt.Time.Equal(databaseNow.Add(-time.Minute)) {
			t.Fatal("committed session record did not survive a connection change")
		}
	})

	t.Run("concurrent_unique_conflict_revoke_and_reuse", func(t *testing.T) {
		const attempts = 8
		outcomes := make(chan error, attempts)
		start := make(chan struct{})
		var workers sync.WaitGroup
		for i := range attempts {
			workers.Add(1)
			go func(i int) {
				defer workers.Done()
				<-start
				candidate := params
				candidate.ID, candidate.StorageToken = int64(100+i), StorageToken("synthetic-concurrent")
				outcomes <- repository.Create(ctx, candidate)
			}(i)
		}
		close(start)
		workers.Wait()
		close(outcomes)
		created, conflicted := 0, 0
		for err := range outcomes {
			switch err {
			case nil:
				created++
			case ErrConflict:
				conflicted++
				assertSafeError(t, err)
			default:
				t.Fatalf("unexpected concurrent insert outcome: %v", err)
			}
		}
		if created != 1 || conflicted != attempts-1 {
			t.Fatalf("created = %d, conflicts = %d", created, conflicted)
		}
		record, err := repository.FindActiveByStorageToken(ctx, StorageToken("synthetic-concurrent"))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.Revoke(ctx, record.ID, databaseNow); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.FindActiveByStorageToken(ctx, record.StorageToken); err != ErrNotFound {
			t.Fatal("revoked row remained active")
		}
		if err := repository.Revoke(ctx, record.ID, databaseNow); err != ErrNotFound {
			t.Fatal("repeated internal revoke should report no matching undeleted row")
		}
		if err := repository.Touch(ctx, record.ID, databaseNow); err != ErrNotFound {
			t.Fatal("touch changed a revoked row")
		}
		reused := params
		reused.ID, reused.StorageToken = 200, record.StorageToken
		if err := repository.Create(ctx, reused); err != nil {
			t.Fatal("partial unique index did not allow reuse after revocation")
		}
		// Reuse is allowed by storage, not prescribed as a token-generation policy.
	})

	t.Run("foreign_key_error_is_sanitized", func(t *testing.T) {
		invalid := params
		invalid.ID, invalid.UserID, invalid.StorageToken = 300, math.MinInt64, StorageToken("secret-token")
		err := repository.Create(ctx, invalid)
		if err != ErrConstraint {
			t.Fatalf("unexpected foreign-key error category: %v", err)
		}
		assertSafeError(t, err)
	})

	t.Run("storage_lookup_is_not_user_authorization", func(t *testing.T) {
		execSynthetic(t, db, "UPDATE portunex_identity.users SET deleted_at = $1 WHERE id = $2", databaseNow, userID)
		if _, err := repository.FindActiveByStorageToken(ctx, params.StorageToken); err != nil {
			t.Fatal("storage lookup silently added a user authorization policy")
		}
	})

	t.Run("real_write_cancellation", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "LOCK TABLE portunex_identity.auth_sessions IN ACCESS EXCLUSIVE MODE"); err != nil {
			t.Fatal(err)
		}
		deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()
		blocked := params
		blocked.ID, blocked.StorageToken = 400, StorageToken("synthetic-blocked")
		if err := repository.Create(deadline, blocked); err != context.DeadlineExceeded {
			t.Fatalf("blocked write did not return a safe context error: %v", err)
		}
	})

	t.Run("physical_user_delete_cascades", func(t *testing.T) {
		execSynthetic(t, db, "DELETE FROM portunex_identity.users WHERE id = $1", userID)
		if _, err := repository.FindActiveByStorageToken(ctx, params.StorageToken); err != ErrNotFound {
			t.Fatal("physical user delete did not cascade")
		}
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
