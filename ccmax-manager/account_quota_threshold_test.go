package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAccountFiveHourThresholdCoolingLifecycle(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, quota_5h_threshold_enabled, quota_5h_threshold_percent) VALUES ('threshold-account', 1, 80)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO account_groups (account_id, group_id) VALUES (?, 'a')`, accountID); err != nil {
		t.Fatal(err)
	}
	resetAt := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	policy := rateLimitPolicy{FiveHourStaggerSet: true, FiveHourStaggerEnabled: false}
	capture := func(utilization float64) {
		headers := make(http.Header)
		headers.Set("anthropic-ratelimit-unified-5h-utilization", formatQuotaHeaderUtilization(utilization))
		headers.Set("anthropic-ratelimit-unified-5h-reset", formatQuotaHeaderReset(resetAt))
		a.captureAccountUpstreamStateWithPolicy(accountID, &http.Response{StatusCode: http.StatusOK, Header: headers}, policy)
	}

	capture(79)
	var reason string
	var cooldown sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_reset_at FROM accounts WHERE id = ?`, accountID).Scan(&reason, &cooldown); err != nil {
		t.Fatal(err)
	}
	if reason != "" || cooldown.Valid {
		t.Fatalf("below threshold reason/reset = %q/%v", reason, cooldown)
	}

	capture(80)
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_reset_at FROM accounts WHERE id = ?`, accountID).Scan(&reason, &cooldown); err != nil {
		t.Fatal(err)
	}
	if reason != "quota_threshold" || !cooldown.Valid || cooldown.String != resetAt.Format(time.RFC3339Nano) {
		t.Fatalf("threshold reason/reset = %q/%v, want quota_threshold/%s", reason, cooldown, resetAt.Format(time.RFC3339Nano))
	}

	capture(20)
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_reset_at FROM accounts WHERE id = ?`, accountID).Scan(&reason, &cooldown); err != nil {
		t.Fatal(err)
	}
	if reason != "" || cooldown.Valid {
		t.Fatalf("released threshold reason/reset = %q/%v", reason, cooldown)
	}

	actual429Reset := resetAt.Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE accounts SET quota_5h_threshold_enabled = 0, rate_limit_reason = 'quota_exhausted', rate_limit_window = '5h', rate_limit_reset_at = ? WHERE id = ?`, actual429Reset, accountID); err != nil {
		t.Fatal(err)
	}
	if err := a.enforceStoredAccountFiveHourThreshold(accountID, policy); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_reset_at FROM accounts WHERE id = ?`, accountID).Scan(&reason, &cooldown); err != nil {
		t.Fatal(err)
	}
	if reason != "quota_exhausted" || !cooldown.Valid || cooldown.String != actual429Reset {
		t.Fatalf("disabled threshold changed real 429 cooldown: %q/%v", reason, cooldown)
	}
}

func TestAccountSevenDayThresholdCoolingLifecycle(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, quota_5h_threshold_enabled, quota_5h_threshold_percent, quota_7d_threshold_enabled, quota_7d_threshold_percent) VALUES ('seven-day-threshold-account', 1, 80, 1, 60)`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO account_groups (account_id, group_id) VALUES (?, 'a')`, accountID); err != nil {
		t.Fatal(err)
	}
	fiveHourReset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	sevenDayReset := time.Now().UTC().Add(4 * 24 * time.Hour).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", formatQuotaHeaderUtilization(85))
	headers.Set("anthropic-ratelimit-unified-5h-reset", formatQuotaHeaderReset(fiveHourReset))
	headers.Set("anthropic-ratelimit-unified-7d-utilization", formatQuotaHeaderUtilization(60))
	headers.Set("anthropic-ratelimit-unified-7d-reset", formatQuotaHeaderReset(sevenDayReset))
	a.captureAccountUpstreamStateWithPolicy(accountID, &http.Response{StatusCode: http.StatusOK, Header: headers}, rateLimitPolicy{FiveHourStaggerSet: true, FiveHourStaggerEnabled: false})

	var reason, window string
	var cooldown sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_window, rate_limit_reset_at FROM accounts WHERE id = ?`, accountID).Scan(&reason, &window, &cooldown); err != nil {
		t.Fatal(err)
	}
	if reason != "quota_threshold" || window != "7d" || !cooldown.Valid || cooldown.String != sevenDayReset.Format(time.RFC3339Nano) {
		t.Fatalf("combined threshold reason/window/reset = %q/%q/%v", reason, window, cooldown)
	}

	headers.Set("anthropic-ratelimit-unified-5h-utilization", formatQuotaHeaderUtilization(20))
	headers.Set("anthropic-ratelimit-unified-7d-utilization", formatQuotaHeaderUtilization(20))
	a.captureAccountUpstreamStateWithPolicy(accountID, &http.Response{StatusCode: http.StatusOK, Header: headers}, rateLimitPolicy{})
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_window, rate_limit_reset_at FROM accounts WHERE id = ?`, accountID).Scan(&reason, &window, &cooldown); err != nil {
		t.Fatal(err)
	}
	if reason != "" || window != "" || cooldown.Valid {
		t.Fatalf("released seven-day threshold state = %q/%q/%v", reason, window, cooldown)
	}
}

