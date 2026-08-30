package main

import (
	"net/http"
	"path/filepath"
	"reflect"
	"strconv"
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
	visiblePages := []string{"overview", "accounts", "onboarding", "proxies", "access"}
	var created panelUser
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username":          "onboarder",
		"name":              "Onboarding Operator",
		"password":          "onboard-password",
		"role":              roleOnboardingUser,
		"status":            "active",
		"allowed_group_ids": []string{"a"},
		"visible_pages":     visiblePages,
		"rpm_limit":         0,
	}, adminCookie, "", http.StatusCreated, &created)
	if created.Role != roleOnboardingUser {
		t.Fatalf("created role = %q", created.Role)
	}
	if want := visiblePages; !reflect.DeepEqual(created.VisiblePages, want) {
		t.Fatalf("visible pages = %v, want %v", created.VisiblePages, want)
	}
	if !reflect.DeepEqual(created.AccountView, defaultAccountView(roleOnboardingUser)) {
		t.Fatalf("onboarding account view should default to all enabled: %+v", created.AccountView)
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
	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, userCookie, "", http.StatusOK, nil)
	requestJSON(t, handler, http.MethodGet, "/api/proxies", nil, userCookie, "", http.StatusOK, nil)
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
		"ids": []int64{999999},
	}, userCookie, "", http.StatusForbidden, nil)

	requestJSON(t, handler, http.MethodPut, "/api/users/"+strconv.FormatInt(created.ID, 10), map[string]any{
		"username":          "onboarder",
		"name":              "Onboarding Operator",
		"password":          "",
		"role":              roleOnboardingUser,
		"status":            "active",
		"allowed_group_ids": []string{"a"},
		"visible_pages":     []string{"daily", "access"},
		"rpm_limit":         0,
	}, adminCookie, "", http.StatusOK, &created)
	if want := []string{"daily", "access"}; !reflect.DeepEqual(created.VisiblePages, want) {
		t.Fatalf("updated visible pages = %v, want %v", created.VisiblePages, want)
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, userCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodGet, "/api/proxies", nil, userCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodGet, "/api/stats/daily", nil, userCookie, "", http.StatusOK, nil)
	requestJSON(t, handler, http.MethodPost, "/api/accounts/batch-authorize", map[string]any{}, userCookie, "", http.StatusForbidden, nil)
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

