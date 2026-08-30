package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	runtimeOnboardingOperationCreate      = "account_create"
	runtimeOnboardingOperationReauthorize = "account_reauthorize"
	runtimeOnboardingSubmissionPending    = "pending"
	runtimeOnboardingSubmissionQueued     = "queued"
	runtimeOnboardingMaxIntakeAttempt     = 1 << 20
)

var (
	errRuntimeOnboardingIdempotency       = errors.New("runtime onboarding idempotency key conflicts with another request")
	errRuntimeOnboardingAttemptSuperseded = errors.New("runtime onboarding attempt was superseded")
)

func runtimeOnboardingReloadError(operation string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return errRuntimeOnboardingIdempotency
	}
	return fmt.Errorf("%s: %w", operation, err)
}

// runtimeOnboardingSubmission is the CCMAX-side durable replay record. It
// contains only request identity and opaque execution references; credential
// material and material-derived hashes are deliberately excluded.
type runtimeOnboardingSubmission struct {
	IdempotencyKey            string
	IntakeIdempotencyKey      string
	IntakeAttempt             uint64
	OperationType             string
	AccountID                 int64
	DesiredGeneration         uint64
	EventType                 string
	MigrationStatus           string
	SourceType                string
	AuthType                  string
	ProxyID                   sql.NullInt64
	Status                    string
	IntentID                  string
	IntentExpiresAtMillis     int64
	EventID                   string
	RequestFingerprintVersion int64
	RequestFingerprintSHA256  [runtimeOnboardingFingerprintSize]byte
	RequestFingerprintPresent bool
}

func newRuntimeOnboardingIntakeKey() string {
	return "onb-" + newRuntimeEventID()
}

func runtimeOnboardingOperationForEvent(eventType string) string {
	if eventType == "account.runtime.provision_requested" {
		return runtimeOnboardingOperationCreate
	}
	return runtimeOnboardingOperationReauthorize
}

func validateRuntimeOnboardingSubmission(candidate runtimeOnboardingSubmission, allowMissingProxy bool) error {
	if !runtimeOpaqueIntentIDPattern.MatchString(candidate.IdempotencyKey) || runtimeSecretString(candidate.IdempotencyKey) ||
		!runtimeOpaqueIntentIDPattern.MatchString(candidate.IntakeIdempotencyKey) || runtimeSecretString(candidate.IntakeIdempotencyKey) ||
		candidate.IntakeAttempt > runtimeOnboardingMaxIntakeAttempt ||
		candidate.AccountID <= 0 || candidate.DesiredGeneration == 0 ||
		(candidate.OperationType != runtimeOnboardingOperationCreate && candidate.OperationType != runtimeOnboardingOperationReauthorize) ||
		!runtimeOnboardingEventTypes[candidate.EventType] ||
		(candidate.EventType != "account.runtime.provision_requested" && candidate.EventType != "account.credential.migrate_requested" && candidate.EventType != "account.credential.rotate_requested") ||
		candidate.OperationType != runtimeOnboardingOperationForEvent(candidate.EventType) ||
		!allowedRuntimeOnboardingMigration(candidate.MigrationStatus) {
		return errRuntimeOnboardingIdempotency
	}
	if _, _, ok := runtimeOnboardingProtoTypes(candidate.SourceType, candidate.AuthType); !ok {
		return errRuntimeOnboardingIdempotency
	}
	zeroFingerprint := [runtimeOnboardingFingerprintSize]byte{}
	switch candidate.OperationType {
	case runtimeOnboardingOperationCreate:
		if candidate.RequestFingerprintVersion == 0 {
			// Version zero is loadable only so legacy rows can fail closed at the
			// account-create replay boundary. New creates may not insert it.
			if candidate.RequestFingerprintPresent || candidate.RequestFingerprintSHA256 != zeroFingerprint {
				return errRuntimeOnboardingIdempotency
			}
		} else if candidate.RequestFingerprintVersion != runtimeOnboardingCreateFingerprintVersion ||
			!candidate.RequestFingerprintPresent {
			return errRuntimeOnboardingIdempotency
		}
	case runtimeOnboardingOperationReauthorize:
		if candidate.RequestFingerprintVersion != 0 || candidate.RequestFingerprintPresent ||
			candidate.RequestFingerprintSHA256 != zeroFingerprint {
			return errRuntimeOnboardingIdempotency
		}
	}
	if !allowMissingProxy && (!candidate.ProxyID.Valid || candidate.ProxyID.Int64 <= 0) {
		return errRuntimeOnboardingIdempotency
	}
	switch candidate.Status {
	case runtimeOnboardingSubmissionPending:
		if candidate.EventID != "" || candidate.IntentExpiresAtMillis < 0 ||
			(candidate.IntentID == "" && candidate.IntentExpiresAtMillis != 0) ||
			(candidate.IntentID != "" && (!runtimeOpaqueIntentIDPattern.MatchString(candidate.IntentID) ||
				runtimeSecretString(candidate.IntentID) || candidate.IntentExpiresAtMillis == 0)) {
			return errRuntimeOnboardingIdempotency
		}
	case runtimeOnboardingSubmissionQueued:
		if !runtimeOpaqueIntentIDPattern.MatchString(candidate.IntentID) || runtimeSecretString(candidate.IntentID) ||
			!runtimeOpaqueIntentIDPattern.MatchString(candidate.EventID) || runtimeSecretString(candidate.EventID) ||
			candidate.IntentExpiresAtMillis < 0 {
			return errRuntimeOnboardingIdempotency
		}
	default:
		return errRuntimeOnboardingIdempotency
	}
	return nil
}

