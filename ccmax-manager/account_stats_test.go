package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	var status string
	var schedulable int
	if err := a.db.QueryRow(`SELECT status, schedulable FROM accounts WHERE id = ?`, got.ID).Scan(&status, &schedulable); err != nil {
		t.Fatal(err)
	}
	if status != "active" || schedulable != 1 {
		t.Fatalf("reauthorized account status/schedulable = %s/%d", status, schedulable)
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

func TestDailyStatsSupportsInclusiveDateRange(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var daily []dailyStat
	putJSON(t, handler, http.MethodGet, "/api/stats/daily?from=2026-08-01&to=2026-08-03", nil, http.StatusOK, &daily)
	if len(daily) != 3 || daily[0].Date != "2026-08-03" || daily[1].Date != "2026-08-02" || daily[2].Date != "2026-08-01" {
		t.Fatalf("daily date range = %+v", daily)
	}
	putJSON(t, handler, http.MethodGet, "/api/stats/daily?from=2026-08-03&to=2026-08-01", nil, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodGet, "/api/stats/daily?from=2025-01-01&to=2026-08-01", nil, http.StatusBadRequest, nil)
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
		case r.URL.Path == "/profile":
			writeJSON(w, http.StatusOK, map[string]any{
				"organization": map[string]string{"organization_type": "team", "rate_limit_tier": "default_team"},
			})
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
			identity := code
			if code == "duplicate-first" {
				identity = "first"
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "access-" + code, "refresh_token": "refresh-" + code,
				"expires_in": 3600, "organization": map[string]string{"uuid": "org-1", "raven_type": "team"},
				"account": map[string]string{"uuid": "account-" + identity, "email_address": identity + "@example.com"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	previousOrganizations := claudeOrganizationsEndpoint
	previousAuthorize := claudeSessionAuthorizeBaseURL
	previousToken := claudeTokenEndpoint
	previousProfile := claudeProfileEndpoint
	claudeOrganizationsEndpoint = upstream.URL + "/organizations"
	claudeSessionAuthorizeBaseURL = upstream.URL + "/v1/oauth"
	claudeTokenEndpoint = upstream.URL + "/token"
	claudeProfileEndpoint = upstream.URL + "/profile"
	defer func() {
		claudeOrganizationsEndpoint = previousOrganizations
		claudeSessionAuthorizeBaseURL = previousAuthorize
		claudeTokenEndpoint = previousToken
		claudeProfileEndpoint = previousProfile
	}()

	firstProxyID, firstCalls := createCountingForwardProxy(t, a)
	secondProxyID, secondCalls := createCountingForwardProxy(t, a)
	thirdProxyID, thirdCalls := createCountingForwardProxy(t, a)
	var strategy struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "onboarding-fixed", "rpm_limit": 15, "rpm_strategy": "fixed", "dispatch_mode": "serial",
	}, http.StatusCreated, &strategy)
	var result batchAuthorizationResponse
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-authorize", map[string]any{
		"session_keys": []string{"first", "second", "duplicate-first"}, "proxy_pool_id": 1,
		"group_ids": []string{"a"}, "auth_type": "oauth", "account_price": 25,
		"concurrency": 2, "base_rpm": 15,
		"rpm_strategy": "fixed", "rpm_sticky_buffer": 4, "strategy_id": strategy.ID,
	}, http.StatusOK, &result)
	if result.Success != 3 || result.Updated != 1 || result.Skipped != 0 || result.Failed != 0 || len(result.Items) != 3 {
		t.Fatalf("batch result = %+v", result)
	}
	if firstCalls.Load() == 0 || secondCalls.Load() == 0 || thirdCalls.Load() == 0 {
		t.Fatalf("authorization bypassed proxy: calls=%d/%d/%d", firstCalls.Load(), secondCalls.Load(), thirdCalls.Load())
	}
	if result.Items[0].Name != "first@example.com" || result.Items[1].Name != "second@example.com" {
		t.Fatalf("account names were not derived from token emails: %+v", result.Items)
	}
	if result.Items[0].Subscription != "team" || result.Items[1].Subscription != "team" || !result.Items[2].Updated || !result.Items[2].Success || result.Items[2].AccountID != result.Items[0].AccountID {
		t.Fatalf("subscription types = %+v", result.Items)
	}
	if firstProxyID == secondProxyID || firstProxyID == thirdProxyID || secondProxyID == thirdProxyID {
		t.Fatal("test proxies are not unique")
	}
	var accountCount, distinctProxyCount, authorizationCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE deleted_at IS NULL`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(DISTINCT proxy_id) FROM accounts WHERE deleted_at IS NULL`).Scan(&distinctProxyCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM authorization_logs WHERE success = 1`).Scan(&authorizationCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 2 || distinctProxyCount != 2 || authorizationCount != 3 {
		t.Fatalf("account/proxy/auth counts = %d/%d/%d", accountCount, distinctProxyCount, authorizationCount)
	}
	var firstTier string
	if err := a.db.QueryRow(`SELECT rate_limit_tier FROM accounts WHERE id = ?`, result.Items[0].AccountID).Scan(&firstTier); err != nil {
		t.Fatal(err)
	}
	if firstTier != "default_team" {
		t.Fatalf("rate limit tier = %q, want default_team", firstTier)
	}
	var firstHint, secondHint, firstCredentials string
	var updatedProxyID int64
	if err := a.db.QueryRow(`SELECT source_sk_hint, credentials_json, proxy_id FROM accounts WHERE id = ?`, result.Items[0].AccountID).Scan(&firstHint, &firstCredentials, &updatedProxyID); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT source_sk_hint FROM accounts WHERE id = ?`, result.Items[1].AccountID).Scan(&secondHint); err != nil {
		t.Fatal(err)
	}
	if firstHint != sourceSKHint("duplicate-first") || secondHint != sourceSKHint("second") {
		t.Fatalf("source SK hints = %q/%q", firstHint, secondHint)
	}
	if decodeObject(firstCredentials)["access_token"] != "access-duplicate-first" {
		t.Fatalf("duplicate email did not replace OAuth credentials: %s", firstCredentials)
	}
	if updatedProxyID != firstProxyID {
		t.Fatalf("updated account proxy = %d, want original bound proxy %d", updatedProxyID, firstProxyID)
	}
	var rpmStrategy, extraJSON string
	var stickyBuffer int
	var boundStrategy sql.NullInt64
	if err := a.db.QueryRow(`SELECT rpm_strategy, rpm_sticky_buffer, strategy_id, extra_json FROM accounts WHERE id = ?`, result.Items[1].AccountID).Scan(&rpmStrategy, &stickyBuffer, &boundStrategy, &extraJSON); err != nil {
		t.Fatal(err)
	}
	if rpmStrategy != "fixed" || stickyBuffer != 4 || !boundStrategy.Valid || boundStrategy.Int64 != strategy.ID {
		t.Fatalf("batch onboarding limits = %q/%d/%v, want fixed/4/%d", rpmStrategy, stickyBuffer, boundStrategy, strategy.ID)
	}
	extra := decodeObject(extraJSON)
	if fmt.Sprint(extra["base_rpm"]) != "15" || extra["rpm_strategy"] != "fixed" || fmt.Sprint(extra["rpm_sticky_buffer"]) != "4" {
		t.Fatalf("batch onboarding did not mirror limits into extra_json: %+v", extra)
	}
}

