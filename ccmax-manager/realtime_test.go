package main

import (
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRealtimeStatsUsesRollingMinute(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "realtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	create := func(name string) account {
		proxyID := createTestForwardProxy(t, a)
		var item account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": "token-" + name}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 4, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
			"base_rpm": 10, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
		}, http.StatusCreated, &item)
		return item
	}
	first := create("realtime-first")
	second := create("realtime-second")
	for range 2 {
		a.recordGatewayAccountRPM(first.ID)
	}
	a.recordGatewayAccountRPM(second.ID)
	if _, err := a.db.Exec(`INSERT INTO account_rpm_events (account_id, created_at) VALUES (?, ?)`, first.ID, time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.recordUsage(usageInput{RequestID: "realtime-usage", PurposeKey: "default", GroupID: "a", AccountID: first.ID, Model: "claude-test", InputTokens: 100, OutputTokens: 40, CacheReadTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 2)`, second.ID); err != nil {
		t.Fatal(err)
	}

	var result realtimeLoad
	putJSON(t, handler, http.MethodGet, "/api/stats/realtime?group_id=a", nil, http.StatusOK, &result)
	if result.WindowSeconds != 60 || result.RPM != 3 || result.TPM != 150 || result.Inflight != 2 {
		t.Fatalf("realtime totals = %+v", result)
	}
	if result.ActiveAccounts != 2 || result.EligibleAccounts != 2 || result.RPMCapacity != 20 || result.Unlimited {
		t.Fatalf("realtime capacity = %+v", result)
	}
	if len(result.Accounts) != 2 || result.Accounts[0].AccountID != first.ID || result.Accounts[0].RPM != 2 || result.Accounts[0].TPM != 150 {
		t.Fatalf("realtime accounts = %+v", result.Accounts)
	}
	a.captureAccountRPMThreshold("a", first.ID)
	var limited realtimeLoad
	putJSON(t, handler, http.MethodGet, "/api/stats/realtime?group_id=a", nil, http.StatusOK, &limited)
	if limited.EligibleAccounts != 1 || limited.RPMCapacity != 10 || limited.Accounts[0].EffectiveRPM != 2 || limited.Accounts[0].TemporaryRPM != 2 || limited.Accounts[0].Eligible {
		t.Fatalf("realtime learned threshold = %+v", limited)
	}

	var empty realtimeLoad
	putJSON(t, handler, http.MethodGet, "/api/stats/realtime?group_id=b", nil, http.StatusOK, &empty)
	if len(empty.Accounts) != 0 || empty.RPM != 0 || empty.TPM != 0 {
		t.Fatalf("group filter leaked realtime data: %+v", empty)
	}
}

func TestRPMConcentratedDispatchFillsOneAccountBeforeNext(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	if !key.RPMDispatchEnabled {
		t.Fatal("RPM concentrated dispatch must default to enabled")
	}
	first := createGatewayTestAccount(t, a, handler, "concentrated-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "concentrated-second", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	if _, err := a.db.Exec(`UPDATE accounts SET base_rpm = 2 WHERE id IN (?, ?)`, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}

	selectedIDs := make([]int64, 0, 3)
	for range 3 {
		selected, acquireErr := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		selectedIDs = append(selectedIDs, selected.ID)
		a.releaseGatewayAccount(selected.ID)
	}
	if selectedIDs[0] != first.ID || selectedIDs[1] != first.ID || selectedIDs[2] != second.ID {
		t.Fatalf("concentrated selections = %v, want [%d %d %d]", selectedIDs, first.ID, first.ID, second.ID)
	}
	var firstRPM, secondRPM int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, first.ID).Scan(&firstRPM); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, second.ID).Scan(&secondRPM); err != nil {
		t.Fatal(err)
	}
	if firstRPM != 2 || secondRPM != 1 {
		t.Fatalf("concentrated RPM reservations = %d/%d", firstRPM, secondRPM)
	}
	if gatewayMaxAttempts(true) != 2 || gatewayMaxAttempts(false) != 11 {
		t.Fatalf("gateway attempts concentrated=%d compatibility=%d", gatewayMaxAttempts(true), gatewayMaxAttempts(false))
	}
}

func TestRPMConcentratedDispatchReservesCapacityAcrossConcurrentSelections(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	first := createGatewayTestAccount(t, a, handler, "concurrent-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "concurrent-second", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	if _, err := a.db.Exec(`UPDATE accounts SET base_rpm = 3 WHERE id IN (?, ?)`, first.ID, second.ID); err != nil {
		t.Fatal(err)
	}

	selected := make(chan int64, 6)
	errors := make(chan error, 6)
	var workers sync.WaitGroup
	for range 6 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			account, acquireErr := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
			if acquireErr != nil {
				errors <- acquireErr
				return
			}
			selected <- account.ID
		}()
	}
	workers.Wait()
	close(selected)
	close(errors)
	for acquireErr := range errors {
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
	}
	counts := map[int64]int{}
	for accountID := range selected {
		counts[accountID]++
		a.releaseGatewayAccount(accountID)
	}
	if counts[first.ID] != 3 || counts[second.ID] != 3 {
		t.Fatalf("concurrent concentrated selections = %+v", counts)
	}
}

func TestDispatchStrategyRoundRobinSpreadsRPM(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	var strategy struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "round-robin-100", "rpm_limit": 100, "rpm_strategy": "fixed", "dispatch_mode": "round_robin",
	}, http.StatusCreated, &strategy)
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "round robin", "rate_multiplier": 1,
		"status": "active", "rpm_dispatch_enabled": true, "strategy_id": strategy.ID,
	}, http.StatusOK, nil)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	first := createGatewayTestAccount(t, a, handler, "rr-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "rr-second", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	third := createGatewayTestAccount(t, a, handler, "rr-third", "https://third.example.test", 0, nil, map[string]any{"access_token": "token-c"})

	selectedIDs := make([]int64, 0, 6)
	for range 6 {
		selected, acquireErr := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		selectedIDs = append(selectedIDs, selected.ID)
		a.releaseGatewayAccount(selected.ID)
	}
	want := []int64{first.ID, second.ID, third.ID, first.ID, second.ID, third.ID}
	for index, id := range want {
		if selectedIDs[index] != id {
			t.Fatalf("round-robin selections = %v, want %v", selectedIDs, want)
		}
	}
}

func TestDispatchStrategyRoundRobinKeepsStickyTrafficInItsShare(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	var strategy struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "round-robin-sticky", "rpm_limit": 100, "rpm_strategy": "fixed", "dispatch_mode": "round_robin",
	}, http.StatusCreated, &strategy)
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "round robin sticky", "rate_multiplier": 1,
		"status": "active", "rpm_dispatch_enabled": true, "strategy_id": strategy.ID,
	}, http.StatusOK, nil)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	first := createGatewayTestAccount(t, a, handler, "rr-sticky-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "rr-sticky-second", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	third := createGatewayTestAccount(t, a, handler, "rr-sticky-third", "https://third.example.test", 0, nil, map[string]any{"access_token": "token-c"})

	selectAccount := func(session string) int64 {
		t.Helper()
		selected, acquireErr := a.acquireGatewayAccount(key, session, "claude-test", map[int64]bool{})
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		a.releaseGatewayAccount(selected.ID)
		return selected.ID
	}
	selectedIDs := []int64{
		selectAccount("conversation-a"),
		selectAccount("conversation-a"),
		selectAccount("conversation-b"),
		selectAccount("conversation-c"),
		selectAccount("conversation-d"),
		selectAccount("conversation-e"),
	}
	want := []int64{first.ID, first.ID, second.ID, third.ID, second.ID, third.ID}
	for index, id := range want {
		if selectedIDs[index] != id {
			t.Fatalf("round-robin sticky selections = %v, want %v", selectedIDs, want)
		}
	}

	for _, accountID := range []int64{first.ID, second.ID, third.ID} {
		var rpm int
		if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, accountID).Scan(&rpm); err != nil {
			t.Fatal(err)
		}
		if rpm != 2 {
			t.Fatalf("account %d reserved RPM = %d, want 2", accountID, rpm)
		}
	}
}

func TestDispatchStrategyRoundRobinReservesRPMWithoutGroupDispatch(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	var strategy struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "round-robin-unlimited", "rpm_limit": 0, "rpm_strategy": "fixed", "dispatch_mode": "round_robin",
	}, http.StatusCreated, &strategy)
	disabled := false
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "round robin", "rate_multiplier": 1,
		"status": "active", "rpm_dispatch_enabled": disabled, "strategy_id": strategy.ID,
	}, http.StatusOK, nil)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	first := createGatewayTestAccount(t, a, handler, "rr-reserve-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "rr-reserve-second", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})

	selected, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(selected.ID)
	next, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(next.ID)
	if selected.ID != first.ID || next.ID != second.ID {
		t.Fatalf("round-robin without group dispatch = %d/%d, want %d/%d", selected.ID, next.ID, first.ID, second.ID)
	}
	var firstRPM int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, first.ID).Scan(&firstRPM); err != nil {
		t.Fatal(err)
	}
	if firstRPM != 1 {
		t.Fatalf("round-robin RPM reservation = %d, want 1", firstRPM)
	}
}

func TestCompatibilityDispatchRetainsRoundRobinLane(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	disabled := false
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "compatibility", "rate_multiplier": 1,
		"status": "active", "rpm_dispatch_enabled": disabled,
	}, http.StatusOK, nil)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	first := createGatewayTestAccount(t, a, handler, "compat-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "token-a"})
	second := createGatewayTestAccount(t, a, handler, "compat-second", "https://second.example.test", 0, nil, map[string]any{"access_token": "token-b"})
	selected, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	a.recordGatewayAccountRPM(selected.ID)
	a.releaseGatewayAccount(selected.ID)
	next, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	a.releaseGatewayAccount(next.ID)
	if selected.ID != first.ID || next.ID != second.ID {
		t.Fatalf("compatibility selections = %d/%d, want %d/%d", selected.ID, next.ID, first.ID, second.ID)
	}
}

func TestRPMDispatchMigrationPreservesExistingHedgeMode(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	databasePath := filepath.Join(t.TempDir(), "hedge-migration.db")
	a, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE groups SET stream_hedge_enabled = 1, rpm_dispatch_enabled = 1 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	var streamHedge, rpmDispatch int
	if err := reopened.db.QueryRow(`SELECT stream_hedge_enabled, rpm_dispatch_enabled FROM groups WHERE id = 'a'`).Scan(&streamHedge, &rpmDispatch); err != nil {
		t.Fatal(err)
	}
	if streamHedge != 1 || rpmDispatch != 0 {
		t.Fatalf("migrated group stream_hedge=%d rpm_dispatch=%d", streamHedge, rpmDispatch)
	}
}
