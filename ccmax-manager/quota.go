package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
)

type quotaWindow struct {
	Utilization float64 `json:"utilization"`
	ResetsAt    string  `json:"resets_at"`
}

type accountQuota struct {
	Source    string      `json:"source"`
	UpdatedAt string      `json:"updated_at"`
	FiveHour  quotaWindow `json:"five_hour"`
	SevenDay  quotaWindow `json:"seven_day"`
}

type claudeUsageResponse struct {
	FiveHour struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"five_hour"`
	SevenDay struct {
		Utilization float64 `json:"utilization"`
		ResetsAt    string  `json:"resets_at"`
	} `json:"seven_day"`
}

var claudeUsageEndpoint = "https://api.anthropic.com/api/oauth/usage"

func (a *app) handleAccountQuotaRefresh(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	quota, err := a.refreshAccountQuota(r.Context(), accountID)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

func (a *app) handleAccountRateLimitReset(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	reset, err := a.clearAccount429State(accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reset": reset})
}

func (a *app) refreshAccountQuota(ctx context.Context, accountID int64) (accountQuota, error) {
	var authType, credentialsJSON string
	var proxyID sql.NullInt64
	var stored accountQuota
	var fiveReset, sevenReset, sampled sql.NullString
	if err := a.db.QueryRow(`SELECT auth_type, credentials_json, proxy_id, quota_5h_utilization, quota_5h_reset_at, quota_7d_utilization, quota_7d_reset_at, quota_sampled_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).
		Scan(&authType, &credentialsJSON, &proxyID, &stored.FiveHour.Utilization, &fiveReset, &stored.SevenDay.Utilization, &sevenReset, &sampled); err != nil {
		return accountQuota{}, err
	}
	stored.Source = "passive"
	stored.FiveHour.ResetsAt = nullText(fiveReset)
	stored.SevenDay.ResetsAt = nullText(sevenReset)
	stored.UpdatedAt = nullText(sampled)
	if authType != "oauth" {
		return stored, nil
	}
	account := gatewayAccount{ID: accountID, AuthType: authType, CredentialsJSON: credentialsJSON, ProxyID: proxyID}
	account, err := a.ensureGatewayAccountToken(ctx, account)
	if err != nil {
		return accountQuota{}, err
	}
	if !account.ProxyID.Valid {
		return accountQuota{}, errors.New("CCMAX account must bind an active proxy")
	}
	proxy, err := a.proxyURL(account.ProxyID.Int64)
	if err != nil {
		return accountQuota{}, errors.New("CCMAX account proxy is unavailable")
	}
	// Keep plan metadata current whenever an administrator explicitly refreshes
	// account quota. Quota remains available even if profile metadata is down.
	_ = a.syncClaudeAccountProfile(ctx, accountID, account.CredentialsJSON, proxy.String())
	client, err := clientForProxy(proxy)
	if err != nil {
		return accountQuota{}, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		credentials := decodeObject(account.CredentialsJSON)
		accessToken, _ := credentials["access_token"].(string)
		if strings.TrimSpace(accessToken) == "" {
			a.markAccountReauth(accountID, "account has no OAuth access token")
			return accountQuota{}, errors.New("account has no OAuth access token; reauthorization is required")
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageEndpoint, nil)
		request.Header.Set("Accept", "application/json, text/plain, */*")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+accessToken)
		request.Header.Set("anthropic-beta", "oauth-2025-04-20")
		request.Header.Set("User-Agent", "claude-code/2.1.7")
		response, requestErr := client.Do(request)
		if requestErr != nil {
			return accountQuota{}, fmt.Errorf("fetch account quota: %w", requestErr)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		response.Body.Close()
		if readErr != nil {
			return accountQuota{}, fmt.Errorf("read Anthropic quota: %w", readErr)
		}
		if response.StatusCode == http.StatusUnauthorized && gatewayAccountHasRefreshToken(account) && attempt == 0 {
			refreshed, refreshErr := a.refreshGatewayAccountToken(ctx, account, true)
			if refreshErr == nil {
				account = refreshed
				continue
			}
			return accountQuota{}, refreshErr
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			a.captureAccountUpstreamFailure(account, response.StatusCode, body)
			return accountQuota{}, fmt.Errorf("Anthropic usage API authorization failed (status %d)", response.StatusCode)
		}
		if response.StatusCode != http.StatusOK {
			return accountQuota{}, fmt.Errorf("Anthropic usage API returned status %d", response.StatusCode)
		}
		var upstream claudeUsageResponse
		if err := json.Unmarshal(body, &upstream); err != nil {
			return accountQuota{}, fmt.Errorf("decode Anthropic quota: %w", err)
		}
		quota := accountQuota{Source: "active", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		quota.FiveHour = quotaWindow{Utilization: upstream.FiveHour.Utilization, ResetsAt: normalizeUpstreamTime(upstream.FiveHour.ResetsAt)}
		quota.SevenDay = quotaWindow{Utilization: upstream.SevenDay.Utilization, ResetsAt: normalizeUpstreamTime(upstream.SevenDay.ResetsAt)}
		if err := a.persistAccountQuota(accountID, quota, true); err != nil {
			return accountQuota{}, err
		}
		if err := a.enforceAccountFiveHourThreshold(accountID, quota.FiveHour, rateLimitPolicy{}); err != nil {
			return accountQuota{}, err
		}
		return quota, nil
	}
	return accountQuota{}, errors.New("Anthropic usage API authorization failed")
}

func normalizeUpstreamTime(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UTC().Format(time.RFC3339Nano)
	}
	return raw
}

