package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
)

func TestBillingAccountBreakdownPaginationAndLazyModelStats(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "billing.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	createAccount := func(name string) int64 {
		t.Helper()
		result, err := a.db.Exec(`INSERT INTO accounts (name) VALUES (?)`, name)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	alphaID := createAccount("alpha-billing@example.com")
	betaID := createAccount("beta-billing@example.com")

	insertUsage := func(requestID string, accountID int64, accountName, model string, inputTokens, outputTokens int64, inputCost, outputCost, billedCost, actualCost float64) {
		t.Helper()
		_, err := a.db.Exec(`INSERT INTO usage_logs (
			request_id, purpose_key, purpose_name, group_id, account_id, account_name, model,
			input_tokens, output_tokens, input_cost, output_cost, base_cost, billed_cost, actual_cost
		) VALUES (?, 'default', '默认用途', 'a', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			requestID, accountID, accountName, model, inputTokens, outputTokens,
			inputCost, outputCost, inputCost+outputCost, billedCost, actualCost)
		if err != nil {
			t.Fatal(err)
		}
	}
	insertUsage("alpha-opus-1", alphaID, "alpha-billing@example.com", "claude-opus-test", 1_000_000, 100_000, 3, 1.5, 6, 4)
	insertUsage("alpha-opus-2", alphaID, "alpha-billing@example.com", "claude-opus-test", 1_000_000, 100_000, 3, 1.5, 6, 4)
	insertUsage("alpha-sonnet", alphaID, "alpha-billing@example.com", "claude-sonnet-test", 500_000, 50_000, 1.5, 0.75, 3, 2)
	insertUsage("beta-opus", betaID, "beta-billing@example.com", "claude-opus-test", 250_000, 25_000, 0.75, 0.375, 1.5, 1)

	var first billingSummary
	putJSON(t, handler, http.MethodGet, "/api/billing?breakdown=account&page=1&page_size=1", nil, http.StatusOK, &first)
	if first.BreakdownTotal != 2 || first.BreakdownPage != 1 || first.BreakdownPageSize != 1 || first.BreakdownTotalPages != 2 {
		t.Fatalf("account breakdown pagination = %+v", first)
	}
	if len(first.ByAccount) != 1 || first.ByAccount[0].Key != fmt.Sprint(alphaID) {
		t.Fatalf("first account breakdown = %+v", first.ByAccount)
	}

	var second billingSummary
	putJSON(t, handler, http.MethodGet, "/api/billing?breakdown=account&page=2&page_size=1", nil, http.StatusOK, &second)
	if len(second.ByAccount) != 1 || second.ByAccount[0].Key != fmt.Sprint(betaID) {
		t.Fatalf("second account breakdown = %+v", second.ByAccount)
	}

	var models struct {
		Items      []billingModelBreakdown `json:"items"`
		Total      int64                   `json:"total"`
		Page       int                     `json:"page"`
		PageSize   int                     `json:"page_size"`
		TotalPages int                     `json:"total_pages"`
	}
	putJSON(t, handler, http.MethodGet, fmt.Sprintf("/api/billing/accounts/%d/models?page=1&page_size=1", alphaID), nil, http.StatusOK, &models)
	if models.Total != 2 || models.Page != 1 || models.PageSize != 1 || models.TotalPages != 2 || len(models.Items) != 1 {
		t.Fatalf("model pagination = %+v", models)
	}
	opus := models.Items[0]
	if opus.Model != "claude-opus-test" || opus.Requests != 2 || opus.InputPerMillion != 3 || opus.OutputPerMillion != 15 || opus.BilledCost != 12 {
		t.Fatalf("opus model stats = %+v", opus)
	}
}
