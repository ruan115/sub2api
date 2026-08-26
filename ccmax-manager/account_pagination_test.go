package main

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountsPaginationSortingAndStatusCounts(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	for index, price := range []float64{3, 9, 5} {
		var created account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name":     []string{"page-one", "page-two", "page-three"}[index],
			"platform": "anthropic", "auth_type": "oauth", "status": "active",
			"schedulable": false, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "account_price": price, "group_ids": []string{"a"},
			"rpm_strategy": "tiered", "user_msg_queue_mode": "off",
		}, http.StatusCreated, &created)
	}

	var payload struct {
		Items        []account        `json:"items"`
		Total        int64            `json:"total"`
		Page         int              `json:"page"`
		PageSize     int              `json:"page_size"`
		TotalPages   int              `json:"total_pages"`
		StatusCounts map[string]int64 `json:"status_counts"`
		Summary      struct {
			Total int64 `json:"total"`
		} `json:"summary"`
	}
	putJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&page=1&page_size=2&sort=account_price&order=desc", nil, http.StatusOK, &payload)
	if payload.Total != 3 || payload.Page != 1 || payload.PageSize != 2 || payload.TotalPages != 2 || len(payload.Items) != 2 {
		t.Fatalf("pagination payload = %+v", payload)
	}
	if payload.Items[0].AccountPrice != 9 || payload.Items[1].AccountPrice != 5 {
		t.Fatalf("sorted prices = %v, %v", payload.Items[0].AccountPrice, payload.Items[1].AccountPrice)
	}
	if payload.StatusCounts["all"] != 3 || payload.StatusCounts["unavailable"] != 3 || payload.Summary.Total != 3 {
		t.Fatalf("status counts = %+v summary = %+v", payload.StatusCounts, payload.Summary)
	}
}

func TestSuccessfulReauthorizationCountersExcludeInitialAndRefresh(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "reauth-count@example.com", "platform": "anthropic", "auth_type": "oauth",
		"status": "active", "schedulable": false, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	token := &claudeTokenInfo{AccessToken: "first", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix(), EmailAddress: created.Name}
	if err := a.saveClaudeToken(created.ID, "oauth", token, false); err != nil {
		t.Fatal(err)
	}
	var count int
	var at any
	if err := a.db.QueryRow(`SELECT reauthorization_count, reauthorized_at FROM accounts WHERE id = ?`, created.ID).Scan(&count, &at); err != nil {
		t.Fatal(err)
	}
	if count != 0 || at != nil {
		t.Fatalf("initial authorization count/time = %d/%v", count, at)
	}
	token.AccessToken = "second"
	if err := a.saveClaudeToken(created.ID, "oauth", token, false); err != nil {
		t.Fatal(err)
	}
	token.AccessToken = "refreshed"
	if err := a.saveClaudeToken(created.ID, "oauth", token, true); err != nil {
		t.Fatal(err)
	}
	var reauthorizedAt string
	if err := a.db.QueryRow(`SELECT reauthorization_count, reauthorized_at FROM accounts WHERE id = ?`, created.ID).Scan(&count, &reauthorizedAt); err != nil {
		t.Fatal(err)
	}
	if count != 1 || reauthorizedAt == "" {
		t.Fatalf("reauthorization count/time = %d/%q", count, reauthorizedAt)
	}
}