func (a *app) persistAccountQuota(accountID int64, quota accountQuota, active bool) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var extraJSON string
	var previousInvalidated sql.NullString
	if err := tx.QueryRow(`SELECT extra_json, invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&extraJSON, &previousInvalidated); err != nil {
		return err
	}
	extra := decodeObject(extraJSON)
	extra["session_window_utilization"] = quota.FiveHour.Utilization / 100
	extra["passive_usage_7d_utilization"] = quota.SevenDay.Utilization / 100
	if reset := unixFromTime(quota.SevenDay.ResetsAt); reset > 0 {
		extra["passive_usage_7d_reset"] = reset
	}
	extra["passive_usage_sampled_at"] = quota.UpdatedAt
	encoded, _ := json.Marshal(extra)
	authStatus := "auth_status"
	if active {
		authStatus = "'valid'"
	}
	_, err = tx.Exec(`UPDATE accounts SET quota_5h_utilization = ?, quota_5h_reset_at = NULLIF(?, ''), quota_7d_utilization = ?, quota_7d_reset_at = NULLIF(?, ''), quota_sampled_at = ?, extra_json = ?, auth_status = `+authStatus+`, auth_error = CASE WHEN ? THEN '' ELSE auth_error END, auth_checked_at = CASE WHEN ? THEN `+nowSQL+` ELSE auth_checked_at END, onboarded_at = CASE WHEN ? AND invalidated_at IS NOT NULL THEN `+nowSQL+` ELSE onboarded_at END, invalidated_at = CASE WHEN ? THEN NULL ELSE invalidated_at END, status = CASE WHEN ? AND status = 'error' THEN 'active' ELSE status END, error_message = CASE WHEN ? THEN '' ELSE error_message END, updated_at = `+nowSQL+` WHERE id = ?`, quota.FiveHour.Utilization, quota.FiveHour.ResetsAt, quota.SevenDay.Utilization, quota.SevenDay.ResetsAt, quota.UpdatedAt, string(encoded), active, active, active, active, active, active, accountID)
	if err != nil {
		return err
	}
	if active && previousInvalidated.Valid {
		if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, 'onboarded')`, accountID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *app) enforceStoredAccountFiveHourThreshold(accountID int64, policy rateLimitPolicy) error {
	var window quotaWindow
	var reset sql.NullString
	if err := a.db.QueryRow(`SELECT quota_5h_utilization, quota_5h_reset_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&window.Utilization, &reset); err != nil {
		return err
	}
	window.ResetsAt = nullText(reset)
	return a.enforceAccountFiveHourThreshold(accountID, window, policy)
}

func (a *app) accountFiveHourThresholdReleaseDeadline(accountID int64, resetAt time.Time, policy rateLimitPolicy) (time.Time, error) {
	if policy.FiveHourStaggerSet {
		return policy.quotaReleaseDeadline(accountID, "5h", resetAt), nil
	}
	rows, err := a.db.Query(`SELECT g.five_hour_release_stagger_enabled, g.five_hour_release_stagger_min_minutes, g.five_hour_release_stagger_max_minutes
		FROM groups g JOIN account_groups ag ON ag.group_id = g.id
		WHERE ag.account_id = ? AND g.status = 'active'`, accountID)
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	deadline := resetAt.UTC()
	found := false
	for rows.Next() {
		var enabled int
		var minimum, maximum int
		if err := rows.Scan(&enabled, &minimum, &maximum); err != nil {
			return time.Time{}, err
		}
		found = true
		candidate := (rateLimitPolicy{
			FiveHourStaggerSet:     true,
			FiveHourStaggerEnabled: enabled == 1,
			FiveHourStaggerMin:     minimum,
			FiveHourStaggerMax:     maximum,
		}).quotaReleaseDeadline(accountID, "5h", resetAt)
		if candidate.After(deadline) {
			deadline = candidate
		}
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	if !found {
		deadline = policy.quotaReleaseDeadline(accountID, "5h", resetAt)
	}
	return deadline, nil
}

func (a *app) clearAccountFiveHourThresholdCooldown(accountID int64) error {
	_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = NULL, rate_limit_window = '',
		rate_limit_reason = CASE WHEN rate_limit_downweight_until > `+nowSQL+` THEN '429_backoff' ELSE '' END,
		quota_refreshed_at = CASE WHEN quota_5h_reset_at IS NOT NULL AND quota_5h_reset_at <= `+nowSQL+` THEN quota_5h_reset_at ELSE quota_refreshed_at END,
		error_message = CASE WHEN auth_error = '' THEN '' ELSE error_message END,
		updated_at = `+nowSQL+`
		WHERE id = ? AND deleted_at IS NULL AND rate_limit_reason = 'quota_threshold'`, accountID)
	return err
}

func (a *app) enforceAccountFiveHourThreshold(accountID int64, window quotaWindow, policy rateLimitPolicy) error {
	var enabled int
	var threshold int
	var existingReason string
	var existingReset sql.NullString
	if err := a.db.QueryRow(`SELECT quota_5h_threshold_enabled, quota_5h_threshold_percent, rate_limit_reason, rate_limit_reset_at
		FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&enabled, &threshold, &existingReason, &existingReset); err != nil {
		return err
	}
	if enabled != 1 || window.Utilization < float64(threshold) {
		return a.clearAccountFiveHourThresholdCooldown(accountID)
	}
	now := time.Now().UTC()
	resetAt, valid := parseQuotaResetTime(window.ResetsAt)
	if !valid || !resetAt.After(now) {
		return a.clearAccountFiveHourThresholdCooldown(accountID)
	}
	eligibleAt, err := a.accountFiveHourThresholdReleaseDeadline(accountID, resetAt, policy)
	if err != nil {
		return err
	}
	if existingReset.Valid {
		if current, ok := parseQuotaResetTime(existingReset.String); ok && current.After(now) && existingReason != "quota_threshold" {
			return nil
		}
	}
	message := fmt.Sprintf("5h 使用率 %.1f%% 已达到账户阈值 %d%%，提前冷却至 %s", window.Utilization, threshold, eligibleAt.Local().Format("01/02 15:04"))
	_, err = a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, rate_limit_window = '5h', rate_limit_reason = 'quota_threshold',
		quota_refreshed_at = NULL, error_message = ?, updated_at = `+nowSQL+`
		WHERE id = ? AND deleted_at IS NULL AND quota_5h_threshold_enabled = 1
		AND quota_5h_utilization >= quota_5h_threshold_percent`, eligibleAt.Format(time.RFC3339Nano), message, accountID)
	return err
}

func unixFromTime(value string) int64 {
	if value == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, value)
	}
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

func quotaFromHeaders(headers http.Header) (accountQuota, bool) {
	quota := accountQuota{Source: "passive", UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	found := false
	if value, err := strconv.ParseFloat(strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-5h-utilization")), 64); err == nil {
		quota.FiveHour.Utilization = value * 100
		found = true
	}
	if value, err := strconv.ParseFloat(strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-7d-utilization")), 64); err == nil {
		quota.SevenDay.Utilization = value * 100
		found = true
	}
	quota.FiveHour.ResetsAt = resetHeaderTime(headers.Get("anthropic-ratelimit-unified-5h-reset"))
	quota.SevenDay.ResetsAt = resetHeaderTime(headers.Get("anthropic-ratelimit-unified-7d-reset"))
	if quota.FiveHour.ResetsAt != "" || quota.SevenDay.ResetsAt != "" {
		found = true
	}
	return quota, found
}

func quotaWindowMatchingReset(rateLimitResetAt, fiveHourResetAt, sevenDayResetAt string) string {
	limitReset, ok := parseQuotaResetTime(rateLimitResetAt)
	if !ok {
		return ""
	}
	matches := func(raw string) bool {
		reset, ok := parseQuotaResetTime(raw)
		return ok && absDuration(limitReset.Sub(reset)) <= time.Second
	}
	if matches(sevenDayResetAt) {
		return "7d"
	}
	if matches(fiveHourResetAt) {
		return "5h"
	}
	return ""
}

func parseQuotaResetTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, raw)
	}
	return parsed, err == nil
}

func absDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

func resetHeaderTime(raw string) string {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return ""
	}
	if value > 1e11 {
		value /= 1000
	}
	return time.Unix(value, 0).UTC().Format(time.RFC3339Nano)
}

