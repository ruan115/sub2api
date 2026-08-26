package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sub2claude "github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
)

type capturedClaudeRequest struct {
	Path   string
	Query  string
	Header http.Header
	Body   map[string]any
}

func TestOAuthGatewayMimicsClaudeCodeAndFiltersHeaders(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan capturedClaudeRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- capturedClaudeRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req-mimic")
		_, _ = w.Write([]byte(`{"id":"msg_mimic","usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "mimic", upstream.URL, 0, map[string]any{"account_uuid": "account-uuid"}, map[string]any{"access_token": "oauth-token", "account_uuid": "account-uuid"})

	payload := map[string]any{
		"model": "claude-test", "system": "Only answer with facts.", "max_tokens": 32,
		"messages": []any{map[string]any{"role": "user", "content": "hello from a third party client"}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", "sessionKey=must-not-leak")
	request.Header.Set("X-Untrusted-Header", "must-not-leak")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}

	got := <-captured
	if got.Path != "/v1/messages" || got.Query != "beta=true" {
		t.Fatalf("upstream target=%s?%s", got.Path, got.Query)
	}
	if got.Header.Get("Authorization") != "Bearer oauth-token" {
		t.Fatalf("authorization=%q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("X-Untrusted-Header") != "" {
		t.Fatalf("untrusted headers leaked: %#v", got.Header)
	}
	if got.Header.Get("User-Agent") != sub2claude.DefaultHeaders["User-Agent"] || got.Header.Get("X-App") != "cli" {
		t.Fatalf("Claude Code headers missing: %#v", got.Header)
	}
	for _, beta := range sub2claude.FullClaudeCodeMimicryBetas() {
		if !strings.Contains(got.Header.Get("anthropic-beta"), beta) {
			t.Fatalf("anthropic-beta missing %s: %s", beta, got.Header.Get("anthropic-beta"))
		}
	}
	system, _ := got.Body["system"].([]any)
	if len(system) != 3 {
		t.Fatalf("system blocks=%d body=%#v", len(system), got.Body)
	}
	expansion, _ := system[2].(map[string]any)["text"].(string)
	if !strings.Contains(expansion, "authorized security testing") {
		t.Fatalf("default group must retain Sub2API expansion: %q", expansion)
	}
	billing, _ := system[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(billing, "x-anthropic-billing-header: cc_version="+claudeCLIVersion+".") {
		t.Fatalf("billing block=%q", billing)
	}
	messages, _ := got.Body["messages"].([]any)
	if len(messages) != 3 || !strings.Contains(messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "Only answer with facts.") {
		t.Fatalf("original system was not preserved in messages: %#v", messages)
	}
	metadata := got.Body["metadata"].(map[string]any)
	var userID map[string]string
	if err := json.Unmarshal([]byte(metadata["user_id"].(string)), &userID); err != nil || userID["account_uuid"] != "account-uuid" || userID["session_id"] == "" {
		t.Fatalf("metadata.user_id=%#v err=%v", metadata["user_id"], err)
	}
	var fingerprintJSON, extraJSON string
	if err := a.db.QueryRow(`SELECT f.fingerprint_json, a.extra_json FROM account_fingerprints f JOIN accounts a ON a.id = f.account_id WHERE f.account_id = ?`, created.ID).Scan(&fingerprintJSON, &extraJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fingerprintJSON, "ClientID") || strings.Contains(extraJSON, sub2FingerprintExtraKey) {
		t.Fatalf("fingerprint storage=%s extra=%s", fingerprintJSON, extraJSON)
	}
}

func TestOAuthGatewayAppliesGroupFieldPassthrough(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan capturedClaudeRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- capturedClaudeRequest{Header: r.Header.Clone(), Body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_fields","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "字段透传", "rate_multiplier": 1, "status": "active",
		"service_tier_passthrough_enabled":   true,
		"inference_geo_passthrough_enabled":  true,
		"speed_passthrough_enabled":          true,
		"anthropic_beta_passthrough_enabled": true,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "field-passthrough", upstream.URL, 0, nil, map[string]any{"access_token": "oauth-token"})

	body := []byte(`{"model":"claude-test","max_tokens":32,"messages":[{"role":"user","content":"hello"}],"service_tier":"auto","inference_geo":"us","speed":"fast"}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Beta", "client-custom-beta-2099-01-01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}

	got := <-captured
	if got.Body["service_tier"] != "auto" || got.Body["inference_geo"] != "us" || got.Body["speed"] != "fast" {
		t.Fatalf("group passthrough body=%#v", got.Body)
	}
	if !strings.Contains(got.Header.Get("anthropic-beta"), "client-custom-beta-2099-01-01") ||
		!strings.Contains(got.Header.Get("anthropic-beta"), "claude-code-20250219") {
		t.Fatalf("group passthrough anthropic-beta=%q", got.Header.Get("anthropic-beta"))
	}
}

func TestOAuthGatewayUsesGroupDistilledCompatibilityMode(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan capturedClaudeRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- capturedClaudeRequest{Header: r.Header.Clone(), Body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_normal","type":"message","content":[{"type":"thinking","thinking":"","signature":"signed-thinking"},{"type":"text","text":"PARAM_OK"}],"usage":{"input_tokens":20,"output_tokens":27,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"output_tokens_details":{"thinking_tokens":18}}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "蒸馏兼容", "rate_multiplier": 1,
		"status": "active", "normal_request_mode": true,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "normal-mode", upstream.URL, 0, nil, map[string]any{
		"access_token": "oauth-token", "account_uuid": "account-uuid",
	})

	body := []byte(`{"unknown":{"drop":true},"model":"claude-fable-5","system":"Keep this system prompt.","max_tokens":64,"temperature":999,"top_p":999,"top_k":-1,"thinking":{"type":"enabled","budget_tokens":1024},"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}

	got := <-captured
	system, _ := got.Body["system"].([]any)
	if len(system) != 2 || system[1].(map[string]any)["text"] != "Keep this system prompt." {
		t.Fatalf("distilled system=%#v", got.Body["system"])
	}
	encodedBody, _ := json.Marshal(got.Body)
	if !strings.Contains(system[0].(map[string]any)["text"].(string), "x-anthropic-billing-header") ||
		strings.Contains(string(encodedBody), claudeCodeSystemPrompt) {
		t.Fatalf("distilled default identity blocks=%#v", got.Body["system"])
	}
	if got.Body["model"] != "claude-fable-5" {
		t.Fatalf("distilled model fallback=%#v", got.Body["model"])
	}
	for _, field := range []string{"unknown", "temperature", "top_p", "top_k"} {
		if _, exists := got.Body[field]; exists {
			t.Fatalf("distilled request retained %s: %#v", field, got.Body[field])
		}
	}
	thinking, _ := got.Body["thinking"].(map[string]any)
	if thinking["type"] != "adaptive" {
		t.Fatalf("distilled adaptive thinking=%#v", got.Body["thinking"])
	}
	if stops, _ := got.Body["stop_sequences"].([]any); len(stops) != 1 || stops[0] != "END" {
		t.Fatalf("distilled stop sequences=%#v", got.Body["stop_sequences"])
	}
	if got.Header.Get("X-App") != "cli" || !strings.Contains(got.Header.Get("anthropic-beta"), "claude-code-20250219") {
		t.Fatalf("distilled mode lost OAuth headers: %#v", got.Header)
	}
	if gjson.Get(response.Body.String(), "content.0.signature").String() != "signed-thinking" {
		t.Fatalf("thinking signature changed: %s", response.Body.String())
	}
	if gjson.Get(response.Body.String(), "usage.iterations.0.input_tokens").Int() != 20 ||
		gjson.Get(response.Body.String(), "usage.iterations.0.output_tokens").Int() != 27 ||
		gjson.Get(response.Body.String(), "usage.iterations.0.type").String() != "message" {
		t.Fatalf("distilled usage iterations=%s", response.Body.String())
	}
}

func TestOAuthGatewayCanEnableGroupClaudeCodeIdentity(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_identity","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "蒸馏兼容", "rate_multiplier": 1,
		"status": "active", "normal_request_mode": true,
		"claude_code_identity_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.ClaudeCodeIdentityEnabled {
		t.Fatal("group API did not persist Claude Code identity switch")
	}
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "identity-enabled", upstream.URL, 0, nil, map[string]any{
		"access_token": "oauth-token", "account_uuid": "account-uuid",
	})

	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-fable-5", "max_tokens": 64, "system": "Client system.",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil, key.Key, http.StatusOK, nil)

	system, _ := (<-captured)["system"].([]any)
	if len(system) != 3 ||
		!strings.Contains(system[0].(map[string]any)["text"].(string), "x-anthropic-billing-header") ||
		system[1].(map[string]any)["text"] != claudeCodeSystemPrompt ||
		system[2].(map[string]any)["text"] != "Client system." {
		t.Fatalf("enabled identity system=%#v", system)
	}
}

func TestDistilledCompatibilityAccountFailoverKeepsRequestedModel(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	models := make(chan string, 2)
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		models <- gjson.GetBytes(body, "model").String()
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`)
	}))
	defer limited.Close()
	available := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		models <- gjson.GetBytes(body, "model").String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_fable","type":"message","model":"claude-fable-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer available.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "蒸馏兼容", "rate_multiplier": 1,
		"status": "active", "normal_request_mode": true,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	limitedAccount := createGatewayTestAccount(t, a, handler, "limited-fable", limited.URL, 0, nil, map[string]any{"access_token": "first-token"})
	availableAccount := createGatewayTestAccount(t, a, handler, "available-fable", available.URL, 1, nil, map[string]any{"access_token": "second-token"})

	var response map[string]any
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-fable-5", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil, key.Key, http.StatusOK, &response)
	if response["model"] != "claude-fable-5" {
		t.Fatalf("response model=%#v", response["model"])
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if model := <-models; model != "claude-fable-5" {
			t.Fatalf("attempt %d model=%q, want claude-fable-5", attempt, model)
		}
	}
	var learnedRPM int
	if err := a.db.QueryRow(`SELECT rpm_limit FROM account_rpm_thresholds WHERE account_id = ?`, limitedAccount.ID).Scan(&learnedRPM); err != nil {
		t.Fatal(err)
	}
	if learnedRPM != 1 {
		t.Fatalf("learned RPM=%d, want 1", learnedRPM)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = NULL WHERE id = ?`, limitedAccount.ID); err != nil {
		t.Fatal(err)
	}
	gatewayKey, err := a.authenticateGatewayKey(key.Key)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := a.acquireGatewayAccount(gatewayKey, "new-session", "claude-fable-5", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(selected.ID)
	if selected.ID != availableAccount.ID {
		t.Fatalf("selected account=%d after learned threshold, want %d", selected.ID, availableAccount.ID)
	}
	if _, err := a.db.Exec(`UPDATE account_rpm_thresholds SET reset_at = strftime('%Y-%m-%dT%H:%M:%fZ','now','-1 second') WHERE account_id = ?`, limitedAccount.ID); err != nil {
		t.Fatal(err)
	}
	selected, err = a.acquireGatewayAccount(gatewayKey, "expired-threshold", "claude-fable-5", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(selected.ID)
	if selected.ID != limitedAccount.ID {
		t.Fatalf("selected account=%d after threshold expiry, want %d", selected.ID, limitedAccount.ID)
	}
}

func TestReduced429RPMThreshold(t *testing.T) {
	tests := []struct {
		observed int
		strikes  int
		want     int
	}{
		{observed: 20, strikes: 1, want: 15},
		{observed: 20, strikes: 2, want: 10},
		{observed: 20, strikes: 3, want: 5},
		{observed: 20, strikes: 9, want: 5},
		{observed: 2, strikes: 1, want: 1},
		{observed: 1, strikes: 1, want: 1},
	}
	for _, test := range tests {
		if got := reduced429RPMThreshold(test.observed, test.strikes); got != test.want {
			t.Fatalf("reduced429RPMThreshold(%d, %d)=%d, want %d", test.observed, test.strikes, got, test.want)
		}
	}
}

func TestLearned429RPMThresholdLowersPeakAndOverridesSticky(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	limited := createGatewayTestAccount(t, a, handler, "learned-peak", "https://limited.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	healthy := createGatewayTestAccount(t, a, handler, "healthy-peak", "https://healthy.example.test", 1, nil, map[string]any{"access_token": "token-b"})
	resetAt := time.Now().UTC().Add(4 * time.Hour)
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 1, rate_limit_reason = '429_backoff', rate_limit_downweight_until = ? WHERE id = ?`, resetAt.Format(time.RFC3339Nano), limited.ID); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		a.recordGatewayAccountRPM(limited.ID)
	}
	a.captureAccountRPMThreshold("a", limited.ID, resetAt)

	var learned int
	var storedReset string
	if err := a.db.QueryRow(`SELECT rpm_limit, reset_at FROM account_rpm_thresholds WHERE account_id = ?`, limited.ID).Scan(&learned, &storedReset); err != nil {
		t.Fatal(err)
	}
	if learned != 15 {
		t.Fatalf("learned RPM=%d, want a 25%% reduction from 20 to 15", learned)
	}
	parsedReset, err := time.Parse(time.RFC3339Nano, storedReset)
	if err != nil {
		t.Fatal(err)
	}
	if parsedReset.Before(time.Now().UTC().Add(3 * time.Hour)) {
		t.Fatalf("learned RPM reset=%s, want the threshold to survive until the quota window", parsedReset)
	}

	const session = "sticky-over-learned-peak"
	a.bindGatewayStickySession(key.ID, session, limited.ID)

	// A learned cap comes from a 429, so it is rate limiting rather than account
	// failure and must not move a pinned large request: switching accounts turns
	// a nearly free cache read into a full-price cache creation, which is what
	// pushed the account into the limit to begin with. The block is `rpm >= cap`
	// against a rolling 60s count, so it clears as traffic ages out even though
	// the cap itself survives until the quota window — the capacity queue holds
	// the request until then.
	if _, err := a.acquireGatewayAccountPinned(key, session, "claude-fable-5", map[int64]bool{}, false, true); !errors.Is(err, errNoGatewayAccountCapacity) {
		t.Fatalf("pinned acquire error=%v, want a capacity error so the request waits on its own account", err)
	}

	// Without the pin the learned cap still hands the request to another account.
	selected, err := a.acquireGatewayAccountPinned(key, session, "claude-fable-5", map[int64]bool{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(selected.ID)
	if selected.ID != healthy.ID {
		t.Fatalf("selected account=%d, want healthy fallback %d after sticky account reached learned cap", selected.ID, healthy.ID)
	}
}

func TestOrphaned429RPMThresholdDoesNotLimitRecoveredAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	created := createGatewayTestAccount(t, a, handler, "recovered-threshold", "https://recovered.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	if _, err := a.db.Exec(`INSERT INTO account_rpm_thresholds (account_id, rpm_limit, reset_at) VALUES (?, 1, ?)`, created.ID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	a.recordGatewayAccountRPM(created.ID)

	selected, err := a.acquireGatewayAccount(key, "recovered-threshold", "claude-fable-5", map[int64]bool{})
	if err != nil {
		t.Fatalf("recovered account was limited by an orphaned threshold: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
	if selected.ID != created.ID {
		t.Fatalf("selected account=%d, want recovered account %d", selected.ID, created.ID)
	}

	a.sweepExpiredRuntimeState()
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_thresholds WHERE account_id = ?`, created.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("runtime sweep kept %d orphaned RPM thresholds", count)
	}
}

func TestRateLimitSweeperRestoresMissingLearnedRPMThreshold(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "restore-learned-peak", "https://limited.example.test", 0, nil, map[string]any{"access_token": "token"})
	resetAt := time.Now().UTC().Add(3 * time.Hour)
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 1, rate_limit_reason = '429_backoff', rate_limit_downweight_until = ? WHERE id = ?`,
		resetAt.Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		a.recordGatewayAccountRPM(created.ID)
	}

	a.sweepAccountRateLimitState()

	var learned int
	if err := a.db.QueryRow(`SELECT rpm_limit FROM account_rpm_thresholds WHERE account_id = ?`, created.ID).Scan(&learned); err != nil {
		t.Fatal(err)
	}
	if learned != 15 {
		t.Fatalf("restored learned RPM=%d, want 15", learned)
	}
}

func TestGatewayRejectsAnthropicSilentModelDowngradeByGroup(t *testing.T) {
	tests := []struct {
		name       string
		stream     bool
		enabled    bool
		wantStatus int
	}{
		{name: "disabled_non_stream_passes_through", enabled: false, wantStatus: http.StatusOK},
		{name: "enabled_non_stream_rejects", enabled: true, wantStatus: http.StatusBadGateway},
		{name: "enabled_stream_rejects", stream: true, enabled: true, wantStatus: http.StatusBadGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("request-id", "req-anthropic-downgrade")
				if tt.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_downgraded\",\"type\":\"message\",\"model\":\"claude-opus-4-8\",\"usage\":{\"input_tokens\":2}}}\n\n")
					_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_downgraded","type":"message","model":"claude-opus-4-8","content":[{"type":"text","text":"fallback"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
			}))
			defer upstream.Close()

			a, handler := newGatewayTestApp(t)
			defer a.db.Close()
			putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
				"name": "A 分组", "description": "模型降级控制", "rate_multiplier": 1, "status": "active",
				"reject_anthropic_downgrade_enabled": tt.enabled,
			}, http.StatusOK, nil)
			key := createGatewayTestKey(t, handler)
			created := createGatewayTestAccount(t, a, handler, "downgrade", upstream.URL, 0, nil, map[string]any{"access_token": "token"})

			payload := fmt.Sprintf(`{"model":"claude-fable-5","stream":%t,"max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`, tt.stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != tt.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tt.wantStatus, response.Body.String())
			}
			if tt.enabled {
				if !strings.Contains(response.Body.String(), "silently downgraded model from claude-fable-5 to claude-opus-4-8") {
					t.Fatalf("downgrade error body=%s", response.Body.String())
				}
				var model string
				var accountID, inputTokens, outputTokens int64
				var billedCost, actualCost, quotaUsed float64
				if err := a.db.QueryRow(`SELECT model, account_id, input_tokens, output_tokens, billed_cost, actual_cost FROM usage_logs WHERE request_id = 'req-anthropic-downgrade'`).Scan(&model, &accountID, &inputTokens, &outputTokens, &billedCost, &actualCost); err != nil {
					t.Fatal(err)
				}
				if err := a.db.QueryRow(`SELECT quota_used FROM api_keys WHERE id = ?`, key.ID).Scan(&quotaUsed); err != nil {
					t.Fatal(err)
				}
				if model != "claude-opus-4-8" || accountID != created.ID || inputTokens != 2 || billedCost != 0 || actualCost <= 0 || quotaUsed != 0 {
					t.Fatalf("rejected usage model=%q account=%d tokens=%d/%d billed=%f actual=%f quota=%f", model, accountID, inputTokens, outputTokens, billedCost, actualCost, quotaUsed)
				}
			} else if !strings.Contains(response.Body.String(), `"model":"claude-opus-4-8"`) {
				t.Fatalf("disabled switch did not pass through response: %s", response.Body.String())
			}
		})
	}
}

