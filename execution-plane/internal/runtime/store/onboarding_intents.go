package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

func (r *Repository) CreateIntent(ctx context.Context, candidate onboarding.Intent) (onboarding.Intent, bool, error) {
	if candidate.Validate() != nil {
		return onboarding.Intent{}, false, onboarding.ErrIntentRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return onboarding.Intent{}, false, fmt.Errorf("begin onboarding intent create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
INSERT INTO onboarding_intents (
  intent_id, idempotency_key, account_id, desired_generation, source_type, auth_type,
  ciphertext, encrypted_dek, nonce, aad_json, kms_key_id, kms_key_version,
  status, claim_owner, claim_expires_at, expires_at, consumed_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', '', NULL, ?, NULL, ?, ?)
ON DUPLICATE KEY UPDATE idempotency_key = VALUES(idempotency_key)`,
		candidate.ID, candidate.IdempotencyKey, candidate.AccountID, candidate.DesiredGeneration,
		candidate.Source, candidate.AuthType, candidate.Envelope.Ciphertext, candidate.Envelope.EncryptedDEK,
		candidate.Envelope.Nonce, candidate.Envelope.AADJSON, candidate.Envelope.KMSKeyID, candidate.Envelope.KMSKeyVersion,
		candidate.ExpiresAt.UTC(), candidate.CreatedAt.UTC(), candidate.UpdatedAt.UTC(),
	)
	if err != nil {
		return onboarding.Intent{}, false, fmt.Errorf("insert onboarding intent: %w", err)
	}
	stored, err := getOnboardingIntentByIdempotency(ctx, tx, candidate.IdempotencyKey, true)
	if err != nil {
		return onboarding.Intent{}, false, err
	}
	if !sameOnboardingIntentIdentity(stored, candidate) {
		stored.Destroy()
		return onboarding.Intent{}, false, onboarding.ErrIntentRejected
	}
	if err := tx.Commit(); err != nil {
		stored.Destroy()
		return onboarding.Intent{}, false, fmt.Errorf("commit onboarding intent create: %w", err)
	}
	return stored, stored.ID == candidate.ID, nil
}

// RecoverIntentReceipt reads only non-secret identity metadata. In particular,
// this query must never select ciphertext, encrypted_dek or other envelope
// columns merely to recover an already-issued receipt.
func (r *Repository) RecoverIntentReceipt(ctx context.Context, lookup onboarding.ReceiptLookup) (onboarding.Receipt, error) {
	if lookup.Validate() != nil {
		return onboarding.Receipt{}, onboarding.ErrIntentUnavailable
	}
	var receipt onboarding.Receipt
	var intentStatus string
	err := r.db.QueryRowContext(ctx, onboardingReceiptRecoverySelect,
		lookup.IdempotencyKey,
	).Scan(
		&receipt.IntentID, &receipt.AccountID, &receipt.DesiredGeneration,
		&receipt.Source, &receipt.AuthType, &intentStatus, &receipt.ExpiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.Receipt{}, onboarding.ErrIntentNotFound
	}
	if err != nil {
		return onboarding.Receipt{}, fmt.Errorf("recover onboarding intent receipt: %w", err)
	}
	receipt.ExpiresAt = receipt.ExpiresAt.UTC()
	if credential.ValidateTransportID(receipt.IntentID) != nil || receipt.AccountID != lookup.AccountID ||
		receipt.DesiredGeneration != lookup.DesiredGeneration || receipt.Source != lookup.Source ||
		receipt.AuthType != lookup.AuthType || intentStatus != onboarding.IntentPending {
		return onboarding.Receipt{}, onboarding.ErrIntentUnavailable
	}
	if !receipt.ExpiresAt.After(lookup.Now) {
		return onboarding.Receipt{}, onboarding.ErrIntentExpired
	}
	return receipt, nil
}

func (r *Repository) ClaimIntent(ctx context.Context, claim onboarding.Claim) (onboarding.Intent, error) {
	if claim.Validate() != nil {
		return onboarding.Intent{}, onboarding.ErrIntentUnavailable
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return onboarding.Intent{}, fmt.Errorf("begin onboarding intent claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	intent, err := getOnboardingIntentByID(ctx, tx, claim.IntentID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.Intent{}, onboarding.ErrIntentUnavailable
	}
	if err != nil {
		return onboarding.Intent{}, err
	}
	if intent.AccountID != claim.AccountID || intent.DesiredGeneration != claim.DesiredGeneration ||
		intent.Status == onboarding.IntentConsumed || !intent.ExpiresAt.After(claim.ClaimedAt) ||
		(intent.Status == onboarding.IntentClaimed && intent.ClaimOwner != claim.Owner && intent.ClaimExpiresAt != nil && intent.ClaimExpiresAt.After(claim.ClaimedAt)) {
		intent.Destroy()
		return onboarding.Intent{}, onboarding.ErrIntentUnavailable
	}
	result, err := tx.ExecContext(ctx, `
UPDATE onboarding_intents SET status = 'claimed', claim_owner = ?, claim_expires_at = ?, updated_at = ?
WHERE intent_id = ?`, claim.Owner, claim.ClaimExpiresAt.UTC(), claim.ClaimedAt.UTC(), claim.IntentID)
	if err != nil {
		intent.Destroy()
		return onboarding.Intent{}, fmt.Errorf("claim onboarding intent: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		intent.Destroy()
		return onboarding.Intent{}, onboarding.ErrIntentUnavailable
	}
	intent.Status = onboarding.IntentClaimed
	intent.ClaimOwner = claim.Owner
	claimExpiry := claim.ClaimExpiresAt.UTC()
	intent.ClaimExpiresAt = &claimExpiry
	intent.UpdatedAt = claim.ClaimedAt.UTC()
	if intent.Validate() != nil {
		intent.Destroy()
		return onboarding.Intent{}, onboarding.ErrIntentUnavailable
	}
	if err := tx.Commit(); err != nil {
		intent.Destroy()
		return onboarding.Intent{}, fmt.Errorf("commit onboarding intent claim: %w", err)
	}
	return intent, nil
}

func (r *Repository) CompleteIntent(ctx context.Context, intentID, owner string, completedAt time.Time) error {
	if credential.ValidateTransportID(intentID) != nil || credential.ValidateTransportID(owner) != nil || completedAt.IsZero() {
		return onboarding.ErrIntentUnavailable
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin onboarding intent completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	intent, err := getOnboardingIntentByID(ctx, tx, intentID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return onboarding.ErrIntentUnavailable
	}
	if err != nil {
		return err
	}
	defer intent.Destroy()
	if intent.ClaimOwner != owner {
		return onboarding.ErrIntentUnavailable
	}
	if intent.Status == onboarding.IntentConsumed {
		return tx.Commit()
	}
	if intent.Status != onboarding.IntentClaimed || intent.ClaimExpiresAt == nil ||
		!intent.ClaimExpiresAt.After(completedAt) || !intent.ExpiresAt.After(completedAt) {
		return onboarding.ErrIntentUnavailable
	}
	result, err := tx.ExecContext(ctx, `
UPDATE onboarding_intents SET status = 'consumed', claim_expires_at = NULL, consumed_at = ?, updated_at = ?
WHERE intent_id = ? AND status = 'claimed' AND claim_owner = ?`, completedAt.UTC(), completedAt.UTC(), intentID, owner)
	if err != nil {
		return fmt.Errorf("complete onboarding intent: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return onboarding.ErrIntentUnavailable
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit onboarding intent completion: %w", err)
	}
	return nil
}

type onboardingIntentQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getOnboardingIntentByID(ctx context.Context, queryer onboardingIntentQueryer, intentID string, forUpdate bool) (onboarding.Intent, error) {
	query := onboardingIntentSelect + " WHERE intent_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanOnboardingIntent(queryer.QueryRowContext(ctx, query, intentID))
}

func getOnboardingIntentByIdempotency(ctx context.Context, queryer onboardingIntentQueryer, idempotencyKey string, forUpdate bool) (onboarding.Intent, error) {
	query := onboardingIntentSelect + " WHERE idempotency_key = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanOnboardingIntent(queryer.QueryRowContext(ctx, query, idempotencyKey))
}

const onboardingIntentSelect = `
SELECT intent_id, idempotency_key, account_id, desired_generation, source_type, auth_type,
       ciphertext, encrypted_dek, nonce, aad_json, kms_key_id, kms_key_version,
       status, claim_owner, claim_expires_at, expires_at, consumed_at, created_at, updated_at
FROM onboarding_intents`

const onboardingReceiptRecoverySelect = `
SELECT intent_id, account_id, desired_generation, source_type, auth_type, status, expires_at
FROM onboarding_intents
WHERE idempotency_key = ?`

func scanOnboardingIntent(row *sql.Row) (onboarding.Intent, error) {
	var intent onboarding.Intent
	var claimExpires, consumed sql.NullTime
	err := row.Scan(
		&intent.ID, &intent.IdempotencyKey, &intent.AccountID, &intent.DesiredGeneration, &intent.Source, &intent.AuthType,
		&intent.Envelope.Ciphertext, &intent.Envelope.EncryptedDEK, &intent.Envelope.Nonce, &intent.Envelope.AADJSON,
		&intent.Envelope.KMSKeyID, &intent.Envelope.KMSKeyVersion, &intent.Status, &intent.ClaimOwner,
		&claimExpires, &intent.ExpiresAt, &consumed, &intent.CreatedAt, &intent.UpdatedAt,
	)
	if err != nil {
		return onboarding.Intent{}, err
	}
	if claimExpires.Valid {
		value := claimExpires.Time.UTC()
		intent.ClaimExpiresAt = &value
	}
	if consumed.Valid {
		value := consumed.Time.UTC()
		intent.ConsumedAt = &value
	}
	intent.ExpiresAt = intent.ExpiresAt.UTC()
	intent.CreatedAt = intent.CreatedAt.UTC()
	intent.UpdatedAt = intent.UpdatedAt.UTC()
	if intent.Validate() != nil {
		intent.Destroy()
		return onboarding.Intent{}, onboarding.ErrIntentRejected
	}
	return intent, nil
}

func sameOnboardingIntentIdentity(left, right onboarding.Intent) bool {
	return left.IdempotencyKey == right.IdempotencyKey && left.AccountID == right.AccountID &&
		left.DesiredGeneration == right.DesiredGeneration && left.Source == right.Source && left.AuthType == right.AuthType
}

var _ onboarding.Repository = (*Repository)(nil)
