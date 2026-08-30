package store

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

func TestRecoverOnboardingIntentReceiptUsesExactNonSecretMetadataQuery(t *testing.T) {
	lowerQuery := strings.ToLower(onboardingReceiptRecoverySelect)
	for _, forbidden := range []string{"ciphertext", "encrypted_dek", "nonce", "aad_json", "kms_key_id", "kms_key_version"} {
		if strings.Contains(lowerQuery, forbidden) {
			t.Fatalf("receipt recovery query selected secret envelope column %q", forbidden)
		}
	}
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	lookup := onboarding.ReceiptLookup{
		IdempotencyKey: "event-10380", AccountID: "10380", DesiredGeneration: 7,
		Source: "session_key", AuthType: "oauth", Now: now,
	}
	expiresAt := now.Add(30 * time.Minute)
	mock.ExpectQuery(regexp.QuoteMeta(onboardingReceiptRecoverySelect)).
		WithArgs(lookup.IdempotencyKey).
		WillReturnRows(sqlmock.NewRows([]string{
			"intent_id", "account_id", "desired_generation", "source_type", "auth_type", "status", "expires_at",
		}).AddRow("intent-10380", "10380", 7, "session_key", "oauth", onboarding.IntentPending, expiresAt))
	receipt, err := repository.RecoverIntentReceipt(context.Background(), lookup)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.IntentID != "intent-10380" || receipt.AccountID != lookup.AccountID ||
		receipt.DesiredGeneration != lookup.DesiredGeneration || !receipt.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("recovered receipt = %+v", receipt)
	}

	wrongIdentity := lookup
	wrongIdentity.AccountID = "10381"
	mock.ExpectQuery(regexp.QuoteMeta(onboardingReceiptRecoverySelect)).
		WithArgs(wrongIdentity.IdempotencyKey).
		WillReturnRows(sqlmock.NewRows([]string{
			"intent_id", "account_id", "desired_generation", "source_type", "auth_type", "status", "expires_at",
		}).AddRow("intent-10380", "10380", 7, "session_key", "oauth", onboarding.IntentPending, expiresAt))
	if _, err := repository.RecoverIntentReceipt(context.Background(), wrongIdentity); !errors.Is(err, onboarding.ErrIntentUnavailable) {
		t.Fatalf("wrong identity recovery error = %v", err)
	}

	missing := lookup
	missing.IdempotencyKey = "event-missing"
	mock.ExpectQuery(regexp.QuoteMeta(onboardingReceiptRecoverySelect)).
		WithArgs(missing.IdempotencyKey).
		WillReturnRows(sqlmock.NewRows([]string{
			"intent_id", "account_id", "desired_generation", "source_type", "auth_type", "status", "expires_at",
		}))
	if _, err := repository.RecoverIntentReceipt(context.Background(), missing); !errors.Is(err, onboarding.ErrIntentNotFound) {
		t.Fatalf("missing recovery error = %v", err)
	}

	expired := lookup
	expired.Now = expiresAt
	mock.ExpectQuery(regexp.QuoteMeta(onboardingReceiptRecoverySelect)).
		WithArgs(expired.IdempotencyKey).
		WillReturnRows(sqlmock.NewRows([]string{
			"intent_id", "account_id", "desired_generation", "source_type", "auth_type", "status", "expires_at",
		}).AddRow("intent-10380", "10380", 7, "session_key", "oauth", onboarding.IntentPending, expiresAt))
	if _, err := repository.RecoverIntentReceipt(context.Background(), expired); !errors.Is(err, onboarding.ErrIntentExpired) {
		t.Fatalf("expired recovery error = %v", err)
	}
	wrongExpired := expired
	wrongExpired.AccountID = "10381"
	mock.ExpectQuery(regexp.QuoteMeta(onboardingReceiptRecoverySelect)).
		WithArgs(wrongExpired.IdempotencyKey).
		WillReturnRows(sqlmock.NewRows([]string{
			"intent_id", "account_id", "desired_generation", "source_type", "auth_type", "status", "expires_at",
		}).AddRow("intent-10380", "10380", 7, "session_key", "oauth", onboarding.IntentPending, expiresAt))
	if _, err := repository.RecoverIntentReceipt(context.Background(), wrongExpired); !errors.Is(err, onboarding.ErrIntentUnavailable) {
		t.Fatalf("expired wrong-identity recovery error = %v", err)
	}
	for _, intentStatus := range []string{onboarding.IntentClaimed, onboarding.IntentConsumed} {
		mock.ExpectQuery(regexp.QuoteMeta(onboardingReceiptRecoverySelect)).
			WithArgs(lookup.IdempotencyKey).
			WillReturnRows(sqlmock.NewRows([]string{
				"intent_id", "account_id", "desired_generation", "source_type", "auth_type", "status", "expires_at",
			}).AddRow("intent-10380", "10380", 7, "session_key", "oauth", intentStatus, expiresAt))
		if _, err := repository.RecoverIntentReceipt(context.Background(), lookup); !errors.Is(err, onboarding.ErrIntentUnavailable) {
			t.Fatalf("%s recovery error = %v", intentStatus, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateOnboardingIntentIsIdempotentByOpaqueKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	candidate := testOnboardingIntent("intent-new")
	stored := testOnboardingIntent("intent-existing")

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO onboarding_intents.*ON DUPLICATE KEY UPDATE`).
		WithArgs(
			candidate.ID, candidate.IdempotencyKey, candidate.AccountID, candidate.DesiredGeneration,
			candidate.Source, candidate.AuthType, candidate.Envelope.Ciphertext, candidate.Envelope.EncryptedDEK,
			candidate.Envelope.Nonce, candidate.Envelope.AADJSON, candidate.Envelope.KMSKeyID, candidate.Envelope.KMSKeyVersion,
			candidate.ExpiresAt, candidate.CreatedAt, candidate.UpdatedAt,
		).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`(?s)SELECT intent_id.*FROM onboarding_intents WHERE idempotency_key = \? FOR UPDATE`).
		WithArgs(candidate.IdempotencyKey).
		WillReturnRows(onboardingIntentRows(stored))
	mock.ExpectCommit()

	result, created, err := repository.CreateIntent(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	defer result.Destroy()
	if created || result.ID != stored.ID {
		t.Fatalf("idempotent create result/created = %+v/%t", result, created)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimAndCompleteOnboardingIntentAreFencedAndCompletionIsIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	pending := testOnboardingIntent("intent-10380")
	claimedAt := pending.CreatedAt.Add(time.Minute)
	claimExpires := claimedAt.Add(5 * time.Minute)
	claim := onboarding.Claim{
		IntentID: pending.ID, AccountID: pending.AccountID, DesiredGeneration: pending.DesiredGeneration,
		Owner: "job-10380", ClaimedAt: claimedAt, ClaimExpiresAt: claimExpires,
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT intent_id.*FROM onboarding_intents WHERE intent_id = \? FOR UPDATE`).
		WithArgs(pending.ID).
		WillReturnRows(onboardingIntentRows(pending))
	mock.ExpectExec(`(?s)UPDATE onboarding_intents SET status = 'claimed'.*WHERE intent_id = \?`).
		WithArgs(claim.Owner, claimExpires, claimedAt, pending.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claimed, err := repository.ClaimIntent(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != onboarding.IntentClaimed || claimed.ClaimOwner != claim.Owner || claimed.ClaimExpiresAt == nil || !claimed.ClaimExpiresAt.Equal(claimExpires) {
		t.Fatalf("claimed intent = %+v", claimed)
	}
	claimed.Destroy()

	completedAt := claimedAt.Add(2 * time.Minute)
	claimedRecord := pending.Clone()
	claimedRecord.Status = onboarding.IntentClaimed
	claimedRecord.ClaimOwner = claim.Owner
	claimedRecord.ClaimExpiresAt = &claimExpires
	claimedRecord.UpdatedAt = claimedAt
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT intent_id.*FROM onboarding_intents WHERE intent_id = \? FOR UPDATE`).
		WithArgs(pending.ID).
		WillReturnRows(onboardingIntentRows(claimedRecord))
	mock.ExpectExec(`(?s)UPDATE onboarding_intents SET status = 'consumed'.*WHERE intent_id = \? AND status = 'claimed'`).
		WithArgs(completedAt, completedAt, pending.ID, claim.Owner).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := repository.CompleteIntent(context.Background(), pending.ID, claim.Owner, completedAt); err != nil {
		t.Fatal(err)
	}

	consumedRecord := claimedRecord.Clone()
	consumedRecord.Status = onboarding.IntentConsumed
	consumedRecord.ClaimExpiresAt = nil
	consumedRecord.ConsumedAt = &completedAt
	consumedRecord.UpdatedAt = completedAt
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT intent_id.*FROM onboarding_intents WHERE intent_id = \? FOR UPDATE`).
		WithArgs(pending.ID).
		WillReturnRows(onboardingIntentRows(consumedRecord))
	mock.ExpectCommit()
	if err := repository.CompleteIntent(context.Background(), pending.ID, claim.Owner, completedAt.Add(time.Second)); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}

	claimedRecord.Destroy()
	consumedRecord.Destroy()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func testOnboardingIntent(id string) onboarding.Intent {
	now := time.Unix(2_000_000_000, 0).UTC()
	return onboarding.Intent{
		ID: id, IdempotencyKey: "event-10380", AccountID: "10380", DesiredGeneration: 7,
		Source: "session_key", AuthType: "oauth",
		Envelope: credential.Envelope{
			Ciphertext: bytes.Repeat([]byte{0x11}, 32), EncryptedDEK: []byte("wrapped-data-key"),
			Nonce: bytes.Repeat([]byte{0x22}, 12), AADJSON: []byte(`{"schema":"ccmax.onboarding-intent.v1"}`),
			KMSKeyID: "kms-test", KMSKeyVersion: "v1",
		},
		Status: onboarding.IntentPending, ExpiresAt: now.Add(30 * time.Minute), CreatedAt: now, UpdatedAt: now,
	}
}

func onboardingIntentRows(intent onboarding.Intent) *sqlmock.Rows {
	var claimExpires any
	if intent.ClaimExpiresAt != nil {
		claimExpires = intent.ClaimExpiresAt.UTC()
	}
	var consumedAt any
	if intent.ConsumedAt != nil {
		consumedAt = intent.ConsumedAt.UTC()
	}
	return sqlmock.NewRows([]string{
		"intent_id", "idempotency_key", "account_id", "desired_generation", "source_type", "auth_type",
		"ciphertext", "encrypted_dek", "nonce", "aad_json", "kms_key_id", "kms_key_version",
		"status", "claim_owner", "claim_expires_at", "expires_at", "consumed_at", "created_at", "updated_at",
	}).AddRow(
		intent.ID, intent.IdempotencyKey, intent.AccountID, intent.DesiredGeneration, intent.Source, intent.AuthType,
		intent.Envelope.Ciphertext, intent.Envelope.EncryptedDEK, intent.Envelope.Nonce, intent.Envelope.AADJSON,
		intent.Envelope.KMSKeyID, intent.Envelope.KMSKeyVersion, intent.Status, intent.ClaimOwner,
		claimExpires, intent.ExpiresAt, consumedAt, intent.CreatedAt, intent.UpdatedAt,
	)
}
