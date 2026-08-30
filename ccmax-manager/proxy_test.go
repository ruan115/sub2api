package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
)

func TestParseProxyLineFormats(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		protocol string
		host     string
		port     int
		username string
		password string
	}{
		{name: "host port", line: "proxy.example.com:8080", protocol: "socks5", host: "proxy.example.com", port: 8080},
		{name: "host first colon", line: "proxy.example.com:8080:alice:secret", protocol: "socks5", host: "proxy.example.com", port: 8080, username: "alice", password: "secret"},
		{name: "host first ip colon", line: "192.0.2.10:22:root:test-password", protocol: "socks5", host: "192.0.2.10", port: 22, username: "root", password: "test-password"},
		{name: "user first colon", line: "alice:secret:proxy.example.com:8080", protocol: "socks5", host: "proxy.example.com", port: 8080, username: "alice", password: "secret"},
		{name: "user first at", line: "alice:pa:ss@proxy.example.com:8080", protocol: "socks5", host: "proxy.example.com", port: 8080, username: "alice", password: "pa:ss"},
		{name: "host first at", line: "proxy.example.com:8080@alice:pa:ss", protocol: "socks5", host: "proxy.example.com", port: 8080, username: "alice", password: "pa:ss"},
		{name: "http url", line: "http://alice:p%40ss@proxy.example.com:3128", protocol: "http", host: "proxy.example.com", port: 3128, username: "alice", password: "p@ss"},
		{name: "socks5h url", line: "socks5h://alice:secret@127.0.0.1:1080", protocol: "socks5", host: "127.0.0.1", port: 1080, username: "alice", password: "secret"},
		{name: "ipv6", line: "[2001:db8::1]:1080", protocol: "socks5", host: "2001:db8::1", port: 1080},
		{name: "ipv6 auth", line: "alice:secret@[2001:db8::1]:1080", protocol: "socks5", host: "2001:db8::1", port: 1080, username: "alice", password: "secret"},
		{name: "reverse url authority", line: "https://proxy.example.com:8443@alice:secret", protocol: "https", host: "proxy.example.com", port: 8443, username: "alice", password: "secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, err := parseProxyLine(test.line, "socks5")
			if err != nil {
				t.Fatal(err)
			}
			if item.Protocol != test.protocol || item.Host != test.host || item.Port != test.port || item.Username != test.username || item.Password != test.password {
				t.Fatalf("parseProxyLine(%q) = %+v", test.line, item)
			}
		})
	}
}

func TestParseProxyLineRejectsInvalidValues(t *testing.T) {
	for _, line := range []string{"", "proxy.example.com", "proxy.example.com:0", "proxy.example.com:70000", "ftp://proxy.example.com:21", "alice@proxy.example.com:8080"} {
		if item, err := parseProxyLine(line, "socks5"); err == nil {
			t.Fatalf("parseProxyLine(%q) = %+v, want error", line, item)
		}
	}
}

func TestProxyTextFromAPIEncodesCredentials(t *testing.T) {
	body := []byte(`{"data":[{"host":"proxy.example.com","port":8080,"protocol":"http","username":"a@b","password":"p:a ss"}]}`)
	item, err := parseProxyLine(proxyTextFromAPI(body), "socks5")
	if err != nil {
		t.Fatal(err)
	}
	if item.Protocol != "http" || item.Username != "a@b" || item.Password != "p:a ss" {
		t.Fatalf("API proxy = %+v", item)
	}
}

func TestProxyPoolDeleteRemovesPoolProxies(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var pool proxyPool
	putJSON(t, handler, http.MethodPost, "/api/proxy-pools", map[string]any{
		"name": "spare", "source_type": "manual", "default_protocol": "socks5", "status": "active",
	}, http.StatusCreated, &pool)
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": pool.ID, "text": "198.51.100.10:1080\n198.51.100.11:1080",
	}, http.StatusCreated, nil)

	var deletion struct {
		DeletedProxies int `json:"deleted_proxies"`
	}
	putJSON(t, handler, http.MethodDelete, fmt.Sprintf("/api/proxy-pools/%d", pool.ID), nil, http.StatusOK, &deletion)
	if deletion.DeletedProxies != 2 {
		t.Fatalf("deleted proxies = %d, want 2", deletion.DeletedProxies)
	}

	var pools []proxyPool
	putJSON(t, handler, http.MethodGet, "/api/proxy-pools", nil, http.StatusOK, &pools)
	for _, item := range pools {
		if item.ID == pool.ID {
			t.Fatalf("deleted pool %d still listed", pool.ID)
		}
	}
	var proxies []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	for _, item := range proxies {
		if item.PoolID == pool.ID {
			t.Fatalf("proxy %d of deleted pool still listed", item.ID)
		}
	}
}

