package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// dispatchModes lists every accepted dispatch_mode. "" inherits the group's own
// dispatch behaviour.
//
//	serial       fill the busiest account first, spill to the next when it caps
//	balance      spread requests to the least loaded account
//	round_robin  even RPM distribution across all bound accounts
//	concentrated serve from a minimal active set; hold the rest as 待调度 and
//	             promote one more only when every active account is capped
var dispatchModes = []string{"", "serial", "balance", "round_robin", "concentrated"}

// dispatchModeCheckList renders dispatchModes for a SQLite CHECK constraint.
const dispatchModeCheckList = `'', 'serial', 'balance', 'round_robin', 'concentrated'`

// dispatchModesQueueWhenFull are the modes that park requests instead of
// rejecting them once every bound account is capped. They govern how many
// accounts are in play, so failing fast would defeat the policy.
var dispatchModesQueueWhenFull = []string{"round_robin", "concentrated"}

const (
	defaultStrategyITPMProtectionEnabled       = true
	defaultStrategyITPMWindowSeconds           = 60
	defaultStrategyITPMSoftLimit         int64 = 100_000
	defaultStrategyITPMHardLimit         int64 = 150_000
	defaultSmoothColdStartRPM                  = 8
	defaultSmoothColdStartTPM            int64 = 100_000
	minStrategyITPMWindowSeconds               = 1
	maxStrategyITPMWindowSeconds               = 3600
)

