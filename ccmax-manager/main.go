package main

import (
	"context"
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

	mysql "github.com/go-sql-driver/mysql"
)

//go:embed web/*
var embeddedWeb embed.FS

const nowSQL = "strftime('%Y-%m-%dT%H:%M:%fZ','now')"

var (
	purposeKeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
	groupIDPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)
)

type scanner interface {
	Scan(dest ...any) error
}

type app struct {
	db            *database
	adminUser     string
	adminPassword string
	authDisabled  bool
	oauthSessions *oauthSessionStore
	priceSync     *priceSyncController
	accountHealth *accountHealthController
	tokenLocks    sync.Map
	quotaLocks    sync.Map
	balanceLocks  sync.Map
	budgetLocks   sync.Map
	queueStates   sync.Map
	dispatchLocks sync.Map
	// capacityWaiters counts requests parked in the per-group capacity queue
	// (group id -> *int64) so a saturated group cannot pile up unbounded waiters.
	capacityWaiters sync.Map
	// coldCacheFlights serialises large requests per (account, session). A cache
	// entry only becomes readable once the first response starts streaming, so
	// concurrent requests sharing a prefix each pay the full cache-creation
	// price and none of them can read what the others are still writing.
	coldCacheFlights      coldCacheFlightTable
	streamHedges          *gatewayHedgeController
	redis                 *redisRuntime
	localITPMReservations localITPMReservationStore
	batchAuthMu           sync.Mutex
	reserveMu             sync.Mutex
	errorPruneMu          sync.Mutex
	lastErrorPrune        time.Time
	errorRetention        int
}

type group struct {
	ID                         string   `json:"id"`
	Name                       string   `json:"name"`
	Description                string   `json:"description"`
	RateMultiplier             float64  `json:"rate_multiplier"`
	DailyLimitUSD              *float64 `json:"daily_limit_usd"`
	MonthlyLimitUSD            *float64 `json:"monthly_limit_usd"`
	NormalRequestMode          bool     `json:"normal_request_mode"`
	ClaudeCodeIdentityEnabled  bool     `json:"claude_code_identity_enabled"`
	StreamHedgeEnabled         bool     `json:"stream_hedge_enabled"`
	AdaptiveHedgeEnabled       bool     `json:"adaptive_hedge_enabled"`
	RPMDispatchEnabled         bool     `json:"rpm_dispatch_enabled"`
	MCPToolNamesEnabled        bool     `json:"mcp_tool_names_enabled"`
	ServiceTierPassthrough     bool     `json:"service_tier_passthrough_enabled"`
	InferenceGeoPassthrough    bool     `json:"inference_geo_passthrough_enabled"`
	SpeedPassthrough           bool     `json:"speed_passthrough_enabled"`
	AnthropicBetaPassthrough   bool     `json:"anthropic_beta_passthrough_enabled"`
	RejectAnthropicDowngrade   bool     `json:"reject_anthropic_downgrade_enabled"`
	RejectDistillation         bool     `json:"reject_distillation_enabled"`
	RequestFormatFilter        bool     `json:"request_format_filter_enabled"`
	QuotaHeaderMasking         bool     `json:"quota_header_masking_enabled"`
	CacheCreationDetail        bool     `json:"cache_creation_detail_enabled"`
	DatelineNormalization      bool     `json:"dateline_normalization_enabled"`
	OverloadCooldownSeconds    int      `json:"overload_cooldown_seconds"`
	RateLimitDownweightEnabled bool     `json:"rate_limit_downweight_enabled"`
	RateLimitCoolingThreshold  int      `json:"rate_limit_cooling_threshold"`
	RateLimitWaitSeconds       int      `json:"rate_limit_wait_seconds"`
	RateLimitSteppedCooldown   bool     `json:"rate_limit_stepped_cooldown_enabled"`
	RateLimitCooldownStep      int      `json:"rate_limit_cooldown_step_seconds"`
	RateLimitDownweightStepped bool     `json:"rate_limit_downweight_stepped_cooldown_enabled"`
	RateLimitDownweightBase    int      `json:"rate_limit_downweight_base_minutes"`
	RateLimitDownweightStep    int      `json:"rate_limit_downweight_step_minutes"`
	FiveHourStaggerEnabled     bool     `json:"five_hour_release_stagger_enabled"`
	FiveHourStaggerMin         int      `json:"five_hour_release_stagger_min_minutes"`
	FiveHourStaggerMax         int      `json:"five_hour_release_stagger_max_minutes"`
	CapacityQueueEnabled       bool     `json:"capacity_queue_enabled"`
	CapacityQueueTimeout       int      `json:"capacity_queue_timeout_seconds"`
	StrategyRequiredEnabled    bool     `json:"strategy_required_enabled"`
	StrategyID                 *int64   `json:"strategy_id"`
	ReservePoolEnabled         bool     `json:"reserve_pool_enabled"`
	Status                     string   `json:"status"`
	ActiveAccounts             int      `json:"active_accounts"`
	TotalAccounts              int      `json:"total_accounts"`
	MonthBilledCost            float64  `json:"month_billed_cost"`
	MonthActualCost            float64  `json:"month_actual_cost"`
	TodayBilledCost            float64  `json:"today_billed_cost"`
	UpdatedAt                  string   `json:"updated_at"`
}

type groupInput struct {
	Name                       string   `json:"name"`
	Description                string   `json:"description"`
	RateMultiplier             float64  `json:"rate_multiplier"`
	DailyLimitUSD              *float64 `json:"daily_limit_usd"`
	MonthlyLimitUSD            *float64 `json:"monthly_limit_usd"`
	NormalRequestMode          bool     `json:"normal_request_mode"`
	ClaudeCodeIdentityEnabled  *bool    `json:"claude_code_identity_enabled"`
	StreamHedgeEnabled         bool     `json:"stream_hedge_enabled"`
	AdaptiveHedgeEnabled       bool     `json:"adaptive_hedge_enabled"`
	RPMDispatchEnabled         *bool    `json:"rpm_dispatch_enabled"`
	MCPToolNamesEnabled        *bool    `json:"mcp_tool_names_enabled"`
	ServiceTierPassthrough     *bool    `json:"service_tier_passthrough_enabled"`
	InferenceGeoPassthrough    *bool    `json:"inference_geo_passthrough_enabled"`
	SpeedPassthrough           *bool    `json:"speed_passthrough_enabled"`
	AnthropicBetaPassthrough   *bool    `json:"anthropic_beta_passthrough_enabled"`
	RejectAnthropicDowngrade   *bool    `json:"reject_anthropic_downgrade_enabled"`
	RejectDistillation         *bool    `json:"reject_distillation_enabled"`
	RequestFormatFilter        *bool    `json:"request_format_filter_enabled"`
	QuotaHeaderMasking         *bool    `json:"quota_header_masking_enabled"`
	CacheCreationDetail        *bool    `json:"cache_creation_detail_enabled"`
	DatelineNormalization      *bool    `json:"dateline_normalization_enabled"`
	OverloadCooldownSeconds    *int     `json:"overload_cooldown_seconds"`
	RateLimitDownweightEnabled *bool    `json:"rate_limit_downweight_enabled"`
	RateLimitCoolingThreshold  *int     `json:"rate_limit_cooling_threshold"`
	RateLimitWaitSeconds       *int     `json:"rate_limit_wait_seconds"`
	RateLimitSteppedCooldown   *bool    `json:"rate_limit_stepped_cooldown_enabled"`
	RateLimitCooldownStep      *int     `json:"rate_limit_cooldown_step_seconds"`
	RateLimitDownweightStepped *bool    `json:"rate_limit_downweight_stepped_cooldown_enabled"`
	RateLimitDownweightBase    *int     `json:"rate_limit_downweight_base_minutes"`
	RateLimitDownweightStep    *int     `json:"rate_limit_downweight_step_minutes"`
	FiveHourStaggerEnabled     *bool    `json:"five_hour_release_stagger_enabled"`
	FiveHourStaggerMin         *int     `json:"five_hour_release_stagger_min_minutes"`
	FiveHourStaggerMax         *int     `json:"five_hour_release_stagger_max_minutes"`
	CapacityQueueEnabled       *bool    `json:"capacity_queue_enabled"`
	CapacityQueueTimeout       *int     `json:"capacity_queue_timeout_seconds"`
	StrategyRequiredEnabled    *bool    `json:"strategy_required_enabled"`
	// StrategyID: nil keeps the current binding, 0 clears it, >0 sets it.
	StrategyID         *int64 `json:"strategy_id"`
	ReservePoolEnabled *bool  `json:"reserve_pool_enabled"`
	Status             string `json:"status"`
	// StrategyShares: nil keeps the current split, an empty slice clears it.
	StrategyShares *[]groupStrategyShareInput `json:"strategy_shares"`
}

type account struct {
	ID                      int64          `json:"id"`
	Name                    string         `json:"name"`
	Platform                string         `json:"platform"`
	AuthType                string         `json:"auth_type"`
	CredentialHint          string         `json:"credential_hint"`
	SourceSKHint            string         `json:"source_sk_hint"`
	HasCredentials          bool           `json:"has_credentials"`
	Credentials             map[string]any `json:"credentials,omitempty"`
	Extra                   map[string]any `json:"extra"`
	Status                  string         `json:"status"`
	Schedulable             bool           `json:"schedulable"`
	Concurrency             int            `json:"concurrency"`
	Priority                int            `json:"priority"`
	RateMultiplier          float64        `json:"rate_multiplier"`
	Notes                   string         `json:"notes"`
	ErrorMessage            string         `json:"error_message"`
	LastUsedAt              string         `json:"last_used_at"`
	ExpiresAt               string         `json:"expires_at"`
	RateLimitResetAt        string         `json:"rate_limit_reset_at"`
	RateLimitWindow         string         `json:"rate_limit_window"`
	RateLimitReason         string         `json:"rate_limit_reason"`
	Consecutive429          int            `json:"consecutive_429"`
	Last429At               string         `json:"last_429_at"`
	DownweightUntil         string         `json:"rate_limit_downweight_until"`
	QuotaRefreshedAt        string         `json:"quota_refreshed_at"`
	LimitWindow             string         `json:"limit_window"`
	GroupIDs                []string       `json:"group_ids"`
	CreatedAt               string         `json:"created_at"`
	UpdatedAt               string         `json:"updated_at"`
	ProxyPoolID             *int64         `json:"proxy_pool_id"`
	ProxyPoolName           string         `json:"proxy_pool_name"`
	ProxyID                 *int64         `json:"proxy_id"`
	ProxyName               string         `json:"proxy_name"`
	ProxyHint               string         `json:"proxy_hint"`
	ProxyIP                 string         `json:"proxy_ip"`
	AutoProxy               bool           `json:"auto_proxy"`
	BaseRPM                 int            `json:"base_rpm"`
	RPMStrategy             string         `json:"rpm_strategy"`
	RPMStickyBuffer         int            `json:"rpm_sticky_buffer"`
	UserMsgQueueMode        string         `json:"user_msg_queue_mode"`
	StrategyID              *int64         `json:"strategy_id"`
	AuthStatus              string         `json:"auth_status"`
	AuthError               string         `json:"auth_error"`
	AuthCheckedAt           string         `json:"auth_checked_at"`
	TokenExpiresAt          string         `json:"token_expires_at"`
	Quota5H                 float64        `json:"quota_5h_utilization"`
	Quota5HResetAt          string         `json:"quota_5h_reset_at"`
	Quota5HThresholdEnabled bool           `json:"quota_5h_threshold_enabled"`
	Quota5HThresholdPercent int            `json:"quota_5h_threshold_percent"`
	Quota7D                 float64        `json:"quota_7d_utilization"`
	Quota7DResetAt          string         `json:"quota_7d_reset_at"`
	Quota7DThresholdEnabled bool           `json:"quota_7d_threshold_enabled"`
	Quota7DThresholdPercent int            `json:"quota_7d_threshold_percent"`
	QuotaSampledAt          string         `json:"quota_sampled_at"`
	SubscriptionType        string         `json:"subscription_type"`
	RateLimitTier           string         `json:"rate_limit_tier"`
	AccountPrice            float64        `json:"account_price"`
	OnboardedAt             string         `json:"onboarded_at"`
	ReauthorizedAt          string         `json:"reauthorized_at"`
	ReauthorizationCount    int            `json:"reauthorization_count"`
	InvalidatedAt           string         `json:"invalidated_at"`
	ArchivedAt              string         `json:"archived_at"`
	SurvivalTotal           int64          `json:"survival_seconds_total"`
	SurvivalSeconds         int64          `json:"survival_seconds"`
	RequestCount            int64          `json:"request_count"`
	InputTokens             int64          `json:"input_tokens"`
	OutputTokens            int64          `json:"output_tokens"`
	TotalBilledCost         float64        `json:"total_billed_cost"`
	TotalActualCost         float64        `json:"total_actual_cost"`
	ProxyStatus             string         `json:"proxy_status"`
	DispatchStatus          string         `json:"dispatch_status"`
}

type accountInput struct {
	Name                    string          `json:"name"`
	Platform                string          `json:"platform"`
	AuthType                string          `json:"auth_type"`
	SessionKey              string          `json:"session_key"`
	Credentials             json.RawMessage `json:"credentials"`
	Extra                   json.RawMessage `json:"extra"`
	Status                  string          `json:"status"`
	Schedulable             *bool           `json:"schedulable"`
	Concurrency             int             `json:"concurrency"`
	Priority                int             `json:"priority"`
	RateMultiplier          float64         `json:"rate_multiplier"`
	Notes                   string          `json:"notes"`
	ErrorMessage            string          `json:"error_message"`
	ExpiresAt               string          `json:"expires_at"`
	RateLimitResetAt        string          `json:"rate_limit_reset_at"`
	GroupIDs                []string        `json:"group_ids"`
	ProxyPoolID             *int64          `json:"proxy_pool_id"`
	ProxyID                 *int64          `json:"proxy_id"`
	ProxyText               string          `json:"proxy_text"`
	AutoProxy               bool            `json:"auto_proxy"`
	BaseRPM                 int             `json:"base_rpm"`
	RPMStrategy             string          `json:"rpm_strategy"`
	RPMStickyBuffer         int             `json:"rpm_sticky_buffer"`
	UserMsgQueueMode        string          `json:"user_msg_queue_mode"`
	AccountPrice            float64         `json:"account_price"`
	Quota5HThresholdEnabled *bool           `json:"quota_5h_threshold_enabled"`
	Quota5HThresholdPercent *int            `json:"quota_5h_threshold_percent"`
	Quota7DThresholdEnabled *bool           `json:"quota_7d_threshold_enabled"`
	Quota7DThresholdPercent *int            `json:"quota_7d_threshold_percent"`
	// StrategyID binds the account to a dispatch strategy. nil keeps the
	// current binding (old clients), 0 clears it, >0 sets it.
	StrategyID *int64 `json:"strategy_id"`
}

type accountBatchDeleteInput struct {
	IDs []int64 `json:"ids"`
}

