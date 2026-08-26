package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const shanghaiOffset = "+8 hours"

type authorizationLog struct {
	ID               int64  `json:"id"`
	AccountID        *int64 `json:"account_id"`
	AccountName      string `json:"account_name"`
	ProxyID          *int64 `json:"proxy_id"`
	ProxyIP          string `json:"proxy_ip"`
	Method           string `json:"method"`
	Success          bool   `json:"success"`
	StatusMessage    string `json:"status_message"`
	SubscriptionType string `json:"subscription_type"`
	ClientIP         string `json:"client_ip"`
	CreatedAt        string `json:"created_at"`
}

type authorizationSummary struct {
	Total       int64   `json:"total"`
	Successful  int64   `json:"successful"`
	Failed      int64   `json:"failed"`
	SuccessRate float64 `json:"success_rate"`
}

type authorizationStats struct {
	Summary    authorizationSummary `json:"summary"`
	Items      []authorizationLog   `json:"items"`
	Page       int                  `json:"page"`
	PageSize   int                  `json:"page_size"`
	TotalPages int                  `json:"total_pages"`
}

type dailyStat struct {
	Date              string  `json:"date"`
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	BilledCost        float64 `json:"billed_cost"`
	ActualCost        float64 `json:"actual_cost"`
	AccountsOnboarded int64   `json:"accounts_onboarded"`
	AccountsDied      int64   `json:"accounts_died"`
	Authorizations    int64   `json:"authorizations"`
	AuthSuccessful    int64   `json:"auth_successful"`
}

type batchAuthorizationInput struct {
	SessionKeys     []string `json:"session_keys"`
	ProxyPoolID     int64    `json:"proxy_pool_id"`
	GroupIDs        []string `json:"group_ids"`
	AuthType        string   `json:"auth_type"`
	AccountPrice    float64  `json:"account_price"`
	Concurrency     int      `json:"concurrency"`
	BaseRPM         int      `json:"base_rpm"`
	RPMStrategy     string   `json:"rpm_strategy"`
	RPMStickyBuffer int      `json:"rpm_sticky_buffer"`
	StrategyID      *int64   `json:"strategy_id"`
}

type batchAuthorizationResult struct {
	Index        int    `json:"index"`
	AccountID    int64  `json:"account_id,omitempty"`
	Name         string `json:"name,omitempty"`
	ProxyIP      string `json:"proxy_ip,omitempty"`
	Subscription string `json:"subscription_type,omitempty"`
	Success      bool   `json:"success"`
	Updated      bool   `json:"updated,omitempty"`
	Skipped      bool   `json:"skipped,omitempty"`
	Error        string `json:"error,omitempty"`
}

type batchAuthorizationResponse struct {
	Total   int                        `json:"total"`
	Success int                        `json:"success"`
	Updated int                        `json:"updated"`
	Skipped int                        `json:"skipped"`
	Failed  int                        `json:"failed"`
	Items   []batchAuthorizationResult `json:"items"`
}

type batchAuthProxyCandidate struct {
	ID  int64
	URL string
}

type batchAuthProxyProbeResult struct {
	Candidate     batchAuthProxyCandidate
	Organizations []claudeOrganization
	Err           error
}

const accumulateAccountSurvivalSQL = `survival_seconds_total = survival_seconds_total + CASE
	WHEN invalidated_at IS NULL AND onboarded_at IS NOT NULL
	THEN MAX(0, CAST(strftime('%s','now') AS INTEGER) - CAST(strftime('%s', onboarded_at) AS INTEGER))
	ELSE 0 END`

func accountSurvivalSeconds(onboardedAt, invalidatedAt string, accumulated int64) int64 {
	if onboardedAt == "" || invalidatedAt != "" {
		return max(0, accumulated)
	}
	started, err := time.Parse(time.RFC3339Nano, onboardedAt)
	if err != nil {
		return max(0, accumulated)
	}
	ended := time.Now().UTC()
	if ended.Before(started) {
		return max(0, accumulated)
	}
	return max(0, accumulated) + int64(ended.Sub(started).Seconds())
}

func accountDispatchState(item account) string {
	if item.Status == "error" || item.InvalidatedAt != "" || (item.AuthStatus == "reauth_required" && item.OnboardedAt != "") {
		return "error"
	}
	if item.Status != "active" || !item.Schedulable || item.AuthStatus != "valid" || item.ProxyID == nil || item.ProxyStatus != "active" {
		return "unavailable"
	}
	now := time.Now().UTC()
	if item.ExpiresAt != "" {
		if expires, err := time.Parse(time.RFC3339Nano, item.ExpiresAt); err == nil && !expires.After(now) {
			return "unavailable"
		}
	}
	if item.RateLimitResetAt != "" {
		if reset, err := time.Parse(time.RFC3339Nano, item.RateLimitResetAt); err == nil && reset.After(now) {
			return "unavailable"
		}
	}
	return "normal"
}

