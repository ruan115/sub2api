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
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

type proxyPool struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	SourceType      string `json:"source_type"`
	APIURL          string `json:"api_url"`
	APIHeaders      string `json:"api_headers"`
	DefaultProtocol string `json:"default_protocol"`
	Status          string `json:"status"`
	ProxyCount      int    `json:"proxy_count"`
	AvailableCount  int    `json:"available_count"`
	AssignedCount   int    `json:"assigned_count"`
	LastSyncAt      string `json:"last_sync_at"`
	LastError       string `json:"last_error"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type proxyPoolInput struct {
	Name            string `json:"name"`
	SourceType      string `json:"source_type"`
	APIURL          string `json:"api_url"`
	APIHeaders      string `json:"api_headers"`
	DefaultProtocol string `json:"default_protocol"`
	Status          string `json:"status"`
}

type proxyRecord struct {
	ID          int64  `json:"id"`
	PoolID      int64  `json:"pool_id"`
	PoolName    string `json:"pool_name"`
	Name        string `json:"name"`
	Protocol    string `json:"protocol"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	PasswordSet bool   `json:"password_set"`
	Status      string `json:"status"`
	ExitIP      string `json:"exit_ip"`
	LatencyMS   *int64 `json:"latency_ms"`
	LastTestAt  string `json:"last_test_at"`
	AssignedTo  string `json:"assigned_to"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
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

var proxyTestEndpoint = "https://api.ipify.org?format=json"

func (a *app) migrateProxyFeatures() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS proxy_pools (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			source_type TEXT NOT NULL DEFAULT 'manual' CHECK (source_type IN ('manual', 'api')),
			api_url TEXT NOT NULL DEFAULT '',
			api_headers_json TEXT NOT NULL DEFAULT '{}',
			default_protocol TEXT NOT NULL DEFAULT 'socks5' CHECK (default_protocol IN ('socks5', 'http', 'https')),
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
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
			protocol TEXT NOT NULL CHECK (protocol IN ('socks5', 'http', 'https')),
			host TEXT NOT NULL,
			port INTEGER NOT NULL CHECK (port > 0 AND port < 65536),
			username TEXT NOT NULL DEFAULT '',
			password TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'error', 'disabled')),
			exit_ip TEXT NOT NULL DEFAULT '',
			latency_ms INTEGER,
			last_test_at TEXT,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
			deleted_at TEXT
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_proxy_unique ON proxies(pool_id, protocol, host, port, username, password) WHERE deleted_at IS NULL`,
		`CREATE INDEX IF NOT EXISTS idx_proxy_pool_status ON proxies(pool_id, status) WHERE deleted_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS account_rpm_events (
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_rpm ON account_rpm_events(account_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS dispatch_sessions (
			session_hash TEXT NOT NULL,
			api_key_id INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
			account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			expires_at TEXT NOT NULL,
			PRIMARY KEY (session_hash, api_key_id)
		)`,
		`CREATE TABLE IF NOT EXISTS account_inflight (
			account_id INTEGER PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
			requests INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0)
		)`,
	}
	for _, statement := range statements {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate proxy features: %w", err)
		}
	}
	// In-flight counters are process leases. A previous process cannot release
	// them after a crash or restart, so never carry them into this process.
	if _, err := a.db.Exec(`DELETE FROM account_inflight`); err != nil {
		return fmt.Errorf("reset stale account in-flight leases: %w", err)
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
	if _, err := a.db.Exec(`INSERT OR IGNORE INTO proxy_pools (id, name, source_type, default_protocol) VALUES (1, 'default', 'manual', 'socks5')`); err != nil {
		return err
	}
	return nil
}