type accountBatchScheduleInput struct {
	IDs         []int64 `json:"ids"`
	Schedulable bool    `json:"schedulable"`
}

type accountBatchUpdateInput struct {
	IDs                     []int64   `json:"ids"`
	Concurrency             *int      `json:"concurrency"`
	Priority                *int      `json:"priority"`
	RateMultiplier          *float64  `json:"rate_multiplier"`
	AccountPrice            *float64  `json:"account_price"`
	BaseRPM                 *int      `json:"base_rpm"`
	RPMStrategy             *string   `json:"rpm_strategy"`
	RPMStickyBuffer         *int      `json:"rpm_sticky_buffer"`
	UserMsgQueueMode        *string   `json:"user_msg_queue_mode"`
	StrategyID              *int64    `json:"strategy_id"`
	GroupIDs                *[]string `json:"group_ids"`
	Quota5HThresholdEnabled *bool     `json:"quota_5h_threshold_enabled"`
	Quota5HThresholdPercent *int      `json:"quota_5h_threshold_percent"`
	Quota7DThresholdEnabled *bool     `json:"quota_7d_threshold_enabled"`
	Quota7DThresholdPercent *int      `json:"quota_7d_threshold_percent"`
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
	ClientRequestID     string   `json:"-"`
	TraceID             string   `json:"-"`
	UpstreamRequestID   string   `json:"-"`
	PurposeKey          string   `json:"purpose_key"`
	GroupID             string   `json:"group_id"`
	AccountID           int64    `json:"account_id"`
	AccountSKHint       string   `json:"-"`
	Model               string   `json:"model"`
	InputTokens         int64    `json:"input_tokens"`
	OutputTokens        int64    `json:"output_tokens"`
	CacheCreationTokens int64    `json:"cache_creation_tokens"`
	CacheReadTokens     int64    `json:"cache_read_tokens"`
	ActualCostOverride  *float64 `json:"actual_cost_override"`
	BilledCostOverride  *float64 `json:"-"`
	Stream              bool     `json:"stream"`
	DurationMS          int      `json:"duration_ms"`
}

type usageLog struct {
	ID                    int64   `json:"id"`
	UserID                *int64  `json:"user_id,omitempty"`
	APIKeyID              *int64  `json:"api_key_id,omitempty"`
	APIKeyName            string  `json:"api_key_name"`
	APIKeyPrefix          string  `json:"api_key_prefix"`
	AccountSKHint         string  `json:"account_sk_hint"`
	RequestID             string  `json:"request_id"`
	ClientRequestID       string  `json:"client_request_id"`
	TraceID               string  `json:"trace_id"`
	UpstreamRequestID     string  `json:"upstream_request_id"`
	PurposeKey            string  `json:"purpose_key"`
	PurposeName           string  `json:"purpose_name"`
	GroupID               string  `json:"group_id"`
	AccountID             int64   `json:"account_id"`
	AccountName           string  `json:"account_name"`
	ProxyIP               string  `json:"proxy_ip"`
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
	From             string             `json:"from"`
	To               string             `json:"to"`
	AvailableBalance *float64           `json:"available_balance"`
	Totals           billingTotals      `json:"totals"`
	ByGroup          []billingBreakdown `json:"by_group"`
	ByAccount        []billingBreakdown `json:"by_account"`
	ByPurpose        []billingBreakdown `json:"by_purpose"`
	ByAPIKey         []billingBreakdown `json:"by_api_key"`
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

func runServer() {
	addr := envOr("CCMAX_ADDR", "127.0.0.1:8088")
	a, err := newApp("")
	if err != nil {
		log.Fatal(err)
	}
	defer a.db.Close()
	defer a.redis.Close()
	stopPricing := a.startPriceSyncScheduler()
	defer stopPricing()
	stopTokenRefresh := a.startTokenRefreshScheduler()
	defer stopTokenRefresh()
	stopAccountHealth := a.startAccountHealthScheduler()
	defer stopAccountHealth()
	stopRateLimitSweep := a.startAccountRateLimitSweeper()
	defer stopRateLimitSweep()

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
	dialect := dialectSQLite
	driver := "sqlite3"
	dsn := ""
	if mysqlDSN := strings.TrimSpace(os.Getenv("CCMAX_MYSQL_DSN")); mysqlDSN != "" {
		dialect = dialectMySQL
		driver = "mysql"
		config, err := mysql.ParseDSN(mysqlDSN)
		if err != nil {
			return nil, fmt.Errorf("parse MySQL DSN: %w", err)
		}
		config.ParseTime = true
		config.Loc = time.UTC
		if config.Params == nil {
			config.Params = map[string]string{}
		}
		config.Params["time_zone"] = "'+00:00'"
		dsn = config.FormatDSN()
	}

	if dialect == dialectSQLite && dataPath != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(dataPath), 0o755); err != nil && filepath.Dir(dataPath) != "." {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	if dialect == dialectSQLite {
		dsn = "file:" + filepath.ToSlash(dataPath) + "?_foreign_keys=on&_busy_timeout=5000&_txlock=immediate"
		if dataPath == ":memory:" {
			dsn = "file:ccmax-manager-memory?mode=memory&cache=shared&_foreign_keys=on"
		} else {
			dsn += "&_journal_mode=WAL"
		}
	}
	rawDB, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db := &database{DB: rawDB, dialect: dialect}
	if dialect == dialectMySQL {
		db.SetMaxOpenConns(envInt("CCMAX_DB_MAX_OPEN_CONNS", 100))
		db.SetMaxIdleConns(envInt("CCMAX_DB_MAX_IDLE_CONNS", 25))
		db.SetConnMaxLifetime(30 * time.Minute)
		db.SetConnMaxIdleTime(5 * time.Minute)
	} else {
		db.SetMaxOpenConns(5)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	a := &app{
		db:             db,
		adminUser:      envOr("CCMAX_ADMIN_USER", "admin"),
		adminPassword:  envOr("CCMAX_ADMIN_PASSWORD", "ccmax-admin"),
		authDisabled:   strings.EqualFold(strings.TrimSpace(os.Getenv("CCMAX_AUTH_DISABLED")), "true") || strings.TrimSpace(os.Getenv("CCMAX_AUTH_DISABLED")) == "1",
		errorRetention: envInt("CCMAX_ERROR_LOG_RETENTION_DAYS", 7),
		oauthSessions:  newOAuthSessionStore(),
		priceSync:      newPriceSyncController(),
		accountHealth:  newAccountHealthController(),
		streamHedges:   newGatewayHedgeController(),
	}
	a.redis, err = newRedisRuntime()
	if err != nil {
		db.Close()
		return nil, err
	}
	if dialect == dialectMySQL {
		if err := a.migrateMySQL(); err != nil {
			db.Close()
			return nil, err
		}
	} else {
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
	}
	// Dialect-neutral data migrations run for both branches. On a MySQL import
	// run this executes before migrateSQLiteToMySQL fills the target, so the
	// backfills see an empty table; they are idempotent and run on every
	// startup, so the next process start applies them.
	if err := a.migrateSharedData(); err != nil {
		db.Close()
		return nil, err
	}
	if err := a.pruneGatewayErrorLogs(true); err != nil {
		db.Close()
		return nil, err
	}
	return a, nil
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (a *app) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			rate_multiplier REAL NOT NULL DEFAULT 1 CHECK (rate_multiplier >= 0),
			daily_limit_usd REAL,
			monthly_limit_usd REAL,
			normal_request_mode INTEGER NOT NULL DEFAULT 0,
			claude_code_identity_enabled INTEGER NOT NULL DEFAULT 0,
			stream_hedge_enabled INTEGER NOT NULL DEFAULT 0,
			adaptive_hedge_enabled INTEGER NOT NULL DEFAULT 0,
			rpm_dispatch_enabled INTEGER NOT NULL DEFAULT 1,
			mcp_tool_names_enabled INTEGER NOT NULL DEFAULT 0,
			service_tier_passthrough_enabled INTEGER NOT NULL DEFAULT 0,
			inference_geo_passthrough_enabled INTEGER NOT NULL DEFAULT 0,
			speed_passthrough_enabled INTEGER NOT NULL DEFAULT 0,
			anthropic_beta_passthrough_enabled INTEGER NOT NULL DEFAULT 0,
			reject_anthropic_downgrade_enabled INTEGER NOT NULL DEFAULT 0,
			reject_distillation_enabled INTEGER NOT NULL DEFAULT 0,
			request_format_filter_enabled INTEGER NOT NULL DEFAULT 0,
			quota_header_masking_enabled INTEGER NOT NULL DEFAULT 0,
			cache_creation_detail_enabled INTEGER NOT NULL DEFAULT 0,
			dateline_normalization_enabled INTEGER NOT NULL DEFAULT 1,
			overload_cooldown_seconds INTEGER NOT NULL DEFAULT 10 CHECK (overload_cooldown_seconds BETWEEN 1 AND 600),
			rate_limit_downweight_enabled INTEGER NOT NULL DEFAULT 1,
			rate_limit_cooling_threshold INTEGER NOT NULL DEFAULT 3 CHECK (rate_limit_cooling_threshold BETWEEN 1 AND 10),
			rate_limit_wait_seconds INTEGER NOT NULL DEFAULT 120 CHECK (rate_limit_wait_seconds BETWEEN 60 AND 120),
			rate_limit_stepped_cooldown_enabled INTEGER NOT NULL DEFAULT 0,
			rate_limit_cooldown_step_seconds INTEGER NOT NULL DEFAULT 30 CHECK (rate_limit_cooldown_step_seconds BETWEEN 1 AND 60),
			rate_limit_downweight_stepped_cooldown_enabled INTEGER NOT NULL DEFAULT 0,
			rate_limit_downweight_base_minutes INTEGER NOT NULL DEFAULT 60 CHECK (rate_limit_downweight_base_minutes BETWEEN 1 AND 315),
			rate_limit_downweight_step_minutes INTEGER NOT NULL DEFAULT 60 CHECK (rate_limit_downweight_step_minutes BETWEEN 1 AND 315),
			five_hour_release_stagger_enabled INTEGER NOT NULL DEFAULT 1,
			five_hour_release_stagger_min_minutes INTEGER NOT NULL DEFAULT 15 CHECK (five_hour_release_stagger_min_minutes BETWEEN 0 AND 315),
			five_hour_release_stagger_max_minutes INTEGER NOT NULL DEFAULT 30 CHECK (five_hour_release_stagger_max_minutes BETWEEN 0 AND 315),
			capacity_queue_enabled INTEGER NOT NULL DEFAULT 0,
			capacity_queue_timeout_seconds INTEGER NOT NULL DEFAULT 30 CHECK (capacity_queue_timeout_seconds BETWEEN 1 AND 600),
			strategy_required_enabled INTEGER NOT NULL DEFAULT 0,
			reserve_pool_enabled INTEGER NOT NULL DEFAULT 0,
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
			source_sk_hint TEXT NOT NULL DEFAULT '',
			extra_json TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'error', 'disabled')),
			schedulable INTEGER NOT NULL DEFAULT 1,
			concurrency INTEGER NOT NULL DEFAULT 10 CHECK (concurrency > 0),
			priority INTEGER NOT NULL DEFAULT 50,
			rate_multiplier REAL NOT NULL DEFAULT 1 CHECK (rate_multiplier >= 0),
			notes TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			last_used_at TEXT,
			expires_at TEXT,
			rate_limit_reset_at TEXT,
			rate_limit_reason TEXT NOT NULL DEFAULT '',
			consecutive_429 INTEGER NOT NULL DEFAULT 0,
			last_429_at TEXT,
			rate_limit_downweight_until TEXT,
			quota_refreshed_at TEXT,
			quota_5h_threshold_enabled INTEGER NOT NULL DEFAULT 0,
			quota_5h_threshold_percent INTEGER NOT NULL DEFAULT 80 CHECK (quota_5h_threshold_percent BETWEEN 1 AND 100),
			quota_7d_threshold_enabled INTEGER NOT NULL DEFAULT 0,
			quota_7d_threshold_percent INTEGER NOT NULL DEFAULT 80 CHECK (quota_7d_threshold_percent BETWEEN 1 AND 100),
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
		`CREATE TABLE IF NOT EXISTS account_model_cooldowns (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			model TEXT NOT NULL,
			reset_at TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
			PRIMARY KEY (account_id, model)
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
			account_sk_hint TEXT NOT NULL DEFAULT '',
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
		`CREATE TABLE IF NOT EXISTS account_usage_totals (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			request_count INTEGER NOT NULL DEFAULT 0,
			input_tokens INTEGER NOT NULL DEFAULT 0,
			output_tokens INTEGER NOT NULL DEFAULT 0,
			billed_cost REAL NOT NULL DEFAULT 0,
			actual_cost REAL NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_dispatch ON accounts(status, schedulable, priority, last_used_at) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_account_groups_group ON account_groups(group_id, priority, account_id)`,
		`CREATE INDEX IF NOT EXISTS idx_account_model_cooldowns_reset ON account_model_cooldowns(model, reset_at, account_id)`,
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
	mux.HandleFunc("GET /api/proxies/archived", a.handleArchivedProxies)
	mux.HandleFunc("POST /api/proxies/batch", a.handleProxyBatch)
	mux.HandleFunc("POST /api/proxies/batch-delete", a.handleProxyBatchDelete)
	mux.HandleFunc("POST /api/proxies/batch-test", a.handleProxyBatchTest)
	mux.HandleFunc("PUT /api/proxies/{id}", a.handleProxyUpdate)
	mux.HandleFunc("DELETE /api/proxies/{id}", a.handleProxyDelete)
	mux.HandleFunc("POST /api/proxies/{id}/restore", a.handleProxyRestore)
	mux.HandleFunc("POST /api/proxies/{id}/test", a.handleProxyTest)
	mux.HandleFunc("GET /api/dashboard", a.handleDashboard)
	mux.HandleFunc("GET /api/groups", a.handleGroups)
	mux.HandleFunc("POST /api/groups", a.handleGroupCreate)
	mux.HandleFunc("PUT /api/groups/{id}", a.handleGroupUpdate)
	mux.HandleFunc("GET /api/groups/{id}/strategy-shares", a.handleGroupStrategyShares)
	mux.HandleFunc("GET /api/purposes", a.handlePurposes)
	mux.HandleFunc("POST /api/purposes", a.handlePurposeCreate)
	mux.HandleFunc("PUT /api/purposes/{id}", a.handlePurposeUpdate)
	mux.HandleFunc("DELETE /api/purposes/{id}", a.handlePurposeDelete)
	mux.HandleFunc("GET /api/accounts", a.handleAccounts)
	mux.HandleFunc("GET /api/accounts/summary", a.handleAccountSummary)
	mux.HandleFunc("POST /api/accounts", a.handleAccountCreate)
	mux.HandleFunc("POST /api/accounts/batch-authorize", a.handleBatchAuthorization)
	mux.HandleFunc("POST /api/accounts/batch-delete", a.handleAccountBatchDelete)
	mux.HandleFunc("POST /api/accounts/batch-archive", a.handleAccountBatchArchive)
	mux.HandleFunc("POST /api/accounts/batch-schedule", a.handleAccountBatchSchedule)
	mux.HandleFunc("POST /api/accounts/batch-update", a.handleAccountBatchUpdate)
	mux.HandleFunc("POST /api/accounts/health/refresh", a.handleAccountHealthRefresh)
	mux.HandleFunc("PUT /api/accounts/{id}", a.handleAccountUpdate)
	mux.HandleFunc("DELETE /api/accounts/{id}", a.handleAccountDelete)
	mux.HandleFunc("POST /api/accounts/{id}/archive", a.handleAccountArchive)
	mux.HandleFunc("POST /api/accounts/{id}/restore", a.handleAccountRestore)
	mux.HandleFunc("POST /api/accounts/{id}/quota/refresh", a.handleAccountQuotaRefresh)
	mux.HandleFunc("POST /api/accounts/{id}/rate-limit/reset", a.handleAccountRateLimitReset)
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
	mux.HandleFunc("GET /api/audit-logs/cache-prefixes", a.handleCachePrefixAuditLogs)
	mux.HandleFunc("GET /api/usage", a.handleUsageList)
	mux.HandleFunc("POST /api/usage", a.handleUsageCreate)
	mux.HandleFunc("GET /api/billing", a.handleBilling)
	mux.HandleFunc("GET /api/stats/daily", a.handleDailyStats)
	mux.HandleFunc("GET /api/stats/realtime", a.handleRealtimeStats)
	mux.HandleFunc("GET /api/authorization-logs", a.handleAuthorizationStats)
	mux.HandleFunc("GET /api/authorization-deauth", a.handleAuthorizationDeauth)
	mux.HandleFunc("GET /api/error-logs", a.handleErrorLogs)
	mux.HandleFunc("GET /api/error-insights", a.handleErrorInsights)
	mux.HandleFunc("GET /api/strategies", a.handleStrategies)
	mux.HandleFunc("POST /api/strategies", a.handleStrategyCreate)
	mux.HandleFunc("PUT /api/strategies/{id}", a.handleStrategyUpdate)
	mux.HandleFunc("DELETE /api/strategies/{id}", a.handleStrategyDelete)
	mux.HandleFunc("GET /api/strategies/observe", a.handleStrategyObserve)
	mux.HandleFunc("POST /v1/messages", a.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", a.handleCountTokens)
	mux.HandleFunc("POST /v1/chat/completions", a.handleChatCompletions)
	mux.HandleFunc("POST /chat/completions", a.handleChatCompletions)
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
	return a.securityHeaders(a.authMiddleware(a.auditMiddleware(a.gatewayErrorMiddleware(mux))))
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.db.PingContext(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	if err := a.redis.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "runtime store unavailable")
		return
	}
	runtimeStore := "mysql"
	if a.redis != nil {
		runtimeStore = "redis"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":        "ok",
		"database":      string(a.db.dialect),
		"runtime_store": runtimeStore,
	})
}

func (a *app) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	var result dashboard
	accountScope, accountArgs := scopedAccountCondition(user, "accounts")
	if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN `+accountStatePredicate("accounts", "normal")+` THEN 1 ELSE 0 END), 0), COALESCE(SUM(CASE WHEN `+accountStatePredicate("accounts", "unavailable")+` THEN 1 ELSE 0 END), 0), COALESCE(SUM(CASE WHEN `+accountStatePredicate("accounts", "error")+` THEN 1 ELSE 0 END), 0) FROM accounts WHERE deleted_at IS NULL AND archived_at IS NULL AND `+accountScope, accountArgs...).Scan(&result.AccountsTotal, &result.AccountsActive, &result.AccountsUnavailable, &result.AccountsDead); err != nil {
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
	if user.Role == "user" {
		redactBillingTotals(&result.Today)
		redactBillingTotals(&result.Month)
		redactUsageCosts(result.RecentUsage)
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
	rows, err := a.db.Query(`SELECT id, name, description, rate_multiplier, daily_limit_usd, monthly_limit_usd, normal_request_mode, claude_code_identity_enabled, stream_hedge_enabled, adaptive_hedge_enabled, rpm_dispatch_enabled, mcp_tool_names_enabled, service_tier_passthrough_enabled, inference_geo_passthrough_enabled, speed_passthrough_enabled, anthropic_beta_passthrough_enabled, reject_anthropic_downgrade_enabled, reject_distillation_enabled, request_format_filter_enabled, quota_header_masking_enabled, cache_creation_detail_enabled, dateline_normalization_enabled, overload_cooldown_seconds, rate_limit_downweight_enabled, rate_limit_cooling_threshold, rate_limit_wait_seconds, rate_limit_stepped_cooldown_enabled, rate_limit_cooldown_step_seconds, rate_limit_downweight_stepped_cooldown_enabled, rate_limit_downweight_base_minutes, rate_limit_downweight_step_minutes, five_hour_release_stagger_enabled, five_hour_release_stagger_min_minutes, five_hour_release_stagger_max_minutes, capacity_queue_enabled, capacity_queue_timeout_seconds, strategy_required_enabled, strategy_id, reserve_pool_enabled, status, updated_at FROM groups ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := []group{}
	for rows.Next() {
		var item group
		var daily, monthly sql.NullFloat64
		var strategyID sql.NullInt64
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.RateMultiplier, &daily, &monthly, &item.NormalRequestMode, &item.ClaudeCodeIdentityEnabled, &item.StreamHedgeEnabled, &item.AdaptiveHedgeEnabled, &item.RPMDispatchEnabled, &item.MCPToolNamesEnabled, &item.ServiceTierPassthrough, &item.InferenceGeoPassthrough, &item.SpeedPassthrough, &item.AnthropicBetaPassthrough, &item.RejectAnthropicDowngrade, &item.RejectDistillation, &item.RequestFormatFilter, &item.QuotaHeaderMasking, &item.CacheCreationDetail, &item.DatelineNormalization, &item.OverloadCooldownSeconds, &item.RateLimitDownweightEnabled, &item.RateLimitCoolingThreshold, &item.RateLimitWaitSeconds, &item.RateLimitSteppedCooldown, &item.RateLimitCooldownStep, &item.RateLimitDownweightStepped, &item.RateLimitDownweightBase, &item.RateLimitDownweightStep, &item.FiveHourStaggerEnabled, &item.FiveHourStaggerMin, &item.FiveHourStaggerMax, &item.CapacityQueueEnabled, &item.CapacityQueueTimeout, &item.StrategyRequiredEnabled, &strategyID, &item.ReservePoolEnabled, &item.Status, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.DailyLimitUSD = floatPointer(daily)
		item.MonthlyLimitUSD = floatPointer(monthly)
		item.StrategyID = nullIntPointer(strategyID)
		groups = append(groups, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range groups {
		item := &groups[index]
		if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN `+accountStatePredicate("a", "normal")+` THEN 1 ELSE 0 END), 0) FROM account_groups ag JOIN accounts a ON a.id = ag.account_id WHERE ag.group_id = ? AND a.deleted_at IS NULL AND a.archived_at IS NULL`, item.ID).Scan(&item.TotalAccounts, &item.ActiveAccounts); err != nil {
			return nil, err
		}
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0), COALESCE(SUM(actual_cost), 0) FROM usage_logs WHERE group_id = ? AND created_at >= ?`, item.ID, startOfMonthUTC()).Scan(&item.MonthBilledCost, &item.MonthActualCost); err != nil {
			return nil, err
		}
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0) FROM usage_logs WHERE group_id = ? AND created_at >= ?`, item.ID, startOfTodayUTC()).Scan(&item.TodayBilledCost); err != nil {
			return nil, err
		}
	}
	return groups, nil
}

