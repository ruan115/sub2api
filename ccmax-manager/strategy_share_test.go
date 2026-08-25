package main

import (
	"net/http"
	"testing"
)

func TestStrategyOverShareSplitsGroupTraffic(t *testing.T) {
	weights := map[int64]int{1: 6, 2: 4}
	for _, testCase := range []struct {
		name        string
		strategyRPM map[int64]int
		groupRPM    int
		strategyID  int64
		want        bool
	}{
		// Rounding up is what keeps an idle group usable: with floor, the first
		// request would compute a zero allowance for every strategy and the
		// group would deadlock with capacity to spare.
		{"idle group admits the majority strategy", map[int64]int{}, 0, 1, false},
		{"idle group admits the minority strategy", map[int64]int{}, 0, 2, false},
		{"majority still under its 60% slice", map[int64]int{1: 6, 2: 4}, 10, 1, false},
		{"majority past its 60% slice", map[int64]int{1: 7, 2: 4}, 11, 1, true},
		{"minority still under its 40% slice", map[int64]int{1: 6, 2: 4}, 10, 2, false},
		{"minority past its 40% slice", map[int64]int{1: 6, 2: 5}, 11, 2, true},
		{"unconfigured strategy is unrestricted", map[int64]int{3: 99}, 99, 3, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := strategyOverShare(weights, 10, testCase.strategyRPM, testCase.groupRPM, testCase.strategyID)
			if got != testCase.want {
				t.Fatalf("strategyOverShare = %v, want %v", got, testCase.want)
			}
		})
	}
}

// With no weights configured the split must be inert, not a silent block.
func TestStrategyOverShareIgnoresUnconfiguredGroups(t *testing.T) {
	if strategyOverShare(nil, 0, map[int64]int{1: 500}, 500, 1) {
		t.Fatal("an unconfigured group blocked dispatch")
	}
}

// A group whose weighted strategy is over its share must report no capacity so
// the request lands in the existing capacity queue rather than spilling into a
// strategy the operator did not pick for it.
func TestGatewayStrategyShareBlocksOverServedStrategy(t *testing.T) {
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
		"name": "share-a", "rpm_limit": 0, "tpm_limit": 0, "concurrency_limit": 0,
		"rpm_strategy": "fixed", "rpm_sticky_buffer": 0, "dispatch_mode": "",
	}, http.StatusCreated, &strategy)
	account := createGatewayTestAccount(t, a, handler, "share-account", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`UPDATE accounts SET strategy_id = ? WHERE id = ?`, strategy.ID, account.ID); err != nil {
		t.Fatal(err)
	}
	// The only strategy in the group holds the whole weight, so it may take all
	// the traffic and dispatch must still succeed.
	if _, err := a.db.Exec(`INSERT INTO group_strategy_shares (group_id, strategy_id, weight) VALUES ('a', ?, 10)`, strategy.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false)
	if err != nil {
		t.Fatalf("sole weighted strategy was blocked: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)

	// Give it a tenth of the weight while a competing strategy holds the rest,
	// and load the last minute so it is already past its slice.
	if _, err := a.db.Exec(`UPDATE group_strategy_shares SET weight = 1 WHERE strategy_id = ?`, strategy.ID); err != nil {
		t.Fatal(err)
	}
	var other struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "share-b", "rpm_limit": 0, "tpm_limit": 0, "concurrency_limit": 0,
		"rpm_strategy": "fixed", "rpm_sticky_buffer": 0, "dispatch_mode": "",
	}, http.StatusCreated, &other)
	if _, err := a.db.Exec(`INSERT INTO group_strategy_shares (group_id, strategy_id, weight) VALUES ('a', ?, 9)`, other.ID); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if _, err := a.db.Exec(`INSERT INTO account_rpm_events (account_id) VALUES (?)`, account.ID); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := a.tryAcquireGatewayAccountWithPolicy(key, "", "claude-fable-5", map[int64]bool{}, false); err == nil {
		t.Fatal("an over-served strategy still received traffic")
	}
}

func TestGroupStrategySharesRoundTrip(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	var strategy struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "round-trip", "rpm_limit": 0, "tpm_limit": 0, "concurrency_limit": 0,
		"rpm_strategy": "fixed", "rpm_sticky_buffer": 0, "dispatch_mode": "",
	}, http.StatusCreated, &strategy)
	account := createGatewayTestAccount(t, a, handler, "round-trip-account", "https://example.test", 0, nil, map[string]any{"access_token": "token"})
	if _, err := a.db.Exec(`UPDATE accounts SET strategy_id = ? WHERE id = ?`, strategy.ID, account.ID); err != nil {
		t.Fatal(err)
	}

	var shares []groupStrategyShare
	putJSON(t, handler, http.MethodGet, "/api/groups/a/strategy-shares", nil, http.StatusOK, &shares)
	if len(shares) != 1 || shares[0].StrategyID != strategy.ID || shares[0].Weight != 0 || shares[0].Accounts != 1 {
		t.Fatalf("listed shares = %+v", shares)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "策略分配", "rate_multiplier": 1, "status": "active",
		"strategy_shares": []map[string]any{{"strategy_id": strategy.ID, "weight": 7}},
	}, http.StatusOK, nil)

	putJSON(t, handler, http.MethodGet, "/api/groups/a/strategy-shares", nil, http.StatusOK, &shares)
	if len(shares) != 1 || shares[0].Weight != 7 || shares[0].Percent != 100 {
		t.Fatalf("saved shares = %+v", shares)
	}

	// An unknown strategy must be rejected rather than silently dropped.
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "策略分配", "rate_multiplier": 1, "status": "active",
		"strategy_shares": []map[string]any{{"strategy_id": 999999, "weight": 3}},
	}, http.StatusBadRequest, nil)
}
