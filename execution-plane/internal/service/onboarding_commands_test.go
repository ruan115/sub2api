package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

func TestSecureOnboardingCommandBuilderSealsProcessBoundPackage(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	rotationRecipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer rotationRecipient.Destroy()
	workerRecipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer workerRecipient.Destroy()
	builder, err := NewSecureOnboardingCommandBuilder(rotationRecipient, bytes.NewReader(bytes.Repeat([]byte{0x4a}, 256)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	binding := testSecureOnboardingBinding(now)
	keyCommand, err := builder.CredentialKeyCommand(binding)
	if err != nil {
		t.Fatal(err)
	}
	if command := keyCommand.GetCredentialKeyCommand(); command.GetCommandId() != binding.KeyCommandID || command.GetSlotId() != binding.SlotID ||
		!command.GetDeadline().AsTime().Equal(binding.Deadline) {
		t.Fatalf("credential-key command = %+v", command)
	}
	workerKeyID, workerPublicKey, err := workerRecipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	secret := "session-secret-must-not-leak"
	input := &worker.OnboardingInput{Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth, Secret: []byte(secret)}
	keyResult := &executionv1.CommandResult{
		CommandId: binding.KeyCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: binding.ImageDigest,
			ActualState: "ready", Healthy: true,
		},
		CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{KeyId: workerKeyID, PublicKey: workerPublicKey},
	}
	activationResponse, err := builder.SecureActivationCommand(context.Background(), binding, keyResult, input)
	if err != nil {
		t.Fatal(err)
	}
	if input.Secret != nil || input.Auxiliary != nil {
		t.Fatal("plaintext onboarding input was not consumed")
	}
	command := activationResponse.GetSecureActivationCommand()
	if command == nil || command.GetCommandId() != binding.ActivationCommandID || bytes.Contains(command.GetEncryptedCredentialBundle(), []byte(secret)) {
		t.Fatalf("secure activation command = %+v", command)
	}
	plaintext, err := workerRecipient.Open(context.Background(), command.GetEncryptedCredentialBundle(), credential.TransportContext{
		AccountBinding: provider.RuntimeAccountID(binding.AccountID), SlotID: binding.SlotID,
		ExecutionEpoch: binding.ExecutionEpoch, LeaseID: binding.CredentialLeaseID,
		ProxyLeaseID: binding.ProxyLeaseID, Purpose: "onboarding",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eraseOnboardingCommandBytes(plaintext)
	pkg, err := worker.DecodeActivationPackage(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	defer pkg.Destroy()
	rotationKeyID, rotationPublicKey, _ := rotationRecipient.PublicKey()
	defer eraseOnboardingCommandBytes(rotationPublicKey)
	if string(pkg.Input.Secret) != secret || pkg.Input.Source != worker.OnboardingSessionKey ||
		pkg.RotationRecipientKeyID != rotationKeyID || !bytes.Equal(pkg.RotationRecipientPublicKey, rotationPublicKey) {
		t.Fatalf("decoded activation package = %+v", pkg)
	}
	for _, rendered := range []string{fmt.Sprintf("%v", command), fmt.Sprintf("%#v", command)} {
		if strings.Contains(rendered, secret) {
			t.Fatal("command formatting exposed onboarding material")
		}
	}
	encoded, err := json.Marshal(pkg)
	if err != nil || bytes.Contains(encoded, []byte(secret)) {
		t.Fatalf("activation package JSON leaked secret: %s (%v)", encoded, err)
	}
}

func TestSecureOnboardingCommandBuilderRejectsMismatchedKeyBeforeSealing(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	rotationRecipient, _ := credential.NewRecipient(rand.Reader)
	defer rotationRecipient.Destroy()
	workerRecipient, _ := credential.NewRecipient(rand.Reader)
	defer workerRecipient.Destroy()
	builder, _ := NewSecureOnboardingCommandBuilder(rotationRecipient, bytes.NewReader(bytes.Repeat([]byte{0x2f}, 256)), func() time.Time { return now })
	binding := testSecureOnboardingBinding(now)
	keyID, publicKey, _ := workerRecipient.PublicKey()
	input := &worker.OnboardingInput{Source: worker.OnboardingAPIKey, AuthType: worker.AuthTypeAPIKey, Secret: []byte("sk-ant-redacted")}
	_, err := builder.SecureActivationCommand(context.Background(), binding, &executionv1.CommandResult{
		CommandId: binding.KeyCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: "sha256:" + strings.Repeat("b", 64),
			ActualState: "ready", Healthy: true,
		},
		CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{KeyId: keyID, PublicKey: publicKey},
	}, input)
	if !errors.Is(err, ErrSecureOnboardingCommand) || input.Secret != nil {
		t.Fatalf("mismatch error/input = %v/%+v", err, input)
	}
}

func testSecureOnboardingBinding(now time.Time) SecureOnboardingBinding {
	return SecureOnboardingBinding{
		KeyCommandID: "cmd-key-10380", ActivationCommandID: "cmd-activate-10380",
		SlotID: "slot-10380", AccountID: "account-10380", ExecutionEpoch: 19,
		ImageDigest: "sha256:" + strings.Repeat("a", 64), CredentialLeaseID: "lease-10380",
		ProxyLeaseID: "proxy-10380", Deadline: now.Add(2 * time.Minute),
	}
}