func normalizeGroupInput(input *groupInput) error {
	input.Name = strings.TrimSpace(input.Name)
	input.Description = strings.TrimSpace(input.Description)
	if input.Status == "" {
		input.Status = "active"
	}
	if input.Name == "" || len([]rune(input.Name)) > 80 || input.RateMultiplier < 0 || (input.Status != "active" && input.Status != "disabled") {
		return errors.New("invalid group fields")
	}
	if !validOptionalNonNegative(input.DailyLimitUSD) || !validOptionalNonNegative(input.MonthlyLimitUSD) {
		return errors.New("limits must be non-negative")
	}
	if input.StreamHedgeEnabled && input.AdaptiveHedgeEnabled {
		return errors.New("only one stream dispatch algorithm can be enabled")
	}
	if input.RPMDispatchEnabled != nil && *input.RPMDispatchEnabled && (input.StreamHedgeEnabled || input.AdaptiveHedgeEnabled) {
		return errors.New("RPM concentrated dispatch cannot be combined with stream hedging")
	}
	if input.OverloadCooldownSeconds != nil && (*input.OverloadCooldownSeconds < 1 || *input.OverloadCooldownSeconds > 600) {
		return errors.New("529 cooldown must be between 1 and 600 seconds")
	}
	if input.RateLimitCoolingThreshold != nil && (*input.RateLimitCoolingThreshold < 1 || *input.RateLimitCoolingThreshold > maxRateLimitCoolingThreshold) {
		return fmt.Errorf("429 cooling threshold must be between 1 and %d", maxRateLimitCoolingThreshold)
	}
	if input.RateLimitWaitSeconds != nil && (*input.RateLimitWaitSeconds < minRateLimitCooldownSeconds || *input.RateLimitWaitSeconds > maxRateLimitCooldownSeconds) {
		return fmt.Errorf("429 cooldown must be between %d and %d seconds", minRateLimitCooldownSeconds, maxRateLimitCooldownSeconds)
	}
	if input.RateLimitCooldownStep != nil && (*input.RateLimitCooldownStep < 1 || *input.RateLimitCooldownStep > maxRateLimitCooldownStepSeconds) {
		return fmt.Errorf("429 cooldown step must be between 1 and %d seconds", maxRateLimitCooldownStepSeconds)
	}
	if input.RateLimitDownweightBase != nil && (*input.RateLimitDownweightBase < minRateLimitDownweightMinutes || *input.RateLimitDownweightBase > maxRateLimitDownweightMinutes) {
		return fmt.Errorf("429 downweight base must be between %d and %d minutes", minRateLimitDownweightMinutes, maxRateLimitDownweightMinutes)
	}
	if input.RateLimitDownweightStep != nil && (*input.RateLimitDownweightStep < minRateLimitDownweightMinutes || *input.RateLimitDownweightStep > maxRateLimitDownweightMinutes) {
		return fmt.Errorf("429 downweight step must be between %d and %d minutes", minRateLimitDownweightMinutes, maxRateLimitDownweightMinutes)
	}
	if input.FiveHourStaggerMin != nil && (*input.FiveHourStaggerMin < 0 || *input.FiveHourStaggerMin > maxFiveHourReleaseStaggerMinutes) {
		return fmt.Errorf("5h release stagger minimum must be between 0 and %d minutes", maxFiveHourReleaseStaggerMinutes)
	}
	if input.FiveHourStaggerMax != nil && (*input.FiveHourStaggerMax < 0 || *input.FiveHourStaggerMax > maxFiveHourReleaseStaggerMinutes) {
		return fmt.Errorf("5h release stagger maximum must be between 0 and %d minutes", maxFiveHourReleaseStaggerMinutes)
	}
	if (input.FiveHourStaggerMin == nil) != (input.FiveHourStaggerMax == nil) {
		return errors.New("5h release stagger minimum and maximum must be provided together")
	}
	if input.FiveHourStaggerMin != nil && input.FiveHourStaggerMax != nil && *input.FiveHourStaggerMin > *input.FiveHourStaggerMax {
		return errors.New("5h release stagger minimum cannot exceed maximum")
	}
	if input.CapacityQueueTimeout != nil && (*input.CapacityQueueTimeout < 1 || *input.CapacityQueueTimeout > 600) {
		return errors.New("capacity queue timeout must be between 1 and 600 seconds")
	}
	return nil
}

func (a *app) validateReserveGroupChange(groupID string, enabled bool) error {
	if !enabled {
		return nil
	}
	var existing string
	err := a.db.QueryRow(`SELECT id FROM groups WHERE reserve_pool_enabled = 1 AND id != ? LIMIT 1`, groupID).Scan(&existing)
	if err == nil {
		return fmt.Errorf("reserve account pool already exists: %s", existing)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if groupID == "" {
		return nil
	}
	var references int
	if err := a.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM api_keys WHERE group_id = ? AND deleted_at IS NULL) +
		(SELECT COUNT(*) FROM purposes WHERE active_group_id = ?)`, groupID, groupID).Scan(&references); err != nil {
		return err
	}
	if references > 0 {
		return errors.New("a group used by API keys or purposes cannot become a reserve pool")
	}
	var sharedAccounts int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_groups own
		WHERE own.group_id = ? AND EXISTS (
			SELECT 1 FROM account_groups other WHERE other.account_id = own.account_id AND other.group_id != own.group_id
		)`, groupID).Scan(&sharedAccounts); err != nil {
		return err
	}
	if sharedAccounts > 0 {
		return errors.New("reserve pool accounts cannot also belong to dispatch groups")
	}
	return nil
}

