package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestErrorLogsAggregateFilterPaginateAndRedact(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	accountResult, err := a.db.Exec(`INSERT INTO accounts (name, status, schedulable, auth_status, auth_error, error_message, auth_checked_at)
		VALUES ('broken-account@example.com', 'error', 0, 'reauth_required', 'Claude token refresh failed (status 401) for sk-secret-example-1234567890', '', ` + nowSQL + `)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := accountResult.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO account_groups (account_id, group_id) VALUES (?, 'a')`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO authorization_logs (account_id, account_name, method, success, status_message)
		VALUES (?, 'broken-account@example.com', 'session_key', 0, 'OAuth status 400')`, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, status, last_error, last_test_at)
		VALUES (1, 'broken-proxy', 'http', '127.0.0.1', 18080, 'error', 'proxy test failed: timeout', ` + nowSQL + `)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO audit_logs (action, method, path, status_code)
		VALUES ('account.update', 'PUT', '/api/accounts/99', 500)`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE pricing_sync_state SET status = 'error', last_error = 'price endpoint unavailable', last_checked_at = ` + nowSQL + ` WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	handler := a.routes()
	var user panelUser
	putJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "gateway-user", "name": "Gateway User", "password": "gateway-password",
		"role": "user", "status": "active", "allowed_group_ids": []string{"a"}, "visible_pages": []string{"accounts"}, "rpm_limit": 0,
	}, http.StatusCreated, &user)
	var key apiKeyRecord
	putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"user_id": user.ID, "name": "gateway-test", "group_id": "a", "status": "active", "quota": 0,
	}, http.StatusCreated, &key)
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{"messages": []any{}}, nil, key.Key, http.StatusBadRequest, nil)

	var result errorLogResponse
	putJSON(t, handler, http.MethodGet, "/api/error-logs?page=1&page_size=2", nil, http.StatusOK, &result)
	if result.Summary != (errorLogSummary{Total: 6, Accounts: 1, Authorization: 1, Gateway: 1, Proxies: 1, Audit: 1, System: 1}) {
		t.Fatalf("summary = %+v", result.Summary)
	}
	if len(result.Items) != 2 || result.Page != 1 || result.PageSize != 2 || result.TotalPages != 3 {
		t.Fatalf("pagination = items:%d page:%d size:%d pages:%d", len(result.Items), result.Page, result.PageSize, result.TotalPages)
	}
	var gatewayErrors errorLogResponse
	putJSON(t, handler, http.MethodGet, "/api/error-logs?source=gateway", nil, http.StatusOK, &gatewayErrors)
	if gatewayErrors.Summary.Gateway != 1 || len(gatewayErrors.Items) != 1 || gatewayErrors.Items[0].StatusCode != http.StatusBadRequest || gatewayErrors.Items[0].Message != "invalid Anthropic message request" || gatewayErrors.Items[0].AccountName != "Gateway User" {
		t.Fatalf("gateway error was not recorded: %+v", gatewayErrors)
	}

	var accountErrors errorLogResponse
	putJSON(t, a.routes(), http.MethodGet, "/api/error-logs?source=account&group_id=a&search=broken-account", nil, http.StatusOK, &accountErrors)
	if accountErrors.Summary.Total != 1 || len(accountErrors.Items) != 1 {
		t.Fatalf("account errors = %+v", accountErrors)
	}
	item := accountErrors.Items[0]
	if item.StatusCode != 401 || item.AccountID == nil || *item.AccountID != accountID || strings.Contains(item.Message, "sk-secret-example-1234567890") {
		t.Fatalf("account error was not attributed or redacted: %+v", item)
	}
	if !strings.Contains(item.Message, "sk-sec") || !strings.HasSuffix(item.Message, "7890") {
		t.Fatalf("redacted message lost its useful hint: %q", item.Message)
	}

	putJSON(t, a.routes(), http.MethodGet, "/api/error-logs?source=unknown", nil, http.StatusBadRequest, nil)
	putJSON(t, a.routes(), http.MethodGet, "/api/error-logs?group_id=c", nil, http.StatusBadRequest, nil)
}

