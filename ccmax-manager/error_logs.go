package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
)

type errorLogItem struct {
	Source              string   `json:"source"`
	SourceID            int64    `json:"source_id"`
	StatusCode          int      `json:"status_code"`
	Category            string   `json:"category"`
	Message             string   `json:"message"`
	AccountID           *int64   `json:"account_id"`
	AccountName         string   `json:"account_name"`
	GroupIDs            []string `json:"group_ids"`
	ProxyID             *int64   `json:"proxy_id"`
	ProxyIP             string   `json:"proxy_ip"`
	Actor               string   `json:"actor"`
	Method              string   `json:"method"`
	Path                string   `json:"path"`
	RequestID           string   `json:"request_id"`
	ClientRequestID     string   `json:"client_request_id"`
	TraceID             string   `json:"trace_id"`
	UpstreamRequestID   string   `json:"upstream_request_id"`
	DispatchDiagnostics string   `json:"dispatch_diagnostics"`
	DurationMS          int64    `json:"duration_ms"`
	CreatedAt           string   `json:"created_at"`
}

type errorLogSummary struct {
	Total         int64 `json:"total"`
	Accounts      int64 `json:"accounts"`
	Authorization int64 `json:"authorization"`
	Gateway       int64 `json:"gateway"`
	Proxies       int64 `json:"proxies"`
	Audit         int64 `json:"audit"`
	System        int64 `json:"system"`
}

type errorLogResponse struct {
	Summary       errorLogSummary `json:"summary"`
	Items         []errorLogItem  `json:"items"`
	Page          int             `json:"page"`
	PageSize      int             `json:"page_size"`
	TotalPages    int             `json:"total_pages"`
	RetentionDays int             `json:"retention_days"`
}

const errorLogPruneInterval = time.Hour

var (
	errorStatusPattern          = regexp.MustCompile(`(?i)(?:http|status(?:\s+code)?|oauth)[^0-9]{0,12}([1-5][0-9]{2})`)
	errorSecretPattern          = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}\b`)
	errorBearerPattern          = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{12,}=*`)
	errorCredentialFieldPattern = regexp.MustCompile(`(?i)((?:access[_ -]?token|refresh[_ -]?token|session[_ -]?key|api[_ -]?key|authorization|cookie)\s*[:=]\s*["']?)([^"'\s,;}]{8,})`)
	errorHTMLPattern            = regexp.MustCompile(`(?is)<(?:!doctype|html|head|body)\b`)
)