func TestGatewayQuotaHeaderMaskingByGroup(t *testing.T) {
	tests := []struct {
		name    string
		stream  bool
		enabled bool
	}{
		{name: "disabled_non_stream_passes_through", enabled: false},
		{name: "enabled_non_stream_masks", enabled: true},
		{name: "enabled_stream_masks", stream: true, enabled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("request-id", "req-quota-mask")
				w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.93")
				w.Header().Set("Anthropic-Ratelimit-Unified-Fallback-Percentage", "0.5")
				w.Header().Set("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
				w.Header().Set("Anthropic-Organization-Id", "org-secret")
				w.Header().Set("Anthropic-Workspace-Id", "wrkspc-secret")
				if tt.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_mask\",\"type\":\"message\",\"model\":\"claude-opus-5\",\"usage\":{\"input_tokens\":2}}}\n\n")
					_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg_mask","type":"message","model":"claude-opus-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
			}))
			defer upstream.Close()

			a, handler := newGatewayTestApp(t)
			defer a.db.Close()
			putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
				"name": "A 分组", "description": "限额头屏蔽", "rate_multiplier": 1, "status": "active",
				"quota_header_masking_enabled": tt.enabled,
			}, http.StatusOK, nil)
			key := createGatewayTestKey(t, handler)
			created := createGatewayTestAccount(t, a, handler, "quota-mask", upstream.URL, 0, nil, map[string]any{"access_token": "token"})

			payload := fmt.Sprintf(`{"model":"claude-opus-5","stream":%t,"max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`, tt.stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			header := response.Header()
			if header.Get("request-id") != "req-quota-mask" {
				t.Fatalf("benign upstream header lost: %v", header)
			}
			if header.Get("X-CCMAX-Group") == "" {
				t.Fatalf("group attribution header lost: %v", header)
			}
			masked := []string{
				"Anthropic-Ratelimit-Unified-5h-Utilization",
				"Anthropic-Ratelimit-Unified-Fallback-Percentage",
				"Anthropic-Ratelimit-Unified-Status",
				"Anthropic-Organization-Id",
				"Anthropic-Workspace-Id",
			}
			if tt.enabled {
				for _, name := range masked {
					if header.Get(name) != "" {
						t.Fatalf("header %s leaked despite masking: %q", name, header.Get(name))
					}
				}
				if header.Get("X-CCMAX-Account") != "" {
					t.Fatalf("account header leaked despite masking: %q", header.Get("X-CCMAX-Account"))
				}
				return
			}
			for _, name := range masked {
				if header.Get(name) == "" {
					t.Fatalf("header %s missing with masking disabled: %v", name, header)
				}
			}
			if header.Get("X-CCMAX-Account") != created.Name {
				t.Fatalf("account header=%q want=%q", header.Get("X-CCMAX-Account"), created.Name)
			}
		})
	}
}