func validDispatchMode(mode string) bool {
	for _, candidate := range dispatchModes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// dispatchStrategy is a reusable limit-and-algorithm profile. Groups and
// accounts bind to one strategy; an account-level binding overrides the
// group-level one. Zero limits mean "not limited by this strategy".
type dispatchStrategy struct {
	ID                     int64  `json:"id"`
	Name                   string `json:"name"`
	Description            string `json:"description"`
	RPMLimit               int    `json:"rpm_limit"`
	TPMLimit               int64  `json:"tpm_limit"`
	ITPMLimit              int64  `json:"itpm_limit"`
	ITPMProtectionEnabled  bool   `json:"itpm_protection_enabled"`
	ITPMWindowSeconds      int    `json:"itpm_window_seconds"`
	ITPMSoftLimit          int64  `json:"itpm_soft_limit"`
	ITPMHardLimit          int64  `json:"itpm_hard_limit"`
	ConcurrencyLimit       int    `json:"concurrency_limit"`
	RPMStrategy            string `json:"rpm_strategy"`
	RPMStickyBuffer        int    `json:"rpm_sticky_buffer"`
	DispatchMode           string `json:"dispatch_mode"`
	CapacityEnabled        bool   `json:"capacity_enabled"`
	DispatchPacing         string `json:"dispatch_pacing"`
	PacingConcurrency      int    `json:"pacing_concurrency"`
	PacingIntervalSeconds  int    `json:"pacing_interval_seconds"`
	SmoothColdStartEnabled bool   `json:"smooth_cold_start_enabled"`
	SmoothColdStartRPM     int    `json:"smooth_cold_start_rpm"`
	SmoothColdStartTPM     int64  `json:"smooth_cold_start_tpm"`
	BoundGroups            int    `json:"bound_groups"`
	BoundAccounts          int    `json:"bound_accounts"`
	CreatedAt              string `json:"created_at"`
	UpdatedAt              string `json:"updated_at"`
}

type dispatchStrategyInput struct {
	Name                   string `json:"name"`
	Description            string `json:"description"`
	RPMLimit               int    `json:"rpm_limit"`
	TPMLimit               int64  `json:"tpm_limit"`
	ITPMLimit              int64  `json:"itpm_limit"`
	ITPMProtectionEnabled  *bool  `json:"itpm_protection_enabled"`
	ITPMWindowSeconds      *int   `json:"itpm_window_seconds"`
	ITPMSoftLimit          *int64 `json:"itpm_soft_limit"`
	ITPMHardLimit          *int64 `json:"itpm_hard_limit"`
	ConcurrencyLimit       int    `json:"concurrency_limit"`
	RPMStrategy            string `json:"rpm_strategy"`
	RPMStickyBuffer        int    `json:"rpm_sticky_buffer"`
	DispatchMode           string `json:"dispatch_mode"`
	CapacityEnabled        *bool  `json:"capacity_enabled"`
	DispatchPacing         string `json:"dispatch_pacing"`
	PacingConcurrency      int    `json:"pacing_concurrency"`
	PacingIntervalSeconds  int    `json:"pacing_interval_seconds"`
	SmoothColdStartEnabled *bool  `json:"smooth_cold_start_enabled"`
	SmoothColdStartRPM     *int   `json:"smooth_cold_start_rpm"`
	SmoothColdStartTPM     *int64 `json:"smooth_cold_start_tpm"`
}

func (input *dispatchStrategyInput) validate() error {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return errors.New("strategy name is required")
	}
	if input.RPMLimit < 0 || input.TPMLimit < 0 || input.ITPMLimit < 0 || input.ConcurrencyLimit < 0 || input.RPMStickyBuffer < 0 {
		return errors.New("strategy limits cannot be negative")
	}
	if input.ITPMWindowSeconds != nil && (*input.ITPMWindowSeconds < minStrategyITPMWindowSeconds || *input.ITPMWindowSeconds > maxStrategyITPMWindowSeconds) {
		return errors.New("ITPM window must be between 1 and 3600 seconds")
	}
	if input.ITPMSoftLimit != nil && *input.ITPMSoftLimit <= 0 {
		return errors.New("ITPM soft limit must be greater than zero")
	}
	if input.ITPMHardLimit != nil && *input.ITPMHardLimit <= 0 {
		return errors.New("ITPM hard limit must be greater than zero")
	}
	if input.ITPMSoftLimit != nil && input.ITPMHardLimit != nil && *input.ITPMSoftLimit >= *input.ITPMHardLimit {
		return errors.New("ITPM hard limit must be greater than the soft limit")
	}
	if input.RPMStrategy == "" {
		input.RPMStrategy = "fixed"
	}
	if input.RPMStrategy != "tiered" && input.RPMStrategy != "sticky_exempt" && input.RPMStrategy != "fixed" {
		return errors.New("invalid strategy RPM mode")
	}
	if !validDispatchMode(input.DispatchMode) {
		return errors.New("invalid strategy dispatch mode")
	}
	if input.SmoothColdStartRPM != nil && (*input.SmoothColdStartRPM < 1 || *input.SmoothColdStartRPM > 10000) {
		return errors.New("smooth cold start RPM must be between 1 and 10000")
	}
	if input.SmoothColdStartTPM != nil && *input.SmoothColdStartTPM < 1 {
		return errors.New("smooth cold start TPM must be greater than zero")
	}
	switch input.DispatchPacing {
	case "":
		// 正常：突发直到 RPM 用尽，保持既有行为。
		input.PacingConcurrency = 0
		input.PacingIntervalSeconds = 0
	case "interval":
		// 按秒间隔：每 pacing_interval_seconds 秒最多放行 pacing_concurrency 个新请求。
		if input.PacingConcurrency < 1 {
			return errors.New("pacing concurrency must be at least 1")
		}
		if input.PacingIntervalSeconds < 1 || input.PacingIntervalSeconds > 3600 {
			return errors.New("pacing interval must be between 1 and 3600 seconds")
		}
	case "completion":
		// 成功接续：保持 pacing_concurrency 个在途，空出一个立即接入下一个。
		if input.PacingConcurrency < 1 {
			return errors.New("pacing concurrency must be at least 1")
		}
		input.PacingIntervalSeconds = 0
	default:
		return errors.New("invalid strategy dispatch pacing")
	}
	return nil
}

func (input dispatchStrategyInput) itpmProtectionValues(defaultEnabled bool, defaultWindow int, defaultSoft, defaultHard int64) (bool, int, int64, int64) {
	enabled := defaultEnabled
	window := defaultWindow
	soft := defaultSoft
	hard := defaultHard
	if input.ITPMProtectionEnabled != nil {
		enabled = *input.ITPMProtectionEnabled
	}
	if input.ITPMWindowSeconds != nil {
		window = *input.ITPMWindowSeconds
	}
	if input.ITPMSoftLimit != nil {
		soft = *input.ITPMSoftLimit
	}
	if input.ITPMHardLimit != nil {
		hard = *input.ITPMHardLimit
	}
	return enabled, window, soft, hard
}

