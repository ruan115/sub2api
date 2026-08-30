package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

func TestSecureOnboardingWorkflowBridgesDurableIntentToExactActivation(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	clock := now
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x41}, 32), "kms-test", "v1")
	cryptoService, _ := credential.NewService(kms)
	intentVault, err := onboarding.NewVault(onboarding.VaultConfig{
		Crypto: cryptoService, Repository: onboarding.NewMemoryRepository(),
		Random: bytes.NewReader(bytes.Repeat([]byte{0x51}, 2048)), Now: func() time.Time { return clock },
		IntentTTL: 30 * time.Minute, ClaimTTL: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := "durable-session-secret"
	receipt, err := intentVault.Create(context.Background(), onboarding.CreateRequest{
		IdempotencyKey: "event-workflow", AccountID: "account-10380", DesiredGeneration: 7,
		Input: &worker.OnboardingInput{Source: worker.OnboardingSessionKey, AuthType: worker.AuthTypeOAuth, Secret: []byte(secret)},
	})
	if err != nil {
		t.Fatal(err)
	}
	rotationRecipient, _ := credential.NewRecipient(rand.Reader)
	defer rotationRecipient.Destroy()
	workerRecipient, _ := credential.NewRecipient(rand.Reader)
	defer workerRecipient.Destroy()
	builder, _ := NewSecureOnboardingCommandBuilder(rotationRecipient, bytes.NewReader(bytes.Repeat([]byte{0x61}, 256)), func() time.Time { return clock })
	authority := &recordingActivationAuthority{}
	workflow, _ := NewSecureOnboardingWorkflow(intentVault, builder, authority, func() time.Time { return clock })
	binding := testSecureOnboardingBinding(now)
	plan := SecureOnboardingPlan{IntentID: receipt.IntentID, DesiredGeneration: 7, Owner: "job-workflow", Binding: binding}
	workerKeyID, workerPublicKey, _ := workerRecipient.PublicKey()
	keyResult := &executionv1.CommandResult{
		CommandId: binding.KeyCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: binding.ImageDigest,
			ActualState: "ready", Healthy: true,
		},
		CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{KeyId: workerKeyID, PublicKey: workerPublicKey},
	}
	response, err := workflow.PrepareActivation(context.Background(), plan, keyResult)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(response.GetSecureActivationCommand().GetEncryptedCredentialBundle(), []byte(secret)) {
		t.Fatal("activation command contains durable intent plaintext")
	}
	if authority.calls != 2 {
		t.Fatalf("activation authority calls = %d, want 2", authority.calls)
	}
	failed := &executionv1.CommandResult{
		CommandId: binding.ActivationCommandID, Succeeded: false, ErrorCode: "secure_activation_failed",
		Slot: &executionv1.SlotObservation{
			SlotId: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: binding.ImageDigest,
			ActualState: "ready", Healthy: false,
		},
	}
	if err := workflow.CompleteActivation(context.Background(), plan, failed); !errors.Is(err, ErrSecureOnboardingWorkflow) {
		t.Fatalf("failed activation completion error = %v", err)
	}
	if _, err := workflow.PrepareActivation(context.Background(), plan, keyResult); err != nil {
		t.Fatalf("same-owner preparation retry failed: %v", err)
	}
	succeeded := &executionv1.CommandResult{
		CommandId: binding.ActivationCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: binding.ImageDigest,
			ActualState: "ready", Healthy: true,
		},
	}
	// Dispatch deadlines gate new commands, not a valid result that arrives
	// slightly late while the intent claim itself is still current.
	clock = binding.Deadline.Add(time.Second)
	if err := workflow.CompleteActivation(context.Background(), plan, succeeded); err != nil {
		t.Fatal(err)
	}
	if err := workflow.CompleteActivation(context.Background(), plan, succeeded); err != nil {
		t.Fatalf("idempotent workflow completion: %v", err)
	}
	if _, err := workflow.PrepareActivation(context.Background(), plan, keyResult); !errors.Is(err, ErrSecureOnboardingWorkflow) {
		t.Fatalf("consumed intent preparation error = %v", err)
	}
}