const errorEventsCTE = `WITH error_events AS (
	SELECT 'account' AS source, a.id AS source_id, 0 AS status_code,
		CASE WHEN TRIM(a.auth_error) != '' OR a.auth_status = 'reauth_required' THEN 'account_auth' ELSE 'account_state' END AS category,
		CASE WHEN TRIM(a.auth_error) != '' THEN a.auth_error WHEN TRIM(a.error_message) != '' THEN a.error_message ELSE '账号状态异常' END AS message,
		a.id AS account_id, a.name AS account_name,
		COALESCE((SELECT GROUP_CONCAT(ag.group_id, ',') FROM account_groups ag WHERE ag.account_id = a.id), '') AS group_ids,
		a.proxy_id AS proxy_id, COALESCE(NULLIF(px.exit_ip, ''), px.host, '') AS proxy_ip,
		'' AS actor, '' AS method, '' AS path, '' AS request_id, '' AS client_request_id, '' AS trace_id, '' AS upstream_request_id, '' AS dispatch_diagnostics, 0 AS duration_ms,
		COALESCE(a.auth_checked_at, a.invalidated_at, a.updated_at) AS created_at
	FROM accounts a
	LEFT JOIN proxies px ON px.id = a.proxy_id
	WHERE a.deleted_at IS NULL AND a.archived_at IS NULL AND (TRIM(a.auth_error) != '' OR TRIM(a.error_message) != '' OR a.status = 'error' OR a.auth_status = 'reauth_required')

	UNION ALL

	SELECT 'authorization', l.id, 0, 'authorization',
		CASE WHEN TRIM(l.status_message) != '' THEN l.status_message ELSE '账号授权失败' END,
		l.account_id, l.account_name,
		COALESCE((SELECT GROUP_CONCAT(ag.group_id, ',') FROM account_groups ag WHERE ag.account_id = l.account_id), ''),
		l.proxy_id, l.proxy_ip, '', l.method, '', '', '', '', '', '', 0, l.created_at
	FROM authorization_logs l
	WHERE l.success = 0

	UNION ALL

	SELECT 'gateway', ge.id, ge.status_code, COALESCE(NULLIF(ge.category, ''), 'gateway_request'), ge.message,
		ge.account_id, COALESCE(NULLIF(a.name, ''), NULLIF(u.name, ''), u.username, ''), ge.group_id,
		a.proxy_id, COALESCE(NULLIF(px.exit_ip, ''), px.host, ''), COALESCE(NULLIF(u.username, ''), 'anonymous'), ge.method, ge.path,
		ge.request_id, ge.client_request_id, ge.trace_id, ge.upstream_request_id, ge.dispatch_diagnostics, ge.duration_ms, ge.created_at
	FROM gateway_error_logs ge
	LEFT JOIN users u ON u.id = ge.user_id
	LEFT JOIN accounts a ON a.id = ge.account_id
	LEFT JOIN proxies px ON px.id = a.proxy_id

	UNION ALL

	SELECT 'proxy', px.id, 0, 'proxy_test',
		CASE WHEN TRIM(px.last_error) != '' THEN px.last_error ELSE '代理最近一次检测失败' END,
		a.id, COALESCE(a.name, ''),
		COALESCE((SELECT GROUP_CONCAT(ag.group_id, ',') FROM account_groups ag WHERE ag.account_id = a.id), ''),
		px.id, COALESCE(NULLIF(px.exit_ip, ''), px.host), '', 'TEST', '', '', '', '', '', '', 0, COALESCE(px.last_test_at, px.updated_at)
	FROM proxies px
	LEFT JOIN accounts a ON a.proxy_id = px.id AND a.deleted_at IS NULL AND a.archived_at IS NULL
	WHERE px.deleted_at IS NULL AND px.status = 'error'

	UNION ALL

	SELECT 'proxy', pp.id, 0, 'proxy_pool_sync', pp.last_error,
		NULL, '', '', NULL, '', '', 'SYNC', '', '', '', '', '', '', 0, COALESCE(pp.last_sync_at, pp.updated_at)
	FROM proxy_pools pp
	WHERE pp.deleted_at IS NULL AND TRIM(pp.last_error) != ''

	UNION ALL

	SELECT 'audit', al.id, al.status_code, 'admin_request',
		al.action || ' · ' || al.method || ' ' || al.path || ' 返回 HTTP ' || al.status_code,
		NULL, '', '', NULL, '', al.actor_username, al.method, al.path, '', '', '', '', '', al.duration_ms, al.created_at
	FROM audit_logs al
	WHERE al.status_code >= 400

	UNION ALL

	SELECT 'system', ps.id, 0, 'pricing_sync',
		CASE WHEN TRIM(ps.last_error) != '' THEN ps.last_error ELSE '模型价格同步失败' END,
		NULL, '', '', NULL, '', 'system', 'SYNC', ps.remote_url, '', '', '', '', '', 0, COALESCE(ps.last_checked_at, ps.last_synced_at, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	FROM pricing_sync_state ps
	WHERE ps.status = 'error' OR TRIM(ps.last_error) != ''
)`

func errorGroupIDPredicate(dialect databaseDialect) string {
	if dialect == dialectMySQL {
		return "FIND_IN_SET(?, group_ids) > 0"
	}
	return "instr(',' || group_ids || ',', ',' || ? || ',') > 0"
}

func appendScopedErrorGroups(dialect databaseDialect, user panelUser, where *[]string, args *[]any) {
	if !isScopedUserRole(user.Role) {
		return
	}
	groups := scopedGroupIDs(user)
	if len(groups) == 0 {
		*where = append(*where, "0 = 1")
		return
	}
	predicates := make([]string, 0, len(groups))
	for _, groupID := range groups {
		predicates = append(predicates, errorGroupIDPredicate(dialect))
		*args = append(*args, groupID)
	}
	*where = append(*where, "("+strings.Join(predicates, " OR ")+")")
}