func TestDetectGatewayAnthropicModelDowngradeFamilies(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		response  string
		want      bool
	}{
		{name: "fable", requested: "claude-fable-5", response: `{"model":"claude-opus-4-8"}`, want: true},
		{name: "versioned_opus", requested: "claude-opus-5-20260725", response: `{"message":{"model":"claude-opus-4-8-20260529"}}`, want: true},
		{name: "same_model", requested: "claude-opus-5", response: `{"model":"claude-opus-5"}`},
		{name: "older_request", requested: "claude-opus-4-7", response: `{"model":"claude-opus-4-8"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared := claudePreparedRequest{Model: tt.requested, RejectAnthropicDowngrade: true}
			if got := detectGatewayAnthropicModelDowngrade([]byte(tt.response), prepared) != nil; got != tt.want {
				t.Fatalf("downgrade=%t want=%t", got, tt.want)
			}
		})
	}
}

func TestDistilledCompatibilityStreamPreservesSignatureAndAddsIterations(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":23,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"stream-signature\"}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":33,\"output_tokens_details\":{\"thinking_tokens\":21}}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "蒸馏兼容", "rate_multiplier": 1,
		"status": "active", "normal_request_mode": true,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "distilled-stream", upstream.URL, 0, nil, map[string]any{
		"access_token": "oauth-token", "account_uuid": "account-uuid",
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-fable-5","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}

	var signatureSeen, finalUsageSeen bool
	for _, line := range strings.Split(response.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if gjson.Get(payload, "delta.type").String() == "signature_delta" {
			signatureSeen = gjson.Get(payload, "delta.signature").String() == "stream-signature"
		}
		if gjson.Get(payload, "type").String() == "message_delta" {
			finalUsageSeen = gjson.Get(payload, "usage.input_tokens").Int() == 23 &&
				gjson.Get(payload, "usage.output_tokens").Int() == 33 &&
				gjson.Get(payload, "usage.iterations.0.input_tokens").Int() == 23 &&
				gjson.Get(payload, "usage.iterations.0.output_tokens").Int() == 33 &&
				gjson.Get(payload, "usage.iterations.0.type").String() == "message"
		}
	}
	if !signatureSeen || !finalUsageSeen {
		t.Fatalf("distilled stream signature=%v final_usage=%v body=%s", signatureSeen, finalUsageSeen, response.Body.String())
	}
}

func TestDistilledCacheCreationDetailByGroup(t *testing.T) {
	tests := []struct {
		name    string
		stream  bool
		enabled bool
		want5m  int64
		want1h  int64
	}{
		{name: "enabled_stream_restores_buckets", stream: true, enabled: true, want5m: 2, want1h: 5},
		{name: "enabled_non_stream_restores_buckets", enabled: true, want5m: 2, want1h: 5},
		{name: "disabled_non_stream_keeps_legacy_zeros", enabled: false, want5m: 0, want1h: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Real Anthropic reports the cache_creation bucket split only on
				// message_start; message_delta carries totals without it.
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_detail\",\"type\":\"message\",\"model\":\"claude-fable-5\",\"usage\":{\"input_tokens\":3,\"cache_creation_input_tokens\":7,\"cache_read_input_tokens\":4,\"cache_creation\":{\"ephemeral_5m_input_tokens\":2,\"ephemeral_1h_input_tokens\":5},\"output_tokens\":0}}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
				_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":33}}\n\n")
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer upstream.Close()

			a, handler := newGatewayTestApp(t)
			defer a.db.Close()
			putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
				"name": "A 分组", "description": "缓存明细", "rate_multiplier": 1, "status": "active",
				"normal_request_mode":           true,
				"cache_creation_detail_enabled": tt.enabled,
			}, http.StatusOK, nil)
			key := createGatewayTestKey(t, handler)
			createGatewayTestAccount(t, a, handler, "cache-detail", upstream.URL, 0, nil, map[string]any{"access_token": "token"})

			payload := fmt.Sprintf(`{"model":"claude-fable-5","stream":%t,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`, tt.stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}

			finalUsage := ""
			if tt.stream {
				for _, line := range strings.Split(response.Body.String(), "\n") {
					line = strings.TrimSpace(line)
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
					if gjson.Get(payload, "type").String() == "message_delta" {
						finalUsage = gjson.Get(payload, "usage").Raw
					}
				}
			} else {
				finalUsage = gjson.Get(response.Body.String(), "usage").Raw
			}
			if finalUsage == "" {
				t.Fatalf("final usage not found in body=%s", response.Body.String())
			}
			if gjson.Get(finalUsage, "input_tokens").Int() != 3 ||
				gjson.Get(finalUsage, "output_tokens").Int() != 33 ||
				gjson.Get(finalUsage, "cache_creation_input_tokens").Int() != 7 ||
				gjson.Get(finalUsage, "cache_read_input_tokens").Int() != 4 {
				t.Fatalf("usage totals changed: %s", finalUsage)
			}
			for path, want := range map[string]int64{
				"cache_creation.ephemeral_5m_input_tokens":              tt.want5m,
				"cache_creation.ephemeral_1h_input_tokens":              tt.want1h,
				"iterations.0.cache_creation.ephemeral_5m_input_tokens": tt.want5m,
				"iterations.0.cache_creation.ephemeral_1h_input_tokens": tt.want1h,
			} {
				if got := gjson.Get(finalUsage, path).Int(); got != want {
					t.Fatalf("usage.%s=%d want=%d usage=%s", path, got, want, finalUsage)
				}
			}
			if gjson.Get(finalUsage, "iterations.0.cache_creation_input_tokens").Int() != 7 {
				t.Fatalf("iteration cache total changed: %s", finalUsage)
			}
		})
	}
}

func TestDistilledCompatibilityPreservesDottedToolCalls(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		encoded, _ := json.Marshal(body)
		if gjson.GetBytes(encoded, "tools.0.name").String() != "read.file" ||
			gjson.GetBytes(encoded, "tool_choice.name").String() != "read.file" {
			t.Errorf("distilled tool request=%#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","model":"claude-fable-5","content":[{"type":"tool_use","id":"toolu_1","name":"read.file","input":{"path":"/tmp/audit"},"caller":{"type":"direct"}}],"stop_reason":"tool_use","usage":{"input_tokens":515,"output_tokens":38,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "蒸馏兼容", "rate_multiplier": 1,
		"status": "active", "normal_request_mode": true,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "distilled-tool", upstream.URL, 0, nil, map[string]any{
		"access_token": "oauth-token", "account_uuid": "account-uuid",
	})

	payload := map[string]any{
		"model": "claude-fable-5", "max_tokens": 256,
		"messages": []any{map[string]any{"role": "user", "content": "Call read.file."}},
		"tools": []any{map[string]any{
			"name": "read.file", "description": "Read a file",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}},
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "read.file"},
	}
	var response map[string]any
	requestJSON(t, handler, http.MethodPost, "/v1/messages", payload, nil, key.Key, http.StatusOK, &response)
	encoded, _ := json.Marshal(response)
	if gjson.GetBytes(encoded, "content.0.name").String() != "read.file" ||
		gjson.GetBytes(encoded, "content.0.input.path").String() != "/tmp/audit" ||
		gjson.GetBytes(encoded, "content.0.caller.type").String() != "direct" ||
		gjson.GetBytes(encoded, "usage.iterations.0.input_tokens").Int() != 515 {
		t.Fatalf("distilled tool response=%s", encoded)
	}
}

func TestOAuthGatewayRewritesToolNamesAsMCPForGroup(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan capturedClaudeRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- capturedClaudeRequest{Header: r.Header.Clone(), Body: body}
		tools, _ := body["tools"].([]any)
		first, _ := tools[0].(map[string]any)
		name, _ := first["name"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_mcp","content":[{"type":"tool_use","id":"tu_1","name":"` + name + `","input":{}}],"usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "MCP 工具名", "rate_multiplier": 1,
		"status": "active", "mcp_tool_names_enabled": true,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "mcp-tools", upstream.URL, 0, nil, map[string]any{
		"access_token": "oauth-token", "account_uuid": "account-uuid",
	})

	body := []byte(`{"model":"claude-fable-5","max_tokens":64,"tools":[{"name":"read.file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}

	got := <-captured
	tools, _ := got.Body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("upstream tools=%#v", got.Body["tools"])
	}
	sent, _ := tools[0].(map[string]any)["name"].(string)
	if !strings.HasPrefix(sent, "mcp__") || !strings.HasSuffix(sent, "__read_file") {
		t.Fatalf("upstream tool name=%q", sent)
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`).MatchString(sent) {
		t.Fatalf("upstream tool name violates Anthropic pattern: %q", sent)
	}
	if strings.Contains(response.Body.String(), sent) {
		t.Fatalf("client response leaked the upstream alias: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"name":"read.file"`) {
		t.Fatalf("client response lost the original tool name: %s", response.Body.String())
	}
}

func TestAccountOverrideDisablesMCPToolNames(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan capturedClaudeRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- capturedClaudeRequest{Header: r.Header.Clone(), Body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_plain","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "MCP 工具名", "rate_multiplier": 1,
		"status": "active", "mcp_tool_names_enabled": true,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "mcp-opt-out", upstream.URL, 0, map[string]any{
		"mcp_tool_names": false,
	}, map[string]any{"access_token": "oauth-token", "account_uuid": "account-uuid"})

	body := []byte(`{"model":"claude-fable-5","max_tokens":64,"tools":[{"name":"read_file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}

	got := <-captured
	tools, _ := got.Body["tools"].([]any)
	sent, _ := tools[0].(map[string]any)["name"].(string)
	if sent != "read_file" {
		t.Fatalf("account override did not opt out of the MCP lane: %q", sent)
	}
}

func TestAccountRequestPassthroughPreservesRawBody(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- body
		if r.Header.Get("Authorization") != "Bearer passthrough-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Custom-Mode") != "raw" || r.Header.Get("anthropic-beta") != "client-beta" {
			t.Errorf("passthrough headers changed: %#v", r.Header)
		}
		if r.Header.Get("Cookie") != "" {
			t.Errorf("cookie leaked upstream: %q", r.Header.Get("Cookie"))
		}
		_, _ = w.Write([]byte(`{"id":"msg_passthrough","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	extra := map[string]any{
		"request_passthrough": true,
		"model_mapping":       map[string]any{"claude-client": "claude-upstream"},
	}
	createGatewayTestAccount(t, a, handler, "passthrough", upstream.URL, 0, extra, map[string]any{"access_token": "passthrough-token"})
	rawBody := []byte("{\n  \"messages\": [{\"role\":\"user\",\"content\":\"hello\"}],\n  \"custom_parameter\": {\"keep\": true},\n  \"system\": \"do not rewrite\",\n  \"max_tokens\": 17,\n  \"model\": \"claude-client\"\n}")
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(rawBody))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Custom-Mode", "raw")
	request.Header.Set("anthropic-beta", "client-beta")
	request.Header.Set("Cookie", "session=must-not-leak")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}
	if got := <-captured; !bytes.Equal(got, rawBody) {
		t.Fatalf("raw body changed\n got: %s\nwant: %s", got, rawBody)
	}
}

func TestRateLimitPolicyThresholdClamps(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		configured int
		want       int
	}{
		{"unset falls back to the default", 0, defaultRateLimitCoolingThreshold},
		{"negative falls back to the default", -1, defaultRateLimitCoolingThreshold},
		{"one parks on the first strike", 1, 1},
		{"in range is honoured", 5, 5},
		{"above range is clamped", 99, maxRateLimitCoolingThreshold},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := (rateLimitPolicy{CoolingThreshold: testCase.configured}).threshold(); got != testCase.want {
				t.Fatalf("threshold = %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestRateLimitPolicyCooldownSecondsNormalises(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		configured int
		want       int
	}{
		{"unset falls back to the default", 0, defaultRateLimitCooldownSeconds},
		{"below range falls back to the default", 59, defaultRateLimitCooldownSeconds},
		{"minimum is honoured", 60, 60},
		{"in range is honoured", 90, 90},
		{"maximum is honoured", 120, 120},
		{"above range falls back to the default", 121, defaultRateLimitCooldownSeconds},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := (rateLimitPolicy{CooldownSeconds: testCase.configured}).cooldownSeconds(); got != testCase.want {
				t.Fatalf("cooldown seconds = %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestRateLimitPolicySteppedCooldown(t *testing.T) {
	policy := rateLimitPolicy{
		CoolingThreshold:       3,
		CooldownSeconds:        120,
		SteppedCooldownEnabled: true,
		CooldownStepSeconds:    15,
	}
	for strikes, want := range map[int]int{1: 60, 2: 75, 3: 90, 4: 105, 5: 120, 8: 120} {
		if got := policy.cooldownForStrikes(strikes); got != want {
			t.Fatalf("strikes %d cooldown = %d, want %d", strikes, got, want)
		}
	}
	policy.CooldownStepSeconds = 0
	if got := policy.cooldownForStrikes(2); got != 90 {
		t.Fatalf("default cooldown step = %d, want 90", got)
	}
}

func TestRateLimitPolicyDownweightMinutes(t *testing.T) {
	policy := rateLimitPolicy{DownweightBaseMinutes: 20, DownweightStepMinutes: 35}
	for strikes, want := range map[int]int{1: 20, 2: 55, 3: 90, 9: 300, 10: maxRateLimitDownweightMinutes} {
		if got := policy.downweightMinutesForStrikes(strikes); got != want {
			t.Fatalf("strikes %d downweight = %d minutes, want %d", strikes, got, want)
		}
	}
	policy.DownweightBaseMinutes = 0
	policy.DownweightStepMinutes = 0
	if got := policy.downweightMinutesForStrikes(2); got != defaultRateLimitDownweightBaseMinutes+defaultRateLimitDownweightStepMinutes {
		t.Fatalf("default downweight = %d minutes", got)
	}
}

func TestGatewayGroupDownweightStepControlsPeakDuration(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "降峰阶梯", "rate_multiplier": 1, "status": "active",
		"rate_limit_downweight_enabled":                  true,
		"rate_limit_downweight_stepped_cooldown_enabled": true,
		"rate_limit_downweight_base_minutes":             20,
		"rate_limit_downweight_step_minutes":             35,
	}, http.StatusOK, nil)
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !key.RateLimitDownweightStepped || key.RateLimitDownweightBase != 20 || key.RateLimitDownweightStep != 35 {
		t.Fatalf("gateway policy did not load group downweight step: %+v", key)
	}
	created := createGatewayTestAccount(t, a, handler, "stepped-peak", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	a.recordGatewayAccountRPM(created.ID)
	before := time.Now().UTC()
	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, key.rateLimitPolicy(), &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)})

	var raw string
	if err := a.db.QueryRow(`SELECT rate_limit_downweight_until FROM accounts WHERE id = ?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	if delay := deadline.Sub(before); delay < 19*time.Minute || delay > 21*time.Minute {
		t.Fatalf("first stepped downweight = %s, want about 20m", delay)
	}
	var thresholdResetRaw string
	if err := a.db.QueryRow(`SELECT reset_at FROM account_rpm_thresholds WHERE account_id = ?`, created.ID).Scan(&thresholdResetRaw); err != nil {
		t.Fatal(err)
	}
	thresholdReset, err := time.Parse(time.RFC3339Nano, thresholdResetRaw)
	if err != nil {
		t.Fatal(err)
	}
	if !thresholdReset.Equal(deadline) {
		t.Fatalf("learned RPM threshold expires at %s, want downweight deadline %s", thresholdReset, deadline)
	}
}

func TestGatewayHighQuota429UsesMaximumDownweight(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "high-quota-peak", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.90")
	before := time.Now().UTC()
	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, rateLimitPolicy{
		DownweightEnabled: true, DownweightStepped: true, DownweightBaseMinutes: 5, DownweightStepMinutes: 5,
	}, &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var raw string
	if err := a.db.QueryRow(`SELECT rate_limit_downweight_until FROM accounts WHERE id = ?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Duration(maxRateLimitDownweightMinutes) * time.Minute
	if delay := deadline.Sub(before); delay < want-time.Second || delay > want+time.Second {
		t.Fatalf("90%% quota downweight = %s, want %s", delay, want)
	}
}

func TestGatewayRetryAfterBypassesLocalDownweightStep(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "explicit-retry-after", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	headers := make(http.Header)
	headers.Set("retry-after", "45")
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.99")
	before := time.Now().UTC()
	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, rateLimitPolicy{
		DownweightEnabled: true, DownweightStepped: true, DownweightBaseMinutes: 5, DownweightStepMinutes: 5,
	}, &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var resetRaw, downweightRaw string
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at, rate_limit_downweight_until FROM accounts WHERE id = ?`, created.ID).Scan(&resetRaw, &downweightRaw); err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{"cooldown": resetRaw, "downweight": downweightRaw} {
		deadline, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t.Fatal(err)
		}
		if delay := deadline.Sub(before); delay < 44*time.Second || delay > 46*time.Second {
			t.Fatalf("%s used local step instead of retry-after: %s", name, delay)
		}
	}
}

func TestGatewayExplicitQuotaResetBypassesLocalDownweightStep(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "explicit-quota-reset", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	now := time.Now().UTC()
	fiveHourReset := now.Add(2 * time.Hour).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(fiveHourReset.Unix(), 10))
	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, rateLimitPolicy{
		DownweightEnabled: true, DownweightStepped: true, DownweightBaseMinutes: 5, DownweightStepMinutes: 5,
	}, &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var reason, window, downweightRaw string
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_window, rate_limit_downweight_until FROM accounts WHERE id = ?`, created.ID).Scan(&reason, &window, &downweightRaw); err != nil {
		t.Fatal(err)
	}
	deadline, err := time.Parse(time.RFC3339Nano, downweightRaw)
	if err != nil {
		t.Fatal(err)
	}
	want := staggeredAccountQuotaRelease(created.ID, fiveHourReset)
	if reason != "quota_exhausted" || window != "5h" || !deadline.Equal(want) {
		t.Fatalf("explicit quota reset = reason %q, window %q, deadline %s; want quota_exhausted/5h/%s", reason, window, deadline, want)
	}
}

func TestGatewayFiveHourReleaseStaggerCanBeDisabledPerGroup(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "关闭 5h 错峰", "rate_multiplier": 1, "status": "active",
		"five_hour_release_stagger_enabled":     false,
		"five_hour_release_stagger_min_minutes": 15,
		"five_hour_release_stagger_max_minutes": 30,
	}, http.StatusOK, nil)
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	if key.FiveHourStaggerEnabled || key.FiveHourStaggerMin != 15 || key.FiveHourStaggerMax != 30 {
		t.Fatalf("gateway did not load disabled 5h stagger policy: %+v", key)
	}
	created := createGatewayTestAccount(t, a, handler, "no-five-hour-stagger", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	reset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset.Unix(), 10))
	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, key.rateLimitPolicy(), &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var raw string
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	got, ok := parseQuotaResetTime(raw)
	if !ok || !got.Equal(reset) {
		t.Fatalf("disabled 5h stagger released at %q, want exact reset %s", raw, reset)
	}
}

func TestGatewayFiveHourReleaseStaggerUsesConfiguredRange(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "custom-five-hour-stagger", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	reset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	policy := rateLimitPolicy{
		DownweightEnabled: true, FiveHourStaggerSet: true, FiveHourStaggerEnabled: true,
		FiveHourStaggerMin: 2, FiveHourStaggerMax: 4,
	}
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset.Unix(), 10))
	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, policy, &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var raw string
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	got, ok := parseQuotaResetTime(raw)
	if !ok || got.Sub(reset) < 2*time.Minute || got.Sub(reset) > 4*time.Minute {
		t.Fatalf("configured 5h stagger released at %q, delay=%s, want 2-4m", raw, got.Sub(reset))
	}
	want := policy.quotaReleaseDeadline(created.ID, "5h", reset)
	if !got.Equal(want) {
		t.Fatalf("configured 5h stagger is unstable: got %s, want %s", got, want)
	}
	sevenDay := policy.quotaReleaseDeadline(created.ID, "7d", reset)
	if delay := sevenDay.Sub(reset); delay < accountQuotaReleaseDelayMin || delay > accountQuotaReleaseDelayMax {
		t.Fatalf("5h policy changed 7d stagger: %s", delay)
	}
}

// The switch gates the complete transient-429 path: strike tracking, learned
// RPM threshold, downweighting and the short cooldown.
func TestGatewayDisabledDownweightSkipsTransientCooldown(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "no-downweight", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	a.recordGatewayAccountRPM(created.ID)

	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10,
		rateLimitPolicy{DownweightEnabled: false, CoolingThreshold: 3, CooldownSeconds: 120},
		&http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)})

	var consecutive int
	var reason string
	var downweight, resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason, rate_limit_downweight_until, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).
		Scan(&consecutive, &reason, &downweight, &resetAt); err != nil {
		t.Fatal(err)
	}
	if consecutive != 0 || reason != "" || downweight.Valid || resetAt.Valid {
		t.Fatalf("disabled adaptive cooling state: consecutive %d, reason %q, downweight %v, reset %v", consecutive, reason, downweight, resetAt)
	}
	var learned int
	if err := a.db.QueryRow(`SELECT rpm_limit FROM account_rpm_thresholds WHERE account_id = ?`, created.ID).Scan(&learned); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("disabled switch unexpectedly learned RPM threshold = %d, err=%v", learned, err)
	}
}

// Covers the whole chain rather than just the gate: group column →
// authenticateGatewayKey → gatewayKey → rateLimitPolicy → capture.
func TestGatewayGroupSwitchGovernsDownweightEndToEnd(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`)
	}))
	defer limited.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "关闭降权", "rate_multiplier": 1, "status": "active",
		"rate_limit_downweight_enabled": false,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "switch-off", limited.URL, 0, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token"})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	var consecutive int
	var reason string
	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason FROM accounts WHERE id = ?`, created.ID).Scan(&consecutive, &reason); err != nil {
		t.Fatal(err)
	}
	if consecutive != 0 || reason != "" {
		t.Fatalf("group switch off state: consecutive %d, reason %q", consecutive, reason)
	}

	// Turning it back on must take effect without a restart. Clear the standard
	// 429 fallout first (short cooldown plus learned RPM cap), which would
	// otherwise keep the account out of the next dispatch.
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "开启降权", "rate_multiplier": 1, "status": "active",
		"rate_limit_downweight_enabled": true,
	}, http.StatusOK, nil)
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = NULL WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`DELETE FROM account_rpm_thresholds`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`DELETE FROM account_rpm_events`); err != nil {
		t.Fatal(err)
	}
	retry := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	retry.Header.Set("Authorization", "Bearer "+key.Key)
	handler.ServeHTTP(httptest.NewRecorder(), retry)

	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason FROM accounts WHERE id = ?`, created.ID).Scan(&consecutive, &reason); err != nil {
		t.Fatal(err)
	}
	if consecutive != 1 || reason != "429_cooling" {
		t.Fatalf("group switch was on but the 429 was not recorded: consecutive %d, reason %q", consecutive, reason)
	}
}

// Turning the switch off must take effect immediately, including for accounts
// already carrying a penalty — otherwise they stay deprioritised for up to the
// rest of the quota window despite the feature being off.
func TestGatewayDisabledDownweightIgnoresExistingPenalty(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	penalised := createGatewayTestAccount(t, a, handler, "penalised", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	plain := createGatewayTestAccount(t, a, handler, "plain", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	// Earned while the switch was on, or by another group that still runs it.
	// Give it the more recent use as well, so the default RPM-dispatch ordering
	// would prefer it: the only thing that can push it last is the penalty.
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 2, rate_limit_reason = '429_backoff', rate_limit_downweight_until = ?, last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339Nano),
		time.Now().UTC().Format(time.RFC3339Nano), penalised.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), plain.ID); err != nil {
		t.Fatal(err)
	}

	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(selected.ID)
	if selected.ID != plain.ID {
		t.Fatalf("switch on: selected %d, want the unpenalised account %d", selected.ID, plain.ID)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "关闭降权", "rate_multiplier": 1, "status": "active",
		"rate_limit_downweight_enabled": false,
	}, http.StatusOK, nil)
	// Phase one left traffic on the winner, and RPM dispatch deliberately
	// concentrates on an already-active account. Clear it so the only thing
	// separating the two candidates is the penalty.
	if _, err := a.db.Exec(`DELETE FROM account_rpm_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), penalised.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET last_used_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), plain.ID); err != nil {
		t.Fatal(err)
	}
	offKey, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	// With the feature off the stored penalty must not reorder anything, so the
	// usual least-recently-used ordering picks the account it skipped before.
	offSelected, err := a.tryAcquireGatewayAccountWithPolicy(offKey, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(offSelected.ID)
	if offSelected.ID != penalised.ID {
		t.Fatalf("switch off: selected %d, want %d — the stored penalty still reordered dispatch", offSelected.ID, penalised.ID)
	}
}

