package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
