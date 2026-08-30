package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const realtimeWindowSeconds = 60

type accountRealtimeLoad struct {
	AccountID            int64  `json:"account_id"`
	Name                 string `json:"name"`
	RPM                  int64  `json:"rpm"`
	TPM                  int64  `json:"tpm"`
	ITPM                 int64  `json:"itpm"`
	ITPMReserved         int64  `json:"itpm_reserved"`
	ITPMLoad             int64  `json:"itpm_load"`
	ITPMWindowSeconds    int    `json:"itpm_window_seconds"`
	ITPMSoftLimit        int64  `json:"itpm_soft_limit"`
	ITPMHardLimit        int64  `json:"itpm_hard_limit"`
	ITPMProtection       bool   `json:"itpm_protection_enabled"`
	ITPMRestricted       bool   `json:"itpm_restricted"`
	ITPMHardBlocked      bool   `json:"itpm_hard_blocked"`
	CacheReadTPM         int64  `json:"cache_read_tpm"`
	OTPM                 int64  `json:"otpm"`
	Inflight             int64  `json:"inflight"`
	Concurrency          int    `json:"concurrency"`
	BaseRPM              int    `json:"base_rpm"`
	EffectiveRPM         int    `json:"effective_rpm"`
	TemporaryRPM         int    `json:"temporary_rpm"`
	Eligible             bool   `json:"eligible"`
	Active               bool   `json:"active"`
	baseEligible         bool
	itpmLegacyLimit      int64
	itpmReservationIsBig bool
	extraJSON            string
	smooth               smoothColdStartConfig
}

type realtimeLoad struct {
	WindowSeconds       int                   `json:"window_seconds"`
	RPM                 int64                 `json:"rpm"`
	TPM                 int64                 `json:"tpm"`
	ITPM                int64                 `json:"itpm"`
	CacheReadTPM        int64                 `json:"cache_read_tpm"`
	OTPM                int64                 `json:"otpm"`
	Inflight            int64                 `json:"inflight"`
	ConcurrencyCapacity int64                 `json:"concurrency_capacity"`
	WaitingRequests     int64                 `json:"waiting_requests"`
	ActiveAccounts      int                   `json:"active_accounts"`
	EligibleAccounts    int                   `json:"eligible_accounts"`
	ITPMRestricted      int                   `json:"itpm_restricted_accounts"`
	ITPMHardBlocked     int                   `json:"itpm_hard_blocked_accounts"`
	RPMCapacity         int64                 `json:"rpm_capacity"`
	Unlimited           bool                  `json:"unlimited_capacity"`
	UpdatedAt           string                `json:"updated_at"`
	Accounts            []accountRealtimeLoad `json:"accounts"`
}

