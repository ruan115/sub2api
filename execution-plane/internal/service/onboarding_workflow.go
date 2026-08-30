package service

import (
	"context"
	"errors"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

var ErrSecureOnboardingWorkflow = errors.New("secure onboarding workflow rejected")

type OnboardingIntentVault interface {
	ClaimAndOpen(ctx context.Context, intentID, accountID string, desiredGeneration uint64, owner string) (worker.OnboardingInput, error)
	Complete(ctx context.Context, intentID, owner string) error
}

type SecureOnboardingWorkflow struct {
	intents   OnboardingIntentVault
	builder   *SecureOnboardingCommandBuilder
	authority ProxyLeaseAuthority
	now       func() time.Time
}

type SecureOnboardingPlan struct {
	IntentID          string
	DesiredGeneration uint64
	Owner             string
	Binding           SecureOnboardingBinding
}

func NewSecureOnboardingWorkflow(
	intents OnboardingIntentVault,
	builder *SecureOnboardingCommandBuilder,
	authority ProxyLeaseAuthority,
	now func() time.Time,
) (*SecureOnboardingWorkflow, error) {
	if intents == nil || builder == nil || authority == nil {
		return nil, ErrSecureOnboardingWorkflow
	}
	if now == nil {
		now = time.Now
	}
	return &SecureOnboardingWorkflow{intents: intents, builder: builder, authority: authority, now: now}, nil
}

func (w *SecureOnboardingWorkflow) CredentialKeyCommand(plan SecureOnboardingPlan) (*executionv1.NodeControlServiceControlResponse, error) {
	if w == nil || w.builder == nil || w.builder.validateBinding(plan.Binding) != nil || plan.IntentID == "" ||
		plan.Owner == "" || plan.DesiredGeneration == 0 {
		return nil, ErrSecureOnboardingWorkflow
	}
	response, err := w.builder.CredentialKeyCommand(plan.Binding)
	if err != nil {
		return nil, ErrSecureOnboardingWorkflow
	}
	return response, nil
}

// PrepareActivation claims/decrypts one durable intent and immediately hands
// ownership of the resulting plaintext to the process-key command builder.
func (w *SecureOnboardingWorkflow) PrepareActivation(
	ctx context.Context,
	plan SecureOnboardingPlan,
	keyResult *executionv1.CommandResult,
) (*executionv1.NodeControlServiceControlResponse, error) {
	if w == nil || w.intents == nil || w.builder == nil || w.authority == nil || w.now == nil ||
		w.builder.validateBinding(plan.Binding) != nil ||
		w.builder.validateKeyResult(plan.Binding, keyResult) != nil || plan.IntentID == "" || plan.Owner == "" || plan.DesiredGeneration == 0 {
		return nil, ErrSecureOnboardingWorkflow
	}
	// The starter's authorization is only a point-in-time creation fence. Check
	// the complete proxy-lease authority again immediately before the intent is
	// claimed/decrypted so a revoked reservation, expired execution lease or
	// replaced assignment cannot expose credential material to stale work.
	if w.validateActivationAuthority(ctx, plan) != nil {
		return nil, ErrSecureOnboardingWorkflow
	}
	input, err := w.intents.ClaimAndOpen(ctx, plan.IntentID, plan.Binding.AccountID, plan.DesiredGeneration, plan.Owner)
	if err != nil {
		return nil, ErrSecureOnboardingWorkflow
	}
	// Bracket the KMS open with live checks. A revocation racing the first check
	// can briefly produce caller-owned plaintext, but it is destroyed here and
	// never sealed or dispatched to a worker.
	if w.validateActivationAuthority(ctx, plan) != nil {
		input.Destroy()
		return nil, ErrSecureOnboardingWorkflow
	}
	response, err := w.builder.SecureActivationCommand(ctx, plan.Binding, keyResult, &input)
	if err != nil {
		return nil, ErrSecureOnboardingWorkflow
	}
	return response, nil
}

func (w *SecureOnboardingWorkflow) validateActivationAuthority(ctx context.Context, plan SecureOnboardingPlan) error {
	checkedAt := w.now().UTC().Truncate(time.Microsecond)
	if checkedAt.IsZero() {
		return ErrSecureOnboardingWorkflow
	}
	if err := w.authority.ValidateCurrentProxyLease(
		ctx, plan.Binding.AccountID, plan.Binding.SlotID, plan.Binding.ExecutionEpoch,
		plan.Binding.ProxyLeaseID, checkedAt,
	); err != nil {
		return ErrSecureOnboardingWorkflow
	}
	return nil
}

// CompleteActivation consumes the durable intent only after the exact secure
// activation command reports a healthy, image-matched success. Replays are
// idempotent in the intent repository.
func (w *SecureOnboardingWorkflow) CompleteActivation(ctx context.Context, plan SecureOnboardingPlan, result *executionv1.CommandResult) error {
	if w == nil || w.intents == nil || w.builder == nil || ctx == nil || ctx.Err() != nil ||
		w.builder.validateBindingIdentity(plan.Binding) != nil || plan.IntentID == "" || plan.Owner == "" || plan.DesiredGeneration == 0 ||
		result == nil || result.GetCommandId() != plan.Binding.ActivationCommandID || !result.GetSucceeded() ||
		result.GetErrorCode() != "" || result.GetCredentialTransportKey() != nil || result.GetSlot() == nil ||
		result.GetSlot().GetSlotId() != plan.Binding.SlotID || result.GetSlot().GetExecutionEpoch() != plan.Binding.ExecutionEpoch ||
		result.GetSlot().GetImageDigest() != plan.Binding.ImageDigest || !result.GetSlot().GetHealthy() {
		return ErrSecureOnboardingWorkflow
	}
	if err := w.intents.Complete(ctx, plan.IntentID, plan.Owner); err != nil {
		return ErrSecureOnboardingWorkflow
	}
	return nil
}
