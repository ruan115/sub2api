package main

import "strings"

type accountViewConfig struct {
	Columns  []string `json:"columns"`
	Filters  []string `json:"filters"`
	Statuses []string `json:"statuses"`
	Blocks   []string `json:"blocks"`
}

var accountViewColumnOrder = []string{
	"select",
	"id",
	"account",
	"status",
	"subscription",
	"price",
	"billing",
	"quota",
	"requests",
	"tpm",
	"onboarded",
	"reauthorized",
	"reauth-count",
	"survival",
	"last-used",
	"actions",
}

var accountViewFilterOrder = []string{
	"search",
	"group",
	"strategy",
	"quota_5h",
	"cooling_5h",
	"quota_7d",
	"cooling_7d",
	"from",
	"to",
}

var accountViewStatusOrder = []string{
	"all",
	"normal",
	"unavailable",
	"limited_5h",
	"limited_7d",
	"cooling_429",
	"error",
}

var accountViewBlockOrder = []string{
	"filtered_accounts",
	"billed",
	"actual_cost",
	"tokens",
	"rpm",
	"itpm",
	"cache_read",
	"otpm",
	"throughput",
	"concurrency",
	"queue",
}

func defaultAccountView(role string) accountViewConfig {
	if role == roleAdmin || role == roleOnboardingUser {
		return accountViewConfig{
			Columns:  append([]string{}, accountViewColumnOrder...),
			Filters:  append([]string{}, accountViewFilterOrder...),
			Statuses: append([]string{}, accountViewStatusOrder...),
			Blocks:   append([]string{}, accountViewBlockOrder...),
		}
	}
	return accountViewConfig{
		Columns:  []string{"account", "status", "subscription", "quota", "requests", "tpm"},
		Filters:  []string{},
		Statuses: []string{},
		Blocks:   []string{"filtered_accounts", "tokens", "itpm", "cache_read", "otpm", "throughput"},
	}
}

func normalizeAccountView(role string, input accountViewConfig) accountViewConfig {
	if role == roleAdmin {
		return defaultAccountView(role)
	}
	if input.Columns == nil && input.Filters == nil && input.Statuses == nil && input.Blocks == nil {
		return defaultAccountView(role)
	}
	if role == roleOnboardingUser && input.Filters == nil && input.Statuses == nil {
		// Upgrade the legacy onboarding-user default, which only stored the old
		// columns/blocks pair, to the new complete configurable view.
		return defaultAccountView(role)
	}
	filters := input.Filters
	if filters == nil && accountViewHas(input.Blocks, "filters") {
		filters = accountViewFilterOrder
	}
	return accountViewConfig{
		Columns:  normalizeAccountViewValues(input.Columns, accountViewColumnOrder),
		Filters:  normalizeAccountViewValues(filters, accountViewFilterOrder),
		Statuses: normalizeAccountViewValues(input.Statuses, accountViewStatusOrder),
		Blocks:   normalizeAccountViewValues(input.Blocks, accountViewBlockOrder),
	}
}

func normalizeAccountViewValues(input, allowed []string) []string {
	valid := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		valid[value] = true
	}
	selected := make(map[string]bool, len(input))
	for _, value := range input {
		value = strings.ToLower(strings.TrimSpace(value))
		if valid[value] {
			selected[value] = true
		}
	}
	result := make([]string, 0, len(selected))
	for _, value := range allowed {
		if selected[value] {
			result = append(result, value)
		}
	}
	return result
}

