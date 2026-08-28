package main

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAccountStrategyFilterUsesEffectiveGroupBinding(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "account-strategy-filter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	createStrategy := func(name string) int64 {
		var strategy struct {
			ID int64 `json:"id"`
		}
		putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
			"name": name, "rpm_limit": 0, "tpm_limit": 0, "concurrency_limit": 0,
			"rpm_strategy": "fixed", "rpm_sticky_buffer": 0, "dispatch_mode": "",
		}, http.StatusCreated, &strategy)
		return strategy.ID
	}
	firstStrategyID := createStrategy("filter-first")
	secondStrategyID := createStrategy("filter-second")
	if _, err := a.db.Exec(`UPDATE groups SET strategy_id = CASE id WHEN 'a' THEN ? WHEN 'b' THEN ? ELSE strategy_id END WHERE id IN ('a', 'b')`, firstStrategyID, secondStrategyID); err != nil {
		t.Fatal(err)
	}

	createAccount := func(name string, groups []string, strategyID any) account {
		payload := map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"status": "active", "schedulable": false, "concurrency": 1,
			"priority": 10, "rate_multiplier": 1, "group_ids": groups,
			"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
		}
		if strategyID != nil {
			payload["strategy_id"] = strategyID
		}
		var created account
		putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusCreated, &created)
		return created
	}
	inherited := createAccount("inherits-by-group", []string{"a", "b"}, nil)
	explicit := createAccount("explicit-strategy", []string{"a"}, secondStrategyID)

	type accountPage struct {
		Items []account `json:"items"`
		Total int64     `json:"total"`
	}
	load := func(path string) accountPage {
		var page accountPage
		putJSON(t, handler, http.MethodGet, path, nil, http.StatusOK, &page)
		return page
	}

	page := load("/api/accounts?paginated=1&strategy_id=" + stringInt64(firstStrategyID))
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != inherited.ID {
		t.Fatalf("first strategy page = %+v", page)
	}
	page = load("/api/accounts?paginated=1&group_id=a&strategy_id=" + stringInt64(secondStrategyID))
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != explicit.ID {
		t.Fatalf("explicit strategy page = %+v", page)
	}
	page = load("/api/accounts?paginated=1&group_id=b&strategy_id=" + stringInt64(firstStrategyID))
	if page.Total != 0 {
		t.Fatalf("group-specific inherited strategy leaked from another group: %+v", page)
	}
	page = load("/api/accounts?paginated=1&group_id=b&strategy_id=" + stringInt64(secondStrategyID))
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != inherited.ID {
		t.Fatalf("second group inherited strategy page = %+v", page)
	}

	var summary accountSummary
	putJSON(t, handler, http.MethodGet, "/api/accounts/summary?strategy_id="+stringInt64(firstStrategyID), nil, http.StatusOK, &summary)
	if summary.Accounts != 1 {
		t.Fatalf("filtered summary accounts = %d, want 1", summary.Accounts)
	}

	var options []accountStrategyOption
	putJSON(t, handler, http.MethodGet, "/api/accounts/strategy-options", nil, http.StatusOK, &options)
	if len(options) != 2 || options[0].ID != firstStrategyID || options[1].ID != secondStrategyID {
		t.Fatalf("strategy options = %+v", options)
	}

	putJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&strategy_id=invalid", nil, http.StatusBadRequest, nil)
}

func stringInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