func (a *app) captureAccountUpstreamState(accountID int64, response *http.Response) {
	a.captureAccountUpstreamStateWithPolicy(accountID, response, rateLimitPolicy{})
}

func (a *app) captureAccountUpstreamStateWithPolicy(accountID int64, response *http.Response, policy rateLimitPolicy) {
	if response == nil {
		return
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		_, err := a.db.Exec(`UPDATE accounts SET auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, accountID)
		logDatabaseWriteError("record valid upstream account", err)
	}
	if quota, ok := quotaFromHeaders(response.Header); ok {
		if err := a.persistAccountQuota(accountID, quota, false); err != nil {
			logDatabaseWriteError("record account quota headers", err)
		} else if strings.TrimSpace(response.Header.Get("anthropic-ratelimit-unified-5h-utilization")) != "" && quota.FiveHour.ResetsAt != "" {
			if err := a.enforceAccountFiveHourThreshold(accountID, quota.FiveHour, policy); err != nil {
				logDatabaseWriteError("enforce account 5h quota threshold", err)
			}
		}
	}
	a.captureAccountRateLimitBudget(accountID, response.Header)
	resetAt, window, ok := sub2service.ResolveCCMaxCompatibilityCooldownWindow(response.StatusCode, response.Header)
	if !ok {
		return
	}
	if window == "" {
		// A bare 429 names no exhausted window. Only an explicit retry-after is
		// acted on here: it is an upstream instruction, so it applies whatever
		// the group's adaptive-cooling switch says, and ignoring it means
		// hammering an account that just asked us to back off — which is what
		// turns one 429 into a failover, a cold cache, and a far larger ITPM
		// charge. Without the header the wait would be our own heuristic, and
		// that belongs to captureAccount429State where the policy is applied.
		deadline, found := retryAfterDeadline(response.Header, time.Now().UTC())
		if !found {
			return
		}
		stamp := deadline.UTC().Format(time.RFC3339Nano)
		_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, updated_at = `+nowSQL+`
			WHERE id = ? AND deleted_at IS NULL AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at < ?)`,
			stamp, accountID, stamp)
		logDatabaseWriteError("record account retry-after cooldown", err)
		return
	}
	// Keep the sampled quota reset untouched, but stagger quota-limited accounts
	// before making them dispatchable so a large cohort does not return at once.
	eligibleAt := policy.quotaReleaseDeadline(accountID, window, resetAt.UTC())
	_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, rate_limit_window = ?, updated_at = `+nowSQL+` WHERE id = ?`, eligibleAt.Format(time.RFC3339Nano), window, accountID)
	logDatabaseWriteError("record account quota cooldown", err)
}

// Guards against a pathological retry-after parking an account for days.
const maxRetryAfterWait = time.Hour

// retryAfterDeadline reads RFC 7231 retry-after, which is either delta-seconds
// or an HTTP-date. Anthropic sends it on every rate-limit 429, so it is the most
// authoritative wait available — strictly better than any local estimate.
func retryAfterDeadline(headers http.Header, now time.Time) (time.Time, bool) {
	raw := strings.TrimSpace(headers.Get("retry-after"))
	if raw == "" {
		return time.Time{}, false
	}
	if seconds, err := strconv.Atoi(raw); err == nil {
		if seconds < 0 {
			return time.Time{}, false
		}
		if maximum := int(maxRetryAfterWait / time.Second); seconds > maximum {
			seconds = maximum
		}
		return now.Add(time.Duration(seconds) * time.Second), true
	}
	if at, err := http.ParseTime(raw); err == nil {
		if !at.After(now) {
			return time.Time{}, false
		}
		if at.After(now.Add(maxRetryAfterWait)) {
			return now.Add(maxRetryAfterWait), true
		}
		return at, true
	}
	return time.Time{}, false
}

// captureAccountRateLimitBudget records the upstream's own view of how much
// per-minute input budget the account has left. Anthropic reports this on every
// response, which removes the need to predict a request's token count before
// sending: the dispatcher reads the remaining budget instead of guessing it.
func (a *app) captureAccountRateLimitBudget(accountID int64, headers http.Header) {
	raw := strings.TrimSpace(headers.Get("anthropic-ratelimit-input-tokens-remaining"))
	if raw == "" {
		return
	}
	remaining, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || remaining < 0 {
		return
	}
	resetAt := ""
	if at, parseErr := time.Parse(time.RFC3339, strings.TrimSpace(headers.Get("anthropic-ratelimit-input-tokens-reset"))); parseErr == nil {
		resetAt = at.UTC().Format(time.RFC3339Nano)
	}
	_, err = a.db.Exec(`UPDATE accounts SET itpm_remaining = ?, itpm_reset_at = NULLIF(?, ''), itpm_sampled_at = `+nowSQL+`, updated_at = `+nowSQL+`
		WHERE id = ? AND deleted_at IS NULL`, remaining, resetAt, accountID)
	logDatabaseWriteError("record account ITPM budget", err)
}

func modelCooldownKey(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func normalizeOverloadCooldownSeconds(seconds int) int {
	if seconds <= 0 {
		return 10
	}
	if seconds > 600 {
		return 600
	}
	return seconds
}

func (a *app) captureAccountModelOverload(accountID int64, model string, seconds int) {
	model = modelCooldownKey(model)
	if accountID <= 0 || model == "" {
		return
	}
	resetAt := time.Now().UTC().Add(time.Duration(normalizeOverloadCooldownSeconds(seconds)) * time.Second).Format(time.RFC3339Nano)
	_, err := a.db.Exec(`INSERT INTO account_model_cooldowns (account_id, model, reset_at) VALUES (?, ?, ?)
		ON CONFLICT(account_id, model) DO UPDATE SET reset_at = excluded.reset_at, updated_at = `+nowSQL, accountID, model, resetAt)
	logDatabaseWriteError("record account model cooldown", err)
}

// captureGatewayUpstreamState records what the upstream reported. A successful
// response deliberately clears nothing: hitting a rate limit is a fact about
// the account's current quota window, and later successes inside that same
// window do not make it untrue. Only the window rolling over — or an
// administrator — lifts the penalty.
// rateLimitPolicy is the group's 429 handling configuration. Accounts can sit
// in several groups, so the policy of the group that served the request decides
// how that request's 429 is recorded — the same shape as the 529 cooldown.
type rateLimitPolicy struct {
	DownweightEnabled      bool
	CoolingThreshold       int
	CooldownSeconds        int
	SteppedCooldownEnabled bool
	CooldownStepSeconds    int
	DownweightStepped      bool
	DownweightBaseMinutes  int
	DownweightStepMinutes  int
	FiveHourStaggerSet     bool
	FiveHourStaggerEnabled bool
	FiveHourStaggerMin     int
	FiveHourStaggerMax     int
}

func (p rateLimitPolicy) threshold() int {
	if p.CoolingThreshold < 1 {
		return defaultRateLimitCoolingThreshold
	}
	if p.CoolingThreshold > maxRateLimitCoolingThreshold {
		return maxRateLimitCoolingThreshold
	}
	return p.CoolingThreshold
}

func (p rateLimitPolicy) cooldownSeconds() int {
	if p.CooldownSeconds < minRateLimitCooldownSeconds || p.CooldownSeconds > maxRateLimitCooldownSeconds {
		return defaultRateLimitCooldownSeconds
	}
	return p.CooldownSeconds
}

func (p rateLimitPolicy) cooldownStepSeconds() int {
	if p.CooldownStepSeconds < 1 || p.CooldownStepSeconds > maxRateLimitCooldownStepSeconds {
		return defaultRateLimitCooldownStepSeconds
	}
	return p.CooldownStepSeconds
}

func (p rateLimitPolicy) cooldownForStrikes(strikes int) int {
	strikes = max(1, strikes)
	maximum := p.cooldownSeconds()
	if p.SteppedCooldownEnabled {
		return min(maximum, minRateLimitCooldownSeconds+(strikes-1)*p.cooldownStepSeconds())
	}
	cooldown := minRateLimitCooldownSeconds
	if strikes > 1 {
		cooldown += (maximum - minRateLimitCooldownSeconds) / 2
	}
	if strikes >= p.threshold() {
		cooldown = maximum
	}
	return cooldown
}

func (p rateLimitPolicy) downweightMinutesForStrikes(strikes int) int {
	base := p.DownweightBaseMinutes
	if base < minRateLimitDownweightMinutes || base > maxRateLimitDownweightMinutes {
		base = defaultRateLimitDownweightBaseMinutes
	}
	step := p.DownweightStepMinutes
	if step < minRateLimitDownweightMinutes || step > maxRateLimitDownweightMinutes {
		step = defaultRateLimitDownweightStepMinutes
	}
	return min(maxRateLimitDownweightMinutes, base+(max(1, strikes)-1)*step)
}

func (p rateLimitPolicy) fiveHourReleaseRange() (bool, time.Duration, time.Duration) {
	if p.FiveHourStaggerSet && !p.FiveHourStaggerEnabled {
		return false, 0, 0
	}
	minimum, maximum := p.FiveHourStaggerMin, p.FiveHourStaggerMax
	if minimum < 0 || maximum < minimum || maximum > maxFiveHourReleaseStaggerMinutes || (!p.FiveHourStaggerSet && minimum == 0 && maximum == 0) {
		minimum = defaultFiveHourReleaseStaggerMinMinutes
		maximum = defaultFiveHourReleaseStaggerMaxMinutes
	}
	return true, time.Duration(minimum) * time.Minute, time.Duration(maximum) * time.Minute
}

func (p rateLimitPolicy) quotaReleaseDeadline(accountID int64, window string, resetAt time.Time) time.Time {
	switch window {
	case "5h":
		enabled, minimum, maximum := p.fiveHourReleaseRange()
		if !enabled {
			return resetAt.UTC()
		}
		return staggeredAccountQuotaReleaseRange(accountID, resetAt, minimum, maximum)
	case "7d":
		return staggeredAccountQuotaRelease(accountID, resetAt)
	default:
		return resetAt.UTC()
	}
}

func (a *app) captureGatewayUpstreamState(accountID int64, groupID, model string, overloadCooldownSeconds int, policy rateLimitPolicy, response *http.Response) {
	a.captureAccountUpstreamStateWithPolicy(accountID, response, policy)
	if response == nil {
		return
	}
	if response.StatusCode == http.StatusTooManyRequests {
		downweightUntil, transient := a.captureAccount429State(accountID, policy, response)
		if transient {
			// The learned peak and the scheduling penalty are one policy. Keeping
			// their deadlines aligned ensures the configurable downweight staircase
			// actually releases both parts of the penalty at the same time.
			a.captureAccountRPMThreshold(groupID, accountID, downweightUntil)
		}
	}
	if response.StatusCode == 529 {
		a.captureAccountModelOverload(accountID, model, overloadCooldownSeconds)
	}
}

const (
	// How many strikes escalate an account to the maximum configured short
	// cooldown. The group can override it; it never turns an ambiguous 429 into
	// a synthetic 5h quota exhaustion.
	defaultRateLimitCoolingThreshold        = 3
	maxRateLimitCoolingThreshold            = 10
	minRateLimitCooldownSeconds             = 60
	defaultRateLimitCooldownSeconds         = 120
	maxRateLimitCooldownSeconds             = 120
	defaultRateLimitCooldownStepSeconds     = 30
	maxRateLimitCooldownStepSeconds         = maxRateLimitCooldownSeconds - minRateLimitCooldownSeconds
	minRateLimitDownweightMinutes           = 1
	defaultRateLimitDownweightBaseMinutes   = 60
	defaultRateLimitDownweightStepMinutes   = 60
	maxRateLimitDownweightMinutes           = 5*60 + 15
	defaultFiveHourReleaseStaggerMinMinutes = 15
	defaultFiveHourReleaseStaggerMaxMinutes = 30
	maxFiveHourReleaseStaggerMinutes        = 315
	highQuota429UtilizationPercent          = 90
	highQuotaSampleFreshness                = 10 * time.Minute
	accountQuotaReleaseDelayMin             = 15 * time.Minute
	accountQuotaReleaseDelayMax             = 30 * time.Minute
	// Upstream rate limiting hits every in-flight request on an account at
	// once. Without a debounce a concurrency-3 account would collect three
	// strikes from a single burst, so strikes within this window count once.
	account429DebounceWindow = time.Minute
	// Each independent 429 strike lowers the account's observed peak by another
	// quarter. The temporary threshold is a hard admission limit, including for
	// sticky sessions, so the remaining traffic is forced onto another account.
	account429RPMReductionPercentPerStrike = 25
	account429RPMMinimumPercent            = 25
	// An account whose quota window just rolled over has a full allowance, so
	// it is the best candidate available. The boost is time-boxed to keep it
	// from monopolising traffic for the rest of the window.
	accountQuotaFreshPriorityWindow = 30 * time.Minute
)

// captureAccount429State keeps rate-limit pressure separate from the
// administrator-controlled schedulable flag. Explicit quota exhaustion follows
// the upstream reset window. Ambiguous 429s first enter a short cooldown, then
// return as downweighted accounts with a learned RPM ceiling. Repeated strikes
// can increase both local durations when their group enables the corresponding
// staircase; a 5h/7d park still requires an explicit upstream quota-window
// signal.
func (a *app) captureAccount429State(accountID int64, policy rateLimitPolicy, response *http.Response) (time.Time, bool) {
	if accountID <= 0 {
		return time.Time{}, false
	}
	now := time.Now().UTC()
	_, explicitWindow, _ := sub2service.ResolveCCMaxCompatibilityCooldownWindow(http.StatusTooManyRequests, response.Header)
	if explicitWindow == "5h" || explicitWindow == "7d" {
		quotaResetAt := a.nextAccountQuotaReset(accountID, response.Header, now, explicitWindow)
		eligibleAt := quotaResetAt
		message := "上游 " + explicitWindow + " 配额窗口已满，等待窗口刷新"
		if explicitWindow == "5h" || explicitWindow == "7d" {
			eligibleAt = policy.quotaReleaseDeadline(accountID, explicitWindow, quotaResetAt)
			if eligibleAt.After(quotaResetAt) {
				message = fmt.Sprintf("上游 %s 配额窗口已满，刷新后错峰等待至 %s", explicitWindow, eligibleAt.Local().Format("01/02 15:04"))
			}
		}
		_, err := a.db.Exec(`UPDATE accounts SET
			consecutive_429 = consecutive_429 + 1,
			last_429_at = ?, rate_limit_reset_at = ?, rate_limit_window = ?,
			rate_limit_downweight_until = ?, quota_refreshed_at = NULL,
			rate_limit_reason = 'quota_exhausted', error_message = ?, updated_at = `+nowSQL+`
			WHERE id = ? AND deleted_at IS NULL`,
			now.Format(time.RFC3339Nano), eligibleAt.Format(time.RFC3339Nano), explicitWindow,
			eligibleAt.Format(time.RFC3339Nano), message, accountID)
		logDatabaseWriteError("record exhausted account quota", err)
		return eligibleAt, false
	}
	if !policy.DownweightEnabled {
		return time.Time{}, false
	}
	// Multiple in-flight requests can observe the same upstream burst after the
	// first response has already parked the account. They are one rate-limit
	// event and must not extend the cooldown or manufacture extra strikes.
	var existingReason string
	var existingReset sql.NullString
	if err := a.db.QueryRow(`SELECT rate_limit_reason, rate_limit_reset_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&existingReason, &existingReset); err == nil && existingReset.Valid && (existingReason == "429_cooling" || existingReason == "quota_exhausted" || existingReason == "quota_threshold") {
		if parsed, parseErr := parseQuotaResetTime(existingReset.String); parseErr && parsed.After(now) {
			return parsed, existingReason == "429_cooling"
		}
	}

	// An ordinary 429 is transient pressure, not proof that a 5h/7d allowance
	// is exhausted. The group's single switch owns strike learning, temporary
	// RPM threshold capture, downweighting and this short cooldown together.
	debounceCutoff := now.Add(-account429DebounceWindow).Format(time.RFC3339Nano)
	_, err := a.db.Exec(`UPDATE accounts SET
		consecutive_429 = CASE
			WHEN last_429_at IS NOT NULL AND last_429_at > ? THEN consecutive_429
			ELSE consecutive_429 + 1
		END,
		last_429_at = CASE WHEN last_429_at IS NOT NULL AND last_429_at > ? THEN last_429_at ELSE ? END,
		updated_at = `+nowSQL+`
		WHERE id = ? AND deleted_at IS NULL`,
		debounceCutoff, debounceCutoff, now.Format(time.RFC3339Nano), accountID)
	if err != nil {
		logDatabaseWriteError("record transient account 429", err)
		return time.Time{}, false
	}
	strikes := 1
	_ = a.db.QueryRow(`SELECT consecutive_429 FROM accounts WHERE id = ?`, accountID).Scan(&strikes)
	retryAfterAt, hasRetryAfter := retryAfterDeadline(response.Header, now)
	downweightUntil, downweightMessage := a.transient429DownweightDeadline(accountID, policy, response.Header, now, strikes, retryAfterAt, hasRetryAfter)
	cooldownSeconds := policy.cooldownForStrikes(strikes)
	cooldown := time.Duration(cooldownSeconds) * time.Second
	resetAt := now.Add(cooldown)
	if hasRetryAfter {
		resetAt = retryAfterAt
	}
	message := fmt.Sprintf("上游瞬时 429，账号短暂冷却 %s；%s", resetAt.Sub(now).Round(time.Second), downweightMessage)
	if cooldownSeconds >= policy.cooldownSeconds() {
		message = fmt.Sprintf("连续 %d 次上游 429，账号短暂冷却 %s；%s", strikes, resetAt.Sub(now).Round(time.Second), downweightMessage)
	}
	// Never shorten a cooldown already in force: captureAccountUpstreamState runs
	// first and may have recorded an explicit retry-after of up to an hour, which
	// this local 60-120s default must not clamp away.
	stamp := resetAt.Format(time.RFC3339Nano)
	_, err = a.db.Exec(`UPDATE accounts SET
		rate_limit_reset_at = CASE WHEN rate_limit_reset_at IS NULL OR rate_limit_reset_at < ? THEN ? ELSE rate_limit_reset_at END,
		rate_limit_window = '', rate_limit_reason = '429_cooling', rate_limit_downweight_until = ?,
		quota_refreshed_at = NULL, error_message = ?, updated_at = `+nowSQL+`
		WHERE id = ? AND deleted_at IS NULL`, stamp, stamp, downweightUntil.Format(time.RFC3339Nano), message, accountID)
	if err != nil {
		logDatabaseWriteError("record transient account 429 cooldown", err)
		return time.Time{}, false
	}
	return downweightUntil, true
}

