package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountInvalidationCauseBucketsUpstreamReasons(t *testing.T) {
	cases := map[string]string{
		"OAuth 401: This organization has been disabled.":                   deauthCauseOAuth401,
		"upstream authentication failed: invalid bearer token":              deauthCauseOAuth401,
		"OAuth access token was rejected and no refresh token is available": deauthCauseOAuth401,
		"Claude token refresh failed (status 401): invalid_grant":           deauthCauseOAuth401,
		"Claude token refresh failed (status 400): response was empty":      deauthCauseRefresh,
		"upstream access forbidden: region is not supported":                deauthCauseForbidden,
		"upstream account action required: identity verification required":  deauthCauseActionNeed,
		"account has no OAuth access token":                                 deauthCauseNoCredetial,
		"管理员手动置为待重新授权":                                                      deauthCauseManual,
		"":                                                                  deauthCauseOther,
		"something nobody has seen before":                                  deauthCauseOther,
	}
	for reason, want := range cases {
		if got := accountInvalidationCause(reason); got != want {
			t.Errorf("accountInvalidationCause(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestDeauthWindowMinutesClampsRequestedRange(t *testing.T) {
	cases := map[string]int{"": 60, "abc": 60, "0": 60, "-5": 60, "1": 5, "15": 15, "1440": 1440, "999999": 10080}
	for value, want := range cases {
		if got := deauthWindowMinutes(value); got != want {
			t.Errorf("deauthWindowMinutes(%q) = %d, want %d", value, got, want)
		}
	}
}

func TestAuthorizationDeauthCountsRecent401AccountsInsideWindow(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	created := make([]account, 0, 3)
	for index := 0; index < 3; index++ {
		proxyID := createTestForwardProxy(t, a)
		var item account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": fmt.Sprintf("deauth-%d@example.com", index), "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": fmt.Sprintf("token-%d", index)}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		}, http.StatusCreated, &item)
		created = append(created, item)
	}
	a.markAccountReauth(created[0].ID, "OAuth 401: access token has been revoked")
	a.markAccountReauth(created[1].ID, "upstream access forbidden: region is not supported")
	a.markAccountReauth(created[2].ID, "OAuth 401: invalid bearer token")
	// Push the third account's de-authorization outside a one-hour window.
	stale := time.Now().UTC().Add(-3 * time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE account_lifecycle_events SET created_at = ? WHERE account_id = ? AND event_type = 'invalidated'`, stale, created[2].ID); err != nil {
		t.Fatal(err)
	}

	var hour deauthStats
	putJSON(t, handler, http.MethodGet, "/api/authorization-deauth?window=60", nil, http.StatusOK, &hour)
	if hour.WindowMinutes != 60 || hour.Total != 2 || hour.Accounts401 != 1 || hour.OAuth401 != 1 {
		t.Fatalf("one hour window = %+v, want 1 of 2 events attributed to 401", hour)
	}
	if hour.PendingReauth != 3 {
		t.Fatalf("pending reauth = %d, want 3", hour.PendingReauth)
	}
	if hour.Recovered401 != 0 {
		t.Fatalf("recovered 401 = %d, want 0", hour.Recovered401)
	}
	if len(hour.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(hour.Events))
	}
	if hour.Events[0].Cause != deauthCauseForbidden && hour.Events[1].Cause != deauthCauseForbidden {
		t.Fatalf("403 event was not classified: %+v", hour.Events)
	}
	for _, cause := range hour.Causes {
		if cause.Cause == deauthCauseOAuth401 && cause.Count != 1 {
			t.Fatalf("401 cause count = %d, want 1", cause.Count)
		}
		if cause.Label == "" {
			t.Fatalf("cause %q has no label", cause.Cause)
		}
	}

	// A wider window must pick the stale event up again.
	var day deauthStats
	putJSON(t, handler, http.MethodGet, "/api/authorization-deauth?window=1440", nil, http.StatusOK, &day)
	if day.Total != 3 || day.Accounts401 != 2 {
		t.Fatalf("24 hour window = %+v, want 3 events and 2 accounts on 401", day)
	}

	// Re-authorizing keeps the event but marks it recovered.
	if err := a.saveClaudeToken(created[0].ID, "oauth", &claudeTokenInfo{AccessToken: "fresh", ExpiresAt: 4_102_444_800}, false); err != nil {
		t.Fatal(err)
	}
	var recovered deauthStats
	putJSON(t, handler, http.MethodGet, "/api/authorization-deauth?window=60", nil, http.StatusOK, &recovered)
	if recovered.Accounts401 != 1 || recovered.Recovered401 != 1 {
		t.Fatalf("after reauthorization = %+v, want the 401 event kept and marked recovered", recovered)
	}
	if recovered.PendingReauth != 2 {
		t.Fatalf("pending reauth after recovery = %d, want 2", recovered.PendingReauth)
	}
	// The reason must survive the account's auth_error being cleared.
	if recovered.Events[0].Reason == "" {
		t.Fatalf("invalidation reason was lost after reauthorization: %+v", recovered.Events[0])
	}

	// A second knockout of the same account must not double-count it, otherwise
	// the "still down" figure the panel derives can go negative.
	a.markAccountReauth(created[0].ID, "OAuth 401: access token has been revoked")
	if err := a.saveClaudeToken(created[0].ID, "oauth", &claudeTokenInfo{AccessToken: "fresher", ExpiresAt: 4_102_444_800}, false); err != nil {
		t.Fatal(err)
	}
	var repeated deauthStats
	putJSON(t, handler, http.MethodGet, "/api/authorization-deauth?window=60", nil, http.StatusOK, &repeated)
	if repeated.OAuth401 != 2 {
		t.Fatalf("401 events = %d, want both knockouts recorded", repeated.OAuth401)
	}
	if repeated.Accounts401 != 1 || repeated.Recovered401 != 1 {
		t.Fatalf("repeated knockout = %+v, want 1 distinct account and 1 recovered", repeated)
	}
}
