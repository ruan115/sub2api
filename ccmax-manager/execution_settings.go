package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
)

const (
	executionModeCLINative = "cli_native"
	executionModeOAuthAPI  = "oauth_api"
)

type groupExecutionSettings struct {
	GroupID            string `json:"group_id"`
	ExecutionPolicy    string `json:"execution_policy"`
	WorkerQueueMode    string `json:"worker_queue_mode"`
	WorkerImageChannel string `json:"worker_image_channel"`
	DataPlaneEnabled   bool   `json:"data_plane_enabled"`
}

type groupExecutionSettingsInput struct {
	ExecutionPolicy    string `json:"execution_policy"`
	WorkerQueueMode    string `json:"worker_queue_mode"`
	WorkerImageChannel string `json:"worker_image_channel"`
}

type accountExecutionSettings struct {
	AccountID             int64               `json:"account_id"`
	AllowedModes          []string            `json:"allowed_modes"`
	PreferredMode         string              `json:"preferred_mode"`
	MigrationStatus       string              `json:"migration_status"`
	RuntimeStatus         string              `json:"runtime_status"`
	RuntimeErrorCode      string              `json:"runtime_error_code"`
	RuntimeGeneration     uint64              `json:"runtime_generation"`
	RuntimeSlotID         string              `json:"runtime_slot_id"`
	RuntimeProvider       string              `json:"runtime_provider"`
	RuntimeExecutionEpoch uint64              `json:"runtime_execution_epoch"`
	CLINativeLimit        int                 `json:"cli_native_limit"`
	OAuthAPILimit         int                 `json:"oauth_api_limit"`
	TotalLimit            int                 `json:"total_limit"`
	ModeHealth            []accountModeHealth `json:"mode_health"`
	DataPlaneEnabled      bool                `json:"data_plane_enabled"`
}

type accountExecutionSettingsInput struct {
	AllowedModes   []string `json:"allowed_modes"`
	PreferredMode  string   `json:"preferred_mode"`
	CLINativeLimit int      `json:"cli_native_limit"`
	OAuthAPILimit  int      `json:"oauth_api_limit"`
	TotalLimit     int      `json:"total_limit"`
}

func (input *groupExecutionSettingsInput) normalize() error {
	input.ExecutionPolicy = strings.ToLower(strings.TrimSpace(input.ExecutionPolicy))
	input.WorkerQueueMode = strings.ToLower(strings.TrimSpace(input.WorkerQueueMode))
	input.WorkerImageChannel = strings.ToLower(strings.TrimSpace(input.WorkerImageChannel))
	if input.ExecutionPolicy != "auto" && input.ExecutionPolicy != "cli_only" && input.ExecutionPolicy != "api_only" {
		return errors.New("execution_policy must be auto, cli_only, or api_only")
	}
	if input.WorkerQueueMode != "queue" && input.WorkerQueueMode != "reject" {
		return errors.New("worker_queue_mode must be queue or reject")
	}
	if input.WorkerImageChannel != "stable" && input.WorkerImageChannel != "canary" {
		return errors.New("worker_image_channel must be stable or canary")
	}
	return nil
}

func (input *accountExecutionSettingsInput) normalize() error {
	modes, err := normalizeExecutionModes(input.AllowedModes)
	if err != nil {
		return err
	}
	input.AllowedModes = modes
	input.PreferredMode = strings.ToLower(strings.TrimSpace(input.PreferredMode))
	if !containsExecutionMode(modes, input.PreferredMode) {
		return errors.New("preferred_mode must be present in allowed_modes")
	}
	if input.CLINativeLimit < 1 || input.CLINativeLimit > 1000 || input.OAuthAPILimit < 1 || input.OAuthAPILimit > 1000 || input.TotalLimit < 1 || input.TotalLimit > 1000 {
		return errors.New("execution limits must be between 1 and 1000")
	}
	if input.CLINativeLimit > input.TotalLimit || input.OAuthAPILimit > input.TotalLimit {
		return errors.New("per-mode execution limit cannot exceed total_limit")
	}
	return nil
}