func (a *app) transient429DownweightDeadline(accountID int64, policy rateLimitPolicy, headers http.Header, now time.Time, strikes int, retryAfterAt time.Time, hasRetryAfter bool) (time.Time, string) {
	if hasRetryAfter {
		return retryAfterAt, fmt.Sprintf("上游已明确返回冷却时间，降峰同步持续至 %s", retryAfterAt.Local().Format("01/02 15:04:05"))
	}
	if !policy.DownweightStepped {
		deadline := a.nextAccount5HReset(accountID, headers, now)
		return deadline, "随后按降峰 RPM 运行至 5h 窗口刷新"
	}
	if utilization, ok := a.recentFiveHourUtilization(accountID, headers, now); ok && utilization >= highQuota429UtilizationPercent {
		deadline := now.Add(time.Duration(maxRateLimitDownweightMinutes) * time.Minute)
		return deadline, fmt.Sprintf("5h 用量 %.0f%%，直接采用最大降峰时间 %d 分钟", utilization, maxRateLimitDownweightMinutes)
	}
	minutes := policy.downweightMinutesForStrikes(strikes)
	deadline := now.Add(time.Duration(minutes) * time.Minute)
	return deadline, fmt.Sprintf("第 %d 次独立 429 按阶梯降峰 %d 分钟", max(1, strikes), minutes)
}

