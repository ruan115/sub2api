package main

import (
	"context"
	"database/sql"
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
	if response == nil {
		return
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		_, err := a.db.Exec(`UPDATE accounts SET auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, accountID)
		logDatabaseWriteError("record valid upstream account", err)
	}
	if quota, ok := quotaFromHeaders(response.Header); ok {
		_ = a.persistAccountQuota(accountID, quota, false)
	}
	if resetAt, window, ok := sub2service.ResolveCCMaxCompatibilityCooldownWindow(response.StatusCode, response.Header); ok {
		// The cooldown itself only carries a reset time, so record which window
		// caused it while the response headers are still at hand.
		_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, rate_limit_window = ?, updated_at = `+nowSQL+` WHERE id = ?`, resetAt.UTC().Format(time.RFC3339Nano), window, accountID)
		logDatabaseWriteError("record account quota cooldown", err)
	}
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

func (a *app) captureGatewayUpstreamState(accountID int64, model string, overloadCooldownSeconds int, response *http.Response) {
	a.captureAccountUpstreamState(accountID, response)
	if response != nil && response.StatusCode == 529 {
		a.captureAccountModelOverload(accountID, model, overloadCooldownSeconds)
	}
}

func (a *app) captureAccountRPMThreshold(groupID string, accountID int64) {
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
	var observedRPM int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`, accountID).Scan(&observedRPM); err != nil || observedRPM <= 0 {
		return
	}
	resetAt := time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO account_rpm_thresholds (account_id, rpm_limit, reset_at) VALUES (?, ?, ?)
		ON CONFLICT(account_id) DO UPDATE SET
			rpm_limit = MIN(account_rpm_thresholds.rpm_limit, excluded.rpm_limit),
			reset_at = MAX(account_rpm_thresholds.reset_at, excluded.reset_at),
			updated_at = `+nowSQL, accountID, observedRPM, resetAt); err != nil {
		return
	}
	logDatabaseWriteError("commit account RPM threshold", tx.Commit())
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
