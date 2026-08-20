package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "ccmax_session"

var panelPages = []string{"overview", "accounts", "dead", "onboarding", "daily", "authorization", "proxies", "access", "pricing", "billing", "audit"}

type authContextKey struct{}

type panelUser struct {
	ID              int64    `json:"id"`
	Username        string   `json:"username"`
	Name            string   `json:"name"`
	Role            string   `json:"role"`
	Status          string   `json:"status"`
	AllowedGroupIDs []string `json:"allowed_group_ids"`
	VisiblePages    []string `json:"visible_pages"`
	RPM             int      `json:"rpm_limit"`
	CreatedAt       string   `json:"created_at"`
	UpdatedAt       string   `json:"updated_at"`
}

type userInput struct {
	Username        string   `json:"username"`
	Name            string   `json:"name"`
	Password        string   `json:"password"`
	Role            string   `json:"role"`
	Status          string   `json:"status"`
	AllowedGroupIDs []string `json:"allowed_group_ids"`
	VisiblePages    []string `json:"visible_pages"`
	RPM             int      `json:"rpm_limit"`
}

type apiKeyRecord struct {
	ID         int64   `json:"id"`
	UserID     int64   `json:"user_id"`
	Username   string  `json:"username"`
	Name       string  `json:"name"`
	KeyPrefix  string  `json:"key_prefix"`
	Key        string  `json:"key,omitempty"`
	GroupID    string  `json:"group_id"`
	Status     string  `json:"status"`
	Quota      float64 `json:"quota"`
	QuotaUsed  float64 `json:"quota_used"`
	ExpiresAt  string  `json:"expires_at"`
	LastUsedAt string  `json:"last_used_at"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

type apiKeyInput struct {
	UserID    int64   `json:"user_id"`
	Name      string  `json:"name"`
	GroupID   string  `json:"group_id"`
	Status    string  `json:"status"`
	Quota     float64 `json:"quota"`
	ExpiresAt string  `json:"expires_at"`
}

func (a *app) migrateFeatures() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE COLLATE NOCASE,
			name TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'readonly_admin', 'user')),
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
			allowed_group_ids_json TEXT NOT NULL DEFAULT '[]',
			rpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (rpm_limit >= 0),
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			deleted_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS panel_sessions (
			token_hash TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			key_hash TEXT NOT NULL UNIQUE,
			key_prefix TEXT NOT NULL,
			name TEXT NOT NULL,
			group_id TEXT REFERENCES groups(id),
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
			quota REAL NOT NULL DEFAULT 0 CHECK (quota >= 0),
			quota_used REAL NOT NULL DEFAULT 0 CHECK (quota_used >= 0),
			expires_at TEXT,
			last_used_at TEXT,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			deleted_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS user_rpm_events (
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON panel_sessions(expires_at)`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id, status) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_user_rpm ON user_rpm_events(user_id, created_at)`,
	}
	for _, statement := range statements {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate auth: %w", err)
		}
	}
	if err := addColumnIfMissing(a.db, "users", "visible_pages_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if _, err := a.db.Exec(`UPDATE users SET visible_pages_json = CASE role WHEN 'admin' THEN '["overview","accounts","dead","onboarding","daily","authorization","proxies","access","pricing","billing","audit"]' WHEN 'readonly_admin' THEN '["overview","accounts","dead","daily","authorization","proxies","pricing","billing","audit"]' ELSE '["access"]' END WHERE visible_pages_json = '[]'`); err != nil {
		return fmt.Errorf("initialize visible pages: %w", err)
	}
	if err := a.migrateProxyFeatures(); err != nil {
		return err
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND deleted_at IS NULL`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		hash, err := bcrypt.GenerateFromPassword([]byte(a.adminPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		visible, _ := json.Marshal(defaultVisiblePages("admin"))
		_, err = a.db.Exec(`INSERT INTO users (username, name, password_hash, role, allowed_group_ids_json, visible_pages_json) VALUES (?, '系统管理员', ?, 'admin', '["a","b"]', ?)`, a.adminUser, string(hash), string(visible))
		if err != nil {
			return fmt.Errorf("seed administrator: %w", err)
		}
	}
	return nil
}

