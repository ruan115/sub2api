package main

import (
	"net/http"
	"strings"
	"time"
)

const realtimeWindowSeconds = 60

type accountRealtimeLoad struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	RPM       int64  `json:"rpm"`
	TPM       int64  `json:"tpm"`
	Inflight  int64  `json:"inflight"`
	BaseRPM   int    `json:"base_rpm"`
	Eligible  bool   `json:"eligible"`
	Active    bool   `json:"active"`
}

type realtimeLoad struct {
	WindowSeconds    int                   `json:"window_seconds"`
	RPM              int64                 `json:"rpm"`
	TPM              int64                 `json:"tpm"`
	Inflight         int64                 `json:"inflight"`
	ActiveAccounts   int                   `json:"active_accounts"`
	EligibleAccounts int                   `json:"eligible_accounts"`
	RPMCapacity      int64                 `json:"rpm_capacity"`
	Unlimited        bool                  `json:"unlimited_capacity"`
	UpdatedAt        string                `json:"updated_at"`
	Accounts         []accountRealtimeLoad `json:"accounts"`
}

func (a *app) handleRealtimeStats(w http.ResponseWriter, r *http.Request) {
	where := []string{"a.deleted_at IS NULL"}
	args := []any{}
	user := currentUser(r)
	if user.Role == "user" {
		condition, scopeArgs := scopedAccountCondition(user, "a")
		where = append(where, condition)
		args = append(args, scopeArgs...)
	}
	if groupID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id"))); groupID == "a" || groupID == "b" {
		where = append(where, `EXISTS (SELECT 1 FROM account_groups filter_ag WHERE filter_ag.account_id = a.id AND filter_ag.group_id = ?)`)
		args = append(args, groupID)
	}

	query := `WITH recent_rpm AS (
		SELECT account_id, COUNT(*) AS rpm
		FROM account_rpm_events
		WHERE created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')
		GROUP BY account_id
	), recent_tokens AS (
		SELECT account_id, COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0) AS tpm
		FROM usage_logs
		WHERE created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')
		GROUP BY account_id
	)
	SELECT a.id, a.name, a.base_rpm, COALESCE(rr.rpm, 0), COALESCE(rt.tpm, 0), COALESCE(ai.requests, 0),
		CASE WHEN ` + accountStatePredicate("a", "normal") + ` THEN 1 ELSE 0 END
	FROM accounts a
	LEFT JOIN recent_rpm rr ON rr.account_id = a.id
	LEFT JOIN recent_tokens rt ON rt.account_id = a.id
	LEFT JOIN account_inflight ai ON ai.account_id = a.id
	WHERE ` + strings.Join(where, " AND ") + `
	ORDER BY COALESCE(rr.rpm, 0) DESC, COALESCE(ai.requests, 0) DESC, a.priority, a.id`

	rows, err := a.db.Query(query, args...)
	if err != nil {
		writeDBError(w, err)
		return
	}
	defer rows.Close()
	result := realtimeLoad{
		WindowSeconds: realtimeWindowSeconds,
		UpdatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		Accounts:      []accountRealtimeLoad{},
	}
	for rows.Next() {
		var item accountRealtimeLoad
		var eligible int
		if err := rows.Scan(&item.AccountID, &item.Name, &item.BaseRPM, &item.RPM, &item.TPM, &item.Inflight, &eligible); err != nil {
			writeDBError(w, err)
			return
		}
		item.Eligible = eligible == 1
		item.Active = item.RPM > 0 || item.Inflight > 0
		result.RPM += item.RPM
		result.TPM += item.TPM
		result.Inflight += item.Inflight
		if item.Active {
			result.ActiveAccounts++
		}
		if item.Eligible {
			result.EligibleAccounts++
			if item.BaseRPM > 0 {
				result.RPMCapacity += int64(item.BaseRPM)
			} else {
				result.Unlimited = true
			}
		}
		result.Accounts = append(result.Accounts, item)
	}
	if err := rows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