func addColumnIfMissing(db *sql.DB, table, name, definition string) error {
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
	if input.Name == "" || (input.SourceType != "manual" && input.SourceType != "api") || (input.DefaultProtocol != "socks5" && input.DefaultProtocol != "http" && input.DefaultProtocol != "https") || (input.Status != "active" && input.Status != "disabled") {
		return errors.New("invalid proxy pool fields")
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

const proxyPoolSelect = `SELECT p.id, p.name, p.source_type, p.api_url, p.api_headers_json, p.default_protocol, p.status,
	(SELECT COUNT(*) FROM proxies x WHERE x.pool_id = p.id AND x.deleted_at IS NULL),
	(SELECT COUNT(*) FROM proxies x WHERE x.pool_id = p.id AND x.status = 'active' AND x.deleted_at IS NULL),
	(SELECT COUNT(DISTINCT a.id) FROM accounts a WHERE a.proxy_pool_id = p.id AND a.proxy_id IS NOT NULL AND a.deleted_at IS NULL),
	p.last_sync_at, p.last_error, p.created_at, p.updated_at FROM proxy_pools p`

func scanProxyPool(row scanner) (proxyPool, error) {
	var item proxyPool
	var lastSync sql.NullString
	err := row.Scan(&item.ID, &item.Name, &item.SourceType, &item.APIURL, &item.APIHeaders, &item.DefaultProtocol, &item.Status, &item.ProxyCount, &item.AvailableCount, &item.AssignedCount, &lastSync, &item.LastError, &item.CreatedAt, &item.UpdatedAt)
	item.LastSyncAt = nullText(lastSync)
	return item, err
}

func (a *app) handleProxyPools(w http.ResponseWriter, r *http.Request) {
	query := proxyPoolSelect + ` WHERE p.deleted_at IS NULL`
	args := []any{}
	user := currentUser(r)
	if user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "scope_account")
		query += ` AND EXISTS (SELECT 1 FROM accounts scope_account WHERE scope_account.proxy_pool_id = p.id AND scope_account.deleted_at IS NULL AND ` + condition + `)`
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
	result, err := a.db.Exec(`INSERT INTO proxy_pools (name, source_type, api_url, api_headers_json, default_protocol, status) VALUES (?, ?, ?, ?, ?, ?)`, input.Name, input.SourceType, input.APIURL, input.APIHeaders, input.DefaultProtocol, input.Status)
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
	_, err := a.db.Exec(`UPDATE proxy_pools SET name = ?, source_type = ?, api_url = ?, api_headers_json = ?, default_protocol = ?, status = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, input.Name, input.SourceType, input.APIURL, input.APIHeaders, input.DefaultProtocol, input.Status, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	item, err := scanProxyPool(a.db.QueryRow(proxyPoolSelect+` WHERE p.id = ? AND p.deleted_at IS NULL`, id))
	if err != nil {
		writeError(w, http.StatusNotFound, "proxy pool not found")
		return
	}
	writeJSON(w, http.StatusOK, item)
}

func (a *app) handleProxyPoolDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var assigned int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE proxy_pool_id = ? AND deleted_at IS NULL`, id).Scan(&assigned)
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
	if err := tx.QueryRow(`SELECT name FROM proxy_pools WHERE id = ? AND deleted_at IS NULL`, id).Scan(&name); err != nil {
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
		"deleted":         true,
		"name":            name,
		"deleted_proxies": deletedProxies,
	})
}

const proxySelect = `SELECT x.id, x.pool_id, p.name, x.name, x.protocol, x.host, x.port, x.username, x.password != '', x.status, x.exit_ip, x.latency_ms, x.last_test_at,
	COALESCE((SELECT GROUP_CONCAT(a.name, ', ') FROM accounts a WHERE a.proxy_id = x.id AND a.deleted_at IS NULL), ''), x.created_at, x.updated_at
	FROM proxies x JOIN proxy_pools p ON p.id = x.pool_id`

func scanProxy(row scanner) (proxyRecord, error) {
	var item proxyRecord
	var latency sql.NullInt64
	var lastTest sql.NullString
	err := row.Scan(&item.ID, &item.PoolID, &item.PoolName, &item.Name, &item.Protocol, &item.Host, &item.Port, &item.Username, &item.PasswordSet, &item.Status, &item.ExitIP, &latency, &lastTest, &item.AssignedTo, &item.CreatedAt, &item.UpdatedAt)
	item.LatencyMS = nullIntPointer(latency)
	item.LastTestAt = nullText(lastTest)
	return item, err
}

func (a *app) handleProxies(w http.ResponseWriter, r *http.Request) {
	query := proxySelect + ` WHERE x.deleted_at IS NULL`
	args := []any{}
	if user := currentUser(r); user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "scope_account")
		query += ` AND EXISTS (SELECT 1 FROM accounts scope_account WHERE scope_account.proxy_id = x.id AND scope_account.deleted_at IS NULL AND ` + condition + `)`
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
	if input.DefaultProtocol == "" {
		_ = a.db.QueryRow(`SELECT default_protocol FROM proxy_pools WHERE id = ? AND deleted_at IS NULL`, input.PoolID).Scan(&input.DefaultProtocol)
	}
	created, skipped, invalid, err := a.importProxyText(input.PoolID, input.DefaultProtocol, input.Text)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int{"created": created, "skipped": skipped, "invalid": invalid})
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