func validateStrategyITPMValues(window int, soft, hard int64) error {
	if window < minStrategyITPMWindowSeconds || window > maxStrategyITPMWindowSeconds {
		return errors.New("ITPM window must be between 1 and 3600 seconds")
	}
	if soft <= 0 {
		return errors.New("ITPM soft limit must be greater than zero")
	}
	if hard <= soft {
		return errors.New("ITPM hard limit must be greater than the soft limit")
	}
	return nil
}

func optionalBoolInt(value *bool) any {
	if value == nil {
		return nil
	}
	return boolInt(*value)
}

func optionalInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func optionalInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

const dispatchStrategySelect = `SELECT s.id, s.name, s.description, s.rpm_limit, s.tpm_limit, s.itpm_limit, s.itpm_protection_enabled, s.itpm_window_seconds, s.itpm_soft_limit, s.itpm_hard_limit, s.concurrency_limit, s.rpm_strategy, s.rpm_sticky_buffer, s.dispatch_mode, s.capacity_enabled, s.dispatch_pacing, s.pacing_concurrency, s.pacing_interval_seconds, s.smooth_cold_start_enabled, s.smooth_cold_start_rpm, s.smooth_cold_start_tpm,
	(SELECT COUNT(*) FROM groups g WHERE g.strategy_id = s.id) AS bound_groups,
	(SELECT COUNT(DISTINCT a.id) FROM accounts a
		LEFT JOIN account_groups ag ON ag.account_id = a.id
		LEFT JOIN groups g2 ON g2.id = ag.group_id
		WHERE a.deleted_at IS NULL AND a.archived_at IS NULL
		AND (a.strategy_id = s.id OR (a.strategy_id IS NULL AND g2.strategy_id = s.id))) AS bound_accounts,
	s.created_at, s.updated_at
	FROM dispatch_strategies s WHERE s.deleted_at IS NULL`

func scanDispatchStrategy(rows *sql.Rows) (dispatchStrategy, error) {
	var item dispatchStrategy
	var itpmProtectionEnabled, capacityEnabled, smoothColdStartEnabled int
	err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.RPMLimit, &item.TPMLimit, &item.ITPMLimit, &itpmProtectionEnabled, &item.ITPMWindowSeconds, &item.ITPMSoftLimit, &item.ITPMHardLimit, &item.ConcurrencyLimit, &item.RPMStrategy, &item.RPMStickyBuffer, &item.DispatchMode, &capacityEnabled, &item.DispatchPacing, &item.PacingConcurrency, &item.PacingIntervalSeconds, &smoothColdStartEnabled, &item.SmoothColdStartRPM, &item.SmoothColdStartTPM, &item.BoundGroups, &item.BoundAccounts, &item.CreatedAt, &item.UpdatedAt)
	item.ITPMProtectionEnabled = itpmProtectionEnabled == 1
	item.CapacityEnabled = capacityEnabled == 1
	item.SmoothColdStartEnabled = smoothColdStartEnabled == 1
	return item, err
}