func TestErrorLogsDefaultToRetentionWindowAndPruneExpiredGatewayRows(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	now := time.Now().UTC()
	for _, item := range []struct {
		requestID string
		createdAt time.Time
	}{
		{requestID: "req-retention-recent", createdAt: now.AddDate(0, 0, -1)},
		{requestID: "req-retention-expired", createdAt: now.AddDate(0, 0, -8)},
	} {
		if _, err := a.db.Exec(`INSERT INTO gateway_error_logs (request_id, group_id, status_code, category, method, path, message, created_at)
			VALUES (?, 'a', 429, 'upstream_rate_limited', 'POST', '/v1/messages', 'retention marker', ?)`, item.requestID, item.createdAt.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	var result errorLogResponse
	putJSON(t, a.routes(), http.MethodGet, "/api/error-logs?source=gateway&search=req-retention", nil, http.StatusOK, &result)
	if result.RetentionDays != 7 || result.Summary.Total != 1 || len(result.Items) != 1 || result.Items[0].RequestID != "req-retention-recent" {
		t.Fatalf("default retention query = %+v", result)
	}

	if err := a.pruneGatewayErrorLogs(true); err != nil {
		t.Fatal(err)
	}
	var expired, recent int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM gateway_error_logs WHERE request_id = 'req-retention-expired'`).Scan(&expired); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM gateway_error_logs WHERE request_id = 'req-retention-recent'`).Scan(&recent); err != nil {
		t.Fatal(err)
	}
	if expired != 0 || recent != 1 {
		t.Fatalf("pruned rows expired=%d recent=%d", expired, recent)
	}
}

func TestErrorInsightsIncludeFilteredTimeDistribution(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	createAccount := func(name, groupID string) int64 {
		result, err := a.db.Exec(`INSERT INTO accounts (name, base_rpm) VALUES (?, 10)`, name)
		if err != nil {
			t.Fatal(err)
		}
		accountID, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.db.Exec(`INSERT INTO account_groups (account_id, group_id) VALUES (?, ?)`, accountID, groupID); err != nil {
			t.Fatal(err)
		}
		return accountID
	}
	targetID := createAccount("timeline-hit-account", "a")
	otherID := createAccount("timeline-other-account", "b")
	hour := time.Now().UTC().Truncate(time.Hour)
	insertEvent := func(accountID int64, groupID string, createdAt time.Time) {
		if _, err := a.db.Exec(`INSERT INTO gateway_error_logs (request_id, account_id, group_id, status_code, category, method, path, message, rpm_snapshot, tpm_snapshot, total_requests, created_at)
			VALUES (?, ?, ?, 429, 'upstream_rate_limited', 'POST', '/v1/messages', 'timeline marker', 8, 1200, 99, ?)`, newRequestID(), accountID, groupID, createdAt.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	insertEvent(targetID, "a", hour.Add(-2*time.Hour))
	insertEvent(targetID, "a", hour.Add(-time.Hour))
	insertEvent(otherID, "b", hour.Add(-time.Hour))

	query := url.Values{
		"status":   {"429"},
		"group_id": {"a"},
		"search":   {"timeline-hit"},
		"from":     {hour.Add(-3 * time.Hour).Format(time.RFC3339Nano)},
		"to":       {hour.Add(time.Hour).Format(time.RFC3339Nano)},
	}
	var result errorInsightResponse
	putJSON(t, a.routes(), http.MethodGet, "/api/error-insights?"+query.Encode(), nil, http.StatusOK, &result)
	if result.TimelineGranularity != "hour" || len(result.Accounts) != 1 || result.Accounts[0].AccountID != targetID || result.Accounts[0].Count != 2 {
		t.Fatalf("filtered insight accounts = %+v", result)
	}
	if len(result.Events) != 2 || len(result.Timeline) != 2 {
		t.Fatalf("filtered insight events=%d timeline=%+v", len(result.Events), result.Timeline)
	}
	var timelineTotal int64
	for _, point := range result.Timeline {
		timelineTotal += point.Count
	}
	if timelineTotal != 2 {
		t.Fatalf("timeline total=%d, want 2", timelineTotal)
	}
}

func TestErrorLogsPagePermission(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")

	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "error-reader", "name": "Error Reader", "password": "reader-password",
		"role": "readonly_admin", "status": "active", "allowed_group_ids": []string{"a", "b"}, "visible_pages": []string{"errors"}, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, nil)
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "ordinary-reader", "name": "Ordinary Reader", "password": "reader-password",
		"role": "user", "status": "active", "allowed_group_ids": []string{"a"}, "visible_pages": []string{"accounts"}, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, nil)

	readonlyCookie := loginCookie(t, handler, "error-reader", "reader-password")
	requestJSON(t, handler, http.MethodGet, "/api/error-logs", nil, readonlyCookie, "", http.StatusOK, nil)
	ordinaryCookie := loginCookie(t, handler, "ordinary-reader", "reader-password")
	requestJSON(t, handler, http.MethodGet, "/api/error-logs", nil, ordinaryCookie, "", http.StatusForbidden, nil)
}

func TestGatewayClientCancellationIsRecordedWithoutUpstream502(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	started := make(chan struct{})
	stopUpstream := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-stopUpstream:
		}
	}))
	defer func() {
		close(stopUpstream)
		upstream.Close()
	}()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	account := createGatewayTestAccount(t, a, handler, "cancel-account", upstream.URL, 0, nil, map[string]any{"access_token": "cancel-token"})

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach upstream")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("canceled gateway request did not return")
	}

	var status int
	var category, message string
	var durationMS, accountID int64
	if err := a.db.QueryRow(`SELECT status_code, category, message, duration_ms, account_id FROM gateway_error_logs ORDER BY id DESC LIMIT 1`).
		Scan(&status, &category, &message, &durationMS, &accountID); err != nil {
		t.Fatal(err)
	}
	if status != 499 || category != "client_canceled" || message != "Client canceled request before completion" || accountID != account.ID {
		t.Fatalf("canceled event=%d %q %q account=%d", status, category, message, accountID)
	}
	if strings.Contains(response.Body.String(), "Upstream request failed") {
		t.Fatalf("canceled request was still exposed as upstream 502: %s", response.Body.String())
	}
}

