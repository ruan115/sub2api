package main

import (
	"database/sql"
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

	if _, err := a.db.Exec(`UPDATE accounts SET auth_error = 'OAuth 401: rejected', rate_limit_reset_at = ? WHERE id = ?`, future, created.ID); err != nil {
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