func runtimeOnboardingSubmissionMatchesRequest(
	submission runtimeOnboardingSubmission,
	idempotencyKey string,
	request runtimeTransitionRequest,
	material *runtimeOnboardingMaterial,
) bool {
	if material == nil {
		return false
	}
	return submission.IdempotencyKey == idempotencyKey &&
		submission.OperationType == runtimeOnboardingOperationForEvent(request.EventType) &&
		submission.AccountID == request.AccountID &&
		submission.EventType == request.EventType &&
		submission.MigrationStatus == strings.TrimSpace(request.MigrationStatus) &&
		submission.SourceType == material.Source && submission.AuthType == material.AuthType
}

func allowedRuntimeOnboardingMigration(value string) bool {
	return value == "migrating" || value == "migrated"
}

func allowedRuntimeOnboardingReauthorizePredecessor(value string) bool {
	switch value {
	case "legacy", "ready", "failed":
		return true
	default:
		return false
	}
}

func insertRuntimeOnboardingSubmissionTx(
	ctx context.Context,
	tx *databaseTx,
	candidate runtimeOnboardingSubmission,
) (bool, error) {
	if candidate.IntakeIdempotencyKey == "" {
		candidate.IntakeIdempotencyKey = newRuntimeOnboardingIntakeKey()
	}
	if tx == nil || validateRuntimeOnboardingSubmission(candidate, true) != nil || candidate.Status != runtimeOnboardingSubmissionPending ||
		candidate.IntentID != "" || candidate.IntentExpiresAtMillis != 0 ||
		(candidate.OperationType == runtimeOnboardingOperationCreate &&
			(candidate.RequestFingerprintVersion != runtimeOnboardingCreateFingerprintVersion || !candidate.RequestFingerprintPresent)) {
		return false, errRuntimeOnboardingIdempotency
	}
	var proxyID any
	if candidate.ProxyID.Valid && candidate.ProxyID.Int64 > 0 {
		proxyID = candidate.ProxyID.Int64
	}
	var requestFingerprint any
	if candidate.RequestFingerprintPresent {
		requestFingerprint = candidate.RequestFingerprintSHA256[:]
	}
	for collisionRetry := 0; collisionRetry < 4; collisionRetry++ {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO runtime_onboarding_submissions
			(idempotency_key, intake_idempotency_key, intake_attempt, operation_type, account_id, desired_generation,
			event_type, migration_status, source_type, auth_type, proxy_id, status,
			request_fingerprint_version, request_fingerprint_sha256)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			candidate.IdempotencyKey, candidate.IntakeIdempotencyKey, candidate.IntakeAttempt,
			candidate.OperationType, candidate.AccountID, candidate.DesiredGeneration,
			candidate.EventType, candidate.MigrationStatus, candidate.SourceType, candidate.AuthType, proxyID,
			candidate.RequestFingerprintVersion, requestFingerprint)
		if err != nil {
			return false, fmt.Errorf("insert runtime onboarding submission: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("read runtime onboarding submission insert: %w", err)
		}
		if affected == 1 {
			return true, nil
		}
		var intakeKeyOwner string
		ownerErr := tx.QueryRowContext(ctx, `SELECT idempotency_key FROM runtime_onboarding_submissions
			WHERE intake_idempotency_key = ?`, candidate.IntakeIdempotencyKey).Scan(&intakeKeyOwner)
		if ownerErr == nil && intakeKeyOwner != candidate.IdempotencyKey {
			candidate.IntakeIdempotencyKey = newRuntimeOnboardingIntakeKey()
			continue
		}
		if ownerErr != nil && !errors.Is(ownerErr, sql.ErrNoRows) {
			return false, fmt.Errorf("inspect runtime onboarding intake-key collision: %w", ownerErr)
		}
		return false, nil
	}
	return false, errRuntimeOnboardingIdempotency
}

