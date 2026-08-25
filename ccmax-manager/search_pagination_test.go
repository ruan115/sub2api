package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

func TestAuthorizationSearchByAccountAndIPKeepsServerPagination(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	rows := []struct {
		account string
		proxyIP string
		client  string
	}{
		{account: "alpha@example.com", proxyIP: "198.51.100.10", client: "203.0.113.10"},
		{account: "alpha-backup@example.com", proxyIP: "198.51.100.10", client: "203.0.113.11"},
		{account: "beta@example.com", proxyIP: "198.51.100.20", client: "203.0.113.20"},
	}
	for _, row := range rows {
		if _, err := a.db.Exec(`INSERT INTO authorization_logs
			(account_name, proxy_ip, method, success, status_message, subscription_type, client_ip)
			VALUES (?, ?, 'oauth', 1, 'authorization succeeded', 'max', ?)`, row.account, row.proxyIP, row.client); err != nil {
			t.Fatal(err)
		}
	}

	var first authorizationStats
	putJSON(t, a.routes(), http.MethodGet, "/api/authorization-logs?search=198.51.100.10&page=1&page_size=1", nil, http.StatusOK, &first)
	if first.Summary.Total != 2 || first.TotalPages != 2 || first.Page != 1 || len(first.Items) != 1 {
		t.Fatalf("first authorization page = %+v", first)
	}
	var second authorizationStats
	putJSON(t, a.routes(), http.MethodGet, "/api/authorization-logs?search=198.51.100.10&page=2&page_size=1", nil, http.StatusOK, &second)
	if second.Summary.Total != 2 || second.Page != 2 || len(second.Items) != 1 || second.Items[0].ID == first.Items[0].ID {
		t.Fatalf("second authorization page = %+v", second)
	}
	var accountMatch authorizationStats
	putJSON(t, a.routes(), http.MethodGet, "/api/authorization-logs?search=beta@example.com", nil, http.StatusOK, &accountMatch)
	if accountMatch.Summary.Total != 1 || len(accountMatch.Items) != 1 || accountMatch.Items[0].AccountName != "beta@example.com" {
		t.Fatalf("authorization account search = %+v", accountMatch)
	}
}

func TestBillingSearchByAccountAndProxyIPKeepsServerPagination(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": 1,
		"text":    "198.51.100.31:1080\n198.51.100.32:1080",
	}, http.StatusCreated, nil)
	var proxies []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	if len(proxies) < 2 {
		t.Fatalf("proxies = %d, want at least 2", len(proxies))
	}
	proxyByHost := map[string]proxyRecord{}
	for _, proxy := range proxies {
		proxyByHost[proxy.Host] = proxy
	}
	for host, exitIP := range map[string]string{"198.51.100.31": "203.0.113.31", "198.51.100.32": "203.0.113.32"} {
		proxy := proxyByHost[host]
		if proxy.ID == 0 {
			t.Fatalf("proxy host %s was not imported", host)
		}
		if _, err := a.db.Exec(`UPDATE proxies SET exit_ip = ? WHERE id = ?`, exitIP, proxy.ID); err != nil {
			t.Fatal(err)
		}
	}

	createAccount := func(name, host string) account {
		t.Helper()
		proxy := proxyByHost[host]
		var created account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": "token-" + host}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxy.ID,
		}, http.StatusCreated, &created)
		return created
	}
	alpha := createAccount("alpha-ledger@example.com", "198.51.100.31")
	beta := createAccount("beta-ledger@example.com", "198.51.100.32")
	for index := 1; index <= 2; index++ {
		putJSON(t, handler, http.MethodPost, "/api/usage", map[string]any{
			"request_id": fmt.Sprintf("alpha-request-%d", index), "purpose_key": "default", "account_id": alpha.ID,
			"model": "claude-test", "input_tokens": 10, "output_tokens": 2,
		}, http.StatusCreated, nil)
	}
	putJSON(t, handler, http.MethodPost, "/api/usage", map[string]any{
		"request_id": "beta-request", "purpose_key": "default", "account_id": beta.ID,
		"model": "claude-test", "input_tokens": 20, "output_tokens": 3,
	}, http.StatusCreated, nil)

	var first struct {
		Items      []usageLog `json:"items"`
		Total      int64      `json:"total"`
		Page       int        `json:"page"`
		TotalPages int        `json:"total_pages"`
	}
	putJSON(t, handler, http.MethodGet, "/api/usage?search=203.0.113.31&page=1&page_size=1", nil, http.StatusOK, &first)
	if first.Total != 2 || first.Page != 1 || first.TotalPages != 2 || len(first.Items) != 1 || first.Items[0].ProxyIP != "203.0.113.31" {
		t.Fatalf("billing usage first page = %+v", first)
	}
	var second struct {
		Items []usageLog `json:"items"`
		Total int64      `json:"total"`
		Page  int        `json:"page"`
	}
	putJSON(t, handler, http.MethodGet, "/api/usage?search=alpha-ledger&page=2&page_size=1", nil, http.StatusOK, &second)
	if second.Total != 2 || second.Page != 2 || len(second.Items) != 1 || second.Items[0].AccountID != alpha.ID {
		t.Fatalf("billing usage second page = %+v", second)
	}
	var summary billingSummary
	putJSON(t, handler, http.MethodGet, "/api/billing?search=203.0.113.32", nil, http.StatusOK, &summary)
	if summary.Totals.Requests != 1 || len(summary.ByAccount) != 1 || summary.ByAccount[0].Key != fmt.Sprint(beta.ID) {
		t.Fatalf("billing IP search summary = %+v", summary)
	}
}