// A threshold of 1 means the first 429 parks the account outright.
func TestGatewayCoolingThresholdOfOneParksImmediately(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "instant-park", "https://example.test", 0, nil, map[string]any{"access_token": "token"})

	a.captureAccount429State(created.ID, rateLimitPolicy{DownweightEnabled: true, CoolingThreshold: 1},
		&http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)})

	var reason string
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&reason, &resetAt); err != nil {
		t.Fatal(err)
	}
	if reason != "429_cooling" || !resetAt.Valid {
		t.Fatalf("threshold 1 did not park on the first strike: reason %q, reset %v", reason, resetAt)
	}
}

func TestCompatibilityAuthLaneUsesAccountTypeWithMixedCredentials(t *testing.T) {
	body := []byte(`{"model":"claude-alias","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	account := gatewayAccount{
		AuthType:        "api_key",
		CredentialsJSON: `{"access_token":"stale-oauth-token","api_key":"upstream-api-key"}`,
		ExtraJSON:       `{"model_mapping":{"claude-alias":"claude-upstream"}}`,
	}
	prepared, err := prepareClaudeRequest(request, body, account, "claude-alias", false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.OAuth || prepared.Model != "claude-upstream" {
		t.Fatalf("OAuth=%v model=%q", prepared.OAuth, prepared.Model)
	}
	headers := http.Header{}
	if err := buildClaudeHeaders(headers, request.Header, prepared, account.CredentialsJSON); err != nil {
		t.Fatal(err)
	}
	if headers.Get("x-api-key") != "upstream-api-key" || headers.Get("Authorization") != "" {
		t.Fatalf("unexpected auth headers: %#v", headers)
	}
}

func TestRealClaudeCodeRequestPreservesSystem(t *testing.T) {
	payload := map[string]any{
		"model": "claude-test", "max_tokens": 32,
		"system":   []any{map[string]any{"type": "text", "text": claudeCodeSystemPrompt}, map[string]any{"type": "text", "text": "client-owned-cache", "cache_control": map[string]any{"type": "ephemeral"}}},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"metadata": map[string]any{"user_id": `{"device_id":"device","account_uuid":"account","session_id":"11111111-1111-4111-8111-111111111111"}`},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("User-Agent", "claude-cli/2.1.220 (external, cli)")
	request.Header.Set("X-App", "cli")
	request.Header.Set("anthropic-beta", "claude-code-20250219")
	request.Header.Set("anthropic-version", claudeAPIVersion)
	prepared, err := prepareClaudeRequest(request, body, gatewayAccount{ID: 7, AuthType: "oauth", CredentialsJSON: `{"access_token":"token","account_uuid":"account"}`}, "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.ClaudeCode || prepared.Mimic {
		t.Fatalf("classification ClaudeCode=%v Mimic=%v", prepared.ClaudeCode, prepared.Mimic)
	}
	var transformed map[string]any
	_ = json.Unmarshal(prepared.Body, &transformed)
	system := transformed["system"].([]any)
	if len(system) != 2 || system[1].(map[string]any)["text"] != "client-owned-cache" {
		t.Fatalf("real Claude Code system changed: %#v", system)
	}
}

func TestClaudeCLIWithInvalidMetadataUsesSub2MimicLane(t *testing.T) {
	body := []byte(`{"model":"claude-test","max_tokens":32,"system":"keep this instruction","messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"not-a-claude-code-id"}}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("User-Agent", "claude-cli/2.1.220 (external, cli)")
	prepared, err := prepareClaudeRequest(request, body, gatewayAccount{ID: 7, AuthType: "oauth", CredentialsJSON: `{"access_token":"token","account_uuid":"account"}`}, "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ClaudeCode || !prepared.Mimic {
		t.Fatalf("classification ClaudeCode=%v Mimic=%v", prepared.ClaudeCode, prepared.Mimic)
	}
}

func TestCompatibilitySignatureRetryReturnsStrongRetryResponse(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch calls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"thinking.signature is invalid"}}`)
		case 2:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tool_use signature is invalid"}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"strong-final"}}`)
		}
	}))
	defer upstream.Close()

	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":2048,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"sig"},{"type":"tool_use","id":"toolu_1","name":"search","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"result"}]}],"tools":[{"name":"search","description":"search","input_schema":{"type":"object"}}]}`)
	clientRequest := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	prepared, err := prepareClaudeRequest(clientRequest, body, gatewayAccount{ID: 9, AuthType: "oauth", CredentialsJSON: `{"access_token":"token"}`}, "claude-sonnet-4-5", false)
	if err != nil {
		t.Fatal(err)
	}
	if !sub2service.IsCCMaxCompatibilitySignatureError([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"thinking.signature is invalid"}}`), prepared.Model) {
		t.Fatalf("signature error was not detected for model %q", prepared.Model)
	}
	request, err := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewReader(prepared.Body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header = prepared.Compat.Headers.Clone()
	response, err := upstream.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response = retryGatewayCompatibility400(upstream.Client(), request, response, prepared, time.Now())
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || response.StatusCode != http.StatusBadRequest || !strings.Contains(string(responseBody), "strong-final") {
		t.Fatalf("calls=%d status=%d body=%s", calls.Load(), response.StatusCode, responseBody)
	}
}

func TestCompatibilityAPIKeyPoolModeRetriesSameAccountLikeSub2(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"pool busy"}}`)
	}))
	defer upstream.Close()

	body := []byte(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
	request, err := http.NewRequest(http.MethodPost, upstream.URL, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	prepared := claudePreparedRequest{
		AuthType: "api_key", Compat: &sub2service.CCMaxCompatibilityPrepared{},
		Credentials: map[string]any{
			"api_key": "upstream-key", "pool_mode": true, "pool_mode_retry_count": float64(2),
		},
	}
	response, err := doGatewayUpstreamRequest(httptest.NewRequest(http.MethodPost, "/v1/messages", nil), upstream.Client(), request, prepared)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusTooManyRequests || calls.Load() != 3 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, calls.Load())
	}
	if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
		t.Fatal("default account state handling must be skipped in pool mode")
	}
}

func TestGatewayStreamsBeforeUpstreamCompletes(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "stream", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	gatewayServer := httptest.NewServer(handler)
	defer gatewayServer.Close()

	payload := `{"model":"claude-test","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	request, _ := http.NewRequest(http.MethodPost, gatewayServer.URL+"/v1/messages", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	responseCh := make(chan *http.Response, 1)
	errorCh := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		if err != nil {
			errorCh <- err
			return
		}
		responseCh <- response
	}()
	var response *http.Response
	select {
	case response = <-responseCh:
	case err := <-errorCh:
		close(release)
		t.Fatal(err)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("gateway did not expose streaming response headers before upstream completion")
	}
	reader := bufio.NewReader(response.Body)
	lineCh := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		if !strings.Contains(line, "message_start") {
			t.Fatalf("first SSE line=%q", line)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("gateway buffered the first SSE event")
	}
	close(release)
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
}

func TestGatewaySendsHeartbeatBeforeFirstStreamEvent(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	t.Setenv("CCMAX_STREAM_HEARTBEAT_INTERVAL", "20ms")
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "heartbeat", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	gatewayServer := httptest.NewServer(handler)
	defer gatewayServer.Close()

	payload := `{"model":"claude-test","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	request, _ := http.NewRequest(http.MethodPost, gatewayServer.URL+"/v1/messages", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(request)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	if line != ": ping\n" {
		close(release)
		t.Fatalf("first stream line = %q, want heartbeat", line)
	}
	blank, err := reader.ReadString('\n')
	if err != nil || blank != "\n" {
		close(release)
		t.Fatalf("heartbeat terminator = %q, err=%v", blank, err)
	}
	close(release)
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("message_stop")) {
		t.Fatalf("stream body after heartbeat = %s", body)
	}
}

func TestGatewayFailsOverWhenUpstreamStreamGoesIdle(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	t.Setenv("CCMAX_UPSTREAM_STREAM_IDLE_TIMEOUT", "150ms")
	// A heartbeat fires before the idle deadline, so this also proves a stalled
	// attempt can still fail over after the status line went out.
	t.Setenv("CCMAX_STREAM_HEARTBEAT_INTERVAL", "20ms")
	var stalledCalls, healthyCalls atomic.Int32
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalledCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	defer close(release)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer healthy.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "idle-stream", stalled.URL, 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "healthy-stream", healthy.URL, 1, nil, map[string]any{"access_token": "token-b"})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("failover did not stream the healthy account: %s", body)
	}
	if !strings.Contains(body, ": ping") {
		t.Fatalf("expected heartbeat before failover: %s", body)
	}
	if stalledCalls.Load() != 1 || healthyCalls.Load() != 1 {
		t.Fatalf("upstream calls stalled=%d healthy=%d", stalledCalls.Load(), healthyCalls.Load())
	}
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, first.ID).Scan(&resetAt); err != nil {
		t.Fatal(err)
	}
	if !resetAt.Valid {
		t.Fatal("stalled account was not parked after the idle timeout")
	}
}

func TestGatewayReportsIdleStreamAfterPartialOutputWithoutFailover(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	t.Setenv("CCMAX_UPSTREAM_STREAM_IDLE_TIMEOUT", "150ms")
	var stalledCalls, backupCalls atomic.Int32
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stalledCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9}}}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stalled.Close()
	defer close(release)
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backup.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "partial-stream", stalled.URL, 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "unused-stream", backup.URL, 1, nil, map[string]any{"access_token": "token-b"})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	body := response.Body.String()
	if !strings.Contains(body, "message_start") {
		t.Fatalf("partial output was dropped: %s", body)
	}
	if !strings.Contains(body, "upstream_stream_idle") {
		t.Fatalf("client was not told the stream stalled: %s", body)
	}
	if backupCalls.Load() != 0 {
		t.Fatalf("committed stream must not fail over, backup calls=%d", backupCalls.Load())
	}
	if stalledCalls.Load() != 1 {
		t.Fatalf("stalled upstream calls=%d", stalledCalls.Load())
	}
}

func TestGatewayFailsOverOnSub2PreOutputSSEOverload(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var overloadedCalls, healthyCalls atomic.Int32
	overloaded := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		overloadedCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")
	}))
	defer overloaded.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer healthy.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "overloaded-stream", overloaded.URL, 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "healthy-stream", healthy.URL, 1, nil, map[string]any{"access_token": "token-b"})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("X-CCMAX-Account") != second.Name {
		t.Fatalf("response=%d account=%q body=%s", response.Code, response.Header().Get("X-CCMAX-Account"), response.Body.String())
	}
	if strings.Contains(response.Body.String(), "Overloaded") || !strings.Contains(response.Body.String(), "message_stop") {
		t.Fatalf("unexpected streamed body: %s", response.Body.String())
	}
	if overloadedCalls.Load() != 1 || healthyCalls.Load() != 1 {
		t.Fatalf("upstream calls overloaded=%d healthy=%d", overloadedCalls.Load(), healthyCalls.Load())
	}
	var accountResetAt sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, first.ID).Scan(&accountResetAt); err != nil {
		t.Fatal(err)
	}
	if accountResetAt.Valid {
		t.Fatalf("529 incorrectly set account-wide cooldown=%q", accountResetAt.String)
	}
	var modelResetAt string
	if err := a.db.QueryRow(`SELECT reset_at FROM account_model_cooldowns WHERE account_id = ? AND model = ?`, first.ID, "claude-test").Scan(&modelResetAt); err != nil {
		t.Fatalf("529 model cooldown missing: %v", err)
	}
	var firstRPM, secondRPM int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, first.ID).Scan(&firstRPM); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, second.ID).Scan(&secondRPM); err != nil {
		t.Fatal(err)
	}
	if firstRPM != 1 || secondRPM != 1 {
		t.Fatalf("RPM events overloaded=%d healthy=%d", firstRPM, secondRPM)
	}
}

func TestGatewayPassthroughStreamRateLimitLearnsRPMAndFailsOver(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var limitedCalls, healthyCalls atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		limitedCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"limited\"}}\n\n")
	}))
	defer limited.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer healthy.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "流式 429 failover", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "passthrough-limited", limited.URL, 0, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "passthrough-healthy", healthy.URL, 1, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token-b"})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("X-CCMAX-Account") != second.Name {
		t.Fatalf("response=%d account=%q body=%s", response.Code, response.Header().Get("X-CCMAX-Account"), response.Body.String())
	}
	if strings.Contains(response.Body.String(), "rate_limit_error") || !strings.Contains(response.Body.String(), "message_stop") {
		t.Fatalf("unexpected streamed body: %s", response.Body.String())
	}
	if limitedCalls.Load() != 1 || healthyCalls.Load() != 1 {
		t.Fatalf("upstream calls limited=%d healthy=%d", limitedCalls.Load(), healthyCalls.Load())
	}
	var learnedRPM int
	if err := a.db.QueryRow(`SELECT rpm_limit FROM account_rpm_thresholds WHERE account_id = ?`, first.ID).Scan(&learnedRPM); err != nil {
		t.Fatal(err)
	}
	if learnedRPM != 1 {
		t.Fatalf("learned RPM=%d, want 1", learnedRPM)
	}
}

func TestStreamHedgeDisabledPreservesSerialSelection(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		time.Sleep(80 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backup.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "serial-primary", primary.URL, 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "serial-backup", backup.URL, 1, nil, map[string]any{"access_token": "token-b"})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"serial-only"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("X-CCMAX-Account") != first.Name {
		t.Fatalf("response=%d account=%q body=%s", response.Code, response.Header().Get("X-CCMAX-Account"), response.Body.String())
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 0 {
		t.Fatalf("serial calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestStreamHedgeSkipsBackupWhenPrimaryBootstrapsBeforeDelay(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	previousDelay := gatewayStreamHedgeDelay
	gatewayStreamHedgeDelay = 100 * time.Millisecond
	defer func() { gatewayStreamHedgeDelay = previousDelay }()
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		http.Error(w, "backup should not start", http.StatusInternalServerError)
	}))
	defer backup.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	enableGatewayGroupStreamHedge(t, handler)
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "fast-primary", primary.URL, 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "unused-backup", backup.URL, 1, nil, map[string]any{"access_token": "token-b"})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"fast-primary"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("X-CCMAX-Account") != first.Name {
		t.Fatalf("response=%d account=%q body=%s", response.Code, response.Header().Get("X-CCMAX-Account"), response.Body.String())
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 0 {
		t.Fatalf("hedge calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestStreamHedgeEnabledPreservesNonStreamSelection(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-primary","type":"message","role":"assistant","content":[{"type":"text","text":"primary"}],"model":"claude-test","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		http.Error(w, "backup should not start", http.StatusInternalServerError)
	}))
	defer backup.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	enableGatewayGroupStreamHedge(t, handler)
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "non-stream-primary", primary.URL, 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "non-stream-backup", backup.URL, 1, nil, map[string]any{"access_token": "token-b"})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"non-stream"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("X-CCMAX-Account") != first.Name {
		t.Fatalf("response=%d account=%q body=%s", response.Code, response.Header().Get("X-CCMAX-Account"), response.Body.String())
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 0 {
		t.Fatalf("non-stream calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestStreamHedgePreservesExistingStickyAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	previousDelay := gatewayStreamHedgeDelay
	gatewayStreamHedgeDelay = 20 * time.Millisecond
	defer func() { gatewayStreamHedgeDelay = previousDelay }()
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := primaryCalls.Add(1)
		if call > 1 {
			time.Sleep(60 * time.Millisecond)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backup.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	enableGatewayGroupStreamHedge(t, handler)
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "sticky-primary", primary.URL, 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "sticky-backup", backup.URL, 1, nil, map[string]any{"access_token": "token-b"})
	payload := `{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"sticky-session"}]}`
	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
		request.Header.Set("Authorization", "Bearer "+key.Key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || response.Header().Get("X-CCMAX-Account") != first.Name {
			t.Fatalf("request %d response=%d account=%q body=%s", i+1, response.Code, response.Header().Get("X-CCMAX-Account"), response.Body.String())
		}
	}
	if primaryCalls.Load() != 2 || backupCalls.Load() != 0 {
		t.Fatalf("sticky calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
}

func TestStreamHedgeChoosesFastestBootstrapAndCancelsLoser(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	previousDelay := gatewayStreamHedgeDelay
	gatewayStreamHedgeDelay = 20 * time.Millisecond
	defer func() { gatewayStreamHedgeDelay = previousDelay }()
	var primaryCalls, backupCalls atomic.Int32
	primaryCanceled := make(chan struct{}, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			primaryCanceled <- struct{}{}
			return
		case <-time.After(500 * time.Millisecond):
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		}
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backupCalls.Add(1)
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req-hedge-winner")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"fast\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer backup.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	enableGatewayGroupStreamHedge(t, handler)
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "slow-primary", primary.URL, 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "fast-backup", backup.URL, 1, nil, map[string]any{"access_token": "token-b"})
	payload := `{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hedge-winner"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, request)
	elapsed := time.Since(started)

	if response.Code != http.StatusOK || response.Header().Get("X-CCMAX-Account") != second.Name || !strings.Contains(response.Body.String(), `"text":"fast"`) {
		t.Fatalf("response=%d account=%q body=%s", response.Code, response.Header().Get("X-CCMAX-Account"), response.Body.String())
	}
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("hedged response took %v", elapsed)
	}
	if primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
		t.Fatalf("hedge calls primary=%d backup=%d", primaryCalls.Load(), backupCalls.Load())
	}
	select {
	case <-primaryCanceled:
	case <-time.After(time.Second):
		t.Fatal("losing upstream request was not canceled")
	}
	var firstInflight, secondInflight, firstRPM, secondRPM int
	if err := a.db.QueryRow(`SELECT requests FROM account_inflight WHERE account_id = ?`, first.ID).Scan(&firstInflight); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT requests FROM account_inflight WHERE account_id = ?`, second.ID).Scan(&secondInflight); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, first.ID).Scan(&firstRPM); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, second.ID).Scan(&secondRPM); err != nil {
		t.Fatal(err)
	}
	if firstInflight != 0 || secondInflight != 0 || firstRPM != 1 || secondRPM != 1 {
		t.Fatalf("inflight=(%d,%d) rpm=(%d,%d)", firstInflight, secondInflight, firstRPM, secondRPM)
	}
	var usageAccountID int64
	if err := a.db.QueryRow(`SELECT account_id FROM usage_logs WHERE request_id = 'req-hedge-winner'`).Scan(&usageAccountID); err != nil || usageAccountID != second.ID {
		t.Fatalf("usage account=%d err=%v", usageAccountID, err)
	}
	session := sub2service.GenerateCCMaxCompatibilitySessionHash([]byte(payload), "192.0.2.1", "", key.ID)
	if sticky := a.gatewayStickyAccountID(key.ID, session); sticky != second.ID {
		t.Fatalf("sticky account=%d, want winner %d", sticky, second.ID)
	}
}

func enableGatewayGroupStreamHedge(t *testing.T, handler http.Handler) {
	t.Helper()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "首包竞速", "rate_multiplier": 1,
		"status": "active", "normal_request_mode": false, "stream_hedge_enabled": true,
	}, http.StatusOK, nil)
}

func TestAdaptiveStreamHedgeUsesLoadAwarePrimary(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var busyCalls, idleCalls atomic.Int32
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		busyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer busy.Close()
	idle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		idleCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer idle.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	enableGatewayGroupAdaptiveHedge(t, handler)
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "busy-primary", busy.URL, 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "idle-primary", idle.URL, 0, nil, map[string]any{"access_token": "token-b"})
	if _, err := a.db.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 8) ON CONFLICT(account_id) DO UPDATE SET requests = 8`, first.ID); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"load-aware"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("X-CCMAX-Account") != second.Name {
		t.Fatalf("response=%d account=%q body=%s", response.Code, response.Header().Get("X-CCMAX-Account"), response.Body.String())
	}
	if busyCalls.Load() != 0 || idleCalls.Load() != 1 {
		t.Fatalf("adaptive calls busy=%d idle=%d", busyCalls.Load(), idleCalls.Load())
	}
}