func TestOnboardingUserCanReadEveryConfiguredPage(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "configured-pages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username":          "all-pages-onboarder",
		"name":              "All Pages Onboarder",
		"password":          "onboard-password",
		"role":              roleOnboardingUser,
		"status":            "active",
		"allowed_group_ids": []string{"a"},
		"visible_pages":     panelPages,
		"rpm_limit":         0,
	}, adminCookie, "", http.StatusCreated, nil)
	strategyAResult, err := a.db.Exec(`INSERT INTO dispatch_strategies (name) VALUES ('strategy-a')`)
	if err != nil {
		t.Fatal(err)
	}
	strategyAID, _ := strategyAResult.LastInsertId()
	strategyBResult, err := a.db.Exec(`INSERT INTO dispatch_strategies (name) VALUES ('strategy-b')`)
	if err != nil {
		t.Fatal(err)
	}
	strategyBID, _ := strategyBResult.LastInsertId()
	if _, err := a.db.Exec(`UPDATE groups SET strategy_id = CASE id WHEN 'a' THEN ? WHEN 'b' THEN ? END WHERE id IN ('a', 'b')`, strategyAID, strategyBID); err != nil {
		t.Fatal(err)
	}
	accountAResult, err := a.db.Exec(`INSERT INTO accounts (name, status, schedulable) VALUES ('visible-a', 'error', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	accountAID, _ := accountAResult.LastInsertId()
	accountBResult, err := a.db.Exec(`INSERT INTO accounts (name, status, schedulable) VALUES ('hidden-b', 'error', 0)`)
	if err != nil {
		t.Fatal(err)
	}
	accountBID, _ := accountBResult.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO account_groups (account_id, group_id) VALUES (?, 'a'), (?, 'b')`, accountAID, accountBID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO gateway_error_logs (request_id, account_id, group_id, status_code, category, method, path, message)
		VALUES ('visible-error-a', ?, 'a', 401, 'upstream', 'POST', '/v1/messages', 'visible a'),
		       ('hidden-error-b', ?, 'b', 401, 'upstream', 'POST', '/v1/messages', 'hidden b')`, accountAID, accountBID); err != nil {
		t.Fatal(err)
	}

	userCookie := loginCookie(t, handler, "all-pages-onboarder", "onboard-password")
	paths := []string{
		"/api/me",
		"/api/dashboard",
		"/api/groups",
		"/api/purposes",
		"/api/accounts?paginated=1",
		"/api/accounts/summary",
		"/api/accounts/strategy-options",
		"/api/stats/realtime",
		"/api/proxy-pools",
		"/api/proxies",
		"/api/proxies/archived",
		"/api/api-keys",
		"/api/prices",
		"/api/prices/sync-status",
		"/api/billing",
		"/api/usage",
		"/api/stats/daily",
		"/api/authorization-logs",
		"/api/authorization-deauth",
		"/api/error-logs",
		"/api/error-insights",
		"/api/strategies/observe",
		"/api/audit-logs",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			requestJSON(t, handler, http.MethodGet, path, nil, userCookie, "", http.StatusOK, nil)
		})
	}
	var strategyOptions []accountStrategyOption
	requestJSON(t, handler, http.MethodGet, "/api/accounts/strategy-options", nil, userCookie, "", http.StatusOK, &strategyOptions)
	if len(strategyOptions) != 2 || strategyOptions[0].ID != strategyAID || strategyOptions[1].ID != strategyBID {
		t.Fatalf("managed strategy options = %+v", strategyOptions)
	}
	var observations []strategyObservation
	requestJSON(t, handler, http.MethodGet, "/api/strategies/observe", nil, userCookie, "", http.StatusOK, &observations)
	if len(observations) != 1 || observations[0].ID != strategyAID || observations[0].BoundGroups != 1 {
		t.Fatalf("scoped strategy observations = %+v", observations)
	}
	var errors errorLogResponse
	requestJSON(t, handler, http.MethodGet, "/api/error-logs?source=gateway", nil, userCookie, "", http.StatusOK, &errors)
	if errors.Summary.Total != 1 || len(errors.Items) != 1 || errors.Items[0].RequestID != "visible-error-a" {
		t.Fatalf("scoped error logs = %+v", errors)
	}
	var insights errorInsightResponse
	requestJSON(t, handler, http.MethodGet, "/api/error-insights?status=401", nil, userCookie, "", http.StatusOK, &insights)
	if len(insights.Accounts) != 1 || insights.Accounts[0].AccountID != accountAID {
		t.Fatalf("scoped error insights = %+v", insights)
	}
	requestJSON(t, handler, http.MethodPost, "/api/accounts/batch-schedule", map[string]any{
		"ids":         []int64{accountAID},
		"schedulable": false,
	}, userCookie, "", http.StatusOK, nil)
	requestJSON(t, handler, http.MethodPost, "/api/accounts/batch-schedule", map[string]any{
		"ids":         []int64{accountAID, accountBID},
		"schedulable": false,
	}, userCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodPost, "/api/accounts/"+strconv.FormatInt(accountBID, 10)+"/quota/refresh", map[string]any{}, userCookie, "", http.StatusNotFound, nil)
	requestJSON(t, handler, http.MethodGet, "/api/users", nil, userCookie, "", http.StatusForbidden, nil)
}

func TestOnboardingVisiblePagesGrantMatchingWritePermissions(t *testing.T) {
	user := panelUser{Role: roleOnboardingUser, VisiblePages: panelPages}
	tests := []struct {
		method string
		path   string
	}{
		{http.MethodPut, "/api/groups/a"},
		{http.MethodPost, "/api/accounts"},
		{http.MethodPost, "/api/accounts/batch-authorize"},
		{http.MethodPost, "/api/accounts/batch-update"},
		{http.MethodPost, "/api/accounts/1/quota/refresh"},
		{http.MethodPost, "/api/proxy-pools"},
		{http.MethodPost, "/api/proxies/batch"},
		{http.MethodPost, "/api/strategies"},
		{http.MethodPost, "/api/prices"},
		{http.MethodPost, "/api/usage"},
	}
	for _, test := range tests {
		if !onboardingUserCanWrite(user, test.path, test.method) {
			t.Errorf("%s %s should be writable", test.method, test.path)
		}
	}
	for _, test := range tests {
		user.VisiblePages = []string{"access"}
		if onboardingUserCanWrite(user, test.path, test.method) {
			t.Errorf("%s %s should be denied when its page is hidden", test.method, test.path)
		}
	}
	if onboardingUserCanWrite(panelUser{Role: roleOnboardingUser, VisiblePages: panelPages}, "/api/users", http.MethodPost) {
		t.Fatal("onboarding user management must stay forbidden")
	}
	if onboardingUserCanWrite(panelUser{Role: roleOnboardingUser, VisiblePages: panelPages}, "/api/pool/resolve", http.MethodPost) {
		t.Fatal("runtime account resolution must stay restricted to administrators")
	}
}
