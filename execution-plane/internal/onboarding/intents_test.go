package onboarding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

func TestVaultPersistsOnlyEnvelopeAndSupportsSameOwnerReplay(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := NewMemoryRepository()
	vault := testVault(t, repository, now)
	secret := "session-intent-secret"
	input := &worker.OnboardingInput{Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth, Secret: []byte(secret)}
	receipt, err := vault.Create(context.Background(), CreateRequest{
		IdempotencyKey: "event-10380", AccountID: "10380", DesiredGeneration: 7, Input: input,
	})
	if err != nil {
		t.Fatal(err)
	}
	if input.Secret != nil || receipt.IntentID == "" || receipt.AccountID != "10380" {
		t.Fatalf("input/receipt = %+v/%+v", input, receipt)
	}
	stored := repository.byID[receipt.IntentID]
	if bytes.Contains(stored.Envelope.Ciphertext, []byte(secret)) || bytes.Contains(stored.Envelope.EncryptedDEK, []byte(secret)) ||
		strings.Contains(fmt.Sprintf("%+v", stored), secret) {
		t.Fatal("durable intent exposed plaintext")
	}
	encoded, err := json.Marshal(stored)
	if err != nil || bytes.Contains(encoded, []byte(secret)) || bytes.Contains(encoded, stored.Envelope.Ciphertext) {
		t.Fatalf("public intent JSON exposed envelope/material: %s (%v)", encoded, err)
	}
	opened, err := vault.ClaimAndOpen(context.Background(), receipt.IntentID, "10380", 7, "job-10380")
	if err != nil || string(opened.Secret) != secret {
		t.Fatalf("first claim/open = %+v, %v", opened, err)
	}
	opened.Destroy()
	replayed, err := vault.ClaimAndOpen(context.Background(), receipt.IntentID, "10380", 7, "job-10380")
	if err != nil || string(replayed.Secret) != secret {
		t.Fatalf("same-owner replay = %+v, %v", replayed, err)
	}
	replayed.Destroy()
	if _, err := vault.ClaimAndOpen(context.Background(), receipt.IntentID, "10380", 7, "job-other"); !errors.Is(err, ErrIntentUnavailable) {
		t.Fatalf("other owner claim error = %v", err)
	}
	if err := vault.Complete(context.Background(), receipt.IntentID, "job-10380"); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.ClaimAndOpen(context.Background(), receipt.IntentID, "10380", 7, "job-10380"); !errors.Is(err, ErrIntentUnavailable) {
		t.Fatalf("consumed claim error = %v", err)
	}
}

func TestVaultCreateIsIdempotentAndRejectsBindingChange(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := NewMemoryRepository()
	vault := testVault(t, repository, now)
	create := func(account string, generation uint64) (Receipt, error) {
		return vault.Create(context.Background(), CreateRequest{
			IdempotencyKey: "event-idempotent", AccountID: account, DesiredGeneration: generation,
			Input: &worker.OnboardingInput{Source: worker.OnboardingAPIKey, AuthType: worker.AuthTypeAPIKey, Secret: []byte("sk-ant-test")},
		})
	}
	first, err := create("10380", 7)
	if err != nil {
		t.Fatal(err)
	}
	vault.random = bytes.NewReader(bytes.Repeat([]byte{0x62}, 2048))
	second, err := create("10380", 7)
	if err != nil || second.IntentID != first.IntentID {
		t.Fatalf("idempotent create = %+v, %v", second, err)
	}
	opened, err := vault.ClaimAndOpen(context.Background(), first.IntentID, "10380", 7, "job-idempotent")
	if err != nil {
		t.Fatal(err)
	}
	opened.Destroy()
	if err := vault.Complete(context.Background(), first.IntentID, "job-idempotent"); err != nil {
		t.Fatal(err)
	}
	third, err := create("10380", 7)
	if err != nil || third.IntentID != first.IntentID {
		t.Fatalf("post-completion idempotent create = %+v, %v", third, err)
	}
	if _, err := create("10381", 7); !errors.Is(err, ErrIntentRejected) {
		t.Fatalf("changed binding error = %v", err)
	}
}

