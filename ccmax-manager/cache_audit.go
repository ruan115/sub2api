package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Prompt caching is a prefix match: any byte that changes before a cache
// breakpoint invalidates everything after it. `tools` and `system` render ahead
// of every message, and they are also where this gateway injects the Claude Code
// identity block, the billing fingerprint and the beta merge — so if any of that
// varies per request, we are the ones invalidating the cache, and no amount of
// account pinning or rate-limit accounting will fix it.
//
// The table is diagnostic and self-pruning, so it is deliberately left out of
// mysqlMigrationTables: carrying 7-day-retention rows across a cutover buys
// nothing and only risks a row-count mismatch failing the migration.
//
// This is deliberately observation-only. It never edits the request: the
// injections exist to keep requests indistinguishable from a real Claude Code
// client, and changing them risks the accounts themselves. Measure first, and
// only then decide whether anything is worth touching.
const cachePrefixAuditRetentionDays = 7

type cachePrefixAuditLog struct {
	ID                 int64  `json:"id"`
	SessionHash        string `json:"session_hash"`
	AccountID          *int64 `json:"account_id"`
	AccountName        string `json:"account_name"`
	Model              string `json:"model"`
	PrefixHash         string `json:"prefix_hash"`
	ToolsHash          string `json:"tools_hash"`
	SystemHash         string `json:"system_hash"`
	ChangedSegment     string `json:"changed_segment"`
	PreviousPrefixHash string `json:"previous_prefix_hash"`
	CreatedAt          string `json:"created_at"`
}

type cachePrefixAuditSummary struct {
	Total         int64 `json:"total"`
	Sessions      int64 `json:"sessions"`
	Initial       int64 `json:"initial"`
	ToolsChanged  int64 `json:"tools_changed"`
	SystemChanged int64 `json:"system_changed"`
	Accounts      int64 `json:"accounts"`
}

