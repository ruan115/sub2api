package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

func TestGatewayRevokedOAuthRefreshesAndRetriesSameAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var refreshCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload["refresh_token"] != "old-refresh" {
			t.Errorf("refresh token=%#v", payload["refresh_token"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-token","refresh_token":"fresh-refresh","expires_in":3600,"token_type":"bearer"}`)
	}))
	defer tokenServer.Close()
	previousEndpoint := claudeTokenEndpoint
	claudeTokenEndpoint = tokenServer.URL
	defer func() { claudeTokenEndpoint = previousEndpoint }()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer old-token":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"type":"error","error":{"message":"OAuth access token has been revoked"}}`)
		case "Bearer fresh-token":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req-oauth-resurrected")
			_, _ = io.WriteString(w, `{"id":"msg_oauth_resurrected","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
		default:
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "revoked-then-refreshed", upstream.URL, 0, nil, map[string]any{
		"access_token": "old-token", "refresh_token": "old-refresh", "expires_at": time.Now().Add(time.Hour).Unix(),
	})
	var response map[string]any
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusOK, &response)
	if response["id"] != "msg_oauth_resurrected" || upstreamCalls.Load() != 2 || refreshCalls.Load() != 1 {
		t.Fatalf("response=%#v upstream=%d refresh=%d", response, upstreamCalls.Load(), refreshCalls.Load())
	}

	var status, authStatus, authError, credentials string
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error, credentials_json FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError, &credentials); err != nil {
		t.Fatal(err)
	}
	if status != "active" || authStatus != "valid" || authError != "" ||
		gjson.Get(credentials, "access_token").String() != "fresh-token" ||
		gjson.Get(credentials, "refresh_token").String() != "fresh-refresh" {
		t.Fatalf("account status=%s auth=%s error=%q credentials=%s", status, authStatus, authError, credentials)
	}
	var rpmEvents int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, created.ID).Scan(&rpmEvents); err != nil || rpmEvents != 2 {
		t.Fatalf("RPM events=%d err=%v, want 2", rpmEvents, err)
	}
}

func TestGatewayRevokedOAuthReturnsExplicitErrorWhenRefreshFails(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var refreshCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	}))
	defer tokenServer.Close()
	previousEndpoint := claudeTokenEndpoint
	claudeTokenEndpoint = tokenServer.URL
	defer func() { claudeTokenEndpoint = previousEndpoint }()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"message":"OAuth access token has been revoked"}}`)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "revoked-refresh-failed", upstream.URL, 0, nil, map[string]any{
		"access_token": "old-token", "refresh_token": "old-refresh", "expires_at": time.Now().Add(time.Hour).Unix(),
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), gatewayOAuthRefreshFailedMessage) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if upstreamCalls.Load() != 1 || refreshCalls.Load() != 1 {
		t.Fatalf("upstream=%d refresh=%d", upstreamCalls.Load(), refreshCalls.Load())
	}
	var status, authStatus, authError string
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError); err != nil {
		t.Fatal(err)
	}
	if status != "error" || authStatus != "reauth_required" || !strings.Contains(authError, gatewayOAuthRefreshFailedMessage) {
		t.Fatalf("account status=%s auth=%s error=%q", status, authStatus, authError)
	}
}

func TestGatewayRevokedOAuthStopsAfterOneSuccessfulRefresh(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var refreshCalls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-token","refresh_token":"fresh-refresh","expires_in":3600,"token_type":"bearer"}`)
	}))
	defer tokenServer.Close()
	previousEndpoint := claudeTokenEndpoint
	claudeTokenEndpoint = tokenServer.URL
	defer func() { claudeTokenEndpoint = previousEndpoint }()

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"message":"OAuth access token has been revoked"}}`)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "revoked-after-refresh", upstream.URL, 0, nil, map[string]any{
		"access_token": "old-token", "refresh_token": "old-refresh", "expires_at": time.Now().Add(time.Hour).Unix(),
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), gatewayOAuthRefreshRejectedMessage) {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	if upstreamCalls.Load() != 2 || refreshCalls.Load() != 1 {
		t.Fatalf("upstream=%d refresh=%d", upstreamCalls.Load(), refreshCalls.Load())
	}
	var status, authStatus, authError string
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError); err != nil {
		t.Fatal(err)
	}
	if status != "error" || authStatus != "reauth_required" || authError != gatewayOAuthRefreshRejectedMessage {
		t.Fatalf("account status=%s auth=%s error=%q", status, authStatus, authError)
	}
}

func TestRevokedOAuthFailureDoesNotOverwriteNewAuthorization(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "reauthorized-during-request", "http://127.0.0.1:1", 0, nil, map[string]any{
		"access_token": "old-token", "refresh_token": "old-refresh",
	})
	var oldCredentials string
	if err := a.db.QueryRow(`SELECT credentials_json FROM accounts WHERE id = ?`, created.ID).Scan(&oldCredentials); err != nil {
		t.Fatal(err)
	}
	newCredentials := `{"access_token":"new-token","refresh_token":"new-refresh"}`
	if _, err := a.db.Exec(`UPDATE accounts SET credentials_json = ?, status = 'active', auth_status = 'valid', auth_error = '' WHERE id = ?`, newCredentials, created.ID); err != nil {
		t.Fatal(err)
	}
	if a.markAccountReauthIfCredentialsCurrent(created.ID, gatewayOAuthRefreshFailedMessage, oldCredentials) {
		t.Fatal("stale request marked newly authorized credentials as revoked")
	}
	var status, authStatus, credentials string
	if err := a.db.QueryRow(`SELECT status, auth_status, credentials_json FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &credentials); err != nil {
		t.Fatal(err)
	}
	if status != "active" || authStatus != "valid" || credentials != newCredentials {
		t.Fatalf("account status=%s auth=%s credentials=%s", status, authStatus, credentials)
	}
}
