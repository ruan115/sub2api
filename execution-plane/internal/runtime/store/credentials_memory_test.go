package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

func TestCredentialVaultRotationLeaseAndReplayAudit(t *testing.T) {
	vault, repository, kms, now := credentialTestRuntime(t)
	ctx := context.Background()
	firstPlaintext := []byte(`{"access_token":"first-at","refresh_token":"first-rt"}`)
	first, err := vault.Rotate(ctx, "account-1", "oauth", "oauth:***st-rt", firstPlaintext)
	if err != nil {
		t.Fatalf("rotate first credential: %v", err)
	}
	if first.VersionNumber != 1 || bytes.Contains(first.Envelope.Ciphertext, firstPlaintext) {
		t.Fatalf("unexpected first credential version: %+v", first)
	}

	grant, err := vault.IssueLease(ctx, "account-1", "slot-1", 1)
	if err != nil {
		t.Fatalf("issue credential lease: %v", err)
	}
	if grant.Token == "" || grant.VersionID != first.ID || !grant.ExpiresAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("unexpected lease grant: %+v", grant)
	}
	grantJSON, err := json.Marshal(grant)
	if err != nil || bytes.Contains(grantJSON, []byte(grant.Token)) || bytes.Contains([]byte(fmt.Sprintf("%+v", grant)), []byte(grant.Token)) {
		t.Fatalf("lease grant serialization exposed token: json=%s err=%v", grantJSON, err)
	}
	material, err := vault.RedeemLease(ctx, grant.Token, "account-1", "slot-1", 1)
	if err != nil {
		t.Fatalf("redeem credential lease: %v", err)
	}
	if !bytes.Equal(material.Plaintext, firstPlaintext) || material.VersionID != first.ID {
		t.Fatalf("unexpected leased credential: version=%s plaintext=%q", material.VersionID, material.Plaintext)
	}
	materialJSON, err := json.Marshal(material)
	if err != nil || bytes.Contains(materialJSON, firstPlaintext) || bytes.Contains([]byte(fmt.Sprintf("%+v", material)), firstPlaintext) {
		t.Fatalf("leased credential serialization exposed plaintext: json=%s err=%v", materialJSON, err)
	}
	material.Destroy()
	if material.Plaintext != nil {
		t.Fatal("Destroy did not release plaintext credential buffer")
	}
	if _, err := vault.RedeemLease(ctx, grant.Token, "account-1", "slot-1", 1); !errors.Is(err, credential.ErrCredentialLeaseRejected) {
		t.Fatalf("replayed lease error = %v, want ErrCredentialLeaseRejected", err)
	}
	events, err := repository.ListCredentialSecurityEvents(ctx, "account-1", 10)
	if err != nil {
		t.Fatalf("list credential security events: %v", err)
	}
	if len(events) != 1 || events[0].ReasonCode != "replayed" || events[0].LeaseID != grant.LeaseID {
		t.Fatalf("unexpected replay security events: %+v", events)
	}

	oldGrant, err := vault.IssueLease(ctx, "account-1", "slot-1", 1)
	if err != nil {
		t.Fatalf("issue pre-rotation lease: %v", err)
	}
	secondPlaintext := []byte(`{"access_token":"second-at","refresh_token":"second-rt"}`)
	second, err := vault.Rotate(ctx, "account-1", "oauth", "oauth:***nd-rt", secondPlaintext)
	if err != nil {
		t.Fatalf("rotate second credential: %v", err)
	}
	if second.VersionNumber != 2 {
		t.Fatalf("second version number = %d, want 2", second.VersionNumber)
	}
	active, err := repository.GetActiveCredentialVersion(ctx, "account-1")
	if err != nil || active.ID != second.ID {
		t.Fatalf("active version = %+v, err=%v", active, err)
	}
	if _, err := vault.RedeemLease(ctx, oldGrant.Token, "account-1", "slot-1", 1); !errors.Is(err, credential.ErrCredentialLeaseRejected) {
		t.Fatalf("superseded lease error = %v, want rejection", err)
	}
	events, _ = repository.ListCredentialSecurityEvents(ctx, "account-1", 10)
	if len(events) != 2 || !containsCredentialSecurityReason(events, "revoked") {
		t.Fatalf("unexpected rotation security events: %+v", events)
	}

	kms.SetFailures(nil, errors.New("KMS decrypt unavailable"))
	failingGrant, err := vault.IssueLease(ctx, "account-1", "slot-1", 1)
	if err != nil {
		t.Fatalf("issue KMS failure lease: %v", err)
	}
	if _, err := vault.RedeemLease(ctx, failingGrant.Token, "account-1", "slot-1", 1); !errors.Is(err, credential.ErrKMSUnavailable) {
		t.Fatalf("KMS failure redemption error = %v", err)
	}
	kms.SetFailures(nil, nil)
	if _, err := vault.RedeemLease(ctx, failingGrant.Token, "account-1", "slot-1", 1); !errors.Is(err, credential.ErrCredentialLeaseRejected) {
		t.Fatalf("KMS failure lease replay error = %v, want rejection", err)
	}
}

