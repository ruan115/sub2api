package onboarding

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

func TestProvisioningRepositoryFencesAndReplaysTwoCommandWorkflow(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := NewMemoryProvisioningRepository()
	candidate := testProvisioning(now)
	created, wasCreated, err := repository.CreateProvisioning(context.Background(), candidate)
	if err != nil || !wasCreated || created.ID != candidate.ID {
		t.Fatalf("create provisioning = %+v/%t/%v", created, wasCreated, err)
	}
	created.Destroy()
	replayed, wasCreated, err := repository.CreateProvisioning(context.Background(), candidate)
	if err != nil || wasCreated || replayed.ID != candidate.ID {
		t.Fatalf("replay provisioning = %+v/%t/%v", replayed, wasCreated, err)
	}
	replayed.Destroy()

	recipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	key := ProvisioningCommandObservation{
		CommandID: candidate.KeyCommandID, NodeID: candidate.NodeID, SlotID: candidate.SlotID,
		ExecutionEpoch: candidate.ExecutionEpoch, ImageDigest: candidate.ImageDigest, Healthy: true, Succeeded: true,
		KeyID: keyID, KeyPublicKey: publicKey, ReceivedAt: now.Add(time.Second),
	}
	keyReady, err := repository.ObserveProvisioningCommand(context.Background(), key)
	if err != nil || keyReady.Status != ProvisioningKeyReady || string(keyReady.KeyPublicKey) != string(key.KeyPublicKey) {
		t.Fatalf("key observation = %+v/%v", keyReady, err)
	}
	keyReady.Destroy()
	duplicate, err := repository.ObserveProvisioningCommand(context.Background(), key)
	if err != nil || duplicate.Status != ProvisioningKeyReady {
		t.Fatalf("duplicate key observation = %+v/%v", duplicate, err)
	}
	duplicate.Destroy()
	tampered := key.Clone()
	tampered.NodeID = "srv-other"
	if _, err := repository.ObserveProvisioningCommand(context.Background(), tampered); !errors.Is(err, ErrProvisioningRejected) {
		t.Fatalf("wrong-node key observation error = %v", err)
	}
	if err := repository.MarkProvisioningActivationDispatched(context.Background(), candidate.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	activation := ProvisioningCommandObservation{
		CommandID: candidate.ActivationCommandID, NodeID: candidate.NodeID, SlotID: candidate.SlotID,
		ExecutionEpoch: candidate.ExecutionEpoch, ImageDigest: candidate.ImageDigest,
		Healthy: true, Succeeded: true, ReceivedAt: now.Add(3 * time.Second),
	}
	succeeded, err := repository.ObserveProvisioningCommand(context.Background(), activation)
	if err != nil || succeeded.Status != ProvisioningActivationSucceeded {
		t.Fatalf("activation observation = %+v/%v", succeeded, err)
	}
	succeeded.Destroy()
	if err := repository.CompleteProvisioning(context.Background(), candidate.ID, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteProvisioning(context.Background(), candidate.ID, now.Add(5*time.Second)); err != nil {
		t.Fatalf("idempotent completion = %v", err)
	}
	completed, err := repository.GetProvisioning(context.Background(), candidate.ID)
	if err != nil || completed.Status != ProvisioningCompleted || completed.ErrorCode != "" {
		t.Fatalf("completed workflow = %+v/%v", completed, err)
	}
	completed.Destroy()
	duplicateCompletion, err := repository.ObserveProvisioningCommand(context.Background(), activation)
	if err != nil || duplicateCompletion.Status != ProvisioningCompleted {
		t.Fatalf("post-completion activation replay = %+v/%v", duplicateCompletion, err)
	}
	duplicateCompletion.Destroy()
}

func TestProvisioningRepositoryAllowsAtMostOneWorkflowPerIntent(t *testing.T) {
	repository := NewMemoryProvisioningRepository()
	first := testProvisioning(time.Unix(2_000_000_000, 0).UTC())
	if _, created, err := repository.CreateProvisioning(context.Background(), first); err != nil || !created {
		t.Fatalf("create first workflow = %t/%v", created, err)
	}
	second := first
	second.ID = "workflow-other"
	second.IdempotencyKey = "event-other"
	second.Owner = second.ID
	second.CredentialLeaseID = "lease-other"
	second.ProxyLeaseID = "proxy-other"
	second.KeyCommandID = "cmd-key-other"
	second.ActivationCommandID = "cmd-activation-other"
	if _, _, err := repository.CreateProvisioning(context.Background(), second); !errors.Is(err, ErrProvisioningRejected) {
		t.Fatalf("second workflow for same intent error = %v", err)
	}
}

func TestProvisioningFailureIsSecretFreeAndIdempotent(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := NewMemoryProvisioningRepository()
	candidate := testProvisioning(now)
	if _, _, err := repository.CreateProvisioning(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	failure := ProvisioningCommandObservation{
		CommandID: candidate.KeyCommandID, NodeID: candidate.NodeID, SlotID: candidate.SlotID,
		ExecutionEpoch: candidate.ExecutionEpoch, ImageDigest: candidate.ImageDigest,
		Succeeded: false, ErrorCode: "worker_key_failed", ReceivedAt: now.Add(time.Second),
	}
	failed, err := repository.ObserveProvisioningCommand(context.Background(), failure)
	if err != nil || failed.Status != ProvisioningFailed || failed.ErrorCode != "worker_key_failed" {
		t.Fatalf("failure observation = %+v/%v", failed, err)
	}
	failed.Destroy()
	replayed, err := repository.ObserveProvisioningCommand(context.Background(), failure)
	if err != nil || replayed.Status != ProvisioningFailed {
		t.Fatalf("failure replay = %+v/%v", replayed, err)
	}
	replayed.Destroy()
	failure.ErrorCode = "session_key_leaked"
	if _, err := repository.ObserveProvisioningCommand(context.Background(), failure); !errors.Is(err, ErrProvisioningRejected) {
		t.Fatalf("sensitive/different failure error = %v", err)
	}
}

func TestProvisioningCanFailAfterActivationSucceeded(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := NewMemoryProvisioningRepository()
	candidate := testProvisioning(now)
	stored, _, err := repository.CreateProvisioning(context.Background(), candidate)
	if err != nil {
		t.Fatal(err)
	}
	stored.Destroy()
	recipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	keyReady, err := repository.ObserveProvisioningCommand(context.Background(), ProvisioningCommandObservation{
		CommandID: candidate.KeyCommandID, NodeID: candidate.NodeID, SlotID: candidate.SlotID,
		ExecutionEpoch: candidate.ExecutionEpoch, ImageDigest: candidate.ImageDigest, Healthy: true, Succeeded: true,
		KeyID: keyID, KeyPublicKey: publicKey, ReceivedAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	keyReady.Destroy()
	if err := repository.MarkProvisioningActivationDispatched(context.Background(), candidate.ID, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	succeeded, err := repository.ObserveProvisioningCommand(context.Background(), ProvisioningCommandObservation{
		CommandID: candidate.ActivationCommandID, NodeID: candidate.NodeID, SlotID: candidate.SlotID,
		ExecutionEpoch: candidate.ExecutionEpoch, ImageDigest: candidate.ImageDigest,
		Healthy: true, Succeeded: true, ReceivedAt: now.Add(3 * time.Second),
	})
	if err != nil || succeeded.Status != ProvisioningActivationSucceeded {
		t.Fatalf("activation observation = %+v/%v", succeeded, err)
	}
	succeeded.Destroy()
	if err := repository.FailProvisioning(context.Background(), candidate.ID, "workflow_deadline_exceeded", now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	failed, err := repository.GetProvisioning(context.Background(), candidate.ID)
	if err != nil || failed.Status != ProvisioningFailed || failed.ErrorCode != "workflow_deadline_exceeded" ||
		failed.LastCommandID != candidate.ActivationCommandID {
		t.Fatalf("failed provisioning = %+v/%v", failed, err)
	}
	failed.Destroy()
}

func testProvisioning(now time.Time) Provisioning {
	return Provisioning{
		ID: "workflow-10380", IdempotencyKey: "event-10380", IntentID: "intent-10380", Owner: "workflow-10380",
		AccountID: "account-10380", DesiredGeneration: 7, NodeID: "srv74", SlotID: "slot-10380",
		ExecutionEpoch: 19, ImageDigest: "sha256:" + strings.Repeat("a", 64), CredentialLeaseID: "lease-10380",
		ProxyLeaseID: "proxy-10380", KeyCommandID: "cmd-key-10380", ActivationCommandID: "cmd-activate-10380",
		CommandDeadline: now.Add(2 * time.Minute), Status: ProvisioningPendingKey, CreatedAt: now, UpdatedAt: now,
	}
}