// accountLimitWindow reports which quota window is holding the account back, so
// the UI can split "暂不可调度" into 5h and 7d buckets. Older rows may not have
// rate_limit_window populated, so matching the selected cooldown reset to the
// sampled quota reset keeps those accounts classified correctly.
func accountLimitWindow(item account) string {
	if item.RateLimitResetAt == "" {
		return ""
	}
	reset, err := time.Parse(time.RFC3339Nano, item.RateLimitResetAt)
	if err != nil || !reset.After(time.Now().UTC()) {
		return ""
	}
	if item.RateLimitWindow == "5h" || item.RateLimitWindow == "7d" {
		return item.RateLimitWindow
	}
	return quotaWindowMatchingReset(item.RateLimitResetAt, item.Quota5HResetAt, item.Quota7DResetAt)
}

func accountStatePredicate(alias, state string) string {
	errorState := "(" + alias + ".status = 'error' OR " + alias + ".invalidated_at IS NOT NULL OR (" + alias + ".auth_status = 'reauth_required' AND " + alias + ".onboarded_at IS NOT NULL))"
	normalState := "(" + alias + ".status = 'active' AND " + alias + ".schedulable = 1 AND " + alias + ".auth_status = 'valid' " +
		"AND " + alias + ".invalidated_at IS NULL " +
		"AND (" + alias + ".expires_at IS NULL OR " + alias + ".expires_at > " + nowSQL + ") " +
		"AND (" + alias + ".rate_limit_reset_at IS NULL OR " + alias + ".rate_limit_reset_at <= " + nowSQL + ") " +
		"AND EXISTS (SELECT 1 FROM proxies state_proxy WHERE state_proxy.id = " + alias + ".proxy_id AND state_proxy.status = 'active' AND state_proxy.deleted_at IS NULL))"
	unavailableState := "(NOT " + errorState + " AND NOT " + normalState + ")"
	resetMatches := func(quotaResetColumn string) string {
		return "(" + alias + "." + quotaResetColumn + " IS NOT NULL" +
			" AND ABS(strftime('%s', " + alias + ".rate_limit_reset_at) - strftime('%s', " + alias + "." + quotaResetColumn + ")) <= 1)"
	}
	fiveHourResetMatches := resetMatches("quota_5h_reset_at")
	sevenDayResetMatches := resetMatches("quota_7d_reset_at")
	// A window bucket is a strict subset of "unavailable". Prefer the explicit
	// value, but infer old/ambiguous rows from the reset selected by the 429
	// scheduler. If both resets happen to match, 7d remains the stricter window.
	limitedState := func(window string) string {
		windowState := alias + ".rate_limit_window = '" + window + "'"
		if window == "7d" {
			windowState += " OR (" + alias + ".rate_limit_window NOT IN ('5h', '7d') AND " + sevenDayResetMatches + ")"
		} else {
			windowState += " OR (" + alias + ".rate_limit_window NOT IN ('5h', '7d') AND " + fiveHourResetMatches + " AND NOT " + sevenDayResetMatches + ")"
		}
		return "(" + unavailableState + " AND COALESCE(" + alias + ".rate_limit_reason, '') != '429_cooling' AND (" + windowState + ")" +
			" AND " + alias + ".rate_limit_reset_at IS NOT NULL AND " + alias + ".rate_limit_reset_at > " + nowSQL + ")"
	}
	switch state {
	case "normal":
		return normalState
	case "error":
		return errorState
	case "unavailable":
		return unavailableState
	case "limited_5h":
		return limitedState("5h")
	case "limited_7d":
		return limitedState("7d")
	case "cooling_429":
		return "(" + unavailableState + " AND " + alias + ".rate_limit_reason = '429_cooling')"
	default:
		return "1 = 1"
	}
}