func TestAdaptiveStreamHedgeBudgetAndDelay(t *testing.T) {
	controller := newGatewayHedgeController()
	if delay := controller.begin("a"); delay != gatewayAdaptiveDefaultDelay {
		t.Fatalf("initial delay=%v", delay)
	}
	if !controller.reserve("a") {
		t.Fatal("initial adaptive hedge credit was not available")
	}
	for index := 0; index < 9; index++ {
		controller.begin("a")
	}
	if controller.reserve("a") {
		t.Fatal("adaptive hedge exceeded the 10 percent request budget")
	}
	controller.begin("a")
	if !controller.reserve("a") {
		t.Fatal("adaptive hedge budget did not replenish after ten requests")
	}
	for _, sample := range []time.Duration{100, 200, 300, 400, 500} {
		controller.observe("b", sample*time.Millisecond)
	}
	if delay := controller.begin("b"); delay != 500*time.Millisecond {
		t.Fatalf("adaptive P90 delay=%v", delay)
	}
}

func TestGroupRejectsBothStreamHedgeAlgorithms(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "invalid dual mode", "rate_multiplier": 1,
		"status": "active", "normal_request_mode": false,
		"stream_hedge_enabled": true, "adaptive_hedge_enabled": true,
	}, http.StatusBadRequest, nil)
}

func enableGatewayGroupAdaptiveHedge(t *testing.T, handler http.Handler) {
	t.Helper()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "大 RPM 自适应", "rate_multiplier": 1,
		"status": "active", "normal_request_mode": false,
		"stream_hedge_enabled": false, "adaptive_hedge_enabled": true,
	}, http.StatusOK, nil)
}

func TestCountTokensUsesSingleAccountAndSub2ModelRules(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var firstCalls, secondCalls atomic.Int32
	mappedModel := make(chan string, 1)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		if r.URL.Path != "/v1/messages/count_tokens" || r.URL.Query().Get("beta") != "true" {
			t.Errorf("count_tokens target=%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		for _, field := range []string{"temperature", "top_p", "top_k", "stream", "stop_sequences", "stop", "max_tokens"} {
			if _, exists := body[field]; exists {
				t.Errorf("count_tokens forwarded generation field %q", field)
			}
		}
		mappedModel <- body["model"].(string)
		if !strings.Contains(r.Header.Get("anthropic-beta"), "token-counting-2024-11-01") {
			t.Errorf("count_tokens beta=%s", r.Header.Get("anthropic-beta"))
		}
		_, _ = w.Write([]byte(`{"input_tokens":9}`))
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		http.Error(w, "unexpected failover", http.StatusInternalServerError)
	}))
	defer second.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	extra := map[string]any{"supported_models": []string{"claude-alias"}, "model_mapping": map[string]any{"claude-alias": "claude-upstream"}}
	createGatewayTestAccount(t, a, handler, "first", first.URL, 0, extra, map[string]any{"access_token": "first-token"})
	createGatewayTestAccount(t, a, handler, "second", second.URL, 1, extra, map[string]any{"access_token": "second-token"})
	requestJSON(t, handler, http.MethodPost, "/v1/messages/count_tokens", map[string]any{
		"model": "claude-alias", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"temperature": 0.7, "top_p": 0.9, "top_k": 40, "stream": true,
		"stop_sequences": []string{"END"}, "stop": []string{"DONE"}, "max_tokens": 1024,
	}, nil, key.Key, http.StatusOK, nil)
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("count_tokens calls first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
	if got := <-mappedModel; got != "claude-alias" {
		t.Fatalf("mapped model=%q", got)
	}
	var models struct {
		Data []gatewayModel `json:"data"`
	}
	requestJSON(t, handler, http.MethodGet, "/v1/models", nil, nil, key.Key, http.StatusOK, &models)
	if len(models.Data) != 1 || models.Data[0].ID != "claude-alias" {
		t.Fatalf("models=%+v", models.Data)
	}
}

func TestGatewayModelsFallsBackToSub2DefaultsWithoutModelMapping(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	for _, model := range []string{"claude-sonnet-4-5", "claude-opus-4-6"} {
		if _, err := a.db.Exec(`INSERT INTO model_prices (model) VALUES (?)`, model); err != nil {
			t.Fatal(err)
		}
	}
	createGatewayTestAccount(
		t,
		a,
		handler,
		"wildcard-models",
		"https://api.anthropic.com",
		0,
		map[string]any{"supported_models": []string{"claude-sonnet-*"}},
		map[string]any{"access_token": "test-token"},
	)
	var result struct {
		Object string         `json:"object"`
		Data   []gatewayModel `json:"data"`
	}
	response := requestJSON(t, handler, http.MethodGet, "/models", nil, nil, key.Key, http.StatusOK, nil)
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Object != "list" || len(result.Data) != len(sub2claude.DefaultModels) {
		t.Fatalf("models=%+v", result.Data)
	}
	for index := range sub2claude.DefaultModels {
		if result.Data[index] != sub2claude.DefaultModels[index] {
			t.Fatalf("model[%d]=%+v, want %+v", index, result.Data[index], sub2claude.DefaultModels[index])
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 || raw["object"] != "list" {
		t.Fatalf("response fields=%+v", raw)
	}
	detailID := sub2claude.DefaultModels[0].ID
	var model gatewayModel
	requestJSON(t, handler, http.MethodGet, "/models/"+detailID, nil, nil, key.Key, http.StatusOK, &model)
	if model != sub2claude.DefaultModels[0] {
		t.Fatalf("model=%+v", model)
	}
	requestJSON(t, handler, http.MethodGet, "/models/not-configured", nil, nil, key.Key, http.StatusNotFound, nil)
}

func TestGatewayNoAccountClassificationMatchesSub2(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	extra := map[string]any{"supported_models": []string{"claude-known"}}
	created := createGatewayTestAccount(t, a, handler, "temporarily-unavailable", "https://unused.example.test", 0, extra, map[string]any{"access_token": "token"})
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ? WHERE id = ?`, future, created.ID); err != nil {
		t.Fatal(err)
	}

	request := func(model string) *httptest.ResponseRecorder {
		body := `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key.Key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	unsupported := request("claude-unknown")
	if unsupported.Code != http.StatusNotFound || !strings.Contains(unsupported.Body.String(), "model_not_found") {
		t.Fatalf("unsupported status=%d body=%s", unsupported.Code, unsupported.Body.String())
	}
	unavailable := request("claude-known")
	if unavailable.Code != http.StatusServiceUnavailable || !strings.Contains(unavailable.Body.String(), "api_error") {
		t.Fatalf("unavailable status=%d body=%s", unavailable.Code, unavailable.Body.String())
	}
}

func TestGatewayNeverDispatchesWithoutAnActiveProxy(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "no-proxy", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{"custom_forward_url": upstream.URL},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 0, "rate_multiplier": 1, "group_ids": []string{"a"},
		"base_rpm": 15, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, nil)

	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusServiceUnavailable, nil)
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("unproxied upstream calls=%d, want 0", calls)
	}
}

func TestPassthroughCountTokensPreservesRawBody(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- body
		_, _ = w.Write([]byte(`{"input_tokens":12}`))
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "passthrough-count", upstream.URL, 0, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token"})

	rawBody := []byte("{\n  \"model\": \"claude-test\",\n  \"messages\": [{\"role\":\"user\",\"content\":\"hello\"}],\n  \"max_tokens\": 512,\n  \"stream\": true,\n  \"temperature\": 0.7,\n  \"custom_parameter\": {\"keep\": true}\n}\n")
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(rawBody))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}
	if got := <-captured; !bytes.Equal(got, rawBody) {
		t.Fatalf("raw count_tokens body changed\n got: %s\nwant: %s", got, rawBody)
	}
	var rpmEvents int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, created.ID).Scan(&rpmEvents); err != nil || rpmEvents != 1 {
		t.Fatalf("count_tokens RPM events=%d err=%v", rpmEvents, err)
	}
}

func TestGatewayTruncatedStreamEmitsErrorAndRecordsPartialUsage(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req-truncated-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":9,\"cache_read_input_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n")
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "truncated-stream", upstream.URL, 0, nil, map[string]any{"access_token": "token"})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "upstream_disconnected") {
		t.Fatalf("truncated stream response=%d %s", response.Code, response.Body.String())
	}
	var input, output, cacheRead int64
	if err := a.db.QueryRow(`SELECT input_tokens, output_tokens, cache_read_tokens FROM usage_logs WHERE request_id = 'req-truncated-stream'`).Scan(&input, &output, &cacheRead); err != nil {
		t.Fatal(err)
	}
	if input != 9 || output != 3 || cacheRead != 2 {
		t.Fatalf("partial usage=%d/%d/%d", input, output, cacheRead)
	}
}

func TestGatewayUnauthorizedTemporarilyUnschedulesAndFailsOver(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Header.Get("Authorization") == "Bearer stale-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"message":"expired access token"}}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fresh-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		w.Header().Set("request-id", "req-refreshed")
		_, _ = w.Write([]byte(`{"id":"msg_refreshed","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "refresh-on-401", upstream.URL, 0, nil, map[string]any{
		"access_token": "stale-token", "refresh_token": "refresh-token", "expires_at": time.Now().Add(time.Hour).Unix(),
	})
	createGatewayTestAccount(t, a, handler, "fallback-after-401", upstream.URL, 1, nil, map[string]any{
		"access_token": "fresh-token", "refresh_token": "fresh-refresh", "expires_at": time.Now().Add(time.Hour).Unix(),
	})
	var response map[string]any
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusOK, &response)
	if response["id"] != "msg_refreshed" || upstreamCalls.Load() != 2 {
		t.Fatalf("response=%#v upstream calls=%d", response, upstreamCalls.Load())
	}
	var authStatus, credentials string
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT auth_status, credentials_json, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&authStatus, &credentials, &resetAt); err != nil {
		t.Fatal(err)
	}
	if authStatus != "valid" || !strings.Contains(credentials, "stale-token") || !resetAt.Valid {
		t.Fatalf("account auth=%s credentials=%s reset=%v", authStatus, credentials, resetAt)
	}
}

func TestGatewayOAuth403FailsOverWithoutSameAccountRetry(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var forbiddenCalls, fallbackCalls atomic.Int32
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		forbiddenCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"type":"error","error":{"message":"account forbidden"}}`))
	}))
	defer forbidden.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"msg_fallback","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer fallback.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "forbidden", forbidden.URL, 0, nil, map[string]any{"access_token": "first-token"})
	createGatewayTestAccount(t, a, handler, "fallback", fallback.URL, 1, nil, map[string]any{"access_token": "second-token"})
	var response map[string]any
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusOK, &response)
	if response["id"] != "msg_fallback" || forbiddenCalls.Load() != 1 || fallbackCalls.Load() != 1 {
		t.Fatalf("response=%#v forbidden=%d fallback=%d", response, forbiddenCalls.Load(), fallbackCalls.Load())
	}
	var status, authStatus string
	if err := a.db.QueryRow(`SELECT status, auth_status FROM accounts WHERE id = ?`, first.ID).Scan(&status, &authStatus); err != nil {
		t.Fatal(err)
	}
	if status != "error" || authStatus != "reauth_required" {
		t.Fatalf("forbidden account status=%s auth=%s", status, authStatus)
	}
}