func (a *app) recentFiveHourUtilization(accountID int64, headers http.Header, now time.Time) (float64, bool) {
	raw := strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-5h-utilization"))
	if raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil && value >= 0 {
			return value * 100, true
		}
	}
	var utilization float64
	var sampledAt any
	if err := a.db.QueryRow(`SELECT quota_5h_utilization, quota_sampled_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&utilization, &sampledAt); err != nil {
		return 0, false
	}
	sampled, ok := databaseQuotaTime(sampledAt)
	if !ok || sampled.After(now.Add(time.Minute)) || now.Sub(sampled) > highQuotaSampleFreshness {
		return 0, false
	}
	return utilization, true
}

func (a *app) nextAccount5HReset(accountID int64, headers http.Header, now time.Time) time.Time {
	if quota, ok := quotaFromHeaders(headers); ok {
		if resetAt, valid := parseQuotaResetTime(quota.FiveHour.ResetsAt); valid && resetAt.After(now) && !resetAt.After(now.Add(6*time.Hour)) {
			return resetAt
		}
	}
	var raw sql.NullString
	if err := a.db.QueryRow(`SELECT quota_5h_reset_at FROM accounts WHERE id = ?`, accountID).Scan(&raw); err == nil && raw.Valid {
		if resetAt, valid := parseQuotaResetTime(raw.String); valid && resetAt.After(now) && !resetAt.After(now.Add(6*time.Hour)) {
			return resetAt
		}
	}
	return now.Add(5 * time.Hour)
}

// staggeredAccountQuotaRelease returns a stable pseudo-random instant for one
// account and quota window. Both 5h and 7d quota releases use it. Stability
// matters because the generic quota recorder and gateway classification can
// observe the same response; they must compute the same release instead of
// extending it on every write.
func staggeredAccountQuotaRelease(accountID int64, quotaResetAt time.Time) time.Time {
	return staggeredAccountQuotaReleaseRange(accountID, quotaResetAt, accountQuotaReleaseDelayMin, accountQuotaReleaseDelayMax)
}

func staggeredAccountQuotaReleaseRange(accountID int64, quotaResetAt time.Time, minimum, maximum time.Duration) time.Time {
	if minimum < 0 || maximum < minimum {
		minimum = accountQuotaReleaseDelayMin
		maximum = accountQuotaReleaseDelayMax
	}
	seed := fmt.Sprintf("%d:%d", accountID, quotaResetAt.UTC().Unix())
	digest := sha256.Sum256([]byte(seed))
	spanSeconds := uint64((maximum-minimum)/time.Second) + 1
	offset := time.Duration(binary.BigEndian.Uint64(digest[:8])%spanSeconds) * time.Second
	return quotaResetAt.UTC().Add(minimum + offset)
}

type quotaReleaseRepair struct {
	accountID      int64
	window         string
	rateLimitReset time.Time
	quotaReset     time.Time
}

func databaseQuotaTime(value any) (time.Time, bool) {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC(), !typed.IsZero()
	case string:
		return parseQuotaResetTime(typed)
	case []byte:
		return parseQuotaResetTime(string(typed))
	default:
		return time.Time{}, false
	}
}

// enforceQuotaReleaseStagger upgrades only legacy 429_cooling rows. New
// quota_exhausted rows already carry the triggering group's policy and must not
// be extended on restart when that group has disabled 5h staggering.
func (a *app) enforceQuotaReleaseStagger() {
	rows, err := a.db.Query(`SELECT id, rate_limit_window, rate_limit_reset_at,
		quota_5h_reset_at, quota_7d_reset_at
		FROM accounts
		WHERE deleted_at IS NULL
		AND rate_limit_reason = '429_cooling'
		AND rate_limit_reset_at IS NOT NULL AND rate_limit_reset_at > ` + nowSQL)
	if err != nil {
		logDatabaseWriteError("read legacy quota release deadlines", err)
		return
	}
	repairs := make([]quotaReleaseRepair, 0)
	for rows.Next() {
		var accountID int64
		var window string
		var rateLimitRaw, fiveHourRaw, sevenDayRaw any
		if err := rows.Scan(&accountID, &window, &rateLimitRaw, &fiveHourRaw, &sevenDayRaw); err != nil {
			logDatabaseWriteError("scan legacy quota release deadline", err)
			continue
		}
		rateLimitReset, valid := databaseQuotaTime(rateLimitRaw)
		if !valid {
			continue
		}
		fiveHourReset, hasFiveHour := databaseQuotaTime(fiveHourRaw)
		sevenDayReset, hasSevenDay := databaseQuotaTime(sevenDayRaw)
		if window == "" {
			switch {
			case hasSevenDay && absDuration(rateLimitReset.Sub(sevenDayReset)) <= time.Second:
				window = "7d"
			case hasFiveHour && absDuration(rateLimitReset.Sub(fiveHourReset)) <= time.Second:
				window = "5h"
			}
		}
		quotaReset := time.Time{}
		switch window {
		case "5h":
			if hasFiveHour {
				quotaReset = fiveHourReset
			}
		case "7d":
			if hasSevenDay {
				quotaReset = sevenDayReset
			}
		}
		if quotaReset.IsZero() {
			continue
		}
		target := staggeredAccountQuotaRelease(accountID, quotaReset)
		if !target.After(rateLimitReset) {
			continue
		}
		repairs = append(repairs, quotaReleaseRepair{
			accountID:      accountID,
			window:         window,
			rateLimitReset: target,
			quotaReset:     quotaReset,
		})
	}
	if err := rows.Close(); err != nil {
		logDatabaseWriteError("close legacy quota release rows", err)
	}
	if err := rows.Err(); err != nil {
		logDatabaseWriteError("iterate legacy quota release deadlines", err)
	}
	for _, repair := range repairs {
		stamp := repair.rateLimitReset.Format(time.RFC3339Nano)
		message := fmt.Sprintf("上游 %s 配额窗口已满，额度于 %s 刷新，错峰等待至 %s",
			repair.window,
			repair.quotaReset.Local().Format("01/02 15:04"),
			repair.rateLimitReset.Local().Format("01/02 15:04"))
		_, err := a.db.Exec(`UPDATE accounts SET
			rate_limit_reset_at = ?, rate_limit_window = ?, rate_limit_reason = 'quota_exhausted',
			rate_limit_downweight_until = CASE
				WHEN rate_limit_downweight_until IS NULL OR rate_limit_downweight_until < ? THEN ?
				ELSE rate_limit_downweight_until
			END,
			error_message = CASE WHEN auth_error = '' THEN ? ELSE error_message END, updated_at = `+nowSQL+`
			WHERE id = ? AND deleted_at IS NULL
			AND rate_limit_reason = '429_cooling'
			AND (rate_limit_window = ? OR rate_limit_window = '')
			AND rate_limit_reset_at IS NOT NULL AND rate_limit_reset_at < ?`,
			stamp, repair.window, stamp, stamp, message, repair.accountID, repair.window, stamp)
		logDatabaseWriteError("stagger legacy quota release deadline", err)
	}
}

// nextAccountQuotaReset resolves when the penalty should lift. When the
// upstream named the exhausted window we follow that one; otherwise the 5h
// window governs, since it is the one an ordinary 429 reflects.
func (a *app) nextAccountQuotaReset(accountID int64, headers http.Header, now time.Time, window string) time.Time {
	if window != "7d" {
		return a.nextAccount5HReset(accountID, headers, now)
	}
	if quota, ok := quotaFromHeaders(headers); ok {
		if resetAt, valid := parseQuotaResetTime(quota.SevenDay.ResetsAt); valid && resetAt.After(now) && !resetAt.After(now.Add(8*24*time.Hour)) {
			return resetAt
		}
	}
	var raw sql.NullString
	if err := a.db.QueryRow(`SELECT quota_7d_reset_at FROM accounts WHERE id = ?`, accountID).Scan(&raw); err == nil && raw.Valid {
		if resetAt, valid := parseQuotaResetTime(raw.String); valid && resetAt.After(now) && !resetAt.After(now.Add(8*24*time.Hour)) {
			return resetAt
		}
	}
	return now.Add(7 * 24 * time.Hour)
}

// clearAccount429State lifts the rate-limit penalty. It is the manual recovery
// path; the sweeper does the same thing automatically once the quota window
// rolls over. Accounts released this way get the fresh-window priority boost,
// because a released account is one whose allowance is believed to be back.
//
// It only releases the cooldown that the 429 path itself set: rate_limit_reset_at
// is also written by the stream-idle guard and the token refresh backoff, and
// clearing those would discard a cooldown this code does not own.
func (a *app) clearAccount429State(accountID int64) (bool, error) {
	if accountID <= 0 {
		return false, sql.ErrNoRows
	}
	tx, err := a.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, sql.ErrNoRows
	}
	result, err := tx.Exec(`UPDATE accounts SET
		rate_limit_reset_at = CASE WHEN rate_limit_reason IN ('429_cooling', 'quota_exhausted', 'quota_threshold') THEN NULL ELSE rate_limit_reset_at END,
		rate_limit_window = CASE WHEN rate_limit_reason IN ('429_cooling', 'quota_exhausted', 'quota_threshold') THEN '' ELSE rate_limit_window END,
		error_message = CASE WHEN rate_limit_reason != '' AND auth_error = '' THEN '' ELSE error_message END,
		rate_limit_reason = '', consecutive_429 = 0, last_429_at = NULL,
		rate_limit_downweight_until = NULL, quota_refreshed_at = `+nowSQL+`, updated_at = `+nowSQL+`
		WHERE id = ? AND deleted_at IS NULL AND (consecutive_429 > 0 OR rate_limit_reason != '' OR rate_limit_downweight_until IS NOT NULL)`, accountID)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	result, err = tx.Exec(`DELETE FROM account_rpm_thresholds WHERE account_id = ?`, accountID)
	if err != nil {
		return false, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return updated > 0 || deleted > 0, nil
}

const accountRateLimitSweepInterval = time.Minute

// startAccountRateLimitSweeper tidies expired rate-limit state in the
// background. This is display-state cleanup only, never a dispatch dependency:
// accountStatePredicate already admits an account whose rate_limit_reset_at has
// passed, so an unswept row is dispatchable on its next request regardless. A
// stale strike count only affects the ORDER BY tie-break, and captureAccount429State
// self-heals it on the next strike.
//
// It also has to stay out of the dispatch transaction, where it was an
// unbounded UPDATE on every request taking locks in the opposite order from
// captureAccount429State's WHERE id = ? — a deadlock shape on MySQL.
func (a *app) startAccountRateLimitSweeper() func() {
	ctx, cancel := context.WithCancel(context.Background())
	var wait sync.WaitGroup
	wait.Add(1)
	go func() {
		defer wait.Done()
		ticker := time.NewTicker(accountRateLimitSweepInterval)
		defer ticker.Stop()
		a.enforceQuotaReleaseStagger()
		a.sweepAccountRateLimitState()
		a.sweepExpiredRuntimeState()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.sweepAccountRateLimitState()
				a.sweepExpiredRuntimeState()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wait.Wait()
		})
	}
}

const runtimeCleanupBatchSize = 5000

func (a *app) sweepExpiredRuntimeState() {
	queries := []string{
		`DELETE FROM account_rpm_events WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`,
		`DELETE FROM dispatch_sessions WHERE expires_at <= ` + nowSQL,
		`DELETE FROM account_model_cooldowns WHERE reset_at <= ` + nowSQL,
		`DELETE FROM account_rpm_thresholds
			WHERE reset_at <= ` + nowSQL + `
			OR NOT EXISTS (
				SELECT 1 FROM accounts a
				WHERE a.id = account_rpm_thresholds.account_id
				AND a.rate_limit_downweight_until IS NOT NULL
				AND a.rate_limit_downweight_until > ` + nowSQL + `
			)`,
	}
	for _, query := range queries {
		for batch := 0; batch < 4; batch++ {
			statement := query
			if a.db.dialect == dialectMySQL {
				statement += ` LIMIT ` + strconv.Itoa(runtimeCleanupBatchSize)
			}
			result, err := a.db.Exec(statement)
			if err != nil {
				logDatabaseWriteError("sweep expired runtime state", err)
				break
			}
			deleted, err := result.RowsAffected()
			if err != nil || a.db.dialect != dialectMySQL || deleted < runtimeCleanupBatchSize {
				break
			}
		}
	}
}

func (a *app) sweepAccountRateLimitState() {
	// A short transient cooldown returns to downweighted scheduling while the
	// learned RPM ceiling remains live. Only explicit quota exhaustion stays
	// parked until its upstream quota window rolls.
	_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = NULL, rate_limit_window = '',
		rate_limit_reason = CASE WHEN rate_limit_downweight_until > ` + nowSQL + ` THEN '429_backoff' ELSE '' END,
		consecutive_429 = CASE WHEN rate_limit_downweight_until > ` + nowSQL + ` THEN consecutive_429 ELSE 0 END,
		last_429_at = CASE WHEN rate_limit_downweight_until > ` + nowSQL + ` THEN last_429_at ELSE NULL END,
		quota_refreshed_at = CASE
			WHEN rate_limit_reason IN ('quota_exhausted', 'quota_threshold') AND rate_limit_window = '5h' THEN COALESCE(quota_5h_reset_at, ` + nowSQL + `)
			WHEN rate_limit_reason = 'quota_exhausted' AND rate_limit_window = '7d' THEN COALESCE(quota_7d_reset_at, ` + nowSQL + `)
			WHEN rate_limit_reason = 'quota_exhausted' THEN ` + nowSQL + `
			ELSE quota_refreshed_at
		END,
		error_message = CASE
			WHEN auth_error != '' THEN error_message
			WHEN rate_limit_downweight_until > ` + nowSQL + ` THEN '上游 429，已降低调度权重和峰值 RPM，等待当前降峰时间结束'
			ELSE ''
		END,
		updated_at = ` + nowSQL + `
		WHERE rate_limit_reason IN ('429_cooling', 'quota_exhausted', 'quota_threshold')
		AND rate_limit_reset_at IS NOT NULL AND rate_limit_reset_at <= ` + nowSQL)
	logDatabaseWriteError("release expired account rate-limit cooldowns", err)
	// Remove an expired priority penalty. Explicit quota refreshes are stamped
	// by the branch above; a legacy backoff row alone is
	// not proof that upstream quota was replenished.
	_, err = a.db.Exec(`UPDATE accounts SET rate_limit_reason = '', consecutive_429 = 0, last_429_at = NULL,
		rate_limit_downweight_until = NULL,
		error_message = CASE WHEN auth_error = '' THEN '' ELSE error_message END, updated_at = ` + nowSQL + `
		WHERE rate_limit_downweight_until IS NOT NULL AND rate_limit_downweight_until <= ` + nowSQL + `
		AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at <= ` + nowSQL + `)`)
	logDatabaseWriteError("release account rate-limit downweight", err)
	a.restoreMissing429RPMThresholds()
	a.pruneCachePrefixEvents()
}

