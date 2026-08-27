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

	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
)

// A warm session's ITPM demand follows its last settled uncached input, so an
// account with plenty of hard-limit headroom for the true cost keeps the
// session even when the raw body estimate would overflow the limit.
func TestStickySessionITPMDiscountKeepsWarmAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "sticky-itpm-discount", "rpm_limit": 8, "rpm_strategy": "fixed", "dispatch_mode": "balance",
		"itpm_protection_enabled": true,
		"itpm_window_seconds":     60,
		"itpm_soft_limit":         100_000,
		"itpm_hard_limit":         150_000,
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	home := createGatewayTestAccount(t, a, handler, "warm-home", "https://warm.example.test", 0, nil, map[string]any{"access_token": "warm"})
	spare := createGatewayTestAccount(t, a, handler, "cold-spare", "https://spare.example.test", 0, nil, map[string]any{"access_token": "spare"})
	if _, _, err := a.recordUsage(usageInput{
		RequestID: "warm-home-load", PurposeKey: "default", GroupID: "a", AccountID: home.ID,
		Model: "claude-fable-5", InputTokens: 120_000,
	}); err != nil {
		t.Fatal(err)
	}
	session := "sticky-itpm-discount-session"
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO dispatch_sessions (session_hash, api_key_id, account_id, expires_at, last_input_tokens) VALUES (?, ?, ?, ?, ?)`, session, key.ID, home.ID, expires, 500); err != nil {
		t.Fatal(err)
	}

	// Body estimate of 40k projected onto 120k current usage crosses the 150k
	// hard limit, but the session's settled cost (500 → discounted 1000) fits.
	demand := gatewayDispatchDemand{EstimatedITPM: 40_000}
	selected, err := a.tryAcquireGatewayAccountPinned(key, session, "claude-fable-5", map[int64]bool{}, false, false, demand)
	if err != nil {
		t.Fatalf("discounted sticky dispatch failed: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
	a.releaseGatewayITPM(selected)
	if selected.ID != home.ID {
		t.Fatalf("selected account %d, want warm home %d kept by the settled-cost discount", selected.ID, home.ID)
	}

	// Without a settled cost the full estimate applies again and the projection
	// over the hard limit moves the session away.
	if _, err := a.db.Exec(`UPDATE dispatch_sessions SET last_input_tokens = 0, account_id = ? WHERE session_hash = ? AND api_key_id = ?`, home.ID, session, key.ID); err != nil {
		t.Fatal(err)
	}
	selected, err = a.tryAcquireGatewayAccountPinned(key, session, "claude-fable-5", map[int64]bool{}, false, false, demand)
	if err != nil {
		t.Fatalf("undiscounted dispatch failed: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
	a.releaseGatewayITPM(selected)
	if selected.ID != spare.ID {
		t.Fatalf("selected account %d, want spare %d once the projection overflows the hard limit", selected.ID, spare.ID)
	}
}

// A pinned warm session whose account has no ITPM budget waits in the queue
// first; when the window does not slide in time, the request rebuilds its cache
// on the account with the most budget and that account becomes the session's
// new sticky home. The old binding is never restored.
func TestPinnedStickyITPMTimeoutRebindsNewAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	t.Setenv("CCMAX_ITPM_STICKY_QUEUE_TIMEOUT", "300ms")
	var oldAccountCalls, newAccountCalls atomic.Int64
	oldUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		oldAccountCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_old\",\"type\":\"message\",\"model\":\"claude-fable-5\",\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer oldUpstream.Close()
	newUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		newAccountCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_rebuilt\",\"type\":\"message\",\"model\":\"claude-fable-5\",\"usage\":{\"input_tokens\":1200,\"cache_creation_input_tokens\":3400,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"rebuilt\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer newUpstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "sticky-itpm-rebind", "rpm_limit": 8, "rpm_strategy": "fixed", "dispatch_mode": "balance",
		"itpm_protection_enabled": true,
		"itpm_window_seconds":     60,
		"itpm_soft_limit":         100_000,
		"itpm_hard_limit":         150_000,
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	oldHome := createGatewayTestAccount(t, a, handler, "exhausted-home", oldUpstream.URL, 0, nil, map[string]any{"access_token": "old"})
	newHome := createGatewayTestAccount(t, a, handler, "fresh-home", newUpstream.URL, 0, nil, map[string]any{"access_token": "new"})
	if _, _, err := a.recordUsage(usageInput{
		RequestID: "exhausted-home-load", PurposeKey: "default", GroupID: "a", AccountID: oldHome.ID,
		Model: "claude-fable-5", InputTokens: 200_000,
	}); err != nil {
		t.Fatal(err)
	}

	// A >512KB body makes the request hold a large cache, which is what pins it
	// to the bound account in the first place.
	payload, err := json.Marshal(map[string]any{
		"model": "claude-fable-5", "max_tokens": 32, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": strings.Repeat("缓存前缀", 180_000)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(payload)))
	request.Header.Set("Authorization", "Bearer "+createdKey.Key)
	request.Header.Set("Content-Type", "application/json")
	session := sub2service.GenerateCCMaxCompatibilitySessionHash(payload, gatewayClientIP(request), request.UserAgent(), key.ID)
	if session == "" {
		t.Fatal("test request produced an empty session hash")
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`INSERT INTO dispatch_sessions (session_hash, api_key_id, account_id, expires_at, last_input_tokens) VALUES (?, ?, ?, ?, ?)`, session, key.ID, oldHome.ID, expires, 600); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if waited := time.Since(started); waited < 250*time.Millisecond {
		t.Fatalf("request completed in %s, want it to wait for the sticky queue window first", waited)
	}
	if oldAccountCalls.Load() != 0 || newAccountCalls.Load() != 1 {
		t.Fatalf("upstream calls old=%d new=%d, want the rebuilt request on the fresh account only", oldAccountCalls.Load(), newAccountCalls.Load())
	}
	if got := response.Header().Get("X-CCMAX-Account"); got != newHome.Name {
		t.Fatalf("served by %q, want %q", got, newHome.Name)
	}

	var boundAccount, lastInput int64
	if err := a.db.QueryRow(`SELECT account_id, last_input_tokens FROM dispatch_sessions WHERE session_hash = ? AND api_key_id = ?`, session, key.ID).Scan(&boundAccount, &lastInput); err != nil {
		t.Fatal(err)
	}
	if boundAccount != newHome.ID {
		t.Fatalf("session bound to %d after rebuild, want new home %d", boundAccount, newHome.ID)
	}
	if lastInput != 1200+3400 {
		t.Fatalf("session settled cost=%d, want 4600 (input + cache_creation)", lastInput)
	}
}
