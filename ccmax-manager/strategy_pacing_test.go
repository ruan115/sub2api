package main

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// Interval pacing admits at most pacing_concurrency new requests per
// pacing_interval_seconds window, so an RPM budget cannot burn out in the
// first seconds of the minute.
func TestDispatchPacingIntervalLimitsAdmissionsPerWindow(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "pacing-interval", "rpm_limit": 8, "rpm_strategy": "fixed", "dispatch_mode": "balance",
		"dispatch_pacing":         "interval",
		"pacing_concurrency":      2,
		"pacing_interval_seconds": 60,
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	created := createGatewayTestAccount(t, a, handler, "pacing-interval-account", "https://pacing.example.test", 0, nil, map[string]any{"access_token": "token"})

	demand := gatewayDispatchDemand{EstimatedITPM: 1}
	for admitted := 0; admitted < 2; admitted++ {
		selected, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
		if err != nil {
			t.Fatalf("admission %d within the interval budget failed: %v", admitted+1, err)
		}
		a.releaseGatewayAccount(selected.ID)
		a.releaseGatewayITPM(selected)
	}

	_, err = a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
	if !errors.Is(err, errNoGatewayAccountCapacity) {
		t.Fatalf("third admission in the same interval error=%v, want capacity error", err)
	}
	if !gatewayPacingBlocked(err) {
		t.Fatalf("capacity error does not report pacing block: %v", err)
	}

	// Once the window slides past the earlier admissions the account opens again.
	aged := time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE account_rpm_events SET created_at = ? WHERE account_id = ?`, aged, created.ID); err != nil {
		t.Fatal(err)
	}
	selected, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
	if err != nil {
		t.Fatalf("admission after the interval window slid failed: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
	a.releaseGatewayITPM(selected)
}

// Completion pacing keeps at most pacing_concurrency requests in flight and
// admits the next one as soon as a slot frees — no batch barrier.
func TestDispatchPacingCompletionAdmitsAsSlotsFree(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "pacing-completion", "rpm_limit": 8, "rpm_strategy": "fixed", "dispatch_mode": "balance",
		"dispatch_pacing":    "completion",
		"pacing_concurrency": 2,
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	createGatewayTestAccount(t, a, handler, "pacing-completion-account", "https://pacing2.example.test", 0, nil, map[string]any{"access_token": "token"})

	demand := gatewayDispatchDemand{EstimatedITPM: 1}
	first, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
	if err != nil {
		t.Fatalf("first in-flight admission failed: %v", err)
	}
	second, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
	if err != nil {
		t.Fatalf("second in-flight admission failed: %v", err)
	}

	_, err = a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
	if !errors.Is(err, errNoGatewayAccountCapacity) || !gatewayPacingBlocked(err) {
		t.Fatalf("third admission with both slots busy error=%v, want pacing-blocked capacity error", err)
	}

	// One completion frees one slot: the next request enters without waiting
	// for the whole batch.
	a.releaseGatewayAccount(first.ID)
	a.releaseGatewayITPM(first)
	third, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, demand)
	if err != nil {
		t.Fatalf("admission after one slot freed failed: %v", err)
	}
	a.releaseGatewayAccount(third.ID)
	a.releaseGatewayITPM(third)
	a.releaseGatewayAccount(second.ID)
	a.releaseGatewayITPM(second)
}

func TestDispatchStrategyPacingValidationAndPersistence(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()

	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "pacing-invalid-n", "rpm_limit": 8, "rpm_strategy": "fixed",
		"dispatch_pacing": "completion", "pacing_concurrency": 0,
	}, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "pacing-invalid-interval", "rpm_limit": 8, "rpm_strategy": "fixed",
		"dispatch_pacing": "interval", "pacing_concurrency": 2, "pacing_interval_seconds": 0,
	}, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "pacing-invalid-mode", "rpm_limit": 8, "rpm_strategy": "fixed",
		"dispatch_pacing": "burst",
	}, http.StatusBadRequest, nil)

	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "pacing-valid", "rpm_limit": 8, "rpm_strategy": "fixed",
		"dispatch_pacing": "interval", "pacing_concurrency": 3, "pacing_interval_seconds": 10,
	})
	var strategies []dispatchStrategy
	putJSON(t, handler, http.MethodGet, "/api/strategies", nil, http.StatusOK, &strategies)
	found := false
	for _, item := range strategies {
		if item.ID != strategyID {
			continue
		}
		found = true
		if item.DispatchPacing != "interval" || item.PacingConcurrency != 3 || item.PacingIntervalSeconds != 10 {
			t.Fatalf("persisted pacing=%q n=%d interval=%d", item.DispatchPacing, item.PacingConcurrency, item.PacingIntervalSeconds)
		}
	}
	if !found {
		t.Fatal("created pacing strategy missing from the list")
	}
}

func TestSmoothColdStartSpreadsRPMAndStrictlyCapsUncachedInput(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "smooth-cold-start", "rpm_limit": 30, "rpm_strategy": "fixed",
		"capacity_enabled":          true,
		"smooth_cold_start_enabled": true,
		"smooth_cold_start_rpm":     8,
		"smooth_cold_start_tpm":     100_000,
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	created := createGatewayTestAccount(t, a, handler, "smooth-account", "https://smooth.example.test", 0, nil, map[string]any{"access_token": "token"})

	selected, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, gatewayDispatchDemand{EstimatedITPM: 50_000})
	if err != nil {
		t.Fatalf("first smooth admission failed: %v", err)
	}
	if selected.BaseRPM != 8 || selected.RPMStrategy != "fixed" || selected.StickyBuffer != 0 || !selected.ITPMStrictHard || selected.ITPMHardLimit != 100_000 {
		t.Fatalf("effective smooth account = %+v", selected)
	}
	a.releaseGatewayAccount(selected.ID)
	a.releaseGatewayITPM(selected)

	_, err = a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, gatewayDispatchDemand{EstimatedITPM: 1})
	if !errors.Is(err, errNoGatewayAccountCapacity) || !gatewayPacingBlocked(err) {
		t.Fatalf("second request inside the seven-second interval error=%v, want pacing block", err)
	}

	aged := time.Now().UTC().Add(-8 * time.Second).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE account_rpm_events SET created_at = ? WHERE account_id = ?`, aged, created.ID); err != nil {
		t.Fatal(err)
	}
	_, err = a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, gatewayDispatchDemand{EstimatedITPM: 100_001, Oversized: true})
	if !errors.Is(err, errNoGatewayAccountCapacity) {
		t.Fatalf("request above strict smooth ITPM cap error=%v, want capacity error", err)
	}

	selected, err = a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, gatewayDispatchDemand{EstimatedITPM: 100_000})
	if err != nil {
		t.Fatalf("request at strict smooth ITPM cap failed: %v", err)
	}
	a.releaseGatewayAccount(selected.ID)
	a.releaseGatewayITPM(selected)
}