// Old builds kept the learned RPM ceiling for only one minute. Rebuild a
// missing ceiling from the live rolling window so an already-downweighted
// account becomes effective immediately after an upgrade instead of waiting for
// another upstream 429.
func (a *app) restoreMissing429RPMThresholds() {
	rows, err := a.db.Query(`SELECT a.id, a.rate_limit_downweight_until
		FROM accounts a
		WHERE a.deleted_at IS NULL AND a.rate_limit_reason = '429_backoff'
		AND a.rate_limit_downweight_until IS NOT NULL AND a.rate_limit_downweight_until > ` + nowSQL + `
		AND EXISTS (SELECT 1 FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds'))
		AND NOT EXISTS (SELECT 1 FROM account_rpm_thresholds t WHERE t.account_id = a.id AND t.reset_at > ` + nowSQL + `)`)
	if err != nil {
		return
	}
	type pendingThreshold struct {
		accountID int64
		resetAt   time.Time
	}
	pending := []pendingThreshold{}
	for rows.Next() {
		var accountID int64
		var rawReset string
		if scanErr := rows.Scan(&accountID, &rawReset); scanErr != nil {
			continue
		}
		if resetAt, valid := parseQuotaResetTime(rawReset); valid {
			pending = append(pending, pendingThreshold{accountID: accountID, resetAt: resetAt})
		}
	}
	rows.Close()
	for _, item := range pending {
		a.captureAccountRPMThreshold("", item.accountID, item.resetAt)
	}
}