func TestCredentialVaultRejectsUnmaskedHint(t *testing.T) {
	vault, _, _, _ := credentialTestRuntime(t)
	if _, err := vault.Rotate(context.Background(), "account-1", "oauth", "raw-token-value", []byte("credential-secret")); err == nil {
		t.Fatal("unmasked credential hint was accepted")
	}
}

func TestCredentialLeaseConcurrentRedemptionSucceedsOnce(t *testing.T) {
	vault, repository, _, _ := credentialTestRuntime(t)
	ctx := context.Background()
	if _, err := vault.Rotate(ctx, "account-1", "oauth", "", []byte("credential-secret")); err != nil {
		t.Fatal(err)
	}
	grant, err := vault.IssueLease(ctx, "account-1", "slot-1", 1)
	if err != nil {
		t.Fatal(err)
	}

	const contenders = 32
	var successes atomic.Int32
	var rejections atomic.Int32
	var wait sync.WaitGroup
	wait.Add(contenders)
	for range contenders {
		go func() {
			defer wait.Done()
			material, err := vault.RedeemLease(ctx, grant.Token, "account-1", "slot-1", 1)
			switch {
			case err == nil:
				successes.Add(1)
				material.Destroy()
			case errors.Is(err, credential.ErrCredentialLeaseRejected):
				rejections.Add(1)
			default:
				t.Errorf("unexpected redemption error: %v", err)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 || rejections.Load() != contenders-1 {
		t.Fatalf("redemptions: success=%d rejected=%d", successes.Load(), rejections.Load())
	}
	events, err := repository.ListCredentialSecurityEvents(ctx, "account-1", contenders)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != contenders-1 {
		t.Fatalf("replay event count = %d, want %d", len(events), contenders-1)
	}
}

func TestCredentialLeaseRejectsInvalidatedExecutionEpoch(t *testing.T) {
	vault, repository, _, now := credentialTestRuntime(t)
	ctx := context.Background()
	if _, err := vault.Rotate(ctx, "account-1", "oauth", "", []byte("credential-secret")); err != nil {
		t.Fatal(err)
	}
	grant, err := vault.IssueLease(ctx, "account-1", "slot-1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.RevokeExecutionLease(ctx, "slot-1", 1, "host-agent-1", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := vault.RedeemLease(ctx, grant.Token, "account-1", "slot-1", 1); !errors.Is(err, credential.ErrCredentialLeaseRejected) {
		t.Fatalf("invalidated epoch redemption error = %v", err)
	}
	events, _ := repository.ListCredentialSecurityEvents(ctx, "account-1", 10)
	if len(events) != 1 || events[0].ReasonCode != "epoch_or_version_inactive" {
		t.Fatalf("unexpected epoch rejection events: %+v", events)
	}
}

func TestConcurrentCredentialRotationsAreMonotonic(t *testing.T) {
	vault, repository, _, _ := credentialTestRuntime(t)
	ctx := context.Background()
	const rotations = 8
	var wait sync.WaitGroup
	wait.Add(rotations)
	versions := make(chan uint64, rotations)
	for index := range rotations {
		index := index
		go func() {
			defer wait.Done()
			record, err := vault.Rotate(ctx, "account-1", "oauth", "", []byte(fmt.Sprintf("credential-%d", index)))
			if err != nil {
				t.Errorf("rotate %d: %v", index, err)
				return
			}
			versions <- record.VersionNumber
		}()
	}
	wait.Wait()
	close(versions)
	seen := make(map[uint64]bool)
	for version := range versions {
		seen[version] = true
	}
	for expected := uint64(1); expected <= rotations; expected++ {
		if !seen[expected] {
			t.Fatalf("missing committed credential version %d; got %v", expected, seen)
		}
	}
	active, err := repository.GetActiveCredentialVersion(ctx, "account-1")
	if err != nil || active.VersionNumber != rotations {
		t.Fatalf("active version = %+v, err=%v", active, err)
	}
}

func TestIdempotentCredentialRotationConvergesAcrossVaultInstances(t *testing.T) {
	firstVault, repository, kms, now := credentialTestRuntime(t)
	cryptoService, err := credential.NewService(kms)
	if err != nil {
		t.Fatal(err)
	}
	secondVault, err := credential.NewVault(cryptoService, repository, credential.VaultConfig{
		LeaseTTL: 30 * time.Second,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	const contenders = 16
	versions := make(chan credential.VersionRecord, contenders)
	errorsSeen := make(chan error, contenders)
	var wait sync.WaitGroup
	wait.Add(contenders)
	for index := range contenders {
		index := index
		go func() {
			defer wait.Done()
			vault := firstVault
			if index%2 == 1 {
				vault = secondVault
			}
			record, rotateErr := vault.RotateIdempotent(
				context.Background(), "credential-lease-idempotent", "account-1", "oauth", "oauth:***same", []byte("same-credential"),
			)
			if rotateErr != nil {
				errorsSeen <- rotateErr
				return
			}
			versions <- record
		}()
	}
	wait.Wait()
	close(versions)
	close(errorsSeen)
	for rotateErr := range errorsSeen {
		t.Errorf("idempotent rotation: %v", rotateErr)
	}

	var committedID string
	for record := range versions {
		if committedID == "" {
			committedID = record.ID
		}
		if record.ID != committedID || record.VersionNumber != 1 {
			t.Fatalf("idempotent result = %+v, want version %q/1", record, committedID)
		}
	}
	if committedID == "" {
		t.Fatal("no idempotent rotation completed")
	}
	repository.mu.RLock()
	versionCount := len(repository.credentialVersions)
	operationVersion := repository.credentialOperations["credential-lease-idempotent"]
	repository.mu.RUnlock()
	if versionCount != 1 || operationVersion != committedID {
		t.Fatalf("durable operation mapping versions=%d mapped=%q committed=%q", versionCount, operationVersion, committedID)
	}
	if _, err := firstVault.RotateIdempotent(
		context.Background(), "credential-lease-idempotent", "account-1", "oauth", "oauth:***changed", []byte("same-credential"),
	); !errors.Is(err, credential.ErrCredentialOperationConflict) {
		t.Fatalf("changed operation metadata error = %v, want conflict", err)
	}
}

func containsCredentialSecurityReason(events []credential.SecurityEvent, reason string) bool {
	for _, event := range events {
		if event.ReasonCode == reason {
			return true
		}
	}
	return false
}

func credentialTestRuntime(t *testing.T) (*credential.Vault, *MemoryRepository, *credential.FakeKMS, time.Time) {
	t.Helper()
	repository, base := connectedMemoryRepository(t)
	now := base.Add(10 * time.Second)
	if _, err := repository.PutDesiredSlot(context.Background(), desiredSlot("slot-1", "account-1", 1, base)); err != nil {
		t.Fatal(err)
	}
	assignment, err := repository.ReserveAssignment(context.Background(), AssignmentReservation{
		ID: "assignment-1", SlotID: "slot-1", NodeID: "srv74", ExpectedNodeSessionID: "session-1",
		NodeSeenAfter: base.Add(-45 * time.Second), ReservedAt: base.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GrantExecutionLease(context.Background(), ExecutionLease{
		ID: "execution-lease-1", SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: assignment.ExecutionEpoch,
		OwnerID: "host-agent-1", ExpiresAt: now.Add(time.Minute), CreatedAt: base.Add(2 * time.Second), UpdatedAt: base.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	kms, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x71}, 32), "kms-test", "v1")
	if err != nil {
		t.Fatal(err)
	}
	cryptoService, err := credential.NewService(kms)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := credential.NewVault(cryptoService, repository, credential.VaultConfig{
		LeaseTTL: 30 * time.Second,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return vault, repository, kms, now
}
