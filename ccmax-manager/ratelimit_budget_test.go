package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfterDeadlineParsesBothFormats(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name   string
		header string
		want   time.Time
		found  bool
	}{
		{"absent", "", time.Time{}, false},
		{"delta seconds", "30", now.Add(30 * time.Second), true},
		{"zero seconds", "0", now, true},
		{"http date", now.Add(90 * time.Second).Format(http.TimeFormat), now.Add(90 * time.Second), true},
		// A date already in the past means the wait is over, not that we should
		// park the account at an earlier timestamp.
		{"past http date", now.Add(-time.Hour).Format(http.TimeFormat), time.Time{}, false},
		{"negative seconds", "-5", time.Time{}, false},
		{"garbage", "soon", time.Time{}, false},
		// A pathological value must not park an account for days.
		{"absurd seconds", "999999", now.Add(maxRetryAfterWait), true},
		{"absurd date", now.Add(72 * time.Hour).Format(http.TimeFormat), now.Add(maxRetryAfterWait), true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			headers := make(http.Header)
			if testCase.header != "" {
				headers.Set("retry-after", testCase.header)
			}
			got, found := retryAfterDeadline(headers, now)
			if found != testCase.found {
				t.Fatalf("found = %v, want %v", found, testCase.found)
			}
			if found && !got.Equal(testCase.want) {
				t.Fatalf("deadline = %s, want %s", got, testCase.want)
			}
		})
	}
}