func TestAccountSmoothColdStartOverridesStrategySetting(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "account-smooth", "rpm_limit": 20, "rpm_strategy": "fixed",
		"smooth_cold_start_enabled": false,
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	createGatewayTestAccount(t, a, handler, "account-smooth", "https://account-smooth.example.test", 0, map[string]any{
		"smooth_cold_start_enabled": true,
		"smooth_cold_start_rpm":     6,
		"smooth_cold_start_tpm":     90_000,
	}, map[string]any{"access_token": "token"})

	selected, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, gatewayDispatchDemand{EstimatedITPM: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	defer a.releaseGatewayITPM(selected)
	if selected.BaseRPM != 6 || selected.ITPMHardLimit != 90_000 || !selected.ITPMStrictHard {
		t.Fatalf("account smooth override did not apply: %+v", selected)
	}
}

func TestDisabledStrategyCapacityKeepsAccountLimits(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	createdKey := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(createdKey.Key)
	if err != nil {
		t.Fatal(err)
	}
	strategyID := createTestStrategy(t, handler, map[string]any{
		"name": "capacity-off", "rpm_limit": 1, "tpm_limit": 1, "concurrency_limit": 1,
		"rpm_strategy": "fixed", "dispatch_mode": "concentrated", "capacity_enabled": false,
	})
	bindGroupStrategy(t, handler, strategyID, nil)
	createGatewayTestAccount(t, a, handler, "capacity-off", "https://capacity-off.example.test", 0, nil, map[string]any{"access_token": "token"})

	first, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, gatewayDispatchDemand{EstimatedITPM: 1})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, "", "claude-fable-5", map[int64]bool{}, false, gatewayDispatchDemand{EstimatedITPM: 1})
	if err != nil {
		t.Fatalf("disabled strategy capacity still limited the account: %v", err)
	}
	defer a.releaseGatewayAccount(first.ID)
	defer a.releaseGatewayITPM(first)
	defer a.releaseGatewayAccount(second.ID)
	defer a.releaseGatewayITPM(second)
	if first.BaseRPM != 100 || first.Concurrency != 10 {
		t.Fatalf("strategy capacity leaked into account: %+v", first)
	}
}

// The load snapshot answers "how much concurrent volume was the account
// carrying when it hit 429": tokens and requests in the 60s window, the
// ITPM-relevant uncached input, and the in-flight count.
func TestAccountLoadSnapshotCapturesITPMAndInflight(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	created := createGatewayTestAccount(t, a, handler, "snapshot-account", "https://snap.example.test", 0, nil, map[string]any{"access_token": "token"})
	if _, _, err := a.recordUsage(usageInput{
		RequestID: "snapshot-usage", PurposeKey: "default", GroupID: "a", AccountID: created.ID,
		Model: "claude-fable-5", InputTokens: 1_000, OutputTokens: 200, CacheCreationTokens: 3_000, CacheReadTokens: 50_000,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 4)`, created.ID); err != nil {
		t.Fatal(err)
	}
	snapshot := a.accountLoadSnapshot(created.ID)
	if snapshot.TPM != 54_200 || snapshot.ITPM != 4_000 || snapshot.Inflight != 4 {
		t.Fatalf("snapshot=%+v, want tpm=54200 itpm=4000 inflight=4", snapshot)
	}
}