func TestBatchAuthorizationRetriesCloudflareChallengeWithAnotherProxy(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	previousRetryDelays := claudeSessionChallengeDelays
	claudeSessionChallengeDelays = []time.Duration{0, 0, 0}
	defer func() { claudeSessionChallengeDelays = previousRetryDelays }()
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/organizations":
			writeJSON(w, http.StatusOK, []map[string]any{{"uuid": "org-1", "raven_type": "team"}})
		case strings.HasSuffix(r.URL.Path, "/authorize"):
			writeJSON(w, http.StatusOK, map[string]string{"redirect_uri": "https://platform.claude.com/oauth/code/callback?code=fallback-code"})
		case r.URL.Path == "/token":
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "access-fallback", "refresh_token": "refresh-fallback", "expires_in": 3600,
				"organization": map[string]string{"uuid": "org-1", "raven_type": "team"},
				"account":      map[string]string{"uuid": "account-fallback", "email_address": "fallback@example.com"},
			})
		case r.URL.Path == "/profile":
			writeJSON(w, http.StatusOK, map[string]any{"organization": map[string]string{"organization_type": "team"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	previousOrganizations := claudeOrganizationsEndpoint
	previousAuthorize := claudeSessionAuthorizeBaseURL
	previousToken := claudeTokenEndpoint
	previousProfile := claudeProfileEndpoint
	claudeOrganizationsEndpoint = upstream.URL + "/organizations"
	claudeSessionAuthorizeBaseURL = upstream.URL + "/v1/oauth"
	claudeTokenEndpoint = upstream.URL + "/token"
	claudeProfileEndpoint = upstream.URL + "/profile"
	defer func() {
		claudeOrganizationsEndpoint = previousOrganizations
		claudeSessionAuthorizeBaseURL = previousAuthorize
		claudeTokenEndpoint = previousToken
		claudeProfileEndpoint = previousProfile
	}()

	challengeProxyID, challengeCalls := createCloudflareChallengeProxy(t, a)
	workingProxyID, workingCalls := createCountingForwardProxy(t, a)
	var result batchAuthorizationResponse
	putJSON(t, a.routes(), http.MethodPost, "/api/accounts/batch-authorize", map[string]any{
		"session_keys": []string{"valid-session"}, "proxy_pool_id": 1,
		"group_ids": []string{"a"}, "auth_type": "oauth", "account_price": 0,
	}, http.StatusOK, &result)
	if result.Success != 1 || result.Failed != 0 || len(result.Items) != 1 {
		t.Fatalf("batch result = %+v", result)
	}
	if challengeCalls.Load() < 4 || workingCalls.Load() == 0 {
		t.Fatalf("proxy calls = challenge %d, working %d", challengeCalls.Load(), workingCalls.Load())
	}
	var assignedProxyID int64
	if err := a.db.QueryRow(`SELECT proxy_id FROM accounts WHERE id = ?`, result.Items[0].AccountID).Scan(&assignedProxyID); err != nil {
		t.Fatal(err)
	}
	if assignedProxyID == challengeProxyID || assignedProxyID != workingProxyID {
		t.Fatalf("assigned proxy = %d, challenge = %d, working = %d", assignedProxyID, challengeProxyID, workingProxyID)
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

func createCloudflareChallengeProxy(t *testing.T, a *app) (int64, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.Header().Set("Server", "cloudflare")
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><title>Just a moment...</title></html>"))
	}))
	t.Cleanup(server.Close)
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, status, latency_ms, last_test_at) VALUES (1, ?, 'http', ?, ?, 'active', 0, `+nowSQL+`)`, "challenge-proxy-"+portText, host, port)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id, &calls
}