// retry-after is an upstream instruction, not our scheduling heuristic, so it
// has to survive the group's adaptive-cooling switch being off.
func TestRetryAfterAppliesEvenWhenDownweightDisabled(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "retry-after", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	headers := make(http.Header)
	headers.Set("retry-after", "45")

	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10,
		rateLimitPolicy{DownweightEnabled: false, CoolingThreshold: 3, CooldownSeconds: 120},
		&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var resetAt sql.NullString
	var reason string
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at, rate_limit_reason FROM accounts WHERE id = ?`, created.ID).Scan(&resetAt, &reason); err != nil {
		t.Fatal(err)
	}
	if !resetAt.Valid {
		t.Fatal("upstream retry-after was ignored while the downweight switch was off")
	}
	// The switch still governs our own bookkeeping — only the wait is honoured.
	if reason != "" {
		t.Fatalf("disabled switch still recorded a rate-limit reason %q", reason)
	}
	parsed, err := time.Parse(time.RFC3339Nano, resetAt.String)
	if err != nil {
		t.Fatal(err)
	}
	if wait := time.Until(parsed); wait < 30*time.Second || wait > 45*time.Second {
		t.Fatalf("cooldown wait = %s, want roughly the 45s the upstream asked for", wait)
	}
}

// A longer cooldown already in force must not be shortened by a small
// retry-after from a later response.
func TestRetryAfterNeverShortensAnExistingCooldown(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "long-cooldown", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	parked := time.Now().UTC().Add(2 * time.Hour)
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ? WHERE id = ?`, parked.Format(time.RFC3339Nano), created.ID); err != nil {
		t.Fatal(err)
	}
	headers := make(http.Header)
	headers.Set("retry-after", "5")

	// Downweight off so only the retry-after path runs — adaptive cooling has
	// its own cooldown and would otherwise be the thing under test.
	a.captureGatewayUpstreamState(created.ID, "a", "claude-fable-5", 10,
		rateLimitPolicy{DownweightEnabled: false, CoolingThreshold: 3, CooldownSeconds: 120},
		&http.Response{StatusCode: http.StatusTooManyRequests, Header: headers})

	var resetAt sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reset_at FROM accounts WHERE id = ?`, created.ID).Scan(&resetAt); err != nil {
		t.Fatal(err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, resetAt.String)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(parsed) < time.Hour {
		t.Fatalf("a 5s retry-after shortened a 2h cooldown to %s", time.Until(parsed))
	}
}

// The upstream reports remaining ITPM on every response, which is what lets the
// dispatcher stop guessing a request's token cost before sending it.
func TestCaptureAccountRateLimitBudgetRecordsRemaining(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "budget", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	reset := time.Now().UTC().Add(30 * time.Second).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-input-tokens-remaining", "184000")
	headers.Set("anthropic-ratelimit-input-tokens-reset", reset.Format(time.RFC3339))

	a.captureAccountUpstreamState(created.ID, &http.Response{StatusCode: http.StatusOK, Header: headers})

	var remaining sql.NullInt64
	var resetAt, sampledAt sql.NullString
	if err := a.db.QueryRow(`SELECT itpm_remaining, itpm_reset_at, itpm_sampled_at FROM accounts WHERE id = ?`, created.ID).
		Scan(&remaining, &resetAt, &sampledAt); err != nil {
		t.Fatal(err)
	}
	if !remaining.Valid || remaining.Int64 != 184000 || !resetAt.Valid || !sampledAt.Valid {
		t.Fatalf("budget = %v, reset %v, sampled %v", remaining, resetAt, sampledAt)
	}
	parsed, err := time.Parse(time.RFC3339Nano, resetAt.String)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Equal(reset) {
		t.Fatalf("reset = %s, want %s", parsed, reset)
	}
}

func TestCaptureAccountRateLimitBudgetIgnoresJunk(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "junk-budget", "https://example.test", 0, nil, map[string]any{"access_token": "token"})

	for _, value := range []string{"", "not-a-number", "-1"} {
		headers := make(http.Header)
		if value != "" {
			headers.Set("anthropic-ratelimit-input-tokens-remaining", value)
		}
		a.captureAccountUpstreamState(created.ID, &http.Response{StatusCode: http.StatusOK, Header: headers})
		var remaining sql.NullInt64
		if err := a.db.QueryRow(`SELECT itpm_remaining FROM accounts WHERE id = ?`, created.ID).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining.Valid {
			t.Fatalf("header %q was accepted as a budget: %v", value, remaining)
		}
	}
}

// ITPM must count uncached input only: cache reads are excluded upstream, so
// charging them here throttles exactly the warm sessions that cost the least.
func TestStrategyITPMLimitExcludesCacheReads(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	var strategy struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "itpm", "rpm_limit": 0, "tpm_limit": 0, "itpm_limit": 10000, "concurrency_limit": 0,
		"rpm_strategy": "fixed", "rpm_sticky_buffer": 0, "dispatch_mode": "",
	}, http.StatusCreated, &strategy)
	account := createGatewayTestAccount(t, a, handler, "itpm-account", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`UPDATE accounts SET strategy_id = ? WHERE id = ?`, strategy.ID, account.ID); err != nil {
		t.Fatal(err)
	}

	// A warm session: a million cached tokens read back, almost no uncached
	// input. This is far above any total-throughput reading but costs the
	// upstream nothing against ITPM, so the account must stay dispatchable.
	if _, err := a.db.Exec(`INSERT INTO usage_logs (request_id, purpose_key, purpose_name, group_id, account_id, account_name, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
		VALUES ('warm', 'default', '默认用途', 'a', ?, 'itpm-account', 'claude-test', 500, 200, 0, 1000000)`, account.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatalf("a cache-heavy account was blocked by the ITPM limit: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)

	// Cache creation is uncached input and does count, so the same volume
	// written rather than read must block.
	if _, err := a.db.Exec(`INSERT INTO usage_logs (request_id, purpose_key, purpose_name, group_id, account_id, account_name, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens)
		VALUES ('cold', 'default', '默认用途', 'a', ?, 'itpm-account', 'claude-test', 500, 200, 20000, 0)`, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false); err == nil {
		t.Fatal("cache creation did not count toward the ITPM limit")
	}
}

// A large warm request that hits 429 must stay on its own account. Moving it
// discards the cached prefix and re-creates it elsewhere, turning a nearly free
// cache read into a full-price cache write — the very thing that pushed the
// account into the limit. Only permanent account failure justifies migrating.
func TestLargeWarmRequestStaysOnItsAccountAfterRateLimit(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var limitedCalls, sparecalls atomic.Int32
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		limitedCalls.Add(1)
		w.Header().Set("retry-after", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`)
	}))
	defer limited.Close()
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sparecalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer spare.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "warm", limited.URL, 0, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "spare", spare.URL, 1, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token-b"})

	// Cache affinity keys on input size, not the output cap: a body past the
	// large-request threshold is what makes the cached prefix worth protecting.
	body := `{"model":"claude-test","max_tokens":1024,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", gatewayLargeRequestBodyBytes) + `"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if limitedCalls.Load() != 1 {
		t.Fatalf("warm account calls = %d, want exactly 1", limitedCalls.Load())
	}
	if sparecalls.Load() != 0 {
		t.Fatalf("a rate-limited large request was moved to another account (%d calls), discarding its cache", sparecalls.Load())
	}
}