func storeProxy(tx *sql.Tx, poolID int64, item parsedProxy) (int64, bool, error) {
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
	if err := a.db.QueryRow(`SELECT default_protocol FROM proxy_pools WHERE id = ? AND status = 'active' AND deleted_at IS NULL`, *poolID).Scan(&defaultProtocol); err != nil {
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
	if protocol != "socks5" && protocol != "http" && protocol != "https" {
		return "", errors.New("invalid proxy")
	}
	return protocol, nil
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
	if err := a.db.QueryRow(`SELECT api_url, api_headers_json, default_protocol, source_type FROM proxy_pools WHERE id = ? AND deleted_at IS NULL`, id).Scan(&apiURL, &headersJSON, &protocol, &sourceType); err != nil {
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
	_, _ = a.db.Exec(`UPDATE proxy_pools SET last_sync_at = `+nowSQL+`, last_error = '', updated_at = `+nowSQL+` WHERE id = ?`, id)
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
	_, _ = a.db.Exec(`UPDATE proxy_pools SET last_sync_at = `+nowSQL+`, last_error = ?, updated_at = `+nowSQL+` WHERE id = ?`, err.Error(), id)
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
	if input.Password == "" {
		_, _ = a.db.Exec(`UPDATE proxies SET name = ?, status = ?, username = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, strings.TrimSpace(input.Name), input.Status, strings.TrimSpace(input.Username), id)
	} else {
		_, _ = a.db.Exec(`UPDATE proxies SET name = ?, status = ?, username = ?, password = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, strings.TrimSpace(input.Name), input.Status, strings.TrimSpace(input.Username), input.Password, id)
	}
	item, err := scanProxy(a.db.QueryRow(proxySelect+` WHERE x.id = ? AND x.deleted_at IS NULL`, id))
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
	tx, err := a.db.Begin()
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer tx.Rollback()

	var poolID int64
	if err := tx.QueryRow(`SELECT pool_id FROM proxies WHERE id = ? AND deleted_at IS NULL`, id).Scan(&poolID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "proxy not found")
			return
		}
		writeDBError(w, err)
		return
	}
	type assignedAccount struct {
		ID        int64
		Name      string
		AutoProxy bool
	}
	rows, err := tx.Query(`SELECT id, name, auto_proxy FROM accounts WHERE proxy_id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	assigned := []assignedAccount{}
	for rows.Next() {
		var item assignedAccount
		var autoProxy int
		if err := rows.Scan(&item.ID, &item.Name, &autoProxy); err != nil {
			rows.Close()
			writeDBError(w, err)
			return
		}
		item.AutoProxy = autoProxy == 1
		assigned = append(assigned, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		writeDBError(w, err)
		return
	}
	if err := rows.Close(); err != nil {
		writeDBError(w, err)
		return
	}
	if _, err := tx.Exec(`UPDATE proxies SET status = 'disabled', deleted_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, id); err != nil {
		writeDBError(w, err)
		return
	}

	reassigned, paused := 0, 0
	for _, account := range assigned {
		if account.AutoProxy {
			replacement, assignErr := assignAccountProxy(tx, account.ID, &poolID, nil, true)
			if assignErr == nil && replacement != nil {
				if _, err := tx.Exec(`UPDATE accounts SET proxy_id = ?, updated_at = `+nowSQL+` WHERE id = ?`, replacement, account.ID); err != nil {
					writeDBError(w, err)
					return
				}
				reassigned++
				continue
			}
		}
		message := "绑定代理已删除，请重新分配独享 IP"
		if _, err := tx.Exec(`UPDATE accounts SET proxy_id = NULL, schedulable = 0, error_message = ?, updated_at = `+nowSQL+` WHERE id = ?`, message, account.ID); err != nil {
			writeDBError(w, err)
			return
		}
		paused++
	}
	if err := tx.Commit(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":             true,
		"reassigned_accounts": reassigned,
		"paused_accounts":     paused,
	})
}

