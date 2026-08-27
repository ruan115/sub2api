package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

func TestDispatchStrategyITPMProtectionConfiguration(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "strategy-itpm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var created struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "custom-itpm", "rpm_limit": 8, "rpm_strategy": "fixed",
		"itpm_protection_enabled": false,
		"itpm_window_seconds":     45,
		"itpm_soft_limit":         120_000,
		"itpm_hard_limit":         180_000,
	}, http.StatusCreated, &created)

	read := func() dispatchStrategy {
		t.Helper()
		var strategies []dispatchStrategy
		putJSON(t, handler, http.MethodGet, "/api/strategies", nil, http.StatusOK, &strategies)
		for _, strategy := range strategies {
			if strategy.ID == created.ID {
				return strategy
			}
		}
		t.Fatalf("strategy %d not found", created.ID)
		return dispatchStrategy{}
	}
	strategy := read()
	if strategy.ITPMProtectionEnabled || strategy.ITPMWindowSeconds != 45 || strategy.ITPMSoftLimit != 120_000 || strategy.ITPMHardLimit != 180_000 {
		t.Fatalf("strategy ITPM config = %+v", strategy)
	}

	// Older clients omit the new fields. Updating an unrelated limit must keep
	// the existing ITPM protection configuration intact.
	putJSON(t, handler, http.MethodPut, fmt.Sprintf("/api/strategies/%d", created.ID), map[string]any{
		"name": "custom-itpm-renamed", "rpm_limit": 9, "rpm_strategy": "fixed",
	}, http.StatusOK, nil)
	strategy = read()
	if strategy.ITPMProtectionEnabled || strategy.ITPMWindowSeconds != 45 || strategy.ITPMSoftLimit != 120_000 || strategy.ITPMHardLimit != 180_000 {
		t.Fatalf("legacy update changed ITPM config: %+v", strategy)
	}

	putJSON(t, handler, http.MethodPut, fmt.Sprintf("/api/strategies/%d", created.ID), map[string]any{
		"name": "invalid-itpm", "rpm_limit": 9, "rpm_strategy": "fixed",
		"itpm_protection_enabled": true,
		"itpm_window_seconds":     60,
		"itpm_soft_limit":         150_000,
		"itpm_hard_limit":         150_000,
	}, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodPut, fmt.Sprintf("/api/strategies/%d", created.ID), map[string]any{
		"name": "invalid-partial-itpm", "rpm_limit": 9, "rpm_strategy": "fixed",
		"itpm_hard_limit": 100_000,
	}, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "invalid-default-merge", "rpm_limit": 8, "rpm_strategy": "fixed",
		"itpm_soft_limit": 200_000,
	}, http.StatusBadRequest, nil)
}
