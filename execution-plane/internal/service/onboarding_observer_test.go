package service

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

func TestProvisioningCommandObserverDurablyRecordsBothSecureCommands(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := onboarding.NewMemoryProvisioningRepository()
	candidate := observerProvisioning(now)
	stored, _, err := repository.CreateProvisioning(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	stored.Destroy()
	observer, err := NewProvisioningCommandObserver(repository, func() time.Time { return now.Add(time.Second) })
	if err != nil {
		t.Fatal(err)
	}
	recipient, _ := credential.NewRecipient(rand.Reader)
	defer recipient.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	keyResult := &executionv1.CommandResult{
		CommandId: candidate.KeyCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: candidate.SlotID, ExecutionEpoch: candidate.ExecutionEpoch,
			ImageDigest: candidate.ImageDigest, Healthy: true,
		},
		CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{KeyId: keyID, PublicKey: publicKey},
	}
	if err := observer.ObserveCommandResult(context.Background(), candidate.NodeID, keyResult); err != nil {
		t.Fatal(err)
	}
	if err := repository.MarkProvisioningActivationDispatched(context.Background(), candidate.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	activationResult := &executionv1.CommandResult{
		CommandId: candidate.ActivationCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: candidate.SlotID, ExecutionEpoch: candidate.ExecutionEpoch,
			ImageDigest: candidate.ImageDigest, Healthy: true,
		},
	}
	if err := observer.ObserveCommandResult(context.Background(), candidate.NodeID, activationResult); err != nil {
		t.Fatal(err)
	}
	record, err := repository.GetProvisioning(context.Background(), candidate.ID)
	if err != nil || record.Status != onboarding.ProvisioningActivationSucceeded || record.LastCommandID != candidate.ActivationCommandID {
		t.Fatalf("observed provisioning = %+v/%v", record, err)
	}
	record.Destroy()
}

func observerProvisioning(now time.Time) onboarding.Provisioning {
	return onboarding.Provisioning{
		ID: "workflow-observer", IdempotencyKey: "event-observer", IntentID: "intent-observer", Owner: "workflow-observer",
		AccountID: "account-observer", DesiredGeneration: 3, NodeID: "srv74", SlotID: "slot-observer",
		ExecutionEpoch: 9, ImageDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CredentialLeaseID: "lease-observer", ProxyLeaseID: "proxy-observer", KeyCommandID: "cmd-key-observer",
		ActivationCommandID: "cmd-activate-observer", CommandDeadline: now.Add(2 * time.Minute),
		Status: onboarding.ProvisioningPendingKey, CreatedAt: now, UpdatedAt: now,
	}
}
