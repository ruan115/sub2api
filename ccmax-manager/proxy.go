package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

type proxyPool struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	SourceType       string `json:"source_type"`
	APIURL           string `json:"api_url"`
	APIHeaders       string `json:"api_headers"`
	DefaultProtocol  string `json:"default_protocol"`
	Status           string `json:"status"`
	SingleUseEnabled bool   `json:"single_use_enabled"`
	ProxyCount       int    `json:"proxy_count"`
	AvailableCount   int    `json:"available_count"`
	AssignedCount    int    `json:"assigned_count"`
	LastSyncAt       string `json:"last_sync_at"`
	LastError        string `json:"last_error"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	ProtocolSynced   int64  `json:"protocol_synced,omitempty"`
}

type proxyPoolInput struct {
	Name             string `json:"name"`
	SourceType       string `json:"source_type"`
	APIURL           string `json:"api_url"`
	APIHeaders       string `json:"api_headers"`
	DefaultProtocol  string `json:"default_protocol"`
	Status           string `json:"status"`
	SingleUseEnabled *bool  `json:"single_use_enabled"`
}

type proxyRecord struct {
	ID                 int64  `json:"id"`
	PoolID             int64  `json:"pool_id"`
	PoolName           string `json:"pool_name"`
	Name               string `json:"name"`
	Protocol           string `json:"protocol"`
	Host               string `json:"host"`
	Port               int    `json:"port"`
	Username           string `json:"username"`
	PasswordSet        bool   `json:"password_set"`
	Status             string `json:"status"`
	ExitIP             string `json:"exit_ip"`
	LatencyMS          *int64 `json:"latency_ms"`
	LastTestAt         string `json:"last_test_at"`
	LastError          string `json:"last_error"`
	AssignedTo         string `json:"assigned_to"`
	HistoricalAccounts string `json:"historical_accounts"`
	UsedAccountCount   int    `json:"used_account_count"`
	SingleUseEnabled   bool   `json:"single_use_enabled"`
	CreatedAt          string `json:"created_at"`
	UpdatedAt          string `json:"updated_at"`
	ArchivedAt         string `json:"archived_at,omitempty"`
	ReuseApprovedAt    string `json:"reuse_approved_at,omitempty"`
}

type parsedProxy struct {
	Protocol string
	Host     string
	Port     int
	Username string
	Password string
}

type proxyTestResult struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Success   bool   `json:"success"`
	IP        string `json:"ip,omitempty"`
	LatencyMS int64  `json:"latency_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type proxyBatchTestResponse struct {
	Total   int               `json:"total"`
	Success int               `json:"success"`
	Failed  int               `json:"failed"`
	Items   []proxyTestResult `json:"items"`
}

type proxyBatchDeleteInput struct {
	IDs []int64 `json:"ids"`
}

type proxyBatchDeleteResponse struct {
	Matched            int64 `json:"matched"`
	Deleted            int64 `json:"deleted"`
	Archived           int64 `json:"archived"`
	ReassignedAccounts int   `json:"reassigned_accounts"`
	PausedAccounts     int   `json:"paused_accounts"`
}

var proxyTestEndpoint = "https://api.ipify.org?format=json"

const (
	defaultUpstreamResponseHeaderTimeout = 15 * time.Minute
	defaultUpstreamRequestTimeout        = 0
	deadProxyPoolKind                    = "dead"
	deadProxyPoolName                    = "死亡 IP 池"
	deadProxyReason                      = "死亡账号释放，禁止重新分配"
)

type upstreamProxyClientKey struct {
	proxyURLHash          string
	responseHeaderTimeout time.Duration
	requestTimeout        time.Duration
}

var upstreamProxyClients sync.Map