func TestCompatibilityRetriesThinkingSignature400OnSameAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if call == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"message":"Invalid signature in thinking block"}}`))
			return
		}
		if gjson.GetBytes(body, "thinking").Exists() || gjson.GetBytes(body, "messages.0.content.0.type").String() != "text" {
			t.Errorf("thinking retry body=%s", body)
		}
		_, _ = w.Write([]byte(`{"id":"msg_rectified","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "rectifier", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-5", "thinking": map[string]any{"type": "enabled", "budget_tokens": 2048}, "max_tokens": 4096,
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "reasoning", "signature": "valid"}, map[string]any{"type": "text", "text": "answer"}}},
			map[string]any{"role": "user", "content": "continue"},
		},
	}, nil, key.Key, http.StatusOK, nil)
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls.Load())
	}
	var rpmEvents int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, created.ID).Scan(&rpmEvents); err != nil || rpmEvents != 1 {
		t.Fatalf("successful request RPM events=%d err=%v", rpmEvents, err)
	}
}

func TestCompatibilityTransportFailureDoesNotSwitchAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var fallbackCalls atomic.Int32
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"should-not-arrive"}`))
	}))
	unreachable.Close()
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		_, _ = w.Write([]byte(`{"id":"msg_fallback","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer fallback.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	first := createGatewayTestAccount(t, a, handler, "transport-error", unreachable.URL, 0, nil, map[string]any{"access_token": "first-token"})
	createGatewayTestAccount(t, a, handler, "fallback", fallback.URL, 1, nil, map[string]any{"access_token": "second-token"})
	if first.ProxyID == nil {
		t.Fatal("first account has no proxy")
	}
	if _, err := a.db.Exec(`UPDATE proxies SET port = 1 WHERE id = ?`, *first.ProxyID); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "Upstream request failed") {
		t.Fatalf("transport response=%d %s", response.Code, response.Body.String())
	}
	if fallbackCalls.Load() != 0 {
		t.Fatalf("fallback calls=%d, want 0", fallbackCalls.Load())
	}
}

func TestSub2CompatibilityErrorMapping(t *testing.T) {
	tests := []struct {
		name         string
		upstream     int
		countTokens  bool
		wantStatus   int
		wantType     string
		wantMessage  string
		upstreamBody string
		wantRawBody  bool
	}{
		{name: "messages unauthorized", upstream: 401, wantStatus: 502, wantType: "upstream_error", wantMessage: "Upstream authentication failed, please contact administrator"},
		{name: "messages rate limit", upstream: 429, wantStatus: 429, wantType: "rate_limit_error", wantMessage: "Upstream rate limit exceeded, please retry later"},
		{name: "messages bad request", upstream: 400, wantStatus: 400, upstreamBody: `{"type":"error","error":{"message":"bad input"}}`, wantRawBody: true},
		{name: "count tokens overloaded", upstream: 529, countTokens: true, wantStatus: 529, wantType: "upstream_error", wantMessage: "Service overloaded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			writeSub2CompatibilityError(response, tt.upstream, []byte(tt.upstreamBody), tt.countTokens)
			if response.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if tt.wantRawBody {
				if response.Body.String() != tt.upstreamBody {
					t.Fatalf("raw body=%s", response.Body.String())
				}
				return
			}
			var payload struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Error.Type != tt.wantType || payload.Error.Message != tt.wantMessage {
				t.Fatalf("error response=%s", response.Body.String())
			}
		})
	}
}

func TestSerialQueueTimeoutIsReturned(t *testing.T) {
	previous := gatewaySerialQueueTimeout
	gatewaySerialQueueTimeout = 10 * time.Millisecond
	t.Cleanup(func() { gatewaySerialQueueTimeout = previous })
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	_ = handler
	account := gatewayAccount{ID: 99, AuthType: "oauth", UserMsgQueueMode: "serial", BaseRPM: 15}
	release, err := a.acquireUserMessageQueue(context.Background(), account, []byte(`{"messages":[{"role":"user","content":"one"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := a.acquireUserMessageQueue(context.Background(), account, []byte(`{"messages":[{"role":"user","content":"two"}]}`), false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queue timeout err=%v", err)
	}
}

func TestGatewaySerialQueueTimeoutFailsOpenLikeSub2(t *testing.T) {
	previous := gatewaySerialQueueTimeout
	gatewaySerialQueueTimeout = 10 * time.Millisecond
	t.Cleanup(func() { gatewaySerialQueueTimeout = previous })
	t.Setenv("CCMAX_AUTH_DISABLED", "1")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":"msg_queue","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	account := createGatewayTestAccount(t, a, handler, "queue-fail-open", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`UPDATE accounts SET user_msg_queue_mode = 'serial' WHERE id = ?`, account.ID); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
	locked := gatewayAccount{ID: account.ID, AuthType: "oauth", BaseRPM: 15, UserMsgQueueMode: "serial"}
	release, err := a.acquireUserMessageQueue(context.Background(), locked, body, false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestConcurrentTokenRefreshUsesSingleUpstreamCall(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-token","refresh_token":"fresh-refresh","expires_in":3600,"token_type":"bearer"}`))
	}))
	defer server.Close()
	previousEndpoint := claudeTokenEndpoint
	claudeTokenEndpoint = server.URL
	defer func() { claudeTokenEndpoint = previousEndpoint }()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "refresh", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "old-token", "refresh_token": "old-refresh", "expires_at": time.Now().Add(-time.Minute).Unix()},
		"extra":       map[string]any{}, "status": "active", "schedulable": true, "concurrency": 10, "priority": 0, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)
	base := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"old-token","refresh_token":"old-refresh","expires_at":1}`}
	var wg sync.WaitGroup
	errorsCh := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.ensureGatewayAccountToken(context.Background(), base)
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d, want 1", calls.Load())
	}
}

func TestConcurrentForcedTokenRefreshUsesSingleUpstreamCall(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"forced-token","refresh_token":"forced-refresh","expires_in":3600,"token_type":"bearer"}`))
	}))
	defer server.Close()
	previousEndpoint := claudeTokenEndpoint
	claudeTokenEndpoint = server.URL
	defer func() { claudeTokenEndpoint = previousEndpoint }()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "forced-refresh", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "rejected-token", "refresh_token": "old-refresh", "expires_at": time.Now().Add(time.Hour).Unix()},
		"extra":       map[string]any{}, "status": "active", "schedulable": true, "concurrency": 10, "priority": 0, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)
	base := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"rejected-token","refresh_token":"old-refresh"}`}
	var wait sync.WaitGroup
	errorsCh := make(chan error, 8)
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := a.refreshGatewayAccountToken(context.Background(), base, true)
			errorsCh <- err
		}()
	}
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("forced refresh calls=%d, want 1", calls.Load())
	}
}

func TestGatewayRetryRecordsRateLimitReset(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	reset := time.Now().UTC().Add(time.Hour).Unix()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("anthropic-ratelimit-unified-5h-utilization", "1")
		w.Header().Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset, 10))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"message":"rate limited"}}`))
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "rate-limited", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 8, "messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusTooManyRequests, nil)
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&resetAt); err != nil {
		t.Fatal(err)
	}
	if !resetAt.Valid || resetAt.String == "" {
		t.Fatal("429 response did not persist a rate-limit reset")
	}
}

func TestGatewayUsesTransient429Cooldown(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "429 fallback", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	before := time.Now()

	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, defaultTestRateLimitPolicy(), &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)})

	var raw sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !raw.Valid {
		t.Fatal("expected account cooldown")
	}
	resetAt, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		t.Fatal(err)
	}
	delay := resetAt.Sub(before)
	if delay < 59*time.Second || delay > 61*time.Second {
		t.Fatalf("cooldown %s outside the 60s transient window", delay)
	}
}

func TestGateway529CooldownIsModelScopedAndConfigurable(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "model cooldown", "rate_multiplier": 1,
		"status": "active", "overload_cooldown_seconds": 12,
	}, http.StatusOK, nil)
	createdKey := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "model-overload", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	if key.OverloadCooldownSeconds != 12 {
		t.Fatalf("group 529 cooldown=%d, want 12", key.OverloadCooldownSeconds)
	}
	before := time.Now().UTC()

	a.captureGatewayUpstreamState(created.ID, "a", "Claude-Opus-5", key.OverloadCooldownSeconds, key.rateLimitPolicy(), &http.Response{StatusCode: 529, Header: make(http.Header)})

	var accountResetAt sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&accountResetAt); err != nil {
		t.Fatal(err)
	}
	if accountResetAt.Valid {
		t.Fatalf("529 incorrectly set account-wide cooldown=%q", accountResetAt.String)
	}
	var modelResetAtRaw string
	if err := a.db.QueryRow(`SELECT reset_at FROM account_model_cooldowns WHERE account_id = ? AND model = ?`, created.ID, "claude-opus-5").Scan(&modelResetAtRaw); err != nil {
		t.Fatal(err)
	}
	modelResetAt, err := time.Parse(time.RFC3339Nano, modelResetAtRaw)
	if err != nil {
		t.Fatal(err)
	}
	if delay := modelResetAt.Sub(before); delay < 11*time.Second || delay > 13*time.Second {
		t.Fatalf("model cooldown=%s, want about 12s", delay)
	}

	if _, err := a.acquireGatewayAccount(key, "same-model", "claude-opus-5", map[int64]bool{}); err == nil {
		t.Fatal("same account+model was selected during 529 cooldown")
	}
	selected, err := a.acquireGatewayAccount(key, "other-model", "claude-fable-5", map[int64]bool{})
	if err != nil {
		t.Fatalf("other model should remain schedulable: %v", err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	if selected.ID != created.ID {
		t.Fatalf("selected account=%d, want %d", selected.ID, created.ID)
	}
}

func TestGatewayDoesNotTrustAmbiguousAnthropicAggregateReset(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "ambiguous-aggregate-reset", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	before := time.Now().UTC()
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(before.Add(7*24*time.Hour).Unix(), 10))

	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, defaultTestRateLimitPolicy(), &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var raw sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !raw.Valid {
		t.Fatal("expected account cooldown")
	}
	resetAt, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		t.Fatal(err)
	}
	delay := resetAt.Sub(before)
	if delay < 59*time.Second || delay > 61*time.Second {
		t.Fatalf("ambiguous aggregate reset produced cooldown %s, want about 60s", delay)
	}
}

