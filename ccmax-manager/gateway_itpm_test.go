package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLocalITPMReservationZones(t *testing.T) {
	var store localITPMReservationStore

	allowed, _ := store.reserve(1, "normal", 5_000, 90_000, gatewayITPMSoftLimit, gatewayITPMHardLimit, false, false, 0)
	if !allowed {
		t.Fatal("request below the soft ITPM limit was rejected")
	}
	store.release(1, "normal")

	allowed, _ = store.reserve(1, "soft-large", gatewayITPMSmallRequest+1, 95_000, gatewayITPMSoftLimit, gatewayITPMHardLimit, false, false, 0)
	if allowed {
		t.Fatal("non-sticky large request was accepted in the soft zone")
	}
	allowed, _ = store.reserve(1, "soft-sticky", gatewayITPMSmallRequest+1, 95_000, gatewayITPMSoftLimit, gatewayITPMHardLimit, true, false, 0)
	if !allowed {
		t.Fatal("sticky request was rejected in the soft zone")
	}
	store.release(1, "soft-sticky")

	allowed, _ = store.reserve(1, "hard", 5_001, 145_000, gatewayITPMSoftLimit, gatewayITPMHardLimit, true, false, 0)
	if allowed {
		t.Fatal("request crossing the hard ITPM limit was accepted")
	}
}

func TestLocalITPMExclusiveReservationAndCacheReadSettlement(t *testing.T) {
	app := &app{}
	store := &app.localITPMReservations
	allowed, _ := store.reserve(9, "large", 120_000, 20_000, gatewayITPMSoftLimit, gatewayITPMHardLimit, false, true, 0)
	if !allowed {
		t.Fatal("idle account rejected its exclusive large request")
	}
	allowed, _ = store.reserve(9, "parallel", 1, 20_000, gatewayITPMSoftLimit, gatewayITPMHardLimit, true, false, 0)
	if allowed {
		t.Fatal("exclusive request did not reduce account concurrency to one")
	}

	store.settle(9, "large", 3_000)
	store.mu.Lock()
	settled := store.accounts[9]["large"]
	store.mu.Unlock()
	if settled.tokens != 3_000 || settled.exclusive {
		t.Fatalf("settled reservation = %+v, want 3000 non-exclusive tokens", settled)
	}

	account := gatewayAccount{ID: 9, ITPMReservationID: "large"}
	app.settleGatewayITPM(account, tokenUsage{Input: 1_000, CacheCreation: 50_000, CacheRead: 500_000})
	store.mu.Lock()
	settled = store.accounts[9]["large"]
	store.mu.Unlock()
	if settled.tokens != 51_000 {
		t.Fatalf("ITPM settlement capped real cache creation or included cache read: got %d, want 51000", settled.tokens)
	}
}

