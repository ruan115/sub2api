package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestAccountArchiveQuarantinesProxyAndPreservesHistory(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "archive@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "archive-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	putJSON(t, handler, http.MethodPost, "/api/usage", map[string]any{
		"request_id": "archive-usage", "purpose_key": "default", "account_id": created.ID,
		"model": "claude-test", "input_tokens": 1000, "output_tokens": 500,
	}, http.StatusCreated, nil)
	a.markAccountReauth(created.ID, "token expired")

	var result accountArchiveResult
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(created.ID, 10)+"/archive", map[string]any{}, http.StatusOK, &result)
	if result.Matched != 1 || result.Archived != 1 || result.Skipped != 0 {
		t.Fatalf("archive result = %+v", result)
	}
	var archivedAt sql.NullString
	var currentProxy, archivedProxy sql.NullInt64
	var status string
	var schedulable int
	if err := a.db.QueryRow(`SELECT archived_at, proxy_id, archived_proxy_id, status, schedulable FROM accounts WHERE id = ?`, created.ID).Scan(&archivedAt, &currentProxy, &archivedProxy, &status, &schedulable); err != nil {
		t.Fatal(err)
	}
	if !archivedAt.Valid || currentProxy.Valid || !archivedProxy.Valid || archivedProxy.Int64 != proxyID || status != "disabled" || schedulable != 0 {
		t.Fatalf("archived state = archived:%v current:%v previous:%v status:%s schedulable:%d", archivedAt, currentProxy, archivedProxy, status, schedulable)
	}
	var archivedProxies []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies/archived", nil, http.StatusOK, &archivedProxies)
	if len(archivedProxies) != 1 || archivedProxies[0].ID != proxyID || archivedProxies[0].ArchivedAt == "" || !strings.Contains(archivedProxies[0].HistoricalAccounts, created.Name) {
		t.Fatalf("released proxy archive = %+v", archivedProxies)
	}

	var current []account
	putJSON(t, handler, http.MethodGet, "/api/accounts", nil, http.StatusOK, &current)
	if len(current) != 0 {
		t.Fatalf("current accounts = %d, want 0", len(current))
	}
	var archived []account
	putJSON(t, handler, http.MethodGet, "/api/accounts?archived=only", nil, http.StatusOK, &archived)
	if len(archived) != 1 || archived[0].ArchivedAt == "" || archived[0].ProxyID != nil || archived[0].ProxyName == "" || archived[0].RequestCount != 1 {
		t.Fatalf("archived account = %+v", archived)
	}
	if _, err := a.db.Exec(`UPDATE proxies SET name = 'archived-seoul-proxy', host = 'proxy.example.test', exit_ip = '203.0.113.42' WHERE id = ?`, proxyID); err != nil {
		t.Fatal(err)
	}
	for _, search := range []string{"archive@example.com", "archived-seoul", "proxy.example", "203.0.113.42"} {
		archived = nil
		putJSON(t, handler, http.MethodGet, "/api/accounts?archived=only&search="+search, nil, http.StatusOK, &archived)
		if len(archived) != 1 || archived[0].ID != created.ID || archived[0].ProxyIP != "203.0.113.42" {
			t.Fatalf("archived account search %q = %+v", search, archived)
		}
	}
	archived = nil
	putJSON(t, handler, http.MethodGet, "/api/accounts?archived=only&search=no-match", nil, http.StatusOK, &archived)
	if len(archived) != 0 {
		t.Fatalf("unmatched archived search = %+v", archived)
	}

	var proxyStatus, poolKind, poolName string
	var deadPoolID int64
	if err := a.db.QueryRow(`SELECT p.status, pp.id, pp.system_kind, pp.name
		FROM proxies p JOIN proxy_pools pp ON pp.id = p.pool_id WHERE p.id = ?`, proxyID).Scan(&proxyStatus, &deadPoolID, &poolKind, &poolName); err != nil {
		t.Fatal(err)
	}
	if proxyStatus != "disabled" || poolKind != deadProxyPoolKind || poolName != deadProxyPoolName {
		t.Fatalf("quarantined proxy = status:%s pool:%d kind:%s name:%s", proxyStatus, deadPoolID, poolKind, poolName)
	}
	var proxyPort int
	if err := a.db.QueryRow(`SELECT port FROM proxies WHERE id = ?`, proxyID).Scan(&proxyPort); err != nil {
		t.Fatal(err)
	}
	var importResult map[string]int
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": 1, "default_protocol": "http", "text": "http://proxy.example.test:" + strconv.Itoa(proxyPort),
	}, http.StatusCreated, &importResult)
	if importResult["created"] != 0 || importResult["skipped"] != 1 || importResult["invalid"] != 0 {
		t.Fatalf("reimport dead proxy result = %+v", importResult)
	}
	rogueResult, err := a.db.Exec(`INSERT INTO proxies
		(pool_id, name, protocol, host, port, username, password, status)
		SELECT 1, 'rogue-reimport', protocol, host, port, username, password, 'active'
		FROM proxies WHERE id = ?`, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	rogueProxyID, _ := rogueResult.LastInsertId()

	var pools []proxyPool
	putJSON(t, handler, http.MethodGet, "/api/proxy-pools", nil, http.StatusOK, &pools)
	for _, pool := range pools {
		if pool.ID == deadPoolID || pool.Name == deadProxyPoolName {
			t.Fatalf("dead proxy pool leaked into normal pools: %+v", pool)
		}
	}
	var proxies []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	if containsProxyRecord(proxies, proxyID) || containsProxyRecord(proxies, rogueProxyID) {
		t.Fatalf("dead proxy or matching normal duplicate leaked into normal proxies: %+v", proxies)
	}
	putJSON(t, handler, http.MethodPut, "/api/proxies/"+strconv.FormatInt(proxyID, 10), map[string]any{
		"name": "reactivated", "status": "active", "username": "",
	}, http.StatusNotFound, nil)

	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "blocked-reuse@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "blocked-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusConflict, nil)
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "blocked-duplicate@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "blocked-duplicate-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": rogueProxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusConflict, nil)

	replacementProxyID := createTestForwardProxy(t, a)
	var second account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "replacement@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "replacement-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": replacementProxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &second)
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	replacementProxy := findProxyRecord(t, proxies, replacementProxyID)
	if replacementProxy.AssignedTo != second.Name || replacementProxy.UsedAccountCount != 1 {
		t.Fatalf("replacement proxy = %+v", replacementProxy)
	}
	var historyCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxy_account_history WHERE proxy_id = ?`, proxyID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 1 {
		t.Fatalf("dead proxy history count = %d, want 1", historyCount)
	}

	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(created.ID, 10)+"/restore", map[string]any{}, http.StatusOK, nil)
	var restored account
	putJSON(t, handler, http.MethodGet, "/api/accounts?search=archive@example.com", nil, http.StatusOK, &current)
	if len(current) != 1 {
		t.Fatalf("restored account list = %+v", current)
	}
	restored = current[0]
	if restored.ArchivedAt != "" || restored.ProxyID != nil || restored.Schedulable || restored.DispatchStatus != "error" {
		t.Fatalf("restored account = %+v", restored)
	}
}

func TestAccountArchiveKeepsProxyReusableWhenPoolAllowsReuse(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var pool proxyPool
	putJSON(t, handler, http.MethodPut, "/api/proxy-pools/1", map[string]any{
		"name": "default", "source_type": "manual", "default_protocol": "socks5", "status": "active",
		"single_use_enabled": false,
	}, http.StatusOK, &pool)

	createAccount := func(name string) account {
		var item account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": name + "-token"}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
			"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
		}, http.StatusCreated, &item)
		return item
	}

	first := createAccount("reusable-archive-first@example.com")
	a.markAccountReauth(first.ID, "token expired")
	putJSON(t, handler, http.MethodPost, fmt.Sprintf("/api/accounts/%d/archive", first.ID), map[string]any{}, http.StatusOK, nil)

	var status, poolKind string
	var poolID int64
	if err := a.db.QueryRow(`SELECT proxy.status, pool.id, pool.system_kind FROM proxies proxy
		JOIN proxy_pools pool ON pool.id = proxy.pool_id WHERE proxy.id = ?`, proxyID).Scan(&status, &poolID, &poolKind); err != nil {
		t.Fatal(err)
	}
	if status != "active" || poolID != 1 || poolKind != "" {
		t.Fatalf("reusable archived proxy = status:%s pool:%d kind:%q", status, poolID, poolKind)
	}
	createAccount("reusable-archive-second@example.com")
}

func TestArchivedSingleUseProxyRequiresExplicitRestoreForOneReuse(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)

	create := func(name string, wantStatus int) account {
		var item account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": name + "-token"}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
			"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
		}, wantStatus, &item)
		return item
	}

	first := create("single-use-first@example.com", http.StatusCreated)
	a.markAccountReauth(first.ID, "expired")
	putJSON(t, handler, http.MethodPost, fmt.Sprintf("/api/accounts/%d/archive", first.ID), map[string]any{}, http.StatusOK, nil)

	var restored proxyRecord
	putJSON(t, handler, http.MethodPost, fmt.Sprintf("/api/proxies/%d/restore", proxyID), map[string]any{"pool_id": 1}, http.StatusOK, &restored)
	if restored.PoolID != 1 || restored.Status != "active" || restored.ReuseApprovedAt == "" {
		t.Fatalf("restored proxy = %+v", restored)
	}

	second := create("single-use-second@example.com", http.StatusCreated)
	var approval sql.NullString
	if err := a.db.QueryRow(`SELECT reuse_approved_at FROM proxies WHERE id = ?`, proxyID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if approval.Valid {
		t.Fatalf("reuse approval remained after account %d bound the proxy", second.ID)
	}
	a.markAccountReauth(second.ID, "expired")
	putJSON(t, handler, http.MethodPost, fmt.Sprintf("/api/accounts/%d/archive", second.ID), map[string]any{}, http.StatusOK, nil)
	create("single-use-third@example.com", http.StatusConflict)
}

func TestDeadProxyMigrationQuarantinesLegacyReleasedProxy(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	databasePath := filepath.Join(t.TempDir(), "test.db")
	a, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "legacy-archive@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "legacy-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	if _, err := a.db.Exec(`UPDATE accounts SET archived_proxy_id = proxy_id, proxy_id = NULL,
		status = 'disabled', schedulable = 0, archived_at = `+nowSQL+` WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	var status, kind string
	if err := reopened.db.QueryRow(`SELECT p.status, pool.system_kind FROM proxies p
		JOIN proxy_pools pool ON pool.id = p.pool_id WHERE p.id = ?`, proxyID).Scan(&status, &kind); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || kind != deadProxyPoolKind {
		t.Fatalf("migrated legacy proxy = status:%s kind:%s", status, kind)
	}
	var proxies []proxyRecord
	putJSON(t, reopened.routes(), http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	if containsProxyRecord(proxies, proxyID) {
		t.Fatalf("migrated dead proxy %d leaked into normal proxies", proxyID)
	}
}

