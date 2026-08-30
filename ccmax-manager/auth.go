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
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const sessionCookie = "ccmax_session"

const (
	roleAdmin          = "admin"
	roleReadOnlyAdmin  = "readonly_admin"
	roleUser           = "user"
	roleOnboardingUser = "onboarding_user"

	userKindStandard   = "standard"
	userKindOnboarding = "onboarding"
)

var panelPages = []string{"overview", "accounts", "dead", "onboarding", "daily", "authorization", "errors", "strategies", "proxies", "access", "pricing", "billing", "audit"}

type authContextKey struct{}

type panelUser struct {
	ID               int64             `json:"id"`
	Username         string            `json:"username"`
	Name             string            `json:"name"`
	Role             string            `json:"role"`
	Status           string            `json:"status"`
	AllowedGroupIDs  []string          `json:"allowed_group_ids"`
	VisiblePages     []string          `json:"visible_pages"`
	AccountView      accountViewConfig `json:"account_view"`
	Balance          *float64          `json:"balance"`
	RPM              int               `json:"rpm_limit"`
	Consumed         float64           `json:"consumed"`
	UsageRequests    int64             `json:"usage_requests"`
	ActiveKeyCount   int64             `json:"active_key_count"`
	ArchivedKeyCount int64             `json:"archived_key_count"`
	CreatedAt        string            `json:"created_at"`
	UpdatedAt        string            `json:"updated_at"`
}

type userInput struct {
	Username        string            `json:"username"`
	Name            string            `json:"name"`
	Password        string            `json:"password"`
	Role            string            `json:"role"`
	Status          string            `json:"status"`
	AllowedGroupIDs []string          `json:"allowed_group_ids"`
	VisiblePages    []string          `json:"visible_pages"`
	AccountView     accountViewConfig `json:"account_view"`
	Balance         *float64          `json:"balance"`
	RPM             int               `json:"rpm_limit"`
}