func (a *app) listDispatchStrategies() ([]dispatchStrategy, error) {
	rows, err := a.db.Query(dispatchStrategySelect + ` ORDER BY s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	strategies := []dispatchStrategy{}
	for rows.Next() {
		item, err := scanDispatchStrategy(rows)
		if err != nil {
			return nil, err
		}
		strategies = append(strategies, item)
	}
	return strategies, rows.Err()
}

func (a *app) scopeDispatchStrategies(user panelUser, strategies []dispatchStrategy) ([]dispatchStrategy, error) {
	if !isScopedUserRole(user.Role) {
		return strategies, nil
	}
	groupCondition, groupArgs := scopedGroupCondition(user, "scope_group.id")
	accountCondition, accountArgs := scopedAccountCondition(user, "scope_account")
	result := make([]dispatchStrategy, 0, len(strategies))
	for _, strategy := range strategies {
		args := []any{strategy.ID}
		args = append(args, groupArgs...)
		args = append(args, strategy.ID, strategy.ID)
		args = append(args, accountArgs...)
		if err := a.db.QueryRow(`SELECT
			(SELECT COUNT(*) FROM groups scope_group WHERE scope_group.strategy_id = ? AND `+groupCondition+`),
			(SELECT COUNT(DISTINCT scope_account.id) FROM accounts scope_account
				WHERE scope_account.deleted_at IS NULL AND scope_account.archived_at IS NULL
				AND (scope_account.strategy_id = ? OR (scope_account.strategy_id IS NULL AND EXISTS (
					SELECT 1 FROM account_groups strategy_ag JOIN groups strategy_group ON strategy_group.id = strategy_ag.group_id
					WHERE strategy_ag.account_id = scope_account.id AND strategy_group.strategy_id = ?)))
				AND `+accountCondition+`)`, args...).Scan(&strategy.BoundGroups, &strategy.BoundAccounts); err != nil {
			return nil, err
		}
		if strategy.BoundGroups > 0 || strategy.BoundAccounts > 0 {
			result = append(result, strategy)
		}
	}
	return result, nil
}

func (a *app) handleStrategies(w http.ResponseWriter, r *http.Request) {
	strategies, err := a.listDispatchStrategies()
	if err == nil && !userCanManagePage(currentUser(r), "strategies") {
		strategies, err = a.scopeDispatchStrategies(currentUser(r), strategies)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, strategies)
}

func (a *app) handleStrategyCreate(w http.ResponseWriter, r *http.Request) {
	var input dispatchStrategyInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := input.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	itpmProtectionEnabled, itpmWindowSeconds, itpmSoftLimit, itpmHardLimit := input.itpmProtectionValues(defaultStrategyITPMProtectionEnabled, defaultStrategyITPMWindowSeconds, defaultStrategyITPMSoftLimit, defaultStrategyITPMHardLimit)
	if err := validateStrategyITPMValues(itpmWindowSeconds, itpmSoftLimit, itpmHardLimit); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	capacityEnabled := boolPointerValue(input.CapacityEnabled, true)
	smoothColdStartEnabled := boolPointerValue(input.SmoothColdStartEnabled, false)
	smoothColdStartRPM := intPointerValue(input.SmoothColdStartRPM, defaultSmoothColdStartRPM)
	smoothColdStartTPM := int64PointerValue(input.SmoothColdStartTPM, defaultSmoothColdStartTPM)
	result, err := a.db.Exec(`INSERT INTO dispatch_strategies (name, description, rpm_limit, tpm_limit, itpm_limit, itpm_protection_enabled, itpm_window_seconds, itpm_soft_limit, itpm_hard_limit, concurrency_limit, rpm_strategy, rpm_sticky_buffer, dispatch_mode, capacity_enabled, dispatch_pacing, pacing_concurrency, pacing_interval_seconds, smooth_cold_start_enabled, smooth_cold_start_rpm, smooth_cold_start_tpm) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.Name, input.Description, input.RPMLimit, input.TPMLimit, input.ITPMLimit, boolInt(itpmProtectionEnabled), itpmWindowSeconds, itpmSoftLimit, itpmHardLimit, input.ConcurrencyLimit, input.RPMStrategy, input.RPMStickyBuffer, input.DispatchMode, boolInt(capacityEnabled), input.DispatchPacing, input.PacingConcurrency, input.PacingIntervalSeconds, boolInt(smoothColdStartEnabled), smoothColdStartRPM, smoothColdStartTPM)
	if err != nil {
		writeDBError(w, err)
		return
	}
	id, _ := result.LastInsertId()
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (a *app) handleStrategyUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid strategy id")
		return
	}
	var input dispatchStrategyInput
	if !decodeJSON(w, r, &input) {
		return
	}
	if err := input.validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.ITPMProtectionEnabled != nil || input.ITPMWindowSeconds != nil || input.ITPMSoftLimit != nil || input.ITPMHardLimit != nil {
		var currentEnabled int
		var currentWindow int
		var currentSoft, currentHard int64
		err := a.db.QueryRow(`SELECT itpm_protection_enabled, itpm_window_seconds, itpm_soft_limit, itpm_hard_limit FROM dispatch_strategies WHERE id = ? AND deleted_at IS NULL`, id).Scan(&currentEnabled, &currentWindow, &currentSoft, &currentHard)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "strategy not found")
			return
		}
		if err != nil {
			writeDBError(w, err)
			return
		}
		_, window, soft, hard := input.itpmProtectionValues(currentEnabled == 1, currentWindow, currentSoft, currentHard)
		if err := validateStrategyITPMValues(window, soft, hard); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	result, err := a.db.Exec(`UPDATE dispatch_strategies SET name = ?, description = ?, rpm_limit = ?, tpm_limit = ?, itpm_limit = ?,
		itpm_protection_enabled = COALESCE(?, itpm_protection_enabled), itpm_window_seconds = COALESCE(?, itpm_window_seconds),
		itpm_soft_limit = COALESCE(?, itpm_soft_limit), itpm_hard_limit = COALESCE(?, itpm_hard_limit),
		concurrency_limit = ?, rpm_strategy = ?, rpm_sticky_buffer = ?, dispatch_mode = ?, capacity_enabled = COALESCE(?, capacity_enabled), dispatch_pacing = ?, pacing_concurrency = ?, pacing_interval_seconds = ?,
		smooth_cold_start_enabled = COALESCE(?, smooth_cold_start_enabled), smooth_cold_start_rpm = COALESCE(?, smooth_cold_start_rpm), smooth_cold_start_tpm = COALESCE(?, smooth_cold_start_tpm), updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`,
		input.Name, input.Description, input.RPMLimit, input.TPMLimit, input.ITPMLimit,
		optionalBoolInt(input.ITPMProtectionEnabled), optionalInt(input.ITPMWindowSeconds), optionalInt64(input.ITPMSoftLimit), optionalInt64(input.ITPMHardLimit),
		input.ConcurrencyLimit, input.RPMStrategy, input.RPMStickyBuffer, input.DispatchMode, optionalBoolInt(input.CapacityEnabled), input.DispatchPacing, input.PacingConcurrency, input.PacingIntervalSeconds,
		optionalBoolInt(input.SmoothColdStartEnabled), optionalInt(input.SmoothColdStartRPM), optionalInt64(input.SmoothColdStartTPM), id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "strategy not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

func (a *app) handleStrategyDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid strategy id")
		return
	}
	var boundGroups, boundAccounts int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM groups WHERE strategy_id = ?`, id).Scan(&boundGroups)
	// Archived accounts are excluded: they cannot be edited to unbind, so
	// counting them would make the strategy impossible to delete.
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE strategy_id = ? AND deleted_at IS NULL AND archived_at IS NULL`, id).Scan(&boundAccounts)
	if boundGroups > 0 || boundAccounts > 0 {
		writeError(w, http.StatusConflict, "策略仍被 "+strconv.Itoa(boundGroups)+" 个分组、"+strconv.Itoa(boundAccounts)+" 个账号绑定，请先解绑")
		return
	}
	result, err := a.db.Exec(`UPDATE dispatch_strategies SET deleted_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, id)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 0 {
		writeError(w, http.StatusNotFound, "strategy not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id})
}

type strategyAccountObservation struct {
	AccountID      int64    `json:"account_id"`
	Name           string   `json:"name"`
	GroupIDs       []string `json:"group_ids"`
	Alive          bool     `json:"alive"`
	Status         string   `json:"status"`
	RPM            int      `json:"rpm"`
	TPM            int64    `json:"tpm"`
	ITPM           int64    `json:"itpm"`
	ITPMReserved   int64    `json:"itpm_reserved"`
	ITPMLoad       int64    `json:"itpm_load"`
	CacheReadTPM   int64    `json:"cache_read_tpm"`
	ITPMRestricted bool     `json:"itpm_restricted"`
	Inflight       int      `json:"inflight"`
	BaseRPM        int      `json:"base_rpm"`
	EffectiveRPM   int      `json:"effective_rpm"`
	Concurrency    int      `json:"concurrency"`
	Direct         bool     `json:"direct_binding"`
	temporaryRPM   int
	extraJSON      string
	smooth         smoothColdStartConfig
	// Dispatch is "unavailable", "pending" (待调度, held back by a concentrated
	// strategy) or "active".
	Dispatch string `json:"dispatch"`
}

type strategyObservation struct {
	dispatchStrategy
	AccountsAlive          int                          `json:"accounts_alive"`
	AccountsPending        int                          `json:"accounts_pending"`
	AccountsITPMRestricted int                          `json:"accounts_itpm_restricted"`
	CurrentRPM             int                          `json:"current_rpm"`
	RPMCapacity            int64                        `json:"rpm_capacity"`
	RPMCapacityUnlimited   bool                         `json:"rpm_capacity_unlimited"`
	CurrentTPM             int64                        `json:"current_tpm"`
	CurrentInflight        int                          `json:"current_inflight"`
	Accounts               []strategyAccountObservation `json:"accounts"`
}

// handleStrategyObserve returns every strategy with its live per-account load
// so the observation page can render cards and the accordion table.
func (a *app) handleStrategyObserve(w http.ResponseWriter, r *http.Request) {
	strategies, err := a.listDispatchStrategies()
	user := currentUser(r)
	if err == nil {
		strategies, err = a.scopeDispatchStrategies(user, strategies)
	}
	if err != nil {
		writeDBError(w, err)
		return
	}
	result := []strategyObservation{}
	accountScope, accountScopeArgs := scopedAccountCondition(user, "a")
	for _, strategy := range strategies {
		observation := strategyObservation{dispatchStrategy: strategy, Accounts: []strategyAccountObservation{}}
		queryArgs := []any{strategy.ID, strategy.ID}
		queryArgs = append(queryArgs, accountScopeArgs...)
		rows, err := a.db.Query(`SELECT a.id, a.name, a.status, a.base_rpm, a.concurrency, a.extra_json,
			COALESCE((SELECT GROUP_CONCAT(ag2.group_id, ',') FROM account_groups ag2 WHERE ag2.account_id = a.id), '') AS group_ids,
			CASE WHEN `+accountStatePredicate("a", "normal")+` THEN 1 ELSE 0 END AS alive,
			a.strategy_id IS NOT NULL AS direct_binding,
			COALESCE((SELECT COUNT(*) FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0) AS current_rpm,
			COALESCE((SELECT SUM(u.input_tokens + u.output_tokens + u.cache_creation_tokens + u.cache_read_tokens) FROM usage_logs u WHERE u.account_id = a.id AND u.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0) AS current_tpm,
			COALESCE((SELECT SUM(u.cache_read_tokens) FROM usage_logs u WHERE u.account_id = a.id AND u.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0) AS current_cache_read_tpm,
			COALESCE((SELECT f.requests FROM account_inflight f WHERE f.account_id = a.id), 0) AS current_inflight,
			CASE WHEN a.rate_limit_downweight_until IS NOT NULL AND a.rate_limit_downweight_until > `+nowSQL+`
				THEN COALESCE((SELECT threshold.rpm_limit FROM account_rpm_thresholds threshold WHERE threshold.account_id = a.id AND threshold.reset_at > `+nowSQL+`), 0)
				ELSE 0 END AS temporary_rpm
			FROM accounts a
			WHERE a.deleted_at IS NULL AND a.archived_at IS NULL
			AND `+accountStatePredicate("a", "normal")+`
			AND (a.strategy_id = ? OR (a.strategy_id IS NULL AND EXISTS (
				SELECT 1 FROM account_groups ag JOIN groups g ON g.id = ag.group_id
				WHERE ag.account_id = a.id AND g.strategy_id = ?)))
			AND `+accountScope+`
			ORDER BY alive DESC, current_rpm DESC, a.id`, queryArgs...)
		if err != nil {
			writeDBError(w, err)
			return
		}
		for rows.Next() {
			var item strategyAccountObservation
			var alive, direct, temporaryRPM int
			var groupIDs string
			if err := rows.Scan(&item.AccountID, &item.Name, &item.Status, &item.BaseRPM, &item.Concurrency, &item.extraJSON, &groupIDs, &alive, &direct, &item.RPM, &item.TPM, &item.CacheReadTPM, &item.Inflight, &temporaryRPM); err != nil {
				rows.Close()
				writeDBError(w, err)
				return
			}
			item.Alive = alive == 1
			item.Direct = direct == 1
			item.GroupIDs = splitErrorGroupIDs(groupIDs)
			item.temporaryRPM = temporaryRPM
			item.smooth = smoothColdStartFromExtra(item.extraJSON)
			if !item.smooth.Enabled && strategy.SmoothColdStartEnabled {
				item.smooth = smoothColdStartConfig{Enabled: true, RPM: strategy.SmoothColdStartRPM, TPM: strategy.SmoothColdStartTPM}
			}
			observation.Accounts = append(observation.Accounts, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			writeDBError(w, err)
			return
		}
		rows.Close()
		itpmWindows := map[int64]int{}
		accountIDs := make([]int64, 0, len(observation.Accounts))
		for _, item := range observation.Accounts {
			accountIDs = append(accountIDs, item.AccountID)
			if item.smooth.Enabled {
				itpmWindows[item.AccountID] = defaultStrategyITPMWindowSeconds
			} else {
				itpmWindows[item.AccountID] = strategy.ITPMWindowSeconds
			}
		}
		itpmUsage, err := loadAccountITPMUsage(a.db, itpmWindows)
		if err != nil {
			writeDBError(w, err)
			return
		}
		reservations, err := a.accountITPMReservationStatuses(accountIDs)
		if err != nil {
			writeDBError(w, err)
			return
		}
		for index := range observation.Accounts {
			item := &observation.Accounts[index]
			item.ITPM = itpmUsage[item.AccountID]
			reservation := reservations[item.AccountID]
			item.ITPMReserved = reservation.Tokens
			item.ITPMLoad = item.ITPM + item.ITPMReserved
			itpmProtection, _, softLimit, hardLimit := normalizeStrategyITPMConfig(strategy.ITPMProtectionEnabled, strategy.ITPMWindowSeconds, strategy.ITPMSoftLimit, strategy.ITPMHardLimit, strategy.ITPMLimit)
			if item.smooth.Enabled {
				_, smoothTPM := normalizeSmoothColdStartLimits(item.smooth.RPM, item.smooth.TPM)
				itpmProtection = true
				hardLimit = minPositiveInt64(hardLimit, smoothTPM)
				softLimit = hardLimit
			}
			item.ITPMRestricted = itpmProtection && (reservation.Exclusive || item.ITPMLoad >= softLimit)
			// Under a concentrated strategy an idle account is deliberately held
			// back, which is different from being unable to serve.
			switch {
			case !item.Alive:
				item.Dispatch = "unavailable"
			case item.ITPMRestricted:
				item.Dispatch = "itpm_restricted"
				observation.AccountsITPMRestricted++
			case strategy.CapacityEnabled && strategy.DispatchMode == "concentrated" && item.RPM == 0:
				item.Dispatch = "pending"
				observation.AccountsPending++
			default:
				item.Dispatch = "active"
			}
			if item.Alive {
				observation.AccountsAlive++
			}
			if item.Alive && !item.ITPMRestricted {
				effectiveRPM := item.BaseRPM
				if strategy.CapacityEnabled && strategy.RPMLimit > 0 {
					effectiveRPM = strategy.RPMLimit
				}
				if item.smooth.Enabled {
					effectiveRPM = minPositiveInt(effectiveRPM, item.smooth.RPM)
				}
				if item.temporaryRPM > 0 && (effectiveRPM <= 0 || item.temporaryRPM < effectiveRPM) {
					effectiveRPM = item.temporaryRPM
				}
				item.EffectiveRPM = effectiveRPM
				if effectiveRPM > 0 {
					observation.RPMCapacity += int64(effectiveRPM)
				} else {
					observation.RPMCapacityUnlimited = true
				}
			}
			observation.CurrentRPM += item.RPM
			observation.CurrentTPM += item.TPM
			observation.CurrentInflight += item.Inflight
		}
		result = append(result, observation)
	}
	writeJSON(w, http.StatusOK, result)
}
