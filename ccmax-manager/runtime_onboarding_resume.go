package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	errRuntimeOnboardingResumeNotFound = errors.New("current runtime onboarding submission was not found")
	errRuntimeOnboardingResumeConflict = errors.New("current runtime onboarding submission conflicts with account state")
)

type runtimeOnboardingResumeInput struct {
	OnboardingSource    string `json:"onboarding_source"`
	AuthType            string `json:"auth_type"`
	OnboardingSecret    string `json:"onboarding_secret"`
	OnboardingAuxiliary string `json:"onboarding_auxiliary"`
	SessionKey          string `json:"session_key"`
}

type runtimeOnboardingStatusResponse struct {
	AccountID                 int64  `json:"account_id"`
	OperationType             string `json:"operation_type"`
	Status                    string `json:"status"`
	DesiredGeneration         uint64 `json:"desired_generation"`
	EventType                 string `json:"event_type"`
	MigrationStatus           string `json:"migration_status"`
	SourceType                string `json:"source_type"`
	AuthType                  string `json:"auth_type"`
	IntakeAttempt             uint64 `json:"intake_attempt"`
	ReceiptPersisted          bool   `json:"receipt_persisted"`
	ReceiptExpiresAt          string `json:"receipt_expires_at,omitempty"`
	MayRequireMaterial        bool   `json:"may_require_material"`
	EventID                   string `json:"event_id,omitempty"`
	RequestFingerprintVersion int64  `json:"request_fingerprint_version,omitempty"`
	ResumeURL                 string `json:"resume_url"`
}

type runtimeOnboardingAccountFence struct {
	MigrationStatus         string
	RuntimeStatus           string
	RuntimeGeneration       uint64
	ProxyID                 sql.NullInt64
	CredentialsJSON         string
	Schedulable             int
	Platform                string
	AuthType                string
	Status                  string
	Concurrency             int
	Priority                int
	RateMultiplier          float64
	BaseRPM                 int
	RPMStrategy             string
	RPMStickyBuffer         int
	UserMsgQueueMode        string
	AccountPrice            float64
	Quota5HThresholdEnabled int
	Quota5HThresholdPercent int
	Quota7DThresholdEnabled int
	Quota7DThresholdPercent int
	ProxyPoolID             sql.NullInt64
	AutoProxy               int
	StrategyID              sql.NullInt64
}