func normalizeExecutionModes(input []string) ([]string, error) {
	seen := map[string]bool{}
	for _, value := range input {
		mode := strings.ToLower(strings.TrimSpace(value))
		if !validExecutionMode(mode) {
			return nil, errors.New("allowed_modes may contain only cli_native and oauth_api")
		}
		seen[mode] = true
	}
	if len(seen) == 0 {
		return nil, errors.New("allowed_modes must not be empty")
	}
	result := make([]string, 0, len(seen))
	for _, mode := range []string{executionModeCLINative, executionModeOAuthAPI} {
		if seen[mode] {
			result = append(result, mode)
		}
	}
	return result, nil
}

func containsExecutionMode(modes []string, expected string) bool {
	for _, mode := range modes {
		if mode == expected {
			return true
		}
	}
	return false
}

func (a *app) handleGroupExecutionSettings(w http.ResponseWriter, r *http.Request) {
	groupID := strings.ToLower(strings.TrimSpace(r.PathValue("id")))
	if !groupIDPattern.MatchString(groupID) {
		writeError(w, http.StatusBadRequest, "invalid group id")
		return
	}
	if !userCanAccessGroup(currentUser(r), groupID) {
		writeError(w, http.StatusNotFound, "group not found")
		return
	}
	if r.Method == http.MethodPut {
		var input groupExecutionSettingsInput
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := input.normalize(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, err := a.db.Exec(`UPDATE groups SET execution_policy = ?, worker_queue_mode = ?, worker_image_channel = ?, updated_at = `+nowSQL+` WHERE id = ?`,
			input.ExecutionPolicy, input.WorkerQueueMode, input.WorkerImageChannel, groupID)
		if err != nil {
			writeDBError(w, err)
			return
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
	}
	settings, err := a.getGroupExecutionSettings(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "group not found")
			return
		}
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (a *app) getGroupExecutionSettings(ctx context.Context, groupID string) (groupExecutionSettings, error) {
	settings := groupExecutionSettings{GroupID: groupID, DataPlaneEnabled: a.executionClient != nil}
	err := a.db.QueryRowContext(ctx, `SELECT execution_policy, worker_queue_mode, worker_image_channel FROM groups WHERE id = ?`, groupID).Scan(
		&settings.ExecutionPolicy, &settings.WorkerQueueMode, &settings.WorkerImageChannel,
	)
	return settings, err
}

func (a *app) handleAccountExecutionSettings(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	accessible, err := a.accountExecutionSettingsAccessible(r.Context(), currentUser(r), accountID)
	if err != nil {
		writeDBError(w, err)
		return
	}
	if !accessible {
		writeError(w, http.StatusNotFound, "account not found")
		return
	}
	if r.Method == http.MethodPut {
		var input accountExecutionSettingsInput
		if !decodeJSON(w, r, &input) {
			return
		}
		if err := input.normalize(); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		encodedModes, _ := json.Marshal(input.AllowedModes)
		result, err := a.db.Exec(`UPDATE accounts SET execution_allowed_modes = ?, execution_preferred_mode = ?, cli_native_limit = ?, oauth_api_limit = ?, execution_total_limit = ?, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`,
			string(encodedModes), input.PreferredMode, input.CLINativeLimit, input.OAuthAPILimit, input.TotalLimit, accountID)
		if err != nil {
			writeDBError(w, err)
			return
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
	}
	settings, err := a.getAccountExecutionSettings(r.Context(), accountID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (a *app) accountExecutionSettingsAccessible(ctx context.Context, user panelUser, accountID int64) (bool, error) {
	condition, args := scopedAccountCondition(user, "a")
	queryArgs := append([]any{accountID}, args...)
	var count int
	err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts a WHERE a.id = ? AND a.deleted_at IS NULL AND `+condition, queryArgs...).Scan(&count)
	return count == 1, err
}

func (a *app) getAccountExecutionSettings(ctx context.Context, accountID int64) (accountExecutionSettings, error) {
	var settings accountExecutionSettings
	var encodedModes string
	err := a.db.QueryRowContext(ctx, `SELECT id, execution_allowed_modes, execution_preferred_mode, execution_migration_status,
		runtime_status, runtime_error_code, runtime_generation, runtime_slot_id, runtime_provider, runtime_execution_epoch,
		cli_native_limit, oauth_api_limit, execution_total_limit
		FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(
		&settings.AccountID, &encodedModes, &settings.PreferredMode, &settings.MigrationStatus,
		&settings.RuntimeStatus, &settings.RuntimeErrorCode, &settings.RuntimeGeneration, &settings.RuntimeSlotID,
		&settings.RuntimeProvider, &settings.RuntimeExecutionEpoch, &settings.CLINativeLimit, &settings.OAuthAPILimit, &settings.TotalLimit,
	)
	if err != nil {
		return accountExecutionSettings{}, err
	}
	if err := json.Unmarshal([]byte(encodedModes), &settings.AllowedModes); err != nil {
		return accountExecutionSettings{}, errors.New("account execution allowed modes are invalid")
	}
	settings.AllowedModes, err = normalizeExecutionModes(settings.AllowedModes)
	if err != nil || !containsExecutionMode(settings.AllowedModes, settings.PreferredMode) {
		return accountExecutionSettings{}, errors.New("account execution mode configuration is invalid")
	}
	settings.ModeHealth, err = a.listAccountModeHealth(ctx, accountID)
	settings.DataPlaneEnabled = a.executionClient != nil
	return settings, err
}

func (a *app) listAccountModeHealth(ctx context.Context, accountID int64) ([]accountModeHealth, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT account_id, mode, status, error_code, recover_at, updated_at FROM account_mode_health WHERE account_id = ? ORDER BY mode`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byMode := map[string]accountModeHealth{}
	for rows.Next() {
		var health accountModeHealth
		var recoverAt sql.NullString
		if err := rows.Scan(&health.AccountID, &health.Mode, &health.Status, &health.ErrorCode, &recoverAt, &health.UpdatedAt); err != nil {
			return nil, err
		}
		if recoverAt.Valid {
			health.RecoverAt = recoverAt.String
		}
		byMode[health.Mode] = health
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]accountModeHealth, 0, 2)
	for _, mode := range []string{executionModeCLINative, executionModeOAuthAPI} {
		health, ok := byMode[mode]
		if !ok {
			health = accountModeHealth{AccountID: accountID, Mode: mode, Status: "unavailable"}
		}
		result = append(result, health)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Mode < result[right].Mode })
	return result, nil
}

type executionDispatchKind string

const (
	executionDispatchLegacy      executionDispatchKind = "legacy"
	executionDispatchDataPlane   executionDispatchKind = "data_plane"
	executionDispatchUnavailable executionDispatchKind = "unavailable"
)

type executionDispatchDecision struct {
	Kind       executionDispatchKind
	Mode       string
	ReasonCode string
}

func chooseExecutionDispatch(dataPlaneEnabled bool, group groupExecutionSettings, account accountExecutionSettings) executionDispatchDecision {
	if account.MigrationStatus == "legacy" {
		return executionDispatchDecision{Kind: executionDispatchLegacy, ReasonCode: "legacy_account"}
	}
	if account.MigrationStatus != "migrated" {
		return executionDispatchDecision{Kind: executionDispatchUnavailable, ReasonCode: "migration_in_progress"}
	}
	if !dataPlaneEnabled {
		return executionDispatchDecision{Kind: executionDispatchUnavailable, ReasonCode: "data_plane_disabled"}
	}
	if account.RuntimeStatus != "ready" || account.RuntimeSlotID == "" || account.RuntimeGeneration == 0 || account.RuntimeExecutionEpoch == 0 {
		return executionDispatchDecision{Kind: executionDispatchUnavailable, ReasonCode: "runtime_not_ready"}
	}
	healthy := map[string]bool{}
	for _, health := range account.ModeHealth {
		healthy[health.Mode] = health.Status == "healthy"
	}
	candidates := []string{}
	switch group.ExecutionPolicy {
	case "cli_only":
		candidates = []string{executionModeCLINative}
	case "api_only":
		candidates = []string{executionModeOAuthAPI}
	case "auto":
		candidates = append(candidates, account.PreferredMode)
		for _, mode := range []string{executionModeCLINative, executionModeOAuthAPI} {
			if mode != account.PreferredMode {
				candidates = append(candidates, mode)
			}
		}
	default:
		return executionDispatchDecision{Kind: executionDispatchUnavailable, ReasonCode: "invalid_group_policy"}
	}
	for _, mode := range candidates {
		if containsExecutionMode(account.AllowedModes, mode) && healthy[mode] {
			return executionDispatchDecision{Kind: executionDispatchDataPlane, Mode: mode}
		}
	}
	return executionDispatchDecision{Kind: executionDispatchUnavailable, ReasonCode: "no_healthy_mode"}
}