func TestAccountArchiveRejectsLiveAccountAndBatchSkipsIt(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	create := func(name string) account {
		proxyID := createTestForwardProxy(t, a)
		var item account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": name + "-token"}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
			"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
		}, http.StatusCreated, &item)
		return item
	}
	live := create("live@example.com")
	dead := create("dead@example.com")
	for _, item := range []account{live, dead} {
		if _, err := a.db.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 1)`, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	var adminUserID int64
	if err := a.db.QueryRow(`SELECT id FROM users WHERE role = 'admin' ORDER BY id LIMIT 1`).Scan(&adminUserID); err != nil {
		t.Fatal(err)
	}
	apiKeyResult, err := a.db.Exec(`INSERT INTO api_keys (user_id, key_hash, key_prefix, key_secret, name) VALUES (?, 'archive-test-hash', 'sk-archive', 'sk-archive-test', 'archive-test')`, adminUserID)
	if err != nil {
		t.Fatal(err)
	}
	apiKeyID, _ := apiKeyResult.LastInsertId()
	for _, item := range []account{live, dead} {
		if _, err := a.db.Exec(`INSERT INTO dispatch_sessions (session_hash, api_key_id, account_id, expires_at) VALUES (?, ?, ?, '2999-01-01T00:00:00Z')`, "archive-session-"+strconv.FormatInt(item.ID, 10), apiKeyID, item.ID); err != nil {
			t.Fatal(err)
		}
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(live.ID, 10)+"/archive", map[string]any{}, http.StatusConflict, nil)
	a.markAccountReauth(dead.ID, "expired")
	var result accountArchiveResult
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-archive", map[string]any{"ids": []int64{live.ID, dead.ID, dead.ID}}, http.StatusOK, &result)
	if result.Matched != 2 || result.Archived != 1 || result.Skipped != 1 {
		t.Fatalf("batch archive result = %+v", result)
	}
	for table, want := range map[string]int{"account_inflight": 1, "dispatch_sessions": 1} {
		var liveRows, deadRows int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE account_id = ?`, live.ID).Scan(&liveRows); err != nil {
			t.Fatal(err)
		}
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE account_id = ?`, dead.ID).Scan(&deadRows); err != nil {
			t.Fatal(err)
		}
		if liveRows != want || deadRows != 0 {
			t.Fatalf("%s rows after mixed archive: live=%d dead=%d", table, liveRows, deadRows)
		}
	}
	var action string
	if err := a.db.QueryRow(`SELECT action FROM audit_logs WHERE path = '/api/accounts/batch-archive' ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != "account.archive" {
		t.Fatalf("audit action = %q, want account.archive", action)
	}
}

func findProxyRecord(t *testing.T, items []proxyRecord, id int64) proxyRecord {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("proxy %d not found", id)
	return proxyRecord{}
}

func containsProxyRecord(items []proxyRecord, id int64) bool {
	for _, item := range items {
		if item.ID == id {
			return true
		}
	}
	return false
}
