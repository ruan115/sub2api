package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type auditLog struct {
	ID            int64  `json:"id"`
	ActorUserID   *int64 `json:"actor_user_id"`
	ActorUsername string `json:"actor_username"`
	ActorRole     string `json:"actor_role"`
	Action        string `json:"action"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	TargetType    string `json:"target_type"`
	TargetID      string `json:"target_id"`
	RequestBody   string `json:"request_body"`
	ClientIP      string `json:"client_ip"`
	UserAgent     string `json:"user_agent"`
	StatusCode    int    `json:"status_code"`
	DurationMS    int64  `json:"duration_ms"`
	CreatedAt     string `json:"created_at"`
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

func (a *app) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" || !strings.HasPrefix(r.URL.Path, "/api/") || r.Method == http.MethodGet || r.Method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		started := time.Now()
		body := readAuditBody(r)
		wrapped := &auditResponseWriter{ResponseWriter: w}
		next.ServeHTTP(wrapped, r)
		if wrapped.status == 0 {
			wrapped.status = http.StatusNoContent
		}
		user := currentUser(r)
		action, targetType, targetID := auditAction(r)
		a.insertAudit(user, action, r.Method, r.URL.Path, targetType, targetID, body, requestIP(r), r.UserAgent(), wrapped.status, time.Since(started).Milliseconds())
	})
}

func readAuditBody(r *http.Request) string {
	if r.Body == nil {
		return "{}"
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		return "{}"
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	if len(bytes.TrimSpace(body)) == 0 {
		return "{}"
	}
	var value any
	if json.Unmarshal(body, &value) != nil {
		return `{"redacted":"non-json body"}`
	}
	if object, ok := value.(map[string]any); ok && r.URL.Path == "/api/proxies/batch" {
		if _, exists := object["text"]; exists {
			object["text"] = "[REDACTED]"
		}
	}
	redacted, _ := json.Marshal(redactAuditValue(value))
	return string(redacted)
}

func redactAuditValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			if sensitiveAuditKey(key) {
				out[key] = "[REDACTED]"
			} else {
				out[key] = redactAuditValue(item)
			}
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactAuditValue(item)
		}
		return out
	default:
		return value
	}
}

func sensitiveAuditKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", ""), "_", ""))
	for _, fragment := range []string{"password", "secret", "token", "apikey", "apiheaders", "apiurl", "sessionkey", "cookie", "credential", "authorization", "proxytext", "code"} {
		if strings.Contains(key, fragment) {
			return true
		}
	}
	return false
}

func auditAction(r *http.Request) (string, string, string) {
	path := strings.TrimPrefix(r.URL.Path, "/api/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	targetType, targetID := "system", ""
	if len(parts) > 0 && parts[0] != "" {
		targetType = strings.TrimSuffix(parts[0], "s")
		switch parts[0] {
		case "api-keys":
			targetType = "api_key"
		case "proxy-pools":
			targetType = "proxy_pool"
		case "prices":
			targetType = "price"
		case "purposes":
			targetType = "purpose"
		case "accounts":
			targetType = "account"
		case "users":
			targetType = "user"
		case "proxies":
			targetType = "proxy"
		}
	}
	if len(parts) > 1 {
		if _, err := strconv.ParseInt(parts[1], 10, 64); err == nil {
			targetID = parts[1]
		}
	}
	action := targetType + ".update"
	switch r.Method {
	case http.MethodPost:
		action = targetType + ".create"
	case http.MethodDelete:
		action = targetType + ".delete"
	case http.MethodPatch:
		action = targetType + ".status"
	}
	if strings.HasSuffix(path, "/sync") {
		action = targetType + ".sync"
	}
	if strings.Contains(path, "/quota/refresh") {
		action = "account.quota_refresh"
	}
	if path == "accounts/batch-delete" {
		action = "account.delete"
	}
	if path == "accounts/batch-schedule" {
		action = "account.schedule"
	}
	if path == "proxies/batch-test" {
		action = "proxy.test"
	}
	if path == "accounts/health/refresh" {
		action = "account.health_refresh"
	}
	if strings.HasSuffix(path, "/auth-url") {
		action = "account.oauth_start"
	}
	if strings.HasSuffix(path, "/oauth-exchange") || strings.HasSuffix(path, "/session-auth") {
		action = "account.reauthorize"
	}
	if path == "auth/logout" {
		action, targetType = "auth.logout", "session"
	}
	return action, targetType, targetID
}

func (a *app) insertAudit(user panelUser, action, method, path, targetType, targetID, body, clientIP, userAgent string, status int, durationMS int64) {
	var actorID any
	if user.ID > 0 {
		actorID = user.ID
	}
	_, _ = a.db.Exec(`INSERT INTO audit_logs (actor_user_id, actor_username, actor_role, action, method, path, target_type, target_id, request_body, client_ip, user_agent, status_code, duration_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, actorID, user.Username, user.Role, action, method, path, targetType, targetID, body, clientIP, userAgent, status, durationMS)
}

func (a *app) recordLoginAudit(r *http.Request, user panelUser, username string, status int, started time.Time) {
	if user.Username == "" {
		user.Username = strings.TrimSpace(username)
	}
	a.insertAudit(user, "auth.login", r.Method, r.URL.Path, "session", "", "{}", requestIP(r), r.UserAgent(), status, time.Since(started).Milliseconds())
}

func requestIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func (a *app) handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	where := []string{"1 = 1"}
	args := []any{}
	if user := currentUser(r); user.Role == "user" {
		where = append(where, "actor_user_id = ?")
		args = append(args, user.ID)
	}
	if action := strings.TrimSpace(r.URL.Query().Get("action")); action != "" {
		where = append(where, "action LIKE ?")
		args = append(args, action+"%")
	}
	if actor := strings.TrimSpace(r.URL.Query().Get("actor")); actor != "" {
		where = append(where, "actor_username LIKE ?")
		args = append(args, "%"+actor+"%")
	}
	if from := normalizeDateStart(strings.TrimSpace(r.URL.Query().Get("from"))); from != "" {
		where = append(where, "created_at >= ?")
		args = append(args, from)
	}
	if to := normalizeDateEnd(strings.TrimSpace(r.URL.Query().Get("to"))); to != "" {
		where = append(where, "created_at < ?")
		args = append(args, to)
	}
	clause := strings.Join(where, " AND ")
	var total int64
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE `+clause, args...).Scan(&total); err != nil {
		writeDBError(w, err)
		return
	}
	page, pageSize, offset := paginationFromRequest(r, 20, 100)
	queryArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := a.db.Query(`SELECT id, actor_user_id, actor_username, actor_role, action, method, path, target_type, target_id, request_body, client_ip, user_agent, status_code, duration_ms, created_at FROM audit_logs WHERE `+clause+` ORDER BY id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []auditLog{}
	for rows.Next() {
		var item auditLog
		var actorID *int64
		if err := rows.Scan(&item.ID, &actorID, &item.ActorUsername, &item.ActorRole, &item.Action, &item.Method, &item.Path, &item.TargetType, &item.TargetID, &item.RequestBody, &item.ClientIP, &item.UserAgent, &item.StatusCode, &item.DurationMS, &item.CreatedAt); err != nil {
			writeDBError(w, err)
			return
		}
		item.ActorUserID = actorID
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "page_size": pageSize, "total_pages": totalPages(total, pageSize)})
}
