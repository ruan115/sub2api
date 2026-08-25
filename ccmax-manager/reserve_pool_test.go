package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReservePoolActivatesOneAccountWhenTargetHasNoCapacity(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	keyRecord := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(keyRecord.Key)
	if err != nil {
		t.Fatal(err)
	}
	reserve := createReserveTestGroup(t, handler)
	account := createGatewayTestAccount(t, a, handler, "reserve-capacity", "https://reserve.example.test", 0, nil, map[string]any{"access_token": "reserve-token"})
	moveTestAccountToGroup(t, a, account.ID, reserve.ID)

	selected, err := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.releaseGatewayAccount(selected.ID)
	if selected.ID != account.ID {
		t.Fatalf("selected account=%d, want reserve account %d", selected.ID, account.ID)
	}
	assertReserveActivation(t, a, account.ID, reserve.ID, "a", "capacity")
}

func TestReservePoolActivationTracksObservedConcurrencyDeficit(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	keyRecord := createGatewayTestKey(t, handler)
	key, err := a.authenticateGatewayKey(keyRecord.Key)
	if err != nil {
		t.Fatal(err)
	}
	reserve := createReserveTestGroup(t, handler)
	first := createGatewayTestAccount(t, a, handler, "reserve-demand-first", "https://first.example.test", 0, nil, map[string]any{"access_token": "first-token"})
	second := createGatewayTestAccount(t, a, handler, "reserve-demand-second", "https://second.example.test", 1, nil, map[string]any{"access_token": "second-token"})
	for _, accountID := range []int64{first.ID, second.ID} {
		if _, err := a.db.Exec(`UPDATE accounts SET concurrency = 2, base_rpm = 100 WHERE id = ?`, accountID); err != nil {
			t.Fatal(err)
		}
		moveTestAccountToGroup(t, a, accountID, reserve.ID)
	}

	selected := make([]gatewayAccount, 0, 3)
	for range 3 {
		account, acquireErr := a.acquireGatewayAccount(key, "", "claude-test", map[int64]bool{})
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		selected = append(selected, account)
	}
	for _, account := range selected {
		a.releaseGatewayAccount(account.ID)
	}
	if selected[0].ID != first.ID || selected[1].ID != first.ID || selected[2].ID != second.ID {
		t.Fatalf("demand activation selections=%d/%d/%d, want %d/%d/%d", selected[0].ID, selected[1].ID, selected[2].ID, first.ID, first.ID, second.ID)
	}
	var activationCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM reserve_activation_logs WHERE target_group_id = 'a'`).Scan(&activationCount); err != nil {
		t.Fatal(err)
	}
	if activationCount != 2 {
		t.Fatalf("activation count=%d, want exactly the 2 observed capacity slots", activationCount)
	}
}

func TestReservePoolActivatesOn429OnlyWhenNoTargetFailoverExists(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"limited"}}`))
	}))
	defer limited.Close()
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_reserve","type":"message","model":"claude-test","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer healthy.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "limited-active", limited.URL, 0, nil, map[string]any{"access_token": "limited-token"})
	reserve := createReserveTestGroup(t, handler)
	backup := createGatewayTestAccount(t, a, handler, "reserve-healthy", healthy.URL, 1, nil, map[string]any{"access_token": "healthy-token"})
	moveTestAccountToGroup(t, a, backup.ID, reserve.ID)

	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 32,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}, nil, key.Key, http.StatusOK, nil)
	assertReserveActivation(t, a, backup.ID, reserve.ID, "a", "rate_limit")
}

func TestReservePoolCannotReceiveAPIKeysOrMixedAccountMembership(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	reserve := createReserveTestGroup(t, handler)
	var user panelUser
	putJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "reserve-user", "name": "Reserve User", "password": "reserve-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{reserve.ID}, "rpm_limit": 10,
	}, http.StatusCreated, &user)
	putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"user_id": user.ID, "name": "invalid-reserve-key", "group_id": reserve.ID, "status": "active", "quota": 0,
	}, http.StatusForbidden, nil)

	proxyID := createTestForwardProxy(t, a)
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "mixed-reserve-account", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "status": "active", "schedulable": true,
		"concurrency": 1, "priority": 10, "rate_multiplier": 1, "group_ids": []string{"a", reserve.ID},
		"proxy_pool_id": 1, "proxy_id": proxyID, "base_rpm": 10, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusBadRequest, nil)
}

func createReserveTestGroup(t *testing.T, handler http.Handler) group {
	t.Helper()
	var reserve group
	putJSON(t, handler, http.MethodPost, "/api/groups", map[string]any{
		"name": "储备账号池", "description": "按需补号", "rate_multiplier": 1,
		"status": "active", "reserve_pool_enabled": true,
	}, http.StatusCreated, &reserve)
	if !reserve.ReservePoolEnabled {
		t.Fatal("created group is not marked as reserve pool")
	}
	return reserve
}

func moveTestAccountToGroup(t *testing.T, a *app, accountID int64, groupID string) {
	t.Helper()
	tx, err := a.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := setAccountGroups(tx, accountID, []string{groupID}, 50); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func assertReserveActivation(t *testing.T, a *app, accountID int64, sourceGroupID, targetGroupID, reason string) {
	t.Helper()
	var currentGroup string
	if err := a.db.QueryRow(`SELECT group_id FROM account_groups WHERE account_id = ?`, accountID).Scan(&currentGroup); err != nil {
		t.Fatal(err)
	}
	if currentGroup != targetGroupID {
		t.Fatalf("account group=%q, want %q", currentGroup, targetGroupID)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM reserve_activation_logs WHERE account_id = ? AND source_group_id = ? AND target_group_id = ? AND reason = ?`, accountID, sourceGroupID, targetGroupID, reason).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reserve activation log count=%d, want 1", count)
	}
}