func TestSecureOnboardingWorkflowRejectsKeyResultBeforeIntentExposure(t *testing.T) {
	intent := &recordingIntentVault{}
	rotationRecipient, _ := credential.NewRecipient(rand.Reader)
	defer rotationRecipient.Destroy()
	now := time.Now().UTC()
	builder, _ := NewSecureOnboardingCommandBuilder(rotationRecipient, rand.Reader, func() time.Time { return now })
	authority := &recordingActivationAuthority{}
	workflow, _ := NewSecureOnboardingWorkflow(intent, builder, authority, func() time.Time { return now })
	plan := SecureOnboardingPlan{
		IntentID: "intent-1", DesiredGeneration: 1, Owner: "job-1",
		Binding: SecureOnboardingBinding{
			KeyCommandID: "key-1", ActivationCommandID: "activate-1", SlotID: "slot-1", AccountID: "account-1",
			ExecutionEpoch: 1, ImageDigest: "sha256:" + strings.Repeat("a", 64), CredentialLeaseID: "lease-1",
			ProxyLeaseID: "proxy-1", Deadline: now.Add(time.Minute),
		},
	}
	_, err := workflow.PrepareActivation(context.Background(), plan, &executionv1.CommandResult{CommandId: "wrong-command"})
	if !errors.Is(err, ErrSecureOnboardingWorkflow) || intent.claims != 0 || authority.calls != 0 {
		t.Fatalf("wrong key result error/claims/authority = %v/%d/%d", err, intent.claims, authority.calls)
	}
}

func TestSecureOnboardingWorkflowRevalidatesAuthorityBeforeIntentClaim(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		intent := &recordingIntentVault{}
		rotationRecipient, _ := credential.NewRecipient(rand.Reader)
		now := time.Unix(2_000_000_000, 0).UTC()
		builder, _ := NewSecureOnboardingCommandBuilder(rotationRecipient, rand.Reader, func() time.Time { return now })
		authority := &recordingActivationAuthority{err: errors.New("proxy lease revoked"), failAt: failAt}
		workflow, _ := NewSecureOnboardingWorkflow(intent, builder, authority, func() time.Time { return now })
		binding := testSecureOnboardingBinding(now)
		workerRecipient, _ := credential.NewRecipient(rand.Reader)
		keyID, publicKey, _ := workerRecipient.PublicKey()
		keyResult := &executionv1.CommandResult{
			CommandId: binding.KeyCommandID, Succeeded: true,
			Slot: &executionv1.SlotObservation{
				SlotId: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: binding.ImageDigest, Healthy: true,
			},
			CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{KeyId: keyID, PublicKey: publicKey},
		}
		plan := SecureOnboardingPlan{IntentID: "intent-1", DesiredGeneration: 1, Owner: "job-1", Binding: binding}
		if _, err := workflow.PrepareActivation(context.Background(), plan, keyResult); !errors.Is(err, ErrSecureOnboardingWorkflow) {
			t.Fatalf("fail-at-%d authority error = %v", failAt, err)
		}
		wantClaims := 0
		if failAt == 2 {
			wantClaims = 1
		}
		if authority.calls != failAt || intent.claims != wantClaims {
			t.Fatalf("fail-at-%d authority calls/intent claims = %d/%d", failAt, authority.calls, intent.claims)
		}
		workerRecipient.Destroy()
		rotationRecipient.Destroy()
	}
}

type recordingIntentVault struct {
	claims int
}

func (v *recordingIntentVault) ClaimAndOpen(context.Context, string, string, uint64, string) (worker.OnboardingInput, error) {
	v.claims++
	return worker.OnboardingInput{Source: worker.OnboardingAPIKey, AuthType: worker.AuthTypeAPIKey, Secret: []byte("sk-ant-test")}, nil
}

func (*recordingIntentVault) Complete(context.Context, string, string) error { return nil }

type recordingActivationAuthority struct {
	calls  int
	err    error
	failAt int
}

func (a *recordingActivationAuthority) ValidateCurrentProxyLease(
	context.Context, string, string, uint64, string, time.Time,
) error {
	a.calls++
	if a.err != nil && (a.failAt == 0 || a.calls >= a.failAt) {
		return a.err
	}
	return nil
}