func (a *app) recordAccountLifecycle(accountID int64, eventType string) {
	if eventType != "onboarded" && eventType != "invalidated" {
		return
	}
	_, err := a.db.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, ?)`, accountID, eventType)
	logDatabaseWriteError("insert account lifecycle event", err)
}

func normalizeSubscriptionType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch {
	case value == "":
		return ""
	case strings.Contains(value, "enterprise"):
		return "enterprise"
	case strings.Contains(value, "team"):
		return "team"
	case strings.Contains(value, "max"):
		return "max"
	case strings.Contains(value, "pro"):
		return "pro"
	case strings.Contains(value, "free"):
		return "free"
	default:
		return value
	}
}

func normalizeRateLimitTier(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func subscriptionTypeFromCredentials(credentials map[string]any) string {
	if value, ok := credentials["subscription_type"].(string); ok {
		if normalized := normalizeSubscriptionType(value); normalized != "" {
			return normalized
		}
	}
	accessToken, _ := credentials["access_token"].(string)
	return subscriptionTypeFromToken(accessToken)
}

func rateLimitTierFromCredentials(credentials map[string]any) string {
	value, _ := credentials["rate_limit_tier"].(string)
	return normalizeRateLimitTier(value)
}

func subscriptionTypeFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return findSubscriptionClaim(claims)
}

func findSubscriptionClaim(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		for _, key := range []string{"subscription_type", "plan_type", "plan", "raven_type", "organization_type", "account_type", "tier"} {
			if raw, ok := typed[key].(string); ok {
				if normalized := normalizeSubscriptionType(raw); normalized != "" {
					return normalized
				}
			}
		}
		for _, nested := range typed {
			if result := findSubscriptionClaim(nested); result != "" {
				return result
			}
		}
	case []any:
		for _, nested := range typed {
			if result := findSubscriptionClaim(nested); result != "" {
				return result
			}
		}
	}
	return ""
}

func (a *app) recordAuthorization(accountID, proxyID *int64, accountName, method string, success bool, message, subscription, clientIP string) {
	message = strings.TrimSpace(message)
	if len(message) > 500 {
		message = message[:500]
	}
	proxyIP := ""
	if accountID != nil {
		var storedName string
		var storedProxy sql.NullInt64
		if err := a.db.QueryRow(`SELECT name, proxy_id FROM accounts WHERE id = ?`, *accountID).Scan(&storedName, &storedProxy); err == nil {
			if accountName == "" {
				accountName = storedName
			}
			if proxyID == nil && storedProxy.Valid {
				value := storedProxy.Int64
				proxyID = &value
			}
		}
	}
	if proxyID != nil {
		_ = a.db.QueryRow(`SELECT COALESCE(NULLIF(exit_ip, ''), host) FROM proxies WHERE id = ?`, *proxyID).Scan(&proxyIP)
	}
	_, err := a.db.Exec(`INSERT INTO authorization_logs (account_id, account_name, proxy_id, proxy_ip, method, success, status_message, subscription_type, client_ip) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		accountID, accountName, proxyID, proxyIP, method, boolInt(success), message, subscription, clientIP)
	logDatabaseWriteError("insert authorization log", err)
}