func TestProxyPoolUpdateSynchronizesExistingProxyProtocols(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var pool proxyPool
	putJSON(t, handler, http.MethodPost, "/api/proxy-pools", map[string]any{
		"name": "servers", "source_type": "manual", "default_protocol": "socks5", "status": "active",
	}, http.StatusCreated, &pool)
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": pool.ID,
		"text":    "192.0.2.10:22:root:first-password\n192.0.2.11:22:root:second-password",
	}, http.StatusCreated, nil)

	var updated proxyPool
	putJSON(t, handler, http.MethodPut, fmt.Sprintf("/api/proxy-pools/%d", pool.ID), map[string]any{
		"name": "servers", "source_type": "manual", "default_protocol": "ssh", "status": "active",
	}, http.StatusOK, &updated)
	if updated.DefaultProtocol != "ssh" || updated.ProtocolSynced != 2 {
		t.Fatalf("updated pool = %+v, want ssh with 2 synchronized proxies", updated)
	}

	rows, err := a.db.Query(`SELECT protocol, username, password FROM proxies WHERE pool_id = ? AND deleted_at IS NULL ORDER BY id`, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	passwords := []string{"first-password", "second-password"}
	index := 0
	for rows.Next() {
		var protocol, username, password string
		if err := rows.Scan(&protocol, &username, &password); err != nil {
			t.Fatal(err)
		}
		if protocol != "ssh" || username != "root" || index >= len(passwords) || password != passwords[index] {
			t.Fatalf("proxy %d = %s://%s:%s", index, protocol, username, password)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if index != 2 {
		t.Fatalf("synchronized proxies = %d, want 2", index)
	}
}

func TestProxyPoolSingleUseDefaultsOnAndUpdatePreservesOmittedValue(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var pool proxyPool
	putJSON(t, handler, http.MethodPost, "/api/proxy-pools", map[string]any{
		"name": "single-use-default", "source_type": "manual", "default_protocol": "http", "status": "active",
	}, http.StatusCreated, &pool)
	if !pool.SingleUseEnabled {
		t.Fatalf("new proxy pool single_use_enabled = false, want true")
	}

	putJSON(t, handler, http.MethodPut, fmt.Sprintf("/api/proxy-pools/%d", pool.ID), map[string]any{
		"name": "single-use-default", "source_type": "manual", "default_protocol": "http", "status": "active",
		"single_use_enabled": false,
	}, http.StatusOK, &pool)
	if pool.SingleUseEnabled {
		t.Fatalf("updated proxy pool single_use_enabled = true, want false")
	}

	putJSON(t, handler, http.MethodPut, fmt.Sprintf("/api/proxy-pools/%d", pool.ID), map[string]any{
		"name": "single-use-default-renamed", "source_type": "manual", "default_protocol": "http", "status": "active",
	}, http.StatusOK, &pool)
	if pool.SingleUseEnabled {
		t.Fatalf("omitted single_use_enabled reset existing false value")
	}
}

func TestSingleUseProxyCannotMoveToAnotherAccountAfterDelete(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	createAccount := func(name string, wantStatus int) account {
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

	first := createAccount("single-use-first@example.com", http.StatusCreated)
	putJSON(t, handler, http.MethodDelete, fmt.Sprintf("/api/accounts/%d", first.ID), nil, http.StatusNoContent, nil)
	createAccount("single-use-blocked@example.com", http.StatusConflict)

	var pools []proxyPool
	putJSON(t, handler, http.MethodGet, "/api/proxy-pools", nil, http.StatusOK, &pools)
	defaultPool := findProxyPool(t, pools, 1)
	if !defaultPool.SingleUseEnabled || defaultPool.AvailableCount != 0 {
		t.Fatalf("single-use pool after release = %+v, want enabled with zero available proxies", defaultPool)
	}
	var proxies []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	usedProxy := findProxyRecord(t, proxies, proxyID)
	if !usedProxy.SingleUseEnabled || usedProxy.AssignedTo != "" || usedProxy.UsedAccountCount != 1 {
		t.Fatalf("released single-use proxy = %+v", usedProxy)
	}

	var reusablePool proxyPool
	putJSON(t, handler, http.MethodPut, "/api/proxy-pools/1", map[string]any{
		"name": "default", "source_type": "manual", "default_protocol": "socks5", "status": "active",
		"single_use_enabled": false,
	}, http.StatusOK, &reusablePool)
	if reusablePool.SingleUseEnabled {
		t.Fatal("proxy pool did not switch to reusable mode")
	}
	createAccount("reusable-second@example.com", http.StatusCreated)
}

func TestSingleUseProxyCannotBeDeletedAndReimported(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var host string
	var port int
	if err := a.db.QueryRow(`SELECT host, port FROM proxies WHERE id = ?`, proxyID).Scan(&host, &port); err != nil {
		t.Fatal(err)
	}
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "single-use-delete@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "delete-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	putJSON(t, handler, http.MethodDelete, fmt.Sprintf("/api/accounts/%d", created.ID), nil, http.StatusNoContent, nil)
	putJSON(t, handler, http.MethodDelete, fmt.Sprintf("/api/proxies/%d", proxyID), nil, http.StatusOK, nil)

	var imported map[string]int
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": 1, "default_protocol": "http", "text": fmt.Sprintf("http://%s:%d", host, port),
	}, http.StatusCreated, &imported)
	if imported["created"] != 0 || imported["skipped"] != 1 || imported["invalid"] != 0 {
		t.Fatalf("reimported deleted single-use proxy = %+v", imported)
	}
}

func findProxyPool(t *testing.T, items []proxyPool, id int64) proxyPool {
	t.Helper()
	for _, item := range items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("proxy pool %d not found in %+v", id, items)
	return proxyPool{}
}

func TestPersistProxyTestResultsWritesBatchAndPreservesDisabled(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	ids := make([]int64, 0, 3)
	for index, status := range []string{"active", "active", "disabled"} {
		result, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, status) VALUES (1, ?, 'http', ?, ?, ?)`, fmt.Sprintf("batch-%d", index), fmt.Sprintf("192.0.2.%d", index+20), 8000+index, status)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		ids = append(ids, id)
	}
	if err := a.persistProxyTestResults([]proxyTestResult{
		{ID: ids[0], Success: true, IP: "203.0.113.10", LatencyMS: 42},
		{ID: ids[1], Error: "connection refused"},
		{ID: ids[2], Success: true, IP: "203.0.113.12", LatencyMS: 21},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := a.db.Query(`SELECT status, exit_ip, last_error FROM proxies WHERE id IN (?, ?, ?) ORDER BY id`, ids[0], ids[1], ids[2])
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := [][3]string{}
	for rows.Next() {
		var status, exitIP, lastError string
		if err := rows.Scan(&status, &exitIP, &lastError); err != nil {
			t.Fatal(err)
		}
		got = append(got, [3]string{status, exitIP, lastError})
	}
	if len(got) != 3 || got[0] != [3]string{"active", "203.0.113.10", ""} || got[1] != [3]string{"error", "", "connection refused"} || got[2][0] != "disabled" {
		t.Fatalf("persisted proxy results = %#v", got)
	}
}

func TestProxyPoolUpdateRejectsProtocolMergeCollision(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var pool proxyPool
	putJSON(t, handler, http.MethodPost, "/api/proxy-pools", map[string]any{
		"name": "mixed", "source_type": "manual", "default_protocol": "socks5", "status": "active",
	}, http.StatusCreated, &pool)
	for _, protocol := range []string{"socks5", "http"} {
		if _, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, username, password) VALUES (?, ?, ?, '192.0.2.20', 8080, 'user', 'password')`, pool.ID, protocol, protocol); err != nil {
			t.Fatal(err)
		}
	}

	putJSON(t, handler, http.MethodPut, fmt.Sprintf("/api/proxy-pools/%d", pool.ID), map[string]any{
		"name": "mixed", "source_type": "manual", "default_protocol": "https", "status": "active",
	}, http.StatusConflict, nil)

	var defaultProtocol string
	if err := a.db.QueryRow(`SELECT default_protocol FROM proxy_pools WHERE id = ?`, pool.ID).Scan(&defaultProtocol); err != nil {
		t.Fatal(err)
	}
	if defaultProtocol != "socks5" {
		t.Fatalf("default protocol = %q after rejected update, want socks5", defaultProtocol)
	}
	var distinctProtocols int
	if err := a.db.QueryRow(`SELECT COUNT(DISTINCT protocol) FROM proxies WHERE pool_id = ? AND deleted_at IS NULL`, pool.ID).Scan(&distinctProtocols); err != nil {
		t.Fatal(err)
	}
	if distinctProtocols != 2 {
		t.Fatalf("protocols after rejected update = %d, want 2", distinctProtocols)
	}
}