func (a *app) captureAccountRPMThreshold(groupID string, accountID int64, resetAt time.Time) {
	if accountID <= 0 {
		return
	}
	if groupID != "" {
		lockValue, _ := a.dispatchLocks.LoadOrStore(groupID, &sync.Mutex{})
		dispatchLock := lockValue.(*sync.Mutex)
		dispatchLock.Lock()
		defer dispatchLock.Unlock()
	}
	tx, err := a.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM account_rpm_events WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`); err != nil {
		logDatabaseWriteError("prune account RPM events", err)
		return
	}
	if _, err := tx.Exec(`DELETE FROM account_rpm_thresholds WHERE reset_at <= ` + nowSQL); err != nil {
		logDatabaseWriteError("prune account RPM thresholds", err)
		return
	}
	var observedRPM, strikes int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`, accountID).Scan(&observedRPM); err != nil || observedRPM <= 0 {
		return
	}
	if err := tx.QueryRow(`SELECT consecutive_429 FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&strikes); err != nil {
		return
	}
	rpmLimit := reduced429RPMThreshold(observedRPM, strikes)
	if resetAt.IsZero() {
		resetAt = time.Now().UTC().Add(5 * time.Hour)
	}
	resetAtValue := resetAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO account_rpm_thresholds (account_id, rpm_limit, reset_at) VALUES (?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			rpm_limit = MIN(account_rpm_thresholds.rpm_limit, excluded.rpm_limit),
			reset_at = MAX(account_rpm_thresholds.reset_at, excluded.reset_at),
			updated_at = `+nowSQL, accountID, rpmLimit, resetAtValue); err != nil {
		return
	}
	logDatabaseWriteError("commit account RPM threshold", tx.Commit())
}