// canonicalRuntimeOnboardingSubmission resolves the server-owned submission
// for the account's current generation. It never accepts an external key from
// the caller. The account and submission are inspected under one short local
// transaction, which is committed before any execution-plane RPC is made.
func (a *app) canonicalRuntimeOnboardingSubmission(ctx context.Context, accountID int64) (runtimeOnboardingSubmission, error) {
	if a == nil || a.db == nil || ctx == nil || ctx.Err() != nil || accountID <= 0 {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeNotFound
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return runtimeOnboardingSubmission{}, fmt.Errorf("begin canonical runtime onboarding lookup: %w", err)
	}
	defer tx.Rollback()

	accountQuery := `SELECT execution_migration_status, runtime_status, runtime_generation, proxy_id,
		credentials_json, schedulable, platform, auth_type, status, concurrency, priority, rate_multiplier,
		base_rpm, rpm_strategy, rpm_sticky_buffer, user_msg_queue_mode, account_price,
		quota_5h_threshold_enabled, quota_5h_threshold_percent,
		quota_7d_threshold_enabled, quota_7d_threshold_percent,
		proxy_pool_id, auto_proxy, strategy_id
		FROM accounts WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`
	if a.db.dialect == dialectMySQL {
		accountQuery += ` FOR UPDATE`
	}
	var account runtimeOnboardingAccountFence
	if err := tx.QueryRowContext(ctx, accountQuery, accountID).Scan(
		&account.MigrationStatus, &account.RuntimeStatus, &account.RuntimeGeneration, &account.ProxyID,
		&account.CredentialsJSON, &account.Schedulable, &account.Platform, &account.AuthType, &account.Status,
		&account.Concurrency, &account.Priority, &account.RateMultiplier,
		&account.BaseRPM, &account.RPMStrategy, &account.RPMStickyBuffer, &account.UserMsgQueueMode,
		&account.AccountPrice, &account.Quota5HThresholdEnabled, &account.Quota5HThresholdPercent,
		&account.Quota7DThresholdEnabled, &account.Quota7DThresholdPercent,
		&account.ProxyPoolID, &account.AutoProxy, &account.StrategyID,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeNotFound
		}
		return runtimeOnboardingSubmission{}, fmt.Errorf("load runtime onboarding account fence: %w", err)
	}
	if account.RuntimeGeneration == ^uint64(0) {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
	}

	submissionQuery := `SELECT idempotency_key, intake_idempotency_key, intake_attempt,
		operation_type, account_id, desired_generation, event_type, migration_status, source_type, auth_type,
		proxy_id, status, intent_id, intent_expires_at_millis, event_id,
		request_fingerprint_version, request_fingerprint_sha256
		FROM runtime_onboarding_submissions
		WHERE account_id = ? AND (
			(status = 'pending' AND desired_generation = ?) OR
			(status = 'queued' AND desired_generation = ?)
		)
		ORDER BY CASE WHEN status = 'pending' THEN 0 ELSE 1 END, desired_generation DESC
		LIMIT 1`
	if a.db.dialect == dialectMySQL {
		submissionQuery += ` FOR UPDATE`
	}
	var submission runtimeOnboardingSubmission
	var fingerprint []byte
	if err := tx.QueryRowContext(ctx, submissionQuery, accountID, account.RuntimeGeneration+1, account.RuntimeGeneration).Scan(
		&submission.IdempotencyKey, &submission.IntakeIdempotencyKey, &submission.IntakeAttempt,
		&submission.OperationType, &submission.AccountID, &submission.DesiredGeneration,
		&submission.EventType, &submission.MigrationStatus, &submission.SourceType, &submission.AuthType,
		&submission.ProxyID, &submission.Status, &submission.IntentID, &submission.IntentExpiresAtMillis,
		&submission.EventID, &submission.RequestFingerprintVersion, &fingerprint,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeNotFound
		}
		return runtimeOnboardingSubmission{}, fmt.Errorf("load canonical runtime onboarding submission: %w", err)
	}
	if len(fingerprint) != 0 {
		if len(fingerprint) != runtimeOnboardingFingerprintSize {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
		}
		copy(submission.RequestFingerprintSHA256[:], fingerprint)
		submission.RequestFingerprintPresent = true
	}
	if validateRuntimeOnboardingSubmission(submission, false) != nil ||
		submission.AccountID != accountID || !account.ProxyID.Valid || !submission.ProxyID.Valid ||
		account.ProxyID.Int64 != submission.ProxyID.Int64 || account.MigrationStatus != submission.MigrationStatus ||
		!emptyJSONObject(account.CredentialsJSON) {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
	}
	if submission.Status == runtimeOnboardingSubmissionPending && submission.DesiredGeneration != account.RuntimeGeneration+1 {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
	}
	if submission.Status == runtimeOnboardingSubmissionQueued && submission.DesiredGeneration != account.RuntimeGeneration {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
	}

	switch submission.OperationType {
	case runtimeOnboardingOperationCreate:
		if account.Platform != "anthropic" || account.MigrationStatus != "migrating" ||
			account.RuntimeStatus != "provisioning" || account.Schedulable != 0 || submission.DesiredGeneration != 1 ||
			submission.EventType != "account.runtime.provision_requested" ||
			submission.RequestFingerprintVersion != runtimeOnboardingCreateFingerprintVersion ||
			!submission.RequestFingerprintPresent || account.AuthType != submission.AuthType {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
		}
		if submission.Status == runtimeOnboardingSubmissionPending {
			matches, err := runtimeOnboardingCreateFingerprintMatchesAccount(ctx, tx, accountID, account, submission)
			if err != nil {
				return runtimeOnboardingSubmission{}, err
			}
			if !matches {
				return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
			}
		}
	case runtimeOnboardingOperationReauthorize:
		if !allowedRuntimeOnboardingMigration(account.MigrationStatus) ||
			(account.MigrationStatus == "migrating" && submission.EventType != "account.credential.migrate_requested") ||
			(account.MigrationStatus == "migrated" && submission.EventType != "account.credential.rotate_requested") ||
			submission.RequestFingerprintVersion != 0 || submission.RequestFingerprintPresent {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
		}
		if submission.Status == runtimeOnboardingSubmissionPending &&
			!allowedRuntimeOnboardingReauthorizePredecessor(account.RuntimeStatus) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
		}
		if submission.Status == runtimeOnboardingSubmissionQueued &&
			(account.RuntimeStatus != "provisioning" || account.Schedulable != 0) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
		}
	default:
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingResumeConflict
	}

	if err := tx.Commit(); err != nil {
		return runtimeOnboardingSubmission{}, fmt.Errorf("commit canonical runtime onboarding lookup: %w", err)
	}
	return submission, nil
}

