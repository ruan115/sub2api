package main

import "strings"

type accountViewConfig struct {
	Columns []string `json:"columns"`
	Blocks  []string `json:"blocks"`
}

var accountViewColumnOrder = []string{
	"account",
	"status",
	"subscription",
	"quota",
	"requests",
	"tpm",
}

var accountViewBlockOrder = []string{
	"filters",
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
	if role == "admin" {
		return accountViewConfig{
			Columns: append([]string{}, accountViewColumnOrder...),
			Blocks:  append([]string{}, accountViewBlockOrder...),
		}
	}
	return accountViewConfig{
		Columns: []string{"account", "status", "subscription", "quota", "requests", "tpm"},
		Blocks:  []string{"filtered_accounts", "tokens", "itpm", "cache_read", "otpm", "throughput"},
	}
}

func normalizeAccountView(role string, input accountViewConfig) accountViewConfig {
	if role == "admin" {
		return defaultAccountView(role)
	}
	if input.Columns == nil && input.Blocks == nil {
		return defaultAccountView(role)
	}
	return accountViewConfig{
		Columns: normalizeAccountViewValues(input.Columns, accountViewColumnOrder),
		Blocks:  normalizeAccountViewValues(input.Blocks, accountViewBlockOrder),
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
