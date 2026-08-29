package hostagent

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

var commandImageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type SlotCommandProvider interface {
	provider.ExecutionProvider
	provider.SlotInspector
}

type SlotCommandExecutorConfig struct {
	Provider     SlotCommandProvider
	Resources    provider.ResourceLimits
	Security     provider.SecurityPolicy
	Network      provider.NetworkPolicy
	DrainTimeout time.Duration
	MaxSlots     uint32
	Now          func() time.Time
}

type NodeSnapshot struct {
	Slots                []*executionv1.SlotObservation
	ActiveCLI            uint32
	ActiveAPI            uint32
	ActiveTotal          uint32
	AllocatedSlots       uint32
	AllocatedCPUMillis   uint64
	AllocatedMemoryBytes uint64
}

type SlotCommandExecutor struct {
	provider     SlotCommandProvider
	resources    provider.ResourceLimits
	security     provider.SecurityPolicy
	network      provider.NetworkPolicy
	drainTimeout time.Duration
	maxSlots     uint32
	now          func() time.Time

	operationMu    sync.Mutex
	mu             sync.RWMutex
	observations   map[string]*executionv1.SlotObservation
	revokedThrough map[string]uint64
}

func NewSlotCommandExecutor(config SlotCommandExecutorConfig) (*SlotCommandExecutor, error) {
	if config.Provider == nil || config.Resources.Validate() != nil || config.Security.Validate() != nil || config.Network.Validate() != nil ||
		config.DrainTimeout <= 0 || config.MaxSlots == 0 {
		return nil, errors.New("slot command executor configuration is invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &SlotCommandExecutor{
		provider: config.Provider, resources: config.Resources, security: config.Security, network: config.Network,
		drainTimeout: config.DrainTimeout, maxSlots: config.MaxSlots, now: config.Now,
		observations: make(map[string]*executionv1.SlotObservation), revokedThrough: make(map[string]uint64),
	}, nil
}

func (e *SlotCommandExecutor) ExecuteSlotCommand(ctx context.Context, command *executionv1.SlotCommand) *executionv1.CommandResult {
	if command == nil {
		return failedCommandResult("", "", 1, "invalid_command", "slot command is invalid", nil)
	}
	observation := missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest())
	if err := e.validateCommand(command); err != nil {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "invalid_command", "slot command is invalid", observation)
	}
	deadline := command.GetDeadline().AsTime()
	commandContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if commandContext.Err() != nil {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "command_deadline_exceeded", "slot command deadline exceeded", observation)
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	if e.epochRevoked(command.GetSlotId(), command.GetExecutionEpoch()) &&
		(command.GetAction() == executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE || command.GetAction() == executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START) {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "execution_epoch_revoked", "execution epoch is revoked", observation)
	}

	var err error
	switch command.GetAction() {
	case executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE:
		observation, err = e.create(commandContext, command)
	case executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START:
		observation, err = e.start(commandContext, command)
	case executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DRAIN:
		observation, err = e.drain(commandContext, command)
	case executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_STOP:
		observation, err = e.stop(commandContext, command)
	case executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DESTROY:
		observation, err = e.destroy(commandContext, command)
	case executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT:
		observation, err = e.inspect(commandContext, command)
	default:
		err = errors.New("unsupported slot action")
	}
	if err != nil {
		code := classifyCommandError(err)
		if inspected, inspectErr := e.inspect(commandContext, command); inspectErr == nil {
			observation = inspected
		} else if observation == nil {
			observation = missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest())
		}
		observation.Healthy = false
		observation.Reason = code
		e.remember(observation)
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), code, "slot provider operation failed", observation)
	}
	e.remember(observation)
	return &executionv1.CommandResult{CommandId: command.GetCommandId(), Succeeded: true, Slot: cloneObservation(observation)}
}