func accountViewHas(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func accountForRestrictedView(item account, view accountViewConfig) map[string]any {
	result := map[string]any{"id": item.ID}
	if accountViewHas(view.Columns, "account") {
		result["name"] = item.Name
	}
	if accountViewHas(view.Columns, "status") {
		result["status"] = "active"
		result["dispatch_status"] = "normal"
	}
	if accountViewHas(view.Columns, "subscription") {
		result["subscription_type"] = item.SubscriptionType
		result["rate_limit_tier"] = item.RateLimitTier
	}
	if accountViewHas(view.Columns, "quota") {
		result["quota_5h_utilization"] = item.Quota5H
		result["quota_5h_reset_at"] = item.Quota5HResetAt
		result["quota_7d_utilization"] = item.Quota7D
		result["quota_7d_reset_at"] = item.Quota7DResetAt
		result["quota_sampled_at"] = item.QuotaSampledAt
	}
	if accountViewHas(view.Columns, "requests") {
		result["request_count"] = item.RequestCount
	}
	if accountViewHas(view.Columns, "price") {
		result["account_price"] = item.AccountPrice
	}
	if accountViewHas(view.Columns, "billing") {
		result["total_billed_cost"] = item.TotalBilledCost
	}
	if accountViewHas(view.Columns, "onboarded") || accountViewHas(view.Columns, "survival") {
		result["onboarded_at"] = item.OnboardedAt
	}
	if accountViewHas(view.Columns, "reauthorized") {
		result["reauthorized_at"] = item.ReauthorizedAt
	}
	if accountViewHas(view.Columns, "reauth-count") {
		result["reauthorization_count"] = item.ReauthorizationCount
	}
	if accountViewHas(view.Columns, "survival") {
		result["invalidated_at"] = item.InvalidatedAt
		result["survival_seconds"] = item.SurvivalSeconds
	}
	if accountViewHas(view.Columns, "last-used") {
		result["last_used_at"] = item.LastUsedAt
	}
	return result
}

func accountSummaryForRestrictedView(item accountSummary, view accountViewConfig) map[string]any {
	result := map[string]any{}
	if accountViewHas(view.Blocks, "filtered_accounts") {
		result["accounts"] = item.Accounts
		result["active_accounts"] = item.ActiveAccounts
	}
	if accountViewHas(view.Blocks, "billed") {
		result["billed_cost"] = item.BilledCost
		result["requests"] = item.Requests
	}
	if accountViewHas(view.Columns, "requests") {
		result["requests"] = item.Requests
	}
	if accountViewHas(view.Blocks, "actual_cost") {
		result["actual_cost"] = item.ActualCost
	}
	if accountViewHas(view.Blocks, "tokens") {
		result["input_tokens"] = item.InputTokens
		result["output_tokens"] = item.OutputTokens
	}
	return result
}

func realtimeForRestrictedView(item realtimeLoad, view accountViewConfig) map[string]any {
	result := map[string]any{
		"window_seconds": item.WindowSeconds,
		"updated_at":     item.UpdatedAt,
	}
	if accountViewHas(view.Blocks, "rpm") {
		result["rpm"] = item.RPM
		result["rpm_capacity"] = item.RPMCapacity
		result["unlimited_capacity"] = item.Unlimited
	}
	if accountViewHas(view.Blocks, "itpm") {
		result["itpm"] = item.ITPM
		result["itpm_restricted_accounts"] = item.ITPMRestricted
		result["itpm_hard_blocked_accounts"] = item.ITPMHardBlocked
	}
	if accountViewHas(view.Blocks, "cache_read") {
		result["cache_read_tpm"] = item.CacheReadTPM
	}
	if accountViewHas(view.Blocks, "otpm") {
		result["otpm"] = item.OTPM
	}
	if accountViewHas(view.Blocks, "throughput") {
		result["tpm"] = item.TPM
	}
	if accountViewHas(view.Blocks, "concurrency") {
		result["inflight"] = item.Inflight
		result["concurrency_capacity"] = item.ConcurrencyCapacity
	}
	if accountViewHas(view.Blocks, "queue") {
		result["waiting_requests"] = item.WaitingRequests
	}
	if accountViewHas(view.Columns, "tpm") {
		accounts := make([]map[string]any, 0, len(item.Accounts))
		for _, account := range item.Accounts {
			accounts = append(accounts, map[string]any{
				"account_id": account.AccountID,
				"tpm":        account.TPM,
			})
		}
		result["accounts"] = accounts
	}
	return result
}