func defaultVisiblePages(role string) []string {
	switch role {
	case "admin":
		return append([]string{}, panelPages...)
	case "readonly_admin":
		return []string{"overview", "accounts", "dead", "daily", "authorization", "proxies", "pricing", "billing", "audit"}
	default:
		return []string{"access"}
	}
}

func normalizedVisiblePages(role string, input []string) []string {
	if role == "admin" {
		return defaultVisiblePages(role)
	}
	valid := map[string]bool{}
	for _, page := range panelPages {
		valid[page] = true
	}
	seen := map[string]bool{}
	result := []string{}
	for _, page := range input {
		page = strings.ToLower(strings.TrimSpace(page))
		if valid[page] && !seen[page] {
			seen[page] = true
			result = append(result, page)
		}
	}
	if len(result) == 0 {
		return defaultVisiblePages(role)
	}
	return result
}

func userCanSeePage(user panelUser, page string) bool {
	if user.Role == "admin" {
		return true
	}
	for _, allowed := range user.VisiblePages {
		if allowed == page {
			return true
		}
	}
	return false
}

func userCanSeeAnyPage(user panelUser, pages ...string) bool {
	for _, page := range pages {
		if userCanSeePage(user, page) {
			return true
		}
	}
	return false
}

func userCanReadAPI(user panelUser, path string) bool {
	switch {
	case path == "/api/me" || path == "/api/auth/logout":
		return true
	case strings.HasPrefix(path, "/api/api-keys"):
		return userCanSeePage(user, "access")
	case path == "/api/dashboard":
		return userCanSeePage(user, "overview")
	case strings.HasPrefix(path, "/api/groups") || strings.HasPrefix(path, "/api/purposes"):
		return userCanSeeAnyPage(user, "overview", "accounts", "onboarding", "billing")
	case strings.HasPrefix(path, "/api/accounts"):
		return userCanSeeAnyPage(user, "accounts", "dead", "onboarding")
	case strings.HasPrefix(path, "/api/proxy-pools") || strings.HasPrefix(path, "/api/proxies"):
		return userCanSeeAnyPage(user, "proxies", "onboarding")
	case strings.HasPrefix(path, "/api/prices"):
		return userCanSeePage(user, "pricing")
	case strings.HasPrefix(path, "/api/billing") || strings.HasPrefix(path, "/api/usage"):
		return userCanSeePage(user, "billing")
	case strings.HasPrefix(path, "/api/stats/daily"):
		return userCanSeePage(user, "daily")
	case strings.HasPrefix(path, "/api/authorization-logs"):
		return userCanSeePage(user, "authorization")
	case strings.HasPrefix(path, "/api/audit-logs"):
		return userCanSeePage(user, "audit")
	default:
		return false
	}
}

func (a *app) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/api/health" || path == "/api/auth/login" || path == "/api/auth/session" || strings.HasPrefix(path, "/v1/") || !strings.HasPrefix(path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		var user panelUser
		if a.authDisabled {
			user = panelUser{ID: 0, Username: "development", Name: "Development", Role: "admin", Status: "active", AllowedGroupIDs: []string{"a", "b"}, VisiblePages: defaultVisiblePages("admin")}
		} else {
			cookie, err := r.Cookie(sessionCookie)
			if err != nil || strings.TrimSpace(cookie.Value) == "" {
				writeError(w, http.StatusUnauthorized, "login required")
				return
			}
			user, err = a.userBySession(cookie.Value)
			if err != nil {
				http.SetCookie(w, expiredSessionCookie())
				writeError(w, http.StatusUnauthorized, "session expired")
				return
			}
		}
		if user.Role != "admin" && path == "/api/users" {
			writeError(w, http.StatusForbidden, "permission denied")
			return
		}
		if user.Role != "admin" && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if user.Role == "user" && strings.HasPrefix(path, "/api/api-keys") && userCanSeePage(user, "access") {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, user)))
				return
			}
			message := "permission denied"
			if user.Role == "readonly_admin" {
				message = "read-only administrator cannot modify data"
			}
			writeError(w, http.StatusForbidden, message)
			return
		}
		if user.Role != "admin" && !userCanReadAPI(user, path) {
			writeError(w, http.StatusForbidden, "page permission denied")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, user)))
	})
}