func (a *app) handleRealtimeStats(w http.ResponseWriter, r *http.Request) {
	where := []string{"a.deleted_at IS NULL", "a.archived_at IS NULL"}
	args := []any{}
	user := currentUser(r)
	if isScopedUserRole(user.Role) {
		condition, scopeArgs := scopedAccountCondition(user, "a")
		where = append(where, condition)
		args = append(args, scopeArgs...)
	}
	if user.Role != "admin" {
		where = append(where, accountStatePredicate("a", "normal"))
	}
	groupID := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("group_id")))
	if groupIDPattern.MatchString(groupID) {
		where = append(where, `EXISTS (SELECT 1 FROM account_groups filter_ag WHERE filter_ag.account_id = a.id AND filter_ag.group_id = ?)`)
		args = append(args, groupID)
	} else {
		groupID = ""
	}

	query := `WITH recent_rpm AS (
		SELECT account_id, COUNT(*) AS rpm
		FROM account_rpm_events
		WHERE created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')
		GROUP BY account_id
	), recent_tokens AS (
		-- Split by what upstream actually rate-limits: ITPM counts uncached input
		-- only, cache reads are excluded, and output has its own OTPM budget.
		-- The combined total is kept for context-throughput reporting.
		SELECT account_id,
			COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens), 0) AS tpm,
			COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tpm,
			COALESCE(SUM(output_tokens), 0) AS otpm
		FROM usage_logs
		WHERE created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')
		GROUP BY account_id
	)
	SELECT a.id, a.name, a.extra_json, a.base_rpm, COALESCE(rr.rpm, 0), COALESCE(rt.tpm, 0), 0, COALESCE(rt.cache_read_tpm, 0), COALESCE(rt.otpm, 0), COALESCE(ai.requests, 0), a.concurrency,
		COALESCE((SELECT MIN(ds.concurrency_limit)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?) AND ds.capacity_enabled = 1 AND ds.concurrency_limit > 0), 0),
		COALESCE((SELECT MIN(ds.rpm_limit)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?) AND ds.capacity_enabled = 1 AND ds.rpm_limit > 0), 0),
		COALESCE((SELECT MAX(ds.itpm_protection_enabled)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?)), 1),
		COALESCE((SELECT MAX(ds.itpm_window_seconds)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?)), 60),
		COALESCE((SELECT MIN(ds.itpm_soft_limit)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?)), 100000),
		COALESCE((SELECT MIN(ds.itpm_hard_limit)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?)), 150000),
		COALESCE((SELECT MIN(NULLIF(ds.itpm_limit, 0))
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?)), 0),
		COALESCE((SELECT MAX(ds.smooth_cold_start_enabled)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?)), 0),
		COALESCE((SELECT MIN(ds.smooth_cold_start_rpm)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?) AND ds.smooth_cold_start_enabled = 1), 8),
		COALESCE((SELECT MIN(ds.smooth_cold_start_tpm)
			FROM account_groups capacity_ag
			JOIN groups capacity_g ON capacity_g.id = capacity_ag.group_id
			JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, capacity_g.strategy_id) AND ds.deleted_at IS NULL
			WHERE capacity_ag.account_id = a.id AND (? = '' OR capacity_ag.group_id = ?) AND ds.smooth_cold_start_enabled = 1), 100000),
		CASE WHEN ` + accountStatePredicate("a", "normal") + ` THEN 1 ELSE 0 END,
		CASE WHEN a.rate_limit_downweight_until IS NOT NULL AND a.rate_limit_downweight_until > ` + nowSQL + `
			THEN COALESCE((SELECT rpm_limit FROM account_rpm_thresholds threshold WHERE threshold.account_id = a.id AND threshold.reset_at > ` + nowSQL + `), 0)
			ELSE 0 END
	FROM accounts a
	LEFT JOIN recent_rpm rr ON rr.account_id = a.id
	LEFT JOIN recent_tokens rt ON rt.account_id = a.id
	LEFT JOIN account_inflight ai ON ai.account_id = a.id
	WHERE ` + strings.Join(where, " AND ") + `
	ORDER BY COALESCE(rr.rpm, 0) DESC, COALESCE(ai.requests, 0) DESC, a.priority, a.id`

	queryArgs := append([]any{groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID, groupID}, args...)
	rows, err := a.db.Query(query, queryArgs...)
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
	itpmWindows := map[int64]int{}
	for rows.Next() {
		var item accountRealtimeLoad
		var eligible, strategyConcurrency, strategyRPM, itpmProtection, strategySmoothEnabled, strategySmoothRPM int
		var strategySmoothTPM int64
		if err := rows.Scan(&item.AccountID, &item.Name, &item.extraJSON, &item.BaseRPM, &item.RPM, &item.TPM, &item.ITPM, &item.CacheReadTPM, &item.OTPM, &item.Inflight, &item.Concurrency, &strategyConcurrency, &strategyRPM, &itpmProtection, &item.ITPMWindowSeconds, &item.ITPMSoftLimit, &item.ITPMHardLimit, &item.itpmLegacyLimit, &strategySmoothEnabled, &strategySmoothRPM, &strategySmoothTPM, &eligible, &item.TemporaryRPM); err != nil {
			writeDBError(w, err)
			return
		}
		item.ITPMProtection = itpmProtection == 1
		item.ITPMProtection, item.ITPMWindowSeconds, item.ITPMSoftLimit, item.ITPMHardLimit = normalizeStrategyITPMConfig(item.ITPMProtection, item.ITPMWindowSeconds, item.ITPMSoftLimit, item.ITPMHardLimit, item.itpmLegacyLimit)
		item.smooth = smoothColdStartFromExtra(item.extraJSON)
		if !item.smooth.Enabled && strategySmoothEnabled == 1 {
			item.smooth = smoothColdStartConfig{Enabled: true, RPM: strategySmoothRPM, TPM: strategySmoothTPM}
		}
		if item.smooth.Enabled {
			item.smooth.RPM, item.smooth.TPM = normalizeSmoothColdStartLimits(item.smooth.RPM, item.smooth.TPM)
			item.ITPMProtection = true
			item.ITPMWindowSeconds = defaultStrategyITPMWindowSeconds
			item.ITPMHardLimit = minPositiveInt64(item.ITPMHardLimit, item.smooth.TPM)
			item.ITPMSoftLimit = item.ITPMHardLimit
		}
		item.baseEligible = eligible == 1
		itpmWindows[item.AccountID] = item.ITPMWindowSeconds
		item.EffectiveRPM = item.BaseRPM
		// This mirrors gateway candidate resolution: an account-level strategy
		// wins over the group strategy through COALESCE above, while a zero RPM
		// does not force an override and therefore falls back to the account.
		if strategyRPM > 0 {
			item.EffectiveRPM = strategyRPM
		}
		if item.smooth.Enabled {
			item.EffectiveRPM = minPositiveInt(item.EffectiveRPM, item.smooth.RPM)
		}
		if strategyConcurrency > 0 && (item.Concurrency <= 0 || strategyConcurrency < item.Concurrency) {
			item.Concurrency = strategyConcurrency
		}
		if item.TemporaryRPM > 0 && (item.EffectiveRPM <= 0 || item.TemporaryRPM < item.EffectiveRPM) {
			item.EffectiveRPM = item.TemporaryRPM
		}
		result.Accounts = append(result.Accounts, item)
	}
	if err := rows.Err(); err != nil {
		writeDBError(w, err)
		return
	}
	rows.Close()
	itpmUsage, err := loadAccountITPMUsage(a.db, itpmWindows)
	if err != nil {
		writeDBError(w, err)
		return
	}
	accountIDs := make([]int64, 0, len(result.Accounts))
	for _, item := range result.Accounts {
		accountIDs = append(accountIDs, item.AccountID)
	}
	reservations, err := a.accountITPMReservationStatuses(accountIDs)
	if err != nil {
		writeDBError(w, err)
		return
	}
	for index := range result.Accounts {
		item := &result.Accounts[index]
		item.ITPM = itpmUsage[item.AccountID]
		reservation := reservations[item.AccountID]
		item.ITPMReserved = reservation.Tokens
		item.itpmReservationIsBig = reservation.Exclusive
		item.ITPMLoad = item.ITPM + item.ITPMReserved
		item.ITPMHardBlocked = item.ITPMProtection && item.ITPMLoad >= item.ITPMHardLimit
		item.ITPMRestricted = item.ITPMProtection && (item.itpmReservationIsBig || item.ITPMLoad >= item.ITPMSoftLimit)
		temporaryThresholdReached := item.TemporaryRPM > 0 && item.RPM >= int64(item.TemporaryRPM)
		item.Eligible = item.baseEligible && !temporaryThresholdReached && !item.ITPMRestricted
		item.Active = item.RPM > 0 || item.Inflight > 0
		result.RPM += item.RPM
		result.TPM += item.TPM
		result.ITPM += item.ITPMLoad
		result.CacheReadTPM += item.CacheReadTPM
		result.OTPM += item.OTPM
		result.Inflight += item.Inflight
		if item.Active {
			result.ActiveAccounts++
		}
		if item.Eligible {
			result.EligibleAccounts++
			if item.Concurrency > 0 {
				result.ConcurrencyCapacity += int64(item.Concurrency)
			}
			if item.EffectiveRPM > 0 {
				result.RPMCapacity += int64(item.EffectiveRPM)
			} else {
				result.Unlimited = true
			}
		}
		if item.ITPMRestricted {
			result.ITPMRestricted++
		}
		if item.ITPMHardBlocked {
			result.ITPMHardBlocked++
		}
	}
	result.WaitingRequests = a.realtimeCapacityWaiters(groupID, user)
	if user.Role != "admin" {
		writeJSON(w, http.StatusOK, realtimeForRestrictedView(result, user.AccountView))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) realtimeCapacityWaiters(groupID string, user panelUser) int64 {
	if groupID != "" {
		if isScopedUserRole(user.Role) && !userCanAccessGroup(user, groupID) {
			return 0
		}
		value, ok := a.capacityWaiters.Load(groupID)
		if !ok {
			return 0
		}
		return max(int64(0), atomic.LoadInt64(value.(*int64)))
	}
	var total int64
	a.capacityWaiters.Range(func(key, value any) bool {
		candidate, _ := key.(string)
		if !isScopedUserRole(user.Role) || userCanAccessGroup(user, candidate) {
			total += max(int64(0), atomic.LoadInt64(value.(*int64)))
		}
		return true
	})
	return total
}