func (a *app) migrateProxyFeatures() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS proxy_pools (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			source_type TEXT NOT NULL DEFAULT 'manual' CHECK (source_type IN ('manual', 'api')),
			api_url TEXT NOT NULL DEFAULT '',
			api_headers_json TEXT NOT NULL DEFAULT '{}',
			default_protocol TEXT NOT NULL DEFAULT 'socks5' CHECK (default_protocol IN (` + proxyProtocolCheckList + `)),
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
			single_use_enabled INTEGER NOT NULL DEFAULT 1,
			last_sync_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			deleted_at TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS proxies (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pool_id INTEGER NOT NULL REFERENCES proxy_pools(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			protocol TEXT NOT NULL CHECK (protocol IN (` + proxyProtocolCheckList + `)),
			host TEXT NOT NULL,
			port INTEGER NOT NULL CHECK (port > 0 AND port < 65536),
			username TEXT NOT NULL DEFAULT '',
			password TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'error', 'disabled')),
			exit_ip TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER,
			last_test_at TEXT,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			deleted_at TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_unique ON proxies(pool_id, protocol, host, port, username, password) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_proxy_pool_status ON proxies(pool_id, status) WHERE deleted_at IS NULL`,
		// Single-use lookups correlate proxies by address across pools, which
		// idx_proxy_unique cannot serve because it leads with pool_id.
		`CREATE INDEX IF NOT EXISTS idx_proxy_identity ON proxies(protocol, host, port, username, password)`,
		`CREATE TABLE IF NOT EXISTS account_rpm_events (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_rpm ON account_rpm_events(account_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_account_rpm_created ON account_rpm_events(created_at, account_id)`,
		`CREATE TABLE IF NOT EXISTS dispatch_sessions (
			session_hash TEXT NOT NULL,
			api_key_id INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			expires_at TEXT NOT NULL,
			last_input_tokens INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (session_hash, api_key_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_dispatch_sessions_expiry ON dispatch_sessions(expires_at)`,
		`CREATE TABLE IF NOT EXISTS account_inflight (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			requests INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0)
		)`,
		`CREATE TABLE IF NOT EXISTS proxy_account_history (
			proxy_id INTEGER NOT NULL REFERENCES proxies(id) ON DELETE CASCADE,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			first_bound_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			last_bound_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			bind_count INTEGER NOT NULL DEFAULT 1,
			PRIMARY KEY (proxy_id, account_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_proxy_account_history_account ON proxy_account_history(account_id, last_bound_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate proxy features: %w", err)
		}
	}
	if err := addColumnIfMissing(a.db, "proxies", "last_error", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "proxy_pools", "system_kind", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "proxy_pools", "single_use_enabled", "INTEGER NOT NULL DEFAULT 1"); err != nil {
		return err
	}
	if err := addColumnIfMissing(a.db, "proxies", "reuse_approved_at", "TEXT"); err != nil {
		return err
	}
	if _, err := a.db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_pools_system_kind ON proxy_pools(system_kind) WHERE system_kind != ''`); err != nil {
		return fmt.Errorf("index system proxy pools: %w", err)
	}
	if err := a.migrateProxyProtocols(); err != nil {
		return err
	}
	columns := []struct{ name, definition string }{
		{"proxy_pool_id", "INTEGER REFERENCES proxy_pools(id)"},
		{"proxy_id", "INTEGER REFERENCES proxies(id)"},
		{"auto_proxy", "INTEGER NOT NULL DEFAULT 0"},
		{"base_rpm", "INTEGER NOT NULL DEFAULT 0"},
		{"rpm_strategy", "TEXT NOT NULL DEFAULT 'tiered'"},
		{"rpm_sticky_buffer", "INTEGER NOT NULL DEFAULT 0"},
		{"user_msg_queue_mode", "TEXT NOT NULL DEFAULT 'off'"},
	}
	for _, column := range columns {
		if err := addColumnIfMissing(a.db, "accounts", column.name, column.definition); err != nil {
			return err
		}
	}
	triggers := []string{
		`CREATE TRIGGER IF NOT EXISTS trg_account_proxy_history_insert
		AFTER INSERT ON accounts WHEN NEW.proxy_id IS NOT NULL
		BEGIN
			INSERT INTO proxy_account_history (proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
			VALUES (NEW.proxy_id, NEW.id, NEW.created_at, NEW.updated_at, 1)
			ON CONFLICT(proxy_id, account_id) DO UPDATE SET
				last_bound_at = excluded.last_bound_at,
				bind_count = proxy_account_history.bind_count + 1;
		END`,
		`CREATE TRIGGER IF NOT EXISTS trg_account_proxy_history_update
		AFTER UPDATE OF proxy_id ON accounts
		WHEN NEW.proxy_id IS NOT NULL AND (OLD.proxy_id IS NULL OR OLD.proxy_id != NEW.proxy_id)
		BEGIN
			INSERT INTO proxy_account_history (proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
			VALUES (NEW.proxy_id, NEW.id, NEW.updated_at, NEW.updated_at, 1)
			ON CONFLICT(proxy_id, account_id) DO UPDATE SET
				last_bound_at = excluded.last_bound_at,
				bind_count = proxy_account_history.bind_count + 1;
		END`,
	}
	for _, trigger := range triggers {
		if _, err := a.db.Exec(trigger); err != nil {
			return fmt.Errorf("migrate proxy assignment history: %w", err)
		}
	}
	// Proxy pool seeds, assignment-history backfills and in-flight lease resets
	// live in migrateSharedData so MySQL runs them too.
	return nil
}

func ensureDeadProxyPool(tx *databaseTx) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM proxy_pools WHERE system_kind = ? ORDER BY id LIMIT 1`, deadProxyPoolKind).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO proxy_pools
			(name, source_type, api_headers_json, default_protocol, status, single_use_enabled, system_kind)
			VALUES (?, 'manual', '{}', 'socks5', 'disabled', 1, ?)`, deadProxyPoolName, deadProxyPoolKind); err != nil {
			return 0, fmt.Errorf("insert dead proxy pool: %w", err)
		}
		err = tx.QueryRow(`SELECT id FROM proxy_pools WHERE name = ?`, deadProxyPoolName).Scan(&id)
	}
	if err != nil {
		return 0, fmt.Errorf("find dead proxy pool: %w", err)
	}
	if _, err := tx.Exec(`UPDATE proxy_pools SET
		system_kind = ?, source_type = 'manual', api_url = '', api_headers_json = '{}',
		status = 'disabled', single_use_enabled = 1, last_error = '', deleted_at = NULL, updated_at = `+nowSQL+`
		WHERE id = ?`, deadProxyPoolKind, id); err != nil {
		return 0, fmt.Errorf("lock dead proxy pool: %w", err)
	}
	return id, nil
}

func (a *app) migrateDeadProxyAssignments() error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT DISTINCT archived_proxy_id FROM accounts
		WHERE archived_at IS NOT NULL AND archived_proxy_id IS NOT NULL AND deleted_at IS NULL`)
	if err != nil {
		return fmt.Errorf("list archived account proxies: %w", err)
	}
	proxyIDs := []int64{}
	for rows.Next() {
		var proxyID int64
		if err := rows.Scan(&proxyID); err != nil {
			rows.Close()
			return err
		}
		proxyIDs = append(proxyIDs, proxyID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := quarantineProxyIDs(tx, proxyIDs); err != nil {
		return fmt.Errorf("migrate archived account proxies: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit dead proxy migration: %w", err)
	}
	return nil
}

func quarantineProxyIDs(tx *databaseTx, proxyIDs []int64) error {
	if len(proxyIDs) == 0 {
		return nil
	}
	deadPoolID, err := ensureDeadProxyPool(tx)
	if err != nil {
		return err
	}
	for _, proxyID := range proxyIDs {
		var singleUseEnabled int
		err := tx.QueryRow(`SELECT pool.single_use_enabled FROM proxies proxy
			JOIN proxy_pools pool ON pool.id = proxy.pool_id
			WHERE proxy.id = ? AND proxy.deleted_at IS NULL`, proxyID).Scan(&singleUseEnabled)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if singleUseEnabled == 0 {
			continue
		}
		var liveOwners int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM accounts
			WHERE proxy_id = ? AND deleted_at IS NULL AND archived_at IS NULL`, proxyID).Scan(&liveOwners); err != nil {
			return err
		}
		if liveOwners > 0 {
			continue
		}

		var duplicateID int64
		err = tx.QueryRow(`SELECT target.id
			FROM proxies source
			JOIN proxies target ON target.pool_id = ? AND target.id != source.id
				AND target.protocol = source.protocol AND target.host = source.host AND target.port = source.port
				AND target.username = source.username AND target.password = source.password
			WHERE source.id = ? AND source.deleted_at IS NULL AND target.deleted_at IS NULL
			LIMIT 1`, deadPoolID, proxyID).Scan(&duplicateID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if _, err := tx.Exec(`UPDATE accounts SET archived_proxy_id = ?
				WHERE archived_proxy_id = ? AND archived_at IS NOT NULL`, duplicateID, proxyID); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO proxy_account_history
				(proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
				SELECT ?, account_id, first_bound_at, last_bound_at, bind_count
				FROM proxy_account_history WHERE proxy_id = ?
				ON CONFLICT(proxy_id, account_id) DO UPDATE SET
					first_bound_at = MIN(proxy_account_history.first_bound_at, excluded.first_bound_at),
					last_bound_at = MAX(proxy_account_history.last_bound_at, excluded.last_bound_at),
					bind_count = proxy_account_history.bind_count + excluded.bind_count`, duplicateID, proxyID); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM proxy_account_history WHERE proxy_id = ?`, proxyID); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE proxies SET status = 'disabled', last_error = ?, deleted_at = `+nowSQL+`, updated_at = `+nowSQL+`
				WHERE id = ? AND deleted_at IS NULL`, deadProxyReason, proxyID); err != nil {
				return err
			}
			continue
		}

		if _, err := tx.Exec(`UPDATE proxies SET pool_id = ?, status = 'disabled', last_error = ?, updated_at = `+nowSQL+`
			WHERE id = ? AND deleted_at IS NULL`, deadPoolID, deadProxyReason, proxyID); err != nil {
			return err
		}
	}
	return nil
}

func addColumnIfMissing(db *database, table, name, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, pk int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		if columnName == name {
			return nil
		}
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + definition)
	return err
}

func normalizeProxyPool(input *proxyPoolInput) error {
	input.Name = strings.TrimSpace(input.Name)
	input.SourceType = strings.TrimSpace(input.SourceType)
	input.APIURL = strings.TrimSpace(input.APIURL)
	input.APIHeaders = strings.TrimSpace(input.APIHeaders)
	input.DefaultProtocol = strings.ToLower(strings.TrimSpace(input.DefaultProtocol))
	input.Status = strings.TrimSpace(input.Status)
	if input.SourceType == "" {
		input.SourceType = "manual"
	}
	if input.DefaultProtocol == "" {
		input.DefaultProtocol = "socks5"
	}
	if input.Status == "" {
		input.Status = "active"
	}
	if input.APIHeaders == "" {
		input.APIHeaders = "{}"
	}
	if input.Name == "" || (input.SourceType != "manual" && input.SourceType != "api") || !validProxyProtocol(input.DefaultProtocol) || (input.Status != "active" && input.Status != "disabled") {
		return errors.New("invalid proxy pool fields")
	}
	if input.Name == deadProxyPoolName {
		return errors.New("proxy pool name is reserved")
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(input.APIHeaders), &headers); err != nil {
		return errors.New("API headers must be a JSON object")
	}
	if input.SourceType == "api" {
		u, err := url.Parse(input.APIURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("proxy API URL is invalid")
		}
	}
	return nil
}

func proxyNotQuarantinedPredicate(alias string) string {
	return `(EXISTS (SELECT 1 FROM proxy_pools source_pool
		WHERE source_pool.id = ` + alias + `.pool_id AND source_pool.single_use_enabled = 0)
		OR NOT EXISTS (SELECT 1 FROM proxies dead_proxy
		JOIN proxy_pools dead_pool ON dead_pool.id = dead_proxy.pool_id
		WHERE dead_pool.system_kind = 'dead' AND dead_proxy.deleted_at IS NULL
		AND dead_proxy.protocol = ` + alias + `.protocol AND dead_proxy.host = ` + alias + `.host
		AND dead_proxy.port = ` + alias + `.port AND dead_proxy.username = ` + alias + `.username
		AND dead_proxy.password = ` + alias + `.password))`
}

func proxyIdentityUnusedPredicate(alias string) string {
	return `(` + alias + `.reuse_approved_at IS NOT NULL OR NOT EXISTS (SELECT 1 FROM proxy_account_history used_history
		JOIN proxies used_proxy ON used_proxy.id = used_history.proxy_id
		WHERE used_proxy.protocol = ` + alias + `.protocol AND used_proxy.host = ` + alias + `.host
		AND used_proxy.port = ` + alias + `.port AND used_proxy.username = ` + alias + `.username
		AND used_proxy.password = ` + alias + `.password))`
}

func proxyAvailableToAccountPredicate(proxyAlias, poolAlias string) string {
	return `(` + poolAlias + `.single_use_enabled = 0
		OR EXISTS (SELECT 1 FROM accounts current_owner
			WHERE current_owner.id = ? AND current_owner.proxy_id = ` + proxyAlias + `.id
			AND current_owner.deleted_at IS NULL AND current_owner.archived_at IS NULL)
		OR ` + proxyIdentityUnusedPredicate(proxyAlias) + `)`
}

var proxyPoolSelect = `SELECT p.id, p.name, p.source_type, p.api_url, p.api_headers_json, p.default_protocol, p.status, p.single_use_enabled,
	(SELECT COUNT(*) FROM proxies x WHERE x.pool_id = p.id AND x.deleted_at IS NULL AND ` + proxyNotQuarantinedPredicate("x") + `),
	(SELECT COUNT(*) FROM proxies x WHERE x.pool_id = p.id AND x.status = 'active' AND x.deleted_at IS NULL
		AND ` + proxyNotQuarantinedPredicate("x") + `
		AND (p.single_use_enabled = 0 OR ` + proxyIdentityUnusedPredicate("x") + `)
		AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.proxy_id = x.id AND a.deleted_at IS NULL AND a.archived_at IS NULL)),
	(SELECT COUNT(DISTINCT a.id) FROM accounts a WHERE a.proxy_pool_id = p.id AND a.proxy_id IS NOT NULL AND a.deleted_at IS NULL AND a.archived_at IS NULL),
	p.last_sync_at, p.last_error, p.created_at, p.updated_at FROM proxy_pools p`

func scanProxyPool(row scanner) (proxyPool, error) {
	var item proxyPool
	var lastSync sql.NullString
	var singleUseEnabled int
	err := row.Scan(&item.ID, &item.Name, &item.SourceType, &item.APIURL, &item.APIHeaders, &item.DefaultProtocol, &item.Status, &singleUseEnabled, &item.ProxyCount, &item.AvailableCount, &item.AssignedCount, &lastSync, &item.LastError, &item.CreatedAt, &item.UpdatedAt)
	item.SingleUseEnabled = singleUseEnabled == 1
	item.LastSyncAt = nullText(lastSync)
	return item, err
}

func (a *app) handleProxyPools(w http.ResponseWriter, r *http.Request) {
	query := proxyPoolSelect + ` WHERE p.deleted_at IS NULL AND p.system_kind = ''`
	args := []any{}
	user := currentUser(r)
	if user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "scope_account")
		query += ` AND EXISTS (SELECT 1 FROM accounts scope_account WHERE scope_account.proxy_pool_id = p.id AND scope_account.deleted_at IS NULL AND scope_account.archived_at IS NULL AND ` + condition + `)`
		args = append(args, scopeArgs...)
	}
	query += ` ORDER BY p.id`
	rows, err := a.db.Query(query, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []proxyPool{}
	for rows.Next() {
		item, err := scanProxyPool(rows)
		if err != nil {
			writeDBError(w, err)
			return
		}
		if user.Role != "admin" {
			item.APIHeaders = "{}"
			item.APIURL = redactProxyAPIURL(item.APIURL)
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

func redactProxyAPIURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return value
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func (a *app) handleProxyPoolCreate(w http.ResponseWriter, r *http.Request) {
	var input proxyPoolInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := normalizeProxyPool(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	singleUseEnabled := true
	if input.SingleUseEnabled != nil {
		singleUseEnabled = *input.SingleUseEnabled
	}
	result, err := a.db.Exec(`INSERT INTO proxy_pools (name, source_type, api_url, api_headers_json, default_protocol, status, single_use_enabled) VALUES (?, ?, ?, ?, ?, ?, ?)`, input.Name, input.SourceType, input.APIURL, input.APIHeaders, input.DefaultProtocol, input.Status, boolInt(singleUseEnabled))
	if err != nil {
		writeDBError(w, err)
		return
	}
	id, _ := result.LastInsertId()
	item, _ := scanProxyPool(a.db.QueryRow(proxyPoolSelect+` WHERE p.id = ?`, id))
	writeJSON(w, http.StatusCreated, item)
}

func (a *app) handleProxyPoolUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var input proxyPoolInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := normalizeProxyPool(&input); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()

	var previousProtocol string
	if err := tx.QueryRow(`SELECT default_protocol FROM proxy_pools WHERE id = ? AND deleted_at IS NULL AND system_kind = ''`, id).Scan(&previousProtocol); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "proxy pool not found")
			return
		}
		writeDBError(w, err)
		return
	}

	var protocolSynced int64
	if previousProtocol != input.DefaultProtocol {
		var collisions int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM (
			SELECT 1 FROM proxies WHERE pool_id = ? AND deleted_at IS NULL
			GROUP BY host, port, username, password HAVING COUNT(*) > 1
		) duplicate_endpoints`, id).Scan(&collisions); err != nil {
			writeDBError(w, err)
			return
		}
		if collisions > 0 {
			writeError(w, http.StatusConflict, "代理池中存在相同地址和认证信息的多协议代理，无法统一协议")
			return
		}
		result, err := tx.Exec(`UPDATE proxies SET protocol = ?, updated_at = `+nowSQL+` WHERE pool_id = ? AND deleted_at IS NULL`, input.DefaultProtocol, id)
		if err != nil {
			writeDBError(w, err)
			return
		}
		protocolSynced, _ = result.RowsAffected()
	}
	var singleUseEnabled any
	if input.SingleUseEnabled != nil {
		singleUseEnabled = boolInt(*input.SingleUseEnabled)
	}
	if _, err := tx.Exec(`UPDATE proxy_pools SET name = ?, source_type = ?, api_url = ?, api_headers_json = ?, default_protocol = ?, status = ?, single_use_enabled = COALESCE(?, single_use_enabled), updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL AND system_kind = ''`, input.Name, input.SourceType, input.APIURL, input.APIHeaders, input.DefaultProtocol, input.Status, singleUseEnabled, id); err != nil {
		writeDBError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	item, err := scanProxyPool(a.db.QueryRow(proxyPoolSelect+` WHERE p.id = ? AND p.deleted_at IS NULL AND p.system_kind = ''`, id))
	if err != nil {
		writeError(w, http.StatusNotFound, "proxy pool not found")
		return
	}
	item.ProtocolSynced = protocolSynced
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleProxyPoolDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var assigned int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE proxy_pool_id = ? AND deleted_at IS NULL AND archived_at IS NULL`, id).Scan(&assigned)
	if assigned > 0 {
		writeError(w, http.StatusConflict, fmt.Sprintf("代理池仍被 %d 个账号占用，请先解绑账号", assigned))
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()
	var name string
	if err := tx.QueryRow(`SELECT name FROM proxy_pools WHERE id = ? AND deleted_at IS NULL AND system_kind = ''`, id).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "proxy pool not found")
			return
		}
		writeDBError(w, err)
		return
	}
	result, err := tx.Exec(`UPDATE proxies SET status = 'disabled', deleted_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE pool_id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	deletedProxies, _ := result.RowsAffected()
	if _, err := tx.Exec(`UPDATE proxy_pools SET status = 'disabled', deleted_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, id); err != nil {
		writeDBError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":          true,
		"name":             name,
		"deleted_proxies":  deletedProxies,
		"archived_proxies": deletedProxies,
	})
}

