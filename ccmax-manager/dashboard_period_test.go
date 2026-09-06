package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
)

func TestDashboardOnboardingPeriodIsolation(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	accountResult, err := a.db.Exec(`INSERT INTO accounts (name) VALUES ('shared-account')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := accountResult.LastInsertId()
	ownerResult, err := a.db.Exec(`INSERT INTO users (username, name, password_hash, role, user_kind, allowed_group_ids_json, visible_pages_json) VALUES ('uploader', 'Uploader', 'test-only', 'user', 'onboarding', '["a"]', '["overview"]')`)
	if err != nil {
		t.Fatal(err)
	}
	ownerID, _ := ownerResult.LastInsertId()
	for i := 0; i < 2; i++ {
		var userID any
		if i == 0 {
			userID = ownerID
		}
		if _, err := a.db.Exec(`INSERT INTO usage_logs (request_id, purpose_key, purpose_name, group_id, account_id, account_name, user_id, model,
			input_tokens, billed_cost, actual_cost, base_cost, created_at) VALUES (?, 'default', 'Default', 'a', ?, 'shared-account', ?, 'claude-test', 100, 3, 2, 1, '2025-01-01T00:00:00Z')`, i, accountID, userID); err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"", "?from=2025-01-01&to=2025-01-01&user_id=1"} {
		r := httptest.NewRequest(http.MethodGet, "/api/dashboard"+query, nil)
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, panelUser{ID: ownerID, Role: roleOnboardingUser, AllowedGroupIDs: []string{"a"}}))
		w := httptest.NewRecorder()
		a.handleDashboard(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var result dashboard
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Period.Totals.Requests != 1 || result.Period.Totals.BilledCost != 3 || result.Period.Totals.ActualCost != 0 || result.Period.Totals.Margin != 0 || result.Period.Totals.BaseCost != 0 || result.Period.ByGroup["a"].ActualCost != 0 || len(result.RecentUsage) != 1 {
			t.Fatalf("unscoped onboarding dashboard=%+v", result)
		}
	}
}

func TestDashboardFilters(t *testing.T) {
	for _, tc := range []struct {
		name, from, to, wantFrom, wantTo string
		invalid                          bool
	}{
		{name: "all time"},
		{name: "Shanghai dates", from: "2026-08-01", to: "2026-08-31", wantFrom: "2026-07-31T16:00:00Z", wantTo: "2026-08-31T16:00:00Z"},
		{name: "inclusive minute", from: "2026-08-26T21:00", to: "2026-08-26T21:00", wantFrom: "2026-08-26T13:00:00Z", wantTo: "2026-08-26T13:01:00Z"},
		{name: "explicit offset", from: "2026-08-26T01:00:00-07:00", wantFrom: "2026-08-26T08:00:00Z"},
		{name: "open start", to: "2026-08-26", wantTo: "2026-08-26T16:00:00Z"},
		{name: "invalid date", from: "2026-02-30", invalid: true},
		{name: "invalid end", to: "invalid", invalid: true},
		{name: "reversed", from: "2026-08-27", to: "2026-08-25", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := url.Values{"from": {tc.from}, "to": {tc.to}}
			filters, err := dashboardFilters(httptest.NewRequest(http.MethodGet, "/api/dashboard?"+params.Encode(), nil))
			if (err != nil) != tc.invalid {
				t.Fatalf("filters=%+v error=%v", filters, err)
			}
			if !tc.invalid && (filters.From != tc.wantFrom || filters.To != tc.wantTo) {
				t.Fatalf("filters=%+v, want %s to %s", filters, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

func TestDashboardAllTimeAndMinuteRange(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	res, err := a.db.Exec(`INSERT INTO accounts (name, archived_at, deleted_at) VALUES ('historical-account', '2026-08-27T00:00:00Z', '2026-08-28T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for i, row := range []struct{ at, group string }{
		{"2025-01-01T00:00:00Z", "a"},
		{"2026-08-26T12:59:59Z", "a"},
		{"2026-08-26T13:00:00Z", "a"},
		{"2026-08-26T13:00:59.999Z", "b"},
		{"2026-08-26T13:01:00Z", "b"},
	} {
		_, err := a.db.Exec(`INSERT INTO usage_logs (request_id, purpose_key, purpose_name, group_id, account_id, account_name, model,
			input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, base_cost, billed_cost, actual_cost, created_at)
			VALUES (?, 'default', 'Default', ?, ?, 'historical-account', 'claude-test', 10, 20, 30, 40, 1, 3, 2, ?)`, i, row.group, id, row.at)
		if err != nil {
			t.Fatal(err)
		}
	}
	var all dashboard
	putJSON(t, a.routes(), http.MethodGet, "/api/dashboard", nil, http.StatusOK, &all)
	if all.Period.Totals.Requests != 5 || all.AccountsTotal != 0 || len(all.RecentUsage) != 5 {
		t.Fatalf("all-time dashboard=%+v", all)
	}
	assertClose(t, all.Period.Totals.BilledCost, 15)
	assertClose(t, all.Period.Totals.ActualCost, 10)
	assertClose(t, all.Period.Totals.Margin, 5)
	if all.Period.FirstUsageAt != "2025-01-01T00:00:00Z" || all.Period.ByGroup["a"].Requests != 3 || all.Period.Totals.CacheTokens != 350 {
		t.Fatalf("all-time period=%+v", all.Period)
	}
	var selected dashboard
	putJSON(t, a.routes(), http.MethodGet, "/api/dashboard?from=2026-08-26T21:00&to=2026-08-26T21:00", nil, http.StatusOK, &selected)
	if selected.Period.Totals.Requests != 2 || len(selected.RecentUsage) != 2 || selected.Period.ByGroup["b"].Requests != 1 {
		t.Fatalf("minute dashboard=%+v", selected)
	}
	assertClose(t, selected.Period.Totals.BilledCost, 6)
	assertClose(t, selected.Period.Totals.Margin, 2)
	var empty dashboard
	putJSON(t, a.routes(), http.MethodGet, "/api/dashboard?from=2024-01-01&to=2024-01-31", nil, http.StatusOK, &empty)
	if empty.Period.Totals.Requests != 0 || len(empty.RecentUsage) != 0 || len(empty.Period.ByGroup) != 0 {
		t.Fatalf("empty dashboard=%+v", empty)
	}
	putJSON(t, a.routes(), http.MethodGet, "/api/dashboard?from=bad-date", nil, http.StatusBadRequest, nil)
	putJSON(t, a.routes(), http.MethodGet, "/api/dashboard?from=2026-09-01&to=2026-08-01", nil, http.StatusBadRequest, nil)
}