func (a *app) handleErrorLogs(w http.ResponseWriter, r *http.Request) {
	where := []string{"1 = 1"}
	args := []any{}
	user := currentUser(r)
	appendScopedErrorGroups(a.db.dialect, user, &where, &args)
	if source := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("source"))); source != "" {
		switch source {
		case "account", "authorization", "gateway", "proxy", "audit", "system":
			where = append(where, "source = ?")
			args = append(args, source)
		default:
			writeError(w, http.StatusBadRequest, "invalid error source")
			return
		}
	}
	if groupID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))); groupID != "" {
		if !groupIDPattern.MatchString(groupID) {
			writeError(w, http.StatusBadRequest, "invalid group")
			return
		}
		if err := a.validateGroupIDs([]string{groupID}, false); err != nil {
			writeError(w, http.StatusBadRequest, "invalid group")
			return
		}
		if isScopedUserRole(user.Role) && !userCanAccessGroup(user, groupID) {
			writeError(w, http.StatusForbidden, "group permission denied")
			return
		}
		where = append(where, errorGroupIDPredicate(a.db.dialect))
		args = append(args, groupID)
	}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		pattern := "%" + search + "%"
		where = append(where, "(message LIKE ? OR category LIKE ? OR account_name LIKE ? OR proxy_ip LIKE ? OR actor LIKE ? OR method LIKE ? OR path LIKE ? OR request_id LIKE ? OR client_request_id LIKE ? OR trace_id LIKE ? OR upstream_request_id LIKE ?)")
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	from, to := a.errorLogTimeBounds(r)
	if from != "" {
		where = append(where, "created_at >= ?")
		args = append(args, from)
	}
	if to != "" {
		where = append(where, "created_at < ?")
		args = append(args, to)
	}

	clause := strings.Join(where, " AND ")
	result := errorLogResponse{Items: []errorLogItem{}, RetentionDays: a.errorRetention}
	summaryRows, err := a.db.Query(errorEventsCTE+` SELECT source, COUNT(*) FROM error_events WHERE `+clause+` GROUP BY source`, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	for summaryRows.Next() {
		var source string
		var count int64
		if err := summaryRows.Scan(&source, &count); err != nil {
			summaryRows.Close()
			writeDBError(w, err)
			return
		}
		result.Summary.Total += count
		switch source {
		case "account":
			result.Summary.Accounts = count
		case "authorization":
			result.Summary.Authorization = count
		case "gateway":
			result.Summary.Gateway = count
		case "proxy":
			result.Summary.Proxies = count
		case "audit":
			result.Summary.Audit = count
		case "system":
			result.Summary.System = count
		}
	}
	if err := summaryRows.Err(); err != nil {
		summaryRows.Close()
		writeDBError(w, err)
		return
	}
	if err := summaryRows.Close(); err != nil {
		writeDBError(w, err)
		return
	}

	page, pageSize, offset := paginationFromRequest(r, 20, 100)
	result.Page, result.PageSize = page, pageSize
	result.TotalPages = totalPages(result.Summary.Total, pageSize)
	queryArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := a.db.Query(errorEventsCTE+` SELECT source, source_id, status_code, category, message, account_id, account_name, group_ids, proxy_id, proxy_ip, actor, method, path, request_id, client_request_id, trace_id, upstream_request_id, dispatch_diagnostics, duration_ms, created_at FROM error_events WHERE `+clause+` ORDER BY created_at DESC, source_id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var item errorLogItem
		var accountID, proxyID sql.NullInt64
		var groupIDs string
		if err := rows.Scan(&item.Source, &item.SourceID, &item.StatusCode, &item.Category, &item.Message, &accountID, &item.AccountName, &groupIDs, &proxyID, &item.ProxyIP, &item.Actor, &item.Method, &item.Path, &item.RequestID, &item.ClientRequestID, &item.TraceID, &item.UpstreamRequestID, &item.DispatchDiagnostics, &item.DurationMS, &item.CreatedAt); err != nil {
			writeDBError(w, err)
			return
		}
		item.AccountID = nullIntPointer(accountID)
		item.ProxyID = nullIntPointer(proxyID)
		item.GroupIDs = splitErrorGroupIDs(groupIDs)
		item.Message = sanitizeErrorMessage(item.Message)
		if item.StatusCode == 0 {
			item.StatusCode = inferErrorStatus(item.Message)
		}
		result.Items = append(result.Items, item)
	}
	if err := rows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

type errorInsightAccount struct {
	AccountID     int64   `json:"account_id"`
	AccountName   string  `json:"account_name"`
	BaseRPM       int     `json:"base_rpm"`
	Count         int64   `json:"count"`
	AvgRPM        float64 `json:"avg_rpm"`
	MaxRPM        int64   `json:"max_rpm"`
	MaxTPM        int64   `json:"max_tpm"`
	MaxITPM       int64   `json:"max_itpm"`
	MaxInflight   int64   `json:"max_inflight"`
	TotalRequests int64   `json:"total_requests"`
}

type errorInsightEvent struct {
	AccountID     int64  `json:"account_id"`
	AccountName   string `json:"account_name"`
	StatusCode    int    `json:"status_code"`
	Category      string `json:"category"`
	Message       string `json:"message"`
	RPM           int64  `json:"rpm"`
	TPM           int64  `json:"tpm"`
	ITPM          int64  `json:"itpm"`
	Inflight      int64  `json:"inflight"`
	TotalRequests int64  `json:"total_requests"`
	CreatedAt     string `json:"created_at"`
}

type errorInsightTimelinePoint struct {
	Bucket string `json:"bucket"`
	Count  int64  `json:"count"`
}

type errorInsightResponse struct {
	Status              int                         `json:"status"`
	Accounts            []errorInsightAccount       `json:"accounts"`
	Events              []errorInsightEvent         `json:"events"`
	Timeline            []errorInsightTimelinePoint `json:"timeline"`
	TimelineGranularity string                      `json:"timeline_granularity"`
}

// handleErrorInsights aggregates gateway errors of one status code (401 by
// default) per account, together with the load snapshots captured at error
// time, so the UI can chart whether RPM pressure correlates with the errors.
func (a *app) handleErrorInsights(w http.ResponseWriter, r *http.Request) {
	status := 401
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		parsed := inferErrorStatus("status " + raw)
		if parsed == 0 {
			writeError(w, http.StatusBadRequest, "invalid status code")
			return
		}
		status = parsed
	}
	where := []string{"ge.status_code = ?", "ge.account_id IS NOT NULL"}
	args := []any{status}
	user := currentUser(r)
	if isScopedUserRole(user.Role) {
		condition, scopeArgs := scopedGroupCondition(user, "ge.group_id")
		where = append(where, condition)
		args = append(args, scopeArgs...)
	}
	if groupID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))); groupID != "" {
		if !groupIDPattern.MatchString(groupID) {
			writeError(w, http.StatusBadRequest, "invalid group")
			return
		}
		if err := a.validateGroupIDs([]string{groupID}, false); err != nil {
			writeError(w, http.StatusBadRequest, "invalid group")
			return
		}
		if isScopedUserRole(user.Role) && !userCanAccessGroup(user, groupID) {
			writeError(w, http.StatusForbidden, "group permission denied")
			return
		}
		where = append(where, "ge.group_id = ?")
		args = append(args, groupID)
	}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		pattern := "%" + search + "%"
		where = append(where, "(ge.message LIKE ? OR ge.category LIKE ? OR a.name LIKE ? OR ge.request_id LIKE ? OR ge.client_request_id LIKE ? OR ge.trace_id LIKE ? OR ge.upstream_request_id LIKE ?)")
		args = append(args, pattern, pattern, pattern, pattern, pattern, pattern, pattern)
	}
	from, to := a.errorLogTimeBounds(r)
	if from != "" {
		where = append(where, "ge.created_at >= ?")
		args = append(args, from)
	}
	if to != "" {
		where = append(where, "ge.created_at < ?")
		args = append(args, to)
	}
	clause := strings.Join(where, " AND ")
	granularity := errorInsightGranularity(from, to)
	result := errorInsightResponse{
		Status: status, Accounts: []errorInsightAccount{}, Events: []errorInsightEvent{},
		Timeline: []errorInsightTimelinePoint{}, TimelineGranularity: granularity,
	}

	rows, err := a.db.Query(`SELECT ge.account_id, COALESCE(a.name, ''), COALESCE(a.base_rpm, 0), COUNT(*),
		COALESCE(AVG(CASE WHEN ge.rpm_snapshot >= 0 THEN ge.rpm_snapshot END), 0),
		COALESCE(MAX(ge.rpm_snapshot), -1), COALESCE(MAX(ge.tpm_snapshot), -1), COALESCE(MAX(ge.itpm_snapshot), -1), COALESCE(MAX(ge.inflight_snapshot), -1), COALESCE(MAX(ge.total_requests), -1)
		FROM gateway_error_logs ge LEFT JOIN accounts a ON a.id = ge.account_id
		WHERE `+clause+` GROUP BY ge.account_id ORDER BY COUNT(*) DESC LIMIT 40`, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	for rows.Next() {
		var item errorInsightAccount
		if err := rows.Scan(&item.AccountID, &item.AccountName, &item.BaseRPM, &item.Count, &item.AvgRPM, &item.MaxRPM, &item.MaxTPM, &item.MaxITPM, &item.MaxInflight, &item.TotalRequests); err != nil {
			rows.Close()
			writeDBError(w, err)
			return
		}
		result.Accounts = append(result.Accounts, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeDBError(w, err)
		return
	}
	rows.Close()

	bucketExpression := errorInsightBucketExpression(a.db.dialect, granularity)
	timelineRows, err := a.db.Query(`SELECT `+bucketExpression+` AS bucket, COUNT(*)
		FROM gateway_error_logs ge LEFT JOIN accounts a ON a.id = ge.account_id
		WHERE `+clause+` GROUP BY bucket ORDER BY bucket`, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	for timelineRows.Next() {
		var item errorInsightTimelinePoint
		if err := timelineRows.Scan(&item.Bucket, &item.Count); err != nil {
			timelineRows.Close()
			writeDBError(w, err)
			return
		}
		result.Timeline = append(result.Timeline, item)
	}
	if err := timelineRows.Err(); err != nil {
		timelineRows.Close()
		writeDBError(w, err)
		return
	}
	timelineRows.Close()

	eventRows, err := a.db.Query(`SELECT ge.account_id, COALESCE(a.name, ''), ge.status_code, ge.category, ge.message,
		ge.rpm_snapshot, ge.tpm_snapshot, ge.itpm_snapshot, ge.inflight_snapshot, ge.total_requests, ge.created_at
		FROM gateway_error_logs ge LEFT JOIN accounts a ON a.id = ge.account_id
		WHERE `+clause+` ORDER BY ge.created_at DESC LIMIT 50`, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer eventRows.Close()
	for eventRows.Next() {
		var item errorInsightEvent
		if err := eventRows.Scan(&item.AccountID, &item.AccountName, &item.StatusCode, &item.Category, &item.Message, &item.RPM, &item.TPM, &item.ITPM, &item.Inflight, &item.TotalRequests, &item.CreatedAt); err != nil {
			writeDBError(w, err)
			return
		}
		item.Message = sanitizeErrorMessage(item.Message)
		result.Events = append(result.Events, item)
	}
	if err := eventRows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) errorLogTimeBounds(r *http.Request) (string, string) {
	from := normalizeDateStart(strings.TrimSpace(r.URL.Query().Get("from")))
	if from == "" {
		days := a.errorRetention
		if days <= 0 {
			days = 7
		}
		from = time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339Nano)
	}
	return from, normalizeDateEnd(strings.TrimSpace(r.URL.Query().Get("to")))
}

func errorInsightGranularity(from, to string) string {
	start, err := time.Parse(time.RFC3339Nano, from)
	if err != nil {
		return "day"
	}
	end := time.Now().UTC()
	if to != "" {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, to); parseErr == nil {
			end = parsed
		}
	}
	if end.After(start) && end.Sub(start) <= 48*time.Hour {
		return "hour"
	}
	return "day"
}

func errorInsightBucketExpression(dialect databaseDialect, granularity string) string {
	if dialect == dialectMySQL {
		if granularity == "hour" {
			return `DATE_FORMAT(DATE_ADD(ge.created_at, INTERVAL 8 HOUR), '%Y-%m-%dT%H:00:00+08:00')`
		}
		return `DATE_FORMAT(DATE_ADD(ge.created_at, INTERVAL 8 HOUR), '%Y-%m-%dT00:00:00+08:00')`
	}
	if granularity == "hour" {
		return `strftime('%Y-%m-%dT%H:00:00+08:00', ge.created_at, '+8 hours')`
	}
	return `strftime('%Y-%m-%dT00:00:00+08:00', ge.created_at, '+8 hours')`
}

func (a *app) pruneGatewayErrorLogs(force bool) error {
	a.errorPruneMu.Lock()
	defer a.errorPruneMu.Unlock()
	now := time.Now().UTC()
	if !force && !a.lastErrorPrune.IsZero() && now.Sub(a.lastErrorPrune) < errorLogPruneInterval {
		return nil
	}
	days := a.errorRetention
	if days <= 0 {
		days = 7
	}
	cutoff := now.AddDate(0, 0, -days).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`DELETE FROM gateway_error_logs WHERE created_at < ?`, cutoff); err != nil {
		return fmt.Errorf("prune gateway error logs: %w", err)
	}
	a.lastErrorPrune = now
	return nil
}

func splitErrorGroupIDs(value string) []string {
	result := []string{}
	seen := map[string]bool{}
	for _, groupID := range strings.Split(value, ",") {
		groupID = strings.TrimSpace(groupID)
		if groupIDPattern.MatchString(groupID) && !seen[groupID] {
			seen[groupID] = true
			result = append(result, groupID)
		}
	}
	return result
}

func inferErrorStatus(message string) int {
	match := errorStatusPattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return 0
	}
	status := 0
	for _, digit := range match[1] {
		status = status*10 + int(digit-'0')
	}
	return status
}

func sanitizeErrorMessage(message string) string {
	message = errorSecretPattern.ReplaceAllStringFunc(message, func(secret string) string {
		if len(secret) <= 12 {
			return "sk-••••"
		}
		return secret[:6] + "…" + secret[len(secret)-4:]
	})
	message = errorBearerPattern.ReplaceAllString(message, "Bearer [REDACTED]")
	return errorCredentialFieldPattern.ReplaceAllString(message, "${1}[REDACTED]")
}

func classifyGatewayUpstreamError(status int, body []byte) (string, string) {
	detail := strings.TrimSpace(upstreamErrorMessage(body))
	lower := strings.ToLower(detail)
	category := "upstream_error"
	summary := ""
	switch status {
	case http.StatusUnauthorized:
		category = "upstream_authentication_rejected"
		summary = "upstream rejected the OAuth access token"
		if strings.Contains(lower, "revoked") {
			category = "upstream_authentication_revoked"
			summary = "upstream OAuth access token was revoked"
		}
	case http.StatusForbidden:
		category = "upstream_forbidden"
		summary = "upstream denied access"
		switch {
		case strings.Contains(lower, "cloudflare"), strings.Contains(lower, "cf-ray"), strings.Contains(lower, "challenge"), strings.Contains(lower, "attention required"):
			category = "upstream_forbidden_proxy_challenge"
			summary = "residential proxy exit triggered an upstream edge challenge"
		case strings.Contains(lower, "identity verification"):
			category = "upstream_forbidden_identity_verification"
			summary = "upstream requires identity verification"
		case strings.Contains(lower, "oauth authentication") && strings.Contains(lower, "organization"):
			category = "upstream_forbidden_oauth_policy"
			summary = "organization policy does not allow OAuth authentication"
		}
	case http.StatusTooManyRequests:
		category = "upstream_rate_limited"
		summary = "upstream rate or quota limit was reached"
	case 529:
		category = "upstream_overloaded"
		summary = "upstream service is overloaded"
	default:
		if status >= http.StatusInternalServerError {
			category = "upstream_service_error"
			summary = "upstream service returned a server error"
		} else if status == http.StatusBadRequest {
			category = "upstream_bad_request"
		}
	}
	if summary == "" {
		if detail == "" || errorHTMLPattern.MatchString(detail) {
			summary = http.StatusText(status)
		} else {
			summary = sanitizeErrorMessage(detail)
		}
	}
	if runes := []rune(summary); len(runes) > 300 {
		summary = string(runes[:300])
	}
	return category, fmt.Sprintf("Upstream HTTP %d · %s", status, summary)
}

func attributeGatewayUpstreamError(w http.ResponseWriter, upstreamStatus int, body []byte, countTokens bool) {
	category, message := classifyGatewayUpstreamError(upstreamStatus, body)
	status := upstreamStatus
	if !countTokens && upstreamStatus != http.StatusBadRequest {
		status, _, _ = sub2CompatibilityError(upstreamStatus)
	}
	attributeGatewayErrorEvent(w, category, status, message)
}

const gatewayErrorCaptureLimit = 16 << 10

type gatewayErrorResponseWriter struct {
	http.ResponseWriter
	status              int
	body                []byte
	accountID           int64
	category            string
	eventStatus         int
	eventMessage        string
	dispatchDiagnostics string
	durationMS          int64
}

func (w *gatewayErrorResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *gatewayErrorResponseWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status >= 400 && len(w.body) < gatewayErrorCaptureLimit {
		remaining := gatewayErrorCaptureLimit - len(w.body)
		w.body = append(w.body, payload[:min(len(payload), remaining)]...)
	}
	return w.ResponseWriter.Write(payload)
}

func (w *gatewayErrorResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *gatewayErrorResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *gatewayErrorResponseWriter) setGatewayAccountID(accountID int64) {
	if accountID > 0 {
		w.accountID = accountID
	}
}

func (w *gatewayErrorResponseWriter) clearGatewayAccountID() {
	w.accountID = 0
}

func (w *gatewayErrorResponseWriter) setGatewayErrorEvent(category string, status int, message string) {
	if strings.TrimSpace(category) == "" {
		return
	}
	w.category = strings.TrimSpace(category)
	w.eventStatus = status
	w.eventMessage = strings.TrimSpace(message)
}

func (w *gatewayErrorResponseWriter) setGatewayDispatchDiagnostics(diagnostics gatewayCapacityDiagnostics) {
	payload, err := json.Marshal(diagnostics)
	if err != nil {
		return
	}
	if len(payload) > maxDispatchDiagnosticsLen {
		payload = payload[:maxDispatchDiagnosticsLen]
	}
	w.dispatchDiagnostics = string(payload)
}

func (w *gatewayErrorResponseWriter) gatewayResponseStatus() int {
	return w.status
}

type gatewayErrorAccountSetter interface {
	setGatewayAccountID(int64)
}

type gatewayErrorAccountClearer interface {
	clearGatewayAccountID()
}

type gatewayErrorEventSetter interface {
	setGatewayErrorEvent(string, int, string)
}

type gatewayDispatchDiagnosticsSetter interface {
	setGatewayDispatchDiagnostics(gatewayCapacityDiagnostics)
}

type gatewayResponseStatusReader interface {
	gatewayResponseStatus() int
}

type responseWriterUnwrapper interface {
	Unwrap() http.ResponseWriter
}

func attributeGatewayErrorAccount(w http.ResponseWriter, accountID int64) {
	if accountID <= 0 {
		return
	}
	for w != nil {
		if setter, ok := w.(gatewayErrorAccountSetter); ok {
			setter.setGatewayAccountID(accountID)
			return
		}
		unwrapper, ok := w.(responseWriterUnwrapper)
		if !ok {
			return
		}
		next := unwrapper.Unwrap()
		if next == w {
			return
		}
		w = next
	}
}

func clearGatewayErrorAccount(w http.ResponseWriter) {
	for w != nil {
		if clearer, ok := w.(gatewayErrorAccountClearer); ok {
			clearer.clearGatewayAccountID()
			return
		}
		unwrapper, ok := w.(responseWriterUnwrapper)
		if !ok {
			return
		}
		next := unwrapper.Unwrap()
		if next == w {
			return
		}
		w = next
	}
}

func attributeGatewayErrorEvent(w http.ResponseWriter, category string, status int, message string) {
	for w != nil {
		if setter, ok := w.(gatewayErrorEventSetter); ok {
			setter.setGatewayErrorEvent(category, status, message)
			return
		}
		unwrapper, ok := w.(responseWriterUnwrapper)
		if !ok {
			return
		}
		next := unwrapper.Unwrap()
		if next == w {
			return
		}
		w = next
	}
}

func attributeGatewayCapacityDiagnostics(w http.ResponseWriter, err error) {
	diagnostics, ok := gatewayCapacityDiagnosticsFromError(err)
	if !ok {
		return
	}
	for w != nil {
		if setter, ok := w.(gatewayDispatchDiagnosticsSetter); ok {
			setter.setGatewayDispatchDiagnostics(diagnostics)
			return
		}
		unwrapper, ok := w.(responseWriterUnwrapper)
		if !ok {
			return
		}
		next := unwrapper.Unwrap()
		if next == w {
			return
		}
		w = next
	}
}

func gatewayResponseStatus(w http.ResponseWriter) int {
	for w != nil {
		if reader, ok := w.(gatewayResponseStatusReader); ok {
			return reader.gatewayResponseStatus()
		}
		unwrapper, ok := w.(responseWriterUnwrapper)
		if !ok {
			return 0
		}
		next := unwrapper.Unwrap()
		if next == w {
			return 0
		}
		w = next
	}
	return 0
}

func (a *app) gatewayErrorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isGatewayErrorPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		trace := newGatewayRequestTrace(r)
		ctx := context.WithValue(r.Context(), gatewayTraceContextKey{}, trace)
		r = r.WithContext(ctx)
		wrapped := &gatewayErrorResponseWriter{ResponseWriter: w}
		wrapped.Header().Set(gatewayTraceHeader, trace.TraceID)
		next.ServeHTTP(wrapped, r)
		wrapped.durationMS = time.Since(started).Milliseconds()
		if wrapped.category == "" {
			switch {
			case errors.Is(r.Context().Err(), context.Canceled):
				wrapped.setGatewayErrorEvent("client_canceled", 499, "Client canceled request before completion")
			case errors.Is(r.Context().Err(), context.DeadlineExceeded):
				wrapped.setGatewayErrorEvent("timeout", http.StatusRequestTimeout, "Client request timed out before completion")
			}
		}
		if wrapped.status >= 400 || wrapped.category != "" {
			a.recordGatewayError(r, wrapped)
		}
	})
}

func isGatewayErrorPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") || path == "/models" || strings.HasPrefix(path, "/models/") || path == "/chat/completions"
}

func (a *app) recordGatewayError(r *http.Request, response *gatewayErrorResponseWriter) {
	secret := bearerOrAPIKey(r)
	if secret == "" {
		return
	}
	key, err := a.authenticateGatewayKey(secret)
	if err != nil {
		return
	}
	upstreamRequestID := strings.TrimSpace(response.Header().Get("request-id"))
	if upstreamRequestID == "" {
		upstreamRequestID = strings.TrimSpace(response.Header().Get("x-request-id"))
	}
	requestID, clientRequestID, traceID, upstreamRequestID := gatewayCorrelationIDs(r.Context(), upstreamRequestID)
	status := response.status
	if response.eventStatus != 0 {
		status = response.eventStatus
	}
	if status == 0 {
		status = http.StatusInternalServerError
	}
	category := strings.TrimSpace(response.category)
	if category == "" {
		category = "gateway_request"
	}
	message := strings.TrimSpace(response.eventMessage)
	if message == "" {
		message = gatewayErrorResponseMessage(response.body, status)
	}
	message = sanitizeErrorMessage(message)
	if runes := []rune(message); len(runes) > 1000 {
		message = string(runes[:1000])
	}
	snapshot := a.accountLoadSnapshot(response.accountID)
	_, err = a.db.Exec(`INSERT INTO gateway_error_logs (request_id, client_request_id, trace_id, upstream_request_id, api_key_id, user_id, account_id, group_id, status_code, category, method, path, message, client_ip, dispatch_diagnostics, duration_ms, rpm_snapshot, tpm_snapshot, itpm_snapshot, inflight_snapshot, total_requests) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		requestID, clientRequestID, traceID, upstreamRequestID, key.ID, key.UserID, optionalID(response.accountID), key.GroupID, status, category, r.Method, r.URL.Path, message, requestIP(r), response.dispatchDiagnostics, response.durationMS, snapshot.RPM, snapshot.TPM, snapshot.ITPM, snapshot.Inflight, snapshot.Total)
	logDatabaseWriteError("insert gateway error log", err)
	_ = a.pruneGatewayErrorLogs(false)
}

