package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type dashboardPeriod struct {
	From         string                   `json:"from"`
	To           string                   `json:"to"`
	FirstUsageAt string                   `json:"first_usage_at"`
	LastUsageAt  string                   `json:"last_usage_at"`
	Totals       billingTotals            `json:"totals"`
	ByGroup      map[string]billingTotals `json:"by_group"`
}

func dashboardFilters(r *http.Request) (usageFilters, error) {
	filters := usageFilters{}
	for _, bound := range []struct {
		key string
		end bool
		out *string
	}{{"from", false, &filters.From}, {"to", true, &filters.To}} {
		value := strings.TrimSpace(r.URL.Query().Get(bound.key))
		if value == "" {
			continue
		}
		normalized := normalizeFilterTime(value, bound.end)
		if _, err := time.Parse(time.RFC3339Nano, normalized); err != nil {
			return filters, fmt.Errorf("%s must be a valid date or timestamp", bound.key)
		}
		*bound.out = normalized
	}
	if filters.From != "" && filters.To != "" {
		from, _ := time.Parse(time.RFC3339Nano, filters.From)
		to, _ := time.Parse(time.RFC3339Nano, filters.To)
		if !from.Before(to) {
			return filters, fmt.Errorf("from must be earlier than to")
		}
	}
	return filters, nil
}

func (a *app) queryDashboardPeriod(ctx context.Context, filters usageFilters) (dashboardPeriod, error) {
	result := dashboardPeriod{From: filters.From, To: filters.To, ByGroup: map[string]billingTotals{}}
	where, args := buildUsageWhere(filters)
	// Aggregate the ledger once without joining live account/proxy inventories.
	// Archived accounts and historical group membership must remain in the totals.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := a.db.QueryContext(ctx, `SELECT u.group_id, COUNT(*),
		COALESCE(SUM(u.input_tokens), 0), COALESCE(SUM(u.output_tokens), 0),
		COALESCE(SUM(u.cache_creation_tokens + u.cache_read_tokens), 0),
		COALESCE(SUM(u.base_cost), 0), COALESCE(SUM(u.billed_cost), 0),
		COALESCE(SUM(u.actual_cost), 0), COALESCE(SUM(u.billed_cost - u.actual_cost), 0),
		MIN(u.created_at), MAX(u.created_at)
		FROM usage_logs u WHERE `+where+` GROUP BY u.group_id`, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, first, last string
		var totals billingTotals
		if err := rows.Scan(&id, &totals.Requests, &totals.InputTokens, &totals.OutputTokens,
			&totals.CacheTokens, &totals.BaseCost, &totals.BilledCost, &totals.ActualCost,
			&totals.Margin, &first, &last); err != nil {
			return result, err
		}
		result.ByGroup[id] = totals
		result.Totals.Requests += totals.Requests
		result.Totals.InputTokens += totals.InputTokens
		result.Totals.OutputTokens += totals.OutputTokens
		result.Totals.CacheTokens += totals.CacheTokens
		result.Totals.BaseCost += totals.BaseCost
		result.Totals.BilledCost += totals.BilledCost
		result.Totals.ActualCost += totals.ActualCost
		result.Totals.Margin += totals.Margin
		if result.FirstUsageAt == "" || first < result.FirstUsageAt {
			result.FirstUsageAt = first
		}
		if last > result.LastUsageAt {
			result.LastUsageAt = last
		}
	}
	return result, rows.Err()
}