func TestProxyPoolDeleteRejectsAssignedPool(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	if _, err := a.db.Exec(`INSERT INTO accounts (name, proxy_pool_id) VALUES ('bound', 1)`); err != nil {
		t.Fatal(err)
	}
	putJSON(t, a.routes(), http.MethodDelete, "/api/proxy-pools/1", nil, http.StatusConflict, nil)
	var deleted sql.NullString
	if err := a.db.QueryRow(`SELECT deleted_at FROM proxy_pools WHERE id = 1`).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted.Valid {
		t.Fatalf("assigned pool was deleted at %q", deleted.String)
	}
}

func TestBatchProxyTestRecoversWorkingErrorProxy(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ip": "198.51.100.7"})
	}))
	defer target.Close()
	previousEndpoint := proxyTestEndpoint
	proxyTestEndpoint = target.URL
	defer func() { proxyTestEndpoint = previousEndpoint }()

	workingID, _ := createCountingForwardProxy(t, a)
	if _, err := a.db.Exec(`UPDATE proxies SET status = 'error' WHERE id = ?`, workingID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, status) VALUES (1, 'unreachable', 'http', '127.0.0.1', 1, 'error')`); err != nil {
		t.Fatal(err)
	}
	var result proxyBatchTestResponse
	putJSON(t, a.routes(), http.MethodPost, "/api/proxies/batch-test", map[string]any{
		"pool_id": 1, "concurrency": 2,
	}, http.StatusOK, &result)
	if result.Total != 2 || result.Success != 1 || result.Failed != 1 {
		t.Fatalf("batch proxy result = %+v", result)
	}
	var status, exitIP string
	if err := a.db.QueryRow(`SELECT status, exit_ip FROM proxies WHERE id = ?`, workingID).Scan(&status, &exitIP); err != nil {
		t.Fatal(err)
	}
	if status != "active" || exitIP != "198.51.100.7" {
		t.Fatalf("working proxy status/ip = %q/%q", status, exitIP)
	}
}

func TestProxyTestRejectsActiveRuntimeReservationBeforeNetwork(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	fixture := newRuntimeProxyReservationFixture(t, "proxy-test-runtime-fence")
	forwarderID, calls := createCountingForwardProxy(t, fixture.app)
	pointProxyAtForwarder(t, fixture.app, fixture.proxyID, forwarderID, "runtime-probe-owner")
	setProxyTestEndpoint(t, "198.51.100.20")

	putJSON(t, fixture.app.routes(), http.MethodPost,
		"/api/proxies/"+strconv.FormatInt(fixture.proxyID, 10)+"/test", nil, http.StatusConflict, nil)
	if got := calls.Load(); got != 0 {
		t.Fatalf("runtime-owned single proxy emitted %d network requests, want 0", got)
	}
}

func TestProxyTestRejectsPendingRuntimeOwnerBeforeNetwork(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "proxy-test-pending-runtime-fence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID, calls := createCountingForwardProxy(t, a)
	if _, err := a.db.Exec(`INSERT INTO accounts
		(name, credentials_json, execution_migration_status, runtime_status, proxy_id)
		VALUES ('pending-proxy-test-owner', '{}', 'migrating', 'provisioning', ?)`, proxyID); err != nil {
		t.Fatal(err)
	}
	setProxyTestEndpoint(t, "198.51.100.21")

	putJSON(t, a.routes(), http.MethodPost, "/api/proxies/"+strconv.FormatInt(proxyID, 10)+"/test",
		nil, http.StatusConflict, nil)
	if got := calls.Load(); got != 0 {
		t.Fatalf("pending runtime-owned single proxy emitted %d network requests, want 0", got)
	}
}

func TestBatchProxyTestRejectsRuntimeOwnedTargetBeforeAnyNetwork(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	fixture := newRuntimeProxyReservationFixture(t, "proxy-batch-test-runtime-fence")
	legacyID, calls := createCountingForwardProxy(t, fixture.app)
	pointProxyAtForwarder(t, fixture.app, fixture.proxyID, legacyID, "runtime-batch-owner")
	setProxyTestEndpoint(t, "198.51.100.22")

	putJSON(t, fixture.app.routes(), http.MethodPost, "/api/proxies/batch-test", map[string]any{
		"ids": []int64{legacyID, fixture.proxyID}, "concurrency": 2,
	}, http.StatusConflict, nil)
	if got := calls.Load(); got != 0 {
		t.Fatalf("rejected mixed batch emitted %d network requests, want 0", got)
	}
}

func TestBatchProxyTestRejectsPendingRuntimeOwnerBeforeAnyNetwork(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "proxy-batch-test-pending-runtime-fence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	runtimeID, runtimeCalls := createCountingForwardProxy(t, a)
	legacyID, legacyCalls := createCountingForwardProxy(t, a)
	if _, err := a.db.Exec(`INSERT INTO accounts
		(name, credentials_json, execution_migration_status, runtime_status, proxy_id)
		VALUES ('pending-batch-proxy-test-owner', '{}', 'migrating', 'provisioning', ?)`, runtimeID); err != nil {
		t.Fatal(err)
	}
	setProxyTestEndpoint(t, "198.51.100.24")

	putJSON(t, a.routes(), http.MethodPost, "/api/proxies/batch-test", map[string]any{
		"ids": []int64{legacyID, runtimeID}, "concurrency": 2,
	}, http.StatusConflict, nil)
	if runtimeCalls.Load() != 0 || legacyCalls.Load() != 0 {
		t.Fatalf("rejected pending-owner batch emitted runtime/legacy requests = %d/%d, want 0/0",
			runtimeCalls.Load(), legacyCalls.Load())
	}
}

func TestProxyTestLegacyProxyStillProbesAndPersists(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "proxy-test-legacy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID, calls := createCountingForwardProxy(t, a)
	setProxyTestEndpoint(t, "198.51.100.23")

	var result proxyTestResult
	putJSON(t, a.routes(), http.MethodPost, "/api/proxies/"+strconv.FormatInt(proxyID, 10)+"/test",
		nil, http.StatusOK, &result)
	if !result.Success || result.IP != "198.51.100.23" || calls.Load() != 1 {
		t.Fatalf("legacy proxy probe = %+v, network calls=%d", result, calls.Load())
	}
	var status, exitIP string
	if err := a.db.QueryRow(`SELECT status, exit_ip FROM proxies WHERE id = ?`, proxyID).Scan(&status, &exitIP); err != nil {
		t.Fatal(err)
	}
	if status != "active" || exitIP != "198.51.100.23" {
		t.Fatalf("legacy proxy persisted status/ip = %q/%q", status, exitIP)
	}
}

func pointProxyAtForwarder(t *testing.T, a *app, proxyID, forwarderID int64, username string) {
	t.Helper()
	var host string
	var port int
	if err := a.db.QueryRow(`SELECT host, port FROM proxies WHERE id = ?`, forwarderID).Scan(&host, &port); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE proxies SET protocol = 'http', host = ?, port = ?, username = ?, password = ''
		WHERE id = ?`, host, port, username, proxyID); err != nil {
		t.Fatal(err)
	}
}

func setProxyTestEndpoint(t *testing.T, ip string) {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ip": ip})
	}))
	previous := proxyTestEndpoint
	proxyTestEndpoint = target.URL
	t.Cleanup(func() {
		proxyTestEndpoint = previous
		target.Close()
	})
}
