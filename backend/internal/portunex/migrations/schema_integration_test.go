//go:build portunex_integration

package migrations

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/postgres/testcluster"
	"github.com/lib/pq"
)

func applySchema(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(SQL()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func inTransaction(t *testing.T, db *sql.DB, test func(*sql.Tx)) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	test(tx)
}

func execSQL(t *testing.T, tx *sql.Tx, query string, args ...any) {
	t.Helper()
	if _, err := tx.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// An expected statement failure must not poison the surrounding test transaction.
func rejected(t *testing.T, tx *sql.Tx, code string, query string, args ...any) {
	t.Helper()
	execSQL(t, tx, "SAVEPOINT expected_rejection")
	_, err := tx.Exec(query, args...)
	var pgErr *pq.Error
	if !errors.As(err, &pgErr) || string(pgErr.Code) != code {
		t.Fatalf("want PostgreSQL SQLSTATE %s, got %v", code, err)
	}
	execSQL(t, tx, "ROLLBACK TO SAVEPOINT expected_rejection")
	execSQL(t, tx, "RELEASE SAVEPOINT expected_rejection")
}

func assertCount(t *testing.T, tx *sql.Tx, query string, want int) {
	t.Helper()
	var count int
	if err := tx.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("count = %d, want %d", count, want)
	}
}

func TestIntegrationIdentitySchema(t *testing.T) {
	cluster := testcluster.Start(t)
	applySchema(t, cluster.DB)

	t.Run("catalog_matches_evidence", func(t *testing.T) {
		inTransaction(t, cluster.DB, func(tx *sql.Tx) {
			execSQL(t, tx, "SET LOCAL search_path = pg_catalog")
			// Reuse the read-only, committed catalog projection so defaults, index
			// predicates/order, and constraint flags are not manually approximated.
			query := string(recoveryFile(t, "collectors", "postgres", "identity-definition.sql"))
			begin := strings.Index(query, "SELECT jsonb_build_object(")
			end := strings.LastIndex(query, "\nROLLBACK;")
			if begin < 0 || end <= begin {
				t.Fatal("catalog collector shape changed")
			}
			query = strings.ReplaceAll(query[begin:end], "n.nspname = 'public'", "n.nspname = 'portunex_identity'")
			var raw []byte
			if err := tx.QueryRow(query).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var actual catalogDefinition
			if err := json.Unmarshal(raw, &actual); err != nil {
				t.Fatal(err)
			}
			expected := baseline(t)
			for _, constraint := range expected.Constraints {
				constraint["definition"] = relocated(constraint["definition"].(string))
				if constraint["referenced_schema"] == "public" {
					constraint["referenced_schema"] = "portunex_identity"
				}
			}
			for _, index := range expected.Indexes {
				index["definition"] = relocated(index["definition"].(string))
			}
			for _, pair := range []struct {
				name string
				got  []map[string]any
				want []map[string]any
			}{
				{"columns", actual.Columns, expected.Columns},
				{"constraints", actual.Constraints, expected.Constraints},
				{"indexes", actual.Indexes, expected.Indexes},
				{"extensions", actual.Extensions, expected.Extensions},
			} {
				if !reflect.DeepEqual(pair.got, pair.want) {
					t.Errorf("%s catalog differs:\ngot: %v\nwant: %v", pair.name, pair.got, pair.want)
				}
			}
			assertCount(t, tx, "SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'portunex_identity' AND c.relkind = 'i'", 17)
		})
	})

	t.Run("nullable_defaults_checks_and_required_ids", func(t *testing.T) {
		inTransaction(t, cluster.DB, func(tx *sql.Tx) {
			execSQL(t, tx, "INSERT INTO portunex_identity.users (id) VALUES (1), (2)")
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.users WHERE email IS NULL AND password_phc IS NULL AND points = 0 AND role = 'user' AND created_at IS NOT NULL AND updated_at IS NOT NULL AND deleted_at IS NULL AND NOT can_purchase_subscription AND daily_recharge_limit = 0", 2)
			execSQL(t, tx, "INSERT INTO portunex_identity.users (id, email, password_phc, points, role, created_at, updated_at) VALUES (3, NULL, NULL, NULL, NULL, NULL, NULL)")
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.users WHERE id = 3 AND points IS NULL AND role IS NULL AND created_at IS NULL AND updated_at IS NULL", 1)
			rejected(t, tx, "23514", "INSERT INTO portunex_identity.users (id, role) VALUES (4, 'owner')")
			rejected(t, tx, "23502", "INSERT INTO portunex_identity.users (email) VALUES ('synthetic@example.invalid')")
			rejected(t, tx, "23502", "INSERT INTO portunex_identity.users (id, can_purchase_subscription) VALUES (4, NULL)")
			rejected(t, tx, "23502", "INSERT INTO portunex_identity.users (id, daily_recharge_limit) VALUES (4, NULL)")
			execSQL(t, tx, "INSERT INTO portunex_identity.auth_sessions (id, user_id, token, expires_at, created_at, last_used_at) VALUES (1, 1, 'synthetic-session', '2000-01-01T00:00:00Z', NULL, NULL)")
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.auth_sessions WHERE created_at IS NULL AND last_used_at IS NULL AND ip_address IS NULL AND user_agent IS NULL", 1)
			rejected(t, tx, "23502", "INSERT INTO portunex_identity.auth_sessions (user_id, token, expires_at) VALUES (1, 'no-id', now())")
			rejected(t, tx, "23502", "INSERT INTO portunex_identity.auth_sessions (id, user_id, token) VALUES (2, 1, 'no-expiry')")
			rejected(t, tx, "23502", "INSERT INTO portunex_identity.auth_sessions (id, user_id, token, expires_at) VALUES (2, 1, NULL, now())")
			rejected(t, tx, "23502", "INSERT INTO portunex_identity.auth_sessions (id, user_id, token, expires_at) VALUES (2, NULL, 'no-user', now())")
			execSQL(t, tx, "INSERT INTO portunex_identity.api_keys (id) VALUES (1), (2)")
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.api_keys WHERE user_id IS NULL AND key_text IS NULL AND prefix IS NULL AND active AND settings IS NULL AND created_at IS NOT NULL AND rotated_at IS NULL AND deleted_at IS NULL AND name IS NULL AND last_used_at IS NULL", 2)
			execSQL(t, tx, "INSERT INTO portunex_identity.api_keys (id, active, created_at) VALUES (3, NULL, NULL)")
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.api_keys WHERE active IS NULL AND created_at IS NULL", 1)
			rejected(t, tx, "23502", "INSERT INTO portunex_identity.api_keys (name) VALUES ('no-id')")
		})
	})

	t.Run("citext_partial_uniqueness_and_cascades", func(t *testing.T) {
		inTransaction(t, cluster.DB, func(tx *sql.Tx) {
			execSQL(t, tx, "INSERT INTO portunex_identity.users (id, email) VALUES (1, 'Case@EXAMPLE.invalid')")
			rejected(t, tx, "23505", "INSERT INTO portunex_identity.users (id, email) VALUES (2, 'case@example.invalid')")
			execSQL(t, tx, "INSERT INTO portunex_identity.auth_sessions (id, user_id, token, expires_at) VALUES (1, 1, 'synthetic-storage', now())")
			execSQL(t, tx, "INSERT INTO portunex_identity.api_keys (id, user_id, key_text) VALUES (1, 1, 'synthetic-key')")
			rejected(t, tx, "23503", "INSERT INTO portunex_identity.api_keys (id, user_id) VALUES (2, 999)")
			rejected(t, tx, "23503", "INSERT INTO portunex_identity.auth_sessions (id, user_id, token, expires_at) VALUES (2, 999, 'orphan', now())")
			execSQL(t, tx, "UPDATE portunex_identity.users SET deleted_at = now() WHERE id = 1")
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.auth_sessions WHERE user_id = 1 AND deleted_at IS NULL", 1)
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.api_keys WHERE user_id = 1 AND deleted_at IS NULL", 1)
			execSQL(t, tx, "INSERT INTO portunex_identity.users (id, email) VALUES (2, 'case@example.invalid')")
			rejected(t, tx, "23505", "UPDATE portunex_identity.users SET deleted_at = NULL WHERE id = 1")
			rejected(t, tx, "23505", "INSERT INTO portunex_identity.auth_sessions (id, user_id, token, expires_at) VALUES (2, 1, 'synthetic-storage', now())")
			rejected(t, tx, "23505", "INSERT INTO portunex_identity.api_keys (id, user_id, key_text) VALUES (2, 1, 'synthetic-key')")
			execSQL(t, tx, "UPDATE portunex_identity.auth_sessions SET deleted_at = now() WHERE id = 1")
			execSQL(t, tx, "UPDATE portunex_identity.api_keys SET deleted_at = now() WHERE id = 1")
			execSQL(t, tx, "INSERT INTO portunex_identity.auth_sessions (id, user_id, token, expires_at) VALUES (2, 1, 'synthetic-storage', now())")
			execSQL(t, tx, "INSERT INTO portunex_identity.api_keys (id, user_id, key_text) VALUES (2, 1, 'synthetic-key')")
			rejected(t, tx, "23505", "UPDATE portunex_identity.auth_sessions SET deleted_at = NULL WHERE id = 1")
			rejected(t, tx, "23505", "UPDATE portunex_identity.api_keys SET deleted_at = NULL WHERE id = 1")
			execSQL(t, tx, "DELETE FROM portunex_identity.users WHERE id = 1")
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.auth_sessions WHERE user_id = 1", 0)
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.api_keys WHERE user_id = 1", 0)
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.users WHERE id = 2", 1)
		})
	})

	t.Run("exact_numeric_bigint_and_timestamptz", func(t *testing.T) {
		inTransaction(t, cluster.DB, func(tx *sql.Tx) {
			const value = "999999999999.123456789012345678"
			instant := time.Date(2026, 9, 14, 10, 15, 30, 123456000, time.FixedZone("UTC+8", 8*60*60))
			execSQL(t, tx, "INSERT INTO portunex_identity.users (id, points, daily_recharge_limit, created_at) VALUES ($1, $2, $3, $4)", int64(math.MaxInt64), value, "-1.000000000000000001", instant)
			var id int64
			var points, limit string
			var created time.Time
			if err := tx.QueryRow("SELECT id, points::text, daily_recharge_limit::text, created_at FROM portunex_identity.users").Scan(&id, &points, &limit, &created); err != nil {
				t.Fatal(err)
			}
			if id != math.MaxInt64 || points != value || limit != "-1.000000000000000001" || !created.Equal(instant) {
				t.Fatalf("precision changed: id=%d points=%s limit=%s created=%v", id, points, limit, created)
			}
			execSQL(t, tx, "INSERT INTO portunex_identity.users (id, points) VALUES ($1, $2)", int64(math.MinInt64), "0.0000000000000000005")
			if err := tx.QueryRow("SELECT points::text FROM portunex_identity.users WHERE id = $1", int64(math.MinInt64)).Scan(&points); err != nil {
				t.Fatal(err)
			}
			if points != "0.000000000000000001" {
				t.Fatalf("numeric scale/rounding changed: %s", points)
			}
			rejected(t, tx, "22003", "INSERT INTO portunex_identity.users (id, points) VALUES (1, '1000000000000')")
			rejected(t, tx, "22003", "INSERT INTO portunex_identity.users (id) VALUES (9223372036854775808)")
		})
	})

	t.Run("existing_schema_is_a_conflict", func(t *testing.T) {
		inTransaction(t, cluster.DB, func(tx *sql.Tx) {
			rejected(t, tx, "42P06", SQL())
			assertCount(t, tx, "SELECT count(*) FROM portunex_identity.users", 0)
			assertCount(t, tx, "SELECT count(*) FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'portunex_identity' AND c.relkind = 'i'", 17)
		})
	})
}

func TestIntegrationSchemaFailureRollsBackPartialDDL(t *testing.T) {
	cluster := testcluster.Start(t)
	if _, err := cluster.DB.Exec("CREATE EXTENSION citext WITH SCHEMA public VERSION '1.8'"); err != nil {
		t.Fatal(err)
	}
	tx, err := cluster.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(SQL()) // CREATE SCHEMA succeeds; CREATE EXTENSION conflicts.
	var pgErr *pq.Error
	if !errors.As(err, &pgErr) || pgErr.Code != "42710" {
		t.Fatalf("want existing-extension conflict, got %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var schemaAbsent, extensionUnchanged bool
	if err := cluster.DB.QueryRow("SELECT NOT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = 'portunex_identity'), EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = 'citext' AND extversion = '1.8')").Scan(&schemaAbsent, &extensionUnchanged); err != nil {
		t.Fatal(err)
	}
	if !schemaAbsent || !extensionUnchanged {
		t.Fatal("failed schema transaction left partial objects or changed the existing extension")
	}
}
