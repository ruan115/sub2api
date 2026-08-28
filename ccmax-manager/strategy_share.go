package main

import (
	"fmt"
	"math"
	"net/http"
	"sort"
)

// Traffic entering a group is split between the dispatch strategies its
// accounts are bound to. Each strategy carries a weight and receives that
// share of the group's recent requests; a strategy that has already taken more
// than its share is skipped while another strategy can accept the request.
//
// Shares are a routing preference, not a capacity limit. If the preferred
// strategy has no usable account, dispatch retries the same group's other
// strategies while preserving every model, cooldown, concurrency, RPM/TPM and
// ITPM constraint.
type groupStrategyShare struct {
	StrategyID   int64   `json:"strategy_id"`
	StrategyName string  `json:"strategy_name"`
	Weight       int     `json:"weight"`
	Percent      float64 `json:"percent"`
	Accounts     int     `json:"accounts"`
	CurrentRPM   int     `json:"current_rpm"`
}

type groupStrategyShareInput struct {
	StrategyID int64 `json:"strategy_id"`
	Weight     int   `json:"weight"`
}

const groupStrategyShareMaxWeight = 1000

func (a *app) migrateGroupStrategyShares() error {
	if _, err := a.db.Exec(`CREATE TABLE IF NOT EXISTS group_strategy_shares (
		group_id TEXT NOT NULL,
		strategy_id INTEGER NOT NULL REFERENCES dispatch_strategies(id) ON DELETE CASCADE,
		weight INTEGER NOT NULL DEFAULT 0 CHECK (weight >= 0),
		created_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
		updated_at TEXT NOT NULL DEFAULT (` + nowSQL + `),
		PRIMARY KEY (group_id, strategy_id)
	)`); err != nil {
		return fmt.Errorf("create group strategy shares: %w", err)
	}
	return nil
}

// groupStrategyWeights returns the configured split for a group. A group with
// no rows, or whose rows all sit at zero, has no split configured and every
// strategy dispatches unrestricted.
//
// It reads through the caller's transaction: the dispatcher holds one open, and
// borrowing a second pooled connection there can deadlock SQLite.
func groupStrategyWeights(tx *databaseTx, groupID string) (map[int64]int, int) {
	rows, err := tx.Query(`SELECT s.strategy_id, s.weight FROM group_strategy_shares s
		JOIN dispatch_strategies ds ON ds.id = s.strategy_id AND ds.deleted_at IS NULL
		WHERE s.group_id = ? AND s.weight > 0`, groupID)
	if err != nil {
		return nil, 0
	}
	defer rows.Close()
	weights := map[int64]int{}
	total := 0
	for rows.Next() {
		var strategyID int64
		var weight int
		if rows.Scan(&strategyID, &weight) != nil {
			return nil, 0
		}
		weights[strategyID] = weight
		total += weight
	}
	if rows.Err() != nil {
		return nil, 0
	}
	return weights, total
}

// strategyOverShare reports whether a strategy has already consumed more than
// its slice of the group's recent traffic. The incoming request is counted, so
// a quiet group still admits the first request to each strategy.
//
// Strategies present in the group but absent from the configuration are left
// unrestricted: an operator who splits two of three strategies has said nothing
// about the third, and silently starving it would be surprising.
func strategyOverShare(weights map[int64]int, totalWeight int, strategyRPM map[int64]int, groupRPM int, strategyID int64) bool {
	if totalWeight <= 0 {
		return false
	}
	weight, configured := weights[strategyID]
	if !configured {
		return false
	}
	allowed := float64(groupRPM+1) * float64(weight) / float64(totalWeight)
	// Round up so small windows stay usable: with a 6/4 split and 1 request in
	// flight the minority strategy is still allowed its first request.
	return float64(strategyRPM[strategyID]) >= math.Ceil(allowed)
}

func (a *app) handleGroupStrategyShares(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	shares, err := a.listGroupStrategyShares(groupID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, shares)
}

// listGroupStrategyShares reports every strategy actually in play for the
// group — the ones its accounts resolve — joined with any configured weight, so
// the UI can offer a row per strategy without the operator hunting for IDs.
func (a *app) listGroupStrategyShares(groupID string) ([]groupStrategyShare, error) {
	rows, err := a.db.Query(`SELECT ds.id, ds.name,
		COALESCE((SELECT s.weight FROM group_strategy_shares s WHERE s.group_id = ? AND s.strategy_id = ds.id), 0),
		COUNT(DISTINCT a.id),
		COALESCE(SUM((SELECT COUNT(*) FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds'))), 0)
		FROM account_groups ag
		JOIN accounts a ON a.id = ag.account_id AND a.deleted_at IS NULL AND a.archived_at IS NULL
		LEFT JOIN groups g ON g.id = ag.group_id
		JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, g.strategy_id) AND ds.deleted_at IS NULL
		WHERE ag.group_id = ?
		GROUP BY ds.id, ds.name
		ORDER BY ds.id`, groupID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	shares := []groupStrategyShare{}
	totalWeight := 0
	for rows.Next() {
		var item groupStrategyShare
		if err := rows.Scan(&item.StrategyID, &item.StrategyName, &item.Weight, &item.Accounts, &item.CurrentRPM); err != nil {
			return nil, err
		}
		totalWeight += item.Weight
		shares = append(shares, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range shares {
		if totalWeight > 0 {
			shares[index].Percent = math.Round(float64(shares[index].Weight)*10000/float64(totalWeight)) / 100
		}
	}
	return shares, nil
}

func replaceGroupStrategyShares(tx *databaseTx, groupID string, inputs []groupStrategyShareInput) error {
	if _, err := tx.Exec(`DELETE FROM group_strategy_shares WHERE group_id = ?`, groupID); err != nil {
		return err
	}
	seen := map[int64]bool{}
	sorted := append([]groupStrategyShareInput(nil), inputs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].StrategyID < sorted[j].StrategyID })
	for _, item := range sorted {
		if item.StrategyID <= 0 || seen[item.StrategyID] {
			continue
		}
		seen[item.StrategyID] = true
		if item.Weight <= 0 {
			continue
		}
		if item.Weight > groupStrategyShareMaxWeight {
			return fmt.Errorf("strategy %d weight must be between 0 and %d", item.StrategyID, groupStrategyShareMaxWeight)
		}
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM dispatch_strategies WHERE id = ? AND deleted_at IS NULL`, item.StrategyID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("strategy %d does not exist", item.StrategyID)
		}
		if _, err := tx.Exec(`INSERT INTO group_strategy_shares (group_id, strategy_id, weight) VALUES (?, ?, ?)`, groupID, item.StrategyID, item.Weight); err != nil {
			return err
		}
	}
	return nil
}
