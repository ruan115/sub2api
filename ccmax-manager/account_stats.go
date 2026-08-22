package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
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
	SessionKeys  []string `json:"session_keys"`
	ProxyPoolID  int64    `json:"proxy_pool_id"`
	GroupIDs     []string `json:"group_ids"`
	AuthType     string   `json:"auth_type"`
	AccountPrice float64  `json:"account_price"`
	Concurrency  int      `json:"concurrency"`
	BaseRPM      int      `json:"base_rpm"`
}

type batchAuthorizationResult struct {
	Index        int    `json:"index"`
	AccountID    int64  `json:"account_id,omitempty"`
	Name         string `json:"name,omitempty"`
	ProxyIP      string `json:"proxy_ip,omitempty"`
	Subscription string `json:"subscription_type,omitempty"`
	Success      bool   `json:"success"`
	Skipped      bool   `json:"skipped,omitempty"`
	Error        string `json:"error,omitempty"`
}

type batchAuthorizationResponse struct {
	Total   int                        `json:"total"`
	Success int                        `json:"success"`
	Skipped int                        `json:"skipped"`
	Failed  int                        `json:"failed"`
	Items   []batchAuthorizationResult `json:"items"`
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

func accountStatePredicate(alias, state string) string {
	errorState := "(" + alias + ".status = 'error' OR " + alias + ".invalidated_at IS NOT NULL OR (" + alias + ".auth_status = 'reauth_required' AND " + alias + ".onboarded_at IS NOT NULL))"
	normalState := "(" + alias + ".status = 'active' AND " + alias + ".schedulable = 1 AND " + alias + ".auth_status = 'valid' " +
		"AND " + alias + ".invalidated_at IS NULL " +
		"AND (" + alias + ".expires_at IS NULL OR " + alias + ".expires_at > " + nowSQL + ") " +
		"AND (" + alias + ".rate_limit_reset_at IS NULL OR " + alias + ".rate_limit_reset_at <= " + nowSQL + ") " +
		"AND EXISTS (SELECT 1 FROM proxies state_proxy WHERE state_proxy.id = " + alias + ".proxy_id AND state_proxy.status = 'active' AND state_proxy.deleted_at IS NULL))"
	switch state {
	case "normal":
		return normalState
	case "error":
		return errorState
	case "unavailable":
		return "(NOT " + errorState + " AND NOT " + normalState + ")"
	default:
		return "1 = 1"
	}
}

func (a *app) recordAccountLifecycle(accountID int64, eventType string) {
	if eventType != "onboarded" && eventType != "invalidated" {
		return
	}
	_, _ = a.db.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, ?)`, accountID, eventType)
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

func subscriptionTypeFromCredentials(credentials map[string]any) string {
	if value, ok := credentials["subscription_type"].(string); ok {
		if normalized := normalizeSubscriptionType(value); normalized != "" {
			return normalized
		}
	}
	accessToken, _ := credentials["access_token"].(string)
	return subscriptionTypeFromToken(accessToken)
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
	_, _ = a.db.Exec(`INSERT INTO authorization_logs (account_id, account_name, proxy_id, proxy_ip, method, success, status_message, subscription_type, client_ip) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		accountID, accountName, proxyID, proxyIP, method, boolInt(success), message, subscription, clientIP)
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
	days := 30
	if value, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && value > 0 {
		days = min(value, 365)
	}
	start := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)).AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	items := map[string]*dailyStat{}
	for index := 0; index < days; index++ {
		date := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)).AddDate(0, 0, -index).Format("2006-01-02")
		items[date] = &dailyStat{Date: date}
	}
	usageWhere := `date(created_at, '` + shanghaiOffset + `') >= ?`
	usageArgs := []any{start}
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
		var date string
		var requests, inputTokens, outputTokens int64
		var billed, actual float64
		if scanErr := usageRows.Scan(&date, &requests, &inputTokens, &outputTokens, &billed, &actual); scanErr != nil {
			usageRows.Close()
			writeDBError(w, scanErr)
			return
		}
		if item := items[date]; item != nil {
			item.Requests, item.InputTokens, item.OutputTokens, item.BilledCost, item.ActualCost = requests, inputTokens, outputTokens, billed, actual
		}
	}
	usageRows.Close()
	if err := a.mergeDailyAccountEvents(items, start, "onboarded", user, func(item *dailyStat, count int64) { item.AccountsOnboarded = count }); err != nil {
		writeDBError(w, err)
		return
	}
	if err := a.mergeDailyAccountEvents(items, start, "invalidated", user, func(item *dailyStat, count int64) { item.AccountsDied = count }); err != nil {
		writeDBError(w, err)
		return
	}
	authWhere := `date(created_at, '` + shanghaiOffset + `') >= ?`
	authArgs := []any{start}
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
		var date string
		var total, successful int64
		if scanErr := authRows.Scan(&date, &total, &successful); scanErr != nil {
			authRows.Close()
			writeDBError(w, scanErr)
			return
		}
		if item := items[date]; item != nil {
			item.Authorizations, item.AuthSuccessful = total, successful
		}
	}
	authRows.Close()
	result := make([]dailyStat, 0, days)
	for index := 0; index < days; index++ {
		date := time.Now().In(time.FixedZone("Asia/Shanghai", 8*60*60)).AddDate(0, 0, -index).Format("2006-01-02")
		result = append(result, *items[date])
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) mergeDailyAccountEvents(items map[string]*dailyStat, start, eventType string, user panelUser, assign func(*dailyStat, int64)) error {
	if eventType != "onboarded" && eventType != "invalidated" {
		return errors.New("invalid account event column")
	}
	where := `event_type = ? AND date(created_at, '` + shanghaiOffset + `') >= ?`
	args := []any{eventType, start}
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
		var date string
		var count int64
		if err := rows.Scan(&date, &count); err != nil {
			return err
		}
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
	if input.Concurrency <= 0 {
		input.Concurrency = 10
	}
	if input.BaseRPM < 0 || input.BaseRPM > 10000 {
		writeError(w, http.StatusBadRequest, "base RPM is invalid")
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
	response := batchAuthorizationResponse{Total: len(keys), Items: make([]batchAuthorizationResult, 0, len(keys))}
	poolID := input.ProxyPoolID
	for index, sessionKey := range keys {
		item := batchAuthorizationResult{Index: index + 1}
		proxyID, proxyURL, selectErr := a.selectProxyForNewAccount(&poolID, nil, true)
		if selectErr != nil {
			item.Error = selectErr.Error()
			response.Failed++
			response.Items = append(response.Items, item)
			a.recordAuthorization(nil, nil, "", "batch_session_key", false, item.Error, "", requestIP(r))
			continue
		}
		token, exchangeErr := exchangeClaudeSessionKey(r.Context(), sessionKey, apiScope, proxyURL)
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
			item.AccountID = existingID
			item.Name = existingName
			item.Subscription = token.SubscriptionType
			item.Skipped = true
			item.Error = "同邮箱账号已存在，未重复创建"
			response.Skipped++
			response.Items = append(response.Items, item)
			a.recordAuthorization(&existingID, proxyID, item.Name, "batch_session_key", true, item.Error, token.SubscriptionType, requestIP(r))
			continue
		}
		accountID, createErr := a.createBatchAuthorizedAccount(input, authType, item.Name, token, *proxyID)
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

func (a *app) createBatchAuthorizedAccount(input batchAuthorizationInput, authType, name string, token *claudeTokenInfo, proxyID int64) (int64, error) {
	encoded, err := json.Marshal(token)
	if err != nil {
		return 0, err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	expiresAt := time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339Nano)
	result, err := tx.Exec(`INSERT INTO accounts (name, platform, auth_type, credentials_json, credential_hint, extra_json, status, schedulable, concurrency, priority, rate_multiplier, proxy_pool_id, proxy_id, auto_proxy, base_rpm, rpm_strategy, rpm_sticky_buffer, user_msg_queue_mode, auth_status, auth_checked_at, token_expires_at, subscription_type, account_price, onboarded_at) VALUES (?, 'anthropic', ?, ?, ?, '{}', 'active', 1, ?, 50, 1, ?, ?, 1, ?, 'tiered', 0, 'off', 'valid', `+nowSQL+`, ?, ?, ?, `+nowSQL+`)`,
		name, authType, string(encoded), credentialHint(string(encoded)), input.Concurrency, input.ProxyPoolID, proxyID, input.BaseRPM, expiresAt, token.SubscriptionType, input.AccountPrice)
	if err != nil {
		return 0, err
	}
	accountID, _ := result.LastInsertId()
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