func (a *app) handleProxyTest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	result := a.testProxy(r.Context(), id, "")
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
	query := `SELECT id, name FROM proxies WHERE deleted_at IS NULL AND status != 'disabled'`
	args := []any{}
	if input.PoolID > 0 {
		query += ` AND pool_id = ?`
		args = append(args, input.PoolID)
	}
	if len(input.IDs) > 0 {
		ids := uniquePositiveIDs(input.IDs, 501)
		if len(ids) == 0 || len(ids) > 500 {
			writeError(w, http.StatusBadRequest, "select between 1 and 500 proxies")
			return
		}
		query += ` AND id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	query += ` ORDER BY id`
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
				items[index] = a.testProxy(r.Context(), targets[index].ID, targets[index].Name)
			}
		}()
	}
	for index := range targets {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
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

func (a *app) testProxy(parent context.Context, id int64, name string) proxyTestResult {
	result := proxyTestResult{ID: id, Name: name}
	proxyURL, err := a.proxyURLForTest(id)
	if err != nil {
		result.Error = "proxy not found or disabled"
		return result
	}
	client, err := clientForProxy(proxyURL)
	if err != nil {
		result.Error = err.Error()
		a.markProxyTestFailed(id)
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
		a.markProxyTestFailed(id)
		return result
	}
	defer resp.Body.Close()
	var payload struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil || strings.TrimSpace(payload.IP) == "" {
		result.Error = "proxy test returned an invalid response"
		a.markProxyTestFailed(id)
		return result
	}
	result.Success = true
	result.IP = strings.TrimSpace(payload.IP)
	_, _ = a.db.Exec(`UPDATE proxies SET status = 'active', exit_ip = ?, latency_ms = ?, last_test_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, result.IP, result.LatencyMS, id)
	return result
}

func (a *app) markProxyTestFailed(id int64) {
	_, _ = a.db.Exec(`UPDATE proxies SET status = 'error', last_test_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, id)
}

func (a *app) proxyURLForTest(id int64) (*url.URL, error) {
	var protocol, host, username, password string
	var port int
	if err := a.db.QueryRow(`SELECT protocol, host, port, username, password FROM proxies WHERE id = ? AND status != 'disabled' AND deleted_at IS NULL`, id).Scan(&protocol, &host, &port, &username, &password); err != nil {
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
	if err := a.db.QueryRow(`SELECT protocol, host, port, username, password FROM proxies WHERE id = ? AND status = 'active' AND deleted_at IS NULL`, id).Scan(&protocol, &host, &port, &username, &password); err != nil {
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
		err := a.db.QueryRow(`SELECT COUNT(*) FROM proxies p WHERE p.id = ? AND p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
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
		err := a.db.QueryRow(`SELECT p.id FROM proxies p WHERE p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
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
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 60 * time.Second}
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
		default:
			return nil, fmt.Errorf("unsupported proxy scheme: %s", proxyURL.Scheme)
		}
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Minute}, nil
}

func assignAccountProxy(tx *sql.Tx, accountID int64, poolID, requestedProxyID *int64, auto bool) (*int64, error) {
	if poolID == nil {
		return nil, nil
	}
	if !auto {
		if requestedProxyID == nil {
			return nil, nil
		}
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM proxies WHERE id = ? AND pool_id = ? AND status = 'active' AND deleted_at IS NULL`, *requestedProxyID, *poolID).Scan(&exists); err != nil || exists == 0 {
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
			_ = tx.QueryRow(`SELECT COUNT(*) FROM proxies WHERE id = ? AND pool_id = ? AND status = 'active' AND deleted_at IS NULL`, *requestedProxyID, *poolID).Scan(&valid)
			if valid > 0 {
				return requestedProxyID, nil
			}
		}
	}
	var id int64
	err := tx.QueryRow(`SELECT p.id FROM proxies p WHERE p.pool_id = ? AND p.status = 'active' AND p.deleted_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM accounts a WHERE a.proxy_id = p.id AND a.id != ? AND a.deleted_at IS NULL)
		ORDER BY CASE WHEN p.last_test_at IS NULL THEN 1 ELSE 0 END, p.latency_ms, p.id LIMIT 1`, *poolID, accountID).Scan(&id)
	if err != nil {
		return nil, errors.New("no unassigned active proxy is available in this pool")
	}
	return &id, nil
}