func reduced429RPMThreshold(observedRPM, strikes int) int {
	if observedRPM <= 1 {
		return observedRPM
	}
	if strikes < 1 {
		strikes = 1
	}
	remainingPercent := 100 - strikes*account429RPMReductionPercentPerStrike
	if remainingPercent < account429RPMMinimumPercent {
		remainingPercent = account429RPMMinimumPercent
	}
	limit := observedRPM * remainingPercent / 100
	if limit < 1 {
		return 1
	}
	if limit >= observedRPM {
		return observedRPM - 1
	}
	return limit
}

func (a *app) captureAccountUpstreamFailure(account gatewayAccount, status int, body []byte) {
	message := strings.TrimSpace(upstreamErrorMessage(body))
	if message == "" {
		message = http.StatusText(status)
	}
	if len(message) > 500 {
		message = message[:500]
	}
	switch status {
	case http.StatusBadRequest:
		if accountRequiresUpstreamAction(message) {
			a.markAccountReauth(account.ID, "upstream account action required: "+message)
		}
	case http.StatusUnauthorized:
		reason := "OAuth 401: " + message
		if accountAuthenticationFailureIsTerminal(message) {
			a.markAccountReauth(account.ID, reason)
			return
		}
		if !gatewayAccountHasRefreshToken(account) {
			a.markAccountReauth(account.ID, "upstream authentication failed: "+message)
			return
		}
		until := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
		_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, error_message = ?, auth_error = ?, auth_checked_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, until, reason, reason, account.ID)
		logDatabaseWriteError("record account authentication cooldown", err)
	case http.StatusForbidden:
		a.markAccountReauth(account.ID, "upstream access forbidden: "+message)
	}
}

func accountAuthenticationFailureIsTerminal(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "access token has been revoked") ||
		strings.Contains(message, "access token was revoked")
}

func (a *app) reclassifyRevokedOAuthAccounts() error {
	_, err := a.db.Exec(`UPDATE accounts SET
		` + accumulateAccountSurvivalSQL + `,
		auth_status = 'reauth_required',
		invalidated_at = COALESCE(invalidated_at, ` + nowSQL + `),
		error_message = auth_error,
		status = 'error',
		rate_limit_reset_at = NULL,
		updated_at = ` + nowSQL + `
		WHERE deleted_at IS NULL
			AND status != 'disabled'
			AND (LOWER(auth_error) LIKE '%access token has been revoked%'
				OR LOWER(auth_error) LIKE '%access token was revoked%')
			AND (status != 'error' OR auth_status != 'reauth_required' OR invalidated_at IS NULL)`)
	if err != nil {
		return fmt.Errorf("reclassify revoked OAuth accounts: %w", err)
	}
	return nil
}

func accountRequiresUpstreamAction(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	if strings.Contains(message, "identity verification is required") {
		return true
	}
	return strings.Contains(message, "consumer terms") &&
		(strings.Contains(message, "accept") || strings.Contains(message, "agreement"))
}

func upstreamErrorMessage(body []byte) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return string(body)
	}
	if value, ok := payload["error"].(map[string]any); ok {
		if message, ok := value["message"].(string); ok {
			return message
		}
	}
	if message, ok := payload["message"].(string); ok {
		return message
	}
	return ""
}
