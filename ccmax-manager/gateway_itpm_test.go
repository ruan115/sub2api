package main

import (
	"encoding/json"
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
	app.settleGatewayITPM(account, tokenUsage{Input: 1_000, CacheCreation: 2_000, CacheRead: 500_000})
	store.mu.Lock()
	settled = store.accounts[9]["large"]
	store.mu.Unlock()
	if settled.tokens != 3_000 {
		t.Fatalf("cache_read leaked into ITPM settlement: got %d, want 3000", settled.tokens)
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
