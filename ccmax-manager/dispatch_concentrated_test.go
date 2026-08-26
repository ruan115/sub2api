package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func createTestStrategy(t *testing.T, handler http.Handler, payload map[string]any) int64 {
	t.Helper()
	var strategy struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", payload, http.StatusCreated, &strategy)
	return strategy.ID
}

func bindGroupStrategy(t *testing.T, handler http.Handler, strategyID int64, extra map[string]any) {
	t.Helper()
	payload := map[string]any{
		"name": "A 分组", "description": "dispatch test", "rate_multiplier": 1,
		"status": "active", "strategy_id": strategyID,
	}
	for key, value := range extra {
		payload[key] = value
	}
	putJSON(t, handler, http.MethodPut, "/api/groups/a", payload, http.StatusOK, nil)
}

// Concentrated dispatch keeps traffic on one account until its RPM cap is hit,
// then promotes exactly one more account and leaves the rest pending.
func TestDispatchStrategyConcentratedFillsOneAccountBeforePromotingTheNext(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "concentrated-3", "rpm_limit": 3, "rpm_strategy": "fixed", "dispatch_mode": "concentrated",
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	first := createGatewayTestAccount(t, a, handler, "cc-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "cc-second", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	third := createGatewayTestAccount(t, a, handler, "cc-third", "https://third.example.test", 0, nil, map[string]any{"access_token": "token-c"})

	selected := make([]int64, 0, 9)
	for range 9 {
		account, acquireErr := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
		if acquireErr != nil {
			t.Fatalf("acquire %d: %v", len(selected), acquireErr)
		}
		selected = append(selected, account.ID)
		a.releaseGatewayAccount(account.ID)
	}
	// Each account absorbs its full rpm_limit of 3 before the next is woken.
	want := []int64{
		first.ID, first.ID, first.ID,
		second.ID, second.ID, second.ID,
		third.ID, third.ID, third.ID,
	}
	for index, id := range want {
		if selected[index] != id {
			t.Fatalf("concentrated selections = %v, want %v", selected, want)
		}
	}
	// Everything is capped now, so the next request finds no capacity.
	if _, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{}); err == nil {
		t.Fatal("expected no capacity once every account reached its RPM cap")
	}
}

// An idle account under a concentrated strategy is reported as 待调度 rather than
// simply "alive", so the observation page can show what is being held back.
func TestStrategyObservationReportsPendingAccountsForConcentrated(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "concentrated-observe", "rpm_limit": 5, "rpm_strategy": "fixed", "dispatch_mode": "concentrated",
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	active := createGatewayTestAccount(t, a, handler, "cc-active", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "cc-idle", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})

	selected, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(selected.ID)
	if selected.ID != active.ID {
		t.Fatalf("first selection = %d, want %d", selected.ID, active.ID)
	}

	var observed []strategyObservation
	putJSON(t, handler, http.MethodGet, "/api/strategies/observe", nil, http.StatusOK, &observed)
	if len(observed) != 1 {
		t.Fatalf("observations = %d, want 1", len(observed))
	}
	strategy := observed[0]
	if strategy.AccountsPending != 1 {
		t.Fatalf("pending accounts = %d, want 1", strategy.AccountsPending)
	}
	if strategy.RPMCapacity != 10 || strategy.RPMCapacityUnlimited {
		t.Fatalf("rpm capacity = %d unlimited=%v, want 10/false", strategy.RPMCapacity, strategy.RPMCapacityUnlimited)
	}
	states := map[string]string{}
	for _, account := range strategy.Accounts {
		states[account.Name] = account.Dispatch
	}
	if states["cc-active"] != "active" || states["cc-idle"] != "pending" {
		t.Fatalf("account dispatch states = %+v", states)
	}
}

func TestStrategyObservationUsesTemporaryRPMForCapacity(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "temporary-capacity", "rpm_limit": 8, "rpm_strategy": "fixed", "dispatch_mode": "balance",
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	limited := createGatewayTestAccount(t, a, handler, "limited-capacity", "https://limited.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	createGatewayTestAccount(t, a, handler, "normal-capacity", "https://normal.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reason = '429_backoff', rate_limit_downweight_until = strftime('%Y-%m-%dT%H:%M:%fZ','now','+5 minutes') WHERE id = ?`, limited.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_rpm_thresholds (account_id, rpm_limit, reset_at) VALUES (?, 3, strftime('%Y-%m-%dT%H:%M:%fZ','now','+5 minutes'))`, limited.ID); err != nil {
		t.Fatal(err)
	}

	var observed []strategyObservation
	putJSON(t, handler, http.MethodGet, "/api/strategies/observe", nil, http.StatusOK, &observed)
	if len(observed) != 1 {
		t.Fatalf("observations = %d, want 1", len(observed))
	}
	if observed[0].RPMCapacity != 11 || observed[0].RPMCapacityUnlimited {
		t.Fatalf("rpm capacity = %d unlimited=%v, want 11/false", observed[0].RPMCapacity, observed[0].RPMCapacityUnlimited)
	}
}