func TestGatewayChoosesSub2ExceededAnthropicWindow(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "seven-day-limited", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	now := time.Now().UTC()
	fiveHourReset := now.Add(2 * time.Hour).Truncate(time.Second)
	sevenDayReset := now.Add(2 * 24 * time.Hour).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(fiveHourReset.Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(sevenDayReset.Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.2")
	headers.Set("anthropic-ratelimit-unified-7d-surpassed-threshold", "true")

	a.captureAccountUpstreamState(created.ID, &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var raw sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !raw.Valid {
		t.Fatal("expected account cooldown")
	}
	resetAt, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		t.Fatal(err)
	}
	wantEligibleAt := staggeredAccountQuotaRelease(created.ID, sevenDayReset)
	if !resetAt.Equal(wantEligibleAt) {
		t.Fatalf("reset at %s, want staggered 7d release %s", resetAt, wantEligibleAt)
	}
	if delay := resetAt.Sub(sevenDayReset); delay < accountQuotaReleaseDelayMin || delay > accountQuotaReleaseDelayMax {
		t.Fatalf("7d stagger delay = %s, want 15-30m", delay)
	}
}

func TestGatewayRecordsSelectedFiveHourWindow(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "five-hour-limited", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	now := time.Now().UTC()
	fiveHourReset := now.Add(2 * time.Hour).Truncate(time.Second)
	sevenDayReset := now.Add(4 * 24 * time.Hour).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(fiveHourReset.Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(sevenDayReset.Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "0.41")

	a.captureAccountUpstreamState(created.ID, &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var resetAt, quotaResetAt, window string
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at, quota_5h_reset_at, rate_limit_window FROM accounts WHERE id = ?`, created.ID).Scan(&resetAt, &quotaResetAt, &window); err != nil {
		t.Fatal(err)
	}
	if quotaResetAt != fiveHourReset.Format(time.RFC3339Nano) || window != "5h" {
		t.Fatalf("stored quota window = %s/%s, want %s/5h", quotaResetAt, window, fiveHourReset.Format(time.RFC3339Nano))
	}
	eligibleAt, err := time.Parse(time.RFC3339Nano, resetAt)
	if err != nil {
		t.Fatal(err)
	}
	wantEligibleAt := staggeredAccountQuotaRelease(created.ID, fiveHourReset)
	if !eligibleAt.Equal(wantEligibleAt) {
		t.Fatalf("staggered release = %s, want %s", eligibleAt, wantEligibleAt)
	}
	if delay := eligibleAt.Sub(fiveHourReset); delay < accountQuotaReleaseDelayMin || delay > accountQuotaReleaseDelayMax {
		t.Fatalf("stagger delay = %s, want 15-30m", delay)
	}
}

func TestStaggeredAccountQuotaReleaseSpreadsCohort(t *testing.T) {
	resetAt := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seen := map[time.Time]bool{}
	for accountID := int64(1); accountID <= 128; accountID++ {
		first := staggeredAccountQuotaRelease(accountID, resetAt)
		second := staggeredAccountQuotaRelease(accountID, resetAt)
		if !first.Equal(second) {
			t.Fatalf("account %d release is unstable: %s != %s", accountID, first, second)
		}
		if delay := first.Sub(resetAt); delay < accountQuotaReleaseDelayMin || delay > accountQuotaReleaseDelayMax {
			t.Fatalf("account %d delay = %s", accountID, delay)
		}
		seen[first] = true
	}
	if len(seen) < 64 {
		t.Fatalf("only %d distinct release instants for 128 accounts", len(seen))
	}
}

func TestLegacyQuotaReleaseDeadlinesGainRequiredStagger(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	now := time.Now().UTC()
	fiveHour := createGatewayTestAccount(t, a, handler, "legacy-five-hour", "https://example.test", 0, nil, map[string]any{"access_token": "token-5h"})
	sevenDay := createGatewayTestAccount(t, a, handler, "legacy-seven-day", "https://example.test", 0, nil, map[string]any{"access_token": "token-7d"})
	fiveHourReset := now.Add(2 * time.Hour).Truncate(time.Second)
	sevenDayReset := now.Add(4 * 24 * time.Hour).Truncate(time.Second)
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reason = '429_cooling', rate_limit_window = '5h', quota_5h_reset_at = ?, rate_limit_reset_at = ?, rate_limit_downweight_until = ? WHERE id = ?`,
		fiveHourReset.Format(time.RFC3339Nano), fiveHourReset.Format(time.RFC3339Nano), fiveHourReset.Format(time.RFC3339Nano), fiveHour.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reason = '429_cooling', rate_limit_window = '7d', quota_7d_reset_at = ?, rate_limit_reset_at = ?, rate_limit_downweight_until = ? WHERE id = ?`,
		sevenDayReset.Format(time.RFC3339Nano), sevenDayReset.Format(time.RFC3339Nano), sevenDayReset.Format(time.RFC3339Nano), sevenDay.ID); err != nil {
		t.Fatal(err)
	}

	a.enforceQuotaReleaseStagger()

	for _, expectation := range []struct {
		name      string
		accountID int64
		window    string
		resetAt   time.Time
	}{
		{"5h", fiveHour.ID, "5h", fiveHourReset},
		{"7d", sevenDay.ID, "7d", sevenDayReset},
	} {
		var reason, window, resetAt, downweightUntil string
		if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_window, rate_limit_reset_at, rate_limit_downweight_until FROM accounts WHERE id = ?`, expectation.accountID).
			Scan(&reason, &window, &resetAt, &downweightUntil); err != nil {
			t.Fatal(err)
		}
		want := staggeredAccountQuotaRelease(expectation.accountID, expectation.resetAt)
		got, ok := parseQuotaResetTime(resetAt)
		if !ok || !got.Equal(want) {
			t.Fatalf("%s repaired release = %q, want %s", expectation.name, resetAt, want)
		}
		gotDownweight, ok := parseQuotaResetTime(downweightUntil)
		if reason != "quota_exhausted" || window != expectation.window || !ok || !gotDownweight.Equal(want) {
			t.Fatalf("%s repaired state = reason %q window %q downweight %q", expectation.name, reason, window, downweightUntil)
		}
	}
}

func TestFiveHourQuotaRefreshWaitsForStaggeredRelease(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "staggered-release", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	quotaReset := now.Add(-time.Minute).Truncate(time.Second)
	eligibleAt := now.Add(20 * time.Minute).Truncate(time.Second)
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reason = 'quota_exhausted', rate_limit_window = '5h', quota_5h_reset_at = ?, rate_limit_reset_at = ?, rate_limit_downweight_until = ? WHERE id = ?`,
		quotaReset.Format(time.RFC3339Nano), eligibleAt.Format(time.RFC3339Nano), eligibleAt.Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}

	a.sweepAccountRateLimitState()
	var reason string
	if err := a.db.QueryRow(`SELECT rate_limit_reason FROM accounts WHERE id = ?`, created.ID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != "quota_exhausted" {
		t.Fatalf("account released at quota reset instead of staggered instant: reason=%q", reason)
	}
	if _, err := a.acquireGatewayAccount(key, "", "claude-fable-5", map[int64]bool{}); err == nil {
		t.Fatal("account was dispatchable during its post-refresh stagger window")
	}

	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, rate_limit_downweight_until = ? WHERE id = ?`,
		now.Add(-time.Second).Format(time.RFC3339Nano), now.Add(-time.Second).Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}
	a.sweepAccountRateLimitState()
	selected, err := a.acquireGatewayAccount(key, "", "claude-fable-5", map[int64]bool{})
	if err != nil {
		t.Fatalf("account was not dispatchable after staggered release: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
	var refreshedAt string
	if err := a.db.QueryRow(`SELECT quota_refreshed_at FROM accounts WHERE id = ?`, created.ID).Scan(&refreshedAt); err != nil {
		t.Fatal(err)
	}
	if refreshedAt != quotaReset.Format(time.RFC3339Nano) {
		t.Fatalf("quota refresh timestamp = %s, want actual reset %s", refreshedAt, quotaReset)
	}
}

func TestGatewayConsecutiveTransient429UsesConfiguredShortCooling(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "three-strikes", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	fiveHourReset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(fiveHourReset.Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.91")
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: headers}

	for attempt := 1; attempt <= defaultRateLimitCoolingThreshold; attempt++ {
		a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10, defaultTestRateLimitPolicy(), response)
		var consecutive, schedulable int
		var reason, window string
		var resetAt sql.NullString
		if err := a.db.QueryRow(`SELECT consecutive_429, schedulable, rate_limit_reason, rate_limit_window, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).
			Scan(&consecutive, &schedulable, &reason, &window, &resetAt); err != nil {
			t.Fatal(err)
		}
		if consecutive != attempt || schedulable != 1 {
			t.Fatalf("attempt %d state = consecutive %d, schedulable %d", attempt, consecutive, schedulable)
		}
		if reason != "429_cooling" || window != "" || !resetAt.Valid {
			t.Fatalf("attempt %d reason/window = %q/%q", attempt, reason, window)
		}
		parsed, err := time.Parse(time.RFC3339Nano, resetAt.String)
		if err != nil {
			t.Fatal(err)
		}
		wantCooldown := 60 + (attempt-1)*30
		if remaining := time.Until(parsed); remaining < time.Duration(wantCooldown-1)*time.Second || remaining > time.Duration(wantCooldown+1)*time.Second {
			t.Fatalf("attempt %d short cooldown remaining=%s, want about %ds", attempt, remaining, wantCooldown)
		}
		if delta := parsed.Sub(fiveHourReset); delta > -30*time.Minute && delta < 30*time.Minute {
			t.Fatalf("ambiguous 429 inherited the 5h quota reset: %s", parsed)
		}
		if attempt < defaultRateLimitCoolingThreshold {
			// Model a later request after the short cooldown. It returns to
			// downweighted scheduling, then the debounce window ages out before the
			// next independent strike.
			if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, last_429_at = ? WHERE id = ?`,
				time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano),
				time.Now().UTC().Add(-2*account429DebounceWindow).Format(time.RFC3339Nano), created.ID); err != nil {
				t.Fatal(err)
			}
			a.sweepAccountRateLimitState()
		}
	}
}

func TestGatewayConcurrent429BurstCountsOnce(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "burst", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}

	// Upstream rate limiting rejects every in-flight request at once; that is
	// one event, not enough strikes to park the account.
	var wait sync.WaitGroup
	for range defaultRateLimitCoolingThreshold + 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			a.captureAccount429State(created.ID, defaultTestRateLimitPolicy(), response)
		}()
	}
	wait.Wait()

	var consecutive int
	var reason string
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).
		Scan(&consecutive, &reason, &resetAt); err != nil {
		t.Fatal(err)
	}
	if consecutive != 1 || reason != "429_cooling" || !resetAt.Valid {
		t.Fatalf("burst state = consecutive %d, reason %q, reset %v; want one short cooldown", consecutive, reason, resetAt)
	}
}

// An ambiguous 429 must not inherit the account's stored 5h reset.
func TestGatewayFirst429UsesShortCooldownInsteadOfQuotaWindow(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "first-strike", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	windowReset := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Second)
	if _, err := a.db.Exec(`UPDATE accounts SET quota_5h_reset_at = ? WHERE id = ?`, windowReset.Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}

	a.captureAccount429State(created.ID, defaultTestRateLimitPolicy(), &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)})

	var consecutive int
	var reason string
	var downweightUntil, resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason, rate_limit_downweight_until, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).
		Scan(&consecutive, &reason, &downweightUntil, &resetAt); err != nil {
		t.Fatal(err)
	}
	if consecutive != 1 || reason != "429_cooling" || !downweightUntil.Valid {
		t.Fatalf("first 429 state = consecutive %d, reason %q, downweight %v", consecutive, reason, downweightUntil)
	}
	parsed, err := time.Parse(time.RFC3339Nano, resetAt.String)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(parsed); remaining < 59*time.Second || remaining > 61*time.Second {
		t.Fatalf("short cooldown remaining = %s, want about 60s", remaining)
	}
	parsedDownweight, err := time.Parse(time.RFC3339Nano, downweightUntil.String)
	if err != nil {
		t.Fatal(err)
	}
	if delta := parsedDownweight.Sub(windowReset); delta < -time.Second || delta > time.Second {
		t.Fatalf("downweight reset = %s, want stored 5h reset %s", parsedDownweight, windowReset)
	}
}

// Successes inside the same window must not lift the penalty — the account has
// already shown it reaches its ceiling there.
func TestGatewaySuccessDoesNotLiftDownweight(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer healthy.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "still-penalised", healthy.URL, 0, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 1, rate_limit_reason = '429_backoff', rate_limit_downweight_until = ? WHERE id = ?`,
		time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	var consecutive int
	var reason string
	var downweightUntil sql.NullString
	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason, rate_limit_downweight_until FROM accounts WHERE id = ?`, created.ID).
		Scan(&consecutive, &reason, &downweightUntil); err != nil {
		t.Fatal(err)
	}
	if consecutive != 1 || reason != "429_backoff" || !downweightUntil.Valid {
		t.Fatalf("a success lifted the window penalty: consecutive %d, reason %q, downweight %v", consecutive, reason, downweightUntil)
	}
}

func TestGatewayLateTransient429CannotExtendExistingCooling(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "parked", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	parkedUntil := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 3, rate_limit_reason = '429_cooling', rate_limit_reset_at = ?, error_message = '已冷却' WHERE id = ?`,
		parkedUntil, created.ID); err != nil {
		t.Fatal(err)
	}

	// A request that was already in flight when the park landed must not
	// rewrite the reason, or the sweeper would never release the row.
	a.captureAccount429State(created.ID, defaultTestRateLimitPolicy(), &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)})

	var reason, message string
	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reason, error_message, rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).
		Scan(&reason, &message, &resetAt); err != nil {
		t.Fatal(err)
	}
	if reason != "429_cooling" || message != "已冷却" || !resetAt.Valid {
		t.Fatalf("late strike state: reason %q, message %q, reset %v", reason, message, resetAt)
	}
	parsed, err := time.Parse(time.RFC3339Nano, resetAt.String)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := time.Parse(time.RFC3339Nano, parkedUntil)
	if err != nil {
		t.Fatal(err)
	}
	if delta := parsed.Sub(expected); delta < -time.Millisecond || delta > time.Millisecond {
		t.Fatalf("late strike moved cooldown from %s to %s", expected, parsed)
	}
}

// Manual recovery is one of only two ways out of the penalty, and it grants
// the fresh-window boost because the operator is asserting the account is good.
func TestManualClearReleasesDownweightAndBoosts(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "manual-clear", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 3, last_429_at = ?, rate_limit_reason = '429_cooling', rate_limit_window = '5h', rate_limit_reset_at = ?, rate_limit_downweight_until = ?, error_message = '上游 429' WHERE id = ?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_rpm_thresholds (account_id, rpm_limit, reset_at) VALUES (?, 7, ?)`,
		created.ID, time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	a.clearAccount429State(created.ID)

	var consecutive int
	var reason, window, message string
	var last429, resetAt, downweight, refreshed sql.NullString
	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason, rate_limit_window, error_message, last_429_at, rate_limit_reset_at, rate_limit_downweight_until, quota_refreshed_at FROM accounts WHERE id = ?`, created.ID).
		Scan(&consecutive, &reason, &window, &message, &last429, &resetAt, &downweight, &refreshed); err != nil {
		t.Fatal(err)
	}
	if consecutive != 0 || reason != "" || window != "" || message != "" || last429.Valid || resetAt.Valid || downweight.Valid {
		t.Fatalf("manual clear kept state: consecutive=%d reason=%q window=%q message=%q last=%v reset=%v downweight=%v", consecutive, reason, window, message, last429, resetAt, downweight)
	}
	if !refreshed.Valid {
		t.Fatal("manual clear did not grant the fresh-window priority boost")
	}
	var thresholdCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_thresholds WHERE account_id = ?`, created.ID).Scan(&thresholdCount); err != nil {
		t.Fatal(err)
	}
	if thresholdCount != 0 {
		t.Fatalf("manual clear kept %d learned RPM thresholds", thresholdCount)
	}
}

func TestAccountRateLimitResetEndpoint(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "manual-reset-endpoint", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 1, last_429_at = ?, rate_limit_reason = '429_backoff', rate_limit_downweight_until = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339Nano), future, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_rpm_thresholds (account_id, rpm_limit, reset_at) VALUES (?, 7, ?)`, created.ID, future); err != nil {
		t.Fatal(err)
	}

	var response struct {
		Reset bool `json:"reset"`
	}
	putJSON(t, handler, http.MethodPost, fmt.Sprintf("/api/accounts/%d/rate-limit/reset", created.ID), map[string]any{}, http.StatusOK, &response)
	if !response.Reset {
		t.Fatal("rate-limit reset endpoint reported no state change")
	}
	var strikes, thresholds int
	var reason string
	var downweight sql.NullString
	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason, rate_limit_downweight_until FROM accounts WHERE id = ?`, created.ID).Scan(&strikes, &reason, &downweight); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_thresholds WHERE account_id = ?`, created.ID).Scan(&thresholds); err != nil {
		t.Fatal(err)
	}
	if strikes != 0 || reason != "" || downweight.Valid || thresholds != 0 {
		t.Fatalf("rate-limit state remained: strikes=%d reason=%q downweight=%v thresholds=%d", strikes, reason, downweight, thresholds)
	}
	var action string
	if err := a.db.QueryRow(`SELECT action FROM audit_logs WHERE path = ? ORDER BY id DESC LIMIT 1`, fmt.Sprintf("/api/accounts/%d/rate-limit/reset", created.ID)).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != "account.rate_limit_reset" {
		t.Fatalf("audit action = %q, want account.rate_limit_reset", action)
	}

	putJSON(t, handler, http.MethodPost, fmt.Sprintf("/api/accounts/%d/rate-limit/reset", created.ID), map[string]any{}, http.StatusOK, &response)
	if response.Reset {
		t.Fatal("second rate-limit reset should be a no-op")
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/999999/rate-limit/reset", map[string]any{}, http.StatusNotFound, nil)
}

func TestManualClearKeepsCooldownsItDoesNotOwn(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	idle := createGatewayTestAccount(t, a, handler, "idle-cooldown", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	future := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	// A backoff strike never owns rate_limit_reset_at; here it belongs to the
	// stream-idle guard, so clearing the 429 penalty must leave it alone.
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 2, rate_limit_reason = '429_backoff', rate_limit_reset_at = ? WHERE id = ?`, future, idle.ID); err != nil {
		t.Fatal(err)
	}

	a.clearAccount429State(idle.ID)

	var idleReset sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, idle.ID).Scan(&idleReset); err != nil {
		t.Fatal(err)
	}
	if !idleReset.Valid {
		t.Fatal("clear removed a cooldown owned by the stream-idle guard")
	}
}