const proxySelect = `SELECT x.id, x.pool_id, p.name, x.name, x.protocol, x.host, x.port, x.username, x.password != '', x.status, x.exit_ip, x.latency_ms, x.last_test_at, x.last_error,
	COALESCE((SELECT GROUP_CONCAT(a.name, ', ') FROM accounts a WHERE a.proxy_id = x.id AND a.deleted_at IS NULL AND a.archived_at IS NULL), ''),
	COALESCE((SELECT GROUP_CONCAT(history_account.name, ', ') FROM proxy_account_history history JOIN accounts history_account ON history_account.id = history.account_id WHERE history.proxy_id = x.id), ''),
	(SELECT COUNT(DISTINCT used_history.account_id) FROM proxy_account_history used_history
		JOIN proxies used_proxy ON used_proxy.id = used_history.proxy_id
		WHERE used_proxy.protocol = x.protocol AND used_proxy.host = x.host AND used_proxy.port = x.port
		AND used_proxy.username = x.username AND used_proxy.password = x.password),
	p.single_use_enabled, x.created_at, x.updated_at, x.reuse_approved_at,
	CASE WHEN p.system_kind = 'dead' THEN x.updated_at ELSE x.deleted_at END
	FROM proxies x JOIN proxy_pools p ON p.id = x.pool_id`