func (a *app) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if a.authDisabled {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "user": panelUser{ID: 0, Username: "development", Name: "Development", Role: "admin", Status: "active", AllowedGroupIDs: []string{"a", "b"}, VisiblePages: defaultVisiblePages("admin")}})
		return
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	user, err := a.userBySession(cookie.Value)
	if err != nil {
		http.SetCookie(w, expiredSessionCookie())
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "user": user})
}

func currentUser(r *http.Request) panelUser {
	user, _ := r.Context().Value(authContextKey{}).(panelUser)
	return user
}

func scopedGroupIDs(user panelUser) []string {
	if user.Role != "user" {
		return []string{"a", "b"}
	}
	return uniqueGroups(user.AllowedGroupIDs)
}

func scopedGroupCondition(user panelUser, column string) (string, []any) {
	groups := scopedGroupIDs(user)
	if len(groups) == 0 {
		return "0 = 1", nil
	}
	placeholders := make([]string, len(groups))
	args := make([]any, len(groups))
	for index, groupID := range groups {
		placeholders[index] = "?"
		args[index] = groupID
	}
	return column + " IN (" + strings.Join(placeholders, ",") + ")", args
}

func scopedAccountCondition(user panelUser, accountAlias string) (string, []any) {
	if user.Role != "user" {
		return "1 = 1", nil
	}
	condition, args := scopedGroupCondition(user, "scope_ag.group_id")
	return "EXISTS (SELECT 1 FROM account_groups scope_ag WHERE scope_ag.account_id = " + accountAlias + ".id AND " + condition + ")", args
}

func userCanAccessGroup(user panelUser, groupID string) bool {
	for _, allowed := range scopedGroupIDs(user) {
		if allowed == groupID {
			return true
		}
	}
	return false
}

func (a *app) userBySession(token string) (panelUser, error) {
	return a.scanUser(a.db.QueryRow(`SELECT u.id, u.username, u.name, u.role, u.status, u.allowed_group_ids_json, u.visible_pages_json, u.rpm_limit, u.created_at, u.updated_at
		FROM panel_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > `+nowSQL+` AND u.status = 'active' AND u.deleted_at IS NULL`, hashToken(token)))
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		a.recordLoginAudit(r, panelUser{}, input.Username, http.StatusBadRequest, started)
		return
	}
	var user panelUser
	var passwordHash, allowed, visible string
	err := a.db.QueryRow(`SELECT id, username, name, password_hash, role, status, allowed_group_ids_json, visible_pages_json, rpm_limit, created_at, updated_at FROM users WHERE username = ? AND deleted_at IS NULL`, strings.TrimSpace(input.Username)).Scan(&user.ID, &user.Username, &user.Name, &passwordHash, &user.Role, &user.Status, &allowed, &visible, &user.RPM, &user.CreatedAt, &user.UpdatedAt)
	if err != nil || user.Status != "active" || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(input.Password)) != nil {
		a.recordLoginAudit(r, panelUser{}, input.Username, http.StatusUnauthorized, started)
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	_ = json.Unmarshal([]byte(allowed), &user.AllowedGroupIDs)
	_ = json.Unmarshal([]byte(visible), &user.VisiblePages)
	user.VisiblePages = normalizedVisiblePages(user.Role, user.VisiblePages)
	token := randomSecret(32)
	expires := time.Now().UTC().Add(24 * time.Hour)
	if _, err := a.db.Exec(`INSERT INTO panel_sessions (token_hash, user_id, expires_at) VALUES (?, ?, ?)`, hashToken(token), user.ID, expires.Format(time.RFC3339Nano)); err != nil {
		a.recordLoginAudit(r, user, input.Username, http.StatusInternalServerError, started)
		writeDBError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: 86400, Secure: r.TLS != nil})
	a.recordLoginAudit(r, user, input.Username, http.StatusOK, started)
	writeJSON(w, http.StatusOK, user)
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookie); err == nil {
		_, _ = a.db.Exec(`DELETE FROM panel_sessions WHERE token_hash = ?`, hashToken(cookie.Value))
	}
	http.SetCookie(w, expiredSessionCookie())
	w.WriteHeader(http.StatusNoContent)
}