func TestVaultRecoversOnlyExactPendingUnexpiredReceiptWithoutOpeningEnvelope(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	clock := now
	repository := NewMemoryRepository()
	kms, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x44}, 32), "kms-recovery", "v1")
	if err != nil {
		t.Fatal(err)
	}
	cryptoService, err := credential.NewService(kms)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := NewVault(VaultConfig{
		Crypto: cryptoService, Repository: repository, Random: bytes.NewReader(bytes.Repeat([]byte{0x63}, 2048)),
		Now: func() time.Time { return clock }, IntentTTL: 30 * time.Minute, ClaimTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := vault.Create(context.Background(), CreateRequest{
		IdempotencyKey: "event-recover", AccountID: "10380", DesiredGeneration: 7,
		Input: &worker.OnboardingInput{
			Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth, Secret: []byte("recover-session-secret"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := RecoverRequest{
		IdempotencyKey: "event-recover", AccountID: "10380", DesiredGeneration: 7,
		Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth,
	}
	kms.SetFailures(nil, errors.New("receipt recovery must not decrypt"))
	recovered, err := vault.Recover(context.Background(), request)
	if err != nil || recovered != receipt {
		t.Fatalf("exact recovery = %+v/%v, want %+v", recovered, err, receipt)
	}
	kms.SetFailures(nil, nil)
	stored := repository.byID[receipt.IntentID]
	if stored.Status != IntentPending || stored.ClaimOwner != "" || len(stored.Envelope.Ciphertext) == 0 {
		t.Fatalf("receipt recovery mutated/opened intent = %+v", stored)
	}

	wrongIdentity := request
	wrongIdentity.AccountID = "10381"
	if _, err := vault.Recover(context.Background(), wrongIdentity); !errors.Is(err, ErrIntentUnavailable) {
		t.Fatalf("wrong identity recovery error = %v", err)
	}
	missing := request
	missing.IdempotencyKey = "event-missing"
	if _, err := vault.Recover(context.Background(), missing); !errors.Is(err, ErrIntentNotFound) {
		t.Fatalf("missing recovery error = %v", err)
	}

	opened, err := vault.ClaimAndOpen(context.Background(), receipt.IntentID, "10380", 7, "workflow-recover")
	if err != nil {
		t.Fatal(err)
	}
	opened.Destroy()
	if _, err := vault.Recover(context.Background(), request); !errors.Is(err, ErrIntentUnavailable) {
		t.Fatalf("claimed recovery error = %v", err)
	}
	if err := vault.Complete(context.Background(), receipt.IntentID, "workflow-recover"); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.Recover(context.Background(), request); !errors.Is(err, ErrIntentUnavailable) {
		t.Fatalf("consumed recovery error = %v", err)
	}

	vault.random = bytes.NewReader(bytes.Repeat([]byte{0x64}, 2048))
	second, err := vault.Create(context.Background(), CreateRequest{
		IdempotencyKey: "event-expired-recover", AccountID: "10380", DesiredGeneration: 8,
		Input: &worker.OnboardingInput{
			Source: worker.OnboardingAPIKey, AuthType: worker.AuthTypeAPIKey, Secret: []byte("sk-ant-recovery-expiry"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clock = second.ExpiresAt
	if _, err := vault.Recover(context.Background(), RecoverRequest{
		IdempotencyKey: "event-expired-recover", AccountID: "10380", DesiredGeneration: 8,
		Source: worker.OnboardingAPIKey, AuthType: worker.AuthTypeAPIKey,
	}); !errors.Is(err, ErrIntentExpired) {
		t.Fatalf("expired recovery error = %v", err)
	}
	if _, err := vault.Recover(context.Background(), RecoverRequest{
		IdempotencyKey: "event-expired-recover", AccountID: "10381", DesiredGeneration: 8,
		Source: worker.OnboardingAPIKey, AuthType: worker.AuthTypeAPIKey,
	}); !errors.Is(err, ErrIntentUnavailable) {
		t.Fatalf("expired wrong-identity recovery error = %v", err)
	}
}

func TestVaultAcceptsMaximumCredentialImportWithInternalFraming(t *testing.T) {
	vault := testVault(t, NewMemoryRepository(), time.Unix(2_000_000_000, 0).UTC())
	input := &worker.OnboardingInput{
		Source: worker.OnboardingCredentialImport, AuthType: worker.AuthTypeAPIKey,
		Secret: bytes.Repeat([]byte{'a'}, 1<<20),
	}
	receipt, err := vault.Create(context.Background(), CreateRequest{
		IdempotencyKey: "event-max-import", AccountID: "10380", DesiredGeneration: 7, Input: input,
	})
	if err != nil || receipt.IntentID == "" || input.Secret != nil {
		t.Fatalf("maximum credential import receipt/input/error = %+v/%+v/%v", receipt, input, err)
	}
}

func TestVaultClaimFencesExpiryAndGeneration(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	clock := now
	repository := NewMemoryRepository()
	cryptoService := testCrypto(t)
	vault, err := NewVault(VaultConfig{
		Crypto: cryptoService, Repository: repository, Random: bytes.NewReader(bytes.Repeat([]byte{0x71}, 256)),
		Now: func() time.Time { return clock }, IntentTTL: 30 * time.Minute, ClaimTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := vault.Create(context.Background(), CreateRequest{
		IdempotencyKey: "event-expiry", AccountID: "10380", DesiredGeneration: 7,
		Input: &worker.OnboardingInput{Source: worker.OnboardingSetupToken, AuthType: worker.AuthTypeSetupToken, Secret: []byte("setup-token")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vault.ClaimAndOpen(context.Background(), receipt.IntentID, "10380", 8, "job-10380"); !errors.Is(err, ErrIntentUnavailable) {
		t.Fatalf("generation mismatch error = %v", err)
	}
	clock = now.Add(31 * time.Minute)
	if _, err := vault.ClaimAndOpen(context.Background(), receipt.IntentID, "10380", 7, "job-10380"); !errors.Is(err, ErrIntentUnavailable) {
		t.Fatalf("expired intent error = %v", err)
	}
}

func testVault(t *testing.T, repository Repository, now time.Time) *Vault {
	t.Helper()
	vault, err := NewVault(VaultConfig{
		Crypto: testCrypto(t), Repository: repository, Random: bytes.NewReader(bytes.Repeat([]byte{0x61}, 2048)),
		Now: func() time.Time { return now }, IntentTTL: 30 * time.Minute, ClaimTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return vault
}

func testCrypto(t *testing.T) *credential.Service {
	t.Helper()
	kms, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x42}, 32), "kms-test", "v1")
	if err != nil {
		t.Fatal(err)
	}
	service, err := credential.NewService(kms)
	if err != nil {
		t.Fatal(err)
	}
	return service
}
