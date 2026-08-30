package hostagent

import (
	"context"
	"errors"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const maxActivationBundleBytes = 2 << 20

// ActivationCommandExecutor handles the credential-bearing portion of the
// NodeControl protocol. It is deliberately separate from lifecycle execution
// so a host-agent can disable secure onboarding without changing slot control.
type ActivationCommandExecutor interface {
	CredentialTransportKey(ctx context.Context, command *executionv1.CredentialKeyCommand) *executionv1.CommandResult
	SecureActivate(ctx context.Context, command *executionv1.SecureActivationCommand, sink worker.SealedCredentialSink) *executionv1.CommandResult
}

type RuntimeActivationExecutorConfig struct {
	Controller *Controller
	Inspector  provider.SlotInspector
	Resources  provider.ResourceLimits
	Security   provider.SecurityPolicy
	Network    provider.NetworkPolicy
}

type RuntimeActivationExecutor struct {
	starter   activationRuntimeStarter
	inspector provider.SlotInspector
	resources provider.ResourceLimits
	security  provider.SecurityPolicy
	network   provider.NetworkPolicy
}

type activationRuntime interface {
	CredentialTransportKey(ctx context.Context) (CredentialTransportKey, error)
	ActivateSecure(ctx context.Context, activation ActivationLease, sink worker.SealedCredentialSink) ([]executionv1.ExecutionMode, error)
	Health(ctx context.Context) (*executionv1.HealthResponse, error)
	Close() error
}

type activationRuntimeStarter interface {
	StartRuntime(ctx context.Context, spec provider.SlotSpec) (activationRuntime, error)
}

type controllerActivationRuntimeStarter struct {
	controller *Controller
}

func (s controllerActivationRuntimeStarter) StartRuntime(ctx context.Context, spec provider.SlotSpec) (activationRuntime, error) {
	return s.controller.Start(ctx, spec)
}

func NewRuntimeActivationExecutor(config RuntimeActivationExecutorConfig) (*RuntimeActivationExecutor, error) {
	if config.Controller == nil || config.Inspector == nil || config.Resources.Validate() != nil ||
		config.Security.Validate() != nil || config.Network.Validate() != nil {
		return nil, errors.New("runtime activation executor configuration is invalid")
	}
	return &RuntimeActivationExecutor{
		starter: controllerActivationRuntimeStarter{controller: config.Controller}, inspector: config.Inspector, resources: config.Resources,
		security: config.Security, network: config.Network,
	}, nil
}

func (e *RuntimeActivationExecutor) CredentialTransportKey(ctx context.Context, command *executionv1.CredentialKeyCommand) *executionv1.CommandResult {
	if command == nil {
		return failedCommandResult("", "", 1, "invalid_command", "credential-key command is invalid", nil)
	}
	observation := missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest())
	if e == nil || ctx == nil || e.validateBinding(command.GetCommandId(), command.GetSlotId(), command.GetAccountId(), command.GetExecutionEpoch(), command.GetImageDigest(), command.GetDeadline()) != nil {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "invalid_command", "credential-key command is invalid", observation)
	}
	commandContext, cancel := context.WithDeadline(ctx, command.GetDeadline().AsTime())
	defer cancel()
	if commandContext.Err() != nil {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "command_deadline_exceeded", "credential-key command deadline exceeded", observation)
	}
	runtime, observation, err := e.startVerifiedRuntime(
		commandContext, command.GetSlotId(), command.GetAccountId(), command.GetExecutionEpoch(), command.GetImageDigest(),
	)
	if err != nil {
		return e.failure(commandContext, command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), "key_discovery_failed")
	}
	defer runtime.Close()
	transportKey, err := runtime.CredentialTransportKey(commandContext)
	if err != nil || credential.ValidateRecipientKey(transportKey.KeyID, transportKey.PublicKey) != nil {
		return e.failure(commandContext, command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), "key_discovery_failed")
	}
	return &executionv1.CommandResult{
		CommandId: command.GetCommandId(), Succeeded: true, Slot: observation,
		CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{
			KeyId: transportKey.KeyID, PublicKey: append([]byte(nil), transportKey.PublicKey...),
		},
	}
}