func expiredSessionCookie() *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)}
}

func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentUser(r))
}

func (a *app) scanUser(row scanner) (panelUser, error) {
	var item panelUser
	var allowed, visible string
	err := row.Scan(&item.ID, &item.Username, &item.Name, &item.Role, &item.Status, &allowed, &visible, &item.RPM, &item.CreatedAt, &item.UpdatedAt)
	if err == nil {
		_ = json.Unmarshal([]byte(allowed), &item.AllowedGroupIDs)
		_ = json.Unmarshal([]byte(visible), &item.VisiblePages)
		item.VisiblePages = normalizedVisiblePages(item.Role, item.VisiblePages)
	}
	return item, err
}

func (a *app) handleUsers(w http.ResponseWriter, _ *http.Request) {
	rows, err := a.db.Query(`SELECT id, username, name, role, status, allowed_group_ids_json, visible_pages_json, rpm_limit, created_at, updated_at FROM users WHERE deleted_at IS NULL ORDER BY CASE role WHEN 'admin' THEN 0 WHEN 'readonly_admin' THEN 1 ELSE 2 END, id`)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []panelUser{}
	for rows.Next() {
		item, err := a.scanUser(rows)
		if err != nil {
			writeDBError(w, err)
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

func normalizeUserInput(input *userInput, requirePassword bool) error {
	input.Username = strings.TrimSpace(input.Username)
	input.Name = strings.TrimSpace(input.Name)
	input.Role = strings.TrimSpace(input.Role)
	input.Status = strings.TrimSpace(input.Status)
	input.AllowedGroupIDs = uniqueGroups(input.AllowedGroupIDs)
	input.VisiblePages = normalizedVisiblePages(input.Role, input.VisiblePages)
	if input.Status == "" {
		input.Status = "active"
	}
	if input.Role == "admin" || input.Role == "readonly_admin" {
		input.AllowedGroupIDs = []string{"a", "b"}
	}
	if input.Username == "" || (requirePassword && len(input.Password) < 8) || input.RPM < 0 {
		return errors.New("username, password or RPM is invalid")
	}
	if input.Role != "admin" && input.Role != "readonly_admin" && input.Role != "user" {
		return errors.New("invalid role")
	}
	if input.Status != "active" && input.Status != "disabled" {
		return errors.New("invalid status")
	}
	if input.Role == "user" && len(input.AllowedGroupIDs) == 0 {
		return errors.New("select at least one allowed group")
	}
	if len(input.VisiblePages) == 0 {
		return errors.New("select at least one visible page")
	}
	if input.Password != "" && len(input.Password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

func (a *app) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	var input userInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := normalizeUserInput(&input, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	allowed, _ := json.Marshal(input.AllowedGroupIDs)
	visible, _ := json.Marshal(input.VisiblePages)
	result, err := a.db.Exec(`INSERT INTO users (username, name, password_hash, role, status, allowed_group_ids_json, visible_pages_json, rpm_limit) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, input.Username, input.Name, string(hash), input.Role, input.Status, string(allowed), string(visible), input.RPM)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "username already exists")
			return
		}
		writeDBError(w, err)
		return
	}
	id, _ := result.LastInsertId()
	item, err := a.scanUser(a.db.QueryRow(`SELECT id, username, name, role, status, allowed_group_ids_json, visible_pages_json, rpm_limit, created_at, updated_at FROM users WHERE id = ?`, id))
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (a *app) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var input userInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := normalizeUserInput(&input, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	allowed, _ := json.Marshal(input.AllowedGroupIDs)
	visible, _ := json.Marshal(input.VisiblePages)
	var err error
	if input.Password != "" {
		hash, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
		_, err = a.db.Exec(`UPDATE users SET username = ?, name = ?, password_hash = ?, role = ?, status = ?, allowed_group_ids_json = ?, visible_pages_json = ?, rpm_limit = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, input.Username, input.Name, string(hash), input.Role, input.Status, string(allowed), string(visible), input.RPM, id)
		_, _ = a.db.Exec(`DELETE FROM panel_sessions WHERE user_id = ?`, id)
	} else {
		_, err = a.db.Exec(`UPDATE users SET username = ?, name = ?, role = ?, status = ?, allowed_group_ids_json = ?, visible_pages_json = ?, rpm_limit = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, input.Username, input.Name, input.Role, input.Status, string(allowed), string(visible), input.RPM, id)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	item, err := a.scanUser(a.db.QueryRow(`SELECT id, username, name, role, status, allowed_group_ids_json, visible_pages_json, rpm_limit, created_at, updated_at FROM users WHERE id = ? AND deleted_at IS NULL`, id))
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if id == currentUser(r).ID {
		writeError(w, http.StatusConflict, "cannot delete current user")
		return
	}
	result, err := a.db.Exec(`UPDATE users SET status = 'disabled', deleted_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleAPIKeys(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	query := `SELECT k.id, k.user_id, u.username, k.name, k.key_prefix, COALESCE(k.group_id, ''), k.status, k.quota, k.quota_used, k.expires_at, k.last_used_at, k.created_at, k.updated_at FROM api_keys k JOIN users u ON u.id = k.user_id WHERE k.deleted_at IS NULL`
	args := []any{}
	if user.Role == "user" {
		query += ` AND k.user_id = ?`
		args = append(args, user.ID)
	} else if id, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64); id > 0 {
		query += ` AND k.user_id = ?`
		args = append(args, id)
	}
	query += ` ORDER BY k.id DESC`
	rows, err := a.db.Query(query, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []apiKeyRecord{}
	for rows.Next() {
		item, err := scanAPIKey(rows)
		if err != nil {
			writeDBError(w, err)
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) handleAPIKeyCreate(w http.ResponseWriter, r *http.Request) {
	var input apiKeyInput
	if !decodeJSON(w, r, &input) {
		return
	}
	user := currentUser(r)
	if user.Role == "user" {
		input.UserID = user.ID
	}
	input.Name = strings.TrimSpace(input.Name)
	input.GroupID = strings.ToLower(strings.TrimSpace(input.GroupID))
	if input.Status == "" {
		input.Status = "active"
	}
	if input.UserID <= 0 || input.Name == "" || (input.GroupID != "a" && input.GroupID != "b") || input.Quota < 0 {
		writeError(w, http.StatusBadRequest, "invalid API key fields")
		return
	}
	if ok, err := a.userCanUseGroup(input.UserID, input.GroupID); err != nil || !ok {
		writeError(w, http.StatusForbidden, "user cannot use selected group")
		return
	}
	secret := "sk-" + randomSecret(32)
	result, err := a.db.Exec(`INSERT INTO api_keys (user_id, key_hash, key_prefix, name, group_id, status, quota, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))`, input.UserID, hashToken(secret), secret[:11], input.Name, input.GroupID, input.Status, input.Quota, strings.TrimSpace(input.ExpiresAt))
	if err != nil {
		writeDBError(w, err)
		return
	}
	id, _ := result.LastInsertId()
	item, err := a.getAPIKey(id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	item.Key = secret
	writeJSON(w, http.StatusCreated, item)
}

func (a *app) handleAPIKeyUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var input apiKeyInput
	if !decodeJSON(w, r, &input) {
		return
	}
	user := currentUser(r)
	var ownerID int64
	if err := a.db.QueryRow(`SELECT user_id FROM api_keys WHERE id = ? AND deleted_at IS NULL`, id).Scan(&ownerID); err != nil {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}
	if user.Role == "user" && ownerID != user.ID {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.GroupID = strings.ToLower(strings.TrimSpace(input.GroupID))
	if input.Status != "active" && input.Status != "disabled" {
		writeError(w, http.StatusBadRequest, "invalid status")
		return
	}
	if ok, _ := a.userCanUseGroup(ownerID, input.GroupID); !ok {
		writeError(w, http.StatusForbidden, "user cannot use selected group")
		return
	}
	_, err := a.db.Exec(`UPDATE api_keys SET name = ?, group_id = ?, status = ?, quota = ?, expires_at = NULLIF(?, ''), updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, input.Name, input.GroupID, input.Status, input.Quota, strings.TrimSpace(input.ExpiresAt), id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	item, _ := a.getAPIKey(id)
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleAPIKeyStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var input struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Status = strings.TrimSpace(input.Status)
	if input.Status != "active" && input.Status != "disabled" {
		writeError(w, http.StatusBadRequest, "status must be active or disabled")
		return
	}
	user := currentUser(r)
	var ownerID int64
	if err := a.db.QueryRow(`SELECT user_id FROM api_keys WHERE id = ? AND deleted_at IS NULL`, id).Scan(&ownerID); err != nil {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}
	if user.Role == "user" && ownerID != user.ID {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	if _, err := a.db.Exec(`UPDATE api_keys SET status = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, input.Status, id); err != nil {
		writeDBError(w, err)
		return
	}
	item, err := a.getAPIKey(id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleAPIKeyDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	user := currentUser(r)
	query := `UPDATE api_keys SET status = 'disabled', deleted_at = ` + nowSQL + `, updated_at = ` + nowSQL + ` WHERE id = ? AND deleted_at IS NULL`
	args := []any{id}
	if user.Role == "user" {
		query += ` AND user_id = ?`
		args = append(args, user.ID)
	}
	result, err := a.db.Exec(query, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "API key not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) getAPIKey(id int64) (apiKeyRecord, error) {
	return scanAPIKey(a.db.QueryRow(`SELECT k.id, k.user_id, u.username, k.name, k.key_prefix, COALESCE(k.group_id, ''), k.status, k.quota, k.quota_used, k.expires_at, k.last_used_at, k.created_at, k.updated_at FROM api_keys k JOIN users u ON u.id = k.user_id WHERE k.id = ? AND k.deleted_at IS NULL`, id))
}

func scanAPIKey(row scanner) (apiKeyRecord, error) {
	var item apiKeyRecord
	var expires, lastUsed sql.NullString
	err := row.Scan(&item.ID, &item.UserID, &item.Username, &item.Name, &item.KeyPrefix, &item.GroupID, &item.Status, &item.Quota, &item.QuotaUsed, &expires, &lastUsed, &item.CreatedAt, &item.UpdatedAt)
	item.ExpiresAt = nullText(expires)
	item.LastUsedAt = nullText(lastUsed)
	return item, err
}

func (a *app) userCanUseGroup(userID int64, groupID string) (bool, error) {
	var role, allowed string
	err := a.db.QueryRow(`SELECT role, allowed_group_ids_json FROM users WHERE id = ? AND status = 'active' AND deleted_at IS NULL`, userID).Scan(&role, &allowed)
	if err != nil {
		return false, err
	}
	if role == "admin" || role == "readonly_admin" {
		return true, nil
	}
	groups := []string{}
	_ = json.Unmarshal([]byte(allowed), &groups)
	for _, value := range groups {
		if value == groupID {
			return true, nil
		}
	}
	return false, nil
}

func randomSecret(size int) string {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return hex.EncodeToString([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
	}
	return hex.EncodeToString(buffer)
}

func hashToken(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}
