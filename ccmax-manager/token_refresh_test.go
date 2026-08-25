package main

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestExpiringGatewayAccountsMatchesSub2CandidateRules(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	expiresAt := time.Now().Add(time.Minute).Unix()
	setup := createGatewayTestAccount(t, a, handler, "setup-refresh", "https://setup.example.test", 0, nil, map[string]any{
		"access_token": "setup-access", "refresh_token": "setup-refresh", "expires_at": expiresAt,
	})
	if _, err := a.db.Exec(`UPDATE accounts SET auth_type = 'setup_token' WHERE id = ?`, setup.ID); err != nil {
		t.Fatal(err)
	}
	retryCooldown := createGatewayTestAccount(t, a, handler, "retry-cooldown", "https://cooldown.example.test", 1, nil, map[string]any{
		"access_token": "cooldown-access", "refresh_token": "cooldown-refresh", "expires_at": expiresAt,
	})
	future := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE accounts SET auth_error = 'token refresh retry exhausted: timeout', rate_limit_reset_at = ? WHERE id = ?`, future, retryCooldown.ID); err != nil {
		t.Fatal(err)
	}
	oauth401 := createGatewayTestAccount(t, a, handler, "oauth-401", "https://oauth.example.test", 2, nil, map[string]any{
		"access_token": "rejected-access", "refresh_token": "oauth-refresh", "expires_at": expiresAt,
	})
	if _, err := a.db.Exec(`UPDATE accounts SET auth_error = 'OAuth 401: rejected', rate_limit_reset_at = ? WHERE id = ?`, future, oauth401.ID); err != nil {
		t.Fatal(err)
	}

	accounts, err := a.expiringGatewayAccounts(30 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, account := range accounts {
		seen[account.ID] = true
	}
	if !seen[setup.ID] || !seen[oauth401.ID] || seen[retryCooldown.ID] {
		t.Fatalf("refresh candidates=%v", seen)
	}
}

func TestSaveClaudeTokenClearsOnlyAuthRefreshCooldown(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "refresh-state", "https://refresh.example.test", 0, nil, map[string]any{
		"access_token": "old-access", "refresh_token": "refresh-token", "expires_at": time.Now().Add(time.Minute).Unix(),
	})
	future := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
	token := &claudeTokenInfo{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}

	if _, err := a.db.Exec(`UPDATE accounts SET auth_error = 'OAuth 401: rejected', rate_limit_reset_at = ?, schedulable = 0 WHERE id = ?`, future, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.saveClaudeToken(created.ID, "oauth", token, true); err != nil {
		t.Fatal(err)
	}
	var reset sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&reset); err != nil {
		t.Fatal(err)
	}
	if reset.Valid {
		t.Fatalf("OAuth refresh cooldown was not cleared: %s", reset.String)
	}
	var schedulable int
	if err := a.db.QueryRow(`SELECT schedulable FROM accounts WHERE id = ?`, created.ID).Scan(&schedulable); err != nil {
		t.Fatal(err)
	}
	if schedulable != 0 {
		t.Fatal("background token refresh re-enabled a manually paused account")
	}

	if _, err := a.db.Exec(`UPDATE accounts SET auth_error = '', rate_limit_reset_at = ? WHERE id = ?`, future, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.saveClaudeToken(created.ID, "oauth", token, true); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&reset); err != nil {
		t.Fatal(err)
	}
	if !reset.Valid || reset.String != future {
		t.Fatalf("provider rate-limit cooldown changed: %v", reset)
	}
}

func TestTokenRefreshLeaseCoordinatesIndependentAppInstances(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	path := filepath.Join(t.TempDir(), "shared.db")
	a1, err := newApp(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a1.db.Close()
	a2, err := newApp(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a2.db.Close()

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		writeJSON(w, http.StatusOK, map[string]any{
			"access_token": "shared-fresh-access", "refresh_token": "shared-fresh-refresh", "expires_in": 3600,
		})
	}))
	defer upstream.Close()
	previousEndpoint := claudeTokenEndpoint
	claudeTokenEndpoint = upstream.URL
	defer func() { claudeTokenEndpoint = previousEndpoint }()

	handler := a1.routes()
	proxyID := createTestForwardProxy(t, a1)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "shared-refresh", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "shared-old-access", "refresh_token": "shared-old-refresh", "expires_at": time.Now().Add(-time.Minute).Unix()},
		"extra":       map[string]any{}, "status": "active", "schedulable": true, "concurrency": 1,
		"priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)
	base := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"shared-old-access","refresh_token":"shared-old-refresh","expires_at":1}`, ProxyID: sql.NullInt64{Int64: proxyID, Valid: true}}
	errorsCh := make(chan error, 2)
	go func() {
		_, refreshErr := a1.refreshGatewayAccountToken(context.Background(), base, true)
		errorsCh <- refreshErr
	}()
	<-started
	go func() {
		_, refreshErr := a2.refreshGatewayAccountToken(context.Background(), base, true)
		errorsCh <- refreshErr
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	for range 2 {
		if refreshErr := <-errorsCh; refreshErr != nil {
			t.Fatal(refreshErr)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("cross-process refresh calls = %d, want 1", calls.Load())
	}
}

func TestRefreshInvalidGrantRecoversWhenDatabaseTokenAdvanced(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "race-recovery", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "race-old-access", "refresh_token": "race-old-refresh", "expires_at": time.Now().Add(time.Hour).Unix()},
		"extra":       map[string]any{}, "status": "active", "schedulable": true, "concurrency": 1,
		"priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, updateErr := a.db.Exec(`UPDATE accounts SET credentials_json = ? WHERE id = ?`, `{"access_token":"race-new-access","refresh_token":"race-new-refresh","expires_at":4102444800}`, created.ID)
		if updateErr != nil {
			http.Error(w, updateErr.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "refresh token already used"})
	}))
	defer upstream.Close()
	previousEndpoint := claudeTokenEndpoint
	claudeTokenEndpoint = upstream.URL
	defer func() { claudeTokenEndpoint = previousEndpoint }()

	base := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"race-old-access","refresh_token":"race-old-refresh"}`, ProxyID: sql.NullInt64{Int64: proxyID, Valid: true}}
	refreshed, err := a.refreshGatewayAccountToken(context.Background(), base, true)
	if err != nil {
		t.Fatal(err)
	}
	if gatewayAccountAccessToken(refreshed) != "race-new-access" || gatewayAccountRefreshToken(refreshed) != "race-new-refresh" {
		t.Fatalf("race recovery returned stale credentials: %s", refreshed.CredentialsJSON)
	}
	got, err := a.getAccount(created.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || got.AuthStatus != "valid" {
		t.Fatalf("refresh race incorrectly invalidated account: %+v", got)
	}
}
