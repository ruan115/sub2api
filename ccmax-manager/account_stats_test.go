package main

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAccountSurvivalSecondsUsesNowUntilInvalidated(t *testing.T) {
	started := time.Now().UTC().Add(-10 * time.Second)
	live := accountSurvivalSeconds(started.Format(time.RFC3339Nano), "", 0)
	if live < 9 || live > 11 {
		t.Fatalf("live survival = %d seconds, want about 10", live)
	}
	invalidated := started.Add(4 * time.Second)
	fixed := accountSurvivalSeconds(started.Format(time.RFC3339Nano), invalidated.Format(time.RFC3339Nano), 4)
	if fixed != 4 {
		t.Fatalf("fixed survival = %d seconds, want 4", fixed)
	}
}

func TestAccountSurvivalAccumulatesAcrossAuthorizationCycles(t *testing.T) {
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
		"name": "survival@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "first-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)

	firstStarted := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE accounts SET onboarded_at = ? WHERE id = ?`, firstStarted, created.ID); err != nil {
		t.Fatal(err)
	}
	a.markAccountReauth(created.ID, "first cycle ended")
	if err := a.saveClaudeToken(created.ID, "oauth", &claudeTokenInfo{AccessToken: "second-token", ExpiresAt: 4_102_444_800}, false); err != nil {
		t.Fatal(err)
	}
	secondStarted := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE accounts SET onboarded_at = ? WHERE id = ?`, secondStarted, created.ID); err != nil {
		t.Fatal(err)
	}
	a.markAccountReauth(created.ID, "second cycle ended")

	var accounts []account
	putJSON(t, handler, http.MethodGet, "/api/accounts", nil, http.StatusOK, &accounts)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	if accounts[0].SurvivalSeconds < 718 || accounts[0].SurvivalSeconds > 722 {
		t.Fatalf("accumulated survival = %d, want about 720 seconds", accounts[0].SurvivalSeconds)
	}
	if accounts[0].SurvivalTotal != accounts[0].SurvivalSeconds {
		t.Fatalf("stored total = %d, displayed = %d", accounts[0].SurvivalTotal, accounts[0].SurvivalSeconds)
	}
}

func TestAccountStatisticsSubscriptionAndDispatchState(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"organization":{"subscription_type":"claude_max"}}`))
	accessToken := "header." + payload + ".signature"
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "stats@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": accessToken}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "account_price": 40.5, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	putJSON(t, handler, http.MethodPost, "/api/usage", map[string]any{
		"request_id": "stats-usage", "purpose_key": "default", "account_id": created.ID,
		"model": "claude-test", "input_tokens": 1000, "output_tokens": 500,
	}, http.StatusCreated, nil)

	var accounts []account
	putJSON(t, handler, http.MethodGet, "/api/accounts", nil, http.StatusOK, &accounts)
	if len(accounts) != 1 {
		t.Fatalf("account count = %d, want 1", len(accounts))
	}
	got := accounts[0]
	if got.SubscriptionType != "max" || got.AccountPrice != 40.5 || got.RequestCount != 1 || got.TotalBilledCost <= 0 {
		t.Fatalf("account statistics = %+v", got)
	}
	if got.DispatchStatus != "normal" || got.OnboardedAt == "" || got.SurvivalSeconds < 0 {
		t.Fatalf("account lifecycle = %+v", got)
	}

	a.markAccountReauth(got.ID, "token expired")
	var dashboard dashboard
	putJSON(t, handler, http.MethodGet, "/api/dashboard", nil, http.StatusOK, &dashboard)
	if dashboard.AccountsDead != 1 || dashboard.AccountsActive != 0 {
		t.Fatalf("dashboard states = %+v", dashboard)
	}
	if err := a.saveClaudeToken(got.ID, "oauth", &claudeTokenInfo{AccessToken: accessToken, ExpiresAt: 4_102_444_800, SubscriptionType: "max"}, false); err != nil {
		t.Fatal(err)
	}
	var onboardedEvents, invalidatedEvents int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_lifecycle_events WHERE account_id = ? AND event_type = 'onboarded'`, got.ID).Scan(&onboardedEvents); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_lifecycle_events WHERE account_id = ? AND event_type = 'invalidated'`, got.ID).Scan(&invalidatedEvents); err != nil {
		t.Fatal(err)
	}
	if onboardedEvents != 2 || invalidatedEvents != 1 {
		t.Fatalf("lifecycle events = onboarded %d, invalidated %d", onboardedEvents, invalidatedEvents)
	}
	var daily []dailyStat
	putJSON(t, handler, http.MethodGet, "/api/stats/daily?days=1", nil, http.StatusOK, &daily)
	if len(daily) != 1 || daily[0].AccountsOnboarded != 2 || daily[0].AccountsDied != 1 {
		t.Fatalf("daily lifecycle = %+v", daily)
	}
}

func TestBatchAuthorizationUsesExclusiveProxiesAndEmailNames(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/organizations":
			writeJSON(w, http.StatusOK, []map[string]any{{"uuid": "org-1", "raven_type": "team"}})
		case strings.HasSuffix(r.URL.Path, "/authorize"):
			cookie, err := r.Cookie("sessionKey")
			if err != nil {
				http.Error(w, "missing session", http.StatusUnauthorized)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"redirect_uri": "https://platform.claude.com/oauth/code/callback?code=" + url.QueryEscape(cookie.Value)})
		case r.URL.Path == "/token":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			code := strings.TrimSpace(body["code"].(string))
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "access-" + code, "refresh_token": "refresh-" + code,
				"expires_in": 3600, "organization": map[string]string{"uuid": "org-1", "raven_type": "team"},
				"account": map[string]string{"uuid": "account-" + code, "email_address": code + "@example.com"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	previousOrganizations := claudeOrganizationsEndpoint
	previousAuthorize := claudeSessionAuthorizeBaseURL
	previousToken := claudeTokenEndpoint
	claudeOrganizationsEndpoint = upstream.URL + "/organizations"
	claudeSessionAuthorizeBaseURL = upstream.URL + "/v1/oauth"
	claudeTokenEndpoint = upstream.URL + "/token"
	defer func() {
		claudeOrganizationsEndpoint = previousOrganizations
		claudeSessionAuthorizeBaseURL = previousAuthorize
		claudeTokenEndpoint = previousToken
	}()

	firstProxyID, firstCalls := createCountingForwardProxy(t, a)
	secondProxyID, secondCalls := createCountingForwardProxy(t, a)
	var result batchAuthorizationResponse
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-authorize", map[string]any{
		"session_keys": []string{"first", "second"}, "proxy_pool_id": 1,
		"group_ids": []string{"a"}, "auth_type": "oauth", "account_price": 25,
		"concurrency": 2, "base_rpm": 15,
	}, http.StatusOK, &result)
	if result.Success != 2 || result.Failed != 0 || len(result.Items) != 2 {
		t.Fatalf("batch result = %+v", result)
	}
	if firstCalls.Load() == 0 || secondCalls.Load() == 0 {
		t.Fatalf("authorization bypassed proxy: calls=%d/%d", firstCalls.Load(), secondCalls.Load())
	}
	if result.Items[0].Name != "first@example.com" || result.Items[1].Name != "second@example.com" {
		t.Fatalf("account names were not derived from token emails: %+v", result.Items)
	}
	if result.Items[0].Subscription != "team" || result.Items[1].Subscription != "team" {
		t.Fatalf("subscription types = %+v", result.Items)
	}
	if firstProxyID == secondProxyID {
		t.Fatal("test proxies are not unique")
	}
	var distinctProxyCount, authorizationCount int
	if err := a.db.QueryRow(`SELECT COUNT(DISTINCT proxy_id) FROM accounts WHERE deleted_at IS NULL`).Scan(&distinctProxyCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM authorization_logs WHERE success = 1`).Scan(&authorizationCount); err != nil {
		t.Fatal(err)
	}
	if distinctProxyCount != 2 || authorizationCount != 2 {
		t.Fatalf("proxy/auth counts = %d/%d", distinctProxyCount, authorizationCount)
	}
}