func (e *SlotCommandExecutor) RevokeEpoch(ctx context.Context, command *executionv1.RevokeEpochCommand) *executionv1.CommandResult {
	if command == nil || credential.ValidateTransportID(command.GetCommandId()) != nil || credential.ValidateTransportID(command.GetSlotId()) != nil || command.GetExecutionEpoch() == 0 {
		return failedCommandResult(commandID(command), commandSlotID(command), commandEpoch(command), "invalid_command", "epoch revocation is invalid", nil)
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	e.mu.Lock()
	if command.GetExecutionEpoch() > e.revokedThrough[command.GetSlotId()] {
		e.revokedThrough[command.GetSlotId()] = command.GetExecutionEpoch()
	}
	e.mu.Unlock()
	status, err := e.provider.InspectSlot(ctx, command.GetSlotId())
	if errors.Is(err, provider.ErrNotFound) {
		observation := missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), "")
		return &executionv1.CommandResult{CommandId: command.GetCommandId(), Succeeded: true, Slot: observation}
	}
	if err != nil {
		observation := failedObservation(command.GetSlotId(), command.GetExecutionEpoch(), "epoch_revoke_failed")
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "epoch_revoke_failed", "execution epoch revocation failed", observation)
	}
	if status.Epoch > command.GetExecutionEpoch() {
		// The old epoch is already absent and a newer fenced generation owns the
		// stable slot id. Never let a delayed revocation stop the new worker.
		observation := missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), status.ImageDigest)
		return &executionv1.CommandResult{CommandId: command.GetCommandId(), Succeeded: true, Slot: observation}
	}
	deadline := e.now().UTC().Add(e.drainTimeout)
	_ = e.provider.Drain(ctx, status.ProviderRef, deadline)
	stopErr := e.provider.Stop(ctx, status.ProviderRef)
	if stopErr != nil {
		observation := observationFromStatus(status)
		observation.Healthy = false
		observation.Reason = "epoch_revoke_failed"
		e.remember(observation)
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "epoch_revoke_failed", "execution epoch revocation failed", observation)
	}
	status.State = slot.StateStopped
	status.Epoch = command.GetExecutionEpoch()
	status.Healthy = false
	status.Reason = "epoch_revoked"
	observation := observationFromStatus(status)
	e.remember(observation)
	return &executionv1.CommandResult{CommandId: command.GetCommandId(), Succeeded: true, Slot: observation}
}

func (e *SlotCommandExecutor) Snapshot() NodeSnapshot {
	e.mu.RLock()
	observations := make([]*executionv1.SlotObservation, 0, len(e.observations))
	for _, observation := range e.observations {
		observations = append(observations, cloneObservation(observation))
	}
	e.mu.RUnlock()
	sort.Slice(observations, func(left, right int) bool { return observations[left].GetSlotId() < observations[right].GetSlotId() })
	allocatedSlots := uint32(len(observations))
	if allocatedSlots > e.maxSlots {
		allocatedSlots = e.maxSlots
		observations = observations[:allocatedSlots]
	}
	return NodeSnapshot{
		Slots: observations, AllocatedSlots: allocatedSlots,
		AllocatedCPUMillis:   uint64(e.resources.CPUMilli) * uint64(allocatedSlots),
		AllocatedMemoryBytes: uint64(e.resources.MemoryBytes) * uint64(allocatedSlots),
	}
}

func (e *SlotCommandExecutor) create(ctx context.Context, command *executionv1.SlotCommand) (*executionv1.SlotObservation, error) {
	if status, err := e.provider.InspectSlot(ctx, command.GetSlotId()); errors.Is(err, provider.ErrNotFound) {
		if e.allocatedCount() >= e.maxSlots {
			return nil, errSlotCapacity
		}
	} else if err != nil {
		return nil, err
	} else if status.Epoch != command.GetExecutionEpoch() {
		return nil, errEpochConflict
	}
	instance, err := e.provider.Create(ctx, e.spec(command))
	if err != nil {
		return nil, err
	}
	return &executionv1.SlotObservation{
		SlotId: command.GetSlotId(), ProviderRef: instance.ProviderRef, ExecutionEpoch: command.GetExecutionEpoch(),
		ActualState: "created", Healthy: false, ImageDigest: command.GetImageDigest(),
	}, nil
}