// The pin is scoped to rate limiting. An account answering 5xx is genuinely
// suspect, so availability outweighs the cache and failover still happens.
func TestLargeRequestStillFailsOverOnAccountLevelError(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var brokenCalls, spareCalls atomic.Int32
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		brokenCalls.Add(1)
		http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed"}}`, http.StatusInternalServerError)
	}))
	defer broken.Close()
	spare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		spareCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer spare.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "broken", broken.URL, 0, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "spare", spare.URL, 1, map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token-b"})

	large := `{"model":"claude-test","max_tokens":1024,"messages":[{"role":"user","content":"` +
		strings.Repeat("x", gatewayLargeRequestBodyBytes) + `"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(large))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	if brokenCalls.Load() != 1 || spareCalls.Load() != 1 {
		t.Fatalf("broken=%d spare=%d, want one failover away from the 5xx account", brokenCalls.Load(), spareCalls.Load())
	}
}

// Concurrent large requests on one session must not each pay a full cache
// creation: a cache entry is only readable once the first response has started,
// so overlapping siblings all miss. Serialising them is what makes the second
// request cheap rather than merely later.
func TestColdCacheFlightSerialisesSameSession(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, _ := newGatewayTestApp(t)
	defer a.db.Close()

	first, err := a.acquireColdCacheFlight(t.Context(), 7, "session-a", true)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	go func() {
		release, err := a.acquireColdCacheFlight(t.Context(), 7, "session-a", true)
		if err == nil {
			release()
		}
		close(entered)
	}()

	select {
	case <-entered:
		t.Fatal("a second large request entered while the first still held the session")
	case <-time.After(80 * time.Millisecond):
	}
	first()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting request was not released once the holder finished")
	}
}

// A different session, a different account, or a small request must not queue
// behind an unrelated flight — the exclusion is per (account, session) only.
func TestColdCacheFlightScopeIsAccountAndSession(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, _ := newGatewayTestApp(t)
	defer a.db.Close()

	held, err := a.acquireColdCacheFlight(t.Context(), 7, "session-a", true)
	if err != nil {
		t.Fatal(err)
	}
	defer held()

	for _, testCase := range []struct {
		name      string
		accountID int64
		session   string
		large     bool
	}{
		{"other session", 7, "session-b", true},
		{"other account", 8, "session-a", true},
		{"small request", 7, "session-a", false},
		{"no session", 7, "", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				release, err := a.acquireColdCacheFlight(t.Context(), testCase.accountID, testCase.session, testCase.large)
				if err == nil {
					release()
				}
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatalf("%s blocked behind an unrelated flight", testCase.name)
			}
		})
	}
}

// A cancelled request must not leave the session's slot held, or every later
// request on that session would block until the timeout.
func TestColdCacheFlightReleasesOnCancel(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, _ := newGatewayTestApp(t)
	defer a.db.Close()

	held, err := a.acquireColdCacheFlight(t.Context(), 9, "session-c", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	blocked := make(chan error, 1)
	go func() {
		_, waitErr := a.acquireColdCacheFlight(ctx, 9, "session-c", true)
		blocked <- waitErr
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if waitErr := <-blocked; !errors.Is(waitErr, context.Canceled) {
		t.Fatalf("cancelled wait returned %v, want context.Canceled", waitErr)
	}
	held()

	// The table must be clean afterwards, so the next request acquires at once.
	done := make(chan struct{})
	go func() {
		release, err := a.acquireColdCacheFlight(t.Context(), 9, "session-c", true)
		if err == nil {
			release()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a cancelled waiter leaked the session slot")
	}
}

// Cold placement follows the upstream's own remaining-budget report. This is
// what replaces predicting a request's token count before sending it.
func TestColdSessionPrefersAccountWithMostBudget(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	drained := createGatewayTestAccount(t, a, handler, "drained", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	roomy := createGatewayTestAccount(t, a, handler, "roomy", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	if _, err := a.db.Exec(`UPDATE accounts SET itpm_remaining = 5000 WHERE id = ?`, drained.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET itpm_remaining = 1800000 WHERE id = ?`, roomy.ID); err != nil {
		t.Fatal(err)
	}

	// No session hash: a cold start, so placement is free and should follow the
	// budget rather than the default ordering.
	selected, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	if selected.ID != roomy.ID {
		t.Fatalf("cold start selected %d, want the account with budget left (%d)", selected.ID, roomy.ID)
	}
}

