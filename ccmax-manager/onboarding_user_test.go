package main

import (
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOnboardingUserPermissionsAndRestrictedGroupSettings(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")
	var created panelUser
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username":          "onboarder",
		"name":              "Onboarding Operator",
		"password":          "onboard-password",
		"role":              roleOnboardingUser,
		"status":            "active",
		"allowed_group_ids": []string{"a"},
		"visible_pages":     panelPages,
		"rpm_limit":         0,
	}, adminCookie, "", http.StatusCreated, &created)
	if created.Role != roleOnboardingUser {
		t.Fatalf("created role = %q", created.Role)
	}
	if want := defaultVisiblePages(roleOnboardingUser); !reflect.DeepEqual(created.VisiblePages, want) {
		t.Fatalf("visible pages = %v, want %v", created.VisiblePages, want)
	}
	var storedRole, userKind string
	if err := a.db.QueryRow(`SELECT role, user_kind FROM users WHERE id = ?`, created.ID).Scan(&storedRole, &userKind); err != nil {
		t.Fatal(err)
	}
	if storedRole != roleUser || userKind != userKindOnboarding {
		t.Fatalf("stored role/kind = %q/%q", storedRole, userKind)
	}

	userCookie := loginCookie(t, handler, "onboarder", "onboard-password")
	requestJSON(t, handler, http.MethodGet, "/api/users", nil, userCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, userCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodGet, "/api/proxies", nil, userCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodGet, "/api/proxy-pools", nil, userCookie, "", http.StatusOK, nil)

	var groups []group
	requestJSON(t, handler, http.MethodGet, "/api/groups", nil, userCookie, "", http.StatusOK, &groups)
	if len(groups) != 1 || groups[0].ID != "a" {
		t.Fatalf("scoped groups = %+v", groups)
	}

	requestJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"rate_multiplier":                    1.25,
		"normal_request_mode":                true,
		"claude_cli_version":                 "2.3.4",
		"reject_anthropic_downgrade_enabled": true,
		"reject_distillation_enabled":        true,
	}, userCookie, "", http.StatusOK, nil)
	updated, err := a.getGroup("a")
	if err != nil {
		t.Fatal(err)
	}
	if updated.RateMultiplier != 1.25 || !updated.NormalRequestMode || updated.ClaudeCLIVersion != "2.3.4" || !updated.RejectAnthropicDowngrade || !updated.RejectDistillation {
		t.Fatalf("restricted group settings were not saved: %+v", updated)
	}
	requestJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"rate_multiplier": -1,
	}, userCookie, "", http.StatusBadRequest, nil)

	requestJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"normal_request_mode": false,
		"name":                "attempted override",
	}, userCookie, "", http.StatusBadRequest, nil)
	requestJSON(t, handler, http.MethodPut, "/api/groups/b", map[string]any{
		"normal_request_mode": false,
	}, userCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodPut, "/api/groups/a/execution", map[string]any{}, userCookie, "", http.StatusForbidden, nil)

	var key apiKeyRecord
	requestJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"user_id":  1,
		"name":     "own-key",
		"group_id": "a",
		"status":   "active",
		"quota":    0,
	}, userCookie, "", http.StatusCreated, &key)
	if key.UserID != created.ID || key.Key == "" {
		t.Fatalf("generated key owner/key = %d/%q", key.UserID, key.Key)
	}

	requestJSON(t, handler, http.MethodPost, "/api/accounts/batch-authorize", map[string]any{
		"proxy_pool_id": 1,
		"group_ids":     []string{"a"},
		"session_keys":  []string{},
		"account_price": 0,
	}, userCookie, "", http.StatusBadRequest, nil)
	requestJSON(t, handler, http.MethodPost, "/api/accounts/batch-delete", map[string]any{
		"ids": []int64{1},
	}, userCookie, "", http.StatusForbidden, nil)
}

func TestApplyOnboardingBatchRules(t *testing.T) {
	strategyID := int64(99)
	disabled := false
	percent := 12
	input := batchAuthorizationInput{
		GroupIDs:                []string{"a"},
		StrategyID:              &strategyID,
		Quota5HThresholdEnabled: &disabled,
		Quota5HThresholdPercent: &percent,
		Quota7DThresholdEnabled: &disabled,
		Quota7DThresholdPercent: &percent,
	}
	user := panelUser{Role: roleOnboardingUser, AllowedGroupIDs: []string{"a"}}
	if err := applyOnboardingBatchRules(user, &input); err != nil {
		t.Fatal(err)
	}
	if input.StrategyID != nil {
		t.Fatalf("strategy was not cleared: %v", *input.StrategyID)
	}
	if !*input.Quota5HThresholdEnabled || *input.Quota5HThresholdPercent != 95 || !*input.Quota7DThresholdEnabled || *input.Quota7DThresholdPercent != 95 {
		t.Fatalf("quota defaults = %+v", input)
	}

	input.GroupIDs = []string{"b"}
	if err := applyOnboardingBatchRules(user, &input); err == nil {
		t.Fatal("disallowed group was accepted")
	}
}