func TestAccountRateLimitSweeperReleasesOnQuotaWindowReset(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	expired := createGatewayTestAccount(t, a, handler, "expired-park", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	live := createGatewayTestAccount(t, a, handler, "live-park", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	rolled := createGatewayTestAccount(t, a, handler, "window-rolled", "https://third.example.test", 0, nil, map[string]any{"access_token": "token-c"})
	penalised := createGatewayTestAccount(t, a, handler, "still-penalised", "https://fourth.example.test", 0, nil, map[string]any{"access_token": "token-d"})
	past := time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano)
	future := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	// Park expired but the window has not rolled: schedulable again, still last.
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 2, rate_limit_reason = '429_cooling', rate_limit_reset_at = ?, rate_limit_downweight_until = ? WHERE id = ?`, past, future, expired.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 3, rate_limit_reason = '429_cooling', rate_limit_reset_at = ?, rate_limit_downweight_until = ? WHERE id = ?`, future, future, live.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 2, rate_limit_reason = '429_backoff', rate_limit_downweight_until = ? WHERE id = ?`, past, rolled.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 2, rate_limit_reason = '429_backoff', rate_limit_downweight_until = ? WHERE id = ?`, future, penalised.ID); err != nil {
		t.Fatal(err)
	}

	a.sweepAccountRateLimitState()

	var refreshed sql.NullString
	if err := a.db.QueryRow(`SELECT quota_refreshed_at FROM accounts WHERE id = ?`, rolled.ID).Scan(&refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Valid {
		t.Fatal("a legacy transient penalty incorrectly granted a quota refresh boost")
	}
	for _, expectation := range []struct {
		name            string
		id              int64
		wantConsecutive int
		wantReason      string
	}{
		{"expired transient cooldown", expired.ID, 2, "429_backoff"},
		{"live park", live.ID, 3, "429_cooling"},
		{"rolled window", rolled.ID, 0, ""},
		{"still penalised", penalised.ID, 2, "429_backoff"},
	} {
		var consecutive int
		var reason string
		if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason FROM accounts WHERE id = ?`, expectation.id).Scan(&consecutive, &reason); err != nil {
			t.Fatal(err)
		}
		if consecutive != expectation.wantConsecutive || reason != expectation.wantReason {
			t.Fatalf("%s = consecutive %d reason %q, want %d %q", expectation.name, consecutive, reason, expectation.wantConsecutive, expectation.wantReason)
		}
	}
}

func TestGatewayExpired429CoolingAutoRecoversBeforeSelection(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	created := createGatewayTestAccount(t, a, handler, "expired-cooling", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 3, last_429_at = ?, rate_limit_reason = '429_cooling', rate_limit_window = '5h', rate_limit_reset_at = ?, error_message = '连续 3 次上游 429' WHERE id = ?`,
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}

	// Selection must recover the account on its own. The sweeper only tidies
	// display state, so dispatch cannot depend on having run it first.
	selected, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	if selected.ID != created.ID {
		t.Fatalf("selected account = %d, want recovered %d", selected.ID, created.ID)
	}
}

// A stream opens with a 200 header and only then carries a rate_limit_error.
// Resetting on the header status would zero the counter immediately before the
// in-stream error increments it, so strikes could never accumulate.
func TestGatewayStreamRateLimitErrorAccumulatesStrikes(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"limited\"}}\n\n")
	}))
	defer limited.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "stream-limited", limited.URL, 0, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token"})

	// Seed an earlier strike that has aged past the debounce window. If the
	// 200 header still triggered a reset, this would be wiped and the
	// in-stream error would leave the count at 1 instead of 2.
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 1, rate_limit_reason = '429_backoff', last_429_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-2*account429DebounceWindow).Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	var consecutive int
	var reason string
	if err := a.db.QueryRow(`SELECT consecutive_429, rate_limit_reason FROM accounts WHERE id = ?`, created.ID).Scan(&consecutive, &reason); err != nil {
		t.Fatal(err)
	}
	if consecutive != 2 || reason != "429_cooling" {
		t.Fatalf("in-stream rate limit state = consecutive %d, reason %q; want the earlier strike kept and a short cooldown", consecutive, reason)
	}
}

// A freshly refreshed account outranks a plain one, which in turn outranks one
// still carrying this window's penalty.
func TestGatewaySchedulerPrefersFreshlyRefreshedAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	// Equal priority throughout: the weight tier is only a tie-break, an
	// explicit priority still wins outright.
	penalised := createGatewayTestAccount(t, a, handler, "penalised", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "plain", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	refreshed := createGatewayTestAccount(t, a, handler, "refreshed", "https://third.example.test", 0, nil, map[string]any{"access_token": "token-c"})
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 1, rate_limit_reason = '429_backoff', rate_limit_downweight_until = ? WHERE id = ?`,
		time.Now().UTC().Add(2*time.Hour).Format(time.RFC3339Nano), penalised.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET quota_refreshed_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), refreshed.ID); err != nil {
		t.Fatal(err)
	}

	selected, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	if selected.ID != refreshed.ID {
		t.Fatalf("selected account = %d, want the freshly refreshed one %d", selected.ID, refreshed.ID)
	}

	// With the boost aged out it falls back to the plain account, and the
	// penalised one still sorts last.
	if _, err := a.db.Exec(`UPDATE accounts SET quota_refreshed_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-2*accountQuotaFreshPriorityWindow).Format(time.RFC3339Nano), refreshed.ID); err != nil {
		t.Fatal(err)
	}
	next, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(next.ID)
	if next.ID == penalised.ID {
		t.Fatalf("selected the penalised account %d while unpenalised ones were available", penalised.ID)
	}
}

func TestGatewaySchedulerPrefersAccountWithout429Strikes(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	penalized := createGatewayTestAccount(t, a, handler, "penalized", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	healthy := createGatewayTestAccount(t, a, handler, "healthy", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	if _, err := a.db.Exec(`UPDATE accounts SET consecutive_429 = 1, rate_limit_reason = '429_backoff', rate_limit_reset_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-time.Second).Format(time.RFC3339Nano), penalized.ID); err != nil {
		t.Fatal(err)
	}

	selected, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	if selected.ID != healthy.ID {
		t.Fatalf("selected account = %d, want healthy account %d", selected.ID, healthy.ID)
	}
}

func TestGatewayStickyBindingMatchesSub2EagerSelection(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	first := createGatewayTestAccount(t, a, handler, "sticky-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "sticky-second", "https://second.example.test", 1, nil, map[string]any{"access_token": "token-b"})
	const session = "stable-session"
	a.bindGatewayStickySession(key.ID, session, first.ID)

	selected, err := a.acquireGatewayAccount(key, session, "claude-test", map[int64]bool{first.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(selected.ID)
	if selected.ID != second.ID {
		t.Fatalf("selected account=%d, want fallback %d", selected.ID, second.ID)
	}
	if bound := a.gatewayStickyAccountID(key.ID, session); bound != second.ID {
		t.Fatalf("sticky account=%d, want eagerly selected fallback %d", bound, second.ID)
	}
}

func TestGatewayUsesSub2RPMThreeZoneDecision(t *testing.T) {
	account := gatewayAccount{
		AuthType: "oauth", BaseRPM: 10, Concurrency: 3, RPMStrategy: "tiered",
		ExtraJSON: `{"max_sessions":10}`,
	}
	if !rpmSchedulable(account, 15, true) {
		t.Fatal("sticky request should remain schedulable in concurrency + max_sessions buffer")
	}
	if rpmSchedulable(account, 10, false) {
		t.Fatal("non-sticky request should not enter the RPM buffer")
	}
	if rpmSchedulable(account, 23, true) {
		t.Fatal("request should be blocked at the end of the RPM buffer")
	}
	account.AuthType = "apikey"
	if !rpmSchedulable(account, 999, false) {
		t.Fatal("Sub2API does not apply account RPM to Anthropic API-key accounts")
	}
}

func TestGatewayFixedRPMStrategyIsAHardCapWithLiteralStickyExemption(t *testing.T) {
	account := gatewayAccount{
		AuthType: "oauth", BaseRPM: 10, Concurrency: 5, RPMStrategy: "fixed",
		StickyBuffer: 3, ExtraJSON: `{"max_sessions":10}`,
	}
	if !rpmSchedulable(account, 9, false) {
		t.Fatal("below base_rpm every request should be schedulable")
	}
	if rpmSchedulable(account, 10, false) {
		t.Fatal("non-sticky request must be rejected exactly at base_rpm")
	}
	if !rpmSchedulable(account, 12, true) {
		t.Fatal("sticky request should pass inside the literal exemption n")
	}
	if rpmSchedulable(account, 13, true) {
		t.Fatal("sticky request must be rejected at base_rpm + n")
	}
	account.StickyBuffer = 0
	if rpmSchedulable(account, 10, true) {
		t.Fatal("n=0 means no sticky exemption at all; concurrency/max_sessions must not auto-expand it")
	}
}

func TestGatewayCapacityQueueWaitsForAFreeAccountInsteadOfRejecting(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req-capacity-queue")
		_, _ = w.Write([]byte(`{"id":"msg_queue","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "queue-account", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 10)`, created.ID); err != nil {
		t.Fatal(err)
	}

	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
		r.Header.Set("Authorization", "Bearer "+key.Key)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}

	// Queue disabled: a saturated group rejects immediately.
	if response := request(); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("saturated group without queue status=%d body=%s", response.Code, response.Body.String())
	}

	if _, err := a.db.Exec(`UPDATE groups SET capacity_queue_enabled = 1, capacity_queue_timeout_seconds = 5 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		_, _ = a.db.Exec(`UPDATE account_inflight SET requests = 0 WHERE account_id = ?`, created.ID)
	}()
	started := time.Now()
	response := request()
	if response.Code != http.StatusOK {
		t.Fatalf("queued request status=%d body=%s", response.Code, response.Body.String())
	}
	if waited := time.Since(started); waited < 500*time.Millisecond {
		t.Fatalf("request should have waited in the capacity queue, returned after %s", waited)
	}
}

func TestGatewayCapacityQueueTimesOutWhenNoAccountFreesUp(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "stuck-account", "https://unused.example.test", 0, nil, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 10)`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE groups SET capacity_queue_enabled = 1, capacity_queue_timeout_seconds = 1 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
	r.Header.Set("Authorization", "Bearer "+key.Key)
	w := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "capacity queue") {
		t.Fatalf("queue timeout status=%d body=%s", w.Code, w.Body.String())
	}
	if waited := time.Since(started); waited < time.Second {
		t.Fatalf("timeout should honor the configured wait, returned after %s", waited)
	}
}

func TestGatewayDeductsUserBalanceAndBlocksTheNextRequest(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "balance-"+strconv.Itoa(upstreamCalls))
		_, _ = w.Write([]byte(`{"id":"msg_balance","usage":{"input_tokens":100,"output_tokens":100}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	if _, err := a.db.Exec(`UPDATE users SET balance = 0.0001 WHERE id = ?`, key.UserID); err != nil {
		t.Fatal(err)
	}
	createGatewayTestAccount(t, a, handler, "balance", upstream.URL, 0, nil, map[string]any{"access_token": "token"})

	request := func(expectedStatus int) *httptest.ResponseRecorder {
		t.Helper()
		body := strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
		r.Header.Set("Authorization", "Bearer "+key.Key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		if response.Code != expectedStatus {
			t.Fatalf("gateway status=%d want=%d body=%s", response.Code, expectedStatus, response.Body.String())
		}
		return response
	}

	request(http.StatusOK)
	var balance, quotaUsed, billedCost float64
	if err := a.db.QueryRow(`SELECT balance FROM users WHERE id = ?`, key.UserID).Scan(&balance); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT quota_used FROM api_keys WHERE id = ?`, key.ID).Scan(&quotaUsed); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT billed_cost FROM usage_logs WHERE request_id = 'balance-1'`).Scan(&billedCost); err != nil {
		t.Fatal(err)
	}
	if billedCost <= 0 || balance >= 0 || quotaUsed != billedCost || balance != money(0.0001-billedCost) {
		t.Fatalf("balance=%f quota_used=%f billed=%f", balance, quotaUsed, billedCost)
	}
	response := request(http.StatusPaymentRequired)
	if upstreamCalls != 1 || !strings.Contains(response.Body.String(), "billing_error") {
		t.Fatalf("exhausted balance reached upstream=%d body=%s", upstreamCalls, response.Body.String())
	}
}

func TestGatewayQuotaAndBudgetChecksDoNotSerializeUpstream(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var requestNumber atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		arrived <- struct{}{}
		<-release
		id := requestNumber.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "concurrent-"+strconv.FormatInt(id, 10))
		_, _ = w.Write([]byte(`{"id":"msg_concurrent","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	if _, err := a.db.Exec(`UPDATE api_keys SET quota = 100 WHERE id = ?`, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE groups SET daily_limit_usd = 100 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	createGatewayTestAccount(t, a, handler, "concurrent", upstream.URL, 0, nil, map[string]any{"access_token": "token"})

	var wait sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			body := strings.NewReader(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			responses <- response
		}()
	}

	for index := 0; index < 2; index++ {
		select {
		case <-arrived:
		case <-time.After(time.Second):
			unblock()
			wait.Wait()
			t.Fatalf("request %d did not reach upstream concurrently", index+1)
		}
	}
	unblock()
	wait.Wait()
	close(responses)
	for response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestUserRPMStorageFailureFailsOpenLikeSub2(t *testing.T) {
	a, _ := newGatewayTestApp(t)
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.checkAndIncrementUserRPM(gatewayKey{UserID: 1, UserRPM: 1}); err != nil {
		t.Fatalf("storage failure must fail open, got %v", err)
	}
}

func TestSerialUserMessageQueueAndToolResultBypass(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, _ := newGatewayTestApp(t)
	defer a.db.Close()
	account := gatewayAccount{ID: 99, AuthType: "oauth", BaseRPM: 15, UserMsgQueueMode: "serial"}
	userBody := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	firstRelease, err := a.acquireUserMessageQueue(context.Background(), account, userBody, false)
	if err != nil {
		t.Fatal(err)
	}
	secondReady := make(chan func(), 1)
	go func() {
		release, _ := a.acquireUserMessageQueue(context.Background(), account, userBody, false)
		secondReady <- release
	}()
	select {
	case <-secondReady:
		t.Fatal("serial queue admitted a second user message before release")
	case <-time.After(50 * time.Millisecond):
	}
	toolBody := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"ok"}]}]}`)
	toolRelease, err := a.acquireUserMessageQueue(context.Background(), account, toolBody, false)
	if err != nil {
		t.Fatal(err)
	}
	toolRelease()
	firstRelease()
	select {
	case secondRelease := <-secondReady:
		secondRelease()
	case <-time.After(time.Second):
		t.Fatal("serial queue did not admit the next user message after release")
	}
}

// Mirrors the shipped group defaults so a test that is not about the switch
// exercises the same policy production runs with.
func defaultTestRateLimitPolicy() rateLimitPolicy {
	return rateLimitPolicy{
		DownweightEnabled: true,
		CoolingThreshold:  defaultRateLimitCoolingThreshold,
		CooldownSeconds:   defaultRateLimitCooldownSeconds,
	}
}

func newGatewayTestApp(t *testing.T) (*app, http.Handler) {
	t.Helper()
	a, err := newApp(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	return a, a.routes()
}

func createGatewayTestKey(t *testing.T, handler http.Handler) apiKeyRecord {
	t.Helper()
	var user panelUser
	putJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "gateway-user", "name": "Gateway User", "password": "gateway-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 100,
	}, http.StatusCreated, &user)
	var key apiKeyRecord
	putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{"user_id": user.ID, "name": "gateway-key", "group_id": "a", "status": "active", "quota": 0}, http.StatusCreated, &key)
	return key
}

func createGatewayTestAccount(t *testing.T, a *app, handler http.Handler, name, upstream string, priority int, extra, credentials map[string]any) account {
	t.Helper()
	if extra == nil {
		extra = map[string]any{}
	}
	extra["custom_forward_url"] = upstream
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": name, "platform": "anthropic", "auth_type": "oauth", "credentials": credentials, "extra": extra,
		"status": "active", "schedulable": true, "concurrency": 10, "priority": priority, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID,
		"base_rpm": 100, "rpm_strategy": "tiered", "rpm_sticky_buffer": 0, "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	return created
}

func createTestForwardProxy(t *testing.T, a *app) int64 {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		buffer := make([]byte, 32*1024)
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				_, _ = w.Write(buffer[:read])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
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
	result, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, status) VALUES (1, ?, 'http', ?, ?, 'active')`, "test-proxy-"+portText, host, port)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
