package main

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Buckets for why an account lost its authorization. The reason text is written
// by markAccountReauthIfRefreshTokenCurrent and never rewritten, so an event
// keeps its original attribution even after the account is re-authorized.
const (
	deauthCauseOAuth401    = "oauth_401"
	deauthCauseForbidden   = "forbidden"
	deauthCauseRefresh     = "refresh_failed"
	deauthCauseActionNeed  = "action_required"
	deauthCauseNoCredetial = "credentials_missing"
	deauthCauseManual      = "manual"
	deauthCauseOther       = "other"
)

var deauthCauseLabels = map[string]string{
	deauthCauseOAuth401:    "401 掉授权",
	deauthCauseForbidden:   "403 被拒绝",
	deauthCauseRefresh:     "刷新令牌失败",
	deauthCauseActionNeed:  "需上游处理",
	deauthCauseNoCredetial: "缺少凭证",
	deauthCauseManual:      "人工置为待授权",
	deauthCauseOther:       "其他原因",
}

// deauthCauseOrder keeps the response deterministic and puts the buckets the
// operator cares about most first.
var deauthCauseOrder = []string{
	deauthCauseOAuth401,
	deauthCauseRefresh,
	deauthCauseForbidden,
	deauthCauseActionNeed,
	deauthCauseNoCredetial,
	deauthCauseManual,
	deauthCauseOther,
}

// accountInvalidationCause maps a recorded invalidation reason to a bucket. A
// 401 wins over the other patterns because "token was rejected" and "upstream
// authentication failed" are both produced by the 401 handling path.
func accountInvalidationCause(reason string) string {
	text := strings.ToLower(strings.TrimSpace(reason))
	switch {
	case text == "":
		return deauthCauseOther
	case strings.Contains(text, "401"),
		strings.Contains(text, "upstream authentication failed"),
		strings.Contains(text, "access token was rejected"),
		strings.Contains(text, "access token has been revoked"),
		strings.Contains(text, "access token was revoked"),
		strings.Contains(text, "invalid_grant"),
		strings.Contains(text, "account_on_hold"):
		return deauthCauseOAuth401
	case strings.Contains(text, "403"), strings.Contains(text, "forbidden"):
		return deauthCauseForbidden
	case strings.Contains(text, "account action required"), strings.Contains(text, "identity verification"):
		return deauthCauseActionNeed
	case strings.Contains(text, "token refresh failed"):
		return deauthCauseRefresh
	case strings.Contains(text, "no oauth access token"), strings.Contains(text, "no refresh token"):
		return deauthCauseNoCredetial
	case strings.Contains(text, "手动"), strings.Contains(text, "等待重新授权"):
		return deauthCauseManual
	default:
		return deauthCauseOther
	}
}

type deauthCauseCount struct {
	Cause string `json:"cause"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type deauthEvent struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	GroupIDs  string `json:"group_ids"`
	Cause     string `json:"cause"`
	Label     string `json:"label"`
	Reason    string `json:"reason"`
	At        string `json:"at"`
	Recovered bool   `json:"recovered"`
}

type deauthStats struct {
	WindowMinutes int                `json:"window_minutes"`
	Total         int                `json:"total"`
	OAuth401      int                `json:"oauth_401"`
	Accounts401   int                `json:"accounts_401"`
	Recovered401  int                `json:"recovered_401"`
	PendingReauth int                `json:"pending_reauth"`
	Causes        []deauthCauseCount `json:"causes"`
	Events        []deauthEvent      `json:"events"`
}

// deauthWindowMinutes clamps the requested window to a supported range so a
// hand-edited query string cannot force a full-table scan.
func deauthWindowMinutes(value string) int {
	minutes, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || minutes <= 0 {
		return 60
	}
	if minutes < 5 {
		return 5
	}
	if minutes > 10080 {
		return 10080
	}
	return minutes
}

// handleAuthorizationDeauth counts accounts that lost their authorization
// inside a rolling window, split by cause, so the authorization page can show
// how many accounts a 401 wave just took out.
func (a *app) handleAuthorizationDeauth(w http.ResponseWriter, r *http.Request) {
	window := deauthWindowMinutes(r.URL.Query().Get("window"))
	scope, scopeArgs := scopedAccountCondition(currentUser(r), "a")
	cutoff := time.Now().UTC().Add(-time.Duration(window) * time.Minute).Format(time.RFC3339Nano)
	args := append([]any{cutoff}, scopeArgs...)
	rows, err := a.db.Query(`SELECT e.account_id, a.name, e.reason, e.created_at,
		CASE WHEN a.invalidated_at IS NULL THEN 1 ELSE 0 END AS recovered,
		COALESCE((SELECT GROUP_CONCAT(ag.group_id, ',') FROM account_groups ag WHERE ag.account_id = a.id), '') AS group_ids
		FROM account_lifecycle_events e JOIN accounts a ON a.id = e.account_id
		WHERE e.event_type = 'invalidated' AND a.deleted_at IS NULL
			AND e.created_at >= ?
		AND `+scope+`
		ORDER BY e.created_at DESC, e.id DESC`, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	result := deauthStats{WindowMinutes: window, Events: []deauthEvent{}}
	counts := map[string]int{}
	// Recovery is a per-account state, so both tallies are de-duplicated by
	// account: one account can be knocked out more than once per window.
	distinct401 := map[int64]bool{}
	recovered401 := map[int64]bool{}
	for rows.Next() {
		var item deauthEvent
		var reason sql.NullString
		var recovered int
		if err := rows.Scan(&item.AccountID, &item.Name, &reason, &item.At, &recovered, &item.GroupIDs); err != nil {
			writeDBError(w, err)
			return
		}
		item.Reason = strings.TrimSpace(reason.String)
		item.Cause = accountInvalidationCause(item.Reason)
		item.Label = deauthCauseLabels[item.Cause]
		item.Recovered = recovered == 1
		counts[item.Cause]++
		result.Total++
		if item.Cause == deauthCauseOAuth401 {
			result.OAuth401++
			distinct401[item.AccountID] = true
			if item.Recovered {
				recovered401[item.AccountID] = true
			}
		}
		if len(result.Events) < 50 {
			result.Events = append(result.Events, item)
		}
	}
	if err := rows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	result.Accounts401 = len(distinct401)
	result.Recovered401 = len(recovered401)
	result.Causes = make([]deauthCauseCount, 0, len(deauthCauseOrder))
	for _, cause := range deauthCauseOrder {
		result.Causes = append(result.Causes, deauthCauseCount{Cause: cause, Label: deauthCauseLabels[cause], Count: counts[cause]})
	}
	pendingArgs := append([]any{}, scopeArgs...)
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts a
		WHERE a.deleted_at IS NULL AND a.archived_at IS NULL AND a.auth_status = 'reauth_required'
		AND `+scope, pendingArgs...).Scan(&result.PendingReauth); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
