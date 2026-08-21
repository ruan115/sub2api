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
		_, _ = a.db.Exec(`UPDATE accounts SET auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, accountID)
	}
	if quota, ok := quotaFromHeaders(response.Header); ok {
		_ = a.persistAccountQuota(accountID, quota, false)
	}
	if resetAt, ok := sub2service.ResolveCCMaxCompatibilityCooldown(response.StatusCode, response.Header); ok {
		_, _ = a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, updated_at = `+nowSQL+` WHERE id = ?`, resetAt.UTC().Format(time.RFC3339Nano), accountID)
	}
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
	case http.StatusUnauthorized:
		if !gatewayAccountHasRefreshToken(account) {
			a.markAccountReauth(account.ID, "upstream authentication failed: "+message)
			return
		}
		until := time.Now().UTC().Add(10 * time.Minute).Format(time.RFC3339Nano)
		reason := "OAuth 401: " + message
		_, _ = a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, error_message = ?, auth_error = ?, auth_checked_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, until, reason, reason, account.ID)
	case http.StatusForbidden:
		a.markAccountReauth(account.ID, "upstream access forbidden: "+message)
	}
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