func TestStrategyObservationExcludesUnavailableAccounts(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "available-only", "rpm_limit": 10, "rpm_strategy": "fixed", "dispatch_mode": "balance",
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	available := createGatewayTestAccount(t, a, handler, "strategy-available", "https://available.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	unavailable := createGatewayTestAccount(t, a, handler, "strategy-paused", "https://paused.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	if _, err := a.db.Exec(`UPDATE accounts SET schedulable = 0 WHERE id = ?`, unavailable.ID); err != nil {
		t.Fatal(err)
	}

	var observed []strategyObservation
	putJSON(t, handler, http.MethodGet, "/api/strategies/observe", nil, http.StatusOK, &observed)
	if len(observed) != 1 {
		t.Fatalf("observations = %d, want 1", len(observed))
	}
	strategy := observed[0]
	if strategy.BoundAccounts != 2 || strategy.AccountsAlive != 1 || len(strategy.Accounts) != 1 {
		t.Fatalf("strategy accounts = bound:%d alive:%d observation rows:%d, want 2/1/1", strategy.BoundAccounts, strategy.AccountsAlive, len(strategy.Accounts))
	}
	if strategy.Accounts[0].AccountID != available.ID {
		t.Fatalf("observed account = %d, want available account %d", strategy.Accounts[0].AccountID, available.ID)
	}
}

// Concentrated and round-robin strategies decide how many accounts are in play,
// so an exhausted pool must queue even without the group's capacity toggle.
func TestConcentratedStrategyQueuesWithoutGroupCapacityToggle(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "concentrated-queue", "rpm_limit": 1, "rpm_strategy": "fixed", "dispatch_mode": "concentrated",
	})
	bindGroupStrategy(t, handler, strategyID, map[string]any{"capacity_queue_enabled": false})
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	if key.CapacityQueueEnabled {
		t.Fatal("group capacity queue should be off for this test")
	}
	createGatewayTestAccount(t, a, handler, "cc-queue", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	if !a.gatewayShouldQueue(key) {
		t.Fatal("concentrated strategy did not opt the group into capacity queueing")
	}
}

func TestGatewayAccountFailoverBudgetStopsAfterTwoAccounts(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		mode      string
		maxTokens int
	}{
		{name: "round robin", mode: "round_robin", maxTokens: 32},
		{name: "concentrated", mode: "concentrated", maxTokens: 32},
		{name: "large compatibility request", maxTokens: gatewayLargeRequestMaxTokens},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, `{"type":"error","error":{"type":"api_error","message":"failed"}}`, http.StatusInternalServerError)
			}))
			defer upstream.Close()

			a, handler := newGatewayTestApp(t)
			defer a.db.Close()
			key := createGatewayTestKey(t, handler)
			groupUpdate := map[string]any{
				"name": "A 分组", "description": "failover budget", "rate_multiplier": 1,
				"status": "active", "rpm_dispatch_enabled": false,
			}
			if testCase.mode != "" {
				strategyID := createTestStrategy(t, handler, map[string]any{
					"name": testCase.name, "rpm_limit": 100, "rpm_strategy": "fixed", "dispatch_mode": testCase.mode,
				})
				groupUpdate["strategy_id"] = strategyID
			}
			putJSON(t, handler, http.MethodPut, "/api/groups/a", groupUpdate, http.StatusOK, nil)
			for index := range 3 {
				createGatewayTestAccount(t, a, handler, testCase.name+string(rune('a'+index)), upstream.URL, index,
					map[string]any{"request_passthrough": true}, map[string]any{"access_token": "token"})
			}

			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":`+fmt.Sprint(testCase.maxTokens)+`,"messages":[{"role":"user","content":"hello"}]}`))
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			_, _ = io.Copy(io.Discard, response.Result().Body)
			gotCalls := calls.Load()
			if gotCalls < 1 || gotCalls > 2 {
				t.Fatalf("upstream calls=%d, want between 1 and 2", gotCalls)
			}
			if testCase.mode != "" && gotCalls != 2 {
				t.Fatalf("%s upstream calls=%d, want exactly 2-account failover budget", testCase.mode, gotCalls)
			}
		})
	}
}

// The group toggle restricts dispatch to accounts that resolve a strategy, and
// stays off by default so existing unbound pools keep serving traffic.
func TestStrategyRequiredGroupSkipsUnboundAccounts(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "required-strategy", "rpm_limit": 50, "rpm_strategy": "fixed",
	})
	// The group itself stays unbound so only account-level bindings resolve.
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "strategy required", "rate_multiplier": 1,
		"status": "active", "strategy_required_enabled": true, "strategy_id": 0,
	}, http.StatusOK, nil)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !key.StrategyRequiredEnabled {
		t.Fatal("strategy_required_enabled did not reach the gateway key")
	}
	unbound := createGatewayTestAccount(t, a, handler, "no-strategy", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	if _, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{}); err == nil {
		t.Fatal("unbound account was dispatched by a strategy-required group")
	}

	bound := createGatewayTestAccount(t, a, handler, "with-strategy", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{
		"ids": []int64{bound.ID}, "strategy_id": strategyID,
	}, http.StatusOK, nil)
	selected, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
	if err != nil {
		t.Fatalf("strategy-bound account was not dispatched: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
	if selected.ID != bound.ID {
		t.Fatalf("selected account = %d, want the strategy-bound %d", selected.ID, bound.ID)
	}

	// Turning the flag back off restores the unbound account.
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "strategy required", "rate_multiplier": 1,
		"status": "active", "strategy_required_enabled": false,
	}, http.StatusOK, nil)
	relaxed, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	if relaxed.StrategyRequiredEnabled {
		t.Fatal("strategy requirement was not cleared")
	}
	reopened, err := a.acquireGatewayAccount(relaxed, "", "claude-test", map[int64]bool{bound.ID: true})
	if err != nil {
		t.Fatalf("unbound account %d still not dispatchable: %v", unbound.ID, err)
	}
	a.releaseGatewayAccount(reopened.ID)
	if reopened.ID != unbound.ID {
		t.Fatalf("selected account = %d, want the unbound %d", reopened.ID, unbound.ID)
	}
}
