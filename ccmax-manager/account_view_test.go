package main

import (
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRestrictedAccountViewFiltersAndProjectsData(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "account-view.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")

	createAccount := func(name string) account {
		proxyID := createTestForwardProxy(t, a)
		var created account
		requestJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": "token-" + name}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 2, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
		}, adminCookie, "", http.StatusCreated, &created)
		return created
	}
	normalAccount := createAccount("visible-normal")
	errorAccount := createAccount("hidden-error")
	if _, err := a.db.Exec(`UPDATE accounts SET status = 'error', auth_status = 'reauth_required', auth_error = 'test error' WHERE id = ?`, errorAccount.ID); err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{"user", "readonly_admin"} {
		t.Run(role, func(t *testing.T) {
			username := "account-view-" + role
			var created panelUser
			requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
				"username": username, "name": role, "password": "viewer-password", "role": role,
				"status": "active", "allowed_group_ids": []string{"a"}, "visible_pages": []string{"accounts"},
				"rpm_limit": 0,
				"account_view": map[string]any{
					"columns": []string{"tpm", "subscription"},
					"blocks":  []string{"queue", "tokens"},
				},
			}, adminCookie, "", http.StatusCreated, &created)
			if !reflect.DeepEqual(created.AccountView.Columns, []string{"subscription", "tpm"}) || !reflect.DeepEqual(created.AccountView.Blocks, []string{"tokens", "queue"}) {
				t.Fatalf("normalized account view = %+v", created.AccountView)
			}

			cookie := loginCookie(t, handler, username, "viewer-password")
			var page struct {
				Items []map[string]any `json:"items"`
				Total int64            `json:"total"`
			}
			requestJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&status=error", nil, cookie, "", http.StatusOK, &page)
			if page.Total != 1 || len(page.Items) != 1 || int64(page.Items[0]["id"].(float64)) != normalAccount.ID {
				t.Fatalf("restricted account page = %+v", page)
			}
			for _, forbidden := range []string{"name", "status", "group_ids", "quota_5h_utilization", "request_count", "total_billed_cost"} {
				if _, leaked := page.Items[0][forbidden]; leaked {
					t.Fatalf("restricted account item leaked %q: %+v", forbidden, page.Items[0])
				}
			}
			if _, ok := page.Items[0]["subscription_type"]; !ok {
				t.Fatalf("configured subscription column missing: %+v", page.Items[0])
			}

			var summary map[string]any
			requestJSON(t, handler, http.MethodGet, "/api/accounts/summary?status=error", nil, cookie, "", http.StatusOK, &summary)
			for _, expected := range []string{"input_tokens", "output_tokens"} {
				if _, ok := summary[expected]; !ok {
					t.Fatalf("configured summary field %q missing: %+v", expected, summary)
				}
			}
			for _, forbidden := range []string{"accounts", "billed_cost", "actual_cost"} {
				if _, leaked := summary[forbidden]; leaked {
					t.Fatalf("restricted summary leaked %q: %+v", forbidden, summary)
				}
			}

			var realtime map[string]any
			requestJSON(t, handler, http.MethodGet, "/api/stats/realtime", nil, cookie, "", http.StatusOK, &realtime)
			if _, ok := realtime["waiting_requests"]; !ok {
				t.Fatalf("configured queue block missing: %+v", realtime)
			}
			if _, ok := realtime["accounts"]; !ok {
				t.Fatalf("configured TPM column missing realtime rows: %+v", realtime)
			}
			for _, forbidden := range []string{"rpm", "rpm_capacity", "inflight", "concurrency_capacity"} {
				if _, leaked := realtime[forbidden]; leaked {
					t.Fatalf("restricted realtime leaked %q: %+v", forbidden, realtime)
				}
			}
		})
	}
}

func TestAccountViewDefaultsMatchRestrictedUI(t *testing.T) {
	view := normalizeAccountView("user", accountViewConfig{})
	if !reflect.DeepEqual(view.Columns, []string{"status", "subscription", "quota", "requests", "tpm"}) {
		t.Fatalf("default columns = %v", view.Columns)
	}
	if accountViewHas(view.Blocks, "billed") || accountViewHas(view.Blocks, "actual_cost") || accountViewHas(view.Blocks, "concurrency") || accountViewHas(view.Blocks, "queue") || accountViewHas(view.Blocks, "filters") {
		t.Fatalf("restricted defaults expose hidden blocks: %v", view.Blocks)
	}
}