func (e *SlotCommandExecutor) start(ctx context.Context, command *executionv1.SlotCommand) (*executionv1.SlotObservation, error) {
	status, err := e.inspectExact(ctx, command)
	if err != nil {
		return nil, err
	}
	if err := e.provider.Start(ctx, status.ProviderRef); err != nil {
		return observationFromStatus(status), err
	}
	return e.inspect(ctx, command)
}

func (e *SlotCommandExecutor) drain(ctx context.Context, command *executionv1.SlotCommand) (*executionv1.SlotObservation, error) {
	status, err := e.inspectExact(ctx, command)
	if err != nil {
		return nil, err
	}
	if err := e.provider.Drain(ctx, status.ProviderRef, command.GetDeadline().AsTime()); err != nil {
		return observationFromStatus(status), err
	}
	observation := observationFromStatus(status)
	observation.ActualState = "draining"
	observation.Healthy = false
	return observation, nil
}

func (e *SlotCommandExecutor) stop(ctx context.Context, command *executionv1.SlotCommand) (*executionv1.SlotObservation, error) {
	status, err := e.inspectExact(ctx, command)
	if errors.Is(err, provider.ErrNotFound) {
		return missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest()), nil
	}
	if err != nil {
		return nil, err
	}
	if err := e.provider.Stop(ctx, status.ProviderRef); err != nil {
		return observationFromStatus(status), err
	}
	status.State, status.Healthy = slot.StateStopped, false
	return observationFromStatus(status), nil
}

func (e *SlotCommandExecutor) destroy(ctx context.Context, command *executionv1.SlotCommand) (*executionv1.SlotObservation, error) {
	status, err := e.inspectExact(ctx, command)
	if errors.Is(err, provider.ErrNotFound) {
		observation := missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest())
		observation.ActualState = "destroyed"
		return observation, nil
	}
	if err != nil {
		return nil, err
	}
	if err := e.provider.Destroy(ctx, status.ProviderRef); err != nil {
		return observationFromStatus(status), err
	}
	observation := observationFromStatus(status)
	observation.ActualState, observation.Healthy = "destroyed", false
	return observation, nil
}

func (e *SlotCommandExecutor) inspect(ctx context.Context, command *executionv1.SlotCommand) (*executionv1.SlotObservation, error) {
	status, err := e.inspectExact(ctx, command)
	if errors.Is(err, provider.ErrNotFound) {
		return missingObservation(command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest()), nil
	}
	if err != nil {
		return nil, err
	}
	return observationFromStatus(status), nil
}

func (e *SlotCommandExecutor) inspectExact(ctx context.Context, command *executionv1.SlotCommand) (provider.Status, error) {
	status, err := e.provider.InspectSlot(ctx, command.GetSlotId())
	if err != nil {
		return provider.Status{}, err
	}
	if status.SlotID != command.GetSlotId() || status.Epoch != command.GetExecutionEpoch() {
		return provider.Status{}, errEpochConflict
	}
	return status, nil
}

func (e *SlotCommandExecutor) spec(command *executionv1.SlotCommand) provider.SlotSpec {
	return provider.SlotSpec{
		SlotID: command.GetSlotId(), AccountID: command.GetAccountId(), Epoch: command.GetExecutionEpoch(), ImageDigest: command.GetImageDigest(),
		Resources: e.resources, Security: e.security, Network: e.network,
	}
}

func (e *SlotCommandExecutor) validateCommand(command *executionv1.SlotCommand) error {
	if credential.ValidateTransportID(command.GetCommandId()) != nil || credential.ValidateTransportID(command.GetSlotId()) != nil ||
		credential.ValidateTransportID(command.GetAccountId()) != nil || command.GetExecutionEpoch() == 0 || command.GetDeadline() == nil ||
		command.GetDeadline().CheckValid() != nil {
		return errors.New("invalid command identity")
	}
	if command.GetAction() < executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE ||
		command.GetAction() > executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT {
		return errors.New("invalid command action")
	}
	if !commandImageDigestPattern.MatchString(command.GetImageDigest()) {
		return errors.New("invalid image digest")
	}
	return e.spec(command).Validate()
}