func bindRuntimeOnboardingSubmissionProxyTx(ctx context.Context, tx *databaseTx, idempotencyKey string, accountID, proxyID int64) error {
	if tx == nil || !runtimeOpaqueIntentIDPattern.MatchString(idempotencyKey) || runtimeSecretString(idempotencyKey) || accountID <= 0 || proxyID <= 0 {
		return errRuntimeOnboardingIdempotency
	}
	result, err := tx.ExecContext(ctx, `UPDATE runtime_onboarding_submissions SET proxy_id = ?, updated_at = `+nowSQL+`
		WHERE idempotency_key = ? AND account_id = ? AND status = 'pending' AND proxy_id IS NULL`, proxyID, idempotencyKey, accountID)
	if err != nil {
		return fmt.Errorf("bind runtime onboarding submission proxy: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return errRuntimeOnboardingIdempotency
	}
	return nil
}

func (a *app) getRuntimeOnboardingSubmission(ctx context.Context, idempotencyKey string) (runtimeOnboardingSubmission, error) {
	if a == nil || a.db == nil || !runtimeOpaqueIntentIDPattern.MatchString(idempotencyKey) || runtimeSecretString(idempotencyKey) {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	var submission runtimeOnboardingSubmission
	var requestFingerprint []byte
	err := a.db.QueryRowContext(ctx, `SELECT idempotency_key, intake_idempotency_key, intake_attempt,
		operation_type, account_id, desired_generation, event_type, migration_status, source_type, auth_type,
		proxy_id, status, intent_id, intent_expires_at_millis, event_id,
		request_fingerprint_version, request_fingerprint_sha256
		FROM runtime_onboarding_submissions WHERE idempotency_key = ?`, idempotencyKey).Scan(
		&submission.IdempotencyKey, &submission.IntakeIdempotencyKey, &submission.IntakeAttempt,
		&submission.OperationType, &submission.AccountID, &submission.DesiredGeneration,
		&submission.EventType, &submission.MigrationStatus, &submission.SourceType, &submission.AuthType,
		&submission.ProxyID, &submission.Status, &submission.IntentID, &submission.IntentExpiresAtMillis, &submission.EventID,
		&submission.RequestFingerprintVersion, &requestFingerprint,
	)
	if err != nil {
		return runtimeOnboardingSubmission{}, err
	}
	if len(requestFingerprint) != 0 {
		if len(requestFingerprint) != runtimeOnboardingFingerprintSize {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
		}
		copy(submission.RequestFingerprintSHA256[:], requestFingerprint)
		submission.RequestFingerprintPresent = true
	}
	if validateRuntimeOnboardingSubmission(submission, false) != nil {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	return submission, nil
}

func (a *app) ensureRuntimeOnboardingSubmission(
	ctx context.Context,
	idempotencyKey string,
	request runtimeTransitionRequest,
	material *runtimeOnboardingMaterial,
) (runtimeOnboardingSubmission, error) {
	if a == nil || a.db == nil || material == nil || request.AccountID <= 0 {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	loaded, err := a.getRuntimeOnboardingSubmission(ctx, idempotencyKey)
	if err == nil {
		if !runtimeOnboardingSubmissionMatchesRequest(loaded, idempotencyKey, request, material) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
		}
		return a.validateRuntimeOnboardingSubmissionAccountOrRefreshQueued(ctx, loaded, idempotencyKey, request, material)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return runtimeOnboardingSubmission{}, err
	}

	// New-account requests install their claim in the account INSERT
	// transaction so two callers cannot leave two pending accounts. Only an
	// existing-account reauthorization may claim a key here.
	if runtimeOnboardingOperationForEvent(request.EventType) != runtimeOnboardingOperationReauthorize {
		// Preserve the account-state fence before reporting an idempotency
		// conflict. In particular, an archived/deleted account must look like a
		// missing intake target and must never reach the execution plane.
		var activeAccountID int64
		if err := a.db.QueryRowContext(ctx, `SELECT id FROM accounts
			WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`, request.AccountID).Scan(&activeAccountID); err != nil {
			return runtimeOnboardingSubmission{}, err
		}
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return runtimeOnboardingSubmission{}, err
	}
	defer tx.Rollback()
	query := `SELECT execution_migration_status, runtime_status, runtime_generation, proxy_id FROM accounts
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`
	if a.db.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var migrationStatus, runtimeStatus string
	var generation uint64
	var proxyID sql.NullInt64
	if err := tx.QueryRowContext(ctx, query, request.AccountID).Scan(&migrationStatus, &runtimeStatus, &generation, &proxyID); err != nil {
		return runtimeOnboardingSubmission{}, err
	}
	if !allowedRuntimeOnboardingReauthorizePredecessor(runtimeStatus) || !allowedRuntimeOnboardingMigration(migrationStatus) ||
		strings.TrimSpace(request.MigrationStatus) != migrationStatus ||
		!proxyID.Valid || proxyID.Int64 <= 0 || generation == ^uint64(0) {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	candidate := runtimeOnboardingSubmission{
		IdempotencyKey: idempotencyKey, IntakeIdempotencyKey: newRuntimeOnboardingIntakeKey(),
		OperationType: runtimeOnboardingOperationReauthorize,
		AccountID:     request.AccountID, DesiredGeneration: generation + 1, EventType: request.EventType,
		MigrationStatus: migrationStatus, SourceType: material.Source, AuthType: material.AuthType,
		ProxyID: proxyID, Status: runtimeOnboardingSubmissionPending,
	}
	created, err := insertRuntimeOnboardingSubmissionTx(ctx, tx, candidate)
	if err != nil {
		return runtimeOnboardingSubmission{}, err
	}
	if !created {
		// A unique-key conflict may be either an exact-key replay or another key
		// claiming this account generation. Roll back the stale snapshot and
		// replay only the exact-key winner; a different-key winner is a 409.
		_ = tx.Rollback()
		winner, loadErr := a.getRuntimeOnboardingSubmission(ctx, idempotencyKey)
		if errors.Is(loadErr, sql.ErrNoRows) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
		}
		if loadErr != nil {
			return runtimeOnboardingSubmission{}, loadErr
		}
		if !runtimeOnboardingSubmissionMatchesRequest(winner, idempotencyKey, request, material) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
		}
		return a.validateRuntimeOnboardingSubmissionAccountOrRefreshQueued(ctx, winner, idempotencyKey, request, material)
	}
	if err := tx.Commit(); err != nil {
		return runtimeOnboardingSubmission{}, err
	}
	// insertRuntimeOnboardingSubmissionTx may have regenerated the internal
	// intake key after a collision. Reload the committed row so the caller never
	// crosses the execution boundary with the stale candidate value.
	return a.getRuntimeOnboardingSubmission(ctx, idempotencyKey)
}

func runtimeOnboardingReceiptFromSubmission(submission runtimeOnboardingSubmission) (runtimeOnboardingIntentReceipt, bool) {
	if validateRuntimeOnboardingSubmission(submission, false) != nil || submission.IntentID == "" ||
		submission.IntentExpiresAtMillis <= 0 {
		return runtimeOnboardingIntentReceipt{}, false
	}
	return runtimeOnboardingIntentReceipt{
		IdempotencyKey:    submission.IntakeIdempotencyKey,
		IntentID:          submission.IntentID,
		AccountID:         submission.AccountID,
		DesiredGeneration: submission.DesiredGeneration,
		ExpiresAt:         time.UnixMilli(submission.IntentExpiresAtMillis).UTC(),
	}, true
}

// persistRuntimeOnboardingReceipt closes the execution-intake/CCMAX crash
// window. It records only an opaque receipt and exact request fences; no
// onboarding material or material-derived digest is stored.
func (a *app) persistRuntimeOnboardingReceipt(
	ctx context.Context,
	externalKey string,
	submission runtimeOnboardingSubmission,
	receipt runtimeOnboardingIntentReceipt,
	now time.Time,
) (runtimeOnboardingSubmission, error) {
	if a == nil || a.db == nil || ctx == nil || ctx.Err() != nil || now.IsZero() ||
		submission.IdempotencyKey != externalKey || submission.Status != runtimeOnboardingSubmissionPending ||
		receipt.IdempotencyKey != submission.IntakeIdempotencyKey || receipt.AccountID != submission.AccountID ||
		receipt.DesiredGeneration != submission.DesiredGeneration ||
		!runtimeOpaqueIntentIDPattern.MatchString(receipt.IntentID) || runtimeSecretString(receipt.IntentID) ||
		!receipt.ExpiresAt.UTC().After(now.UTC()) {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	expiresAtMillis := receipt.ExpiresAt.UTC().UnixMilli()
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return runtimeOnboardingSubmission{}, fmt.Errorf("begin runtime onboarding receipt persistence: %w", err)
	}
	defer tx.Rollback()
	accountQuery := `SELECT runtime_generation, proxy_id FROM accounts
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`
	if a.db.dialect == dialectMySQL {
		accountQuery += ` FOR UPDATE`
	}
	var generation uint64
	var proxyID sql.NullInt64
	scanErr := tx.QueryRowContext(ctx, accountQuery, submission.AccountID).Scan(&generation, &proxyID)
	if scanErr != nil {
		if errors.Is(scanErr, sql.ErrNoRows) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
		}
		return runtimeOnboardingSubmission{}, fmt.Errorf("validate account before persisting runtime onboarding receipt: %w", scanErr)
	}
	if generation != submission.DesiredGeneration-1 || !proxyID.Valid || proxyID.Int64 != submission.ProxyID.Int64 {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	result, err := tx.ExecContext(ctx, `UPDATE runtime_onboarding_submissions SET
		intent_id = ?, intent_expires_at_millis = ?, updated_at = `+nowSQL+`
		WHERE idempotency_key = ? AND intake_idempotency_key = ? AND intake_attempt = ?
		  AND account_id = ? AND desired_generation = ? AND event_type = ? AND proxy_id = ?
		  AND status = 'pending' AND intent_id = '' AND intent_expires_at_millis = 0 AND event_id = ''`,
		receipt.IntentID, expiresAtMillis, externalKey, submission.IntakeIdempotencyKey, submission.IntakeAttempt,
		submission.AccountID, submission.DesiredGeneration, submission.EventType, submission.ProxyID.Int64)
	if err != nil {
		return runtimeOnboardingSubmission{}, fmt.Errorf("persist runtime onboarding receipt: %w", err)
	}
	affected, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return runtimeOnboardingSubmission{}, fmt.Errorf("read runtime onboarding receipt CAS result: %w", rowsErr)
	}
	if affected == 1 {
		if err := tx.Commit(); err != nil {
			return runtimeOnboardingSubmission{}, fmt.Errorf("commit runtime onboarding receipt persistence: %w", err)
		}
		stored := submission
		stored.IntentID = receipt.IntentID
		stored.IntentExpiresAtMillis = expiresAtMillis
		return stored, nil
	}
	_ = tx.Rollback()
	stored, loadErr := a.getRuntimeOnboardingSubmission(ctx, externalKey)
	if loadErr != nil {
		return runtimeOnboardingSubmission{}, runtimeOnboardingReloadError("reload runtime onboarding receipt after CAS miss", loadErr)
	}
	storedReceipt, ok := runtimeOnboardingReceiptFromSubmission(stored)
	if runtimeOnboardingSubmissionIsNewerPendingSuccessor(submission, stored) {
		return stored, errRuntimeOnboardingAttemptSuperseded
	}
	if !ok || storedReceipt.IdempotencyKey != receipt.IdempotencyKey || storedReceipt.IntentID != receipt.IntentID ||
		storedReceipt.AccountID != receipt.AccountID || storedReceipt.DesiredGeneration != receipt.DesiredGeneration ||
		storedReceipt.ExpiresAt.UnixMilli() != expiresAtMillis {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	return stored, nil
}

// advanceRuntimeOnboardingAttempt supersedes an execution-plane key only after
// the caller has definitive proof that the current key exists but cannot be
// recovered (or its persisted receipt has lost the commit margin). The CAS
// prevents a late response from an older attempt from being made durable.
func (a *app) advanceRuntimeOnboardingAttempt(
	ctx context.Context,
	current runtimeOnboardingSubmission,
) (runtimeOnboardingSubmission, error) {
	if a == nil || a.db == nil || ctx == nil || ctx.Err() != nil ||
		validateRuntimeOnboardingSubmission(current, false) != nil ||
		current.Status != runtimeOnboardingSubmissionPending || current.IntakeAttempt >= runtimeOnboardingMaxIntakeAttempt {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	nextAttempt := current.IntakeAttempt + 1
	nextKey := newRuntimeOnboardingIntakeKey()
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return runtimeOnboardingSubmission{}, fmt.Errorf("begin runtime onboarding attempt advance: %w", err)
	}
	defer tx.Rollback()
	accountQuery := `SELECT runtime_generation, proxy_id FROM accounts
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`
	if a.db.dialect == dialectMySQL {
		accountQuery += ` FOR UPDATE`
	}
	var generation uint64
	var proxyID sql.NullInt64
	scanErr := tx.QueryRowContext(ctx, accountQuery, current.AccountID).Scan(&generation, &proxyID)
	if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
		return runtimeOnboardingSubmission{}, fmt.Errorf("validate account before advancing runtime onboarding attempt: %w", scanErr)
	}
	if scanErr != nil || generation != current.DesiredGeneration-1 || !proxyID.Valid || proxyID.Int64 != current.ProxyID.Int64 {
		_ = tx.Rollback()
		stored, loadErr := a.getRuntimeOnboardingSubmission(ctx, current.IdempotencyKey)
		if loadErr != nil && !errors.Is(loadErr, sql.ErrNoRows) {
			return runtimeOnboardingSubmission{}, fmt.Errorf("reload runtime onboarding attempt after account fence: %w", loadErr)
		}
		if loadErr == nil && runtimeOnboardingSubmissionIsQueuedSuccessor(current, stored) {
			if _, queuedErr := a.queuedRuntimeOnboardingEvent(ctx, stored); queuedErr != nil {
				return runtimeOnboardingSubmission{}, queuedErr
			}
			return stored, nil
		}
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	result, err := tx.ExecContext(ctx, `UPDATE runtime_onboarding_submissions SET
		intake_idempotency_key = ?, intake_attempt = ?, intent_id = '', intent_expires_at_millis = 0,
		updated_at = `+nowSQL+`
		WHERE idempotency_key = ? AND intake_idempotency_key = ? AND intake_attempt = ?
		  AND account_id = ? AND desired_generation = ? AND event_type = ? AND proxy_id = ?
		  AND status = 'pending' AND intent_id = ? AND intent_expires_at_millis = ? AND event_id = ''`,
		nextKey, nextAttempt, current.IdempotencyKey, current.IntakeIdempotencyKey, current.IntakeAttempt,
		current.AccountID, current.DesiredGeneration, current.EventType, current.ProxyID.Int64,
		current.IntentID, current.IntentExpiresAtMillis)
	if err != nil {
		return runtimeOnboardingSubmission{}, fmt.Errorf("advance runtime onboarding attempt: %w", err)
	}
	affected, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return runtimeOnboardingSubmission{}, fmt.Errorf("read runtime onboarding attempt CAS result: %w", rowsErr)
	}
	if affected == 1 {
		if err := tx.Commit(); err != nil {
			return runtimeOnboardingSubmission{}, fmt.Errorf("commit runtime onboarding attempt advance: %w", err)
		}
		current.IntakeIdempotencyKey = nextKey
		current.IntakeAttempt = nextAttempt
		current.IntentID = ""
		current.IntentExpiresAtMillis = 0
		return current, nil
	}
	_ = tx.Rollback()
	stored, loadErr := a.getRuntimeOnboardingSubmission(ctx, current.IdempotencyKey)
	if loadErr != nil {
		return runtimeOnboardingSubmission{}, runtimeOnboardingReloadError("reload runtime onboarding attempt after CAS miss", loadErr)
	}
	if stored.IntakeAttempt < current.IntakeAttempt ||
		(stored.IntakeAttempt == current.IntakeAttempt && stored.IntakeIdempotencyKey == current.IntakeIdempotencyKey &&
			stored.IntentID == current.IntentID && stored.IntentExpiresAtMillis == current.IntentExpiresAtMillis &&
			stored.Status == current.Status) {
		return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
	}
	if err := a.validateRuntimeOnboardingSubmissionAccount(ctx, stored); err != nil {
		if errors.Is(err, errRuntimeOnboardingIdempotency) || errors.Is(err, sql.ErrNoRows) {
			return runtimeOnboardingSubmission{}, errRuntimeOnboardingIdempotency
		}
		return runtimeOnboardingSubmission{}, fmt.Errorf("validate reloaded runtime onboarding attempt: %w", err)
	}
	return stored, nil
}

func runtimeOnboardingSubmissionIsQueuedSuccessor(current, stored runtimeOnboardingSubmission) bool {
	sameAttempt := stored.IntakeAttempt == current.IntakeAttempt &&
		stored.IntakeIdempotencyKey == current.IntakeIdempotencyKey
	newerAttempt := stored.IntakeAttempt > current.IntakeAttempt
	return stored.Status == runtimeOnboardingSubmissionQueued && (sameAttempt || newerAttempt) &&
		runtimeOnboardingSubmissionSameIdentity(current, stored)
}

func runtimeOnboardingSubmissionIsNewerPendingSuccessor(current, stored runtimeOnboardingSubmission) bool {
	return stored.Status == runtimeOnboardingSubmissionPending && stored.IntakeAttempt > current.IntakeAttempt &&
		runtimeOnboardingSubmissionSameIdentity(current, stored)
}

func runtimeOnboardingSubmissionSameIdentity(current, stored runtimeOnboardingSubmission) bool {
	return stored.IdempotencyKey == current.IdempotencyKey &&
		stored.OperationType == current.OperationType &&
		stored.AccountID == current.AccountID &&
		stored.DesiredGeneration == current.DesiredGeneration &&
		stored.EventType == current.EventType &&
		stored.MigrationStatus == current.MigrationStatus &&
		stored.SourceType == current.SourceType &&
		stored.AuthType == current.AuthType &&
		stored.ProxyID == current.ProxyID &&
		stored.RequestFingerprintVersion == current.RequestFingerprintVersion &&
		stored.RequestFingerprintPresent == current.RequestFingerprintPresent &&
		stored.RequestFingerprintSHA256 == current.RequestFingerprintSHA256
}

func (a *app) validateRuntimeOnboardingSubmissionAccount(ctx context.Context, submission runtimeOnboardingSubmission) error {
	var generation uint64
	var proxyID sql.NullInt64
	if err := a.db.QueryRowContext(ctx, `SELECT runtime_generation, proxy_id FROM accounts
		WHERE id = ? AND deleted_at IS NULL AND archived_at IS NULL`, submission.AccountID).Scan(&generation, &proxyID); err != nil {
		return err
	}
	if !proxyID.Valid || !submission.ProxyID.Valid || proxyID.Int64 != submission.ProxyID.Int64 {
		return errRuntimeOnboardingIdempotency
	}
	if submission.Status == runtimeOnboardingSubmissionPending && generation+1 != submission.DesiredGeneration {
		return errRuntimeOnboardingIdempotency
	}
	if submission.Status == runtimeOnboardingSubmissionQueued && generation < submission.DesiredGeneration {
		return errRuntimeOnboardingIdempotency
	}
	return nil
}

// validateRuntimeOnboardingSubmissionAccountOrRefreshQueued closes the small
// replay race where another replica commits the pending submission after this
// caller loads it but before it validates the account generation. Only the
// exact queued successor is accepted; all other identity or state changes fail
// closed.
func (a *app) validateRuntimeOnboardingSubmissionAccountOrRefreshQueued(
	ctx context.Context,
	submission runtimeOnboardingSubmission,
	idempotencyKey string,
	request runtimeTransitionRequest,
	material *runtimeOnboardingMaterial,
) (runtimeOnboardingSubmission, error) {
	validationErr := a.validateRuntimeOnboardingSubmissionAccount(ctx, submission)
	if validationErr == nil {
		return submission, nil
	}
	latest, loadErr := a.getRuntimeOnboardingSubmission(ctx, idempotencyKey)
	if loadErr != nil {
		if !errors.Is(loadErr, sql.ErrNoRows) {
			return runtimeOnboardingSubmission{}, loadErr
		}
		return runtimeOnboardingSubmission{}, validationErr
	}
	if latest.Status != runtimeOnboardingSubmissionQueued ||
		!runtimeOnboardingSubmissionMatchesRequest(latest, idempotencyKey, request, material) {
		return runtimeOnboardingSubmission{}, validationErr
	}
	if err := a.validateRuntimeOnboardingSubmissionAccount(ctx, latest); err != nil {
		return runtimeOnboardingSubmission{}, err
	}
	return latest, nil
}

func (a *app) queuedRuntimeOnboardingEvent(ctx context.Context, submission runtimeOnboardingSubmission) (runtimeOutboxEvent, error) {
	if submission.Status != runtimeOnboardingSubmissionQueued || validateRuntimeOnboardingSubmission(submission, false) != nil {
		return runtimeOutboxEvent{}, errRuntimeOnboardingIdempotency
	}
	var event runtimeOutboxEvent
	err := a.db.QueryRowContext(ctx, `SELECT sequence, event_id, account_id, event_type, desired_generation, payload_json, created_at
		FROM runtime_outbox WHERE event_id = ?`, submission.EventID).Scan(
		&event.Sequence, &event.EventID, &event.AccountID, &event.EventType, &event.DesiredGeneration, &event.PayloadJSON, &event.CreatedAt,
	)
	if err != nil || event.AccountID != submission.AccountID || event.EventType != submission.EventType ||
		event.DesiredGeneration != submission.DesiredGeneration {
		if err != nil {
			return runtimeOutboxEvent{}, err
		}
		return runtimeOutboxEvent{}, errRuntimeOnboardingIdempotency
	}
	intentID, err := runtimeOnboardingIntentID(event.PayloadJSON)
	if err != nil || intentID != submission.IntentID {
		return runtimeOutboxEvent{}, errRuntimeOnboardingIdempotency
	}
	return event, nil
}

func (a *app) replayRuntimeOnboardingCreate(
	ctx context.Context,
	intake runtimeOnboardingIntentIntake,
	idempotencyKey string,
	material *runtimeOnboardingMaterial,
	requestFingerprint [runtimeOnboardingFingerprintSize]byte,
) (int64, runtimeOutboxEvent, bool, error) {
	submission, err := a.getRuntimeOnboardingSubmission(ctx, idempotencyKey)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, runtimeOutboxEvent{}, false, nil
	}
	if err != nil {
		if material != nil {
			material.Destroy()
		}
		return 0, runtimeOutboxEvent{}, true, err
	}
	if submission.OperationType != runtimeOnboardingOperationCreate ||
		submission.EventType != "account.runtime.provision_requested" || submission.MigrationStatus != "migrating" || material == nil ||
		submission.SourceType != material.Source || submission.AuthType != material.AuthType ||
		submission.RequestFingerprintVersion != runtimeOnboardingCreateFingerprintVersion ||
		!submission.RequestFingerprintPresent || submission.RequestFingerprintSHA256 != requestFingerprint {
		if material != nil {
			material.Destroy()
		}
		return 0, runtimeOutboxEvent{}, true, errRuntimeOnboardingIdempotency
	}
	event, err := a.requestRuntimeOnboardingWithMaterial(ctx, intake, idempotencyKey, runtimeTransitionRequest{
		AccountID: submission.AccountID, EventType: submission.EventType,
		MigrationStatus: submission.MigrationStatus, RuntimeStatus: "provisioning",
	}, material)
	if err != nil {
		return submission.AccountID, runtimeOutboxEvent{}, true, err
	}
	return submission.AccountID, event, true, nil
}