type accountLoadSnapshot struct {
	RPM      int64 `json:"rpm"`
	TPM      int64 `json:"tpm"`
	ITPM     int64 `json:"itpm"`
	Inflight int64 `json:"inflight"`
	Total    int64 `json:"total"`
}

// accountLoadSnapshot captures the account's load at the moment an error is
// recorded: requests and tokens in the sliding 60s window, the ITPM-relevant
// uncached input in the same window, requests currently being processed, and
// the lifetime request count. -1 means the value was not captured. Together
// with the 429 category this answers "how much concurrent volume was this
// account carrying when the upstream rate-limited it".
func (a *app) accountLoadSnapshot(accountID int64) accountLoadSnapshot {
	snapshot := accountLoadSnapshot{RPM: -1, TPM: -1, ITPM: -1, Inflight: -1, Total: -1}
	if accountID <= 0 {
		return snapshot
	}
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`, accountID).Scan(&snapshot.RPM)
	_ = a.db.QueryRow(`SELECT COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0), COALESCE(SUM(input_tokens + cache_creation_tokens), 0) FROM usage_logs WHERE account_id = ? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`, accountID).Scan(&snapshot.TPM, &snapshot.ITPM)
	_ = a.db.QueryRow(`SELECT COALESCE(requests, 0) FROM account_inflight WHERE account_id = ?`, accountID).Scan(&snapshot.Inflight)
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE account_id = ?`, accountID).Scan(&snapshot.Total)
	return snapshot
}

func gatewayErrorResponseMessage(body []byte, status int) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return http.StatusText(status)
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) == nil {
		if value, ok := payload["error"].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
		if value, ok := payload["error"].(map[string]any); ok {
			if message, ok := value["message"].(string); ok && strings.TrimSpace(message) != "" {
				return strings.TrimSpace(message)
			}
		}
		if value, ok := payload["message"].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return trimmed
}