func (a *app) handleAuthorizationStats(w http.ResponseWriter, r *http.Request) {
	where := []string{"1 = 1"}
	args := []any{}
	if user := currentUser(r); user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "scope_account")
		where = append(where, `EXISTS (SELECT 1 FROM accounts scope_account WHERE scope_account.id = authorization_logs.account_id AND `+condition+`)`)
		args = append(args, scopeArgs...)
	}
	if from := normalizeDateStart(strings.TrimSpace(r.URL.Query().Get("from"))); from != "" {
		where = append(where, "created_at >= ?")
		args = append(args, from)
	}
	if to := normalizeDateEnd(strings.TrimSpace(r.URL.Query().Get("to"))); to != "" {
		where = append(where, "created_at < ?")
		args = append(args, to)
	}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		term := "%" + search + "%"
		where = append(where, `(account_name LIKE ? OR proxy_ip LIKE ? OR client_ip LIKE ?
			OR method LIKE ? OR status_message LIKE ? OR CAST(COALESCE(account_id, 0) AS CHAR) LIKE ?)`)
		args = append(args, term, term, term, term, term, term)
	}
	switch strings.TrimSpace(r.URL.Query().Get("status")) {
	case "success":
		where = append(where, "success = 1")
	case "failed":
		where = append(where, "success = 0")
	}
	page, pageSize, offset := paginationFromRequest(r, 20, 100)
	clause := strings.Join(where, " AND ")
	var result authorizationStats
	if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(success), 0) FROM authorization_logs WHERE `+clause, args...).Scan(&result.Summary.Total, &result.Summary.Successful); err != nil {
		writeDBError(w, err)
		return
	}
	result.Summary.Failed = result.Summary.Total - result.Summary.Successful
	if result.Summary.Total > 0 {
		result.Summary.SuccessRate = float64(result.Summary.Successful) * 100 / float64(result.Summary.Total)
	}
	result.Page, result.PageSize = page, pageSize
	result.TotalPages = totalPages(result.Summary.Total, pageSize)
	queryArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := a.db.Query(`SELECT id, account_id, account_name, proxy_id, proxy_ip, method, success, status_message, subscription_type, client_ip, created_at FROM authorization_logs WHERE `+clause+` ORDER BY id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	result.Items = []authorizationLog{}
	for rows.Next() {
		var item authorizationLog
		var accountID, proxyID sql.NullInt64
		var success int
		if err := rows.Scan(&item.ID, &accountID, &item.AccountName, &proxyID, &item.ProxyIP, &item.Method, &success, &item.StatusMessage, &item.SubscriptionType, &item.ClientIP, &item.CreatedAt); err != nil {
			writeDBError(w, err)
			return
		}
		item.AccountID = nullIntPointer(accountID)
		item.ProxyID = nullIntPointer(proxyID)
		item.Success = success == 1
		result.Items = append(result.Items, item)
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) handleDailyStats(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	startDate, endDate, days, err := dailyStatsRange(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	start := startDate.Format("2006-01-02")
	end := endDate.Format("2006-01-02")
	items := map[string]*dailyStat{}
	for index := 0; index < days; index++ {
		date := endDate.AddDate(0, 0, -index).Format("2006-01-02")
		items[date] = &dailyStat{Date: date}
	}
	usageWhere := `date(created_at, '` + shanghaiOffset + `') BETWEEN ? AND ?`
	usageArgs := []any{start, end}
	if user.Role == "user" {
		usageWhere += ` AND user_id = ?`
		usageArgs = append(usageArgs, user.ID)
	}
	usageRows, err := a.db.Query(`SELECT date(created_at, '`+shanghaiOffset+`'), COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(billed_cost), 0), COALESCE(SUM(actual_cost), 0) FROM usage_logs WHERE `+usageWhere+` GROUP BY 1`, usageArgs...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	for usageRows.Next() {
		var databaseDate any
		var requests, inputTokens, outputTokens int64
		var billed, actual float64
		if scanErr := usageRows.Scan(&databaseDate, &requests, &inputTokens, &outputTokens, &billed, &actual); scanErr != nil {
			usageRows.Close()
			writeDBError(w, scanErr)
			return
		}
		date := dailyStatsDateKey(databaseDate)
		if item := items[date]; item != nil {
			item.Requests, item.InputTokens, item.OutputTokens, item.BilledCost, item.ActualCost = requests, inputTokens, outputTokens, billed, actual
		}
	}
	usageRows.Close()
	if err := a.mergeDailyAccountEvents(items, start, end, "onboarded", user, func(item *dailyStat, count int64) { item.AccountsOnboarded = count }); err != nil {
		writeDBError(w, err)
		return
	}
	if err := a.mergeDailyAccountEvents(items, start, end, "invalidated", user, func(item *dailyStat, count int64) { item.AccountsDied = count }); err != nil {
		writeDBError(w, err)
		return
	}
	authWhere := `date(created_at, '` + shanghaiOffset + `') BETWEEN ? AND ?`
	authArgs := []any{start, end}
	if user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "scope_account")
		authWhere += ` AND EXISTS (SELECT 1 FROM accounts scope_account WHERE scope_account.id = authorization_logs.account_id AND ` + condition + `)`
		authArgs = append(authArgs, scopeArgs...)
	}
	authRows, err := a.db.Query(`SELECT date(created_at, '`+shanghaiOffset+`'), COUNT(*), COALESCE(SUM(success), 0) FROM authorization_logs WHERE `+authWhere+` GROUP BY 1`, authArgs...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	for authRows.Next() {
		var databaseDate any
		var total, successful int64
		if scanErr := authRows.Scan(&databaseDate, &total, &successful); scanErr != nil {
			authRows.Close()
			writeDBError(w, scanErr)
			return
		}
		date := dailyStatsDateKey(databaseDate)
		if item := items[date]; item != nil {
			item.Authorizations, item.AuthSuccessful = total, successful
		}
	}
	authRows.Close()
	result := make([]dailyStat, 0, days)
	for index := 0; index < days; index++ {
		date := endDate.AddDate(0, 0, -index).Format("2006-01-02")
		result = append(result, *items[date])
	}
	writeJSON(w, http.StatusOK, result)
}

func dailyStatsDateKey(value any) string {
	if date, ok := value.(time.Time); ok {
		return date.Format("2006-01-02")
	}
	var text string
	switch value := value.(type) {
	case string:
		text = value
	case []byte:
		text = string(value)
	default:
		text = fmt.Sprint(value)
	}
	text = strings.TrimSpace(text)
	if len(text) >= len("2006-01-02") {
		candidate := text[:len("2006-01-02")]
		if _, err := time.Parse("2006-01-02", candidate); err == nil {
			return candidate
		}
	}
	return text
}

func dailyStatsRange(r *http.Request) (time.Time, time.Time, int, error) {
	const maxDays = 365
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(location)
	endDate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
	days := 30
	startDate := endDate.AddDate(0, 0, -(days - 1))
	from, to := strings.TrimSpace(r.URL.Query().Get("from")), strings.TrimSpace(r.URL.Query().Get("to"))
	if from == "" && to == "" {
		if value, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && value > 0 {
			days = min(value, maxDays)
			startDate = endDate.AddDate(0, 0, -(days - 1))
		}
		return startDate, endDate, days, nil
	}
	if from == "" || to == "" {
		return time.Time{}, time.Time{}, 0, errors.New("from and to dates are both required")
	}
	var err error
	startDate, err = time.ParseInLocation("2006-01-02", from, location)
	if err != nil {
		return time.Time{}, time.Time{}, 0, errors.New("from date is invalid")
	}
	endDate, err = time.ParseInLocation("2006-01-02", to, location)
	if err != nil {
		return time.Time{}, time.Time{}, 0, errors.New("to date is invalid")
	}
	if startDate.After(endDate) {
		return time.Time{}, time.Time{}, 0, errors.New("from date cannot be after to date")
	}
	days = int(endDate.Sub(startDate)/(24*time.Hour)) + 1
	if days > maxDays {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("date range cannot exceed %d days", maxDays)
	}
	return startDate, endDate, days, nil
}

func (a *app) mergeDailyAccountEvents(items map[string]*dailyStat, start, end, eventType string, user panelUser, assign func(*dailyStat, int64)) error {
	if eventType != "onboarded" && eventType != "invalidated" {
		return errors.New("invalid account event column")
	}
	where := `event_type = ? AND date(created_at, '` + shanghaiOffset + `') BETWEEN ? AND ?`
	args := []any{eventType, start, end}
	if user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "scope_account")
		where += ` AND EXISTS (SELECT 1 FROM accounts scope_account WHERE scope_account.id = account_lifecycle_events.account_id AND ` + condition + `)`
		args = append(args, scopeArgs...)
	}
	rows, err := a.db.Query(`SELECT date(created_at, '`+shanghaiOffset+`'), COUNT(*) FROM account_lifecycle_events WHERE `+where+` GROUP BY 1`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var databaseDate any
		var count int64
		if err := rows.Scan(&databaseDate, &count); err != nil {
			return err
		}
		date := dailyStatsDateKey(databaseDate)
		if item := items[date]; item != nil {
			assign(item, count)
		}
	}
	return rows.Err()
}