// A warm session must not be re-placed by budget: that is exactly the account
// switch the cache pinning exists to prevent.
func TestWarmSessionIgnoresBudgetPlacement(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	bound := createGatewayTestAccount(t, a, handler, "bound", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	roomy := createGatewayTestAccount(t, a, handler, "roomy", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	if _, err := a.db.Exec(`UPDATE accounts SET itpm_remaining = 5000 WHERE id = ?`, bound.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET itpm_remaining = 1800000 WHERE id = ?`, roomy.ID); err != nil {
		t.Fatal(err)
	}
	a.bindGatewayStickySession(key.ID, "warm-session", bound.ID)

	selected, err := a.tryAcquireGatewayAccountWithPolicy(key, "warm-session", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	if selected.ID != bound.ID {
		t.Fatalf("warm session moved to %d for budget reasons, discarding its cache on %d", selected.ID, bound.ID)
	}
}

// The audit must fire on a changed tools/system prefix and stay silent when
// only the conversation grows — otherwise it would report every request as an
// invalidation and tell us nothing.
func TestCachePrefixAuditRecordsOnlyRealChanges(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, _ := newGatewayTestApp(t)
	defer a.db.Close()

	base := []byte(`{"system":[{"type":"text","text":"you are claude code"}],"tools":[{"name":"bash"}],"messages":[{"role":"user","content":"one"}]}`)
	grown := []byte(`{"system":[{"type":"text","text":"you are claude code"}],"tools":[{"name":"bash"}],"messages":[{"role":"user","content":"one"},{"role":"assistant","content":"ok"},{"role":"user","content":"two"}]}`)
	movedSystem := []byte(`{"system":[{"type":"text","text":"you are claude code at 12:01"}],"tools":[{"name":"bash"}],"messages":[{"role":"user","content":"one"}]}`)
	movedTools := []byte(`{"system":[{"type":"text","text":"you are claude code at 12:01"}],"tools":[{"name":"bash"},{"name":"edit"}],"messages":[{"role":"user","content":"one"}]}`)

	count := func() int {
		t.Helper()
		var rows int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM cache_prefix_events WHERE session_hash = 'audit'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	lastSegment := func() string {
		t.Helper()
		var segment string
		if err := a.db.QueryRow(`SELECT changed_segment FROM cache_prefix_events WHERE session_hash = 'audit' ORDER BY id DESC LIMIT 1`).Scan(&segment); err != nil {
			t.Fatal(err)
		}
		return segment
	}

	a.recordCachePrefixChange("audit", 1, "claude-test", base)
	if count() != 1 || lastSegment() != "initial" {
		t.Fatalf("first request = %d rows, segment %q", count(), lastSegment())
	}

	// A growing conversation is normal and must not be reported.
	a.recordCachePrefixChange("audit", 1, "claude-test", grown)
	if count() != 1 {
		t.Fatalf("a longer conversation was reported as a prefix change: %d rows", count())
	}

	a.recordCachePrefixChange("audit", 1, "claude-test", movedSystem)
	if count() != 2 || lastSegment() != "system" {
		t.Fatalf("changed system = %d rows, segment %q", count(), lastSegment())
	}

	a.recordCachePrefixChange("audit", 1, "claude-test", movedTools)
	if count() != 3 || lastSegment() != "tools" {
		t.Fatalf("changed tools = %d rows, segment %q", count(), lastSegment())
	}
}

// The audit is observation-only: it must never alter the outgoing body.
func TestCachePrefixAuditLeavesBodyUntouched(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, _ := newGatewayTestApp(t)
	defer a.db.Close()

	body := []byte(`{"system":[{"type":"text","text":"identity"}],"tools":[{"name":"bash"}],"messages":[]}`)
	original := string(body)
	a.recordCachePrefixChange("untouched", 1, "claude-test", body)
	if string(body) != original {
		t.Fatalf("audit mutated the request body:\n got %s\nwant %s", body, original)
	}
}

// migrateSharedData runs for both dialects, so SQLite-only DDL there breaks the
// MySQL boot. AUTOINCREMENT and CREATE INDEX IF NOT EXISTS are parse errors on
// MySQL — and a parse error is not saved by IF NOT EXISTS — so a shared
// migration must either avoid them or guard on the dialect.
func TestSharedMigrationDDLIsDialectSafe(t *testing.T) {
	sqliteOnly := []string{"AUTOINCREMENT", "CREATE INDEX IF NOT EXISTS", "CREATE UNIQUE INDEX IF NOT EXISTS"}
	for _, file := range []string{"cache_audit.go", "strategy_share.go"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		body := string(source)
		guarded := strings.Contains(body, "a.db.dialect == dialectMySQL")
		for _, token := range sqliteOnly {
			if strings.Contains(body, token) && !guarded {
				t.Fatalf("%s emits SQLite-only DDL (%s) from a shared migration without a MySQL guard", file, token)
			}
		}
	}
}