func boolPointerValue(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func intPointerValue(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func int64PointerValue(value *int64, fallback int64) int64 {
	if value == nil {
		return fallback
	}
	return *value
}

// resolveStrategyBinding validates a strategy_id input. It returns the SQL
// value to store (nil = unbound) and an error when the strategy is missing.
func (a *app) resolveStrategyBinding(id *int64) (any, error) {
	if id == nil || *id <= 0 {
		return nil, nil
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM dispatch_strategies WHERE id = ? AND deleted_at IS NULL`, *id).Scan(&count); err != nil {
		return nil, err
	}
	if count == 0 {
		return nil, errors.New("dispatch strategy not found")
	}
	return *id, nil
}

func (a *app) groupNameAvailable(name, exceptID string) (bool, error) {
	var count int
	err := a.db.QueryRow(`SELECT COUNT(*) FROM groups WHERE lower(name) = lower(?) AND id != ?`, name, exceptID).Scan(&count)
	return count == 0, err
}

func (a *app) getGroup(id string) (group, error) {
	items, err := a.listGroups()
	if err != nil {
		return group{}, err
	}
	for _, item := range items {
		if item.ID == id {
			return item, nil
		}
	}
	return group{}, sql.ErrNoRows
}

func (a *app) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var input groupInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := normalizeGroupInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	available, err := a.groupNameAvailable(input.Name, "")
	if err != nil {
		writeDBError(w, err)
		return
	}
	if !available {
		writeError(w, http.StatusConflict, "group name already exists")
		return
	}
	reservePool := boolPointerValue(input.ReservePoolEnabled, false)
	if err := a.validateReserveGroupChange("", reservePool); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	rpmDispatch := boolPointerValue(input.RPMDispatchEnabled, !(input.StreamHedgeEnabled || input.AdaptiveHedgeEnabled))
	strategyValue, err := a.resolveStrategyBinding(input.StrategyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := ""
	for attempt := 0; attempt < 5; attempt++ {
		id = "g_" + randomSecret(6)
		_, err = a.db.Exec(`INSERT INTO groups (id, name, description, rate_multiplier, daily_limit_usd, monthly_limit_usd, normal_request_mode, claude_code_identity_enabled, stream_hedge_enabled, adaptive_hedge_enabled, rpm_dispatch_enabled, mcp_tool_names_enabled, service_tier_passthrough_enabled, inference_geo_passthrough_enabled, speed_passthrough_enabled, anthropic_beta_passthrough_enabled, reject_anthropic_downgrade_enabled, reject_distillation_enabled, request_format_filter_enabled, quota_header_masking_enabled, cache_creation_detail_enabled, dateline_normalization_enabled, overload_cooldown_seconds, rate_limit_downweight_enabled, rate_limit_cooling_threshold, rate_limit_wait_seconds, rate_limit_stepped_cooldown_enabled, rate_limit_cooldown_step_seconds, rate_limit_downweight_stepped_cooldown_enabled, rate_limit_downweight_base_minutes, rate_limit_downweight_step_minutes, five_hour_release_stagger_enabled, five_hour_release_stagger_min_minutes, five_hour_release_stagger_max_minutes, capacity_queue_enabled, capacity_queue_timeout_seconds, strategy_required_enabled, strategy_id, reserve_pool_enabled, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, input.Name, input.Description, input.RateMultiplier, input.DailyLimitUSD, input.MonthlyLimitUSD, boolInt(input.NormalRequestMode), boolInt(boolPointerValue(input.ClaudeCodeIdentityEnabled, false)), boolInt(input.StreamHedgeEnabled), boolInt(input.AdaptiveHedgeEnabled), boolInt(rpmDispatch), boolInt(boolPointerValue(input.MCPToolNamesEnabled, false)), boolInt(boolPointerValue(input.ServiceTierPassthrough, false)), boolInt(boolPointerValue(input.InferenceGeoPassthrough, false)), boolInt(boolPointerValue(input.SpeedPassthrough, false)), boolInt(boolPointerValue(input.AnthropicBetaPassthrough, false)), boolInt(boolPointerValue(input.RejectAnthropicDowngrade, false)), boolInt(boolPointerValue(input.RejectDistillation, false)), boolInt(boolPointerValue(input.RequestFormatFilter, false)), boolInt(boolPointerValue(input.QuotaHeaderMasking, false)), boolInt(boolPointerValue(input.CacheCreationDetail, false)), boolInt(boolPointerValue(input.DatelineNormalization, true)), intPointerValue(input.OverloadCooldownSeconds, 10), boolInt(boolPointerValue(input.RateLimitDownweightEnabled, true)), intPointerValue(input.RateLimitCoolingThreshold, defaultRateLimitCoolingThreshold), intPointerValue(input.RateLimitWaitSeconds, defaultRateLimitCooldownSeconds), boolInt(boolPointerValue(input.RateLimitSteppedCooldown, false)), intPointerValue(input.RateLimitCooldownStep, defaultRateLimitCooldownStepSeconds), boolInt(boolPointerValue(input.RateLimitDownweightStepped, false)), intPointerValue(input.RateLimitDownweightBase, defaultRateLimitDownweightBaseMinutes), intPointerValue(input.RateLimitDownweightStep, defaultRateLimitDownweightStepMinutes), boolInt(boolPointerValue(input.FiveHourStaggerEnabled, true)), intPointerValue(input.FiveHourStaggerMin, defaultFiveHourReleaseStaggerMinMinutes), intPointerValue(input.FiveHourStaggerMax, defaultFiveHourReleaseStaggerMaxMinutes), boolInt(boolPointerValue(input.CapacityQueueEnabled, false)), intPointerValue(input.CapacityQueueTimeout, 30), boolInt(boolPointerValue(input.StrategyRequiredEnabled, false)), strategyValue, boolInt(reservePool), input.Status)
		if err == nil {
			break
		}
		if !strings.Contains(strings.ToLower(err.Error()), "unique") {
			writeDBError(w, err)
			return
		}
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	item, err := a.getGroup(id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}

func (a *app) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	id := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if !groupIDPattern.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	var input groupInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := normalizeGroupInput(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	available, err := a.groupNameAvailable(input.Name, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if !available {
		writeError(w, http.StatusConflict, "group name already exists")
		return
	}
	var currentReserve int
	if err := a.db.QueryRow(`SELECT reserve_pool_enabled FROM groups WHERE id = ?`, id).Scan(&currentReserve); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeDBError(w, err)
		return
	}
	nextReserve := currentReserve == 1
	if input.ReservePoolEnabled != nil {
		nextReserve = *input.ReservePoolEnabled
	}
	if err := a.validateReserveGroupChange(id, nextReserve); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	var rpmDispatch any
	if input.RPMDispatchEnabled != nil {
		rpmDispatch = boolInt(*input.RPMDispatchEnabled)
	} else if input.StreamHedgeEnabled || input.AdaptiveHedgeEnabled {
		// Older clients do not send the new field. Enabling a hedge mode still
		// selects that explicit multi-account policy instead of silently blocking it.
		rpmDispatch = 0
	}
	// Clients that predate this switch omit the field; keeping the stored value
	// prevents a stale cached page from turning the lane off on an unrelated save.
	var mcpToolNames any
	if input.MCPToolNamesEnabled != nil {
		mcpToolNames = boolInt(*input.MCPToolNamesEnabled)
	}
	var claudeCodeIdentity any
	if input.ClaudeCodeIdentityEnabled != nil {
		claudeCodeIdentity = boolInt(*input.ClaudeCodeIdentityEnabled)
	}
	var serviceTierPassthrough any
	if input.ServiceTierPassthrough != nil {
		serviceTierPassthrough = boolInt(*input.ServiceTierPassthrough)
	}
	var inferenceGeoPassthrough any
	if input.InferenceGeoPassthrough != nil {
		inferenceGeoPassthrough = boolInt(*input.InferenceGeoPassthrough)
	}
	var speedPassthrough any
	if input.SpeedPassthrough != nil {
		speedPassthrough = boolInt(*input.SpeedPassthrough)
	}
	var anthropicBetaPassthrough any
	if input.AnthropicBetaPassthrough != nil {
		anthropicBetaPassthrough = boolInt(*input.AnthropicBetaPassthrough)
	}
	var rejectAnthropicDowngrade any
	if input.RejectAnthropicDowngrade != nil {
		rejectAnthropicDowngrade = boolInt(*input.RejectAnthropicDowngrade)
	}
	var rejectDistillation any
	if input.RejectDistillation != nil {
		rejectDistillation = boolInt(*input.RejectDistillation)
	}
	var requestFormatFilter any
	if input.RequestFormatFilter != nil {
		requestFormatFilter = boolInt(*input.RequestFormatFilter)
	}
	var quotaHeaderMasking any
	if input.QuotaHeaderMasking != nil {
		quotaHeaderMasking = boolInt(*input.QuotaHeaderMasking)
	}
	var cacheCreationDetail any
	if input.CacheCreationDetail != nil {
		cacheCreationDetail = boolInt(*input.CacheCreationDetail)
	}
	var datelineNormalization any
	if input.DatelineNormalization != nil {
		datelineNormalization = boolInt(*input.DatelineNormalization)
	}
	var overloadCooldown any
	if input.OverloadCooldownSeconds != nil {
		overloadCooldown = *input.OverloadCooldownSeconds
	}
	var rateLimitDownweight any
	if input.RateLimitDownweightEnabled != nil {
		rateLimitDownweight = boolInt(*input.RateLimitDownweightEnabled)
	}
	var rateLimitCoolingThreshold any
	if input.RateLimitCoolingThreshold != nil {
		rateLimitCoolingThreshold = *input.RateLimitCoolingThreshold
	}
	var rateLimitWaitSeconds any
	if input.RateLimitWaitSeconds != nil {
		rateLimitWaitSeconds = *input.RateLimitWaitSeconds
	}
	var rateLimitSteppedCooldown any
	if input.RateLimitSteppedCooldown != nil {
		rateLimitSteppedCooldown = boolInt(*input.RateLimitSteppedCooldown)
	}
	var rateLimitCooldownStep any
	if input.RateLimitCooldownStep != nil {
		rateLimitCooldownStep = *input.RateLimitCooldownStep
	}
	var rateLimitDownweightStepped any
	if input.RateLimitDownweightStepped != nil {
		rateLimitDownweightStepped = boolInt(*input.RateLimitDownweightStepped)
	}
	var rateLimitDownweightBase any
	if input.RateLimitDownweightBase != nil {
		rateLimitDownweightBase = *input.RateLimitDownweightBase
	}
	var rateLimitDownweightStep any
	if input.RateLimitDownweightStep != nil {
		rateLimitDownweightStep = *input.RateLimitDownweightStep
	}
	var fiveHourStaggerEnabled any
	if input.FiveHourStaggerEnabled != nil {
		fiveHourStaggerEnabled = boolInt(*input.FiveHourStaggerEnabled)
	}
	var fiveHourStaggerMin any
	if input.FiveHourStaggerMin != nil {
		fiveHourStaggerMin = *input.FiveHourStaggerMin
	}
	var fiveHourStaggerMax any
	if input.FiveHourStaggerMax != nil {
		fiveHourStaggerMax = *input.FiveHourStaggerMax
	}
	var capacityQueueEnabled any
	if input.CapacityQueueEnabled != nil {
		capacityQueueEnabled = boolInt(*input.CapacityQueueEnabled)
	}
	var capacityQueueTimeout any
	if input.CapacityQueueTimeout != nil {
		capacityQueueTimeout = *input.CapacityQueueTimeout
	}
	var strategyRequired any
	if input.StrategyRequiredEnabled != nil {
		strategyRequired = boolInt(*input.StrategyRequiredEnabled)
	}
	var reservePool any
	if input.ReservePoolEnabled != nil {
		reservePool = boolInt(*input.ReservePoolEnabled)
	}
	strategyClear := input.StrategyID != nil && *input.StrategyID <= 0
	strategyValue, err := a.resolveStrategyBinding(input.StrategyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := a.db.Exec(`UPDATE groups SET name = ?, description = ?, rate_multiplier = ?, daily_limit_usd = ?, monthly_limit_usd = ?, normal_request_mode = ?, claude_code_identity_enabled = COALESCE(?, claude_code_identity_enabled), stream_hedge_enabled = ?, adaptive_hedge_enabled = ?, rpm_dispatch_enabled = COALESCE(?, rpm_dispatch_enabled), mcp_tool_names_enabled = COALESCE(?, mcp_tool_names_enabled), service_tier_passthrough_enabled = COALESCE(?, service_tier_passthrough_enabled), inference_geo_passthrough_enabled = COALESCE(?, inference_geo_passthrough_enabled), speed_passthrough_enabled = COALESCE(?, speed_passthrough_enabled), anthropic_beta_passthrough_enabled = COALESCE(?, anthropic_beta_passthrough_enabled), reject_anthropic_downgrade_enabled = COALESCE(?, reject_anthropic_downgrade_enabled), reject_distillation_enabled = COALESCE(?, reject_distillation_enabled), request_format_filter_enabled = COALESCE(?, request_format_filter_enabled), quota_header_masking_enabled = COALESCE(?, quota_header_masking_enabled), cache_creation_detail_enabled = COALESCE(?, cache_creation_detail_enabled), dateline_normalization_enabled = COALESCE(?, dateline_normalization_enabled), overload_cooldown_seconds = COALESCE(?, overload_cooldown_seconds), rate_limit_downweight_enabled = COALESCE(?, rate_limit_downweight_enabled), rate_limit_cooling_threshold = COALESCE(?, rate_limit_cooling_threshold), rate_limit_wait_seconds = COALESCE(?, rate_limit_wait_seconds), rate_limit_stepped_cooldown_enabled = COALESCE(?, rate_limit_stepped_cooldown_enabled), rate_limit_cooldown_step_seconds = COALESCE(?, rate_limit_cooldown_step_seconds), rate_limit_downweight_stepped_cooldown_enabled = COALESCE(?, rate_limit_downweight_stepped_cooldown_enabled), rate_limit_downweight_base_minutes = COALESCE(?, rate_limit_downweight_base_minutes), rate_limit_downweight_step_minutes = COALESCE(?, rate_limit_downweight_step_minutes), five_hour_release_stagger_enabled = COALESCE(?, five_hour_release_stagger_enabled), five_hour_release_stagger_min_minutes = COALESCE(?, five_hour_release_stagger_min_minutes), five_hour_release_stagger_max_minutes = COALESCE(?, five_hour_release_stagger_max_minutes), capacity_queue_enabled = COALESCE(?, capacity_queue_enabled), capacity_queue_timeout_seconds = COALESCE(?, capacity_queue_timeout_seconds), strategy_required_enabled = COALESCE(?, strategy_required_enabled), strategy_id = CASE WHEN ? = 1 THEN NULL ELSE COALESCE(?, strategy_id) END, reserve_pool_enabled = COALESCE(?, reserve_pool_enabled), status = ?, updated_at = `+nowSQL+` WHERE id = ?`, input.Name, input.Description, input.RateMultiplier, input.DailyLimitUSD, input.MonthlyLimitUSD, boolInt(input.NormalRequestMode), claudeCodeIdentity, boolInt(input.StreamHedgeEnabled), boolInt(input.AdaptiveHedgeEnabled), rpmDispatch, mcpToolNames, serviceTierPassthrough, inferenceGeoPassthrough, speedPassthrough, anthropicBetaPassthrough, rejectAnthropicDowngrade, rejectDistillation, requestFormatFilter, quotaHeaderMasking, cacheCreationDetail, datelineNormalization, overloadCooldown, rateLimitDownweight, rateLimitCoolingThreshold, rateLimitWaitSeconds, rateLimitSteppedCooldown, rateLimitCooldownStep, rateLimitDownweightStepped, rateLimitDownweightBase, rateLimitDownweightStep, fiveHourStaggerEnabled, fiveHourStaggerMin, fiveHourStaggerMax, capacityQueueEnabled, capacityQueueTimeout, strategyRequired, boolInt(strategyClear), strategyValue, reservePool, input.Status, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	if input.StrategyShares != nil {
		tx, err := a.db.Begin()
		if err != nil {
			writeDBError(w, err)
			return
		}
		defer tx.Rollback()
		if err := replaceGroupStrategyShares(tx, id, *input.StrategyShares); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := tx.Commit(); err != nil {
			writeDBError(w, err)
			return
		}
	}
	item, err := a.getGroup(id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
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
	if !groupIDPattern.MatchString(input.ActiveGroupID) {
		return errors.New("invalid active group")
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
	if err := a.validateDispatchGroupID(input.ActiveGroupID); err != nil {
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
	if err := a.validateDispatchGroupID(input.ActiveGroupID); err != nil {
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

const accountSelectBase = `SELECT a.id, a.name, a.platform, a.auth_type, a.credential_hint, a.source_sk_hint, a.credentials_json != '{}',
	a.status, a.schedulable, a.concurrency, a.priority, a.rate_multiplier, a.notes, a.error_message,
	a.last_used_at, a.expires_at, a.rate_limit_reset_at, a.rate_limit_window, a.rate_limit_reason, a.consecutive_429, a.last_429_at, a.rate_limit_downweight_until, a.quota_refreshed_at, a.created_at, a.updated_at, a.credentials_json, a.extra_json,
	a.proxy_pool_id, COALESCE(pp.name, ''), a.proxy_id, COALESCE(px.name, archived_px.name, ''),
	CASE WHEN COALESCE(px.id, archived_px.id) IS NULL THEN '' ELSE COALESCE(px.protocol, archived_px.protocol) || '://' || COALESCE(px.host, archived_px.host) || ':' || COALESCE(px.port, archived_px.port) END,
	COALESCE(NULLIF(px.exit_ip, ''), NULLIF(archived_px.exit_ip, ''), px.host, archived_px.host, ''),
	a.auto_proxy, a.base_rpm, a.rpm_strategy, a.rpm_sticky_buffer, a.user_msg_queue_mode, a.strategy_id,
	a.auth_status, a.auth_error, a.auth_checked_at, a.token_expires_at,
	a.quota_5h_utilization, a.quota_5h_reset_at, a.quota_5h_threshold_enabled, a.quota_5h_threshold_percent,
	a.quota_7d_utilization, a.quota_7d_reset_at, a.quota_7d_threshold_enabled, a.quota_7d_threshold_percent, a.quota_sampled_at,
	a.subscription_type, a.rate_limit_tier, a.account_price, a.onboarded_at, a.reauthorized_at, a.reauthorization_count, a.invalidated_at, a.archived_at, a.survival_seconds_total, COALESCE(px.status, archived_px.status, ''), `

const accountUsageSummaryFields = `COALESCE(aut.request_count, 0), COALESCE(aut.input_tokens, 0),
	COALESCE(aut.output_tokens, 0), COALESCE(aut.billed_cost, 0), COALESCE(aut.actual_cost, 0)`

const accountSelectFrom = `
	FROM accounts a LEFT JOIN proxy_pools pp ON pp.id = a.proxy_pool_id LEFT JOIN proxies px ON px.id = a.proxy_id LEFT JOIN proxies archived_px ON archived_px.id = a.archived_proxy_id
	LEFT JOIN account_usage_totals aut ON aut.account_id = a.id`

const accountSelect = accountSelectBase + accountUsageSummaryFields + accountSelectFrom
const accountListSelect = accountSelect

func scanAccount(row scanner, reveal bool) (account, error) {
	var item account
	var schedulable, autoProxy, quota5HThresholdEnabled, quota7DThresholdEnabled int
	var proxyPoolID, proxyID, strategyID sql.NullInt64
	var lastUsed, expires, rateLimit, last429, downweightUntil, quotaRefreshed, authChecked, tokenExpires, quota5HReset, quota7DReset, quotaSampled, onboarded, reauthorized, invalidated, archived sql.NullString
	var credentialsJSON, extraJSON string
	err := row.Scan(&item.ID, &item.Name, &item.Platform, &item.AuthType, &item.CredentialHint, &item.SourceSKHint, &item.HasCredentials, &item.Status, &schedulable, &item.Concurrency, &item.Priority, &item.RateMultiplier, &item.Notes, &item.ErrorMessage, &lastUsed, &expires, &rateLimit, &item.RateLimitWindow, &item.RateLimitReason, &item.Consecutive429, &last429, &downweightUntil, &quotaRefreshed, &item.CreatedAt, &item.UpdatedAt, &credentialsJSON, &extraJSON, &proxyPoolID, &item.ProxyPoolName, &proxyID, &item.ProxyName, &item.ProxyHint, &item.ProxyIP, &autoProxy, &item.BaseRPM, &item.RPMStrategy, &item.RPMStickyBuffer, &item.UserMsgQueueMode, &strategyID, &item.AuthStatus, &item.AuthError, &authChecked, &tokenExpires, &item.Quota5H, &quota5HReset, &quota5HThresholdEnabled, &item.Quota5HThresholdPercent, &item.Quota7D, &quota7DReset, &quota7DThresholdEnabled, &item.Quota7DThresholdPercent, &quotaSampled, &item.SubscriptionType, &item.RateLimitTier, &item.AccountPrice, &onboarded, &reauthorized, &item.ReauthorizationCount, &invalidated, &archived, &item.SurvivalTotal, &item.ProxyStatus, &item.RequestCount, &item.InputTokens, &item.OutputTokens, &item.TotalBilledCost, &item.TotalActualCost)
	if err != nil {
		return item, err
	}
	item.Schedulable = schedulable == 1
	item.AutoProxy = autoProxy == 1
	item.Quota5HThresholdEnabled = quota5HThresholdEnabled == 1
	item.Quota7DThresholdEnabled = quota7DThresholdEnabled == 1
	item.ProxyPoolID = nullIntPointer(proxyPoolID)
	item.ProxyID = nullIntPointer(proxyID)
	item.StrategyID = nullIntPointer(strategyID)
	item.LastUsedAt = nullText(lastUsed)
	item.ExpiresAt = nullText(expires)
	item.RateLimitResetAt = nullText(rateLimit)
	item.Last429At = nullText(last429)
	item.DownweightUntil = nullText(downweightUntil)
	item.QuotaRefreshedAt = nullText(quotaRefreshed)
	item.AuthCheckedAt = nullText(authChecked)
	item.TokenExpiresAt = nullText(tokenExpires)
	item.Quota5HResetAt = nullText(quota5HReset)
	item.Quota7DResetAt = nullText(quota7DReset)
	item.QuotaSampledAt = nullText(quotaSampled)
	item.OnboardedAt = nullText(onboarded)
	item.ReauthorizedAt = nullText(reauthorized)
	item.InvalidatedAt = nullText(invalidated)
	item.ArchivedAt = nullText(archived)
	item.SurvivalSeconds = accountSurvivalSeconds(item.OnboardedAt, item.InvalidatedAt, item.SurvivalTotal)
	item.DispatchStatus = accountDispatchState(item)
	item.LimitWindow = accountLimitWindow(item)
	item.Extra = decodeObject(extraJSON)
	if reveal {
		item.Credentials = decodeObject(credentialsJSON)
	}
	return item, nil
}

func accountQuotaFilterConditions(r *http.Request, alias string) ([]string, []any, error) {
	conditions := []string{}
	args := []any{}
	if raw := strings.TrimSpace(r.URL.Query().Get("quota_5h_utilization")); raw != "" {
		utilization, err := strconv.ParseFloat(raw, 64)
		if err != nil || utilization < 0 || utilization > 100 {
			return nil, nil, errors.New("quota_5h_utilization must be between 0 and 100")
		}
		conditions = append(conditions, alias+".quota_5h_utilization = ?")
		args = append(args, utilization)
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("min_5h_utilization")); raw != "" {
		minimum, err := strconv.ParseFloat(raw, 64)
		if err != nil || minimum < 0 || minimum > 100 {
			return nil, nil, errors.New("min_5h_utilization must be between 0 and 100")
		}
		conditions = append(conditions, alias+".quota_5h_utilization >= ?")
		args = append(args, minimum)
	}
	switch status := strings.TrimSpace(r.URL.Query().Get("quota_5h_threshold")); status {
	case "":
	case "enabled":
		conditions = append(conditions, alias+".quota_5h_threshold_enabled = 1")
	case "disabled":
		conditions = append(conditions, alias+".quota_5h_threshold_enabled = 0")
	case "reached":
		conditions = append(conditions, alias+".quota_5h_threshold_enabled = 1 AND "+alias+".quota_5h_utilization >= "+alias+".quota_5h_threshold_percent")
	default:
		return nil, nil, errors.New("quota_5h_threshold must be enabled, disabled, or reached")
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("quota_7d_utilization")); raw != "" {
		utilization, err := strconv.ParseFloat(raw, 64)
		if err != nil || utilization < 0 || utilization > 100 {
			return nil, nil, errors.New("quota_7d_utilization must be between 0 and 100")
		}
		conditions = append(conditions, alias+".quota_7d_utilization = ?")
		args = append(args, utilization)
	}
	switch status := strings.TrimSpace(r.URL.Query().Get("quota_7d_threshold")); status {
	case "":
	case "enabled":
		conditions = append(conditions, alias+".quota_7d_threshold_enabled = 1")
	case "disabled":
		conditions = append(conditions, alias+".quota_7d_threshold_enabled = 0")
	case "reached":
		conditions = append(conditions, alias+".quota_7d_threshold_enabled = 1 AND "+alias+".quota_7d_utilization >= "+alias+".quota_7d_threshold_percent")
	default:
		return nil, nil, errors.New("quota_7d_threshold must be enabled, disabled, or reached")
	}
	return conditions, args, nil
}

func (a *app) handleAccounts(w http.ResponseWriter, r *http.Request) {
	where := ` WHERE a.deleted_at IS NULL`
	args := []any{}
	archived := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("archived")))
	switch archived {
	case "", "exclude":
		where += ` AND a.archived_at IS NULL`
	case "only":
		where += ` AND a.archived_at IS NOT NULL`
	case "all":
	default:
		writeError(w, http.StatusBadRequest, "archived must be exclude, only, or all")
		return
	}
	user := currentUser(r)
	restrictedView := user.Role != "admin"
	if user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "a")
		where += ` AND ` + condition
		args = append(args, scopeArgs...)
	}
	if restrictedView {
		where += ` AND ` + accountStatePredicate("a", "normal")
	}
	if groupID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))); groupIDPattern.MatchString(groupID) {
		where += ` AND EXISTS (SELECT 1 FROM account_groups ag WHERE ag.account_id = a.id AND ag.group_id = ?)`
		args = append(args, groupID)
	}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		where += ` AND (CAST(a.id AS CHAR) LIKE ? OR a.name LIKE ? OR a.notes LIKE ? OR a.credential_hint LIKE ? OR a.source_sk_hint LIKE ?
			OR COALESCE(px.name, archived_px.name, '') LIKE ?
			OR COALESCE(px.host, archived_px.host, '') LIKE ?
			OR COALESCE(px.exit_ip, archived_px.exit_ip, '') LIKE ?)`
		term := "%" + search + "%"
		args = append(args, term, term, term, term, term, term, term, term)
	}
	quotaConditions, quotaArgs, err := accountQuotaFilterConditions(r, "a")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	for _, condition := range quotaConditions {
		where += ` AND (` + condition + `)`
	}
	args = append(args, quotaArgs...)
	baseWhere := where
	baseArgs := append([]any(nil), args...)
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" && !restrictedView {
		where += ` AND ` + accountStatePredicate("a", status)
	}

	orderBy := `a.priority ASC, a.id ASC`
	if archived == "only" {
		orderBy = `a.archived_at DESC, a.id DESC`
	}
	sortColumns := map[string]string{
		"id":                    "a.id",
		"account_price":         "a.account_price",
		"total_billed_cost":     "COALESCE(aut.billed_cost, 0)",
		"onboarded_at":          "a.onboarded_at",
		"reauthorized_at":       "a.reauthorized_at",
		"reauthorization_count": "a.reauthorization_count",
		"request_count":         "COALESCE(aut.request_count, 0)",
		"invalidated_at":        "a.invalidated_at",
		"archived_at":           "a.archived_at",
		"last_used_at":          "a.last_used_at",
		"updated_at":            "a.updated_at",
		"error_time":            "COALESCE(a.invalidated_at, a.updated_at)",
	}
	if column := sortColumns[strings.TrimSpace(r.URL.Query().Get("sort"))]; column != "" {
		direction := "ASC"
		if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("order")), "desc") {
			direction = "DESC"
		}
		orderBy = column + " " + direction + `, a.id ` + direction
	}
	query := accountListSelect + where + ` ORDER BY ` + orderBy
	paginated := r.URL.Query().Get("paginated") == "1" || r.URL.Query().Has("page")
	page, pageSize, offset := paginationFromRequest(r, 20, 100)
	if paginated {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, pageSize, offset)
	}
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
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeDBError(w, err)
		return
	}
	groupIDs, err := a.accountGroupIDsBulk(items)
	if err != nil {
		writeDBError(w, err)
		return
	}
	for index := range items {
		item := &items[index]
		item.GroupIDs = groupIDs[item.ID]
		if user.Role == "user" {
			visibleGroups := make([]string, 0, len(item.GroupIDs))
			for _, groupID := range item.GroupIDs {
				if userCanAccessGroup(user, groupID) {
					visibleGroups = append(visibleGroups, groupID)
				}
			}
			item.GroupIDs = visibleGroups
		}
	}
	responseItems := any(items)
	if restrictedView {
		projected := make([]map[string]any, 0, len(items))
		for _, item := range items {
			projected = append(projected, accountForRestrictedView(item, user.AccountView))
		}
		responseItems = projected
	}
	if !paginated {
		writeJSON(w, http.StatusOK, responseItems)
		return
	}
	countArgs := args[:len(args)-2]
	var total, summaryRequests, summarySurvival int64
	var summaryBilled float64
	if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(aut.request_count), 0), COALESCE(SUM(aut.billed_cost), 0), COALESCE(SUM(a.survival_seconds_total), 0)`+accountSelectFrom+where, countArgs...).Scan(&total, &summaryRequests, &summaryBilled, &summarySurvival); err != nil {
		writeDBError(w, err)
		return
	}
	statusCounts := map[string]int64{}
	var allCount, normalCount, unavailableCount, fiveHourCount, sevenDayCount, coolingCount, errorCount int64
	statusQuery := `SELECT COUNT(*),
		COALESCE(SUM(CASE WHEN ` + accountStatePredicate("a", "normal") + ` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + accountStatePredicate("a", "unavailable") + ` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + accountStatePredicate("a", "limited_5h") + ` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + accountStatePredicate("a", "limited_7d") + ` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + accountStatePredicate("a", "cooling_429") + ` THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN ` + accountStatePredicate("a", "error") + ` THEN 1 ELSE 0 END), 0)` + accountSelectFrom + baseWhere
	if err := a.db.QueryRow(statusQuery, baseArgs...).Scan(&allCount, &normalCount, &unavailableCount, &fiveHourCount, &sevenDayCount, &coolingCount, &errorCount); err != nil {
		writeDBError(w, err)
		return
	}
	statusCounts["all"] = allCount
	statusCounts["normal"] = normalCount
	statusCounts["unavailable"] = unavailableCount
	statusCounts["limited_5h"] = fiveHourCount
	statusCounts["limited_7d"] = sevenDayCount
	statusCounts["cooling_429"] = coolingCount
	statusCounts["error"] = errorCount
	summary := map[string]any{"total": total, "requests": summaryRequests, "billed_cost": summaryBilled, "survival_seconds": summarySurvival}
	if restrictedView {
		summary = map[string]any{"total": total}
		if accountViewHas(user.AccountView.Columns, "requests") {
			summary["requests"] = summaryRequests
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": responseItems, "total": total, "page": page, "page_size": pageSize, "total_pages": totalPages(total, pageSize), "status_counts": statusCounts,
		"summary": summary,
	})
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
	if err := a.validateAccountGroupIDs(input.GroupIDs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.ProxyText) != "" {
		input.ProxyID, err = a.ensureProxyInPool(input.ProxyPoolID, input.ProxyText)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		input.AutoProxy = false
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
			input.Status = "active"
			*input.Schedulable = true
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
	accountSourceSKHint := sourceSKHint(input.SessionKey)
	if accountSourceSKHint == "" {
		accountSourceSKHint = sourceSKHintFromCredentials(credentialsJSON)
	}
	strategyValue, err := a.resolveStrategyBinding(input.StrategyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := tx.Exec(`INSERT INTO accounts (name, platform, auth_type, credentials_json, credential_hint, source_sk_hint, extra_json, status, schedulable, concurrency, priority, rate_multiplier, notes, error_message, expires_at, rate_limit_reset_at, proxy_pool_id, auto_proxy, base_rpm, rpm_strategy, rpm_sticky_buffer, user_msg_queue_mode, strategy_id, account_price, quota_5h_threshold_enabled, quota_5h_threshold_percent, quota_7d_threshold_enabled, quota_7d_threshold_percent) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, input.Name, input.Platform, input.AuthType, credentialsJSON, credentialHint(credentialsJSON), accountSourceSKHint, extraJSON, input.Status, boolInt(*input.Schedulable), input.Concurrency, input.Priority, input.RateMultiplier, input.Notes, input.ErrorMessage, input.ExpiresAt, input.RateLimitResetAt, input.ProxyPoolID, boolInt(input.AutoProxy), input.BaseRPM, input.RPMStrategy, input.RPMStickyBuffer, input.UserMsgQueueMode, strategyValue, input.AccountPrice, boolInt(boolPointerValue(input.Quota5HThresholdEnabled, false)), intPointerValue(input.Quota5HThresholdPercent, 80), boolInt(boolPointerValue(input.Quota7DThresholdEnabled, false)), intPointerValue(input.Quota7DThresholdPercent, 80))
	if err != nil {
		writeDBError(w, err)
		return
	}
	id, _ := result.LastInsertId()
	if sessionAuthorized {
		credentials := decodeObject(credentialsJSON)
		subscription := subscriptionTypeFromCredentials(credentials)
		rateLimitTier := rateLimitTierFromCredentials(credentials)
		if _, err := tx.Exec(`UPDATE accounts SET auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, token_expires_at = ?, subscription_type = ?, rate_limit_tier = ?, onboarded_at = `+nowSQL+`, invalidated_at = NULL WHERE id = ?`, tokenExpiresAt, subscription, rateLimitTier, id); err != nil {
			writeDBError(w, err)
			return
		}
	} else if deferredAuthorization {
		if _, err := tx.Exec(`UPDATE accounts SET auth_status = 'reauth_required', auth_error = '等待授权' WHERE id = ?`, id); err != nil {
			writeDBError(w, err)
			return
		}
	} else if credentialsJSON != "{}" {
		credentials := decodeObject(credentialsJSON)
		subscription := subscriptionTypeFromCredentials(credentials)
		rateLimitTier := rateLimitTierFromCredentials(credentials)
		if _, err := tx.Exec(`UPDATE accounts SET auth_status = 'valid', auth_checked_at = `+nowSQL+`, subscription_type = ?, rate_limit_tier = ?, onboarded_at = `+nowSQL+`, invalidated_at = NULL WHERE id = ?`, subscription, rateLimitTier, id); err != nil {
			writeDBError(w, err)
			return
		}
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
	if assignedProxy != nil {
		if err := recordProxyAssignment(tx, *assignedProxy, id); err != nil {
			writeDBError(w, err)
			return
		}
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
	leaseOwner, err := a.acquireAccountTokenLease(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	defer a.releaseAccountTokenLease(id, leaseOwner)
	var existingCredentials, existingExtra, existingSourceSKHint string
	var previousProxyID sql.NullInt64
	var previousOnboarded, previousInvalidated sql.NullString
	if err := a.db.QueryRow(`SELECT credentials_json, extra_json, source_sk_hint, proxy_id, onboarded_at, invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`, id).Scan(&existingCredentials, &existingExtra, &existingSourceSKHint, &previousProxyID, &previousOnboarded, &previousInvalidated); err != nil {
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
	if err := a.validateAccountGroupIDs(input.GroupIDs); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(input.ProxyText) != "" {
		input.ProxyID, err = a.ensureProxyInPool(input.ProxyPoolID, input.ProxyText)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		input.AutoProxy = false
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
	accountSourceSKHint := existingSourceSKHint
	if next := sourceSKHint(input.SessionKey); next != "" {
		accountSourceSKHint = next
	} else if next := sourceSKHintFromCredentials(credentialsJSON); next != "" {
		accountSourceSKHint = next
	}
	strategyClear := input.StrategyID != nil && *input.StrategyID <= 0
	strategyValue, err := a.resolveStrategyBinding(input.StrategyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var quota5HThresholdEnabled, quota5HThresholdPercent, quota7DThresholdEnabled, quota7DThresholdPercent any
	if input.Quota5HThresholdEnabled != nil {
		quota5HThresholdEnabled = boolInt(*input.Quota5HThresholdEnabled)
	}
	if input.Quota5HThresholdPercent != nil {
		quota5HThresholdPercent = *input.Quota5HThresholdPercent
	}
	if input.Quota7DThresholdEnabled != nil {
		quota7DThresholdEnabled = boolInt(*input.Quota7DThresholdEnabled)
	}
	if input.Quota7DThresholdPercent != nil {
		quota7DThresholdPercent = *input.Quota7DThresholdPercent
	}
	result, err := tx.Exec(`UPDATE accounts SET name = ?, platform = ?, auth_type = ?, credentials_json = ?, credential_hint = ?, source_sk_hint = ?, extra_json = ?, status = ?, schedulable = ?, concurrency = ?, priority = ?, rate_multiplier = ?, notes = ?, error_message = ?, expires_at = NULLIF(?, ''), rate_limit_reset_at = NULLIF(?, ''),
		rate_limit_window = CASE WHEN NULLIF(?, '') IS NULL THEN '' ELSE rate_limit_window END,
		rate_limit_reason = CASE WHEN NULLIF(?, '') IS NULL THEN '' ELSE rate_limit_reason END,
		consecutive_429 = CASE WHEN NULLIF(?, '') IS NULL THEN 0 ELSE consecutive_429 END,
		last_429_at = CASE WHEN NULLIF(?, '') IS NULL THEN NULL ELSE last_429_at END,
		proxy_pool_id = ?, proxy_id = ?, auto_proxy = ?, base_rpm = ?, rpm_strategy = ?, rpm_sticky_buffer = ?, user_msg_queue_mode = ?, strategy_id = CASE WHEN ? = 1 THEN NULL ELSE COALESCE(?, strategy_id) END, account_price = ?,
		quota_5h_threshold_enabled = COALESCE(?, quota_5h_threshold_enabled), quota_5h_threshold_percent = COALESCE(?, quota_5h_threshold_percent), quota_7d_threshold_enabled = COALESCE(?, quota_7d_threshold_enabled), quota_7d_threshold_percent = COALESCE(?, quota_7d_threshold_percent), updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL
		AND EXISTS (SELECT 1 FROM account_token_leases lease WHERE lease.account_id = accounts.id AND lease.owner = ? AND lease.expires_at > CAST(strftime('%s','now') AS INTEGER))`, input.Name, input.Platform, input.AuthType, credentialsJSON, credentialHint(credentialsJSON), accountSourceSKHint, extraJSON, input.Status, boolInt(*input.Schedulable), input.Concurrency, input.Priority, input.RateMultiplier, input.Notes, input.ErrorMessage, input.ExpiresAt, input.RateLimitResetAt,
		// Clearing the cooldown field is the administrator's manual recovery, so
		// the automatic 429 bookkeeping has to go with it. Otherwise the account
		// keeps its strikes and the next single 429 re-parks it.
		input.RateLimitResetAt, input.RateLimitResetAt, input.RateLimitResetAt, input.RateLimitResetAt,
		input.ProxyPoolID, assignedProxy, boolInt(input.AutoProxy), input.BaseRPM, input.RPMStrategy, input.RPMStickyBuffer, input.UserMsgQueueMode, boolInt(strategyClear), strategyValue, input.AccountPrice, quota5HThresholdEnabled, quota5HThresholdPercent, quota7DThresholdEnabled, quota7DThresholdPercent, id, leaseOwner)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if updated, _ := result.RowsAffected(); updated == 0 {
		writeError(w, http.StatusConflict, "account token lease expired; retry the update")
		return
	}
	if assignedProxy != nil && (!previousProxyID.Valid || previousProxyID.Int64 != *assignedProxy) {
		if err := recordProxyAssignment(tx, *assignedProxy, id); err != nil {
			writeDBError(w, err)
			return
		}
	}
	credentialsProvided := len(input.Credentials) > 0 && string(input.Credentials) != "null"
	if credentialsProvided && credentialsJSON != "{}" {
		credentials := decodeObject(credentialsJSON)
		subscription := subscriptionTypeFromCredentials(credentials)
		rateLimitTier := rateLimitTierFromCredentials(credentials)
		if _, err := tx.Exec(`UPDATE accounts SET auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, subscription_type = ?, rate_limit_tier = ?, onboarded_at = CASE WHEN onboarded_at IS NULL OR invalidated_at IS NOT NULL THEN `+nowSQL+` ELSE onboarded_at END, invalidated_at = NULL, error_message = '', status = CASE WHEN status = 'error' THEN 'active' ELSE status END WHERE id = ?`, subscription, rateLimitTier, id); err != nil {
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
			if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type, reason) VALUES (?, 'invalidated', '管理员手动置为待重新授权')`, id); err != nil {
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
	if err := a.enforceStoredAccountQuotaThresholds(id, rateLimitPolicy{}); err != nil {
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
	if input.Quota5HThresholdPercent != nil && (*input.Quota5HThresholdPercent < 1 || *input.Quota5HThresholdPercent > 100) {
		return "", "", errors.New("5h quota threshold must be between 1 and 100")
	}
	if input.Quota7DThresholdPercent != nil && (*input.Quota7DThresholdPercent < 1 || *input.Quota7DThresholdPercent > 100) {
		return "", "", errors.New("7d quota threshold must be between 1 and 100")
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
	if input.RPMStrategy != "tiered" && input.RPMStrategy != "sticky_exempt" && input.RPMStrategy != "fixed" {
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
	if err := validateSmoothColdStartExtra(extra); err != nil {
		return "", "", err
	}
	extra["base_rpm"] = input.BaseRPM
	extra["rpm_strategy"] = input.RPMStrategy
	extra["rpm_sticky_buffer"] = input.RPMStickyBuffer
	extra["user_msg_queue_mode"] = input.UserMsgQueueMode
	normalizedExtra, _ := json.Marshal(extra)
	return credentialsJSON, string(normalizedExtra), nil
}

func validateSmoothColdStartExtra(extra map[string]any) error {
	if value, exists := extra["smooth_cold_start_enabled"]; exists {
		if _, ok := value.(bool); !ok {
			return errors.New("smooth cold start enabled must be a boolean")
		}
	}
	if value, exists := extra["smooth_cold_start_rpm"]; exists {
		rpm := intFromJSON(value)
		if rpm < 1 || rpm > 10000 {
			return errors.New("smooth cold start RPM must be between 1 and 10000")
		}
	}
	if value, exists := extra["smooth_cold_start_tpm"]; exists {
		if intFromJSON(value) < 1 {
			return errors.New("smooth cold start TPM must be greater than zero")
		}
	}
	return nil
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
		if groupIDPattern.MatchString(id) && !seen[id] {
			seen[id] = true
			groups = append(groups, id)
		}
	}
	return groups
}

func (a *app) validateGroupIDs(groupIDs []string, activeOnly bool) error {
	if len(groupIDs) == 0 {
		return errors.New("select at least one group")
	}
	for _, groupID := range groupIDs {
		if !groupIDPattern.MatchString(groupID) {
			return fmt.Errorf("invalid group: %s", groupID)
		}
		query := `SELECT COUNT(*) FROM groups WHERE id = ?`
		if activeOnly {
			query += ` AND status = 'active'`
		}
		var count int
		if err := a.db.QueryRow(query, groupID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return fmt.Errorf("group not found or unavailable: %s", groupID)
		}
	}
	return nil
}

func (a *app) validateDispatchGroupID(groupID string) error {
	if err := a.validateGroupIDs([]string{groupID}, true); err != nil {
		return err
	}
	var reserve int
	if err := a.db.QueryRow(`SELECT reserve_pool_enabled FROM groups WHERE id = ?`, groupID).Scan(&reserve); err != nil {
		return err
	}
	if reserve == 1 {
		return errors.New("reserve account pool cannot receive API traffic")
	}
	return nil
}

func (a *app) validateAccountGroupIDs(groupIDs []string) error {
	if err := a.validateGroupIDs(groupIDs, false); err != nil {
		return err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(groupIDs)), ",")
	args := make([]any, len(groupIDs))
	for index, groupID := range groupIDs {
		args[index] = groupID
	}
	var reserveCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM groups WHERE reserve_pool_enabled = 1 AND id IN (`+placeholders+`)`, args...).Scan(&reserveCount); err != nil {
		return err
	}
	if reserveCount > 0 && len(groupIDs) != 1 {
		return errors.New("reserve account pool must be the account's only group")
	}
	return nil
}

func setAccountGroups(tx *databaseTx, accountID int64, groups []string, priority int) error {
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

func (a *app) accountGroupIDsBulk(accounts []account) (map[int64][]string, error) {
	result := make(map[int64][]string, len(accounts))
	if len(accounts) == 0 {
		return result, nil
	}
	const batchSize = 500
	for start := 0; start < len(accounts); start += batchSize {
		end := min(start+batchSize, len(accounts))
		args := make([]any, 0, end-start)
		for _, item := range accounts[start:end] {
			args = append(args, item.ID)
			result[item.ID] = []string{}
		}
		rows, err := a.db.Query(`SELECT account_id, group_id FROM account_groups WHERE account_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`) ORDER BY account_id, group_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var accountID int64
			var groupID string
			if err := rows.Scan(&accountID, &groupID); err != nil {
				rows.Close()
				return nil, err
			}
			result[accountID] = append(result[accountID], groupID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (a *app) loadAccountUsageStats(accounts []account) error {
	if len(accounts) == 0 {
		return nil
	}
	byID := make(map[int64]*account, len(accounts))
	for index := range accounts {
		item := &accounts[index]
		byID[item.ID] = item
	}
	const batchSize = 500
	for start := 0; start < len(accounts); start += batchSize {
		end := min(start+batchSize, len(accounts))
		args := make([]any, 0, end-start)
		for _, item := range accounts[start:end] {
			args = append(args, item.ID)
		}
		rows, err := a.db.Query(`SELECT account_id, COUNT(*),
			COALESCE(SUM(input_tokens), 0),
			COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(billed_cost), 0),
			COALESCE(SUM(actual_cost), 0)
			FROM usage_logs
			WHERE account_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")+`)
			GROUP BY account_id`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var accountID int64
			var requestCount, inputTokens, outputTokens int64
			var billedCost, actualCost float64
			if err := rows.Scan(&accountID, &requestCount, &inputTokens, &outputTokens, &billedCost, &actualCost); err != nil {
				rows.Close()
				return err
			}
			if item := byID[accountID]; item != nil {
				item.RequestCount = requestCount
				item.InputTokens = inputTokens
				item.OutputTokens = outputTokens
				item.TotalBilledCost = billedCost
				item.TotalActualCost = actualCost
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
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

func (a *app) handleAccountBatchSchedule(w http.ResponseWriter, r *http.Request) {
	var input accountBatchScheduleInput
	if !decodeJSON(w, r, &input) {
		return
	}
	ids := uniquePositiveIDs(input.IDs, 501)
	if len(ids) == 0 || len(ids) > 500 {
		writeError(w, http.StatusBadRequest, "select between 1 and 500 accounts")
		return
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	var matched int64
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE deleted_at IS NULL AND archived_at IS NULL AND id IN (`+placeholders+`)`, args...).Scan(&matched); err != nil {
		writeDBError(w, err)
		return
	}
	if matched == 0 {
		writeError(w, http.StatusNotFound, "no selected accounts were found")
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()
	var result sql.Result
	if input.Schedulable {
		result, err = tx.Exec(`UPDATE accounts SET status = 'active', schedulable = 1,
			rate_limit_reset_at = NULL, rate_limit_window = '', rate_limit_reason = '', consecutive_429 = 0, last_429_at = NULL,
			rate_limit_downweight_until = NULL, quota_refreshed_at = `+nowSQL+`,
			error_message = '', updated_at = `+nowSQL+`
			WHERE deleted_at IS NULL AND archived_at IS NULL AND id IN (`+placeholders+`) AND auth_status = 'valid' AND proxy_id IS NOT NULL
			AND EXISTS (SELECT 1 FROM proxies p WHERE p.id = accounts.proxy_id AND p.status = 'active' AND p.deleted_at IS NULL)`, args...)
	} else {
		result, err = tx.Exec(`UPDATE accounts SET schedulable = 0, updated_at = `+nowSQL+` WHERE deleted_at IS NULL AND archived_at IS NULL AND id IN (`+placeholders+`)`, args...)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	if input.Schedulable {
		if _, err := tx.Exec(`DELETE FROM account_rpm_thresholds WHERE account_id IN (`+placeholders+`)`, args...); err != nil {
			writeDBError(w, err)
			return
		}
	}
	updated, err := result.RowsAffected()
	if err != nil {
		writeDBError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int64{
		"matched": matched,
		"updated": updated,
		"skipped": matched - updated,
	})
}

func (a *app) handleAccountBatchUpdate(w http.ResponseWriter, r *http.Request) {
	var input accountBatchUpdateInput
	if !decodeJSON(w, r, &input) {
		return
	}
	ids := uniquePositiveIDs(input.IDs, 501)
	if len(ids) == 0 || len(ids) > 500 {
		writeError(w, http.StatusBadRequest, "select between 1 and 500 accounts")
		return
	}
	if err := normalizeAccountBatchUpdate(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.GroupIDs != nil {
		if err := a.validateAccountGroupIDs(*input.GroupIDs); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// A zero strategy_id means "unbind", which resolves to a NULL column value.
	strategyValue, err := a.resolveStrategyBinding(input.StrategyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	rows, err := tx.Query(`SELECT id, extra_json FROM accounts WHERE deleted_at IS NULL AND archived_at IS NULL AND id IN (`+placeholders+`)`, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	type batchAccount struct {
		id    int64
		extra string
	}
	accounts := make([]batchAccount, 0, len(ids))
	for rows.Next() {
		var item batchAccount
		if err := rows.Scan(&item.id, &item.extra); err != nil {
			rows.Close()
			writeDBError(w, err)
			return
		}
		accounts = append(accounts, item)
	}
	if err := rows.Close(); err != nil {
		writeDBError(w, err)
		return
	}
	if len(accounts) == 0 {
		writeError(w, http.StatusNotFound, "no selected accounts were found")
		return
	}
	setParts := make([]string, 0, 9)
	setArgs := make([]any, 0, 9+len(accounts))
	appendUpdate := func(column string, value any) {
		setParts = append(setParts, column+" = ?")
		setArgs = append(setArgs, value)
	}
	if input.Concurrency != nil {
		appendUpdate("concurrency", *input.Concurrency)
	}
	if input.Priority != nil {
		appendUpdate("priority", *input.Priority)
	}
	if input.RateMultiplier != nil {
		appendUpdate("rate_multiplier", *input.RateMultiplier)
	}
	if input.AccountPrice != nil {
		appendUpdate("account_price", *input.AccountPrice)
	}
	if input.BaseRPM != nil {
		appendUpdate("base_rpm", *input.BaseRPM)
	}
	if input.RPMStrategy != nil {
		appendUpdate("rpm_strategy", *input.RPMStrategy)
	}
	if input.RPMStickyBuffer != nil {
		appendUpdate("rpm_sticky_buffer", *input.RPMStickyBuffer)
	}
	if input.UserMsgQueueMode != nil {
		appendUpdate("user_msg_queue_mode", *input.UserMsgQueueMode)
	}
	if input.StrategyID != nil {
		appendUpdate("strategy_id", strategyValue)
	}
	if input.Quota5HThresholdEnabled != nil {
		appendUpdate("quota_5h_threshold_enabled", boolInt(*input.Quota5HThresholdEnabled))
	}
	if input.Quota5HThresholdPercent != nil {
		appendUpdate("quota_5h_threshold_percent", *input.Quota5HThresholdPercent)
	}
	if input.Quota7DThresholdEnabled != nil {
		appendUpdate("quota_7d_threshold_enabled", boolInt(*input.Quota7DThresholdEnabled))
	}
	if input.Quota7DThresholdPercent != nil {
		appendUpdate("quota_7d_threshold_percent", *input.Quota7DThresholdPercent)
	}
	setParts = append(setParts, "updated_at = "+nowSQL)
	accountIDs := make([]any, 0, len(accounts))
	for _, item := range accounts {
		accountIDs = append(accountIDs, item.id)
	}
	updateArgs := append(setArgs, accountIDs...)
	if _, err := tx.Exec(`UPDATE accounts SET `+strings.Join(setParts, ", ")+` WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(accounts)), ",")+`)`, updateArgs...); err != nil {
		writeDBError(w, err)
		return
	}
	for _, item := range accounts {
		extra := decodeObject(item.extra)
		if input.BaseRPM != nil {
			extra["base_rpm"] = *input.BaseRPM
		}
		if input.RPMStrategy != nil {
			extra["rpm_strategy"] = *input.RPMStrategy
		}
		if input.RPMStickyBuffer != nil {
			extra["rpm_sticky_buffer"] = *input.RPMStickyBuffer
		}
		if input.UserMsgQueueMode != nil {
			extra["user_msg_queue_mode"] = *input.UserMsgQueueMode
		}
		if input.BaseRPM != nil || input.RPMStrategy != nil || input.RPMStickyBuffer != nil || input.UserMsgQueueMode != nil {
			encoded, _ := json.Marshal(extra)
			if _, err := tx.Exec(`UPDATE accounts SET extra_json = ? WHERE id = ?`, string(encoded), item.id); err != nil {
				writeDBError(w, err)
				return
			}
		}
		if input.GroupIDs != nil {
			priority := 0
			if input.Priority != nil {
				priority = *input.Priority
			} else if err := tx.QueryRow(`SELECT priority FROM accounts WHERE id = ?`, item.id).Scan(&priority); err != nil {
				writeDBError(w, err)
				return
			}
			if err := setAccountGroups(tx, item.id, *input.GroupIDs, priority); err != nil {
				writeDBError(w, err)
				return
			}
		} else if input.Priority != nil {
			if _, err := tx.Exec(`UPDATE account_groups SET priority = ? WHERE account_id = ?`, *input.Priority, item.id); err != nil {
				writeDBError(w, err)
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	if input.Quota5HThresholdEnabled != nil || input.Quota5HThresholdPercent != nil || input.Quota7DThresholdEnabled != nil || input.Quota7DThresholdPercent != nil {
		for _, item := range accounts {
			if err := a.enforceStoredAccountQuotaThresholds(item.id, rateLimitPolicy{}); err != nil {
				logDatabaseWriteError("enforce batch account quota thresholds", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]int{"matched": len(accounts), "updated": len(accounts), "skipped": len(ids) - len(accounts)})
}

func normalizeAccountBatchUpdate(input *accountBatchUpdateInput) error {
	if input.Concurrency == nil && input.Priority == nil && input.RateMultiplier == nil && input.AccountPrice == nil && input.BaseRPM == nil && input.RPMStrategy == nil && input.RPMStickyBuffer == nil && input.UserMsgQueueMode == nil && input.StrategyID == nil && input.GroupIDs == nil && input.Quota5HThresholdEnabled == nil && input.Quota5HThresholdPercent == nil && input.Quota7DThresholdEnabled == nil && input.Quota7DThresholdPercent == nil {
		return errors.New("select at least one field to update")
	}
	if input.Concurrency != nil && *input.Concurrency <= 0 {
		return errors.New("concurrency must be greater than zero")
	}
	if input.RateMultiplier != nil && *input.RateMultiplier < 0 {
		return errors.New("rate multiplier cannot be negative")
	}
	if input.AccountPrice != nil && *input.AccountPrice < 0 {
		return errors.New("account price cannot be negative")
	}
	if input.BaseRPM != nil && (*input.BaseRPM < 0 || *input.BaseRPM > 10000) {
		return errors.New("base RPM must be between 0 and 10000")
	}
	if input.RPMStickyBuffer != nil && *input.RPMStickyBuffer < 0 {
		return errors.New("RPM sticky buffer cannot be negative")
	}
	if input.Quota5HThresholdPercent != nil && (*input.Quota5HThresholdPercent < 1 || *input.Quota5HThresholdPercent > 100) {
		return errors.New("5h quota threshold must be between 1 and 100")
	}
	if input.Quota7DThresholdPercent != nil && (*input.Quota7DThresholdPercent < 1 || *input.Quota7DThresholdPercent > 100) {
		return errors.New("7d quota threshold must be between 1 and 100")
	}
	if input.RPMStrategy != nil {
		value := strings.TrimSpace(*input.RPMStrategy)
		if value != "tiered" && value != "sticky_exempt" && value != "fixed" {
			return errors.New("invalid RPM strategy")
		}
		*input.RPMStrategy = value
	}
	if input.UserMsgQueueMode != nil {
		value := strings.TrimSpace(*input.UserMsgQueueMode)
		if value != "off" && value != "soft" && value != "serial" {
			return errors.New("invalid user message queue mode")
		}
		*input.UserMsgQueueMode = value
	}
	if input.GroupIDs != nil {
		groups := uniqueGroups(*input.GroupIDs)
		if len(groups) == 0 {
			return errors.New("select at least one group")
		}
		*input.GroupIDs = groups
	}
	return nil
}

func uniquePositiveIDs(values []int64, limit int) []int64 {
	result := make([]int64, 0, len(values))
	seen := make(map[int64]bool, len(values))
	for _, id := range values {
		if id <= 0 || seen[id] || len(result) >= limit {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	return result
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
	input.ClientRequestID = normalizeGatewayCorrelationID(input.ClientRequestID)
	input.TraceID = normalizeGatewayCorrelationID(input.TraceID)
	input.UpstreamRequestID = normalizeGatewayCorrelationID(input.UpstreamRequestID)
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
	if !groupIDPattern.MatchString(input.GroupID) {
		return usageLog{}, false, errors.New("invalid usage group")
	}

	var accountName, accountSourceSKHint string
	var accountRate float64
	if input.AccountID == 0 {
		err = tx.QueryRow(`SELECT a.id, a.name, a.rate_multiplier, a.source_sk_hint FROM accounts a JOIN account_groups ag ON ag.account_id = a.id JOIN groups g ON g.id = ag.group_id WHERE ag.group_id = ? AND g.status = 'active' AND a.deleted_at IS NULL AND a.status = 'active' AND a.schedulable = 1 AND (a.expires_at IS NULL OR a.expires_at > `+nowSQL+`) AND (a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= `+nowSQL+`) ORDER BY ag.priority, a.priority, COALESCE(a.last_used_at, ''), a.id LIMIT 1`, input.GroupID).Scan(&input.AccountID, &accountName, &accountRate, &accountSourceSKHint)
	} else {
		err = tx.QueryRow(`SELECT a.name, a.rate_multiplier, a.source_sk_hint FROM accounts a JOIN account_groups ag ON ag.account_id = a.id WHERE a.id = ? AND ag.group_id = ? AND a.deleted_at IS NULL`, input.AccountID, input.GroupID).Scan(&accountName, &accountRate, &accountSourceSKHint)
	}
	if err != nil {
		return usageLog{}, false, err
	}
	if strings.TrimSpace(input.AccountSKHint) == "" {
		input.AccountSKHint = accountSourceSKHint
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
		ClientRequestID:       input.ClientRequestID,
		TraceID:               input.TraceID,
		UpstreamRequestID:     input.UpstreamRequestID,
		PurposeKey:            input.PurposeKey,
		PurposeName:           purposeName,
		GroupID:               input.GroupID,
		AccountID:             input.AccountID,
		AccountName:           accountName,
		AccountSKHint:         input.AccountSKHint,
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
	if input.BilledCostOverride != nil {
		if *input.BilledCostOverride < 0 {
			return usageLog{}, false, errors.New("invalid usage billed cost")
		}
		item.BilledCost = money(*input.BilledCostOverride)
	}
	if input.ActualCostOverride != nil {
		if *input.ActualCostOverride < 0 {
			return usageLog{}, false, errors.New("invalid usage actual cost")
		}
		item.ActualCost = money(*input.ActualCostOverride)
	}
	item.UserID = optionalID(input.UserID)
	item.APIKeyID = optionalID(input.APIKeyID)
	result, err := tx.Exec(`INSERT INTO usage_logs (user_id, api_key_id, request_id, client_request_id, trace_id, upstream_request_id, purpose_id, purpose_key, purpose_name, group_id, account_id, account_name, account_sk_hint, model, input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, input_cost, output_cost, cache_creation_cost, cache_read_cost, base_cost, billed_cost, actual_cost, group_rate_multiplier, account_rate_multiplier, stream, duration_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, item.UserID, item.APIKeyID, item.RequestID, item.ClientRequestID, item.TraceID, item.UpstreamRequestID, purposeID, item.PurposeKey, item.PurposeName, item.GroupID, item.AccountID, item.AccountName, item.AccountSKHint, item.Model, item.InputTokens, item.OutputTokens, item.CacheCreationTokens, item.CacheReadTokens, item.InputCost, item.OutputCost, item.CacheCreationCost, item.CacheReadCost, item.BaseCost, item.BilledCost, item.ActualCost, item.GroupRateMultiplier, item.AccountRateMultiplier, boolInt(item.Stream), item.DurationMS)
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
	if err := addAccountUsageTotals(tx, item); err != nil {
		return usageLog{}, false, err
	}
	if input.APIKeyID > 0 {
		if _, err := tx.Exec(`UPDATE api_keys SET quota_used = quota_used + ?, last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, item.BilledCost, input.APIKeyID); err != nil {
			return usageLog{}, false, err
		}
	}
	if input.UserID > 0 {
		if _, err := tx.Exec(`UPDATE users SET balance = ROUND(balance - ?, 8), updated_at = `+nowSQL+` WHERE id = ? AND balance IS NOT NULL AND deleted_at IS NULL`, item.BilledCost, input.UserID); err != nil {
			return usageLog{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return usageLog{}, false, err
	}
	return item, true, nil
}

func addAccountUsageTotals(tx *databaseTx, item usageLog) error {
	query := `INSERT INTO account_usage_totals (account_id, request_count, input_tokens, output_tokens, billed_cost, actual_cost, updated_at)
		VALUES (?, 1, ?, ?, ?, ?, ` + nowSQL + `)
		ON CONFLICT(account_id) DO UPDATE SET
			request_count = account_usage_totals.request_count + 1,
			input_tokens = account_usage_totals.input_tokens + excluded.input_tokens,
			output_tokens = account_usage_totals.output_tokens + excluded.output_tokens,
			billed_cost = account_usage_totals.billed_cost + excluded.billed_cost,
			actual_cost = account_usage_totals.actual_cost + excluded.actual_cost,
			updated_at = excluded.updated_at`
	if tx.dialect == dialectMySQL {
		query = `INSERT INTO account_usage_totals (account_id, request_count, input_tokens, output_tokens, billed_cost, actual_cost, updated_at)
			VALUES (?, 1, ?, ?, ?, ?, ` + nowSQL + `)
			ON DUPLICATE KEY UPDATE
				request_count = request_count + 1,
				input_tokens = input_tokens + VALUES(input_tokens),
				output_tokens = output_tokens + VALUES(output_tokens),
				billed_cost = billed_cost + VALUES(billed_cost),
				actual_cost = actual_cost + VALUES(actual_cost),
				updated_at = VALUES(updated_at)`
	}
	_, err := tx.Exec(query, item.AccountID, item.InputTokens, item.OutputTokens, item.BilledCost, item.ActualCost)
	return err
}

func (a *app) getUsageByRequestID(requestID string) (usageLog, error) {
	return scanUsage(a.db.QueryRow(usageSelect+` WHERE u.request_id = ?`, requestID))
}

// Keep the calling API key relation for tenant scoping and cost breakdowns.
// The account authorization SK is stored separately as a masked snapshot.
// Proxy joins expose the account's current or archived IP without rewriting
// historical usage rows, so existing ledgers become searchable immediately.
const usageFrom = ` FROM usage_logs u
	LEFT JOIN api_keys k ON k.id = u.api_key_id
	LEFT JOIN accounts usage_account ON usage_account.id = u.account_id
	LEFT JOIN proxies usage_proxy ON usage_proxy.id = usage_account.proxy_id
	LEFT JOIN proxies usage_archived_proxy ON usage_archived_proxy.id = usage_account.archived_proxy_id`

const usageProxyIPExpr = `COALESCE(NULLIF(usage_proxy.exit_ip, ''), NULLIF(usage_archived_proxy.exit_ip, ''), usage_proxy.host, usage_archived_proxy.host, '')`

const usageSelect = `SELECT u.id, u.user_id, u.api_key_id, COALESCE(k.name, ''), COALESCE(k.key_prefix, ''), u.account_sk_hint, u.request_id, u.client_request_id, u.trace_id, u.upstream_request_id, u.purpose_key, u.purpose_name, u.group_id, u.account_id, u.account_name, ` + usageProxyIPExpr + `, u.model, u.input_tokens, u.output_tokens, u.cache_creation_tokens, u.cache_read_tokens, u.input_cost, u.output_cost, u.cache_creation_cost, u.cache_read_cost, u.base_cost, u.billed_cost, u.actual_cost, u.group_rate_multiplier, u.account_rate_multiplier, u.stream, u.duration_ms, u.created_at` + usageFrom

func scanUsage(row scanner) (usageLog, error) {
	var item usageLog
	var stream int
	var userID, apiKeyID sql.NullInt64
	err := row.Scan(&item.ID, &userID, &apiKeyID, &item.APIKeyName, &item.APIKeyPrefix, &item.AccountSKHint, &item.RequestID, &item.ClientRequestID, &item.TraceID, &item.UpstreamRequestID, &item.PurposeKey, &item.PurposeName, &item.GroupID, &item.AccountID, &item.AccountName, &item.ProxyIP, &item.Model, &item.InputTokens, &item.OutputTokens, &item.CacheCreationTokens, &item.CacheReadTokens, &item.InputCost, &item.OutputCost, &item.CacheCreationCost, &item.CacheReadCost, &item.BaseCost, &item.BilledCost, &item.ActualCost, &item.GroupRateMultiplier, &item.AccountRateMultiplier, &stream, &item.DurationMS, &item.CreatedAt)
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
	Search     string
	GroupID    string
	PurposeKey string
	AccountID  int64
	APIKeyID   int64
	UserID     int64
	Limit      int
	Offset     int
}

func (a *app) handleUsageList(w http.ResponseWriter, r *http.Request) {
	filters := filtersFromRequest(r)
	user := currentUser(r)
	if user.Role == "user" {
		filters.UserID = user.ID
	}
	items, err := a.listUsage(filters)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if user.Role == "user" {
		redactUsageCosts(items)
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
		Search:     strings.TrimSpace(r.URL.Query().Get("search")),
		GroupID:    strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))),
		PurposeKey: strings.ToLower(strings.TrimSpace(r.URL.Query().Get("purpose_key"))),
		Limit:      20,
	}
	filters.AccountID, _ = strconv.ParseInt(r.URL.Query().Get("account_id"), 10, 64)
	filters.APIKeyID, _ = strconv.ParseInt(r.URL.Query().Get("api_key_id"), 10, 64)
	page, pageSize, offset := paginationFromRequest(r, 20, 100)
	filters.Limit, filters.Offset = pageSize, offset
	_ = page
	return filters
}

func buildUsageWhere(filters usageFilters) (string, []any) {
	conditions := []string{"1 = 1"}
	args := []any{}
	if filters.From != "" {
		conditions = append(conditions, "u.created_at >= ?")
		args = append(args, normalizeDateStart(filters.From))
	}
	if filters.To != "" {
		conditions = append(conditions, "u.created_at < ?")
		args = append(args, normalizeDateEnd(filters.To))
	}
	if filters.Search != "" {
		term := "%" + filters.Search + "%"
		conditions = append(conditions, `(u.request_id = ? OR u.client_request_id = ? OR u.trace_id = ? OR u.upstream_request_id = ?
			OR u.account_name LIKE ? OR u.account_sk_hint LIKE ? OR u.model LIKE ? OR u.purpose_name LIKE ?
			OR COALESCE(k.name, '') LIKE ? OR COALESCE(k.key_prefix, '') LIKE ?
			OR `+usageProxyIPExpr+` LIKE ? OR COALESCE(usage_proxy.host, usage_archived_proxy.host, '') LIKE ?)`)
		args = append(args, filters.Search, filters.Search, filters.Search, filters.Search, term, term, term, term, term, term, term, term)
	}
	if groupIDPattern.MatchString(filters.GroupID) {
		conditions = append(conditions, "u.group_id = ?")
		args = append(args, filters.GroupID)
	}
	if filters.PurposeKey != "" {
		conditions = append(conditions, "u.purpose_key = ?")
		args = append(args, filters.PurposeKey)
	}
	if filters.AccountID > 0 {
		conditions = append(conditions, "u.account_id = ?")
		args = append(args, filters.AccountID)
	}
	if filters.APIKeyID > 0 {
		conditions = append(conditions, "u.api_key_id = ?")
		args = append(args, filters.APIKeyID)
	}
	if filters.UserID > 0 {
		conditions = append(conditions, "u.user_id = ?")
		args = append(args, filters.UserID)
	}
	return strings.Join(conditions, " AND "), args
}

func (a *app) listUsage(filters usageFilters) ([]usageLog, error) {
	where, args := buildUsageWhere(filters)
	args = append(args, filters.Limit, filters.Offset)
	rows, err := a.db.Query(usageSelect+` WHERE `+where+` ORDER BY u.created_at DESC, u.id DESC LIMIT ? OFFSET ?`, args...)
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
	err := a.db.QueryRow(`SELECT COUNT(*)`+usageFrom+` WHERE `+where, args...).Scan(&total)
	return total, err
}

func (a *app) handleBilling(w http.ResponseWriter, r *http.Request) {
	filters := filtersFromRequest(r)
	user := currentUser(r)
	if user.Role == "user" {
		filters.UserID = user.ID
	}
	if filters.From == "" {
		filters.From = startOfMonthUTC()
	}
	breakdown := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("breakdown")))
	if breakdown == "" {
		breakdown = "all"
	}
	if breakdown != "all" && breakdown != "group" && breakdown != "account" && breakdown != "purpose" && breakdown != "api_key" {
		writeError(w, http.StatusBadRequest, "breakdown must be all, group, account, purpose, or api_key")
		return
	}
	summary, err := a.billingSummary(filters, breakdown)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if user.Role == "user" {
		summary.AvailableBalance = user.Balance
		redactBillingTotals(&summary.Totals)
		redactBillingBreakdowns(summary.ByGroup)
		redactBillingBreakdowns(summary.ByAccount)
		redactBillingBreakdowns(summary.ByPurpose)
		redactBillingBreakdowns(summary.ByAPIKey)
	}
	writeJSON(w, http.StatusOK, summary)
}

func redactUsageCosts(items []usageLog) {
	for index := range items {
		items[index].InputCost = 0
		items[index].OutputCost = 0
		items[index].CacheCreationCost = 0
		items[index].CacheReadCost = 0
		items[index].BaseCost = 0
		items[index].ActualCost = 0
		items[index].GroupRateMultiplier = 0
		items[index].AccountRateMultiplier = 0
	}
}

func redactBillingTotals(totals *billingTotals) {
	totals.BaseCost = 0
	totals.ActualCost = 0
	totals.Margin = 0
}

func redactBillingBreakdowns(items []billingBreakdown) {
	for index := range items {
		items[index].ActualCost = 0
		items[index].Margin = 0
	}
}

func (a *app) billingSummary(filters usageFilters, breakdown string) (billingSummary, error) {
	where, args := buildUsageWhere(filters)
	result := billingSummary{From: filters.From, To: filters.To, ByGroup: []billingBreakdown{}, ByAccount: []billingBreakdown{}, ByPurpose: []billingBreakdown{}, ByAPIKey: []billingBreakdown{}}
	if err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0), COALESCE(SUM(u.cache_creation_tokens + u.cache_read_tokens), 0), COALESCE(SUM(u.base_cost), 0), COALESCE(SUM(u.billed_cost), 0), COALESCE(SUM(u.actual_cost), 0), COALESCE(SUM(u.billed_cost - u.actual_cost), 0)`+usageFrom+` WHERE `+where, args...).Scan(&result.Totals.Requests, &result.Totals.InputTokens, &result.Totals.OutputTokens, &result.Totals.CacheTokens, &result.Totals.BaseCost, &result.Totals.BilledCost, &result.Totals.ActualCost, &result.Totals.Margin); err != nil {
		return result, err
	}
	type breakdownQuery struct {
		name     string
		target   *[]billingBreakdown
		keyExpr  string
		nameExpr string
	}
	queries := []breakdownQuery{
		{name: "group", target: &result.ByGroup, keyExpr: "u.group_id", nameExpr: "u.group_id"},
		{name: "account", target: &result.ByAccount, keyExpr: "CAST(u.account_id AS CHAR)", nameExpr: "u.account_name"},
		{name: "purpose", target: &result.ByPurpose, keyExpr: "u.purpose_key", nameExpr: "u.purpose_name"},
		{name: "api_key", target: &result.ByAPIKey, keyExpr: "COALESCE(CAST(u.api_key_id AS CHAR), '')", nameExpr: `COALESCE(k.name, '手动记录')`},
	}
	for _, query := range queries {
		if breakdown != "all" && breakdown != query.name {
			continue
		}
		items, err := a.queryBreakdown(where, args, query.keyExpr, query.nameExpr)
		if err != nil {
			return result, err
		}
		*query.target = items
	}
	return result, nil
}

func (a *app) queryBreakdown(where string, args []any, keyExpr, nameExpr string) ([]billingBreakdown, error) {
	query := `SELECT ` + keyExpr + `, ` + nameExpr + `, COUNT(*), COALESCE(SUM(u.billed_cost), 0), COALESCE(SUM(u.actual_cost), 0), COALESCE(SUM(u.billed_cost - u.actual_cost), 0)` + usageFrom + ` WHERE ` + where + ` GROUP BY ` + keyExpr + `, ` + nameExpr + ` ORDER BY SUM(u.billed_cost) DESC`
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
	err := a.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0), COALESCE(SUM(u.cache_creation_tokens + u.cache_read_tokens), 0), COALESCE(SUM(u.base_cost), 0), COALESCE(SUM(u.billed_cost), 0), COALESCE(SUM(u.actual_cost), 0), COALESCE(SUM(u.billed_cost - u.actual_cost), 0)`+usageFrom+` WHERE `+where, args...).Scan(&totals.Requests, &totals.InputTokens, &totals.OutputTokens, &totals.CacheTokens, &totals.BaseCost, &totals.BilledCost, &totals.ActualCost, &totals.Margin)
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

func sourceSKHint(secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(secret))
	fingerprint := hex.EncodeToString(digest[:4])
	runes := []rune(secret)
	if len(runes) <= 20 {
		return "•••••• · " + fingerprint
	}
	return string(runes[:12]) + "…" + string(runes[len(runes)-6:]) + " · " + fingerprint
}

func sourceSKHintFromCredentials(raw string) string {
	credentials := decodeObject(raw)
	for _, key := range []string{"session_key", "api_key"} {
		if secret, ok := credentials[key].(string); ok {
			if hint := sourceSKHint(secret); hint != "" {
				return hint
			}
		}
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
	return normalizeFilterTime(value, false)
}

func normalizeDateEnd(value string) string {
	return normalizeFilterTime(value, true)
}

func normalizeFilterTime(value string, end bool) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	location := time.FixedZone("Asia/Shanghai", 8*60*60)
	for _, format := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04"} {
		if parsed, err := time.ParseInLocation(format, value, location); err == nil {
			if end && format == "2006-01-02T15:04" {
				parsed = parsed.Add(time.Minute)
			}
			return parsed.UTC().Format(time.RFC3339Nano)
		}
	}
	if parsed, err := time.ParseInLocation("2006-01-02", value, location); err == nil {
		if end {
			parsed = parsed.AddDate(0, 0, 1)
		}
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	return value
}
