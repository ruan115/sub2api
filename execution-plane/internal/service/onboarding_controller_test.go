package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/protobuf/proto"
)

func TestSecureOnboardingControllerResumesDurableTwoCommandWorkflow(t *testing.T) {
	clock := time.Unix(2_000_000_000, 0).UTC()
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x31}, 32), "kms-test", "v1")
	cryptoService, _ := credential.NewService(kms)
	intentVault, err := onboarding.NewVault(onboarding.VaultConfig{
		Crypto: cryptoService, Repository: onboarding.NewMemoryRepository(), Random: rand.Reader,
		Now: func() time.Time { return clock }, IntentTTL: 30 * time.Minute, ClaimTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := "controller-session-secret"
	receipt, err := intentVault.Create(context.Background(), onboarding.CreateRequest{
		IdempotencyKey: "event-controller", AccountID: "account-controller", DesiredGeneration: 7,
		Input: &worker.OnboardingInput{Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth, Secret: []byte(secret)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rotationRecipient, _ := credential.NewRecipient(rand.Reader)
	defer rotationRecipient.Destroy()
	builder, _ := NewSecureOnboardingCommandBuilder(rotationRecipient, rand.Reader, func() time.Time { return clock })
	workflow, _ := NewSecureOnboardingWorkflow(intentVault, builder, &recordingActivationAuthority{}, func() time.Time { return clock })
	repository := onboarding.NewMemoryProvisioningRepository()
	record := observerProvisioning(clock)
	record.ID = "workflow-controller"
	record.IdempotencyKey = "event-controller"
	record.IntentID = receipt.IntentID
	record.Owner = "workflow-controller"
	record.AccountID = receipt.AccountID
	record.DesiredGeneration = receipt.DesiredGeneration
	stored, _, err := repository.CreateProvisioning(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	stored.Destroy()
	dispatcher := &recordingProvisioningDispatcher{}
	controller, _ := NewSecureOnboardingController(repository, workflow, dispatcher, SecureOnboardingControllerConfig{
		RetryDelay: 5 * time.Second, Now: func() time.Time { return clock },
	})
	observer, _ := NewProvisioningCommandObserver(repository, func() time.Time { return clock })

	status, err := controller.Advance(context.Background(), record.ID)
	if err != nil || status != onboarding.ProvisioningKeyDispatched || len(dispatcher.responses) != 1 ||
		dispatcher.responses[0].GetCredentialKeyCommand() == nil {
		t.Fatalf("key advance = %q/%v/%+v", status, err, dispatcher.responses)
	}
	if status, err = controller.Advance(context.Background(), record.ID); err != nil || status != onboarding.ProvisioningKeyDispatched || len(dispatcher.responses) != 1 {
		t.Fatalf("early key retry = %q/%v/%d", status, err, len(dispatcher.responses))
	}
	clock = clock.Add(6 * time.Second)
	if status, err = controller.Advance(context.Background(), record.ID); err != nil || status != onboarding.ProvisioningKeyDispatched || len(dispatcher.responses) != 2 {
		t.Fatalf("due key retry = %q/%v/%d", status, err, len(dispatcher.responses))
	}
	workerRecipient, _ := credential.NewRecipient(rand.Reader)
	defer workerRecipient.Destroy()
	keyID, publicKey, _ := workerRecipient.PublicKey()
	if err := observer.ObserveCommandResult(context.Background(), record.NodeID, &executionv1.CommandResult{
		CommandId: record.KeyCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: record.SlotID, ExecutionEpoch: record.ExecutionEpoch, ImageDigest: record.ImageDigest, Healthy: true,
		},
		CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{KeyId: keyID, PublicKey: publicKey},
	}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	status, err = controller.Advance(context.Background(), record.ID)
	if err != nil || status != onboarding.ProvisioningActivationDispatched || len(dispatcher.responses) != 3 {
		t.Fatalf("activation advance = %q/%v/%d", status, err, len(dispatcher.responses))
	}
	bundle := dispatcher.responses[2].GetSecureActivationCommand().GetEncryptedCredentialBundle()
	if len(bundle) == 0 || bytes.Contains(bundle, []byte(secret)) {
		t.Fatal("activation dispatch did not preserve the ciphertext-only boundary")
	}
	if status, err = controller.Advance(context.Background(), record.ID); err != nil || status != onboarding.ProvisioningActivationDispatched || len(dispatcher.responses) != 3 {
		t.Fatalf("early activation retry = %q/%v/%d", status, err, len(dispatcher.responses))
	}
	clock = clock.Add(6 * time.Second)
	if status, err = controller.Advance(context.Background(), record.ID); err != nil || status != onboarding.ProvisioningActivationDispatched || len(dispatcher.responses) != 4 {
		t.Fatalf("due activation retry = %q/%v/%d", status, err, len(dispatcher.responses))
	}
	if err := observer.ObserveCommandResult(context.Background(), record.NodeID, &executionv1.CommandResult{
		CommandId: record.ActivationCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: record.SlotID, ExecutionEpoch: record.ExecutionEpoch, ImageDigest: record.ImageDigest, Healthy: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(time.Second)
	status, err = controller.Advance(context.Background(), record.ID)
	if err != nil || status != onboarding.ProvisioningCompleted {
		t.Fatalf("completion advance = %q/%v", status, err)
	}
	completed, err := repository.GetProvisioning(context.Background(), record.ID)
	if err != nil || completed.Status != onboarding.ProvisioningCompleted {
		t.Fatalf("completed record = %+v/%v", completed, err)
	}
	completed.Destroy()
	if _, err := intentVault.ClaimAndOpen(context.Background(), receipt.IntentID, receipt.AccountID, receipt.DesiredGeneration, record.Owner); !errors.Is(err, onboarding.ErrIntentUnavailable) {
		t.Fatalf("consumed intent claim error = %v", err)
	}

	expired := record
	expired.ID = "workflow-expired"
	expired.IdempotencyKey = "event-expired"
	expired.IntentID = "intent-expired"
	expired.Owner = "workflow-expired"
	expired.KeyCommandID = "cmd-key-expired"
	expired.ActivationCommandID = "cmd-activate-expired"
	expired.CreatedAt = clock
	expired.UpdatedAt = clock
	expired.CommandDeadline = clock.Add(time.Second)
	stored, _, err = repository.CreateProvisioning(context.Background(), expired)
	if err != nil {
		t.Fatal(err)
	}
	stored.Destroy()
	clock = clock.Add(2 * time.Second)
	if status, err = controller.Advance(context.Background(), expired.ID); !errors.Is(err, ErrProvisioningAdvance) || status != onboarding.ProvisioningFailed {
		t.Fatalf("expired workflow advance = %q/%v", status, err)
	}

	activationExpired := record
	activationExpired.ID = "workflow-activation-expired"
	activationExpired.IdempotencyKey = "event-activation-expired"
	activationExpired.IntentID = "intent-activation-expired"
	activationExpired.Owner = activationExpired.ID
	activationExpired.KeyCommandID = "cmd-key-activation-expired"
	activationExpired.ActivationCommandID = "cmd-activate-activation-expired"
	activationExpired.CreatedAt = clock
	activationExpired.UpdatedAt = clock
	activationExpired.CommandDeadline = clock.Add(time.Second)
	stored, _, err = repository.CreateProvisioning(context.Background(), activationExpired)
	if err != nil {
		t.Fatal(err)
	}
	stored.Destroy()
	keyReady, err := repository.ObserveProvisioningCommand(context.Background(), onboarding.ProvisioningCommandObservation{
		CommandID: activationExpired.KeyCommandID, NodeID: activationExpired.NodeID, SlotID: activationExpired.SlotID,
		ExecutionEpoch: activationExpired.ExecutionEpoch, ImageDigest: activationExpired.ImageDigest,
		Healthy: true, Succeeded: true, KeyID: keyID, KeyPublicKey: publicKey, ReceivedAt: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	keyReady.Destroy()
	if err := repository.MarkProvisioningActivationDispatched(context.Background(), activationExpired.ID, clock); err != nil {
		t.Fatal(err)
	}
	activationSucceeded, err := repository.ObserveProvisioningCommand(context.Background(), onboarding.ProvisioningCommandObservation{
		CommandID: activationExpired.ActivationCommandID, NodeID: activationExpired.NodeID, SlotID: activationExpired.SlotID,
		ExecutionEpoch: activationExpired.ExecutionEpoch, ImageDigest: activationExpired.ImageDigest,
		Healthy: true, Succeeded: true, ReceivedAt: clock,
	})
	if err != nil || activationSucceeded.Status != onboarding.ProvisioningActivationSucceeded {
		t.Fatalf("activation-succeeded workflow = %+v/%v", activationSucceeded, err)
	}
	activationSucceeded.Destroy()
	clock = clock.Add(2 * time.Second)
	if status, err = controller.Advance(context.Background(), activationExpired.ID); !errors.Is(err, ErrProvisioningAdvance) || status != onboarding.ProvisioningFailed {
		t.Fatalf("expired activation-succeeded advance = %q/%v", status, err)
	}
	failedActivation, err := repository.GetProvisioning(context.Background(), activationExpired.ID)
	if err != nil || failedActivation.ErrorCode != "workflow_deadline_exceeded" ||
		failedActivation.LastCommandID != activationExpired.ActivationCommandID {
		t.Fatalf("expired activation-succeeded record = %+v/%v", failedActivation, err)
	}
	failedActivation.Destroy()
}

type recordingProvisioningDispatcher struct {
	responses []*executionv1.NodeControlServiceControlResponse
}

func (d *recordingProvisioningDispatcher) Dispatch(_ context.Context, _ string, response *executionv1.NodeControlServiceControlResponse) error {
	cloned, ok := proto.Clone(response).(*executionv1.NodeControlServiceControlResponse)
	if !ok {
		return errors.New("invalid response")
	}
	d.responses = append(d.responses, cloned)
	return nil
}
