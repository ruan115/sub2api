package main

import (
	"database/sql"
	"errors"
	"fmt"
)

var errNoGatewayAccountCapacity = errors.New("no gateway account capacity")

func (a *app) ensureReserveCapacity(targetGroupID, requestedModel, reason string, excluded map[int64]bool) (bool, error) {
	if targetGroupID == "" || (reason != "capacity" && reason != "rate_limit") {
		return false, nil
	}
	a.reserveMu.Lock()
	defer a.reserveMu.Unlock()

	var targetReserve, strategyRequired int
	var targetStrategyID sql.NullInt64
	if err := a.db.QueryRow(`SELECT reserve_pool_enabled, strategy_required_enabled, strategy_id FROM groups WHERE id = ? AND status = 'active'`, targetGroupID).Scan(&targetReserve, &strategyRequired, &targetStrategyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if targetReserve == 1 {
		return false, nil
	}

	hasCapacity, err := a.gatewayGroupHasCapacity(targetGroupID, requestedModel, excluded, strategyRequired == 1)
	if err != nil || hasCapacity {
		return hasCapacity, err
	}
	// When the target group only dispatches strategy-bound accounts and carries
	// no strategy of its own to inherit, activating an unbound reserve account
	// would move it somewhere it can never be selected.
	requireOwnStrategy := strategyRequired == 1 && !targetStrategyID.Valid

	tx, err := a.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT a.id, a.name, a.auth_type, a.credentials_json, a.source_sk_hint, a.extra_json,
		a.concurrency, a.base_rpm, a.rpm_strategy, a.rpm_sticky_buffer, a.user_msg_queue_mode, a.proxy_id, ag.group_id, ag.priority
		FROM accounts a
		JOIN account_groups ag ON ag.account_id = a.id
		JOIN groups g ON g.id = ag.group_id
		WHERE g.reserve_pool_enabled = 1 AND g.status = 'active'
		AND a.deleted_at IS NULL AND `+legacyExecutionPredicate("a")+` AND `+accountStatePredicate("a", "normal")+`
		AND NOT EXISTS (
			SELECT 1 FROM account_groups other
			WHERE other.account_id = a.id AND other.group_id != ag.group_id
		)
		AND NOT EXISTS (
			SELECT 1 FROM account_model_cooldowns mc
			WHERE mc.account_id = a.id AND mc.model = ? AND mc.reset_at > `+nowSQL+`
		)
		AND (? = 0 OR a.strategy_id IS NOT NULL)
		ORDER BY ag.priority, a.priority, COALESCE(a.last_used_at, ''), a.id`, modelCooldownKey(requestedModel), boolInt(requireOwnStrategy))
	if err != nil {
		return false, err
	}
	var selected gatewayAccount
	var sourceGroupID string
	var groupPriority int
	for rows.Next() {
		var candidate gatewayAccount
		if err := rows.Scan(&candidate.ID, &candidate.Name, &candidate.AuthType, &candidate.CredentialsJSON, &candidate.SourceSKHint, &candidate.ExtraJSON,
			&candidate.Concurrency, &candidate.BaseRPM, &candidate.RPMStrategy, &candidate.StickyBuffer, &candidate.UserMsgQueueMode, &candidate.ProxyID, &sourceGroupID, &groupPriority); err != nil {
			rows.Close()
			return false, err
		}
		if accountSupportsModel(candidate, requestedModel) {
			selected = candidate
			break
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if selected.ID == 0 {
		return false, nil
	}
	if _, err := tx.Exec(`DELETE FROM account_groups WHERE account_id = ? AND group_id = ?`, selected.ID, sourceGroupID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO account_groups (account_id, group_id, priority) VALUES (?, ?, ?)`, selected.ID, targetGroupID, groupPriority); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO reserve_activation_logs (account_id, source_group_id, target_group_id, reason, requested_model) VALUES (?, ?, ?, ?, ?)`, selected.ID, sourceGroupID, targetGroupID, reason, requestedModel); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (a *app) gatewayGroupHasCapacity(groupID, requestedModel string, excluded map[int64]bool, strategyRequired bool) (bool, error) {
	rows, err := a.db.Query(`SELECT a.id, a.auth_type, a.extra_json, a.concurrency, a.base_rpm, a.rpm_strategy, a.rpm_sticky_buffer,
		COALESCE((SELECT COUNT(*) FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0),
		COALESCE((SELECT requests FROM account_inflight f WHERE f.account_id = a.id), 0),
		CASE WHEN a.rate_limit_downweight_until IS NOT NULL AND a.rate_limit_downweight_until > `+nowSQL+`
			THEN COALESCE((SELECT rpm_limit FROM account_rpm_thresholds t WHERE t.account_id = a.id AND t.reset_at > `+nowSQL+`), 0)
			ELSE 0 END
		FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		LEFT JOIN groups g ON g.id = ag.group_id
		LEFT JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, g.strategy_id) AND ds.deleted_at IS NULL
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND `+legacyExecutionPredicate("a")+` AND `+accountStatePredicate("a", "normal")+`
		AND (? = 0 OR ds.id IS NOT NULL)
		AND NOT EXISTS (
			SELECT 1 FROM account_model_cooldowns mc
			WHERE mc.account_id = a.id AND mc.model = ? AND mc.reset_at > `+nowSQL+`
		)`, groupID, boolInt(strategyRequired), modelCooldownKey(requestedModel))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var candidate gatewayAccount
		var rpm, inflight, temporaryRPM int
		if err := rows.Scan(&candidate.ID, &candidate.AuthType, &candidate.ExtraJSON, &candidate.Concurrency, &candidate.BaseRPM, &candidate.RPMStrategy, &candidate.StickyBuffer, &rpm, &inflight, &temporaryRPM); err != nil {
			return false, err
		}
		if excluded[candidate.ID] || !accountSupportsModel(candidate, requestedModel) || inflight >= candidate.Concurrency {
			continue
		}
		if temporaryRPM > 0 && rpm >= temporaryRPM {
			continue
		}
		if rpmSchedulable(candidate, rpm, false) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func reserveActivationError(groupID string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("activate reserve account for group %s: %w", groupID, err)
}