func (e *SlotCommandExecutor) epochRevoked(slotID string, epoch uint64) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return epoch <= e.revokedThrough[slotID]
}

func (e *SlotCommandExecutor) remember(observation *executionv1.SlotObservation) {
	if observation == nil || observation.GetSlotId() == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if observation.GetActualState() == "destroyed" || observation.GetActualState() == "missing" {
		delete(e.observations, observation.GetSlotId())
		return
	}
	e.observations[observation.GetSlotId()] = cloneObservation(observation)
}

var errEpochConflict = errors.New("slot execution epoch conflicts with command")
var errSlotCapacity = errors.New("node slot capacity is exhausted")

func (e *SlotCommandExecutor) allocatedCount() uint32 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return uint32(len(e.observations))
}

func classifyCommandError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "command_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "command_deadline_exceeded"
	case errors.Is(err, provider.ErrNotFound):
		return "slot_not_found"
	case errors.Is(err, errEpochConflict):
		return "execution_epoch_conflict"
	case errors.Is(err, errSlotCapacity):
		return "node_slot_capacity"
	default:
		return "provider_operation_failed"
	}
}

func observationFromStatus(status provider.Status) *executionv1.SlotObservation {
	actualState := "failed"
	switch status.State {
	case slot.StateRequested, slot.StatePulling, slot.StateCreating:
		actualState = "creating"
	case slot.StateStarting:
		actualState = "created"
	case slot.StateReady, slot.StateBusy:
		actualState = "running"
	case slot.StateDraining:
		actualState = "draining"
	case slot.StateStopped:
		actualState = "stopped"
	case slot.StateDestroyed:
		actualState = "destroyed"
	case slot.StateUnhealthy, slot.StateRecreating:
		actualState = "failed"
	}
	reason := strings.TrimSpace(status.Reason)
	if len(reason) > 1024 {
		reason = reason[:1024]
	}
	return &executionv1.SlotObservation{
		SlotId: status.SlotID, ProviderRef: status.ProviderRef, ExecutionEpoch: status.Epoch,
		ActualState: actualState, Healthy: status.Healthy, Reason: reason, ImageDigest: status.ImageDigest,
	}
}

func missingObservation(slotID string, epoch uint64, imageDigest string) *executionv1.SlotObservation {
	return &executionv1.SlotObservation{SlotId: slotID, ExecutionEpoch: epoch, ActualState: "missing", Healthy: false, ImageDigest: imageDigest}
}

func failedObservation(slotID string, epoch uint64, reason string) *executionv1.SlotObservation {
	return &executionv1.SlotObservation{SlotId: slotID, ExecutionEpoch: epoch, ActualState: "failed", Healthy: false, Reason: reason}
}

func failedCommandResult(commandID, slotID string, epoch uint64, code, message string, observation *executionv1.SlotObservation) *executionv1.CommandResult {
	if observation == nil {
		observation = failedObservation(slotID, epoch, code)
	}
	return &executionv1.CommandResult{
		CommandId: commandID, Succeeded: false, ErrorCode: code, ErrorMessage: message, Slot: cloneObservation(observation),
	}
}

func cloneObservation(value *executionv1.SlotObservation) *executionv1.SlotObservation {
	if value == nil {
		return nil
	}
	return &executionv1.SlotObservation{
		SlotId: value.GetSlotId(), ProviderRef: value.GetProviderRef(), ExecutionEpoch: value.GetExecutionEpoch(),
		ActualState: value.GetActualState(), Healthy: value.GetHealthy(), Reason: value.GetReason(), ImageDigest: value.GetImageDigest(),
	}
}

func commandID(command *executionv1.RevokeEpochCommand) string {
	if command == nil {
		return ""
	}
	return command.GetCommandId()
}

func commandSlotID(command *executionv1.RevokeEpochCommand) string {
	if command == nil {
		return ""
	}
	return command.GetSlotId()
}

func commandEpoch(command *executionv1.RevokeEpochCommand) uint64 {
	if command == nil || command.GetExecutionEpoch() == 0 {
		return 1
	}
	return command.GetExecutionEpoch()
}