func (a *app) handleBatchAuthorization(w http.ResponseWriter, r *http.Request) {
	var input batchAuthorizationInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.GroupIDs = uniqueGroups(input.GroupIDs)
	if input.ProxyPoolID <= 0 || len(input.GroupIDs) == 0 || input.AccountPrice < 0 {
		writeError(w, http.StatusBadRequest, "proxy pool, account groups or account price is invalid")
		return
	}
	if err := a.validateAccountGroupIDs(input.GroupIDs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.Concurrency <= 0 {
		input.Concurrency = 10
	}
	if input.BaseRPM < 0 || input.BaseRPM > 10000 {
		writeError(w, http.StatusBadRequest, "base RPM is invalid")
		return
	}
	if input.RPMStrategy == "" {
		input.RPMStrategy = "tiered"
	}
	if input.RPMStrategy != "tiered" && input.RPMStrategy != "sticky_exempt" && input.RPMStrategy != "fixed" {
		writeError(w, http.StatusBadRequest, "invalid RPM strategy")
		return
	}
	if input.RPMStickyBuffer < 0 {
		writeError(w, http.StatusBadRequest, "RPM sticky buffer cannot be negative")
		return
	}
	strategyValue, err := a.resolveStrategyBinding(input.StrategyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	authType, _, apiScope, err := normalizeClaudeAuthMode(input.AuthType)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	keys := uniqueSessionKeys(input.SessionKeys)
	if len(keys) == 0 || len(keys) > 200 {
		writeError(w, http.StatusBadRequest, "provide between 1 and 200 Session Keys")
		return
	}
	a.batchAuthMu.Lock()
	defer a.batchAuthMu.Unlock()
	response := batchAuthorizationResponse{Total: len(keys), Items: make([]batchAuthorizationResult, 0, len(keys))}
	poolID := input.ProxyPoolID
	for index, sessionKey := range keys {
		item := batchAuthorizationResult{Index: index + 1}
		token, proxyID, _, exchangeErr := a.exchangeBatchClaudeSessionKey(r.Context(), sessionKey, apiScope, poolID)
		if exchangeErr != nil {
			item.Error = exchangeErr.Error()
			response.Failed++
			response.Items = append(response.Items, item)
			a.recordAuthorization(nil, proxyID, "", "batch_session_key", false, item.Error, "", requestIP(r))
			continue
		}
		item.Name = strings.TrimSpace(token.EmailAddress)
		if item.Name == "" {
			item.Error = "authorized token did not include an account email"
			response.Failed++
			response.Items = append(response.Items, item)
			a.recordAuthorization(nil, proxyID, "", "batch_session_key", false, item.Error, token.SubscriptionType, requestIP(r))
			continue
		}
		existingID, existingName, exists, lookupErr := a.findAccountByEmail(item.Name)
		if lookupErr != nil {
			item.Error = lookupErr.Error()
			response.Failed++
			response.Items = append(response.Items, item)
			a.recordAuthorization(nil, proxyID, item.Name, "batch_session_key", false, item.Error, token.SubscriptionType, requestIP(r))
			continue
		}
		if exists {
			leaseOwner, leaseErr := a.acquireAccountTokenLease(r.Context(), existingID)
			if leaseErr != nil {
				item.Error = leaseErr.Error()
				response.Failed++
				response.Items = append(response.Items, item)
				a.recordAuthorization(&existingID, nil, item.Name, "batch_session_key", false, item.Error, token.SubscriptionType, requestIP(r))
				continue
			}
			existingProxyID, existingProxyURL, proxyErr := a.accountAuthorizationProxy(existingID)
			if proxyErr != nil {
				a.releaseAccountTokenLease(existingID, leaseOwner)
				item.Error = proxyErr.Error()
				response.Failed++
				response.Items = append(response.Items, item)
				a.recordAuthorization(&existingID, nil, item.Name, "batch_session_key", false, item.Error, token.SubscriptionType, requestIP(r))
				continue
			}
			if proxyID == nil || *proxyID != existingProxyID {
				token, exchangeErr = exchangeClaudeSessionKey(r.Context(), sessionKey, apiScope, existingProxyURL)
				if exchangeErr != nil {
					a.releaseAccountTokenLease(existingID, leaseOwner)
					item.Error = exchangeErr.Error()
					response.Failed++
					response.Items = append(response.Items, item)
					a.recordAuthorization(&existingID, &existingProxyID, item.Name, "batch_session_key", false, item.Error, "", requestIP(r))
					continue
				}
				proxyID = &existingProxyID
			}
			updateErr := a.updateBatchAuthorizedAccount(existingID, input, authType, sessionKey, token, leaseOwner)
			a.releaseAccountTokenLease(existingID, leaseOwner)
			if updateErr != nil {
				item.Error = updateErr.Error()
				response.Failed++
				response.Items = append(response.Items, item)
				a.recordAuthorization(&existingID, proxyID, item.Name, "batch_session_key", false, item.Error, token.SubscriptionType, requestIP(r))
				continue
			}
			item.AccountID = existingID
			item.Name = existingName
			item.Subscription = token.SubscriptionType
			item.Success = true
			item.Updated = true
			item.Error = "同邮箱账号已更新 OAuth 凭证与授权代理"
			_ = a.db.QueryRow(`SELECT COALESCE(NULLIF(exit_ip, ''), host) FROM proxies WHERE id = ?`, *proxyID).Scan(&item.ProxyIP)
			response.Success++
			response.Updated++
			response.Items = append(response.Items, item)
			a.recordAuthorization(&existingID, proxyID, item.Name, "batch_session_key", true, item.Error, token.SubscriptionType, requestIP(r))
			continue
		}
		accountID, createErr := a.createBatchAuthorizedAccount(input, authType, item.Name, sessionKey, token, *proxyID, strategyValue)
		if createErr != nil {
			item.Error = createErr.Error()
			response.Failed++
			response.Items = append(response.Items, item)
			a.recordAuthorization(nil, proxyID, item.Name, "batch_session_key", false, item.Error, token.SubscriptionType, requestIP(r))
			continue
		}
		item.AccountID = accountID
		item.Success = true
		item.Subscription = token.SubscriptionType
		_ = a.db.QueryRow(`SELECT COALESCE(NULLIF(exit_ip, ''), host) FROM proxies WHERE id = ?`, *proxyID).Scan(&item.ProxyIP)
		response.Success++
		response.Items = append(response.Items, item)
		a.recordAuthorization(&accountID, proxyID, item.Name, "batch_session_key", true, "authorization succeeded", token.SubscriptionType, requestIP(r))
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) exchangeBatchClaudeSessionKey(ctx context.Context, sessionKey, scope string, poolID int64) (*claudeTokenInfo, *int64, string, error) {
	proxyID, proxyURL, err := a.selectProxyForNewAccount(&poolID, nil, true)
	if err != nil {
		return nil, nil, "", err
	}
	excluded := map[int64]bool{*proxyID: true}
	token, err := exchangeClaudeSessionKey(ctx, sessionKey, scope, proxyURL)
	if err == nil {
		return token, proxyID, proxyURL, nil
	}
	if !isClaudeSessionProxyChallenge(err) {
		return nil, proxyID, proxyURL, err
	}

	lastChallenge := err
	for {
		candidates, candidateErr := a.availableBatchAuthProxyCandidates(poolID, excluded)
		if candidateErr != nil {
			return nil, proxyID, proxyURL, candidateErr
		}
		if len(candidates) == 0 {
			return nil, proxyID, proxyURL, fmt.Errorf("no authorization-compatible proxy is available in this pool: %w", lastChallenge)
		}
		candidate, _, probeErr := probeClaudeSessionProxies(ctx, sessionKey, candidates)
		if probeErr != nil {
			return nil, proxyID, proxyURL, probeErr
		}
		excluded[candidate.ID] = true
		candidateID := candidate.ID
		token, exchangeErr := exchangeClaudeSessionKey(ctx, sessionKey, scope, candidate.URL)
		if exchangeErr == nil {
			return token, &candidateID, candidate.URL, nil
		}
		if !isClaudeSessionProxyChallenge(exchangeErr) {
			return nil, &candidateID, candidate.URL, exchangeErr
		}
		proxyID, proxyURL, lastChallenge = &candidateID, candidate.URL, exchangeErr
	}
}

func (a *app) accountAuthorizationProxy(accountID int64) (int64, string, error) {
	var proxyID sql.NullInt64
	if err := a.db.QueryRow(`SELECT proxy_id FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&proxyID); err != nil {
		return 0, "", err
	}
	if !proxyID.Valid {
		return 0, "", errors.New("existing account must bind an active proxy before reauthorization")
	}
	proxyURL, err := a.proxyURL(proxyID.Int64)
	if err != nil {
		return 0, "", errors.New("existing account proxy is unavailable")
	}
	return proxyID.Int64, proxyURL.String(), nil
}

func (a *app) availableBatchAuthProxyCandidates(poolID int64, excluded map[int64]bool) ([]batchAuthProxyCandidate, error) {
	rows, err := a.db.Query(`SELECT p.id FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
		WHERE p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
		AND pool.status = 'active' AND pool.deleted_at IS NULL AND pool.system_kind = ''
		AND `+proxyNotQuarantinedPredicate("p")+`
		AND (pool.single_use_enabled = 0 OR `+proxyIdentityUnusedPredicate("p")+`)
		AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.proxy_id = p.id AND a.deleted_at IS NULL)
		ORDER BY CASE WHEN p.last_test_at IS NULL THEN 1 ELSE 0 END, p.latency_ms, p.id`, poolID)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if scanErr := rows.Scan(&id); scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		if !excluded[id] {
			ids = append(ids, id)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	candidates := make([]batchAuthProxyCandidate, 0, len(ids))
	for _, id := range ids {
		proxyURL, proxyErr := a.proxyURL(id)
		if proxyErr == nil {
			candidates = append(candidates, batchAuthProxyCandidate{ID: id, URL: proxyURL.String()})
		}
	}
	return candidates, nil
}

func probeClaudeSessionProxies(ctx context.Context, sessionKey string, candidates []batchAuthProxyCandidate) (batchAuthProxyCandidate, []claudeOrganization, error) {
	probeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan batchAuthProxyCandidate)
	results := make(chan batchAuthProxyProbeResult, len(candidates))
	workerCount := 6
	if len(candidates) < workerCount {
		workerCount = len(candidates)
	}
	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for candidate := range jobs {
				organizations, err := getClaudeOrganizations(probeCtx, sessionKey, candidate.URL)
				select {
				case results <- batchAuthProxyProbeResult{Candidate: candidate, Organizations: organizations, Err: err}:
				case <-probeCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, candidate := range candidates {
			select {
			case jobs <- candidate:
			case <-probeCtx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	var lastErr error
	for result := range results {
		if result.Err == nil {
			cancel()
			return result.Candidate, result.Organizations, nil
		}
		lastErr = result.Err
		if isClaudeSessionInvalid(result.Err) && !isClaudeSessionProxyChallenge(result.Err) {
			cancel()
			return batchAuthProxyCandidate{}, nil, result.Err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no unassigned active proxy is available in this pool")
	}
	return batchAuthProxyCandidate{}, nil, fmt.Errorf("no authorization-compatible proxy is available in this pool: %w", lastErr)
}

func normalizeAccountEmail(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (a *app) findAccountByEmail(email string) (int64, string, bool, error) {
	target := normalizeAccountEmail(email)
	if target == "" {
		return 0, "", false, nil
	}
	rows, err := a.db.Query(`SELECT id, name, credentials_json FROM accounts WHERE platform = 'anthropic' AND deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return 0, "", false, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var name, credentialsJSON string
		if err := rows.Scan(&id, &name, &credentialsJSON); err != nil {
			return 0, "", false, err
		}
		credentials := decodeObject(credentialsJSON)
		credentialEmail, _ := credentials["email_address"].(string)
		if normalizeAccountEmail(name) == target || normalizeAccountEmail(credentialEmail) == target {
			return id, name, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, "", false, err
	}
	return 0, "", false, nil
}

func uniqueSessionKeys(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, block := range values {
		for _, line := range strings.FieldsFunc(block, func(r rune) bool { return r == '\n' || r == '\r' || r == ',' }) {
			value := strings.TrimSpace(line)
			if value != "" && !seen[value] {
				seen[value] = true
				result = append(result, value)
			}
		}
	}
	return result
}

func (a *app) createBatchAuthorizedAccount(input batchAuthorizationInput, authType, name, sessionKey string, token *claudeTokenInfo, proxyID int64, strategyID any) (int64, error) {
	encoded, err := json.Marshal(token)
	if err != nil {
		return 0, err
	}
	extraJSON, err := json.Marshal(map[string]any{
		"base_rpm":          input.BaseRPM,
		"rpm_strategy":      input.RPMStrategy,
		"rpm_sticky_buffer": input.RPMStickyBuffer,
	})
	if err != nil {
		return 0, err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	expiresAt := time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339Nano)
	result, err := tx.Exec(`INSERT INTO accounts (name, platform, auth_type, credentials_json, credential_hint, source_sk_hint, extra_json, status, schedulable, concurrency, priority, rate_multiplier, proxy_pool_id, proxy_id, auto_proxy, base_rpm, rpm_strategy, rpm_sticky_buffer, user_msg_queue_mode, strategy_id, auth_status, auth_checked_at, token_expires_at, subscription_type, rate_limit_tier, account_price, onboarded_at) VALUES (?, 'anthropic', ?, ?, ?, ?, ?, 'active', 1, ?, 50, 1, ?, ?, 1, ?, ?, ?, 'off', ?, 'valid', `+nowSQL+`, ?, ?, ?, ?, `+nowSQL+`)`,
		name, authType, string(encoded), credentialHint(string(encoded)), sourceSKHint(sessionKey), string(extraJSON), input.Concurrency, input.ProxyPoolID, nil, input.BaseRPM, input.RPMStrategy, input.RPMStickyBuffer, strategyID, expiresAt, token.SubscriptionType, token.RateLimitTier, input.AccountPrice)
	if err != nil {
		return 0, err
	}
	accountID, _ := result.LastInsertId()
	poolID := input.ProxyPoolID
	requestedProxyID := proxyID
	assignedProxyID, err := assignAccountProxy(tx, accountID, &poolID, &requestedProxyID, false)
	if err != nil {
		return 0, err
	}
	if assignedProxyID == nil {
		return 0, errors.New("authorized account proxy is unavailable")
	}
	if _, err := tx.Exec(`UPDATE accounts SET proxy_id = ? WHERE id = ?`, *assignedProxyID, accountID); err != nil {
		return 0, err
	}
	if err := recordProxyAssignment(tx, *assignedProxyID, accountID); err != nil {
		return 0, err
	}
	if err := setAccountGroups(tx, accountID, input.GroupIDs, 50); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, 'onboarded')`, accountID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return accountID, nil
}

func (a *app) updateBatchAuthorizedAccount(accountID int64, input batchAuthorizationInput, authType, sessionKey string, token *claudeTokenInfo, leaseOwner string) error {
	encoded, err := json.Marshal(token)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previousOnboarded, previousInvalidated sql.NullString
	if err := tx.QueryRow(`SELECT onboarded_at, invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&previousOnboarded, &previousInvalidated); err != nil {
		return err
	}
	expiresAt := time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339Nano)
	result, err := tx.Exec(`UPDATE accounts SET auth_type = ?, credentials_json = ?, credential_hint = ?, source_sk_hint = ?, auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, token_expires_at = ?, subscription_type = ?, rate_limit_tier = ?, reauthorized_at = CASE WHEN ? THEN `+nowSQL+` ELSE reauthorized_at END, reauthorization_count = reauthorization_count + CASE WHEN ? THEN 1 ELSE 0 END, onboarded_at = CASE WHEN onboarded_at IS NULL OR invalidated_at IS NOT NULL THEN `+nowSQL+` ELSE onboarded_at END, invalidated_at = NULL, archived_at = NULL, archived_proxy_id = NULL, error_message = '', rate_limit_reset_at = NULL, rate_limit_window = '', rate_limit_reason = '', consecutive_429 = 0, last_429_at = NULL, rate_limit_downweight_until = NULL, status = 'active', schedulable = 1, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL
		AND EXISTS (SELECT 1 FROM account_token_leases lease WHERE lease.account_id = accounts.id AND lease.owner = ? AND lease.expires_at > CAST(strftime('%s','now') AS INTEGER))`,
		authType, string(encoded), credentialHint(string(encoded)), sourceSKHint(sessionKey), expiresAt, token.SubscriptionType, token.RateLimitTier, previousOnboarded.Valid, previousOnboarded.Valid, accountID, leaseOwner)
	if err != nil {
		return err
	}
	if updated, _ := result.RowsAffected(); updated == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.Exec(`DELETE FROM account_rpm_thresholds WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	if err := setAccountGroups(tx, accountID, input.GroupIDs, 50); err != nil {
		return err
	}
	if !previousOnboarded.Valid || previousInvalidated.Valid {
		if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, 'onboarded')`, accountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