func runtimeOnboardingCreateFingerprintMatchesAccount(
	ctx context.Context,
	tx *databaseTx,
	accountID int64,
	account runtimeOnboardingAccountFence,
	submission runtimeOnboardingSubmission,
) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT group_id FROM account_groups WHERE account_id = ? ORDER BY group_id`, accountID)
	if err != nil {
		return false, fmt.Errorf("load runtime onboarding account groups: %w", err)
	}
	defer rows.Close()
	groups := make([]string, 0, 4)
	for rows.Next() {
		var groupID string
		if err := rows.Scan(&groupID); err != nil {
			return false, fmt.Errorf("scan runtime onboarding account group: %w", err)
		}
		groups = append(groups, groupID)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate runtime onboarding account groups: %w", err)
	}

	schedulable := false
	quota5Enabled := account.Quota5HThresholdEnabled == 1
	quota5Percent := account.Quota5HThresholdPercent
	quota7Enabled := account.Quota7DThresholdEnabled == 1
	quota7Percent := account.Quota7DThresholdPercent
	input := accountInput{
		Platform: account.Platform, AuthType: account.AuthType, Status: account.Status,
		Schedulable: &schedulable, Concurrency: account.Concurrency, Priority: account.Priority,
		RateMultiplier: account.RateMultiplier, GroupIDs: groups, AutoProxy: account.AutoProxy == 1,
		BaseRPM: account.BaseRPM, RPMStrategy: account.RPMStrategy, RPMStickyBuffer: account.RPMStickyBuffer,
		UserMsgQueueMode: account.UserMsgQueueMode, AccountPrice: account.AccountPrice,
		Quota5HThresholdEnabled: &quota5Enabled, Quota5HThresholdPercent: &quota5Percent,
		Quota7DThresholdEnabled: &quota7Enabled, Quota7DThresholdPercent: &quota7Percent,
	}
	if account.ProxyPoolID.Valid {
		value := account.ProxyPoolID.Int64
		input.ProxyPoolID = &value
	}
	if account.StrategyID.Valid && account.StrategyID.Int64 > 0 {
		value := account.StrategyID.Int64
		input.StrategyID = &value
	}
	strategyID := runtimeOnboardingRequestedStrategyID(input.StrategyID)
	material := &runtimeOnboardingMaterial{Source: submission.SourceType, AuthType: submission.AuthType}

	// auto_proxy requests historically allowed both an omitted proxy_id and an
	// explicit preferred proxy. The persisted account contains only the selected
	// proxy, so compare both non-secret canonical possibilities.
	proxyCandidates := []*int64{nil}
	if !input.AutoProxy {
		value := account.ProxyID.Int64
		proxyCandidates = []*int64{&value}
	} else {
		value := account.ProxyID.Int64
		proxyCandidates = append(proxyCandidates, &value)
	}
	for _, candidate := range proxyCandidates {
		input.ProxyID = candidate
		fingerprint, err := runtimeOnboardingCreateFingerprint(&input, material, strategyID)
		if err != nil {
			return false, err
		}
		if fingerprint == submission.RequestFingerprintSHA256 {
			return true, nil
		}
	}
	return false, nil
}

func (a *app) handleRuntimeOnboardingStatus(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	submission, err := a.canonicalRuntimeOnboardingSubmission(r.Context(), accountID)
	if err != nil {
		writeRuntimeOnboardingResumeLookupError(w, err)
		return
	}
	response := runtimeOnboardingStatusResponse{
		AccountID: accountID, OperationType: submission.OperationType, Status: submission.Status,
		DesiredGeneration: submission.DesiredGeneration, EventType: submission.EventType,
		MigrationStatus: submission.MigrationStatus, SourceType: submission.SourceType, AuthType: submission.AuthType,
		IntakeAttempt: submission.IntakeAttempt, EventID: submission.EventID,
		RequestFingerprintVersion: submission.RequestFingerprintVersion,
		ResumeURL:                 fmt.Sprintf("/api/accounts/%d/runtime-onboarding/resume", accountID),
	}
	if receipt, present := runtimeOnboardingReceiptFromSubmission(submission); present {
		response.ReceiptPersisted = true
		response.ReceiptExpiresAt = receipt.ExpiresAt.UTC().Format(time.RFC3339Nano)
		response.MayRequireMaterial = !runtimeOnboardingReceiptHasCommitMargin(receipt.ExpiresAt, time.Now().UTC())
	} else {
		response.MayRequireMaterial = submission.Status == runtimeOnboardingSubmissionPending
	}
	writeJSON(w, http.StatusOK, response)
}

func (a *app) handleRuntimeOnboardingResume(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(r.Header.Get("Idempotency-Key")) != "" {
		writeError(w, http.StatusBadRequest, "runtime onboarding resume uses the account's canonical submission; do not provide Idempotency-Key")
		return
	}
	var input runtimeOnboardingResumeInput
	if !decodeJSON(w, r, &input) {
		return
	}
	defer func() {
		input.OnboardingSecret = ""
		input.OnboardingAuxiliary = ""
		input.SessionKey = ""
	}()
	submission, err := a.canonicalRuntimeOnboardingSubmission(r.Context(), accountID)
	if err != nil {
		writeRuntimeOnboardingResumeLookupError(w, err)
		return
	}
	material, err := runtimeOnboardingMaterialForResume(submission, &input)
	if err != nil {
		if errors.Is(err, errRuntimeOnboardingResumeConflict) {
			writeError(w, http.StatusConflict, errRuntimeOnboardingResumeConflict.Error())
		} else {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	event, err := a.requestRuntimeOnboardingWithMaterial(r.Context(), a.onboardingIntake, submission.IdempotencyKey,
		runtimeTransitionRequest{
			AccountID: submission.AccountID, EventType: submission.EventType,
			MigrationStatus: submission.MigrationStatus, RuntimeStatus: "provisioning",
		}, material)
	if err != nil {
		status, message, outcome := runtimeOnboardingResumeError(err)
		a.recordAuthorization(&accountID, nil, "", "runtime_onboarding_resume", false, outcome, "", requestIP(r))
		writeError(w, status, message)
		return
	}
	a.recordAuthorization(&accountID, nil, "", "runtime_onboarding_resume", true, "canonical runtime onboarding queued", "", requestIP(r))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status": "provisioning", "account_id": accountID,
		"event_id": event.EventID, "desired_generation": event.DesiredGeneration,
	})
}

func runtimeOnboardingMaterialForResume(
	submission runtimeOnboardingSubmission,
	input *runtimeOnboardingResumeInput,
) (*runtimeOnboardingMaterial, error) {
	if input == nil {
		return nil, errors.New("invalid runtime onboarding resume input")
	}
	source := strings.ToLower(strings.TrimSpace(input.OnboardingSource))
	authType := strings.ToLower(strings.TrimSpace(input.AuthType))
	if source == "" {
		source = submission.SourceType
	}
	if authType == "" {
		authType = submission.AuthType
	}
	if source != submission.SourceType || authType != submission.AuthType {
		return nil, errRuntimeOnboardingResumeConflict
	}
	if strings.TrimSpace(input.OnboardingSecret) == "" && strings.TrimSpace(input.SessionKey) == "" &&
		strings.TrimSpace(input.OnboardingAuxiliary) == "" {
		return &runtimeOnboardingMaterial{Source: source, AuthType: authType}, nil
	}
	candidate := accountInput{
		ExecutionOnboarding: true, OnboardingSource: source, AuthType: authType,
		OnboardingSecret: input.OnboardingSecret, OnboardingAuxiliary: input.OnboardingAuxiliary,
		SessionKey: input.SessionKey,
	}
	material, err := prepareRuntimeOnboardingMaterial(&candidate)
	if err != nil {
		return nil, err
	}
	if material == nil || material.Source != submission.SourceType || material.AuthType != submission.AuthType {
		if material != nil {
			material.Destroy()
		}
		return nil, errRuntimeOnboardingResumeConflict
	}
	return material, nil
}

func runtimeOnboardingResumeError(err error) (int, string, string) {
	switch {
	case errors.Is(err, errRuntimeOnboardingMaterialRequired):
		return http.StatusPreconditionRequired, "onboarding material is required to resume this submission", "canonical runtime onboarding requires new material"
	case errors.Is(err, errRuntimeMigration), errors.Is(err, errRuntimeOnboardingIdempotency):
		return http.StatusConflict, errRuntimeOnboardingResumeConflict.Error(), "canonical runtime onboarding was blocked by a state fence"
	case errors.Is(err, errRuntimeOnboardingTimeout):
		return http.StatusGatewayTimeout, "execution onboarding intake timed out", "canonical runtime onboarding intake timed out"
	case errors.Is(err, errRuntimeOnboardingUnavailable):
		return http.StatusServiceUnavailable, "execution onboarding intake is unavailable", "canonical runtime onboarding intake is unavailable"
	default:
		return http.StatusBadGateway, "execution onboarding request failed", "canonical runtime onboarding request failed"
	}
}

func writeRuntimeOnboardingResumeLookupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errRuntimeOnboardingResumeNotFound):
		writeError(w, http.StatusNotFound, errRuntimeOnboardingResumeNotFound.Error())
	case errors.Is(err, errRuntimeOnboardingResumeConflict):
		writeError(w, http.StatusConflict, errRuntimeOnboardingResumeConflict.Error())
	default:
		writeDBError(w, err)
	}
}

func writePendingRuntimeOnboardingError(w http.ResponseWriter, status int, message string, accountID int64) {
	if accountID <= 0 {
		writeError(w, status, message)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": message, "status": "pending", "pending_account_id": accountID,
		"resume_url": fmt.Sprintf("/api/accounts/%d/runtime-onboarding/resume", accountID),
	})
}