type apiKeyRecord struct {
	ID              int64   `json:"id"`
	UserID          int64   `json:"user_id"`
	Username        string  `json:"username"`
	Name            string  `json:"name"`
	KeyPrefix       string  `json:"key_prefix"`
	Key             string  `json:"key,omitempty"`
	GroupID         string  `json:"group_id"`
	Status          string  `json:"status"`
	Quota           float64 `json:"quota"`
	QuotaUsed       float64 `json:"quota_used"`
	ExpiresAt       string  `json:"expires_at"`
	LastUsedAt      string  `json:"last_used_at"`
	DeletedAt       string  `json:"deleted_at"`
	Archived        bool    `json:"archived"`
	UsageRequests   int64   `json:"usage_requests"`
	UsageBilledCost float64 `json:"usage_billed_cost"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
}

type userAccessDetails struct {
	User       panelUser      `json:"user"`
	Keys       []apiKeyRecord `json:"keys"`
	Usage      []usageLog     `json:"usage"`
	Totals     billingTotals  `json:"totals"`
	UsagePage  int            `json:"usage_page"`
	UsageSize  int            `json:"usage_page_size"`
	UsageTotal int64          `json:"usage_total"`
	UsagePages int            `json:"usage_total_pages"`
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
		`CREATE TABLE IF NOT EXISTS feature_migrations (
			name TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE COLLATE NOCASE,
			name TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin', 'readonly_admin', 'user')),
			user_kind TEXT NOT NULL DEFAULT 'standard',
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
			allowed_group_ids_json TEXT NOT NULL DEFAULT '[]',
			account_view_json TEXT NOT NULL DEFAULT '{}',
			balance REAL,
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
			key_secret TEXT NOT NULL DEFAULT '',
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
	if err := addColumnIfMissing(a.db, "api_keys", "key_secret", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "users", "visible_pages_json", "TEXT NOT NULL DEFAULT '[]'"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "users", "account_view_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "users", "balance", "REAL"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "users", "user_kind", "TEXT NOT NULL DEFAULT 'standard'"); err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT OR IGNORE INTO feature_migrations (name) VALUES ('ordinary-user-account-page-v1')`)
	if err != nil {
		return fmt.Errorf("record ordinary user account page migration: %w", err)
	}
	if inserted, _ := result.RowsAffected(); inserted > 0 {
		if _, err := tx.Exec(`UPDATE users SET visible_pages_json = '["accounts","access"]' WHERE role = 'user' AND visible_pages_json = '["access"]'`); err != nil {
			return fmt.Errorf("add account pool to ordinary user pages: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ordinary user account page migration: %w", err)
	}
	// Visible-page backfills and the administrator seed live in
	// migrateSharedData so MySQL runs them too.
	return a.migrateProxyFeatures()
}

func defaultVisiblePages(role string) []string {
	switch role {
	case roleAdmin:
		return append([]string{}, panelPages...)
	case roleReadOnlyAdmin:
		return []string{"overview", "accounts", "dead", "daily", "authorization", "errors", "proxies", "pricing", "billing", "audit"}
	case roleOnboardingUser:
		return []string{"overview", "onboarding", "access"}
	default:
		return []string{"accounts", "access"}
	}
}

func normalizedVisiblePages(role string, input []string) []string {
	if role == roleAdmin {
		return defaultVisiblePages(role)
	}
	if role == roleOnboardingUser {
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
	if user.Role == roleAdmin {
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
	if user.Role == roleOnboardingUser && strings.HasPrefix(path, "/api/accounts") {
		return false
	}
	if user.Role == roleOnboardingUser && strings.HasPrefix(path, "/api/proxies") {
		return false
	}
	switch {
	case path == "/api/me" || path == "/api/auth/logout":
		return true
	case strings.HasPrefix(path, "/api/api-keys"):
		return userCanSeePage(user, "access")
	case path == "/api/dashboard":
		return userCanSeePage(user, "overview")
	case strings.HasPrefix(path, "/api/groups") || strings.HasPrefix(path, "/api/purposes"):
		return userCanSeeAnyPage(user, "overview", "accounts", "onboarding", "billing", "access")
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
	case strings.HasPrefix(path, "/api/stats/realtime"):
		return userCanSeePage(user, "accounts")
	case strings.HasPrefix(path, "/api/authorization-logs") || strings.HasPrefix(path, "/api/authorization-deauth"):
		return userCanSeePage(user, "authorization")
	case strings.HasPrefix(path, "/api/error-logs") || strings.HasPrefix(path, "/api/error-insights"):
		return userCanSeePage(user, "errors")
	case strings.HasPrefix(path, "/api/strategies"):
		return userCanSeePage(user, "strategies")
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
			user = panelUser{ID: 0, Username: "development", Name: "Development", Role: roleAdmin, Status: "active", AllowedGroupIDs: []string{"a", "b"}, VisiblePages: defaultVisiblePages(roleAdmin)}
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
		if user.Role != roleAdmin && path == "/api/users" {
			writeError(w, http.StatusForbidden, "permission denied")
			return
		}
		if user.Role != roleAdmin && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if path == "/api/auth/logout" {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, user)))
				return
			}
			if isOwnedAccessRole(user.Role) && strings.HasPrefix(path, "/api/api-keys") && userCanSeePage(user, "access") {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, user)))
				return
			}
			if user.Role == roleOnboardingUser && onboardingUserCanWrite(path, r.Method) {
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, user)))
				return
			}
			message := "permission denied"
			if user.Role == roleReadOnlyAdmin {
				message = "read-only administrator cannot modify data"
			}
			writeError(w, http.StatusForbidden, message)
			return
		}
		if user.Role != roleAdmin && !userCanReadAPI(user, path) {
			writeError(w, http.StatusForbidden, "page permission denied")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authContextKey{}, user)))
	})
}