func TestEstimateGatewayDispatchDemandUsesFastLargeBodyPath(t *testing.T) {
	body, err := json.Marshal(map[string]any{
		"model":      "claude-fable-5",
		"max_tokens": 16,
		"messages": []map[string]any{{
			"role": "user", "content": strings.Repeat("a", 400_000),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	demand := estimateGatewayDispatchDemand(body, false)
	if !demand.Oversized || demand.EstimatedITPM <= gatewayITPMLargeRequest {
		t.Fatalf("large request demand = %+v", demand)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("large-body estimate took %s", elapsed)
	}
}

func TestOversizedDispatchChoosesIdleLowestITPMAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	high := createGatewayTestAccount(t, a, handler, "high-itpm", "https://high.example.test", 0, nil, map[string]any{"access_token": "high"})
	low := createGatewayTestAccount(t, a, handler, "low-itpm", "https://low.example.test", 10, nil, map[string]any{"access_token": "low"})
	for _, input := range []usageInput{
		{RequestID: "high-itpm-usage", PurposeKey: "default", GroupID: "a", AccountID: high.ID, Model: "claude-fable-5", InputTokens: 50_000},
		{RequestID: "low-itpm-usage", PurposeKey: "default", GroupID: "a", AccountID: low.ID, Model: "claude-fable-5", InputTokens: 10_000},
	} {
		if _, _, err := a.recordUsage(input); err != nil {
			t.Fatal(err)
		}
	}

	selected, err := a.tryAcquireGatewayAccountWithPolicyDemand(gatewayKey{GroupID: "a"}, "", "claude-fable-5", map[int64]bool{}, false, gatewayDispatchDemand{EstimatedITPM: 110_000, Oversized: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	defer a.releaseGatewayITPM(selected)
	if selected.ID != low.ID {
		t.Fatalf("selected account %d, want lowest-ITPM idle account %d", selected.ID, low.ID)
	}

	var inflight int
	if err := a.db.QueryRow(`SELECT requests FROM account_inflight WHERE account_id = ?`, selected.ID).Scan(&inflight); err != nil {
		t.Fatal(err)
	}
	if inflight != 1 || !selected.ITPMExclusive {
		t.Fatalf("large dispatch inflight=%d exclusive=%t", inflight, selected.ITPMExclusive)
	}
}

func TestLocalITPMExclusiveReservationRejectsAccountAtHardLimit(t *testing.T) {
	var store localITPMReservationStore
	allowed, _ := store.reserve(1, "hard-exclusive", 110_000, 150_000, 100_000, 150_000, false, true, 0)
	if allowed {
		t.Fatal("oversized request was reserved on an account already at the hard ITPM limit")
	}
}

func TestDispatchStrategyITPMProtectionUsesConfiguredWindowAndSwitch(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "scheduler-itpm-window", "rpm_limit": 8, "rpm_strategy": "fixed", "dispatch_mode": "balance",
		"itpm_protection_enabled": true,
		"itpm_window_seconds":     30,
		"itpm_soft_limit":         100,
		"itpm_hard_limit":         150,
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	created := createGatewayTestAccount(t, a, handler, "scheduler-itpm-account", "https://itpm.example.test", 0, nil, map[string]any{"access_token": "token"})
	if _, _, err := a.recordUsage(usageInput{
		RequestID: "scheduler-itpm-usage", PurposeKey: "default", GroupID: "a", AccountID: created.ID,
		Model: "claude-fable-5", InputTokens: 160,
	}); err != nil {
		t.Fatal(err)
	}

	demand := gatewayDispatchDemand{EstimatedITPM: 1}
	if _, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand); err == nil {
		t.Fatal("account above the configured hard ITPM limit remained schedulable")
	}

	if _, err := a.db.Exec(`UPDATE usage_logs SET created_at = ? WHERE request_id = ?`, time.Now().UTC().Add(-45*time.Second).Format(time.RFC3339Nano), "scheduler-itpm-usage"); err != nil {
		t.Fatal(err)
	}
	selected, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
	if err != nil {
		t.Fatalf("usage outside the configured 30-second window blocked dispatch: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
	a.releaseGatewayITPM(selected)

	if _, err := a.db.Exec(`UPDATE usage_logs SET created_at = ? WHERE request_id = ?`, time.Now().UTC().Format(time.RFC3339Nano), "scheduler-itpm-usage"); err != nil {
		t.Fatal(err)
	}
	putJSON(t, handler, http.MethodPut, fmt.Sprintf("/api/strategies/%d", strategyID), map[string]any{
		"name": "scheduler-itpm-window", "rpm_limit": 8, "rpm_strategy": "fixed", "dispatch_mode": "balance",
		"itpm_protection_enabled": false,
		"itpm_window_seconds":     30,
		"itpm_soft_limit":         100,
		"itpm_hard_limit":         150,
	}, http.StatusOK, nil)
	selected, err = a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
	if err != nil {
		t.Fatalf("disabled strategy ITPM protection still blocked dispatch: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
}
