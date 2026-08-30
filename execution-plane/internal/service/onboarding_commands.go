package service

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"regexp"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxSecureActivationCommandBytes = 2 << 20

var (
	ErrSecureOnboardingCommand = errors.New("secure onboarding command rejected")
	onboardingImageDigest      = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// SecureOnboardingBinding is the non-secret identity shared by credential-key
// discovery and secure activation. The two command ids are intentionally
// distinct so a delayed key result cannot be accepted as activation progress.
type SecureOnboardingBinding struct {
	KeyCommandID        string
	ActivationCommandID string
	SlotID              string
	AccountID           string
	ExecutionEpoch      uint64
	ImageDigest         string
	CredentialLeaseID   string
	ProxyLeaseID        string
	Deadline            time.Time
}

// SecureOnboardingCommandBuilder owns the orchestrator side of the process-key
// boundary. It never returns an activation package in plaintext.
type SecureOnboardingCommandBuilder struct {
	rotationRecipient *credential.Recipient
	random            io.Reader
	now               func() time.Time
}

func NewSecureOnboardingCommandBuilder(
	rotationRecipient *credential.Recipient,
	random io.Reader,
	now func() time.Time,
) (*SecureOnboardingCommandBuilder, error) {
	if rotationRecipient == nil {
		return nil, ErrSecureOnboardingCommand
	}
	if _, _, err := rotationRecipient.PublicKey(); err != nil {
		return nil, ErrSecureOnboardingCommand
	}
	if random == nil {
		random = rand.Reader
	}
	if now == nil {
		now = time.Now
	}
	return &SecureOnboardingCommandBuilder{rotationRecipient: rotationRecipient, random: random, now: now}, nil
}

func (b *SecureOnboardingCommandBuilder) CredentialKeyCommand(binding SecureOnboardingBinding) (*executionv1.NodeControlServiceControlResponse, error) {
	if b == nil || b.validateBinding(binding) != nil {
		return nil, ErrSecureOnboardingCommand
	}
	return &executionv1.NodeControlServiceControlResponse{
		Event: &executionv1.NodeControlServiceControlResponse_CredentialKeyCommand{
			CredentialKeyCommand: &executionv1.CredentialKeyCommand{
				CommandId: binding.KeyCommandID, SlotId: binding.SlotID, AccountId: binding.AccountID,
				ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: binding.ImageDigest,
				Deadline: timestamppb.New(binding.Deadline.UTC()),
			},
		},
	}, nil
}

// SecureActivationCommand consumes and erases input on every return path. The
// caller should pass material obtained from a short-lived, KMS-backed intent
// and must not retain another plaintext copy.
func (b *SecureOnboardingCommandBuilder) SecureActivationCommand(
	ctx context.Context,
	binding SecureOnboardingBinding,
	keyResult *executionv1.CommandResult,
	input *worker.OnboardingInput,
) (*executionv1.NodeControlServiceControlResponse, error) {
	if input != nil {
		defer input.Destroy()
	}
	if b == nil || ctx == nil || ctx.Err() != nil || input == nil || b.validateBinding(binding) != nil ||
		b.validateKeyResult(binding, keyResult) != nil || input.Validate() != nil {
		return nil, ErrSecureOnboardingCommand
	}
	rotationKeyID, rotationPublicKey, err := b.rotationRecipient.PublicKey()
	if err != nil {
		return nil, ErrSecureOnboardingCommand
	}
	defer eraseOnboardingCommandBytes(rotationPublicKey)
	workerKey := append([]byte(nil), keyResult.GetCredentialTransportKey().GetPublicKey()...)
	defer eraseOnboardingCommandBytes(workerKey)
	pkg := worker.ActivationPackage{
		Input: worker.OnboardingInput{
			Source: input.Source, AuthType: input.AuthType,
			Secret: append([]byte(nil), input.Secret...), Auxiliary: append([]byte(nil), input.Auxiliary...),
		},
		RotationRecipientKeyID: rotationKeyID, RotationRecipientPublicKey: append([]byte(nil), rotationPublicKey...),
	}
	defer pkg.Destroy()
	plaintext, err := worker.EncodeActivationPackage(pkg)
	if err != nil {
		return nil, ErrSecureOnboardingCommand
	}
	defer eraseOnboardingCommandBytes(plaintext)
	sealed, err := credential.SealForRecipient(
		ctx, b.random, keyResult.GetCredentialTransportKey().GetKeyId(), workerKey,
		credential.TransportContext{
			AccountBinding: provider.RuntimeAccountID(binding.AccountID), SlotID: binding.SlotID,
			ExecutionEpoch: binding.ExecutionEpoch, LeaseID: binding.CredentialLeaseID,
			ProxyLeaseID: binding.ProxyLeaseID, Purpose: "onboarding",
		},
		plaintext,
	)
	if err != nil || len(sealed) == 0 || len(sealed) > maxSecureActivationCommandBytes {
		eraseOnboardingCommandBytes(sealed)
		return nil, ErrSecureOnboardingCommand
	}
	return &executionv1.NodeControlServiceControlResponse{
		Event: &executionv1.NodeControlServiceControlResponse_SecureActivationCommand{
			SecureActivationCommand: &executionv1.SecureActivationCommand{
				CommandId: binding.ActivationCommandID, SlotId: binding.SlotID, AccountId: binding.AccountID,
				ExecutionEpoch: binding.ExecutionEpoch, ImageDigest: binding.ImageDigest,
				CredentialLeaseId: binding.CredentialLeaseID, ProxyLeaseId: binding.ProxyLeaseID,
				EncryptedCredentialBundle: sealed, Deadline: timestamppb.New(binding.Deadline.UTC()),
			},
		},
	}, nil
}

func (b *SecureOnboardingCommandBuilder) validateBinding(binding SecureOnboardingBinding) error {
	if b.validateBindingIdentity(binding) != nil || !binding.Deadline.After(b.now().UTC()) {
		return ErrSecureOnboardingCommand
	}
	return nil
}

// validateBindingIdentity checks immutable command identity. Completion uses
// this form because a valid result may arrive at or just after its dispatch
// deadline; claim/intent expiry remains the authoritative completion fence.
func (b *SecureOnboardingCommandBuilder) validateBindingIdentity(binding SecureOnboardingBinding) error {
	if b == nil || b.rotationRecipient == nil || b.random == nil || b.now == nil ||
		credential.ValidateTransportID(binding.KeyCommandID) != nil ||
		credential.ValidateTransportID(binding.ActivationCommandID) != nil ||
		binding.KeyCommandID == binding.ActivationCommandID ||
		credential.ValidateTransportID(binding.SlotID) != nil ||
		credential.ValidateTransportID(binding.AccountID) != nil ||
		credential.ValidateTransportID(binding.CredentialLeaseID) != nil ||
		credential.ValidateTransportID(binding.ProxyLeaseID) != nil ||
		binding.ExecutionEpoch == 0 || !onboardingImageDigest.MatchString(binding.ImageDigest) || binding.Deadline.IsZero() {
		return ErrSecureOnboardingCommand
	}
	return nil
}

func (b *SecureOnboardingCommandBuilder) validateKeyResult(binding SecureOnboardingBinding, result *executionv1.CommandResult) error {
	if result == nil || result.GetCommandId() != binding.KeyCommandID || !result.GetSucceeded() ||
		result.GetErrorCode() != "" || result.GetSlot() == nil || result.GetSlot().GetSlotId() != binding.SlotID ||
		result.GetSlot().GetExecutionEpoch() != binding.ExecutionEpoch || result.GetSlot().GetImageDigest() != binding.ImageDigest ||
		!result.GetSlot().GetHealthy() || result.GetCredentialTransportKey() == nil ||
		credential.ValidateRecipientKey(result.GetCredentialTransportKey().GetKeyId(), result.GetCredentialTransportKey().GetPublicKey()) != nil {
		return ErrSecureOnboardingCommand
	}
	return nil
}

func eraseOnboardingCommandBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
