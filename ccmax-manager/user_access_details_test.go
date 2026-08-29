package main

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
)

func TestArchivedUserKeyIsHiddenFromOwnerAndVisibleInAdminLedger(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")

	var user panelUser
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "ledger-user", "name": "Ledger User", "password": "ledger-password",
		"role": "user", "status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, &user)
	var key apiKeyRecord
	requestJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"user_id": user.ID, "name": "archived-caller", "group_id": "a", "status": "active", "quota": 20,
	}, adminCookie, "", http.StatusCreated, &key)
	if key.Key == "" {
		t.Fatal("created key secret is empty")
	}

	accountResult, err := a.db.Exec(`INSERT INTO accounts (name, schedulable) VALUES ('ledger-upstream', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := accountResult.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO usage_logs (
		user_id, api_key_id, request_id, purpose_key, purpose_name, group_id,
		account_id, account_name, model, input_tokens, output_tokens, billed_cost, actual_cost
	) VALUES (?, ?, 'user-ledger-request', 'default', '默认用途', 'a', ?, 'ledger-upstream', 'claude-test', 120, 30, 2.5, 1.25)`, user.ID, key.ID, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE api_keys SET quota_used = 2.5 WHERE id = ?`, key.ID); err != nil {
		t.Fatal(err)
	}

	userCookie := loginCookie(t, handler, "ledger-user", "ledger-password")
	requestJSON(t, handler, http.MethodDelete, "/api/api-keys/"+strconv.FormatInt(key.ID, 10), nil, userCookie, "", http.StatusNoContent, nil)
	if _, err := a.authenticateGatewayKey(key.Key); err == nil {
		t.Fatal("archived key remained usable")
	}

	var ownerKeys []apiKeyRecord
	requestJSON(t, handler, http.MethodGet, "/api/api-keys", nil, userCookie, "", http.StatusOK, &ownerKeys)
	if len(ownerKeys) != 0 {
		t.Fatalf("owner can still see archived keys: %+v", ownerKeys)
	}
	requestJSON(t, handler, http.MethodGet, "/api/api-keys?include_archived=true", nil, userCookie, "", http.StatusOK, &ownerKeys)
	if len(ownerKeys) != 0 {
		t.Fatalf("owner bypassed archive visibility: %+v", ownerKeys)
	}
	requestJSON(t, handler, http.MethodGet, "/api/users/"+strconv.FormatInt(user.ID, 10)+"/access-details", nil, userCookie, "", http.StatusForbidden, nil)

	var users []panelUser
	requestJSON(t, handler, http.MethodGet, "/api/users", nil, adminCookie, "", http.StatusOK, &users)
	var summary *panelUser
	for index := range users {
		if users[index].ID == user.ID {
			summary = &users[index]
			break
		}
	}
	if summary == nil {
		t.Fatal("user summary is missing")
	}
	if summary.Consumed != 2.5 || summary.UsageRequests != 1 || summary.ActiveKeyCount != 0 || summary.ArchivedKeyCount != 1 {
		t.Fatalf("user summary = %+v", *summary)
	}

	var details userAccessDetails
	requestJSON(t, handler, http.MethodGet, "/api/users/"+strconv.FormatInt(user.ID, 10)+"/access-details", nil, adminCookie, "", http.StatusOK, &details)
	if details.UsageTotal != 1 || len(details.Usage) != 1 || details.Totals.BilledCost != 2.5 {
		t.Fatalf("usage details = %+v", details)
	}
	if len(details.Keys) != 1 || !details.Keys[0].Archived || details.Keys[0].Key != key.Key {
		t.Fatalf("archived key details = %+v", details.Keys)
	}
	if details.Keys[0].UsageRequests != 1 || details.Keys[0].UsageBilledCost != 2.5 {
		t.Fatalf("archived key usage = %+v", details.Keys[0])
	}

	var adminArchived []apiKeyRecord
	requestJSON(t, handler, http.MethodGet, "/api/api-keys?include_archived=true&user_id="+strconv.FormatInt(user.ID, 10), nil, adminCookie, "", http.StatusOK, &adminArchived)
	if len(adminArchived) != 1 || adminArchived[0].Key != key.Key || !adminArchived[0].Archived {
		t.Fatalf("admin archive listing = %+v", adminArchived)
	}
}

func TestCurrentUserAccessSummaryIncludesConsumedBalanceButHidesArchiveCount(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")

	var user panelUser
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "balance-user", "name": "Balance User", "password": "balance-password",
		"role": "user", "status": "active", "allowed_group_ids": []string{"a"}, "balance": 8.0, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, &user)
	if _, err := a.db.Exec(`INSERT INTO api_keys (user_id, key_hash, key_prefix, key_secret, name, group_id, status, deleted_at) VALUES (?, 'archived-hash', 'sk-archive', 'sk-archive-secret', 'old', 'a', 'disabled', `+nowSQL+`)`, user.ID); err != nil {
		t.Fatal(err)
	}
	accountResult, err := a.db.Exec(`INSERT INTO accounts (name, schedulable) VALUES ('balance-upstream', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := accountResult.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO usage_logs (user_id, request_id, purpose_key, purpose_name, group_id, account_id, account_name, model, billed_cost) VALUES (?, 'balance-request', 'default', '默认用途', 'a', ?, 'balance-upstream', 'claude-test', 3.25)`, user.ID, accountID); err != nil {
		t.Fatal(err)
	}

	userCookie := loginCookie(t, handler, "balance-user", "balance-password")
	var me panelUser
	requestJSON(t, handler, http.MethodGet, "/api/me", nil, userCookie, "", http.StatusOK, &me)
	if me.Balance == nil || *me.Balance != 8 || me.Consumed != 3.25 || me.UsageRequests != 1 {
		t.Fatalf("current user summary = %+v", me)
	}
	if me.ArchivedKeyCount != 0 {
		t.Fatalf("ordinary user received archived key count: %+v", me)
	}
}
