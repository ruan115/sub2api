package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExchangeClaudeSessionKeyRetriesChallengeOnSameProxy(t *testing.T) {
	var organizationCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/organizations":
			if organizationCalls.Add(1) <= 3 {
				w.Header().Set("cf-mitigated", "challenge")
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("<html>Just a moment</html>"))
				return
			}
			writeJSON(w, http.StatusOK, []map[string]any{{"uuid": "org-retry", "raven_type": "team"}})
		case "/v1/oauth/org-retry/authorize":
			writeJSON(w, http.StatusOK, map[string]string{"redirect_uri": "https://platform.claude.com/oauth/code/callback?code=retry-code"})
		case "/token":
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "retry-access", "refresh_token": "retry-refresh", "expires_in": 3600,
				"account": map[string]string{"email_address": "retry@example.com"},
			})
		case "/profile":
			writeJSON(w, http.StatusOK, map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()
	restoreClaudeAuthTestEndpoints(t, "http://claude.test")
	previousDelays := claudeSessionChallengeDelays
	claudeSessionChallengeDelays = []time.Duration{0, 0, 0}
	defer func() { claudeSessionChallengeDelays = previousDelays }()

	token, err := exchangeClaudeSessionKey(context.Background(), "session-key", claudeOAuthAPIScope, proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	if token.AccessToken != "retry-access" || organizationCalls.Load() != 4 {
		t.Fatalf("token/calls = %q/%d", token.AccessToken, organizationCalls.Load())
	}
}

func TestSessionChallengeDoesNotInvalidateExistingCredentials(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	proxyID, challengeCalls := createCloudflareChallengeProxy(t, a)
	restoreClaudeAuthTestEndpoints(t, "http://claude.test")
	previousDelays := claudeSessionChallengeDelays
	claudeSessionChallengeDelays = []time.Duration{0, 0, 0}
	defer func() { claudeSessionChallengeDelays = previousDelays }()
	handler := a.routes()
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "still-valid@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "valid-access", "refresh_token": "valid-refresh", "expires_at": time.Now().Add(time.Hour).Unix()},
		"extra":       map[string]any{}, "status": "active", "schedulable": true, "concurrency": 1,
		"priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)

	response := requestJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(created.ID, 10)+"/session-auth", map[string]any{
		"session_key": "replacement-session", "mode": "oauth",
	}, nil, "", http.StatusBadGateway, nil)
	if !strings.Contains(response.Body.String(), "blocked by Cloudflare challenge") || challengeCalls.Load() != 4 {
		t.Fatalf("challenge response/calls = %s / %d", response.Body.String(), challengeCalls.Load())
	}
	got, err := a.getAccount(created.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "active" || got.AuthStatus != "valid" || got.DispatchStatus != "normal" {
		t.Fatalf("failed replacement authorization invalidated existing account: %+v", got)
	}
}

func restoreClaudeAuthTestEndpoints(t *testing.T, baseURL string) {
	t.Helper()
	previousOrganizations := claudeOrganizationsEndpoint
	previousAuthorize := claudeSessionAuthorizeBaseURL
	previousToken := claudeTokenEndpoint
	previousProfile := claudeProfileEndpoint
	claudeOrganizationsEndpoint = baseURL + "/organizations"
	claudeSessionAuthorizeBaseURL = baseURL + "/v1/oauth"
	claudeTokenEndpoint = baseURL + "/token"
	claudeProfileEndpoint = baseURL + "/profile"
	t.Cleanup(func() {
		claudeOrganizationsEndpoint = previousOrganizations
		claudeSessionAuthorizeBaseURL = previousAuthorize
		claudeTokenEndpoint = previousToken
		claudeProfileEndpoint = previousProfile
	})
}
