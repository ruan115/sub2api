package main

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
)

// The dead pool is what proxyNotQuarantinedPredicate matches against. If it is
// missing the predicate is trivially true and single-use enforcement is
// silently disabled, which is exactly what happened on MySQL before
// migrateSharedData existed.
func TestSharedDataMigrationSeedsProxyPoolsAndAdmin(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "seeds.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	var defaultPools, deadPools, admins int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxy_pools WHERE id = 1`).Scan(&defaultPools); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxy_pools WHERE system_kind = ?`, deadProxyPoolKind).Scan(&deadPools); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND deleted_at IS NULL`).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if defaultPools != 1 || deadPools != 1 || admins != 1 {
		t.Fatalf("seeds = default %d, dead %d, admin %d; want 1 each", defaultPools, deadPools, admins)
	}
}

func TestSharedDataMigrationIsIdempotent(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "idempotent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "idempotent@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	a.markAccountReauth(created.ID, "token expired")

	counts := func() (history, events, pools, admins int) {
		t.Helper()
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxy_account_history`).Scan(&history); err != nil {
			t.Fatal(err)
		}
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_lifecycle_events`).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxy_pools`).Scan(&pools); err != nil {
			t.Fatal(err)
		}
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&admins); err != nil {
			t.Fatal(err)
		}
		return
	}
	beforeHistory, beforeEvents, beforePools, beforeAdmins := counts()

	// Migrations run on every startup, so a second pass must be a no-op.
	if err := a.migrateSharedData(); err != nil {
		t.Fatal(err)
	}

	afterHistory, afterEvents, afterPools, afterAdmins := counts()
	if afterHistory != beforeHistory || afterEvents != beforeEvents || afterPools != beforePools || afterAdmins != beforeAdmins {
		t.Fatalf("second migration changed rows: history %d→%d, events %d→%d, pools %d→%d, admins %d→%d",
			beforeHistory, afterHistory, beforeEvents, afterEvents, beforePools, afterPools, beforeAdmins, afterAdmins)
	}
}

// Archiving moves proxy_id into archived_proxy_id, so a backfill keyed on
// proxy_id alone leaves the address looking untouched and a single-use pool
// hands it out again.
func TestSharedDataMigrationBackfillsArchivedProxyHistory(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "archived-backfill.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "burned@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	a.markAccountReauth(created.ID, "token expired")
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(created.ID, 10)+"/archive", map[string]any{}, http.StatusOK, nil)

	// Simulate a database archived before the history table was maintained.
	if _, err := a.db.Exec(`DELETE FROM proxy_account_history`); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateSharedData(); err != nil {
		t.Fatal(err)
	}

	var history int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxy_account_history WHERE account_id = ?`, created.ID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 1 {
		t.Fatalf("archived proxy history rows = %d, want the binding restored", history)
	}
}

// Restoring clears archived_proxy_id, which for a pre-history database is the
// last evidence the address was consumed.
func TestAccountRestoreKeepsSingleUseProxyBurned(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "restore-burn.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "restored@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	a.markAccountReauth(created.ID, "token expired")
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(created.ID, 10)+"/archive", map[string]any{}, http.StatusOK, nil)
	if _, err := a.db.Exec(`DELETE FROM proxy_account_history`); err != nil {
		t.Fatal(err)
	}

	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(created.ID, 10)+"/restore", map[string]any{}, http.StatusOK, nil)

	var history int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxy_account_history WHERE account_id = ?`, created.ID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 1 {
		t.Fatalf("restore dropped the binding record: history rows = %d", history)
	}
}