func TestGatewayUpstreamTimeoutReturnsAndRecords504(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	t.Setenv("CCMAX_UPSTREAM_REQUEST_TIMEOUT", "50ms")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"too-late"}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	account := createGatewayTestAccount(t, a, handler, "timeout-account", upstream.URL, 0, nil, map[string]any{"access_token": "timeout-token"})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusGatewayTimeout || !strings.Contains(response.Body.String(), `"type":"timeout_error"`) {
		t.Fatalf("timeout response=%d %s", response.Code, response.Body.String())
	}

	var status int
	var category, message string
	var durationMS, accountID int64
	if err := a.db.QueryRow(`SELECT status_code, category, message, duration_ms, account_id FROM gateway_error_logs ORDER BY id DESC LIMIT 1`).
		Scan(&status, &category, &message, &durationMS, &accountID); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusGatewayTimeout || category != "timeout" || message != "Upstream request timed out before completion" || accountID != account.ID {
		t.Fatalf("timeout event=%d %q %q account=%d", status, category, message, accountID)
	}
	if durationMS < 40 {
		t.Fatalf("timeout duration=%dms, want the elapsed request time", durationMS)
	}
}

func TestClassifyGatewayUpstreamErrorRedactsCredentials(t *testing.T) {
	category, message := classifyGatewayUpstreamError(http.StatusForbidden, []byte(`{"error":{"message":"Cloudflare challenge; access_token=secret-access-token-123; authorization: Bearer secret-bearer-token-456"}}`))
	if category != "upstream_forbidden_proxy_challenge" {
		t.Fatalf("category=%q", category)
	}
	if strings.Contains(message, "secret-access") || strings.Contains(message, "secret-bearer") {
		t.Fatalf("classified message leaked credentials: %q", message)
	}
	if message != "Upstream HTTP 403 · residential proxy exit triggered an upstream edge challenge" {
		t.Fatalf("message=%q", message)
	}

	category, message = classifyGatewayUpstreamError(http.StatusForbidden, []byte(`{"error":{"message":"Identity verification is required to continue."}}`))
	if category != "upstream_forbidden_identity_verification" || !strings.Contains(message, "identity verification") {
		t.Fatalf("identity classification=%q %q", category, message)
	}
}

func TestGatewayForbiddenFailoverStopsAfterTwoAccountsAndRecordsClass(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"Cloudflare challenge; access_token=secret-access-token-123"}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	if _, err := a.db.Exec(`UPDATE groups SET rpm_dispatch_enabled = 0 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	key := createGatewayTestKey(t, handler)
	for index := 0; index < 3; index++ {
		createGatewayTestAccount(t, a, handler, fmt.Sprintf("forbidden-%d", index), upstream.URL, index, nil, map[string]any{"access_token": fmt.Sprintf("token-%d", index)})
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if requests.Load() != gatewayForbiddenFailoverAttempts {
		t.Fatalf("upstream requests=%d want=%d", requests.Load(), gatewayForbiddenFailoverAttempts)
	}

	var status int
	var category, message string
	if err := a.db.QueryRow(`SELECT status_code, category, message FROM gateway_error_logs ORDER BY id DESC LIMIT 1`).Scan(&status, &category, &message); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusBadGateway || category != "upstream_forbidden_proxy_challenge" || strings.Contains(message, "secret-access") {
		t.Fatalf("recorded error=%d %q %q", status, category, message)
	}
	var errored int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE name LIKE 'forbidden-%' AND status = 'error'`).Scan(&errored); err != nil {
		t.Fatal(err)
	}
	if errored != gatewayForbiddenFailoverAttempts {
		t.Fatalf("errored accounts=%d want=%d", errored, gatewayForbiddenFailoverAttempts)
	}
}