func TestPartialQuotaHeadersPreserveSevenDayThresholdState(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	sevenDayReset := time.Now().UTC().Add(3 * 24 * time.Hour).Truncate(time.Second)
	result, err := a.db.Exec(`INSERT INTO accounts (name, quota_7d_utilization, quota_7d_reset_at, quota_7d_threshold_enabled, quota_7d_threshold_percent) VALUES ('partial-header-account', 70, ?, 1, 60)`, sevenDayReset.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	fiveHourReset := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", formatQuotaHeaderUtilization(20))
	headers.Set("anthropic-ratelimit-unified-5h-reset", formatQuotaHeaderReset(fiveHourReset))
	a.captureAccountUpstreamStateWithPolicy(accountID, &http.Response{StatusCode: http.StatusOK, Header: headers}, rateLimitPolicy{})

	var utilization float64
	var reason, window string
	if err := a.db.QueryRow(`SELECT quota_7d_utilization, rate_limit_reason, rate_limit_window FROM accounts WHERE id = ?`, accountID).Scan(&utilization, &reason, &window); err != nil {
		t.Fatal(err)
	}
	if utilization != 70 || reason != "quota_threshold" || window != "7d" {
		t.Fatalf("partial 5h headers reset 7d state: utilization=%v reason=%q window=%q", utilization, reason, window)
	}
}

func formatQuotaHeaderUtilization(percent float64) string {
	return strconv.FormatFloat(percent/100, 'f', -1, 64)
}

func formatQuotaHeaderReset(resetAt time.Time) string {
	return strconv.FormatInt(resetAt.Unix(), 10)
}

func TestAccountFiveHourThresholdPersistsAndFilters(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	payload := map[string]any{
		"name": "threshold-api-account", "platform": "anthropic", "auth_type": "oauth",
		"extra": map[string]any{}, "status": "active", "schedulable": false,
		"concurrency": 1, "priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"},
		"quota_5h_threshold_enabled": true, "quota_5h_threshold_percent": 82,
	}
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusCreated, &created)
	if !created.Quota5HThresholdEnabled || created.Quota5HThresholdPercent != 82 {
		t.Fatalf("created threshold settings = %+v", created)
	}

	delete(payload, "quota_5h_threshold_enabled")
	delete(payload, "quota_5h_threshold_percent")
	payload["notes"] = "legacy update"
	var updated account
	putJSON(t, handler, http.MethodPut, "/api/accounts/"+strconv.FormatInt(created.ID, 10), payload, http.StatusOK, &updated)
	if !updated.Quota5HThresholdEnabled || updated.Quota5HThresholdPercent != 82 {
		t.Fatalf("legacy update reset threshold settings: %+v", updated)
	}

	if _, err := a.db.Exec(`UPDATE accounts SET quota_5h_utilization = 85 WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO accounts (name, quota_5h_utilization, quota_5h_threshold_enabled, quota_5h_threshold_percent) VALUES ('below-account', 75, 1, 70), ('disabled-account', 95, 0, 80)`); err != nil {
		t.Fatal(err)
	}

	var page struct {
		Items []account `json:"items"`
		Total int64     `json:"total"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&min_5h_utilization=80", nil, nil, "", http.StatusOK, &page)
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("minimum utilization page = %+v", page)
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&quota_5h_utilization=75", nil, nil, "", http.StatusOK, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Name != "below-account" {
		t.Fatalf("exact utilization page = %+v", page)
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&quota_5h_threshold=reached", nil, nil, "", http.StatusOK, &page)
	if page.Total != 2 || len(page.Items) != 2 {
		t.Fatalf("reached threshold page = %+v", page)
	}
	var summary accountSummary
	requestJSON(t, handler, http.MethodGet, "/api/accounts/summary?quota_5h_utilization=75", nil, nil, "", http.StatusOK, &summary)
	if summary.Accounts != 1 {
		t.Fatalf("exact utilization summary = %+v", summary)
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts/summary?quota_5h_threshold=reached", nil, nil, "", http.StatusOK, &summary)
	if summary.Accounts != 2 {
		t.Fatalf("reached threshold summary = %+v", summary)
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&quota_5h_threshold=disabled", nil, nil, "", http.StatusOK, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Name != "disabled-account" {
		t.Fatalf("disabled threshold page = %+v", page)
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts/summary?quota_5h_threshold=disabled", nil, nil, "", http.StatusOK, &summary)
	if summary.Accounts != 1 {
		t.Fatalf("disabled threshold summary = %+v", summary)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/accounts?min_5h_utilization=101", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid threshold filter status = %d, body=%s", response.Code, response.Body.String())
	}
}

func TestAccountFiveHourThresholdBatchUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	resetAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	result, err := a.db.Exec(`INSERT INTO accounts (name, quota_5h_utilization, quota_5h_reset_at, quota_5h_threshold_enabled, quota_5h_threshold_percent)
		VALUES ('batch-threshold-1', 85, ?, 0, 80), ('batch-threshold-2', 85, ?, 0, 80)`, resetAt, resetAt)
	if err != nil {
		t.Fatal(err)
	}
	lastID, _ := result.LastInsertId()
	ids := []int64{lastID - 1, lastID}
	var response map[string]int
	requestJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{
		"ids": ids, "quota_5h_threshold_enabled": true, "quota_5h_threshold_percent": 75,
	}, nil, "", http.StatusOK, &response)
	if response["updated"] != 2 {
		t.Fatalf("batch threshold update response = %+v", response)
	}
	for _, id := range ids {
		var enabled, threshold int
		var reason string
		if err := a.db.QueryRow(`SELECT quota_5h_threshold_enabled, quota_5h_threshold_percent, rate_limit_reason FROM accounts WHERE id = ?`, id).Scan(&enabled, &threshold, &reason); err != nil {
			t.Fatal(err)
		}
		if enabled != 1 || threshold != 75 || reason != "quota_threshold" {
			t.Fatalf("account %d threshold state = %d/%d/%q", id, enabled, threshold, reason)
		}
	}

	requestJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{
		"ids": ids, "quota_5h_threshold_enabled": false,
	}, nil, "", http.StatusOK, &response)
	for _, id := range ids {
		var enabled int
		var reason string
		var cooldown sql.NullString
		if err := a.db.QueryRow(`SELECT quota_5h_threshold_enabled, rate_limit_reason, rate_limit_reset_at FROM accounts WHERE id = ?`, id).Scan(&enabled, &reason, &cooldown); err != nil {
			t.Fatal(err)
		}
		if enabled != 0 || reason != "" || cooldown.Valid {
			t.Fatalf("account %d disabled threshold state = %d/%q/%v", id, enabled, reason, cooldown)
		}
	}

	recorder := httptest.NewRecorder()
	body := `{"ids":[` + strconv.FormatInt(ids[0], 10) + `],"quota_5h_threshold_percent":101}`
	request := httptest.NewRequest(http.MethodPost, "/api/accounts/batch-update", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid batch threshold status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAccountSevenDayThresholdPersistsAndFilters(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	payload := map[string]any{
		"name": "seven-day-api-account", "platform": "anthropic", "auth_type": "oauth",
		"extra": map[string]any{}, "status": "active", "schedulable": false,
		"concurrency": 1, "priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"},
		"quota_7d_threshold_enabled": true, "quota_7d_threshold_percent": 64,
	}
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusCreated, &created)
	if !created.Quota7DThresholdEnabled || created.Quota7DThresholdPercent != 64 {
		t.Fatalf("created 7d threshold settings = %+v", created)
	}
	delete(payload, "quota_7d_threshold_enabled")
	delete(payload, "quota_7d_threshold_percent")
	payload["notes"] = "legacy update"
	var updated account
	putJSON(t, handler, http.MethodPut, "/api/accounts/"+strconv.FormatInt(created.ID, 10), payload, http.StatusOK, &updated)
	if !updated.Quota7DThresholdEnabled || updated.Quota7DThresholdPercent != 64 {
		t.Fatalf("legacy update reset 7d threshold settings: %+v", updated)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET quota_7d_utilization = 64 WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO accounts (name, quota_7d_utilization, quota_7d_threshold_enabled, quota_7d_threshold_percent) VALUES ('seven-day-disabled', 64, 0, 64)`); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items []account `json:"items"`
		Total int64     `json:"total"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&quota_7d_utilization=64&quota_7d_threshold=reached", nil, nil, "", http.StatusOK, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].ID != created.ID {
		t.Fatalf("7d reached threshold page = %+v", page)
	}
	requestJSON(t, handler, http.MethodGet, "/api/accounts?paginated=1&quota_7d_threshold=disabled", nil, nil, "", http.StatusOK, &page)
	if page.Total != 1 || len(page.Items) != 1 || page.Items[0].Name != "seven-day-disabled" {
		t.Fatalf("7d disabled threshold page = %+v", page)
	}
	var summary accountSummary
	requestJSON(t, handler, http.MethodGet, "/api/accounts/summary?quota_7d_utilization=64&quota_7d_threshold=reached", nil, nil, "", http.StatusOK, &summary)
	if summary.Accounts != 1 {
		t.Fatalf("7d filtered summary = %+v", summary)
	}
}
