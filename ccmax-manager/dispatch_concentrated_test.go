package main

import (
	"net/http"
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
	states := map[string]string{}
	for _, account := range strategy.Accounts {
		states[account.Name] = account.Dispatch
	}
	if states["cc-active"] != "active" || states["cc-idle"] != "pending" {
		t.Fatalf("account dispatch states = %+v", states)
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
