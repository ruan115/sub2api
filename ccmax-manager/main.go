package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed web/*
var embeddedWeb embed.FS

const nowSQL = "strftime('%Y-%m-%dT%H:%M:%fZ','now')"

var purposeKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

type scanner interface {
	Scan(dest ...any) error
}

type app struct {
	db            *sql.DB
	adminUser     string
	adminPassword string
	authDisabled  bool
	oauthSessions *oauthSessionStore
	priceSync     *priceSyncController
	accountHealth *accountHealthController
	tokenLocks    sync.Map
	quotaLocks    sync.Map
	budgetLocks   sync.Map
	queueStates   sync.Map
}

type group struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	RateMultiplier  float64  `json:"rate_multiplier"`
	DailyLimitUSD   *float64 `json:"daily_limit_usd"`
	MonthlyLimitUSD *float64 `json:"monthly_limit_usd"`
	Status          string   `json:"status"`
	ActiveAccounts  int      `json:"active_accounts"`
	TotalAccounts   int      `json:"total_accounts"`
	MonthBilledCost float64  `json:"month_billed_cost"`
	MonthActualCost float64  `json:"month_actual_cost"`
	TodayBilledCost float64  `json:"today_billed_cost"`
	UpdatedAt       string   `json:"updated_at"`
}

type groupInput struct {
	Name            string   `json:"name"`
	Description     string   `json:"description"`
	RateMultiplier  float64  `json:"rate_multiplier"`
	DailyLimitUSD   *float64 `json:"daily_limit_usd"`
	MonthlyLimitUSD *float64 `json:"monthly_limit_usd"`
	Status          string   `json:"status"`
}

type account struct {
	ID               int64          `json:"id"`
	Name             string         `json:"name"`
	Platform         string         `json:"platform"`
	AuthType         string         `json:"auth_type"`
	CredentialHint   string         `json:"credential_hint"`
	HasCredentials   bool           `json:"has_credentials"`
	Credentials      map[string]any `json:"credentials,omitempty"`
	Extra            map[string]any `json:"extra"`
	Status           string         `json:"status"`
	Schedulable      bool           `json:"schedulable"`
	Concurrency      int            `json:"concurrency"`
	Priority         int            `json:"priority"`
	RateMultiplier   float64        `json:"rate_multiplier"`
	Notes            string         `json:"notes"`
	ErrorMessage     string         `json:"error_message"`
	LastUsedAt       string         `json:"last_used_at"`
	ExpiresAt        string         `json:"expires_at"`
	RateLimitResetAt string         `json:"rate_limit_reset_at"`
	GroupIDs         []string       `json:"group_ids"`
	CreatedAt        string         `json:"created_at"`
	UpdatedAt        string         `json:"updated_at"`
	ProxyPoolID      *int64         `json:"proxy_pool_id"`
	ProxyPoolName    string         `json:"proxy_pool_name"`
	ProxyID          *int64         `json:"proxy_id"`
	ProxyName        string         `json:"proxy_name"`
	ProxyHint        string         `json:"proxy_hint"`
	AutoProxy        bool           `json:"auto_proxy"`
	BaseRPM          int            `json:"base_rpm"`
	RPMStrategy      string         `json:"rpm_strategy"`
	RPMStickyBuffer  int            `json:"rpm_sticky_buffer"`
	UserMsgQueueMode string         `json:"user_msg_queue_mode"`
	AuthStatus       string         `json:"auth_status"`
	AuthError        string         `json:"auth_error"`
	AuthCheckedAt    string         `json:"auth_checked_at"`
	TokenExpiresAt   string         `json:"token_expires_at"`
	Quota5H          float64        `json:"quota_5h_utilization"`
	Quota5HResetAt   string         `json:"quota_5h_reset_at"`
	Quota7D          float64        `json:"quota_7d_utilization"`
	Quota7DResetAt   string         `json:"quota_7d_reset_at"`
	QuotaSampledAt   string         `json:"quota_sampled_at"`
	SubscriptionType string         `json:"subscription_type"`
	AccountPrice     float64        `json:"account_price"`
	OnboardedAt      string         `json:"onboarded_at"`
	InvalidatedAt    string         `json:"invalidated_at"`
	SurvivalTotal    int64          `json:"survival_seconds_total"`
	SurvivalSeconds  int64          `json:"survival_seconds"`
	RequestCount     int64          `json:"request_count"`
	InputTokens      int64          `json:"input_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	TotalBilledCost  float64        `json:"total_billed_cost"`
	TotalActualCost  float64        `json:"total_actual_cost"`
	ProxyStatus      string         `json:"proxy_status"`
	DispatchStatus   string         `json:"dispatch_status"`
}

type accountInput struct {
	Name             string          `json:"name"`
	Platform         string          `json:"platform"`
	AuthType         string          `json:"auth_type"`
	SessionKey       string          `json:"session_key"`
	Credentials      json.RawMessage `json:"credentials"`
	Extra            json.RawMessage `json:"extra"`
	Status           string          `json:"status"`
	Schedulable      *bool           `json:"schedulable"`
	Concurrency      int             `json:"concurrency"`
	Priority         int             `json:"priority"`
	RateMultiplier   float64         `json:"rate_multiplier"`
	Notes            string          `json:"notes"`
	ErrorMessage     string          `json:"error_message"`
	ExpiresAt        string          `json:"expires_at"`
	RateLimitResetAt string          `json:"rate_limit_reset_at"`
	GroupIDs         []string        `json:"group_ids"`
	ProxyPoolID      *int64          `json:"proxy_pool_id"`
	ProxyID          *int64          `json:"proxy_id"`
	AutoProxy        bool            `json:"auto_proxy"`
	BaseRPM          int             `json:"base_rpm"`
	RPMStrategy      string          `json:"rpm_strategy"`
	RPMStickyBuffer  int             `json:"rpm_sticky_buffer"`
	UserMsgQueueMode string          `json:"user_msg_queue_mode"`
	AccountPrice     float64         `json:"account_price"`
}

type accountBatchDeleteInput struct {
	IDs []int64 `json:"ids"`
}

type purpose struct {
	ID            int64  `json:"id"`
	Key           string `json:"key"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ActiveGroupID string `json:"active_group_id"`
	GroupName     string `json:"group_name"`
	UpdatedAt     string `json:"updated_at"`
}

type purposeInput struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	ActiveGroupID string `json:"active_group_id"`
}

type modelPrice struct {
	ID                      int64   `json:"id"`
	Model                   string  `json:"model"`
	InputPerMillion         float64 `json:"input_per_million"`
	OutputPerMillion        float64 `json:"output_per_million"`
	CacheCreationPerMillion float64 `json:"cache_creation_per_million"`
	CacheReadPerMillion     float64 `json:"cache_read_per_million"`
	Source                  string  `json:"source"`
	SourceHash              string  `json:"source_hash"`
	UpdatedAt               string  `json:"updated_at"`
}

type usageInput struct {
	UserID              int64    `json:"-"`
	APIKeyID            int64    `json:"-"`
	RequestID           string   `json:"request_id"`
	PurposeKey          string   `json:"purpose_key"`
	GroupID             string   `json:"group_id"`
	AccountID           int64    `json:"account_id"`
	Model               string   `json:"model"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheCreationTokens int64    `json:"cache_creation_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	ActualCostOverride  *float64 `json:"actual_cost_override"`
	Stream              bool     `json:"stream"`
	DurationMS          int      `json:"duration_ms"`
}

type usageLog struct {
	ID                    int64   `json:"id"`
	UserID                *int64  `json:"user_id,omitempty"`
	APIKeyID              *int64  `json:"api_key_id,omitempty"`
	RequestID             string  `json:"request_id"`
	PurposeKey            string  `json:"purpose_key"`
	PurposeName           string  `json:"purpose_name"`
	GroupID               string  `json:"group_id"`
	AccountID             int64   `json:"account_id"`
	AccountName           string  `json:"account_name"`
	Model                 string  `json:"model"`
	InputTokens           int64   `json:"input_tokens"`
	OutputTokens          int64   `json:"output_tokens"`
	CacheCreationTokens   int64   `json:"cache_creation_tokens"`
	CacheReadTokens       int64   `json:"cache_read_tokens"`
	InputCost             float64 `json:"input_cost"`
	OutputCost            float64 `json:"output_cost"`
	CacheCreationCost     float64 `json:"cache_creation_cost"`
	CacheReadCost         float64 `json:"cache_read_cost"`
	BaseCost              float64 `json:"base_cost"`
	BilledCost            float64 `json:"billed_cost"`
	ActualCost            float64 `json:"actual_cost"`
	GroupRateMultiplier   float64 `json:"group_rate_multiplier"`
	AccountRateMultiplier float64 `json:"account_rate_multiplier"`
	Stream                bool    `json:"stream"`
	DurationMS            int     `json:"duration_ms"`
	CreatedAt             string  `json:"created_at"`
}

type billingTotals struct {
	Requests     int64   `json:"requests"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheTokens  int64   `json:"cache_tokens"`
	BaseCost     float64 `json:"base_cost"`
	BilledCost   float64 `json:"billed_cost"`
	ActualCost   float64 `json:"actual_cost"`
	Margin       float64 `json:"margin"`
}

type billingBreakdown struct {
	Key        string  `json:"key"`
	Name       string  `json:"name"`
	Requests   int64   `json:"requests"`
	BilledCost float64 `json:"billed_cost"`
	ActualCost float64 `json:"actual_cost"`
	Margin     float64 `json:"margin"`
}

type billingSummary struct {
	From      string             `json:"from"`
	To        string             `json:"to"`
	Totals    billingTotals      `json:"totals"`
	ByGroup   []billingBreakdown `json:"by_group"`
	ByAccount []billingBreakdown `json:"by_account"`
	ByPurpose []billingBreakdown `json:"by_purpose"`
}

type dashboard struct {
	AccountsTotal       int64         `json:"accounts_total"`
	AccountsActive      int64         `json:"accounts_active"`
	AccountsUnavailable int64         `json:"accounts_unavailable"`
	AccountsDead        int64         `json:"accounts_dead"`
	Purposes            []purpose     `json:"purposes"`
	Groups              []group       `json:"groups"`
	Today               billingTotals `json:"today"`
	Month               billingTotals `json:"month"`
	RecentUsage         []usageLog    `json:"recent_usage"`
}

func main() {
	dataPath := envOr("CCMAX_DATA", "ccmax-manager.db")
	addr := envOr("CCMAX_ADDR", "127.0.0.1:8088")
	a, err := newApp(dataPath)
	if err != nil {
		log.Fatal(err)
	}
	defer a.db.Close()
	stopPricing := a.startPriceSyncScheduler()
	defer stopPricing()
	stopTokenRefresh := a.startTokenRefreshScheduler()
	defer stopTokenRefresh()
	stopAccountHealth := a.startAccountHealthScheduler()
	defer stopAccountHealth()

	server := &http.Server{
		Addr:              addr,
		Handler:           a.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("CCMAX Manager listening on http://%s", addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func newApp(dataPath string) (*app, error) {
	if dataPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dataPath), 0o755); err != nil && filepath.Dir(dataPath) != "." {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	dsn := "file:" + filepath.ToSlash(dataPath) + "?_foreign_keys=on&_busy_timeout=5000"
	if dataPath == ":memory:" {
		dsn = "file:ccmax-manager-memory?mode=memory&cache=shared&_foreign_keys=on"
	} else {
		dsn += "&_journal_mode=WAL"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(5)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	a := &app{
		db:            db,
		adminUser:     envOr("CCMAX_ADMIN_USER", "admin"),
		adminPassword: envOr("CCMAX_ADMIN_PASSWORD", "ccmax-admin"),
		authDisabled:  strings.EqualFold(strings.TrimSpace(os.Getenv("CCMAX_AUTH_DISABLED")), "true") || strings.TrimSpace(os.Getenv("CCMAX_AUTH_DISABLED")) == "1",
		oauthSessions: newOAuthSessionStore(),
		priceSync:     newPriceSyncController(),
		accountHealth: newAccountHealthController(),
	}
	if err := a.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.migrateFeatures(); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.migrateAdvancedFeatures(); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func (a *app) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY CHECK (id IN ('a', 'b')),
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			rate_multiplier REAL NOT NULL DEFAULT 1 CHECK (rate_multiplier >= 0),
			daily_limit_usd REAL,
			monthly_limit_usd REAL,
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			platform TEXT NOT NULL DEFAULT 'anthropic',
			auth_type TEXT NOT NULL DEFAULT 'oauth',
			credentials_json TEXT NOT NULL DEFAULT '{}',
			credential_hint TEXT NOT NULL DEFAULT '',
			extra_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'error', 'disabled')),
			schedulable INTEGER NOT NULL DEFAULT 1,
			concurrency INTEGER NOT NULL DEFAULT 3 CHECK (concurrency > 0),
			priority INTEGER NOT NULL DEFAULT 50,
			rate_multiplier REAL NOT NULL DEFAULT 1 CHECK (rate_multiplier >= 0),
			notes TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			last_used_at TEXT,
			expires_at TEXT,
			rate_limit_reset_at TEXT,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			deleted_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS account_groups (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			priority INTEGER NOT NULL DEFAULT 50,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			PRIMARY KEY (account_id, group_id)
		)`,
		`CREATE TABLE IF NOT EXISTS account_fingerprints (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			fingerprint_json TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE TABLE IF NOT EXISTS purposes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			key TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			active_group_id TEXT NOT NULL REFERENCES groups(id),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE TABLE IF NOT EXISTS model_prices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			model TEXT NOT NULL UNIQUE,
			input_per_million REAL NOT NULL DEFAULT 0 CHECK (input_per_million >= 0),
			output_per_million REAL NOT NULL DEFAULT 0 CHECK (output_per_million >= 0),
			cache_creation_per_million REAL NOT NULL DEFAULT 0 CHECK (cache_creation_per_million >= 0),
			cache_read_per_million REAL NOT NULL DEFAULT 0 CHECK (cache_read_per_million >= 0),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			request_id TEXT NOT NULL UNIQUE,
			purpose_id INTEGER REFERENCES purposes(id) ON DELETE SET NULL,
			purpose_key TEXT NOT NULL,
			purpose_name TEXT NOT NULL,
			group_id TEXT NOT NULL REFERENCES groups(id),
			account_id INTEGER NOT NULL REFERENCES accounts(id),
			account_name TEXT NOT NULL,
			model TEXT NOT NULL,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
			cache_read_tokens INTEGER NOT NULL DEFAULT 0,
			input_cost REAL NOT NULL DEFAULT 0,
			output_cost REAL NOT NULL DEFAULT 0,
			cache_creation_cost REAL NOT NULL DEFAULT 0,
			cache_read_cost REAL NOT NULL DEFAULT 0,
			base_cost REAL NOT NULL DEFAULT 0,
			billed_cost REAL NOT NULL DEFAULT 0,
			actual_cost REAL NOT NULL DEFAULT 0,
			group_rate_multiplier REAL NOT NULL DEFAULT 1,
			account_rate_multiplier REAL NOT NULL DEFAULT 1,
			stream INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_dispatch ON accounts(status, schedulable, priority, last_used_at) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_account_groups_group ON account_groups(group_id, priority, account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_created ON usage_logs(created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_group_created ON usage_logs(group_id, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_account_created ON usage_logs(account_id, created_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	seeds := []string{
		`INSERT OR IGNORE INTO groups (id, name, description, rate_multiplier) VALUES ('a', 'A 分组', '主业务账号池', 1)`,
		`INSERT OR IGNORE INTO groups (id, name, description, rate_multiplier) VALUES ('b', 'B 分组', '备用与隔离账号池', 1)`,
		`INSERT OR IGNORE INTO purposes (key, name, description, active_group_id) VALUES ('default', '默认用途', '未指定用途时使用', 'a')`,
		`INSERT OR IGNORE INTO model_prices (model, input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million) VALUES ('*', 3, 15, 3.75, 0.3)`,
	}
	for _, statement := range seeds {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("seed database: %w", err)
		}
	}
	return nil
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.handleHealth)
	mux.HandleFunc("POST /api/auth/login", a.handleLogin)
	mux.HandleFunc("GET /api/auth/session", a.handleAuthSession)
	mux.HandleFunc("POST /api/auth/logout", a.handleLogout)
	mux.HandleFunc("GET /api/me", a.handleMe)
	mux.HandleFunc("GET /api/users", a.handleUsers)
	mux.HandleFunc("POST /api/users", a.handleUserCreate)
	mux.HandleFunc("PUT /api/users/{id}", a.handleUserUpdate)
	mux.HandleFunc("DELETE /api/users/{id}", a.handleUserDelete)
	mux.HandleFunc("GET /api/api-keys", a.handleAPIKeys)
	mux.HandleFunc("POST /api/api-keys", a.handleAPIKeyCreate)
	mux.HandleFunc("PUT /api/api-keys/{id}", a.handleAPIKeyUpdate)
	mux.HandleFunc("PATCH /api/api-keys/{id}/status", a.handleAPIKeyStatus)
	mux.HandleFunc("DELETE /api/api-keys/{id}", a.handleAPIKeyDelete)
	mux.HandleFunc("GET /api/proxy-pools", a.handleProxyPools)
	mux.HandleFunc("POST /api/proxy-pools", a.handleProxyPoolCreate)
	mux.HandleFunc("PUT /api/proxy-pools/{id}", a.handleProxyPoolUpdate)
	mux.HandleFunc("DELETE /api/proxy-pools/{id}", a.handleProxyPoolDelete)
	mux.HandleFunc("POST /api/proxy-pools/{id}/sync", a.handleProxyPoolSync)
	mux.HandleFunc("GET /api/proxies", a.handleProxies)
	mux.HandleFunc("POST /api/proxies/batch", a.handleProxyBatch)
	mux.HandleFunc("PUT /api/proxies/{id}", a.handleProxyUpdate)
	mux.HandleFunc("DELETE /api/proxies/{id}", a.handleProxyDelete)
	mux.HandleFunc("POST /api/proxies/{id}/test", a.handleProxyTest)
	mux.HandleFunc("GET /api/dashboard", a.handleDashboard)
	mux.HandleFunc("GET /api/groups", a.handleGroups)
	mux.HandleFunc("PUT /api/groups/{id}", a.handleGroupUpdate)
	mux.HandleFunc("GET /api/purposes", a.handlePurposes)
	mux.HandleFunc("POST /api/purposes", a.handlePurposeCreate)
	mux.HandleFunc("PUT /api/purposes/{id}", a.handlePurposeUpdate)
	mux.HandleFunc("DELETE /api/purposes/{id}", a.handlePurposeDelete)
	mux.HandleFunc("GET /api/accounts", a.handleAccounts)
	mux.HandleFunc("GET /api/accounts/summary", a.handleAccountSummary)
	mux.HandleFunc("POST /api/accounts", a.handleAccountCreate)
	mux.HandleFunc("POST /api/accounts/batch-authorize", a.handleBatchAuthorization)
	mux.HandleFunc("POST /api/accounts/batch-delete", a.handleAccountBatchDelete)
	mux.HandleFunc("POST /api/accounts/health/refresh", a.handleAccountHealthRefresh)
	mux.HandleFunc("PUT /api/accounts/{id}", a.handleAccountUpdate)
	mux.HandleFunc("DELETE /api/accounts/{id}", a.handleAccountDelete)
	mux.HandleFunc("POST /api/accounts/{id}/quota/refresh", a.handleAccountQuotaRefresh)
	mux.HandleFunc("POST /api/accounts/{id}/auth-url", a.handleAccountAuthURL)
	mux.HandleFunc("POST /api/accounts/{id}/oauth-exchange", a.handleAccountOAuthExchange)
	mux.HandleFunc("POST /api/accounts/{id}/session-auth", a.handleAccountSessionAuth)
	mux.HandleFunc("POST /api/pool/resolve", a.handlePoolResolve)
	mux.HandleFunc("GET /api/prices", a.handlePrices)
	mux.HandleFunc("POST /api/prices", a.handlePriceSave)
	mux.HandleFunc("GET /api/prices/sync-status", a.handlePriceSyncStatus)
	mux.HandleFunc("POST /api/prices/sync", a.handlePriceSync)
	mux.HandleFunc("DELETE /api/prices/{id}", a.handlePriceDelete)
	mux.HandleFunc("GET /api/audit-logs", a.handleAuditLogs)
	mux.HandleFunc("GET /api/usage", a.handleUsageList)
	mux.HandleFunc("POST /api/usage", a.handleUsageCreate)
	mux.HandleFunc("GET /api/billing", a.handleBilling)
	mux.HandleFunc("GET /api/stats/daily", a.handleDailyStats)
	mux.HandleFunc("GET /api/authorization-logs", a.handleAuthorizationStats)
	mux.HandleFunc("POST /v1/messages", a.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", a.handleCountTokens)
	mux.HandleFunc("GET /v1/models", a.handleModels)
	mux.HandleFunc("GET /v1/models/{id}", a.handleModels)
	mux.HandleFunc("GET /models", a.handleModels)
	mux.HandleFunc("GET /models/{id}", a.handleModels)

	webRoot, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		panic(err)
	}
	webHandler, err := versionedWebHandler(webRoot)
	if err != nil {
		panic(err)
	}
	mux.Handle("/", webHandler)
	return a.securityHeaders(a.authMiddleware(a.auditMiddleware(mux)))
}

func versionedWebHandler(webRoot fs.FS) (http.Handler, error) {
	styles, err := fs.ReadFile(webRoot, "styles.css")
	if err != nil {
		return nil, err
	}
	appJS, err := fs.ReadFile(webRoot, "app.js")
	if err != nil {
		return nil, err
	}
	index, err := fs.ReadFile(webRoot, "index.html")
	if err != nil {
		return nil, err
	}
	fingerprintInput := append(append([]byte{}, styles...), appJS...)
	fingerprint := sha256.Sum256(fingerprintInput)
	version := hex.EncodeToString(fingerprint[:8])
	versionedIndex := []byte(strings.ReplaceAll(string(index), "__ASSET_VERSION__", version))
	files := http.FileServer(http.FS(webRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Cache-Control", "no-store, max-age=0")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Length", strconv.Itoa(len(versionedIndex)))
			if r.Method != http.MethodHead {
				_, _ = w.Write(versionedIndex)
			}
			return
		}
		files.ServeHTTP(w, r)
	}), nil
}

func (a *app) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func (a *app) handleHealth(w http.ResponseWriter, _ *http.Request) {
	if err := a.db.Ping(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *app) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var result dashboard
	accountScope, accountArgs := scopedAccountCondition(user, "accounts")
	if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN `+accountStatePredicate("accounts", "normal")+` THEN 1 ELSE 0 END), 0), COALESCE(SUM(CASE WHEN `+accountStatePredicate("accounts", "unavailable")+` THEN 1 ELSE 0 END), 0), COALESCE(SUM(CASE WHEN `+accountStatePredicate("accounts", "error")+` THEN 1 ELSE 0 END), 0) FROM accounts WHERE deleted_at IS NULL AND `+accountScope, accountArgs...).Scan(&result.AccountsTotal, &result.AccountsActive, &result.AccountsUnavailable, &result.AccountsDead); err != nil {
		writeDBError(w, err)
		return
	}
	var err error
	result.Purposes, err = a.listPurposes()
	result.Purposes = scopePurposes(user, result.Purposes)
	if err == nil {
		result.Groups, err = a.listGroups()
		if err == nil {
			result.Groups, err = a.scopeGroups(user, result.Groups)
		}
	}
	usageScope := usageFilters{}
	if user.Role == "user" {
		usageScope.UserID = user.ID
	}
	if err == nil {
		usageScope.From = startOfTodayUTC()
		result.Today, err = a.queryTotalsFiltered(usageScope)
	}
	if err == nil {
		usageScope.From = startOfMonthUTC()
		result.Month, err = a.queryTotalsFiltered(usageScope)
	}
	if err == nil {
		usageScope.From = ""
		usageScope.Limit = 8
		result.RecentUsage, err = a.listUsage(usageScope)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) handleGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := a.listGroups()
	if err == nil {
		groups, err = a.scopeGroups(currentUser(r), groups)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (a *app) scopeGroups(user panelUser, groups []group) ([]group, error) {
	if user.Role != "user" {
		return groups, nil
	}
	allowed := map[string]bool{}
	for _, groupID := range scopedGroupIDs(user) {
		allowed[groupID] = true
	}
	result := make([]group, 0, len(groups))
	for _, item := range groups {
		if !allowed[item.ID] {
			continue
		}
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0), COALESCE(SUM(actual_cost), 0) FROM usage_logs WHERE group_id = ? AND user_id = ? AND created_at >= ?`, item.ID, user.ID, startOfMonthUTC()).Scan(&item.MonthBilledCost, &item.MonthActualCost); err != nil {
			return nil, err
		}
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0) FROM usage_logs WHERE group_id = ? AND user_id = ? AND created_at >= ?`, item.ID, user.ID, startOfTodayUTC()).Scan(&item.TodayBilledCost); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, nil
}

func (a *app) listGroups() ([]group, error) {
	rows, err := a.db.Query(`SELECT id, name, description, rate_multiplier, daily_limit_usd, monthly_limit_usd, status, updated_at FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := make([]group, 0, 2)
	for rows.Next() {
		var item group
		var daily, monthly sql.NullFloat64
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.RateMultiplier, &daily, &monthly, &item.Status, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.DailyLimitUSD = floatPointer(daily)
		item.MonthlyLimitUSD = floatPointer(monthly)
		if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN `+accountStatePredicate("a", "normal")+` THEN 1 ELSE 0 END), 0) FROM account_groups ag JOIN accounts a ON a.id = ag.account_id WHERE ag.group_id = ? AND a.deleted_at IS NULL`, item.ID).Scan(&item.TotalAccounts, &item.ActiveAccounts); err != nil {
			return nil, err
		}
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0), COALESCE(SUM(actual_cost), 0) FROM usage_logs WHERE group_id = ? AND created_at >= ?`, item.ID, startOfMonthUTC()).Scan(&item.MonthBilledCost, &item.MonthActualCost); err != nil {
			return nil, err
		}
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0) FROM usage_logs WHERE group_id = ? AND created_at >= ?`, item.ID, startOfTodayUTC()).Scan(&item.TodayBilledCost); err != nil {
			return nil, err
		}
		groups = append(groups, item)
	}
	return groups, rows.Err()
}

func (a *app) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(r.PathValue("id"))
	if id != "a" && id != "b" {
		writeError(w, http.StatusBadRequest, "group must be a or b")
		return
	}
	var input groupInput
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || input.RateMultiplier < 0 || (input.Status != "active" && input.Status != "disabled") {
		writeError(w, http.StatusBadRequest, "invalid group fields")
		return
	}
	if !validOptionalNonNegative(input.DailyLimitUSD) || !validOptionalNonNegative(input.MonthlyLimitUSD) {
		writeError(w, http.StatusBadRequest, "limits must be non-negative")
		return
	}
	result, err := a.db.Exec(`UPDATE groups SET name = ?, description = ?, rate_multiplier = ?, daily_limit_usd = ?, monthly_limit_usd = ?, status = ?, updated_at = `+nowSQL+` WHERE id = ?`, input.Name, strings.TrimSpace(input.Description), input.RateMultiplier, input.DailyLimitUSD, input.MonthlyLimitUSD, input.Status, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	groups, err := a.listGroups()
	if err != nil {
		writeDBError(w, err)
		return
	}
	for _, item := range groups {
		if item.ID == id {
			writeJSON(w, http.StatusOK, item)
			return
		}
	}
}

func (a *app) handlePurposes(w http.ResponseWriter, r *http.Request) {
	items, err := a.listPurposes()
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, scopePurposes(currentUser(r), items))
}

func scopePurposes(user panelUser, items []purpose) []purpose {
	if user.Role != "user" {
		return items
	}
	result := make([]purpose, 0, len(items))
	for _, item := range items {
		if userCanAccessGroup(user, item.ActiveGroupID) {
			result = append(result, item)
		}
	}
	return result
}

func (a *app) listPurposes() ([]purpose, error) {
	rows, err := a.db.Query(`SELECT p.id, p.key, p.name, p.description, p.active_group_id, g.name, p.updated_at FROM purposes p JOIN groups g ON g.id = p.active_group_id ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []purpose{}
	for rows.Next() {
		var item purpose
		if err := rows.Scan(&item.ID, &item.Key, &item.Name, &item.Description, &item.ActiveGroupID, &item.GroupName, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func normalizePurposeInput(input *purposeInput) error {
	input.Key = strings.ToLower(strings.TrimSpace(input.Key))
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	input.ActiveGroupID = strings.ToLower(strings.TrimSpace(input.ActiveGroupID))
	if !purposeKeyPattern.MatchString(input.Key) {
		return errors.New("purpose key must contain lowercase letters, numbers, - or _")
	}
	if input.Name == "" {
		return errors.New("purpose name is required")
	}
	if input.ActiveGroupID != "a" && input.ActiveGroupID != "b" {
		return errors.New("active group must be a or b")
	}
	return nil
}

func (a *app) handlePurposeCreate(w http.ResponseWriter, r *http.Request) {
	var input purposeInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := normalizePurposeInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := a.db.Exec(`INSERT INTO purposes (key, name, description, active_group_id) VALUES (?, ?, ?, ?)`, input.Key, input.Name, input.Description, input.ActiveGroupID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeError(w, http.StatusConflict, "purpose key already exists")
			return
		}
		writeDBError(w, err)
		return
	}
	id, _ := result.LastInsertId()
	item, err := a.getPurpose(id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (a *app) handlePurposeUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var input purposeInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := normalizePurposeInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := a.db.Exec(`UPDATE purposes SET key = ?, name = ?, description = ?, active_group_id = ?, updated_at = `+nowSQL+` WHERE id = ?`, input.Key, input.Name, input.Description, input.ActiveGroupID, id)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeError(w, http.StatusConflict, "purpose key already exists")
			return
		}
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "purpose not found")
		return
	}
	item, err := a.getPurpose(id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handlePurposeDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var key string
	if err := a.db.QueryRow(`SELECT key FROM purposes WHERE id = ?`, id).Scan(&key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "purpose not found")
			return
		}
		writeDBError(w, err)
		return
	}
	if key == "default" {
		writeError(w, http.StatusConflict, "default purpose cannot be deleted")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM purposes WHERE id = ?`, id); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) getPurpose(id int64) (purpose, error) {
	var item purpose
	err := a.db.QueryRow(`SELECT p.id, p.key, p.name, p.description, p.active_group_id, g.name, p.updated_at FROM purposes p JOIN groups g ON g.id = p.active_group_id WHERE p.id = ?`, id).Scan(&item.ID, &item.Key, &item.Name, &item.Description, &item.ActiveGroupID, &item.GroupName, &item.UpdatedAt)
	return item, err
}

const accountSelect = `SELECT a.id, a.name, a.platform, a.auth_type, a.credential_hint, a.credentials_json != '{}',
	a.status, a.schedulable, a.concurrency, a.priority, a.rate_multiplier, a.notes, a.error_message,
	a.last_used_at, a.expires_at, a.rate_limit_reset_at, a.created_at, a.updated_at, a.credentials_json, a.extra_json,
	a.proxy_pool_id, COALESCE(pp.name, ''), a.proxy_id, COALESCE(px.name, ''),
	CASE WHEN px.id IS NULL THEN '' ELSE px.protocol || '://' || px.host || ':' || px.port END,
	a.auto_proxy, a.base_rpm, a.rpm_strategy, a.rpm_sticky_buffer, a.user_msg_queue_mode,
	a.auth_status, a.auth_error, a.auth_checked_at, a.token_expires_at,
	a.quota_5h_utilization, a.quota_5h_reset_at, a.quota_7d_utilization, a.quota_7d_reset_at, a.quota_sampled_at,
	a.subscription_type, a.account_price, a.onboarded_at, a.invalidated_at, a.survival_seconds_total, COALESCE(px.status, ''),
	(SELECT COUNT(*) FROM usage_logs u WHERE u.account_id = a.id),
	COALESCE((SELECT SUM(u.input_tokens) FROM usage_logs u WHERE u.account_id = a.id), 0),
	COALESCE((SELECT SUM(u.output_tokens) FROM usage_logs u WHERE u.account_id = a.id), 0),
	COALESCE((SELECT SUM(u.billed_cost) FROM usage_logs u WHERE u.account_id = a.id), 0),
	COALESCE((SELECT SUM(u.actual_cost) FROM usage_logs u WHERE u.account_id = a.id), 0)
	FROM accounts a LEFT JOIN proxy_pools pp ON pp.id = a.proxy_pool_id LEFT JOIN proxies px ON px.id = a.proxy_id`

func scanAccount(row scanner, reveal bool) (account, error) {
	var item account
	var schedulable, autoProxy int
	var proxyPoolID, proxyID sql.NullInt64
	var lastUsed, expires, rateLimit, authChecked, tokenExpires, quota5HReset, quota7DReset, quotaSampled, onboarded, invalidated sql.NullString
	var credentialsJSON, extraJSON string
	err := row.Scan(&item.ID, &item.Name, &item.Platform, &item.AuthType, &item.CredentialHint, &item.HasCredentials, &item.Status, &schedulable, &item.Concurrency, &item.Priority, &item.RateMultiplier, &item.Notes, &item.ErrorMessage, &lastUsed, &expires, &rateLimit, &item.CreatedAt, &item.UpdatedAt, &credentialsJSON, &extraJSON, &proxyPoolID, &item.ProxyPoolName, &proxyID, &item.ProxyName, &item.ProxyHint, &autoProxy, &item.BaseRPM, &item.RPMStrategy, &item.RPMStickyBuffer, &item.UserMsgQueueMode, &item.AuthStatus, &item.AuthError, &authChecked, &tokenExpires, &item.Quota5H, &quota5HReset, &item.Quota7D, &quota7DReset, &quotaSampled, &item.SubscriptionType, &item.AccountPrice, &onboarded, &invalidated, &item.SurvivalTotal, &item.ProxyStatus, &item.RequestCount, &item.InputTokens, &item.OutputTokens, &item.TotalBilledCost, &item.TotalActualCost)
	if err != nil {
		return item, err
	}
	item.Schedulable = schedulable == 1
	item.AutoProxy = autoProxy == 1
	item.ProxyPoolID = nullIntPointer(proxyPoolID)
	item.ProxyID = nullIntPointer(proxyID)
	item.LastUsedAt = nullText(lastUsed)
	item.ExpiresAt = nullText(expires)
	item.RateLimitResetAt = nullText(rateLimit)
	item.AuthCheckedAt = nullText(authChecked)
	item.TokenExpiresAt = nullText(tokenExpires)
	item.Quota5HResetAt = nullText(quota5HReset)
	item.Quota7DResetAt = nullText(quota7DReset)
	item.QuotaSampledAt = nullText(quotaSampled)
	item.OnboardedAt = nullText(onboarded)
	item.InvalidatedAt = nullText(invalidated)
	item.SurvivalSeconds = accountSurvivalSeconds(item.OnboardedAt, item.InvalidatedAt, item.SurvivalTotal)
	item.DispatchStatus = accountDispatchState(item)
	item.Extra = decodeObject(extraJSON)
	if reveal {
		item.Credentials = decodeObject(credentialsJSON)
	}
	return item, nil
}

func (a *app) handleAccounts(w http.ResponseWriter, r *http.Request) {
	query := accountSelect + ` WHERE a.deleted_at IS NULL`
	args := []any{}
	user := currentUser(r)
	if user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "a")
		query += ` AND ` + condition
		args = append(args, scopeArgs...)
	}
	if groupID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))); groupID == "a" || groupID == "b" {
		query += ` AND EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = ?)`
		args = append(args, groupID)
	}
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		query += ` AND ` + accountStatePredicate("a", status)
	}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		query += ` AND (a.name LIKE ? OR a.notes LIKE ? OR a.credential_hint LIKE ?)`
		term := "%" + search + "%"
		args = append(args, term, term, term)
	}
	query += ` ORDER BY a.priority, a.id`
	rows, err := a.db.Query(query, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []account{}
	for rows.Next() {
		item, err := scanAccount(rows, false)
		if err != nil {
			writeDBError(w, err)
			return
		}
		item.GroupIDs, err = a.accountGroupIDs(item.ID)
		if err != nil {
			writeDBError(w, err)
			return
		}
		if user.Role == "user" {
			visibleGroups := make([]string, 0, len(item.GroupIDs))
			for _, groupID := range item.GroupIDs {
				if userCanAccessGroup(user, groupID) {
					visibleGroups = append(visibleGroups, groupID)
				}
			}
			item.GroupIDs = visibleGroups
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var input accountInput
	if !decodeJSON(w, r, &input) {
		return
	}
	credentialsJSON, extraJSON, err := normalizeAccountInput(&input, "{}", "{}")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var authorizedProxyID *int64
	var tokenExpiresAt string
	var authorizedSubscription string
	sessionAuthorized := false
	deferredAuthorization := false
	if credentialsJSON == "{}" {
		switch input.AuthType {
		case "oauth", "setup_token":
			if strings.TrimSpace(input.SessionKey) == "" {
				deferredAuthorization = true
				*input.Schedulable = false
				break
			}
			authType, _, apiScope, err := normalizeClaudeAuthMode(input.AuthType)
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			var proxyURL string
			authorizedProxyID, proxyURL, err = a.selectProxyForNewAccount(input.ProxyPoolID, input.ProxyID, input.AutoProxy)
			if err != nil {
				writeError(w, http.StatusConflict, err.Error())
				return
			}
			token, err := exchangeClaudeSessionKey(r.Context(), strings.TrimSpace(input.SessionKey), apiScope, proxyURL)
			if err != nil {
				a.recordAuthorization(nil, authorizedProxyID, input.Name, "session_key_create", false, err.Error(), "", requestIP(r))
				writeError(w, http.StatusBadGateway, err.Error())
				return
			}
			encoded, _ := json.Marshal(token)
			credentialsJSON = string(encoded)
			input.AuthType = authType
			tokenExpiresAt = time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339Nano)
			authorizedSubscription = token.SubscriptionType
			sessionAuthorized = true
		case "api_key":
			writeError(w, http.StatusBadRequest, "API Key credentials are required before creating the account")
			return
		}
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO accounts (name, platform, auth_type, credentials_json, credential_hint, extra_json, status, schedulable, concurrency, priority, rate_multiplier, notes, error_message, expires_at, rate_limit_reset_at, proxy_pool_id, auto_proxy, base_rpm, rpm_strategy, rpm_sticky_buffer, user_msg_queue_mode, account_price) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)`, input.Name, input.Platform, input.AuthType, credentialsJSON, credentialHint(credentialsJSON), extraJSON, input.Status, boolInt(*input.Schedulable), input.Concurrency, input.Priority, input.RateMultiplier, input.Notes, input.ErrorMessage, input.ExpiresAt, input.RateLimitResetAt, input.ProxyPoolID, boolInt(input.AutoProxy), input.BaseRPM, input.RPMStrategy, input.RPMStickyBuffer, input.UserMsgQueueMode, input.AccountPrice)
	if err != nil {
		writeDBError(w, err)
		return
	}
	id, _ := result.LastInsertId()
	if sessionAuthorized {
		credentials := decodeObject(credentialsJSON)
		subscription := subscriptionTypeFromCredentials(credentials)
		_, _ = tx.Exec(`UPDATE accounts SET auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, token_expires_at = ?, subscription_type = ?, onboarded_at = `+nowSQL+`, invalidated_at = NULL WHERE id = ?`, tokenExpiresAt, subscription, id)
	} else if deferredAuthorization {
		_, _ = tx.Exec(`UPDATE accounts SET auth_status = 'reauth_required', auth_error = '等待授权' WHERE id = ?`, id)
	} else if credentialsJSON != "{}" {
		credentials := decodeObject(credentialsJSON)
		subscription := subscriptionTypeFromCredentials(credentials)
		_, _ = tx.Exec(`UPDATE accounts SET auth_status = 'valid', auth_checked_at = `+nowSQL+`, subscription_type = ?, onboarded_at = `+nowSQL+`, invalidated_at = NULL WHERE id = ?`, subscription, id)
	}
	requestedProxyID, assignAutomatically := input.ProxyID, input.AutoProxy
	if sessionAuthorized && authorizedProxyID != nil {
		requestedProxyID = authorizedProxyID
		assignAutomatically = false
	}
	assignedProxy, err := assignAccountProxy(tx, id, input.ProxyPoolID, requestedProxyID, assignAutomatically)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if _, err := tx.Exec(`UPDATE accounts SET proxy_id = ? WHERE id = ?`, assignedProxy, id); err != nil {
		writeDBError(w, err)
		return
	}
	if err := setAccountGroups(tx, id, input.GroupIDs, input.Priority); err != nil {
		writeDBError(w, err)
		return
	}
	if credentialsJSON != "{}" {
		if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, 'onboarded')`, id); err != nil {
			writeDBError(w, err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	if sessionAuthorized {
		a.recordAuthorization(&id, assignedProxy, input.Name, "session_key_create", true, "authorization succeeded", authorizedSubscription, requestIP(r))
	}
	item, err := a.getAccount(id, false)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (a *app) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var existingCredentials, existingExtra string
	var previousOnboarded, previousInvalidated sql.NullString
	if err := a.db.QueryRow(`SELECT credentials_json, extra_json, onboarded_at, invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, id).Scan(&existingCredentials, &existingExtra, &previousOnboarded, &previousInvalidated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		writeDBError(w, err)
		return
	}
	var input accountInput
	if !decodeJSON(w, r, &input) {
		return
	}
	credentialsJSON, extraJSON, err := normalizeAccountInput(&input, existingCredentials, existingExtra)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()
	assignedProxy, err := assignAccountProxy(tx, id, input.ProxyPoolID, input.ProxyID, input.AutoProxy)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	_, err = tx.Exec(`UPDATE accounts SET name = ?, platform = ?, auth_type = ?, credentials_json = ?, credential_hint = ?, extra_json = ?, status = ?, schedulable = ?, concurrency = ?, priority = ?, rate_multiplier = ?, notes = ?, error_message = ?, expires_at = NULLIF(?, ''), rate_limit_reset_at = NULLIF(?, ''), proxy_pool_id = ?, proxy_id = ?, auto_proxy = ?, base_rpm = ?, rpm_strategy = ?, rpm_sticky_buffer = ?, user_msg_queue_mode = ?, account_price = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, input.Name, input.Platform, input.AuthType, credentialsJSON, credentialHint(credentialsJSON), extraJSON, input.Status, boolInt(*input.Schedulable), input.Concurrency, input.Priority, input.RateMultiplier, input.Notes, input.ErrorMessage, input.ExpiresAt, input.RateLimitResetAt, input.ProxyPoolID, assignedProxy, boolInt(input.AutoProxy), input.BaseRPM, input.RPMStrategy, input.RPMStickyBuffer, input.UserMsgQueueMode, input.AccountPrice, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	credentialsProvided := len(input.Credentials) > 0 && string(input.Credentials) != "null"
	if credentialsProvided && credentialsJSON != "{}" {
		subscription := subscriptionTypeFromCredentials(decodeObject(credentialsJSON))
		if _, err := tx.Exec(`UPDATE accounts SET auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, subscription_type = ?, onboarded_at = CASE WHEN onboarded_at IS NULL OR invalidated_at IS NOT NULL THEN `+nowSQL+` ELSE onboarded_at END, invalidated_at = NULL, error_message = '', status = CASE WHEN status = 'error' THEN 'active' ELSE status END WHERE id = ?`, subscription, id); err != nil {
			writeDBError(w, err)
			return
		}
		if !previousOnboarded.Valid || previousInvalidated.Valid {
			if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, 'onboarded')`, id); err != nil {
				writeDBError(w, err)
				return
			}
		}
	} else if (credentialsProvided && credentialsJSON == "{}") || (input.Status == "error" && !previousInvalidated.Valid) {
		if _, err := tx.Exec(`UPDATE accounts SET `+accumulateAccountSurvivalSQL+`, auth_status = 'reauth_required', auth_error = CASE WHEN auth_error = '' THEN '等待重新授权' ELSE auth_error END, invalidated_at = COALESCE(invalidated_at, `+nowSQL+`), schedulable = 0, status = CASE WHEN status = 'disabled' THEN status ELSE 'error' END WHERE id = ?`, id); err != nil {
			writeDBError(w, err)
			return
		}
		if !previousInvalidated.Valid {
			if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, 'invalidated')`, id); err != nil {
				writeDBError(w, err)
				return
			}
		}
	}
	if err := setAccountGroups(tx, id, input.GroupIDs, input.Priority); err != nil {
		writeDBError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	item, err := a.getAccount(id, false)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func normalizeAccountInput(input *accountInput, existingCredentials, existingExtra string) (string, string, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.Platform = strings.TrimSpace(input.Platform)
	input.AuthType = strings.TrimSpace(input.AuthType)
	input.Status = strings.TrimSpace(input.Status)
	input.Notes = strings.TrimSpace(input.Notes)
	input.ErrorMessage = strings.TrimSpace(input.ErrorMessage)
	input.ExpiresAt = strings.TrimSpace(input.ExpiresAt)
	input.RateLimitResetAt = strings.TrimSpace(input.RateLimitResetAt)
	if input.Platform == "" {
		input.Platform = "anthropic"
	}
	if input.AuthType == "" {
		input.AuthType = "oauth"
	}
	if input.Status == "" {
		input.Status = "active"
	}
	if input.RPMStrategy == "" {
		input.RPMStrategy = "tiered"
	}
	if input.UserMsgQueueMode == "" {
		input.UserMsgQueueMode = "off"
	}
	if input.Schedulable == nil {
		value := true
		input.Schedulable = &value
	}
	if len(input.GroupIDs) == 0 {
		input.GroupIDs = []string{"a"}
	}
	input.GroupIDs = uniqueGroups(input.GroupIDs)
	if input.Name == "" || input.Concurrency <= 0 || input.RateMultiplier < 0 || input.AccountPrice < 0 || input.BaseRPM < 0 || input.BaseRPM > 10000 || input.RPMStickyBuffer < 0 {
		return "", "", errors.New("invalid account fields")
	}
	if input.Status != "active" && input.Status != "error" && input.Status != "disabled" {
		return "", "", errors.New("invalid account status")
	}
	if input.AuthType != "oauth" && input.AuthType != "setup_token" && input.AuthType != "api_key" {
		return "", "", errors.New("invalid account auth type")
	}
	if len(input.GroupIDs) == 0 {
		return "", "", errors.New("select at least one group")
	}
	if input.RPMStrategy != "tiered" && input.RPMStrategy != "sticky_exempt" {
		return "", "", errors.New("invalid RPM strategy")
	}
	if input.UserMsgQueueMode != "off" && input.UserMsgQueueMode != "soft" && input.UserMsgQueueMode != "serial" {
		return "", "", errors.New("invalid user message queue mode")
	}
	credentialsJSON, err := normalizeJSONObject(input.Credentials, existingCredentials)
	if err != nil {
		return "", "", fmt.Errorf("credentials: %w", err)
	}
	extraJSON, err := normalizeJSONObject(input.Extra, existingExtra)
	if err != nil {
		return "", "", fmt.Errorf("extra: %w", err)
	}
	extra := decodeObject(extraJSON)
	extra["base_rpm"] = input.BaseRPM
	extra["rpm_strategy"] = input.RPMStrategy
	extra["rpm_sticky_buffer"] = input.RPMStickyBuffer
	extra["user_msg_queue_mode"] = input.UserMsgQueueMode
	normalizedExtra, _ := json.Marshal(extra)
	return credentialsJSON, string(normalizedExtra), nil
}

func normalizeJSONObject(raw json.RawMessage, fallback string) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback, nil
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", errors.New("must be a JSON object")
	}
	normalized, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func uniqueGroups(input []string) []string {
	seen := map[string]bool{}
	groups := []string{}
	for _, id := range input {
		id = strings.ToLower(strings.TrimSpace(id))
		if (id == "a" || id == "b") && !seen[id] {
			seen[id] = true
			groups = append(groups, id)
		}
	}
	return groups
}

func setAccountGroups(tx *sql.Tx, accountID int64, groups []string, priority int) error {
	if _, err := tx.Exec(`DELETE FROM account_groups WHERE account_id = ?`, accountID); err != nil {
		return err
	}
	for _, groupID := range groups {
		if _, err := tx.Exec(`INSERT INTO account_groups (account_id, group_id, priority) VALUES (?, ?, ?)`, accountID, groupID, priority); err != nil {
			return err
		}
	}
	return nil
}

func (a *app) getAccount(id int64, reveal bool) (account, error) {
	item, err := scanAccount(a.db.QueryRow(accountSelect+` WHERE a.id = ? AND a.deleted_at IS NULL`, id), reveal)
	if err != nil {
		return item, err
	}
	item.GroupIDs, err = a.accountGroupIDs(id)
	return item, err
}

func (a *app) accountGroupIDs(id int64) ([]string, error) {
	rows, err := a.db.Query(`SELECT group_id FROM account_groups WHERE account_id = ? ORDER BY group_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []string{}
	for rows.Next() {
		var groupID string
		if err := rows.Scan(&groupID); err != nil {
			return nil, err
		}
		groups = append(groups, groupID)
	}
	return groups, rows.Err()
}

func (a *app) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	result, err := a.db.Exec(`UPDATE accounts SET status = 'disabled', schedulable = 0, deleted_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleAccountBatchDelete(w http.ResponseWriter, r *http.Request) {
	var input accountBatchDeleteInput
	if !decodeJSON(w, r, &input) {
		return
	}
	ids := make([]int64, 0, len(input.IDs))
	seen := make(map[int64]bool, len(input.IDs))
	for _, id := range input.IDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 || len(ids) > 500 {
		writeError(w, http.StatusBadRequest, "select between 1 and 500 accounts")
		return
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE accounts SET status = 'disabled', schedulable = 0, deleted_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE deleted_at IS NULL AND id IN (`+placeholders+`)`, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		writeDBError(w, err)
		return
	}
	if deleted == 0 {
		writeError(w, http.StatusNotFound, "no selected accounts were found")
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{"deleted": deleted})
}

func (a *app) handlePoolResolve(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PurposeKey string `json:"purpose_key"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.PurposeKey) == "" {
		input.PurposeKey = "default"
	}
	item, groupID, err := a.resolveAccount(input.PurposeKey, true)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusConflict, "no schedulable account for this purpose")
			return
		}
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"purpose_key": input.PurposeKey, "group_id": groupID, "account": item})
}

func (a *app) resolveAccount(purposeKey string, reveal bool) (account, string, error) {
	var groupID string
	if err := a.db.QueryRow(`SELECT active_group_id FROM purposes WHERE key = ?`, strings.ToLower(strings.TrimSpace(purposeKey))).Scan(&groupID); err != nil {
		return account{}, "", err
	}
	query := accountSelect + ` JOIN account_groups ag ON ag.account_id = a.id JOIN groups g ON g.id = ag.group_id WHERE ag.group_id = ? AND g.status = 'active' AND a.deleted_at IS NULL AND ` + accountStatePredicate("a", "normal") + ` ORDER BY ag.priority, a.priority, COALESCE(a.last_used_at, ''), a.id LIMIT 1`
	item, err := scanAccount(a.db.QueryRow(query, groupID), reveal)
	if err != nil {
		return account{}, groupID, err
	}
	item.GroupIDs, err = a.accountGroupIDs(item.ID)
	if err != nil {
		return account{}, groupID, err
	}
	_, err = a.db.Exec(`UPDATE accounts SET last_used_at = `+nowSQL+` WHERE id = ?`, item.ID)
	return item, groupID, err
}

func (a *app) handlePrices(w http.ResponseWriter, _ *http.Request) {
	items, err := a.listPrices()
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) listPrices() ([]modelPrice, error) {
	rows, err := a.db.Query(`SELECT id, model, input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million, source, source_hash, updated_at FROM model_prices ORDER BY CASE WHEN model = '*' THEN 0 ELSE 1 END, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []modelPrice{}
	for rows.Next() {
		var item modelPrice
		if err := rows.Scan(&item.ID, &item.Model, &item.InputPerMillion, &item.OutputPerMillion, &item.CacheCreationPerMillion, &item.CacheReadPerMillion, &item.Source, &item.SourceHash, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *app) handlePriceSave(w http.ResponseWriter, r *http.Request) {
	var input modelPrice
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Model = strings.TrimSpace(input.Model)
	if input.Model == "" || input.InputPerMillion < 0 || input.OutputPerMillion < 0 || input.CacheCreationPerMillion < 0 || input.CacheReadPerMillion < 0 {
		writeError(w, http.StatusBadRequest, "invalid model price")
		return
	}
	_, err := a.db.Exec(`INSERT INTO model_prices (model, input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million, source, source_hash) VALUES (?, ?, ?, ?, ?, 'manual', '') ON CONFLICT(model) DO UPDATE SET input_per_million = excluded.input_per_million, output_per_million = excluded.output_per_million, cache_creation_per_million = excluded.cache_creation_per_million, cache_read_per_million = excluded.cache_read_per_million, source = 'manual', source_hash = '', updated_at = `+nowSQL, input.Model, input.InputPerMillion, input.OutputPerMillion, input.CacheCreationPerMillion, input.CacheReadPerMillion)
	if err != nil {
		writeDBError(w, err)
		return
	}
	var saved modelPrice
	err = a.db.QueryRow(`SELECT id, model, input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million, source, source_hash, updated_at FROM model_prices WHERE model = ?`, input.Model).Scan(&saved.ID, &saved.Model, &saved.InputPerMillion, &saved.OutputPerMillion, &saved.CacheCreationPerMillion, &saved.CacheReadPerMillion, &saved.Source, &saved.SourceHash, &saved.UpdatedAt)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

func (a *app) handlePriceDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var model string
	if err := a.db.QueryRow(`SELECT model FROM model_prices WHERE id = ?`, id).Scan(&model); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "model price not found")
			return
		}
		writeDBError(w, err)
		return
	}
	if model == "*" {
		writeError(w, http.StatusConflict, "fallback price cannot be deleted")
		return
	}
	if _, err := a.db.Exec(`DELETE FROM model_prices WHERE id = ?`, id); err != nil {
		writeDBError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleUsageCreate(w http.ResponseWriter, r *http.Request) {
	var input usageInput
	if !decodeJSON(w, r, &input) {
		return
	}
	item, created, err := a.recordUsage(input)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusConflict, "purpose, group, price, or account is unavailable")
			return
		}
		if strings.Contains(err.Error(), "invalid usage") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeDBError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, item)
}

func (a *app) recordUsage(input usageInput) (usageLog, bool, error) {
	input.RequestID = strings.TrimSpace(input.RequestID)
	input.PurposeKey = strings.ToLower(strings.TrimSpace(input.PurposeKey))
	input.GroupID = strings.ToLower(strings.TrimSpace(input.GroupID))
	input.Model = strings.TrimSpace(input.Model)
	if input.RequestID == "" {
		input.RequestID = newRequestID()
	}
	if input.PurposeKey == "" {
		input.PurposeKey = "default"
	}
	if input.Model == "" || input.InputTokens < 0 || input.OutputTokens < 0 || input.CacheCreationTokens < 0 || input.CacheReadTokens < 0 || input.DurationMS < 0 {
		return usageLog{}, false, errors.New("invalid usage fields")
	}
	if existing, err := a.getUsageByRequestID(input.RequestID); err == nil {
		return existing, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return usageLog{}, false, err
	}

	tx, err := a.db.Begin()
	if err != nil {
		return usageLog{}, false, err
	}
	defer tx.Rollback()

	var purposeID int64
	var purposeName, activeGroupID string
	if err := tx.QueryRow(`SELECT id, name, active_group_id FROM purposes WHERE key = ?`, input.PurposeKey).Scan(&purposeID, &purposeName, &activeGroupID); err != nil {
		return usageLog{}, false, err
	}
	if input.GroupID == "" {
		input.GroupID = activeGroupID
	}
	if input.GroupID != "a" && input.GroupID != "b" {
		return usageLog{}, false, errors.New("invalid usage group")
	}

	var accountName string
	var accountRate float64
	if input.AccountID == 0 {
		err = tx.QueryRow(`SELECT a.id, a.name, a.rate_multiplier FROM accounts a JOIN account_groups ag ON ag.account_id = a.id JOIN groups g ON g.id = ag.group_id WHERE ag.group_id = ? AND g.status = 'active' AND a.deleted_at IS NULL AND a.status = 'active' AND a.schedulable = 1 AND (a.expires_at IS NULL OR a.expires_at > `+nowSQL+`) AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= `+nowSQL+`) ORDER BY ag.priority, a.priority, COALESCE(a.last_used_at, ''), a.id LIMIT 1`, input.GroupID).Scan(&input.AccountID, &accountName, &accountRate)
	} else {
		err = tx.QueryRow(`SELECT a.name, a.rate_multiplier FROM accounts a JOIN account_groups ag ON ag.account_id = a.id WHERE a.id = ? AND ag.group_id = ? AND a.deleted_at IS NULL`, input.AccountID, input.GroupID).Scan(&accountName, &accountRate)
	}
	if err != nil {
		return usageLog{}, false, err
	}
	var groupRate float64
	if err := tx.QueryRow(`SELECT rate_multiplier FROM groups WHERE id = ?`, input.GroupID).Scan(&groupRate); err != nil {
		return usageLog{}, false, err
	}
	var price modelPrice
	err = tx.QueryRow(`SELECT id, model, input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million, source, source_hash, updated_at FROM model_prices WHERE model IN (?, '*') ORDER BY CASE WHEN model = ? THEN 0 ELSE 1 END LIMIT 1`, input.Model, input.Model).Scan(&price.ID, &price.Model, &price.InputPerMillion, &price.OutputPerMillion, &price.CacheCreationPerMillion, &price.CacheReadPerMillion, &price.Source, &price.SourceHash, &price.UpdatedAt)
	if err != nil {
		return usageLog{}, false, err
	}

	item := usageLog{
		RequestID:             input.RequestID,
		PurposeKey:            input.PurposeKey,
		PurposeName:           purposeName,
		GroupID:               input.GroupID,
		AccountID:             input.AccountID,
		AccountName:           accountName,
		Model:                 input.Model,
		InputTokens:           input.InputTokens,
		OutputTokens:          input.OutputTokens,
		CacheCreationTokens:   input.CacheCreationTokens,
		CacheReadTokens:       input.CacheReadTokens,
		GroupRateMultiplier:   groupRate,
		AccountRateMultiplier: accountRate,
		Stream:                input.Stream,
		DurationMS:            input.DurationMS,
	}
	item.InputCost = tokenCost(input.InputTokens, price.InputPerMillion)
	item.OutputCost = tokenCost(input.OutputTokens, price.OutputPerMillion)
	item.CacheCreationCost = tokenCost(input.CacheCreationTokens, price.CacheCreationPerMillion)
	item.CacheReadCost = tokenCost(input.CacheReadTokens, price.CacheReadPerMillion)
	item.BaseCost = money(item.InputCost + item.OutputCost + item.CacheCreationCost + item.CacheReadCost)
	item.BilledCost = money(item.BaseCost * groupRate)
	item.ActualCost = money(item.BaseCost * accountRate)
	if input.ActualCostOverride != nil {
		if *input.ActualCostOverride < 0 {
			return usageLog{}, false, errors.New("invalid usage actual cost")
		}
		item.ActualCost = money(*input.ActualCostOverride)
	}
	item.UserID = optionalID(input.UserID)
	item.APIKeyID = optionalID(input.APIKeyID)
	result, err := tx.Exec(`INSERT INTO usage_logs (user_id, api_key_id, request_id, purpose_id, purpose_key, purpose_name, group_id, account_id, account_name, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, input_cost, output_cost, cache_creation_cost, cache_read_cost, base_cost, billed_cost, actual_cost, group_rate_multiplier, account_rate_multiplier, stream, duration_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item.UserID, item.APIKeyID, item.RequestID, purposeID, item.PurposeKey, item.PurposeName, item.GroupID, item.AccountID, item.AccountName, item.Model, item.InputTokens, item.OutputTokens, item.CacheCreationTokens, item.CacheReadTokens, item.InputCost, item.OutputCost, item.CacheCreationCost, item.CacheReadCost, item.BaseCost, item.BilledCost, item.ActualCost, item.GroupRateMultiplier, item.AccountRateMultiplier, boolInt(item.Stream), item.DurationMS)
	if err != nil {
		return usageLog{}, false, err
	}
	item.ID, _ = result.LastInsertId()
	if err := tx.QueryRow(`SELECT created_at FROM usage_logs WHERE id = ?`, item.ID).Scan(&item.CreatedAt); err != nil {
		return usageLog{}, false, err
	}
	if _, err := tx.Exec(`UPDATE accounts SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, item.AccountID); err != nil {
		return usageLog{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return usageLog{}, false, err
	}
	return item, true, nil
}

func (a *app) getUsageByRequestID(requestID string) (usageLog, error) {
	return scanUsage(a.db.QueryRow(usageSelect+` WHERE request_id = ?`, requestID))
}

const usageSelect = `SELECT id, user_id, api_key_id, request_id, purpose_key, purpose_name, group_id, account_id, account_name, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, input_cost, output_cost, cache_creation_cost, cache_read_cost, base_cost, billed_cost, actual_cost, group_rate_multiplier, account_rate_multiplier, stream, duration_ms, created_at FROM usage_logs`

func scanUsage(row scanner) (usageLog, error) {
	var item usageLog
	var stream int
	var userID, apiKeyID sql.NullInt64
	err := row.Scan(&item.ID, &userID, &apiKeyID, &item.RequestID, &item.PurposeKey, &item.PurposeName, &item.GroupID, &item.AccountID, &item.AccountName, &item.Model, &item.InputTokens, &item.OutputTokens, &item.CacheCreationTokens, &item.CacheReadTokens, &item.InputCost, &item.OutputCost, &item.CacheCreationCost, &item.CacheReadCost, &item.BaseCost, &item.BilledCost, &item.ActualCost, &item.GroupRateMultiplier, &item.AccountRateMultiplier, &stream, &item.DurationMS, &item.CreatedAt)
	item.UserID = nullIntPointer(userID)
	item.APIKeyID = nullIntPointer(apiKeyID)
	item.Stream = stream == 1
	return item, err
}

func optionalID(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}

type usageFilters struct {
	From       string
	To         string
	GroupID    string
	PurposeKey string
	AccountID  int64
	UserID     int64
	Limit      int
	Offset     int
}

func (a *app) handleUsageList(w http.ResponseWriter, r *http.Request) {
	filters := filtersFromRequest(r)
	if user := currentUser(r); user.Role == "user" {
		filters.UserID = user.ID
	}
	items, err := a.listUsage(filters)
	if err != nil {
		writeDBError(w, err)
		return
	}
	total, err := a.countUsage(filters)
	if err != nil {
		writeDBError(w, err)
		return
	}
	page := filters.Offset/filters.Limit + 1
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total, "page": page, "page_size": filters.Limit, "total_pages": totalPages(total, filters.Limit)})
}

func filtersFromRequest(r *http.Request) usageFilters {
	filters := usageFilters{
		From:       strings.TrimSpace(r.URL.Query().Get("from")),
		To:         strings.TrimSpace(r.URL.Query().Get("to")),
		GroupID:    strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))),
		PurposeKey: strings.ToLower(strings.TrimSpace(r.URL.Query().Get("purpose_key"))),
		Limit:      20,
	}
	filters.AccountID, _ = strconv.ParseInt(r.URL.Query().Get("account_id"), 10, 64)
	page, pageSize, offset := paginationFromRequest(r, 20, 100)
	filters.Limit, filters.Offset = pageSize, offset
	_ = page
	return filters
}

func buildUsageWhere(filters usageFilters) (string, []any) {
	conditions := []string{"1 = 1"}
	args := []any{}
	if filters.From != "" {
		conditions = append(conditions, "created_at >= ?")
		args = append(args, normalizeDateStart(filters.From))
	}
	if filters.To != "" {
		conditions = append(conditions, "created_at < ?")
		args = append(args, normalizeDateEnd(filters.To))
	}
	if filters.GroupID == "a" || filters.GroupID == "b" {
		conditions = append(conditions, "group_id = ?")
		args = append(args, filters.GroupID)
	}
	if filters.PurposeKey != "" {
		conditions = append(conditions, "purpose_key = ?")
		args = append(args, filters.PurposeKey)
	}
	if filters.AccountID > 0 {
		conditions = append(conditions, "account_id = ?")
		args = append(args, filters.AccountID)
	}
	if filters.UserID > 0 {
		conditions = append(conditions, "user_id = ?")
		args = append(args, filters.UserID)
	}
	return strings.Join(conditions, " AND "), args
}

func (a *app) listUsage(filters usageFilters) ([]usageLog, error) {
	where, args := buildUsageWhere(filters)
	args = append(args, filters.Limit, filters.Offset)
	rows, err := a.db.Query(usageSelect+` WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []usageLog{}
	for rows.Next() {
		item, err := scanUsage(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *app) countUsage(filters usageFilters) (int64, error) {
	where, args := buildUsageWhere(filters)
	var total int64
	err := a.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE `+where, args...).Scan(&total)
	return total, err
}

func (a *app) handleBilling(w http.ResponseWriter, r *http.Request) {
	filters := filtersFromRequest(r)
	if user := currentUser(r); user.Role == "user" {
		filters.UserID = user.ID
	}
	if filters.From == "" {
		filters.From = startOfMonthUTC()
	}
	summary, err := a.billingSummary(filters)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (a *app) billingSummary(filters usageFilters) (billingSummary, error) {
	where, args := buildUsageWhere(filters)
	result := billingSummary{From: filters.From, To: filters.To, ByGroup: []billingBreakdown{}, ByAccount: []billingBreakdown{}, ByPurpose: []billingBreakdown{}}
	if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_creation_tokens + cache_read_tokens), 0), COALESCE(SUM(base_cost), 0), COALESCE(SUM(billed_cost), 0), COALESCE(SUM(actual_cost), 0), COALESCE(SUM(billed_cost - actual_cost), 0) FROM usage_logs WHERE `+where, args...).Scan(&result.Totals.Requests, &result.Totals.InputTokens, &result.Totals.OutputTokens, &result.Totals.CacheTokens, &result.Totals.BaseCost, &result.Totals.BilledCost, &result.Totals.ActualCost, &result.Totals.Margin); err != nil {
		return result, err
	}
	var err error
	result.ByGroup, err = a.queryBreakdown(where, args, "group_id", "group_id")
	if err == nil {
		result.ByAccount, err = a.queryBreakdown(where, args, "CAST(account_id AS TEXT)", "account_name")
	}
	if err == nil {
		result.ByPurpose, err = a.queryBreakdown(where, args, "purpose_key", "purpose_name")
	}
	return result, err
}

func (a *app) queryBreakdown(where string, args []any, keyExpr, nameExpr string) ([]billingBreakdown, error) {
	query := `SELECT ` + keyExpr + `, ` + nameExpr + `, COUNT(*), COALESCE(SUM(billed_cost), 0), COALESCE(SUM(actual_cost), 0), COALESCE(SUM(billed_cost - actual_cost), 0) FROM usage_logs WHERE ` + where + ` GROUP BY ` + keyExpr + `, ` + nameExpr + ` ORDER BY SUM(billed_cost) DESC`
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []billingBreakdown{}
	for rows.Next() {
		var item billingBreakdown
		if err := rows.Scan(&item.Key, &item.Name, &item.Requests, &item.BilledCost, &item.ActualCost, &item.Margin); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (a *app) queryTotals(from, to string) (billingTotals, error) {
	return a.queryTotalsFiltered(usageFilters{From: from, To: to})
}

func (a *app) queryTotalsFiltered(filters usageFilters) (billingTotals, error) {
	where, args := buildUsageWhere(filters)
	var totals billingTotals
	err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cache_creation_tokens + cache_read_tokens), 0), COALESCE(SUM(base_cost), 0), COALESCE(SUM(billed_cost), 0), COALESCE(SUM(actual_cost), 0), COALESCE(SUM(billed_cost - actual_cost), 0) FROM usage_logs WHERE `+where, args...).Scan(&totals.Requests, &totals.InputTokens, &totals.OutputTokens, &totals.CacheTokens, &totals.BaseCost, &totals.BilledCost, &totals.ActualCost, &totals.Margin)
	return totals, err
}

func paginationFromRequest(r *http.Request, defaultSize, maxSize int) (page, pageSize, offset int) {
	page = 1
	pageSize = defaultSize
	if value, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && value > 0 {
		page = value
	}
	if value, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && value > 0 {
		pageSize = min(value, maxSize)
	} else if value, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && value > 0 {
		pageSize = min(value, maxSize)
	}
	offset = (page - 1) * pageSize
	return page, pageSize, offset
}

func totalPages(total int64, pageSize int) int {
	if total <= 0 || pageSize <= 0 {
		return 1
	}
	return int((total + int64(pageSize) - 1) / int64(pageSize))
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid id")
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeDBError(w http.ResponseWriter, err error) {
	log.Printf("database error: %v", err)
	writeError(w, http.StatusInternalServerError, "database operation failed")
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func floatPointer(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return &value.Float64
}

func nullText(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func nullIntPointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func validOptionalNonNegative(value *float64) bool {
	return value == nil || *value >= 0
}

func decodeObject(raw string) map[string]any {
	value := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &value)
	return value
}

func credentialHint(raw string) string {
	value := decodeObject(raw)
	for _, key := range []string{"access_token", "api_key", "session_key", "refresh_token"} {
		if secret, ok := value[key].(string); ok && secret != "" {
			runes := []rune(secret)
			if len(runes) <= 6 {
				return "••••••"
			}
			return "••••" + string(runes[len(runes)-6:])
		}
	}
	if len(value) > 0 {
		return "JSON · " + strconv.Itoa(len(value)) + " fields"
	}
	return ""
}

func tokenCost(tokens int64, perMillion float64) float64 {
	return money(float64(tokens) / 1_000_000 * perMillion)
}

func money(value float64) float64 {
	return math.Round(value*1e8) / 1e8
}

func newRequestID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "req_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "req_" + hex.EncodeToString(buffer)
}

func startOfTodayUTC() string {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(location)
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location).UTC().Format(time.RFC3339)
}

func startOfMonthUTC() string {
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	now := time.Now().In(location)
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, location).UTC().Format(time.RFC3339)
}

func normalizeDateStart(value string) string {
	if len(value) == 10 {
		return value + "T00:00:00Z"
	}
	return value
}

func normalizeDateEnd(value string) string {
	if len(value) == 10 {
		if parsed, err := time.Parse("2006-01-02", value); err == nil {
			return parsed.AddDate(0, 0, 1).Format("2006-01-02") + "T00:00:00Z"
		}
	}
	return value
}