func TestVisiblePagesRestrictOrdinaryAndReadOnlyUsers(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "daily-reader", "name": "Daily Reader", "password": "reader-password",
		"role": "user", "status": "active", "allowed_group_ids": []string{"a"},
		"visible_pages": []string{"daily"}, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, nil)
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "account-reader", "name": "Account Reader", "password": "reader-password",
		"role": "readonly_admin", "status": "active", "visible_pages": []string{"accounts"}, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, nil)

	dailyCookie := loginCookie(t, handler, "daily-reader", "reader-password")
	requestJSON(t, handler, http.MethodGet, "/api/stats/daily?days=7", nil, dailyCookie, "", http.StatusOK, nil)
	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, dailyCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{}, dailyCookie, "", http.StatusForbidden, nil)

	readonlyCookie := loginCookie(t, handler, "account-reader", "reader-password")
	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, readonlyCookie, "", http.StatusOK, nil)
	requestJSON(t, handler, http.MethodGet, "/api/stats/daily?days=7", nil, readonlyCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodDelete, "/api/accounts/1", nil, readonlyCookie, "", http.StatusForbidden, nil)
}

func TestPendingAccountRemainsUnavailableAfterMigration(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	databasePath := filepath.Join(t.TempDir(), "test.db")
	a, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	putJSON(t, a.routes(), http.MethodPost, "/api/accounts", map[string]any{
		"name": "pending@example.com", "platform": "anthropic", "auth_type": "oauth",
		"status": "active", "schedulable": false, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, nil)
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	var accounts []account
	putJSON(t, reopened.routes(), http.MethodGet, "/api/accounts", nil, http.StatusOK, &accounts)
	if len(accounts) != 1 || accounts[0].DispatchStatus != "unavailable" || accounts[0].InvalidatedAt != "" {
		t.Fatalf("pending account after migration = %+v", accounts)
	}
}

func createCountingForwardProxy(t *testing.T, a *app) (int64, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		outgoing := r.Clone(r.Context())
		outgoing.RequestURI = ""
		outgoing.Header.Del("Proxy-Connection")
		response, err := transport.RoundTrip(outgoing)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		buffer := make([]byte, 32*1024)
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				_, _ = w.Write(buffer[:read])
			}
			if readErr != nil {
				break
			}
		}
	}))
	t.Cleanup(func() {
		server.Close()
		transport.CloseIdleConnections()
	})
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, status) VALUES (1, ?, 'http', ?, ?, 'active')`, "counting-proxy-"+portText, host, port)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id, &calls
}
