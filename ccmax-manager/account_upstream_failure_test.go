package main

import (
	"database/sql"
	"net/http"
	"testing"
)

func TestCaptureAccountUpstreamFailureRequiresManualActionForTerminal400(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "identity-check", "http://127.0.0.1:1", 0, nil, map[string]any{"access_token": "token"})
	account := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"token"}`}

	a.captureAccountUpstreamFailure(account, http.StatusBadRequest, []byte(`{"type":"error","error":{"message":"Identity verification is required to continue."}}`))
	var status, authStatus, authError string
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError); err != nil {
		t.Fatal(err)
	}
	if status != "error" || authStatus != "reauth_required" || authError != "upstream account action required: Identity verification is required to continue." {
		t.Fatalf("account state=%q/%q error=%q", status, authStatus, authError)
	}
}

func TestCaptureAccountUpstreamFailureLeavesOrdinary400Schedulable(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "ordinary-bad-request", "http://127.0.0.1:1", 0, nil, map[string]any{"access_token": "token"})
	account := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"token"}`}

	a.captureAccountUpstreamFailure(account, http.StatusBadRequest, []byte(`{"type":"error","error":{"message":"max_tokens must be positive"}}`))
	var status, authStatus, authError string
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError); err != nil {
		t.Fatal(err)
	}
	if status != "active" || authStatus == "reauth_required" || authError != "" {
		t.Fatalf("ordinary 400 changed account state=%q/%q error=%q", status, authStatus, authError)
	}
}

func TestCaptureAccountUpstreamFailureMarksRevokedOAuthTokenError(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "revoked-oauth", "http://127.0.0.1:1", 0, nil, map[string]any{
		"access_token": "revoked-token", "refresh_token": "refresh-token",
	})
	account := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"revoked-token","refresh_token":"refresh-token"}`}

	a.captureAccountUpstreamFailure(account, http.StatusUnauthorized, []byte(`{"type":"error","error":{"message":"OAuth access token has been revoked."}}`))
	var status, authStatus, authError string
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError, &resetAt); err != nil {
		t.Fatal(err)
	}
	if status != "error" || authStatus != "reauth_required" || authError != "OAuth 401: OAuth access token has been revoked." || resetAt.Valid {
		t.Fatalf("revoked OAuth account state=%q/%q error=%q reset=%v", status, authStatus, authError, resetAt)
	}
}

func TestCaptureAccountUpstreamFailureMarksInvalidAuthenticationCredentialsError(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "invalid-oauth-credentials", "http://127.0.0.1:1", 0, nil, map[string]any{
		"access_token": "invalid-token", "refresh_token": "refresh-token",
	})
	account := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"invalid-token","refresh_token":"refresh-token"}`}

	a.captureAccountUpstreamFailure(account, http.StatusUnauthorized, []byte(`{"type":"error","error":{"message":"Invalid authentication credentials"}}`))
	var status, authStatus, authError string
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError, &resetAt); err != nil {
		t.Fatal(err)
	}
	if status != "error" || authStatus != "reauth_required" || authError != "OAuth 401: Invalid authentication credentials" || resetAt.Valid {
		t.Fatalf("invalid OAuth credentials account state=%q/%q error=%q reset=%v", status, authStatus, authError, resetAt)
	}
}

func TestReclassifyTerminalOAuth401Accounts(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "previously-misclassified", "http://127.0.0.1:1", 0, nil, map[string]any{
		"access_token": "revoked-token", "refresh_token": "refresh-token",
	})
	if _, err := a.db.Exec(`UPDATE accounts SET status = 'active', auth_status = 'valid', auth_error = 'OAuth 401: OAuth access token has been revoked.', error_message = auth_error, rate_limit_reset_at = '2030-01-01T00:00:00Z' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}

	if err := a.reclassifyTerminalOAuth401Accounts(); err != nil {
		t.Fatal(err)
	}
	var status, authStatus, authError string
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError, &resetAt); err != nil {
		t.Fatal(err)
	}
	if status != "error" || authStatus != "reauth_required" || authError != "OAuth 401: OAuth access token has been revoked." || resetAt.Valid {
		t.Fatalf("reclassified account state=%q/%q error=%q reset=%v", status, authStatus, authError, resetAt)
	}
}

func TestReclassifyTerminalOAuth401AccountsInvalidCredentials(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "previous-invalid-credentials", "http://127.0.0.1:1", 0, nil, map[string]any{
		"access_token": "invalid-token", "refresh_token": "refresh-token",
	})
	if _, err := a.db.Exec(`UPDATE accounts SET status = 'active', auth_status = 'valid', auth_error = 'OAuth 401: Invalid authentication credentials', error_message = auth_error, rate_limit_reset_at = '2030-01-01T00:00:00Z' WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}

	if err := a.reclassifyTerminalOAuth401Accounts(); err != nil {
		t.Fatal(err)
	}
	var status, authStatus, authError string
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT status, auth_status, auth_error, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&status, &authStatus, &authError, &resetAt); err != nil {
		t.Fatal(err)
	}
	if status != "error" || authStatus != "reauth_required" || authError != "OAuth 401: Invalid authentication credentials" || resetAt.Valid {
		t.Fatalf("reclassified invalid credentials account state=%q/%q error=%q reset=%v", status, authStatus, authError, resetAt)
	}
}

func TestAccountAuthenticationFailureIsTerminal(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{message: "OAuth access token has been revoked.", want: true},
		{message: "OAuth access token was revoked.", want: true},
		{message: "Invalid authentication credentials", want: true},
		{message: "INVALID AUTHENTICATION CREDENTIALS", want: true},
		{message: "access token expired"},
		{message: "temporary authentication failure"},
	}
	for _, tt := range tests {
		if got := accountAuthenticationFailureIsTerminal(tt.message); got != tt.want {
			t.Fatalf("accountAuthenticationFailureIsTerminal(%q)=%t want=%t", tt.message, got, tt.want)
		}
	}
}

func TestAccountRequiresUpstreamActionMessages(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{message: "Identity verification is required to continue.", want: true},
		{message: "You must accept Anthropic's updated Consumer Terms to continue.", want: true},
		{message: "Consumer Terms agreement is required.", want: true},
		{message: "Third-party apps now draw from extra usage."},
		{message: "invalid anthropic-version"},
	}
	for _, tt := range tests {
		if got := accountRequiresUpstreamAction(tt.message); got != tt.want {
			t.Fatalf("accountRequiresUpstreamAction(%q)=%t want=%t", tt.message, got, tt.want)
		}
	}
}