func (e *RuntimeActivationExecutor) SecureActivate(ctx context.Context, command *executionv1.SecureActivationCommand, sink worker.SealedCredentialSink) *executionv1.CommandResult {
	if command == nil {
		return failedCommandResult("", "", 1, "invalid_command", "secure activation command is invalid", nil)
	}
	observation := missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest())
	if e == nil || ctx == nil || sink == nil || e.validateBinding(command.GetCommandId(), command.GetSlotId(), command.GetAccountId(), command.GetExecutionEpoch(), command.GetImageDigest(), command.GetDeadline()) != nil ||
		credential.ValidateTransportID(command.GetCredentialLeaseId()) != nil || credential.ValidateTransportID(command.GetProxyLeaseId()) != nil ||
		len(command.GetEncryptedCredentialBundle()) == 0 || len(command.GetEncryptedCredentialBundle()) > maxActivationBundleBytes {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "invalid_command", "secure activation command is invalid", observation)
	}
	commandContext, cancel := context.WithDeadline(ctx, command.GetDeadline().AsTime())
	defer cancel()
	if commandContext.Err() != nil {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "command_deadline_exceeded", "secure activation command deadline exceeded", observation)
	}
	runtime, _, err := e.startVerifiedRuntime(
		commandContext, command.GetSlotId(), command.GetAccountId(), command.GetExecutionEpoch(), command.GetImageDigest(),
	)
	if err != nil {
		return e.failure(commandContext, command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), "secure_activation_failed")
	}
	defer runtime.Close()
	bundle := append([]byte(nil), command.GetEncryptedCredentialBundle()...)
	defer zero(bundle)
	if _, err := runtime.ActivateSecure(commandContext, ActivationLease{
		CredentialLeaseID: command.GetCredentialLeaseId(), EncryptedCredentialBundle: bundle, ProxyLeaseID: command.GetProxyLeaseId(),
	}, sink); err != nil {
		return e.failure(commandContext, command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), "secure_activation_failed")
	}
	health, err := runtime.Health(commandContext)
	if err != nil || health.GetSlotId() != command.GetSlotId() || health.GetExecutionEpoch() != command.GetExecutionEpoch() {
		return e.failure(commandContext, command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), "secure_activation_unhealthy")
	}
	observation = e.observe(commandContext, command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest())
	if !observation.GetHealthy() || observation.GetImageDigest() != command.GetImageDigest() {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "secure_activation_unhealthy", "secure activation health check failed", observation)
	}
	return &executionv1.CommandResult{CommandId: command.GetCommandId(), Succeeded: true, Slot: observation}
}

// startVerifiedRuntime keeps encrypted credential material out of any runtime
// until the provider observation proves that the exact requested slot, epoch,
// image and health state are active. Provider implementations should enforce
// the same invariant, but the credential boundary must fail closed on its own.
func (e *RuntimeActivationExecutor) startVerifiedRuntime(
	ctx context.Context,
	slotID string,
	accountID string,
	epoch uint64,
	imageDigest string,
) (activationRuntime, *executionv1.SlotObservation, error) {
	runtime, err := e.starter.StartRuntime(ctx, e.spec(slotID, accountID, epoch, imageDigest))
	if err != nil {
		return nil, nil, err
	}
	observation := e.observe(ctx, slotID, epoch, imageDigest)
	if !observation.GetHealthy() || observation.GetImageDigest() != imageDigest {
		_ = runtime.Close()
		return nil, observation, errors.New("runtime binding verification failed")
	}
	return runtime, observation, nil
}

func (e *RuntimeActivationExecutor) validateBinding(commandID, slotID, accountID string, epoch uint64, imageDigest string, deadline *timestamppb.Timestamp) error {
	if credential.ValidateTransportID(commandID) != nil || credential.ValidateTransportID(slotID) != nil ||
		credential.ValidateTransportID(accountID) != nil || epoch == 0 || deadline == nil || deadline.CheckValid() != nil {
		return errors.New("activation command binding is invalid")
	}
	return e.spec(slotID, accountID, epoch, imageDigest).Validate()
}

func (e *RuntimeActivationExecutor) spec(slotID, accountID string, epoch uint64, imageDigest string) provider.SlotSpec {
	return provider.SlotSpec{
		SlotID: slotID, AccountID: accountID, Epoch: epoch, ImageDigest: imageDigest,
		Resources: e.resources, Security: e.security, Network: e.network,
	}
}

func (e *RuntimeActivationExecutor) failure(ctx context.Context, commandID, slotID string, epoch uint64, imageDigest, code string) *executionv1.CommandResult {
	observation := e.observe(ctx, slotID, epoch, imageDigest)
	observation.Healthy = false
	observation.Reason = code
	return failedCommandResult(commandID, slotID, epoch, code, "worker activation operation failed", observation)
}

func (e *RuntimeActivationExecutor) observe(ctx context.Context, slotID string, epoch uint64, imageDigest string) *executionv1.SlotObservation {
	status, err := e.inspector.InspectSlot(ctx, slotID)
	if err != nil || status.SlotID != slotID || status.Epoch != epoch {
		return missingObservation(slotID, epoch, imageDigest)
	}
	return observationFromStatus(status)
}
