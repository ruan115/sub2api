package service

import (
	"context"
	"errors"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

var ErrProvisioningAdvance = errors.New("secure onboarding provisioning advance failed")

type ProvisioningControlDispatcher interface {
	Dispatch(ctx context.Context, nodeID string, response *executionv1.NodeControlServiceControlResponse) error
}

type SecureOnboardingController struct {
	repository onboarding.ProvisioningRepository
	workflow   *SecureOnboardingWorkflow
	dispatcher ProvisioningControlDispatcher
	now        func() time.Time
	retryDelay time.Duration
}

type SecureOnboardingControllerConfig struct {
	RetryDelay time.Duration
	Now        func() time.Time
}

func DefaultSecureOnboardingControllerConfig() SecureOnboardingControllerConfig {
	return SecureOnboardingControllerConfig{RetryDelay: 5 * time.Second, Now: time.Now}
}

func NewSecureOnboardingController(
	repository onboarding.ProvisioningRepository,
	workflow *SecureOnboardingWorkflow,
	dispatcher ProvisioningControlDispatcher,
	config SecureOnboardingControllerConfig,
) (*SecureOnboardingController, error) {
	if repository == nil || workflow == nil || dispatcher == nil || config.RetryDelay <= 0 {
		return nil, ErrProvisioningAdvance
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &SecureOnboardingController{
		repository: repository, workflow: workflow, dispatcher: dispatcher,
		now: config.Now, retryDelay: config.RetryDelay,
	}, nil
}

// Advance performs at most one durable state transition. A polling runner may
// call it repeatedly and after process restarts; NodeControl results are
// persisted by ProvisioningCommandObserver between calls.
func (c *SecureOnboardingController) Advance(ctx context.Context, workflowID string) (string, error) {
	if c == nil || c.repository == nil || c.workflow == nil || c.dispatcher == nil || c.now == nil || c.retryDelay <= 0 ||
		ctx == nil || ctx.Err() != nil {
		return "", ErrProvisioningAdvance
	}
	record, err := c.repository.GetProvisioning(ctx, workflowID)
	if err != nil {
		return "", ErrProvisioningAdvance
	}
	defer record.Destroy()
	plan := secureOnboardingPlan(record)
	now := c.now().UTC()
	if !record.CommandDeadline.After(now) && record.Status != onboarding.ProvisioningCompleted &&
		record.Status != onboarding.ProvisioningFailed {
		if err := c.repository.FailProvisioning(ctx, record.ID, "workflow_deadline_exceeded", now); err != nil {
			return record.Status, ErrProvisioningAdvance
		}
		return onboarding.ProvisioningFailed, ErrProvisioningAdvance
	}
	switch record.Status {
	case onboarding.ProvisioningPendingKey:
		if err := c.dispatchKey(ctx, plan, record, now); err != nil {
			return record.Status, ErrProvisioningAdvance
		}
		return onboarding.ProvisioningKeyDispatched, nil
	case onboarding.ProvisioningKeyDispatched:
		if now.Before(record.UpdatedAt.Add(c.retryDelay)) {
			return record.Status, nil
		}
		if err := c.dispatchKey(ctx, plan, record, now); err != nil {
			return record.Status, ErrProvisioningAdvance
		}
		return record.Status, nil
	case onboarding.ProvisioningActivationDispatched:
		if now.Before(record.UpdatedAt.Add(c.retryDelay)) {
			return record.Status, nil
		}
		if err := c.dispatchActivation(ctx, plan, record, now); err != nil {
			return record.Status, ErrProvisioningAdvance
		}
		return record.Status, nil
	case onboarding.ProvisioningKeyReady:
		if err := c.dispatchActivation(ctx, plan, record, now); err != nil {
			return record.Status, ErrProvisioningAdvance
		}
		return onboarding.ProvisioningActivationDispatched, nil
	case onboarding.ProvisioningActivationSucceeded:
		if err := c.workflow.CompleteActivation(ctx, plan, provisioningActivationResult(record)); err != nil {
			return record.Status, ErrProvisioningAdvance
		}
		if err := c.repository.CompleteProvisioning(ctx, record.ID, now); err != nil {
			return record.Status, ErrProvisioningAdvance
		}
		return onboarding.ProvisioningCompleted, nil
	case onboarding.ProvisioningCompleted:
		return record.Status, nil
	case onboarding.ProvisioningFailed:
		return record.Status, ErrProvisioningAdvance
	default:
		return record.Status, ErrProvisioningAdvance
	}
}

func (c *SecureOnboardingController) dispatchKey(ctx context.Context, plan SecureOnboardingPlan, record onboarding.Provisioning, now time.Time) error {
	response, err := c.workflow.CredentialKeyCommand(plan)
	if err != nil || c.dispatcher.Dispatch(ctx, record.NodeID, response) != nil {
		return ErrProvisioningAdvance
	}
	if err := c.repository.MarkProvisioningKeyDispatched(ctx, record.ID, now); err != nil {
		return ErrProvisioningAdvance
	}
	return nil
}

func (c *SecureOnboardingController) dispatchActivation(ctx context.Context, plan SecureOnboardingPlan, record onboarding.Provisioning, now time.Time) error {
	keyResult := provisioningKeyResult(record)
	response, err := c.workflow.PrepareActivation(ctx, plan, keyResult)
	if err != nil {
		return ErrProvisioningAdvance
	}
	command := response.GetSecureActivationCommand()
	if command == nil {
		return ErrProvisioningAdvance
	}
	defer eraseProvisioningCiphertext(command.EncryptedCredentialBundle)
	if err := c.dispatcher.Dispatch(ctx, record.NodeID, response); err != nil {
		return ErrProvisioningAdvance
	}
	if err := c.repository.MarkProvisioningActivationDispatched(ctx, record.ID, now); err != nil {
		return ErrProvisioningAdvance
	}
	return nil
}

func secureOnboardingPlan(record onboarding.Provisioning) SecureOnboardingPlan {
	return SecureOnboardingPlan{
		IntentID: record.IntentID, DesiredGeneration: record.DesiredGeneration, Owner: record.Owner,
		Binding: SecureOnboardingBinding{
			KeyCommandID: record.KeyCommandID, ActivationCommandID: record.ActivationCommandID,
			SlotID: record.SlotID, AccountID: record.AccountID, ExecutionEpoch: record.ExecutionEpoch,
			ImageDigest: record.ImageDigest, CredentialLeaseID: record.CredentialLeaseID,
			ProxyLeaseID: record.ProxyLeaseID, Deadline: record.CommandDeadline,
		},
	}
}

func provisioningKeyResult(record onboarding.Provisioning) *executionv1.CommandResult {
	return &executionv1.CommandResult{
		CommandId: record.KeyCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: record.SlotID, ExecutionEpoch: record.ExecutionEpoch, ImageDigest: record.ImageDigest, Healthy: true,
		},
		CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{
			KeyId: record.KeyID, PublicKey: append([]byte(nil), record.KeyPublicKey...),
		},
	}
}

func provisioningActivationResult(record onboarding.Provisioning) *executionv1.CommandResult {
	return &executionv1.CommandResult{
		CommandId: record.ActivationCommandID, Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: record.SlotID, ExecutionEpoch: record.ExecutionEpoch, ImageDigest: record.ImageDigest, Healthy: true,
		},
	}
}

func eraseProvisioningCiphertext(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
