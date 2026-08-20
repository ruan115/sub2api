package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestAccountHealthRefreshUpdatesQuotaAndRestoresAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer live-token" {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"five_hour": map[string]any{"utilization": 25.0, "resets_at": "2030-01-01T01:00:00Z"},
			"seven_day": map[string]any{"utilization": 50.0, "resets_at": "2030-01-07T01:00:00Z"},
		})
	}))
	defer upstream.Close()
	previousUsageEndpoint := claudeUsageEndpoint
	claudeUsageEndpoint = upstream.URL
	defer func() { claudeUsageEndpoint = previousUsageEndpoint }()

	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "health@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "live-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)
	a.markAccountReauth(created.ID, "previous rejection")

	var result accountHealthResult
	putJSON(t, handler, http.MethodPost, "/api/accounts/health/refresh", map[string]any{"ids": []int64{created.ID}}, http.StatusOK, &result)
	if result.Checked != 1 || result.Healthy != 1 || result.Failed != 0 {
		t.Fatalf("health result = %+v", result)
	}
	var accounts []account
	putJSON(t, handler, http.MethodGet, "/api/accounts", nil, http.StatusOK, &accounts)
	if len(accounts) != 1 || accounts[0].DispatchStatus != "normal" || accounts[0].AuthCheckedAt == "" {
		t.Fatalf("restored account = %+v", accounts)
	}
	if accounts[0].Quota5H != 25 || accounts[0].Quota7D != 50 || accounts[0].InvalidatedAt != "" {
		t.Fatalf("restored quota/lifecycle = %+v", accounts[0])
	}
}

func TestAccountHealthRefreshMarksRejectedTokenDead(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"message": "token expired"}})
	}))
	defer upstream.Close()
	previousUsageEndpoint := claudeUsageEndpoint
	claudeUsageEndpoint = upstream.URL
	defer func() { claudeUsageEndpoint = previousUsageEndpoint }()

	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "dead@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "expired-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)

	var result accountHealthResult
	putJSON(t, handler, http.MethodPost, "/api/accounts/health/refresh", map[string]any{"ids": []int64{created.ID}}, http.StatusOK, &result)
	if result.Checked != 1 || result.Healthy != 0 || result.Failed != 1 {
		t.Fatalf("health result = %+v", result)
	}
	var accounts []account
	putJSON(t, handler, http.MethodGet, "/api/accounts", nil, http.StatusOK, &accounts)
	if len(accounts) != 1 || accounts[0].DispatchStatus != "error" || accounts[0].AuthStatus != "reauth_required" {
		t.Fatalf("rejected account = %+v", accounts)
	}
}