func (a *app) migrateCachePrefixAudit() error {
	// migrateSharedData runs for both dialects, but this DDL is SQLite-only:
	// AUTOINCREMENT and CREATE INDEX IF NOT EXISTS are parse errors on MySQL, and
	// a parse error is not saved by IF NOT EXISTS. MySQL already declares the
	// table in migrateMySQL, so there is nothing to do there.
	if a.db.dialect == dialectMySQL {
		return nil
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS cache_prefix_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_hash TEXT NOT NULL,
			account_id INTEGER,
			model TEXT NOT NULL DEFAULT '',
			prefix_hash TEXT NOT NULL,
			tools_hash TEXT NOT NULL,
			system_hash TEXT NOT NULL,
			changed_segment TEXT NOT NULL DEFAULT '',
			previous_prefix_hash TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (` + nowSQL + `)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_cache_prefix_session ON cache_prefix_events(session_hash, created_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_cache_prefix_created ON cache_prefix_events(created_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := a.db.Exec(statement); err != nil {
			return fmt.Errorf("create cache prefix audit: %w", err)
		}
	}
	return nil
}

type cachePrefixFingerprint struct {
	Prefix string
	Tools  string
	System string
}

// fingerprintCachePrefix hashes the parts of the outgoing body that render
// before any message: the tool definitions and the system prompt. Messages are
// excluded on purpose — a conversation growing is normal and does not
// invalidate the prefix, whereas a changed tool list or system block does.
func fingerprintCachePrefix(body []byte) cachePrefixFingerprint {
	digest := func(raw string) string {
		sum := sha256.Sum256([]byte(raw))
		return hex.EncodeToString(sum[:8])
	}
	tools := digest(gjson.GetBytes(body, "tools").Raw)
	system := digest(gjson.GetBytes(body, "system").Raw)
	return cachePrefixFingerprint{Prefix: digest(tools + ":" + system), Tools: tools, System: system}
}

// recordCachePrefixChange writes a row only when the prefix actually differs
// from the previous request on the same session, so a stable session costs one
// indexed read per request and nothing else.
func (a *app) recordCachePrefixChange(sessionHash string, accountID int64, model string, body []byte) {
	if sessionHash == "" || len(body) == 0 {
		return
	}
	current := fingerprintCachePrefix(body)
	var previousPrefix, previousTools, previousSystem string
	err := a.db.QueryRow(`SELECT prefix_hash, tools_hash, system_hash FROM cache_prefix_events
		WHERE session_hash = ? ORDER BY id DESC LIMIT 1`, sessionHash).Scan(&previousPrefix, &previousTools, &previousSystem)
	if err == nil && previousPrefix == current.Prefix {
		return
	}
	segment := "initial"
	if err == nil {
		switch {
		case previousTools != current.Tools && previousSystem != current.System:
			segment = "tools+system"
		case previousTools != current.Tools:
			segment = "tools"
		case previousSystem != current.System:
			segment = "system"
		default:
			segment = "unknown"
		}
	}
	_, writeErr := a.db.Exec(`INSERT INTO cache_prefix_events
		(session_hash, account_id, model, prefix_hash, tools_hash, system_hash, changed_segment, previous_prefix_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionHash, accountID, model, current.Prefix, current.Tools, current.System, segment, previousPrefix)
	logDatabaseWriteError("record cache prefix change", writeErr)
}

func (a *app) pruneCachePrefixEvents() {
	// The cutoff is computed here rather than in SQL: rewriteQuery only knows a
	// fixed set of strftime interval literals, and a day-scale one is not among
	// them. normalizeQueryArgs converts the timestamp for MySQL.
	cutoff := time.Now().UTC().Add(-cachePrefixAuditRetentionDays * 24 * time.Hour).Format(time.RFC3339Nano)
	_, err := a.db.Exec(`DELETE FROM cache_prefix_events WHERE created_at < ?`, cutoff)
	logDatabaseWriteError("prune cache prefix events", err)
}

func (a *app) handleCachePrefixAuditLogs(w http.ResponseWriter, r *http.Request) {
	if isScopedUserRole(currentUser(r).Role) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "无权查看跨账号缓存审计"})
		return
	}

	where := []string{"1 = 1"}
	args := []any{}
	if search := strings.TrimSpace(r.URL.Query().Get("search")); search != "" {
		like := "%" + search + "%"
		searchParts := []string{"cpe.session_hash LIKE ?", "cpe.model LIKE ?", "COALESCE(a.name, '') LIKE ?"}
		searchArgs := []any{like, like, like}
		if accountID, err := strconv.ParseInt(search, 10, 64); err == nil && accountID > 0 {
			searchParts = append(searchParts, "cpe.account_id = ?")
			searchArgs = append(searchArgs, accountID)
		}
		where = append(where, "("+strings.Join(searchParts, " OR ")+")")
		args = append(args, searchArgs...)
	}
	if segment := strings.TrimSpace(r.URL.Query().Get("segment")); segment != "" {
		allowed := map[string]bool{"initial": true, "tools": true, "system": true, "tools+system": true, "unknown": true}
		if !allowed[segment] {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "无效的变更区段"})
			return
		}
		where = append(where, "cpe.changed_segment = ?")
		args = append(args, segment)
	}
	if from := normalizeDateStart(r.URL.Query().Get("from")); from != "" {
		where = append(where, "cpe.created_at >= ?")
		args = append(args, from)
	}
	if to := normalizeDateEnd(r.URL.Query().Get("to")); to != "" {
		where = append(where, "cpe.created_at < ?")
		args = append(args, to)
	}

	clause := strings.Join(where, " AND ")
	summary := cachePrefixAuditSummary{}
	if err := a.db.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT cpe.session_hash),
		COALESCE(SUM(CASE WHEN cpe.changed_segment = 'initial' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN cpe.changed_segment IN ('tools', 'tools+system') THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN cpe.changed_segment IN ('system', 'tools+system') THEN 1 ELSE 0 END), 0),
		COUNT(DISTINCT cpe.account_id)
		FROM cache_prefix_events cpe LEFT JOIN accounts a ON a.id = cpe.account_id
		WHERE `+clause, args...).Scan(&summary.Total, &summary.Sessions, &summary.Initial, &summary.ToolsChanged, &summary.SystemChanged, &summary.Accounts); err != nil {
		writeDBError(w, err)
		return
	}

	page, pageSize, offset := paginationFromRequest(r, 20, 100)
	queryArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := a.db.Query(`SELECT cpe.id, cpe.session_hash, cpe.account_id, COALESCE(a.name, ''),
		cpe.model, cpe.prefix_hash, cpe.tools_hash, cpe.system_hash, cpe.changed_segment,
		cpe.previous_prefix_hash, cpe.created_at
		FROM cache_prefix_events cpe LEFT JOIN accounts a ON a.id = cpe.account_id
		WHERE `+clause+` ORDER BY cpe.id DESC LIMIT ? OFFSET ?`, queryArgs...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	items := []cachePrefixAuditLog{}
	for rows.Next() {
		var item cachePrefixAuditLog
		var accountID sql.NullInt64
		if err := rows.Scan(&item.ID, &item.SessionHash, &accountID, &item.AccountName, &item.Model, &item.PrefixHash, &item.ToolsHash, &item.SystemHash, &item.ChangedSegment, &item.PreviousPrefixHash, &item.CreatedAt); err != nil {
			writeDBError(w, err)
			return
		}
		if accountID.Valid {
			item.AccountID = &accountID.Int64
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": items, "summary": summary, "total": summary.Total,
		"page": page, "page_size": pageSize, "total_pages": totalPages(summary.Total, pageSize),
		"retention_days": cachePrefixAuditRetentionDays,
	})
}
