package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		if r.URL.Path == "/profile" {
			writeJSON(w, http.StatusOK, map[string]any{
				"account": map[string]bool{"has_claude_max": true},
				"organization": map[string]string{
					"organization_type": "claude_max",
					"rate_limit_tier":   "default_claude_max_5x",
				},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"five_hour": map[string]any{"utilization": 25.0, "resets_at": "2030-01-01T01:00:00Z"},
			"seven_day": map[string]any{"utilization": 50.0, "resets_at": "2030-01-07T01:00:00Z"},
		})
	}))
	defer upstream.Close()
	previousUsageEndpoint := claudeUsageEndpoint
	previousProfileEndpoint := claudeProfileEndpoint
	claudeUsageEndpoint = upstream.URL
	claudeProfileEndpoint = upstream.URL + "/profile"
	defer func() {
		claudeUsageEndpoint = previousUsageEndpoint
		claudeProfileEndpoint = previousProfileEndpoint
	}()

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
	if accounts[0].Quota5H != 25 || accounts[0].Quota7D != 50 || accounts[0].InvalidatedAt != "" || accounts[0].SubscriptionType != "max" || accounts[0].RateLimitTier != "default_claude_max_5x" {
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
	previousProfileEndpoint := claudeProfileEndpoint
	claudeUsageEndpoint = upstream.URL
	claudeProfileEndpoint = upstream.URL
	defer func() {
		claudeUsageEndpoint = previousUsageEndpoint
		claudeProfileEndpoint = previousProfileEndpoint
	}()

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

func TestQuotaRefreshPreservesTokenRefreshFailureAndMarksAccountError(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	refreshCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/profile":
			writeJSON(w, http.StatusOK, map[string]any{})
		case "/token":
			refreshCalls++
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":             "invalid_grant",
				"error_description": "Refresh token has already been used",
			})
		default:
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error": map[string]any{"type": "authentication_error", "message": "OAuth access token has been revoked."},
			})
		}
	}))
	defer upstream.Close()
	previousUsageEndpoint := claudeUsageEndpoint
	previousProfileEndpoint := claudeProfileEndpoint
	previousTokenEndpoint := claudeTokenEndpoint
	claudeUsageEndpoint = upstream.URL + "/usage"
	claudeProfileEndpoint = upstream.URL + "/profile"
	claudeTokenEndpoint = upstream.URL + "/token"
	defer func() {
		claudeUsageEndpoint = previousUsageEndpoint
		claudeProfileEndpoint = previousProfileEndpoint
		claudeTokenEndpoint = previousTokenEndpoint
	}()

	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "refresh-failure@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{
			"access_token": "revoked-access-token", "refresh_token": "expired-refresh-token",
			"expires_at": time.Now().Add(time.Hour).Unix(),
		},
		"extra": map[string]any{}, "status": "active", "schedulable": true,
		"concurrency": 1, "priority": 10, "rate_multiplier": 1,
		"group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)

	response := requestJSON(t, handler, http.MethodPost, fmt.Sprintf("/api/accounts/%d/quota/refresh", created.ID), map[string]any{}, nil, "", http.StatusBadGateway, nil)
	body := response.Body.String()
	for _, expected := range []string{"Claude token refresh failed (status 400)", "invalid_grant", "Refresh token has already been used"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("refresh response %q does not contain %q", body, expected)
		}
	}
	if strings.Contains(body, "OAuth access token has been revoked") {
		t.Fatalf("refresh response was overwritten by the original usage failure: %s", body)
	}
	if refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", refreshCalls)
	}

	var accounts []account
	putJSON(t, handler, http.MethodGet, "/api/accounts", nil, http.StatusOK, &accounts)
	if len(accounts) != 1 || accounts[0].Status != "error" || accounts[0].DispatchStatus != "error" || accounts[0].AuthStatus != "reauth_required" {
		t.Fatalf("refresh-rejected account = %+v", accounts)
	}
	for _, expected := range []string{"invalid_grant", "Refresh token has already been used"} {
		if !strings.Contains(accounts[0].AuthError, expected) || !strings.Contains(accounts[0].ErrorMessage, expected) {
			t.Fatalf("account did not preserve refresh error detail: %+v", accounts[0])
		}
	}
}

func TestProfileSyncCannotRollBackRotatedCredentials(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	profile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		writeJSON(w, http.StatusOK, map[string]any{
			"organization": map[string]string{"organization_type": "claude_max", "rate_limit_tier": "default_claude_max_5x"},
		})
	}))
	defer profile.Close()
	previousProfileEndpoint := claudeProfileEndpoint
	claudeProfileEndpoint = profile.URL
	defer func() { claudeProfileEndpoint = previousProfileEndpoint }()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "profile-race@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "profile-old-access", "refresh_token": "profile-old-refresh", "expires_at": time.Now().Add(time.Hour).Unix()},
		"extra":       map[string]any{}, "status": "active", "schedulable": true, "concurrency": 1,
		"priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)
	var oldCredentials string
	if err := a.db.QueryRow(`SELECT credentials_json FROM accounts WHERE id = ?`, created.ID).Scan(&oldCredentials); err != nil {
		t.Fatal(err)
	}
	proxyURL, err := a.proxyURL(proxyID)
	if err != nil {
		t.Fatal(err)
	}
	errorsCh := make(chan error, 1)
	go func() {
		errorsCh <- a.syncClaudeAccountProfile(context.Background(), created.ID, oldCredentials, proxyURL.String())
	}()
	<-started
	newCredentials := `{"access_token":"profile-new-access","refresh_token":"profile-new-refresh","expires_at":4102444800}`
	if _, err := a.db.Exec(`UPDATE accounts SET credentials_json = ? WHERE id = ?`, newCredentials, created.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errorsCh; err != nil {
		t.Fatal(err)
	}
	var storedCredentials, subscription, tier string
	if err := a.db.QueryRow(`SELECT credentials_json, subscription_type, rate_limit_tier FROM accounts WHERE id = ?`, created.ID).Scan(&storedCredentials, &subscription, &tier); err != nil {
		t.Fatal(err)
	}
	if storedCredentials != newCredentials || subscription != "max" || tier != "default_claude_max_5x" {
		t.Fatalf("profile sync rolled back token state: credentials=%s subscription=%s tier=%s", storedCredentials, subscription, tier)
	}
}