func scanProxy(row scanner) (proxyRecord, error) {
	var item proxyRecord
	var latency sql.NullInt64
	var lastTest sql.NullString
	var archivedAt, reuseApprovedAt sql.NullString
	var singleUseEnabled int
	err := row.Scan(&item.ID, &item.PoolID, &item.PoolName, &item.Name, &item.Protocol, &item.Host, &item.Port, &item.Username, &item.PasswordSet, &item.Status, &item.ExitIP, &latency, &lastTest, &item.LastError, &item.AssignedTo, &item.HistoricalAccounts, &item.UsedAccountCount, &singleUseEnabled, &item.CreatedAt, &item.UpdatedAt, &reuseApprovedAt, &archivedAt)
	item.LatencyMS = nullIntPointer(latency)
	item.LastTestAt = nullText(lastTest)
	item.ArchivedAt = nullText(archivedAt)
	item.ReuseApprovedAt = nullText(reuseApprovedAt)
	item.SingleUseEnabled = singleUseEnabled == 1
	return item, err
}

func (a *app) handleProxies(w http.ResponseWriter, r *http.Request) {
	query := proxySelect + ` WHERE x.deleted_at IS NULL AND p.system_kind = ''`
	args := []any{}
	query += ` AND ` + proxyNotQuarantinedPredicate("x")
	if user := currentUser(r); user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "scope_account")
		query += ` AND EXISTS (SELECT 1 FROM accounts scope_account WHERE scope_account.proxy_id = x.id AND scope_account.deleted_at IS NULL AND scope_account.archived_at IS NULL AND ` + condition + `)`
		args = append(args, scopeArgs...)
	}
	if poolID, _ := strconv.ParseInt(r.URL.Query().Get("pool_id"), 10, 64); poolID > 0 {
		query += ` AND x.pool_id = ?`
		args = append(args, poolID)
	}
	query += ` ORDER BY x.pool_id, x.id DESC`
	rows, err := a.db.Query(query, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []proxyRecord{}
	for rows.Next() {
		item, err := scanProxy(rows)
		if err != nil {
			writeDBError(w, err)
			return
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *app) handleArchivedProxies(w http.ResponseWriter, r *http.Request) {
	query := proxySelect + ` WHERE ((x.deleted_at IS NOT NULL AND p.system_kind = '')
		OR (p.system_kind = 'dead' AND x.deleted_at IS NULL))`
	args := []any{}
	if user := currentUser(r); user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "scope_account")
		query += ` AND EXISTS (SELECT 1 FROM proxy_account_history scope_history
			JOIN accounts scope_account ON scope_account.id = scope_history.account_id
			WHERE scope_history.proxy_id = x.id AND ` + condition + `)`
		args = append(args, scopeArgs...)
	}
	query += ` ORDER BY x.deleted_at DESC, x.id DESC`
	rows, err := a.db.Query(query, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []proxyRecord{}
	for rows.Next() {
		item, err := scanProxy(rows)
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
	writeJSON(w, http.StatusOK, items)
}

func (a *app) handleProxyRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var input struct {
		PoolID int64 `json:"pool_id"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.PoolID <= 0 {
		writeError(w, http.StatusBadRequest, "target proxy pool is required")
		return
	}
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()
	var protocol, host, username, password, systemKind string
	var port int
	var deletedAt sql.NullString
	if err := tx.QueryRow(`SELECT x.protocol, x.host, x.port, x.username, x.password, p.system_kind, x.deleted_at
		FROM proxies x JOIN proxy_pools p ON p.id = x.pool_id WHERE x.id = ?`, id).Scan(&protocol, &host, &port, &username, &password, &systemKind, &deletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "archived proxy not found")
			return
		}
		writeDBError(w, err)
		return
	}
	if systemKind != deadProxyPoolKind && !deletedAt.Valid {
		writeError(w, http.StatusConflict, "proxy is not archived")
		return
	}
	var targetExists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM proxy_pools WHERE id = ? AND deleted_at IS NULL AND system_kind = '' AND status = 'active'`, input.PoolID).Scan(&targetExists); err != nil {
		writeDBError(w, err)
		return
	}
	if targetExists == 0 {
		writeError(w, http.StatusConflict, "target proxy pool is unavailable")
		return
	}
	var duplicate int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM proxies WHERE id != ? AND deleted_at IS NULL
		AND protocol = ? AND host = ? AND port = ? AND username = ? AND password = ?`, id, protocol, host, port, username, password).Scan(&duplicate); err != nil {
		writeDBError(w, err)
		return
	}
	if duplicate > 0 {
		writeError(w, http.StatusConflict, "the same proxy endpoint already exists in an active pool")
		return
	}
	if _, err := tx.Exec(`UPDATE proxies SET pool_id = ?, status = 'active', last_error = '', deleted_at = NULL,
		reuse_approved_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, input.PoolID, id); err != nil {
		writeDBError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	item, err := scanProxy(a.db.QueryRow(proxySelect+` WHERE x.id = ? AND x.deleted_at IS NULL AND p.system_kind = ''`, id))
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleProxyBatch(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PoolID          int64  `json:"pool_id"`
		Text            string `json:"text"`
		DefaultProtocol string `json:"default_protocol"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.PoolID <= 0 {
		writeError(w, http.StatusBadRequest, "select a proxy pool")
		return
	}
	var poolProtocol string
	if err := a.db.QueryRow(`SELECT default_protocol FROM proxy_pools
		WHERE id = ? AND status = 'active' AND deleted_at IS NULL AND system_kind = ''`, input.PoolID).Scan(&poolProtocol); err != nil {
		writeError(w, http.StatusBadRequest, "selected proxy pool is unavailable")
		return
	}
	if input.DefaultProtocol == "" {
		input.DefaultProtocol = poolProtocol
	}
	created, skipped, invalid, err := a.importProxyText(input.PoolID, input.DefaultProtocol, input.Text)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int{"created": created, "skipped": skipped, "invalid": invalid})
}

func (a *app) handleProxyBatchDelete(w http.ResponseWriter, r *http.Request) {
	var input proxyBatchDeleteInput
	if !decodeJSON(w, r, &input) {
		return
	}
	ids := uniquePositiveIDs(input.IDs, 501)
	if len(ids) == 0 || len(ids) > 500 {
		writeError(w, http.StatusBadRequest, "select between 1 and 500 proxies")
		return
	}
	result, err := a.deleteProxies(ids)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if result.Matched == 0 {
		writeError(w, http.StatusNotFound, "no selected proxies were found")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) importProxyText(poolID int64, defaultProtocol, text string) (created, skipped, invalid int, err error) {
	tx, err := a.db.Begin()
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback()
	scanner := bufio.NewScanner(strings.NewReader(text))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		item, parseErr := parseProxyLine(line, defaultProtocol)
		if parseErr != nil {
			invalid++
			continue
		}
		_, inserted, execErr := storeProxy(tx, poolID, item)
		if execErr != nil {
			return 0, 0, 0, execErr
		}
		if !inserted {
			skipped++
		} else {
			created++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, 0, 0, err
	}
	err = tx.Commit()
	return
}

func storeProxy(tx *databaseTx, poolID int64, item parsedProxy) (int64, bool, error) {
	var poolAvailable, singleUseEnabled int
	if err := tx.QueryRow(`SELECT COUNT(*), COALESCE(MAX(single_use_enabled), 1) FROM proxy_pools
		WHERE id = ? AND status = 'active' AND deleted_at IS NULL AND system_kind = ''`, poolID).Scan(&poolAvailable, &singleUseEnabled); err != nil {
		return 0, false, err
	}
	if poolAvailable == 0 {
		return 0, false, errors.New("selected proxy pool is unavailable")
	}
	if singleUseEnabled == 1 {
		var usedID int64
		err := tx.QueryRow(`SELECT used_proxy.id FROM proxies used_proxy
			WHERE used_proxy.protocol = ? AND used_proxy.host = ? AND used_proxy.port = ?
			AND used_proxy.username = ? AND used_proxy.password = ?
			AND (EXISTS (SELECT 1 FROM proxy_account_history used_history WHERE used_history.proxy_id = used_proxy.id)
				OR EXISTS (SELECT 1 FROM proxy_pools used_pool WHERE used_pool.id = used_proxy.pool_id AND used_pool.system_kind = ?))
			ORDER BY CASE WHEN used_proxy.deleted_at IS NULL THEN 0 ELSE 1 END, used_proxy.id LIMIT 1`,
			item.Protocol, item.Host, item.Port, item.Username, item.Password, deadProxyPoolKind).Scan(&usedID)
		if err == nil {
			return usedID, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
	}
	name := item.Host + ":" + strconv.Itoa(item.Port)
	result, err := tx.Exec(`INSERT OR IGNORE INTO proxies (pool_id, name, protocol, host, port, username, password) VALUES (?, ?, ?, ?, ?, ?, ?)`, poolID, name, item.Protocol, item.Host, item.Port, item.Username, item.Password)
	if err != nil {
		return 0, false, err
	}
	count, _ := result.RowsAffected()
	var id int64
	err = tx.QueryRow(`SELECT id FROM proxies WHERE pool_id = ? AND protocol = ? AND host = ? AND port = ? AND username = ? AND password = ? AND deleted_at IS NULL`, poolID, item.Protocol, item.Host, item.Port, item.Username, item.Password).Scan(&id)
	return id, count > 0, err
}

func (a *app) ensureProxyInPool(poolID *int64, value string) (*int64, error) {
	if poolID == nil || *poolID <= 0 {
		return nil, errors.New("select a proxy pool before entering a manual proxy")
	}
	var defaultProtocol string
	if err := a.db.QueryRow(`SELECT default_protocol FROM proxy_pools WHERE id = ? AND status = 'active' AND deleted_at IS NULL AND system_kind = ''`, *poolID).Scan(&defaultProtocol); err != nil {
		return nil, errors.New("selected proxy pool is unavailable")
	}
	item, err := parseProxyLine(value, defaultProtocol)
	if err != nil {
		return nil, errors.New("invalid proxy format")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	id, _, err := storeProxy(tx, *poolID, item)
	if err != nil {
		return nil, err
	}
	var status string
	if err := tx.QueryRow(`SELECT status FROM proxies WHERE id = ?`, id).Scan(&status); err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, errors.New("manual proxy already exists but is not active")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &id, nil
}

func parseProxyLine(line, defaultProtocol string) (parsedProxy, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return parsedProxy{}, errors.New("invalid proxy")
	}
	protocol := defaultProtocol
	authority := line
	if scheme, rest, ok := strings.Cut(line, "://"); ok {
		protocol = scheme
		authority = rest
		if item, err := parseImportedProxyURL(line); err == nil {
			return item, nil
		}
	}
	protocol, err := normalizeProxyProtocol(protocol)
	if err != nil {
		return parsedProxy{}, err
	}
	return parseProxyAuthority(authority, protocol)
}

func parseImportedProxyURL(raw string) (parsedProxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return parsedProxy{}, err
	}
	protocol, err := normalizeProxyProtocol(u.Scheme)
	if err != nil {
		return parsedProxy{}, err
	}
	port, err := parseProxyPort(u.Port())
	if err != nil || !validProxyHost(u.Hostname()) {
		return parsedProxy{}, errors.New("invalid proxy")
	}
	item := parsedProxy{Protocol: protocol, Host: u.Hostname(), Port: port}
	if u.User != nil {
		item.Username = u.User.Username()
		item.Password, _ = u.User.Password()
	}
	return item, nil
}

func parseProxyAuthority(authority, protocol string) (parsedProxy, error) {
	if host, port, ok := parseProxyEndpoint(authority); ok {
		return parsedProxy{Protocol: protocol, Host: host, Port: port}, nil
	}
	if strings.Count(authority, "@") == 1 {
		left, right, _ := strings.Cut(authority, "@")
		if host, port, ok := parseProxyEndpoint(right); ok {
			username, password, credentialsOK := parseProxyCredentials(left)
			if credentialsOK {
				return parsedProxy{Protocol: protocol, Host: host, Port: port, Username: username, Password: password}, nil
			}
		}
		if host, port, ok := parseProxyEndpoint(left); ok {
			username, password, credentialsOK := parseProxyCredentials(right)
			if credentialsOK {
				return parsedProxy{Protocol: protocol, Host: host, Port: port, Username: username, Password: password}, nil
			}
		}
	}
	parts := strings.Split(authority, ":")
	if len(parts) == 4 {
		hostFirst, hostFirstOK := proxyFromFourParts(parts[0], parts[1], parts[2], parts[3], protocol)
		userFirst, userFirstOK := proxyFromFourParts(parts[2], parts[3], parts[0], parts[1], protocol)
		switch {
		case hostFirstOK && !userFirstOK:
			return hostFirst, nil
		case userFirstOK && !hostFirstOK:
			return userFirst, nil
		case hostFirstOK && userFirstOK:
			if proxyHostScore(userFirst.Host) > proxyHostScore(hostFirst.Host) {
				return userFirst, nil
			}
			return hostFirst, nil
		}
	}
	return parsedProxy{}, errors.New("invalid proxy")
}

func normalizeProxyProtocol(protocol string) (string, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "socks5h" {
		protocol = "socks5"
	}
	if !validProxyProtocol(protocol) {
		return "", errors.New("invalid proxy")
	}
	return protocol, nil
}

func validProxyProtocol(protocol string) bool {
	switch protocol {
	case "socks5", "http", "https", sshProxyScheme:
		return true
	default:
		return false
	}
}

func parseProxyEndpoint(value string) (string, int, bool) {
	host, portText, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil || !validProxyHost(host) {
		return "", 0, false
	}
	port, err := parseProxyPort(portText)
	if err != nil {
		return "", 0, false
	}
	return host, port, true
}

func parseProxyPort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port <= 0 || port > 65535 {
		return 0, errors.New("invalid proxy port")
	}
	return port, nil
}

func validProxyHost(host string) bool {
	return host != "" && !strings.ContainsAny(host, " \t\r\n/@")
}

func parseProxyCredentials(value string) (string, string, bool) {
	username, password, ok := strings.Cut(value, ":")
	if !ok || strings.TrimSpace(username) == "" {
		return "", "", false
	}
	decodedUsername, err := url.PathUnescape(username)
	if err != nil {
		return "", "", false
	}
	decodedPassword, err := url.PathUnescape(password)
	if err != nil {
		return "", "", false
	}
	return decodedUsername, decodedPassword, true
}

func proxyFromFourParts(host, portText, username, password, protocol string) (parsedProxy, bool) {
	if !validProxyHost(host) {
		return parsedProxy{}, false
	}
	port, err := parseProxyPort(portText)
	if err != nil {
		return parsedProxy{}, false
	}
	username, password, ok := parseProxyCredentials(username + ":" + password)
	if !ok {
		return parsedProxy{}, false
	}
	return parsedProxy{Protocol: protocol, Host: host, Port: port, Username: username, Password: password}, true
}

func proxyHostScore(host string) int {
	withoutZone, _, _ := strings.Cut(host, "%")
	if net.ParseIP(withoutZone) != nil {
		return 4
	}
	if strings.EqualFold(host, "localhost") {
		return 3
	}
	if strings.Contains(host, ".") {
		return 2
	}
	if strings.Contains(host, "-") {
		return 1
	}
	return 0
}

func (a *app) handleProxyPoolSync(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var apiURL, headersJSON, protocol, sourceType string
	if err := a.db.QueryRow(`SELECT api_url, api_headers_json, default_protocol, source_type FROM proxy_pools WHERE id = ? AND deleted_at IS NULL AND system_kind = ''`, id).Scan(&apiURL, &headersJSON, &protocol, &sourceType); err != nil {
		writeError(w, http.StatusNotFound, "proxy pool not found")
		return
	}
	if sourceType != "api" {
		writeError(w, http.StatusConflict, "proxy pool is not configured for API sync")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	headers := map[string]string{}
	_ = json.Unmarshal([]byte(headersJSON), &headers)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.proxySyncFailed(id, err)
		writeError(w, http.StatusBadGateway, "proxy API request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err = fmt.Errorf("proxy API returned %s", resp.Status)
		a.proxySyncFailed(id, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	text := proxyTextFromAPI(body)
	created, skipped, invalid, err := a.importProxyText(id, protocol, text)
	if err != nil {
		a.proxySyncFailed(id, err)
		writeDBError(w, err)
		return
	}
	if _, err := a.db.Exec(`UPDATE proxy_pools SET last_sync_at = `+nowSQL+`, last_error = '', updated_at = `+nowSQL+` WHERE id = ?`, id); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"created": created, "skipped": skipped, "invalid": invalid})
}

func proxyTextFromAPI(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || (trimmed[0] != '[' && trimmed[0] != '{') {
		return string(body)
	}
	var value any
	if json.Unmarshal(trimmed, &value) != nil {
		return string(body)
	}
	lines := []string{}
	var walk func(any)
	walk = func(current any) {
		switch item := current.(type) {
		case string:
			if strings.Contains(item, ":") {
				lines = append(lines, item)
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		case map[string]any:
			if host, ok := item["host"].(string); ok {
				port := fmt.Sprint(item["port"])
				protocol, _ := item["protocol"].(string)
				username, _ := item["username"].(string)
				password, _ := item["password"].(string)
				prefix := ""
				if protocol != "" {
					prefix = protocol + "://"
				}
				auth := ""
				if username != "" {
					auth = url.UserPassword(username, password).String() + "@"
				}
				lines = append(lines, prefix+auth+net.JoinHostPort(host, port))
				return
			}
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(value)
	return strings.Join(lines, "\n")
}

func (a *app) proxySyncFailed(id int64, err error) {
	if _, updateErr := a.db.Exec(`UPDATE proxy_pools SET last_sync_at = `+nowSQL+`, last_error = ?, updated_at = `+nowSQL+` WHERE id = ?`, err.Error(), id); updateErr != nil {
		log.Printf("record proxy sync failure for pool %d: %v", id, updateErr)
	}
}

func (a *app) handleProxyUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var input struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Status != "active" && input.Status != "error" && input.Status != "disabled" {
		writeError(w, http.StatusBadRequest, "invalid proxy status")
		return
	}
	var result sql.Result
	var err error
	if input.Password == "" {
		result, err = a.db.Exec(`UPDATE proxies SET name = ?, status = ?, username = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL
			AND EXISTS (SELECT 1 FROM proxy_pools pool WHERE pool.id = proxies.pool_id AND pool.system_kind = '')`, strings.TrimSpace(input.Name), input.Status, strings.TrimSpace(input.Username), id)
	} else {
		result, err = a.db.Exec(`UPDATE proxies SET name = ?, status = ?, username = ?, password = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL
			AND EXISTS (SELECT 1 FROM proxy_pools pool WHERE pool.id = proxies.pool_id AND pool.system_kind = '')`, strings.TrimSpace(input.Name), input.Status, strings.TrimSpace(input.Username), input.Password, id)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "proxy not found")
		return
	}
	item, err := scanProxy(a.db.QueryRow(proxySelect+` WHERE x.id = ? AND x.deleted_at IS NULL AND p.system_kind = ''`, id))
	if err != nil {
		writeError(w, http.StatusNotFound, "proxy not found")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	result, err := a.deleteProxies([]int64{id})
	if err != nil {
		writeDBError(w, err)
		return
	}
	if result.Matched == 0 {
		writeError(w, http.StatusNotFound, "proxy not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"archived":            true,
		"deleted":             true,
		"reassigned_accounts": result.ReassignedAccounts,
		"paused_accounts":     result.PausedAccounts,
	})
}

func (a *app) deleteProxies(ids []int64) (proxyBatchDeleteResponse, error) {
	result := proxyBatchDeleteResponse{}
	tx, err := a.db.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}

	type assignedAccount struct {
		ID        int64
		Name      string
		PoolID    int64
		AutoProxy bool
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
		WHERE p.deleted_at IS NULL AND pool.system_kind = '' AND p.id IN (`+placeholders+`)`, args...).Scan(&result.Matched); err != nil {
		return result, err
	}
	if result.Matched == 0 {
		return result, nil
	}
	rows, err := tx.Query(`SELECT a.id, a.name, p.pool_id, a.auto_proxy
		FROM accounts a JOIN proxies p ON p.id = a.proxy_id JOIN proxy_pools pool ON pool.id = p.pool_id
		WHERE a.deleted_at IS NULL AND a.archived_at IS NULL AND pool.system_kind = '' AND a.proxy_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return result, err
	}
	assigned := []assignedAccount{}
	for rows.Next() {
		var item assignedAccount
		var autoProxy int
		if err := rows.Scan(&item.ID, &item.Name, &item.PoolID, &autoProxy); err != nil {
			rows.Close()
			return result, err
		}
		item.AutoProxy = autoProxy == 1
		assigned = append(assigned, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	updateResult, err := tx.Exec(`UPDATE proxies SET status = 'disabled', deleted_at = `+nowSQL+`, updated_at = `+nowSQL+`
		WHERE deleted_at IS NULL AND id IN (`+placeholders+`)
		AND EXISTS (SELECT 1 FROM proxy_pools pool WHERE pool.id = proxies.pool_id AND pool.system_kind = '')`, args...)
	if err != nil {
		return result, err
	}
	result.Deleted, err = updateResult.RowsAffected()
	if err != nil {
		return result, err
	}
	result.Archived = result.Deleted

	for _, account := range assigned {
		if account.AutoProxy {
			poolID := account.PoolID
			replacement, assignErr := assignAccountProxy(tx, account.ID, &poolID, nil, true)
			if assignErr == nil && replacement != nil {
				if _, err := tx.Exec(`UPDATE accounts SET proxy_id = ?, updated_at = `+nowSQL+` WHERE id = ?`, replacement, account.ID); err != nil {
					return result, err
				}
				if err := recordProxyAssignment(tx, *replacement, account.ID); err != nil {
					return result, err
				}
				result.ReassignedAccounts++
				continue
			}
		}
		message := "绑定代理已删除，请重新分配独享 IP"
		if _, err := tx.Exec(`UPDATE accounts SET proxy_id = NULL, schedulable = 0, error_message = ?, updated_at = `+nowSQL+` WHERE id = ?`, message, account.ID); err != nil {
			return result, err
		}
		result.PausedAccounts++
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func (a *app) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	result := a.probeProxy(r.Context(), id, "")
	if result.Error != "proxy not found or disabled" {
		if err := a.persistProxyTestResults([]proxyTestResult{result}); err != nil {
			writeDBError(w, err)
			return
		}
	}
	if !result.Success {
		status := http.StatusBadGateway
		if result.Error == "proxy not found or disabled" {
			status = http.StatusNotFound
		}
		writeError(w, status, result.Error)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) handleProxyBatchTest(w http.ResponseWriter, r *http.Request) {
	var input struct {
		PoolID      int64   `json:"pool_id"`
		IDs         []int64 `json:"ids"`
		Concurrency int     `json:"concurrency"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Concurrency <= 0 {
		input.Concurrency = 8
	}
	if input.Concurrency > 20 {
		input.Concurrency = 20
	}
	query := `SELECT p.id, p.name FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
		WHERE p.deleted_at IS NULL AND p.status != 'disabled' AND pool.system_kind = ''`
	query += ` AND ` + proxyNotQuarantinedPredicate("p")
	args := []any{}
	if input.PoolID > 0 {
		query += ` AND p.pool_id = ?`
		args = append(args, input.PoolID)
	}
	if len(input.IDs) > 0 {
		ids := uniquePositiveIDs(input.IDs, 501)
		if len(ids) == 0 || len(ids) > 500 {
			writeError(w, http.StatusBadRequest, "select between 1 and 500 proxies")
			return
		}
		query += ` AND p.id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += ` ORDER BY p.id`
	rows, err := a.db.Query(query, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	targets := []proxyTestResult{}
	for rows.Next() {
		var item proxyTestResult
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			rows.Close()
			writeDBError(w, err)
			return
		}
		targets = append(targets, item)
	}
	if err := rows.Close(); err != nil {
		writeDBError(w, err)
		return
	}
	if len(targets) == 0 {
		writeError(w, http.StatusBadRequest, "no active or error proxies to test")
		return
	}
	if len(targets) > 500 {
		writeError(w, http.StatusBadRequest, "batch proxy testing is limited to 500 proxies")
		return
	}
	items := make([]proxyTestResult, len(targets))
	jobs := make(chan int)
	var workers sync.WaitGroup
	workerCount := min(input.Concurrency, len(targets))
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				items[index] = a.probeProxy(r.Context(), targets[index].ID, targets[index].Name)
			}
		}()
	}
	for index := range targets {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	if err := a.persistProxyTestResults(items); err != nil {
		writeDBError(w, err)
		return
	}
	response := proxyBatchTestResponse{Total: len(items), Items: items}
	for _, item := range items {
		if item.Success {
			response.Success++
		} else {
			response.Failed++
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) probeProxy(parent context.Context, id int64, name string) proxyTestResult {
	result := proxyTestResult{ID: id, Name: name}
	proxyURL, err := a.proxyURLForTest(id)
	if err != nil {
		result.Error = "proxy not found or disabled"
		return result
	}
	client, err := clientForProxy(proxyURL)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	ctx, cancel := context.WithTimeout(parent, 12*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, proxyTestEndpoint, nil)
	started := time.Now()
	resp, err := client.Do(req)
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Error = "proxy test failed: " + err.Error()
		return result
	}
	defer resp.Body.Close()
	var payload struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || strings.TrimSpace(payload.IP) == "" {
		result.Error = "proxy test returned an invalid response"
		return result
	}
	result.Success = true
	result.IP = strings.TrimSpace(payload.IP)
	return result
}

func (a *app) persistProxyTestResults(items []proxyTestResult) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range items {
		if item.Error == "proxy not found or disabled" {
			continue
		}
		if item.Success {
			if _, err := tx.Exec(`UPDATE proxies SET status = 'active', exit_ip = ?, latency_ms = ?, last_test_at = `+nowSQL+`, last_error = '', updated_at = `+nowSQL+` WHERE id = ? AND status != 'disabled' AND deleted_at IS NULL`, item.IP, item.LatencyMS, item.ID); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`UPDATE proxies SET status = 'error', last_test_at = `+nowSQL+`, last_error = ?, updated_at = `+nowSQL+` WHERE id = ? AND status != 'disabled' AND deleted_at IS NULL`, sanitizeErrorMessage(item.Error), item.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) proxyURLForTest(id int64) (*url.URL, error) {
	var protocol, host, username, password string
	var port int
	if err := a.db.QueryRow(`SELECT p.protocol, p.host, p.port, p.username, p.password
		FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
		WHERE p.id = ? AND p.status != 'disabled' AND p.deleted_at IS NULL AND pool.system_kind = ''
		AND `+proxyNotQuarantinedPredicate("p"), id).Scan(&protocol, &host, &port, &username, &password); err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: protocol, Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if username != "" {
		u.User = url.UserPassword(username, password)
	}
	return u, nil
}

func (a *app) proxyURL(id int64) (*url.URL, error) {
	var protocol, host, username, password string
	var port int
	if err := a.db.QueryRow(`SELECT p.protocol, p.host, p.port, p.username, p.password
		FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
		WHERE p.id = ? AND p.status = 'active' AND p.deleted_at IS NULL
		AND pool.status = 'active' AND pool.deleted_at IS NULL AND pool.system_kind = ''
		AND `+proxyNotQuarantinedPredicate("p"), id).Scan(&protocol, &host, &port, &username, &password); err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: protocol, Host: net.JoinHostPort(host, strconv.Itoa(port))}
	if username != "" {
		u.User = url.UserPassword(username, password)
	}
	return u, nil
}

func (a *app) selectProxyForNewAccount(poolID, requestedProxyID *int64, auto bool) (*int64, string, error) {
	if poolID == nil {
		return nil, "", errors.New("select a proxy pool before authorizing a CCMAX account")
	}
	selected := requestedProxyID
	if !auto && selected == nil {
		return nil, "", errors.New("select a proxy or enable automatic matching")
	}
	if selected != nil {
		var available int
		err := a.db.QueryRow(`SELECT COUNT(*) FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
			WHERE p.id = ? AND p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
			AND pool.status = 'active' AND pool.deleted_at IS NULL AND pool.system_kind = ''
			AND `+proxyNotQuarantinedPredicate("p")+`
			AND (pool.single_use_enabled = 0 OR `+proxyIdentityUnusedPredicate("p")+`)
			AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.proxy_id = p.id AND a.deleted_at IS NULL)`, *selected, *poolID).Scan(&available)
		if err != nil {
			return nil, "", err
		}
		if available == 0 {
			if !auto {
				return nil, "", errors.New("selected proxy is unavailable or already assigned")
			}
			selected = nil
		}
	}
	if selected == nil {
		var id int64
		err := a.db.QueryRow(`SELECT p.id FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
			WHERE p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
			AND pool.status = 'active' AND pool.deleted_at IS NULL AND pool.system_kind = ''
			AND `+proxyNotQuarantinedPredicate("p")+`
			AND (pool.single_use_enabled = 0 OR `+proxyIdentityUnusedPredicate("p")+`)
			AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.proxy_id = p.id AND a.deleted_at IS NULL)
			ORDER BY CASE WHEN p.last_test_at IS NULL THEN 1 ELSE 0 END, p.latency_ms, p.id LIMIT 1`, *poolID).Scan(&id)
		if err != nil {
			return nil, "", errors.New("no unassigned active proxy is available in this pool")
		}
		selected = &id
	}
	proxyURL, err := a.proxyURL(*selected)
	if err != nil {
		return nil, "", errors.New("selected proxy is unavailable")
	}
	return selected, proxyURL.String(), nil
}

func clientForProxy(proxyURL *url.URL) (*http.Client, error) {
	responseHeaderTimeout := durationFromEnv("CCMAX_UPSTREAM_RESPONSE_HEADER_TIMEOUT", defaultUpstreamResponseHeaderTimeout)
	requestTimeout := durationFromEnv("CCMAX_UPSTREAM_REQUEST_TIMEOUT", defaultUpstreamRequestTimeout)
	key := upstreamProxyClientKey{responseHeaderTimeout: responseHeaderTimeout, requestTimeout: requestTimeout}
	if proxyURL != nil {
		key.proxyURLHash = hashToken(proxyURL.String())
	}
	if cached, ok := upstreamProxyClients.Load(key); ok {
		return cached.(*http.Client), nil
	}

	client, err := newClientForProxy(proxyURL, responseHeaderTimeout, requestTimeout)
	if err != nil {
		return nil, err
	}
	actual, loaded := upstreamProxyClients.LoadOrStore(key, client)
	if loaded {
		client.CloseIdleConnections()
		return actual.(*http.Client), nil
	}
	return client, nil
}

func durationFromEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

func newClientForProxy(proxyURL *url.URL, responseHeaderTimeout, requestTimeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	if proxyURL != nil {
		switch strings.ToLower(proxyURL.Scheme) {
		case "http", "https":
			transport.Proxy = http.ProxyURL(proxyURL)
		case "socks5", "socks5h":
			dialer, err := xproxy.FromURL(proxyURL, &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second})
			if err != nil {
				return nil, err
			}
			if contextDialer, ok := dialer.(xproxy.ContextDialer); ok {
				transport.DialContext = contextDialer.DialContext
			} else {
				transport.DialContext = func(_ context.Context, network, address string) (net.Conn, error) {
					return dialer.Dial(network, address)
				}
			}
		case sshProxyScheme:
			transport.DialContext = sshProxyDialContext(proxyURL)
		default:
			return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
		}
	}
	return &http.Client{Transport: decompressingRoundTripper{base: transport}, Timeout: requestTimeout}, nil
}

func assignAccountProxy(tx *databaseTx, accountID int64, poolID, requestedProxyID *int64, auto bool) (*int64, error) {
	if poolID == nil {
		return nil, nil
	}
	if !auto {
		if requestedProxyID == nil {
			return nil, nil
		}
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
			WHERE p.id = ? AND p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
			AND pool.status = 'active' AND pool.deleted_at IS NULL AND pool.system_kind = ''
			AND `+proxyNotQuarantinedPredicate("p")+`
			AND `+proxyAvailableToAccountPredicate("p", "pool"), *requestedProxyID, *poolID, accountID).Scan(&exists); err != nil || exists == 0 {
			return nil, errors.New("selected proxy is unavailable")
		}
		var exclusiveOwner int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM accounts WHERE proxy_id = ? AND id != ? AND deleted_at IS NULL`, *requestedProxyID, accountID).Scan(&exclusiveOwner); err != nil {
			return nil, err
		}
		if exclusiveOwner > 0 {
			return nil, errors.New("selected proxy is already assigned to another account")
		}
		return requestedProxyID, nil
	}
	if requestedProxyID != nil {
		var occupied int
		err := tx.QueryRow(`SELECT COUNT(*) FROM accounts WHERE proxy_id = ? AND id != ? AND deleted_at IS NULL`, *requestedProxyID, accountID).Scan(&occupied)
		if err == nil && occupied == 0 {
			var valid int
			_ = tx.QueryRow(`SELECT COUNT(*) FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
				WHERE p.id = ? AND p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
				AND pool.status = 'active' AND pool.deleted_at IS NULL AND pool.system_kind = ''
				AND `+proxyNotQuarantinedPredicate("p")+`
				AND `+proxyAvailableToAccountPredicate("p", "pool"), *requestedProxyID, *poolID, accountID).Scan(&valid)
			if valid > 0 {
				return requestedProxyID, nil
			}
		}
	}
	var id int64
	err := tx.QueryRow(`SELECT p.id FROM proxies p JOIN proxy_pools pool ON pool.id = p.pool_id
		WHERE p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
		AND pool.status = 'active' AND pool.deleted_at IS NULL AND pool.system_kind = ''
		AND `+proxyNotQuarantinedPredicate("p")+`
		AND `+proxyAvailableToAccountPredicate("p", "pool")+`
		AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.proxy_id = p.id AND a.id != ? AND a.deleted_at IS NULL)
		ORDER BY CASE WHEN p.last_test_at IS NULL THEN 1 ELSE 0 END, p.latency_ms, p.id LIMIT 1`, *poolID, accountID, accountID).Scan(&id)
	if err != nil {
		return nil, errors.New("no unassigned active proxy is available in this pool")
	}
	return &id, nil
}

func recordProxyAssignment(tx *databaseTx, proxyID, accountID int64) error {
	if tx.dialect == dialectMySQL {
		if _, err := tx.Exec(`INSERT INTO proxy_account_history
			(proxy_id, account_id, first_bound_at, last_bound_at, bind_count)
			VALUES (?, ?, `+nowSQL+`, `+nowSQL+`, 1)
			ON DUPLICATE KEY UPDATE
				last_bound_at = VALUES(last_bound_at),
				bind_count = proxy_account_history.bind_count + 1`, proxyID, accountID); err != nil {
			return err
		}
	}
	_, err := tx.Exec(`UPDATE proxies SET reuse_approved_at = NULL WHERE id = ?`, proxyID)
	return err
}
