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
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	RPMLimit         int    `json:"rpm_limit"`
	TPMLimit         int64  `json:"tpm_limit"`
	ITPMLimit        int64  `json:"itpm_limit"`
	ConcurrencyLimit int    `json:"concurrency_limit"`
	RPMStrategy      string `json:"rpm_strategy"`
	RPMStickyBuffer  int    `json:"rpm_sticky_buffer"`
	DispatchMode     string `json:"dispatch_mode"`
	BoundGroups      int    `json:"bound_groups"`
	BoundAccounts    int    `json:"bound_accounts"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type dispatchStrategyInput struct {
	Name             string `json:"name"`
	Description      string `json:"description"`
	RPMLimit         int    `json:"rpm_limit"`
	TPMLimit         int64  `json:"tpm_limit"`
	ITPMLimit        int64  `json:"itpm_limit"`
	ConcurrencyLimit int    `json:"concurrency_limit"`
	RPMStrategy      string `json:"rpm_strategy"`
	RPMStickyBuffer  int    `json:"rpm_sticky_buffer"`
	DispatchMode     string `json:"dispatch_mode"`
}

func (input *dispatchStrategyInput) validate() error {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		return errors.New("strategy name is required")
	}
	if input.RPMLimit < 0 || input.TPMLimit < 0 || input.ITPMLimit < 0 || input.ConcurrencyLimit < 0 || input.RPMStickyBuffer < 0 {
		return errors.New("strategy limits cannot be negative")
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
	return nil
}

const dispatchStrategySelect = `SELECT s.id, s.name, s.description, s.rpm_limit, s.tpm_limit, s.itpm_limit, s.concurrency_limit, s.rpm_strategy, s.rpm_sticky_buffer, s.dispatch_mode,
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
	err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.RPMLimit, &item.TPMLimit, &item.ITPMLimit, &item.ConcurrencyLimit, &item.RPMStrategy, &item.RPMStickyBuffer, &item.DispatchMode, &item.BoundGroups, &item.BoundAccounts, &item.CreatedAt, &item.UpdatedAt)
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

func (a *app) handleStrategies(w http.ResponseWriter, _ *http.Request) {
	strategies, err := a.listDispatchStrategies()
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
	result, err := a.db.Exec(`INSERT INTO dispatch_strategies (name, description, rpm_limit, tpm_limit, itpm_limit, concurrency_limit, rpm_strategy, rpm_sticky_buffer, dispatch_mode) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.Name, input.Description, input.RPMLimit, input.TPMLimit, input.ITPMLimit, input.ConcurrencyLimit, input.RPMStrategy, input.RPMStickyBuffer, input.DispatchMode)
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
	result, err := a.db.Exec(`UPDATE dispatch_strategies SET name = ?, description = ?, rpm_limit = ?, tpm_limit = ?, itpm_limit = ?, concurrency_limit = ?, rpm_strategy = ?, rpm_sticky_buffer = ?, dispatch_mode = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`,
		input.Name, input.Description, input.RPMLimit, input.TPMLimit, input.ITPMLimit, input.ConcurrencyLimit, input.RPMStrategy, input.RPMStickyBuffer, input.DispatchMode, id)
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
	AccountID   int64    `json:"account_id"`
	Name        string   `json:"name"`
	GroupIDs    []string `json:"group_ids"`
	Alive       bool     `json:"alive"`
	Status      string   `json:"status"`
	RPM         int      `json:"rpm"`
	TPM         int64    `json:"tpm"`
	Inflight    int      `json:"inflight"`
	BaseRPM     int      `json:"base_rpm"`
	Concurrency int      `json:"concurrency"`
	Direct      bool     `json:"direct_binding"`
	// Dispatch is "unavailable", "pending" (待调度, held back by a concentrated
	// strategy) or "active".
	Dispatch string `json:"dispatch"`
}

type strategyObservation struct {
	dispatchStrategy
	AccountsAlive        int                          `json:"accounts_alive"`
	AccountsPending      int                          `json:"accounts_pending"`
	CurrentRPM           int                          `json:"current_rpm"`
	RPMCapacity          int64                        `json:"rpm_capacity"`
	RPMCapacityUnlimited bool                         `json:"rpm_capacity_unlimited"`
	CurrentTPM           int64                        `json:"current_tpm"`
	CurrentInflight      int                          `json:"current_inflight"`
	Accounts             []strategyAccountObservation `json:"accounts"`
}

// handleStrategyObserve returns every strategy with its live per-account load
// so the observation page can render cards and the accordion table.
func (a *app) handleStrategyObserve(w http.ResponseWriter, _ *http.Request) {
	strategies, err := a.listDispatchStrategies()
	if err != nil {
		writeDBError(w, err)
		return
	}
	result := []strategyObservation{}
	for _, strategy := range strategies {
		observation := strategyObservation{dispatchStrategy: strategy, Accounts: []strategyAccountObservation{}}
		rows, err := a.db.Query(`SELECT a.id, a.name, a.status, a.base_rpm, a.concurrency,
			COALESCE((SELECT GROUP_CONCAT(ag2.group_id, ',') FROM account_groups ag2 WHERE ag2.account_id = a.id), '') AS group_ids,
			CASE WHEN `+accountStatePredicate("a", "normal")+` THEN 1 ELSE 0 END AS alive,
			a.strategy_id IS NOT NULL AS direct_binding,
			COALESCE((SELECT COUNT(*) FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0) AS current_rpm,
			COALESCE((SELECT SUM(u.input_tokens + u.output_tokens + u.cache_creation_tokens + u.cache_read_tokens) FROM usage_logs u WHERE u.account_id = a.id AND u.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0) AS current_tpm,
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
			ORDER BY alive DESC, current_rpm DESC, a.id`, strategy.ID, strategy.ID)
		if err != nil {
			writeDBError(w, err)
			return
		}
		for rows.Next() {
			var item strategyAccountObservation
			var alive, direct, temporaryRPM int
			var groupIDs string
			if err := rows.Scan(&item.AccountID, &item.Name, &item.Status, &item.BaseRPM, &item.Concurrency, &groupIDs, &alive, &direct, &item.RPM, &item.TPM, &item.Inflight, &temporaryRPM); err != nil {
				rows.Close()
				writeDBError(w, err)
				return
			}
			item.Alive = alive == 1
			item.Direct = direct == 1
			item.GroupIDs = splitErrorGroupIDs(groupIDs)
			// Under a concentrated strategy an idle account is deliberately held
			// back, which is different from being unable to serve.
			switch {
			case !item.Alive:
				item.Dispatch = "unavailable"
			case strategy.DispatchMode == "concentrated" && item.RPM == 0:
				item.Dispatch = "pending"
				observation.AccountsPending++
			default:
				item.Dispatch = "active"
			}
			if item.Alive {
				observation.AccountsAlive++
				effectiveRPM := item.BaseRPM
				if strategy.RPMLimit > 0 {
					effectiveRPM = strategy.RPMLimit
				}
				if temporaryRPM > 0 && (effectiveRPM <= 0 || temporaryRPM < effectiveRPM) {
					effectiveRPM = temporaryRPM
				}
				if effectiveRPM > 0 {
					observation.RPMCapacity += int64(effectiveRPM)
				} else {
					observation.RPMCapacityUnlimited = true
				}
			}
			observation.CurrentRPM += item.RPM
			observation.CurrentTPM += item.TPM
			observation.CurrentInflight += item.Inflight
			observation.Accounts = append(observation.Accounts, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			writeDBError(w, err)
			return
		}
		rows.Close()
		result = append(result, observation)
	}
	writeJSON(w, http.StatusOK, result)
}