func onboardingUserCanWrite(path, method string) bool {
	if method == http.MethodPost && path == "/api/accounts/batch-authorize" {
		return true
	}
	if method != http.MethodPut || !strings.HasPrefix(path, "/api/groups/") {
		return false
	}
	id := strings.TrimPrefix(path, "/api/groups/")
	return groupIDPattern.MatchString(id)
}

func (a *app) handleAuthSession(w http.ResponseWriter, r *http.Request) {
	if a.authDisabled {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "user": panelUser{ID: 0, Username: "development", Name: "Development", Role: roleAdmin, Status: "active", AllowedGroupIDs: []string{"a", "b"}, VisiblePages: defaultVisiblePages(roleAdmin)}})
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
	if !isScopedUserRole(user.Role) {
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
	if !isScopedUserRole(user.Role) {
		return "1 = 1", nil
	}
	condition, args := scopedGroupCondition(user, "scope_ag.group_id")
	return "EXISTS (SELECT 1 FROM account_groups scope_ag WHERE scope_ag.account_id = " + accountAlias + ".id AND " + condition + ")", args
}

func isScopedUserRole(role string) bool {
	return role == roleUser || role == roleOnboardingUser
}

func isOwnedAccessRole(role string) bool {
	return isScopedUserRole(role)
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
	return a.scanUser(a.db.QueryRow(`SELECT u.id, u.username, u.name, u.role, u.user_kind, u.status, u.allowed_group_ids_json, u.visible_pages_json, u.account_view_json, u.balance, u.rpm_limit, u.created_at, u.updated_at
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
	var passwordHash, userKind, allowed, visible, accountView string
	var balance sql.NullFloat64
	err := a.db.QueryRow(`SELECT id, username, name, password_hash, role, user_kind, status, allowed_group_ids_json, visible_pages_json, account_view_json, balance, rpm_limit, created_at, updated_at FROM users WHERE username = ? AND deleted_at IS NULL`, strings.TrimSpace(input.Username)).Scan(&user.ID, &user.Username, &user.Name, &passwordHash, &user.Role, &userKind, &user.Status, &allowed, &visible, &accountView, &balance, &user.RPM, &user.CreatedAt, &user.UpdatedAt)
	if err != nil || user.Status != "active" || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(input.Password)) != nil {
		a.recordLoginAudit(r, panelUser{}, input.Username, http.StatusUnauthorized, started)
		writeError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	finishScannedUser(&user, userKind, allowed, visible, accountView, balance)
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
		if _, err := a.db.Exec(`DELETE FROM panel_sessions WHERE token_hash = ?`, hashToken(cookie.Value)); err != nil {
			writeDBError(w, err)
			return
		}
	}
	http.SetCookie(w, expiredSessionCookie())
	w.WriteHeader(http.StatusNoContent)
}

func expiredSessionCookie() *http.Cookie {
	return &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1, Expires: time.Unix(1, 0)}
}

func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if err := a.populateUserAccessSummary(&user, user.Role == roleAdmin); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, user)
}

func (a *app) scanUser(row scanner) (panelUser, error) {
	var item panelUser
	var userKind, allowed, visible, accountView string
	var balance sql.NullFloat64
	err := row.Scan(&item.ID, &item.Username, &item.Name, &item.Role, &userKind, &item.Status, &allowed, &visible, &accountView, &balance, &item.RPM, &item.CreatedAt, &item.UpdatedAt)
	if err == nil {
		finishScannedUser(&item, userKind, allowed, visible, accountView, balance)
	}
	return item, err
}

func finishScannedUser(item *panelUser, userKind, allowed, visible, accountView string, balance sql.NullFloat64) {
	_ = json.Unmarshal([]byte(allowed), &item.AllowedGroupIDs)
	_ = json.Unmarshal([]byte(visible), &item.VisiblePages)
	_ = json.Unmarshal([]byte(accountView), &item.AccountView)
	if item.Role == roleUser && userKind == userKindOnboarding {
		item.Role = roleOnboardingUser
	}
	item.Balance = floatPointer(balance)
	item.VisiblePages = normalizedVisiblePages(item.Role, item.VisiblePages)
	item.AccountView = normalizeAccountView(item.Role, item.AccountView)
}

func (a *app) populateUserAccessSummary(item *panelUser, includeArchived bool) error {
	err := a.db.QueryRow(`SELECT
		COALESCE((SELECT SUM(billed_cost) FROM usage_logs WHERE user_id = ?), 0),
		(SELECT COUNT(*) FROM usage_logs WHERE user_id = ?),
		(SELECT COUNT(*) FROM api_keys WHERE user_id = ? AND deleted_at IS NULL AND status = 'active'),
		(SELECT COUNT(*) FROM api_keys WHERE user_id = ? AND deleted_at IS NOT NULL)`,
		item.ID, item.ID, item.ID, item.ID).Scan(&item.Consumed, &item.UsageRequests, &item.ActiveKeyCount, &item.ArchivedKeyCount)
	if err != nil {
		return err
	}
	if !includeArchived {
		item.ArchivedKeyCount = 0
	}
	return nil
}

func (a *app) handleUsers(w http.ResponseWriter, _ *http.Request) {
	rows, err := a.db.Query(`SELECT u.id, u.username, u.name, u.role, u.user_kind, u.status, u.allowed_group_ids_json, u.visible_pages_json, u.account_view_json, u.balance, u.rpm_limit, u.created_at, u.updated_at,
		COALESCE(usage_summary.billed_cost, 0), COALESCE(usage_summary.request_count, 0),
		COALESCE(key_summary.active_count, 0), COALESCE(key_summary.archived_count, 0)
		FROM users u
		LEFT JOIN (
			SELECT user_id, SUM(billed_cost) AS billed_cost, COUNT(*) AS request_count
			FROM usage_logs WHERE user_id IS NOT NULL GROUP BY user_id
		) usage_summary ON usage_summary.user_id = u.id
		LEFT JOIN (
			SELECT user_id,
				SUM(CASE WHEN deleted_at IS NULL AND status = 'active' THEN 1 ELSE 0 END) AS active_count,
				SUM(CASE WHEN deleted_at IS NOT NULL THEN 1 ELSE 0 END) AS archived_count
			FROM api_keys GROUP BY user_id
		) key_summary ON key_summary.user_id = u.id
		WHERE u.deleted_at IS NULL
		ORDER BY CASE u.role WHEN 'admin' THEN 0 WHEN 'readonly_admin' THEN 1 ELSE 2 END, u.id`)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []panelUser{}
	for rows.Next() {
		var item panelUser
		var userKind, allowed, visible, accountView string
		var balance sql.NullFloat64
		if err := rows.Scan(&item.ID, &item.Username, &item.Name, &item.Role, &userKind, &item.Status, &allowed, &visible, &accountView, &balance, &item.RPM, &item.CreatedAt, &item.UpdatedAt, &item.Consumed, &item.UsageRequests, &item.ActiveKeyCount, &item.ArchivedKeyCount); err != nil {
			writeDBError(w, err)
			return
		}
		finishScannedUser(&item, userKind, allowed, visible, accountView, balance)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) handleUserAccessDetails(w http.ResponseWriter, r *http.Request) {
	if currentUser(r).Role != roleAdmin {
		writeError(w, http.StatusForbidden, "permission denied")
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	item, err := a.scanUser(a.db.QueryRow(`SELECT id, username, name, role, user_kind, status, allowed_group_ids_json, visible_pages_json, account_view_json, balance, rpm_limit, created_at, updated_at FROM users WHERE id = ? AND deleted_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	if err := a.populateUserAccessSummary(&item, true); err != nil {
		writeDBError(w, err)
		return
	}

	keyRows, err := a.db.Query(apiKeyColumns+`,
		COALESCE(key_usage.request_count, 0), COALESCE(key_usage.billed_cost, 0)
		FROM api_keys k JOIN users u ON u.id = k.user_id
		LEFT JOIN (
			SELECT api_key_id, COUNT(*) AS request_count, SUM(billed_cost) AS billed_cost
			FROM usage_logs WHERE user_id = ? AND api_key_id IS NOT NULL GROUP BY api_key_id
		) key_usage ON key_usage.api_key_id = k.id
		WHERE k.user_id = ?
		ORDER BY CASE WHEN k.deleted_at IS NULL THEN 0 ELSE 1 END, k.id DESC`, id, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer keyRows.Close()
	keys := []apiKeyRecord{}
	for keyRows.Next() {
		key, err := scanAPIKeyWithUsage(keyRows)
		if err != nil {
			writeDBError(w, err)
			return
		}
		keys = append(keys, key)
	}
	if err := keyRows.Err(); err != nil {
		writeDBError(w, err)
		return
	}

	page, pageSize, offset := detailPaginationFromRequest(r)
	filters := usageFilters{UserID: id, Limit: pageSize, Offset: offset}
	usage, err := a.listUsage(filters)
	if err != nil {
		writeDBError(w, err)
		return
	}
	usageTotal, err := a.countUsage(filters)
	if err != nil {
		writeDBError(w, err)
		return
	}
	totals, err := a.queryTotalsFiltered(usageFilters{UserID: id})
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, userAccessDetails{
		User: item, Keys: keys, Usage: usage, Totals: totals,
		UsagePage: page, UsageSize: pageSize, UsageTotal: usageTotal, UsagePages: totalPages(usageTotal, pageSize),
	})
}

func detailPaginationFromRequest(r *http.Request) (page, pageSize, offset int) {
	page, pageSize = 1, 10
	if value, err := strconv.Atoi(r.URL.Query().Get("usage_page")); err == nil && value > 0 {
		page = value
	}
	if value, err := strconv.Atoi(r.URL.Query().Get("usage_page_size")); err == nil && value > 0 {
		pageSize = min(value, 50)
	}
	offset = (page - 1) * pageSize
	return
}

func normalizeUserInput(input *userInput, requirePassword bool) error {
	input.Username = strings.TrimSpace(input.Username)
	input.Name = strings.TrimSpace(input.Name)
	input.Role = strings.TrimSpace(input.Role)
	input.Status = strings.TrimSpace(input.Status)
	input.AllowedGroupIDs = uniqueGroups(input.AllowedGroupIDs)
	input.VisiblePages = normalizedVisiblePages(input.Role, input.VisiblePages)
	input.AccountView = normalizeAccountView(input.Role, input.AccountView)
	if input.Status == "" {
		input.Status = "active"
	}
	if input.Username == "" || (requirePassword && len(input.Password) < 8) || input.RPM < 0 {
		return errors.New("username, password or RPM is invalid")
	}
	if input.Balance != nil {
		if math.IsNaN(*input.Balance) || math.IsInf(*input.Balance, 0) || *input.Balance < 0 {
			return errors.New("balance must be a non-negative number or empty")
		}
		value := money(*input.Balance)
		input.Balance = &value
	}
	if input.Role != roleAdmin && input.Role != roleReadOnlyAdmin && input.Role != roleUser && input.Role != roleOnboardingUser {
		return errors.New("invalid role")
	}
	if input.Status != "active" && input.Status != "disabled" {
		return errors.New("invalid status")
	}
	if isScopedUserRole(input.Role) && len(input.AllowedGroupIDs) == 0 {
		return errors.New("select at least one allowed group")
	}
	if len(input.VisiblePages) == 0 {
		return errors.New("select at least one visible page")
	}
	if input.Role != roleAdmin && len(input.AccountView.Columns) == 0 {
		return errors.New("select at least one account pool column")
	}
	if input.Password != "" && len(input.Password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	return nil
}

func storedUserRole(role string) (string, string) {
	if role == roleOnboardingUser {
		return roleUser, userKindOnboarding
	}
	return role, userKindStandard
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
	if err := a.prepareUserGroups(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	allowed, _ := json.Marshal(input.AllowedGroupIDs)
	visible, _ := json.Marshal(input.VisiblePages)
	accountView, _ := json.Marshal(input.AccountView)
	storedRole, userKind := storedUserRole(input.Role)
	result, err := a.db.Exec(`INSERT INTO users (username, name, password_hash, role, user_kind, status, allowed_group_ids_json, visible_pages_json, account_view_json, balance, rpm_limit) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, input.Username, input.Name, string(hash), storedRole, userKind, input.Status, string(allowed), string(visible), string(accountView), input.Balance, input.RPM)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeError(w, http.StatusConflict, "username already exists")
			return
		}
		writeDBError(w, err)
		return
	}
	id, _ := result.LastInsertId()
	item, err := a.scanUser(a.db.QueryRow(`SELECT id, username, name, role, user_kind, status, allowed_group_ids_json, visible_pages_json, account_view_json, balance, rpm_limit, created_at, updated_at FROM users WHERE id = ?`, id))
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
	if err := a.prepareUserGroups(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	allowed, _ := json.Marshal(input.AllowedGroupIDs)
	visible, _ := json.Marshal(input.VisiblePages)
	accountView, _ := json.Marshal(input.AccountView)
	storedRole, userKind := storedUserRole(input.Role)
	var err error
	if input.Password != "" {
		hash, _ := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
		_, err = a.db.Exec(`UPDATE users SET username = ?, name = ?, password_hash = ?, role = ?, user_kind = ?, status = ?, allowed_group_ids_json = ?, visible_pages_json = ?, account_view_json = ?, balance = ?, rpm_limit = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, input.Username, input.Name, string(hash), storedRole, userKind, input.Status, string(allowed), string(visible), string(accountView), input.Balance, input.RPM, id)
		if err == nil {
			_, err = a.db.Exec(`DELETE FROM panel_sessions WHERE user_id = ?`, id)
		}
	} else {
		_, err = a.db.Exec(`UPDATE users SET username = ?, name = ?, role = ?, user_kind = ?, status = ?, allowed_group_ids_json = ?, visible_pages_json = ?, account_view_json = ?, balance = ?, rpm_limit = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, input.Username, input.Name, storedRole, userKind, input.Status, string(allowed), string(visible), string(accountView), input.Balance, input.RPM, id)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	item, err := a.scanUser(a.db.QueryRow(`SELECT id, username, name, role, user_kind, status, allowed_group_ids_json, visible_pages_json, account_view_json, balance, rpm_limit, created_at, updated_at FROM users WHERE id = ? AND deleted_at IS NULL`, id))
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
	includeArchived := user.Role == roleAdmin && strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_archived")), "true")
	query := apiKeySelect + ` WHERE 1 = 1`
	args := []any{}
	if !includeArchived {
		query += ` AND k.deleted_at IS NULL`
	}
	if isOwnedAccessRole(user.Role) {
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
		if !canRevealAPIKey(user, item.UserID) {
			item.Key = ""
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
	if isOwnedAccessRole(user.Role) {
		input.UserID = user.ID
	}
	input.Name = strings.TrimSpace(input.Name)
	input.GroupID = strings.ToLower(strings.TrimSpace(input.GroupID))
	if input.Status == "" {
		input.Status = "active"
	}
	if input.UserID <= 0 || input.Name == "" || !groupIDPattern.MatchString(input.GroupID) || input.Quota < 0 {
		writeError(w, http.StatusBadRequest, "invalid API key fields")
		return
	}
	if ok, err := a.userCanUseGroup(input.UserID, input.GroupID); err != nil || !ok {
		writeError(w, http.StatusForbidden, "user cannot use selected group")
		return
	}
	secret := "sk-" + randomSecret(32)
	result, err := a.db.Exec(`INSERT INTO api_keys (user_id, key_hash, key_prefix, key_secret, name, group_id, status, quota, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''))`, input.UserID, hashToken(secret), secret[:11], secret, input.Name, input.GroupID, input.Status, input.Quota, strings.TrimSpace(input.ExpiresAt))
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
	if isOwnedAccessRole(user.Role) && ownerID != user.ID {
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
	if isOwnedAccessRole(user.Role) && ownerID != user.ID {
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
	if isOwnedAccessRole(user.Role) {
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

const apiKeyColumns = `SELECT k.id, k.user_id, u.username, k.name, k.key_prefix, k.key_secret, COALESCE(k.group_id, ''), k.status, k.quota, k.quota_used, k.expires_at, k.last_used_at, k.created_at, k.updated_at, k.deleted_at`

const apiKeySelect = apiKeyColumns + ` FROM api_keys k JOIN users u ON u.id = k.user_id`

func (a *app) getAPIKey(id int64) (apiKeyRecord, error) {
	return scanAPIKey(a.db.QueryRow(apiKeySelect+` WHERE k.id = ? AND k.deleted_at IS NULL`, id))
}

func scanAPIKey(row scanner) (apiKeyRecord, error) {
	var item apiKeyRecord
	var expires, lastUsed, deleted sql.NullString
	err := row.Scan(&item.ID, &item.UserID, &item.Username, &item.Name, &item.KeyPrefix, &item.Key, &item.GroupID, &item.Status, &item.Quota, &item.QuotaUsed, &expires, &lastUsed, &item.CreatedAt, &item.UpdatedAt, &deleted)
	item.ExpiresAt = nullText(expires)
	item.LastUsedAt = nullText(lastUsed)
	item.DeletedAt = nullText(deleted)
	item.Archived = item.DeletedAt != ""
	return item, err
}

func scanAPIKeyWithUsage(row scanner) (apiKeyRecord, error) {
	var item apiKeyRecord
	var expires, lastUsed, deleted sql.NullString
	err := row.Scan(&item.ID, &item.UserID, &item.Username, &item.Name, &item.KeyPrefix, &item.Key, &item.GroupID, &item.Status, &item.Quota, &item.QuotaUsed, &expires, &lastUsed, &item.CreatedAt, &item.UpdatedAt, &deleted, &item.UsageRequests, &item.UsageBilledCost)
	item.ExpiresAt = nullText(expires)
	item.LastUsedAt = nullText(lastUsed)
	item.DeletedAt = nullText(deleted)
	item.Archived = item.DeletedAt != ""
	return item, err
}

func canRevealAPIKey(user panelUser, ownerID int64) bool {
	if user.Role == roleAdmin {
		return true
	}
	return isOwnedAccessRole(user.Role) && user.ID == ownerID
}

func (a *app) userCanUseGroup(userID int64, groupID string) (bool, error) {
	if err := a.validateDispatchGroupID(groupID); err != nil {
		return false, err
	}
	var role, allowed string
	err := a.db.QueryRow(`SELECT role, allowed_group_ids_json FROM users WHERE id = ? AND status = 'active' AND deleted_at IS NULL`, userID).Scan(&role, &allowed)
	if err != nil {
		return false, err
	}
	if role == roleAdmin || role == roleReadOnlyAdmin {
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

func (a *app) prepareUserGroups(input *userInput) error {
	if input.Role == roleAdmin || input.Role == roleReadOnlyAdmin {
		rows, err := a.db.Query(`SELECT id FROM groups ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		input.AllowedGroupIDs = []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			input.AllowedGroupIDs = append(input.AllowedGroupIDs, id)
		}
		return rows.Err()
	}
	return a.validateGroupIDs(input.AllowedGroupIDs, false)
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
