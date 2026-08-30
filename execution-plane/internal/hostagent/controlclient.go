package hostagent

import (
	"context"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	controlProtocolMajor uint32 = 1
	controlProtocolMinor uint32 = 1

	controlSecureActivationCapability = "secure_activation"
)

var (
	hostNodeIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	hostLabelKeyPattern   = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	hostCapabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	hostSensitiveWords    = []string{"authorization", "credential", "password", "cookie", "secret", "session_key", "access_token", "refresh_token", "bearer", "sk-"}
)

type ControlCommandExecutor interface {
	ExecuteSlotCommand(ctx context.Context, command *executionv1.SlotCommand) *executionv1.CommandResult
	RevokeEpoch(ctx context.Context, command *executionv1.RevokeEpochCommand) *executionv1.CommandResult
	Snapshot() NodeSnapshot
}

type ControlClientConfig struct {
	Client                executionv1.NodeControlServiceClient
	Executor              ControlCommandExecutor
	ActivationExecutor    ActivationCommandExecutor
	NodeID                string
	Labels                map[string]string
	Capabilities          []string
	Capacity              *executionv1.Capacity
	HeartbeatInterval     time.Duration
	ReconnectMin          time.Duration
	ReconnectMax          time.Duration
	MaxConcurrentCommands int
	CommandQueue          int
	Now                   func() time.Time
}

type ControlClient struct {
	client                executionv1.NodeControlServiceClient
	executor              ControlCommandExecutor
	activationExecutor    ActivationCommandExecutor
	nodeID                string
	labels                map[string]string
	capabilities          []string
	capacity              *executionv1.Capacity
	heartbeatInterval     time.Duration
	reconnectMin          time.Duration
	reconnectMax          time.Duration
	maxConcurrentCommands int
	commandQueue          int
	now                   func() time.Time
}

type controlCommandEnvelope struct {
	slot       *executionv1.SlotCommand
	revoke     *executionv1.RevokeEpochCommand
	key        *executionv1.CredentialKeyCommand
	activation *executionv1.SecureActivationCommand
}

type credentialCommitForward struct {
	commandID string
	request   worker.SealedCredentialCommitRequest
	result    chan credentialCommitResult
}

type credentialCommitResult struct {
	versionID string
	err       error
}

func NewControlClient(config ControlClientConfig) (*ControlClient, error) {
	if config.Client == nil || config.Executor == nil || !hostNodeIDPattern.MatchString(config.NodeID) || config.Capacity == nil ||
		config.HeartbeatInterval <= 0 || config.ReconnectMin <= 0 || config.ReconnectMax < config.ReconnectMin ||
		config.MaxConcurrentCommands <= 0 || config.CommandQueue < config.MaxConcurrentCommands || !validControlCapacity(config.Capacity) {
		return nil, errors.New("host-agent control client configuration is invalid")
	}
	if len(config.Labels) > 32 || len(config.Capabilities) > 64 {
		return nil, errors.New("host-agent control metadata exceeds limits")
	}
	labels := make(map[string]string, len(config.Labels))
	for key, value := range config.Labels {
		if !hostLabelKeyPattern.MatchString(key) || len(value) > 128 || hostContainsSensitiveWord(key) || hostContainsSensitiveWord(value) {
			return nil, errors.New("host-agent control labels are invalid")
		}
		labels[key] = value
	}
	capabilities := append([]string(nil), config.Capabilities...)
	sort.Strings(capabilities)
	for index, capability := range capabilities {
		if !hostCapabilityPattern.MatchString(capability) || hostContainsSensitiveWord(capability) || index > 0 && capability == capabilities[index-1] {
			return nil, errors.New("host-agent control capabilities are invalid")
		}
	}
	_, secureCapability := stringSet(capabilities)[controlSecureActivationCapability]
	if secureCapability != (config.ActivationExecutor != nil) {
		return nil, errors.New("secure activation capability and executor must be configured together")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ControlClient{
		client: config.Client, executor: config.Executor, activationExecutor: config.ActivationExecutor,
		nodeID: config.NodeID, labels: labels, capabilities: capabilities,
		capacity: cloneCapacity(config.Capacity), heartbeatInterval: config.HeartbeatInterval,
		reconnectMin: config.ReconnectMin, reconnectMax: config.ReconnectMax,
		maxConcurrentCommands: config.MaxConcurrentCommands, commandQueue: config.CommandQueue, now: config.Now,
	}, nil
}

// Run maintains the authenticated bidirectional control stream until ctx is
// canceled. Transient transport failures reconnect with bounded backoff;
// authentication and protocol failures stop instead of retrying forever.
func (c *ControlClient) Run(ctx context.Context) error {
	backoff := c.reconnectMin
	for {
		err := c.runSession(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if terminalControlError(err) {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > c.reconnectMax {
			backoff = c.reconnectMax
		}
	}
}

func (c *ControlClient) runSession(ctx context.Context) error {
	sessionContext, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.client.Control(sessionContext)
	if err != nil {
		return err
	}
	defer func() { _ = stream.CloseSend() }()
	if err := stream.Send(&executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_Hello{Hello: c.hello()}}); err != nil {
		return err
	}

	type inbound struct {
		response *executionv1.NodeControlServiceControlResponse
		err      error
	}
	inboundEvents := make(chan inbound, 1)
	go func() {
		for {
			response, receiveErr := stream.Recv()
			select {
			case inboundEvents <- inbound{response: response, err: receiveErr}:
			case <-sessionContext.Done():
				return
			}
			if receiveErr != nil {
				return
			}
		}
	}()

	commands := make(chan controlCommandEnvelope, c.commandQueue)
	results := make(chan *executionv1.CommandResult, c.commandQueue+c.maxConcurrentCommands)
	commitForwards := make(chan credentialCommitForward, c.commandQueue)
	pendingCommitAcks := make(map[string]chan credentialCommitResult)
	for index := 0; index < c.maxConcurrentCommands; index++ {
		go c.runCommandWorker(sessionContext, commands, results, commitForwards)
	}

	if err := stream.Send(c.heartbeatEvent()); err != nil {
		return err
	}
	ticker := time.NewTicker(c.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := stream.Send(c.heartbeatEvent()); err != nil {
				return err
			}
		case result := <-results:
			if result == nil {
				return errors.New("slot command executor returned no result")
			}
			if err := stream.Send(&executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_CommandResult{CommandResult: result}}); err != nil {
				return err
			}
		case forward := <-commitForwards:
			if _, exists := pendingCommitAcks[forward.commandID]; exists || credential.ValidateTransportID(forward.commandID) != nil {
				forward.result <- credentialCommitResult{err: worker.ErrCredentialCommitRejected}
				zeroControlCredentialCommit(&forward.request)
				continue
			}
			pendingCommitAcks[forward.commandID] = forward.result
			commit := &executionv1.ControlCredentialCommit{
				CommandId: forward.commandID, AccountBinding: forward.request.AccountBinding,
				SlotId: forward.request.SlotID, ExecutionEpoch: forward.request.ExecutionEpoch,
				CredentialLeaseId: forward.request.CredentialLeaseID, ProxyLeaseId: forward.request.ProxyLeaseID,
				SealedCredentialBundle: append([]byte(nil), forward.request.SealedCredentialBundle...),
			}
			zeroControlCredentialCommit(&forward.request)
			sendErr := stream.Send(&executionv1.NodeControlServiceControlRequest{
				Event: &executionv1.NodeControlServiceControlRequest_CredentialCommit{CredentialCommit: commit},
			})
			zero(commit.SealedCredentialBundle)
			commit.SealedCredentialBundle = nil
			if sendErr != nil {
				delete(pendingCommitAcks, forward.commandID)
				forward.result <- credentialCommitResult{err: worker.ErrCredentialCommitRejected}
				return sendErr
			}
		case event := <-inboundEvents:
			if event.err != nil {
				if errors.Is(event.err, io.EOF) {
					return io.EOF
				}
				return event.err
			}
			if event.response == nil {
				return errors.New("orchestrator returned an empty control event")
			}
			if ack := event.response.GetCredentialCommitAck(); ack != nil {
				waiter, exists := pendingCommitAcks[ack.GetCommandId()]
				if !exists || !validCredentialCommitAck(ack) {
					return errors.New("orchestrator returned an invalid credential commit acknowledgement")
				}
				delete(pendingCommitAcks, ack.GetCommandId())
				if ack.GetAccepted() {
					waiter <- credentialCommitResult{versionID: ack.GetVersionId()}
				} else {
					waiter <- credentialCommitResult{err: worker.ErrCredentialCommitRejected}
				}
				continue
			}
			command := controlCommandEnvelope{
				slot: event.response.GetSlotCommand(), revoke: event.response.GetRevokeEpoch(),
				key: event.response.GetCredentialKeyCommand(), activation: event.response.GetSecureActivationCommand(),
			}
			if controlEnvelopeCount(command) != 1 {
				return errors.New("orchestrator returned an empty control event")
			}
			select {
			case commands <- command:
			default:
				result := overloadedCommandResult(command)
				if err := stream.Send(&executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_CommandResult{CommandResult: result}}); err != nil {
					return err
				}
			}
		}
	}
}

func (c *ControlClient) runCommandWorker(
	ctx context.Context,
	commands <-chan controlCommandEnvelope,
	results chan<- *executionv1.CommandResult,
	commitForwards chan<- credentialCommitForward,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case command := <-commands:
			var result *executionv1.CommandResult
			if command.slot != nil {
				result = c.executor.ExecuteSlotCommand(ctx, command.slot)
			} else if command.revoke != nil {
				result = c.executor.RevokeEpoch(ctx, command.revoke)
			} else if command.key != nil && c.activationExecutor != nil {
				result = c.activationExecutor.CredentialTransportKey(ctx, command.key)
			} else if command.activation != nil && c.activationExecutor != nil {
				sink := controlCredentialSink{ctx: ctx, commandID: command.activation.GetCommandId(), forwards: commitForwards}
				result = c.activationExecutor.SecureActivate(ctx, command.activation, sink)
			} else {
				result = unsupportedActivationCommandResult(command)
			}
			select {
			case results <- result:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (c *ControlClient) hello() *executionv1.NodeHello {
	labels := make(map[string]string, len(c.labels))
	for key, value := range c.labels {
		labels[key] = value
	}
	return &executionv1.NodeHello{
		NodeId: c.nodeID, ProtocolVersion: &executionv1.ProtocolVersion{Major: controlProtocolMajor, Minor: controlProtocolMinor},
		Capabilities: append([]string(nil), c.capabilities...), Capacity: cloneCapacity(c.capacity), Labels: labels,
	}
}

func (c *ControlClient) heartbeatEvent() *executionv1.NodeControlServiceControlRequest {
	snapshot := c.executor.Snapshot()
	return &executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_Heartbeat{Heartbeat: &executionv1.NodeHeartbeat{
		NodeId: c.nodeID, ObservedAt: timestamppb.New(c.now().UTC()),
		ActiveCli: snapshot.ActiveCLI, ActiveApi: snapshot.ActiveAPI, ActiveTotal: snapshot.ActiveTotal,
		AllocatedSlots: snapshot.AllocatedSlots, Slots: snapshot.Slots,
		AllocatedCpuMillis: snapshot.AllocatedCPUMillis, AllocatedMemoryBytes: snapshot.AllocatedMemoryBytes,
	}}}
}

func overloadedCommandResult(command controlCommandEnvelope) *executionv1.CommandResult {
	if command.slot != nil {
		return failedCommandResult(
			command.slot.GetCommandId(), command.slot.GetSlotId(), command.slot.GetExecutionEpoch(),
			"node_command_capacity", "node command capacity exceeded",
			missingObservation(command.slot.GetSlotId(), command.slot.GetExecutionEpoch(), command.slot.GetImageDigest()),
		)
	}
	if command.key != nil {
		return failedCommandResult(
			command.key.GetCommandId(), command.key.GetSlotId(), command.key.GetExecutionEpoch(),
			"node_command_capacity", "node command capacity exceeded",
			missingObservation(command.key.GetSlotId(), command.key.GetExecutionEpoch(), command.key.GetImageDigest()),
		)
	}
	if command.activation != nil {
		return failedCommandResult(
			command.activation.GetCommandId(), command.activation.GetSlotId(), command.activation.GetExecutionEpoch(),
			"node_command_capacity", "node command capacity exceeded",
			missingObservation(command.activation.GetSlotId(), command.activation.GetExecutionEpoch(), command.activation.GetImageDigest()),
		)
	}
	return failedCommandResult(
		commandID(command.revoke), commandSlotID(command.revoke), commandEpoch(command.revoke),
		"node_command_capacity", "node command capacity exceeded", nil,
	)
}

func unsupportedActivationCommandResult(command controlCommandEnvelope) *executionv1.CommandResult {
	if command.key != nil {
		return failedCommandResult(
			command.key.GetCommandId(), command.key.GetSlotId(), command.key.GetExecutionEpoch(),
			"unsupported_command", "secure activation is unavailable",
			missingObservation(command.key.GetSlotId(), command.key.GetExecutionEpoch(), command.key.GetImageDigest()),
		)
	}
	if command.activation != nil {
		return failedCommandResult(
			command.activation.GetCommandId(), command.activation.GetSlotId(), command.activation.GetExecutionEpoch(),
			"unsupported_command", "secure activation is unavailable",
			missingObservation(command.activation.GetSlotId(), command.activation.GetExecutionEpoch(), command.activation.GetImageDigest()),
		)
	}
	return failedCommandResult("", "", 1, "unsupported_command", "control command is unavailable", nil)
}

type controlCredentialSink struct {
	ctx       context.Context
	commandID string
	forwards  chan<- credentialCommitForward
}

func (s controlCredentialSink) CommitSealedCredential(ctx context.Context, request worker.SealedCredentialCommitRequest) (string, error) {
	if ctx == nil || ctx.Err() != nil || s.ctx == nil || s.ctx.Err() != nil || credential.ValidateTransportID(s.commandID) != nil ||
		credential.ValidateTransportID(request.AccountBinding) != nil || credential.ValidateTransportID(request.SlotID) != nil ||
		request.ExecutionEpoch == 0 || credential.ValidateTransportID(request.CredentialLeaseID) != nil ||
		credential.ValidateTransportID(request.ProxyLeaseID) != nil || len(request.SealedCredentialBundle) == 0 ||
		len(request.SealedCredentialBundle) > maxActivationBundleBytes {
		return "", worker.ErrCredentialCommitRejected
	}
	forward := credentialCommitForward{
		commandID: s.commandID,
		request: worker.SealedCredentialCommitRequest{
			AccountBinding: request.AccountBinding, SlotID: request.SlotID, ExecutionEpoch: request.ExecutionEpoch,
			CredentialLeaseID: request.CredentialLeaseID, ProxyLeaseID: request.ProxyLeaseID,
			SealedCredentialBundle: append([]byte(nil), request.SealedCredentialBundle...),
		},
		result: make(chan credentialCommitResult, 1),
	}
	select {
	case s.forwards <- forward:
	case <-ctx.Done():
		zeroControlCredentialCommit(&forward.request)
		return "", worker.ErrCredentialCommitRejected
	case <-s.ctx.Done():
		zeroControlCredentialCommit(&forward.request)
		return "", worker.ErrCredentialCommitRejected
	}
	select {
	case result := <-forward.result:
		if result.err != nil || credential.ValidateTransportID(result.versionID) != nil {
			return "", worker.ErrCredentialCommitRejected
		}
		return result.versionID, nil
	case <-ctx.Done():
		return "", worker.ErrCredentialCommitRejected
	case <-s.ctx.Done():
		return "", worker.ErrCredentialCommitRejected
	}
}

func validCredentialCommitAck(ack *executionv1.ControlCredentialCommitAck) bool {
	if ack == nil || credential.ValidateTransportID(ack.GetCommandId()) != nil || len(ack.GetErrorCode()) > 64 {
		return false
	}
	if ack.GetAccepted() {
		return credential.ValidateTransportID(ack.GetVersionId()) == nil && ack.GetErrorCode() == ""
	}
	return ack.GetVersionId() == "" && ack.GetErrorCode() != "" && !hostContainsSensitiveWord(ack.GetErrorCode())
}

func controlEnvelopeCount(command controlCommandEnvelope) int {
	count := 0
	if command.slot != nil {
		count++
	}
	if command.revoke != nil {
		count++
	}
	if command.key != nil {
		count++
	}
	if command.activation != nil {
		count++
	}
	return count
}

func zeroControlCredentialCommit(request *worker.SealedCredentialCommitRequest) {
	if request == nil {
		return
	}
	zero(request.SealedCredentialBundle)
	request.SealedCredentialBundle = nil
}

func validControlCapacity(capacity *executionv1.Capacity) bool {
	return capacity.GetMaxSlots() > 0 && capacity.GetMaxActiveCli() > 0 && capacity.GetMaxActiveApi() > 0 && capacity.GetMaxActiveTotal() > 0 &&
		capacity.GetMaxActiveCli() <= capacity.GetMaxActiveTotal() && capacity.GetMaxActiveApi() <= capacity.GetMaxActiveTotal() &&
		capacity.GetMaxActiveTotal() <= capacity.GetMaxSlots() && capacity.GetAllocatableCpuMillis() > 0 && capacity.GetAllocatableCpuMillis() <= 10_000_000 &&
		capacity.GetAllocatableMemoryBytes() > 0 && capacity.GetAllocatableMemoryBytes() <= 1<<60
}

func hostContainsSensitiveWord(value string) bool {
	normalized := strings.ToLower(value)
	for _, word := range hostSensitiveWords {
		if strings.Contains(normalized, word) {
			return true
		}
	}
	return false
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func cloneCapacity(capacity *executionv1.Capacity) *executionv1.Capacity {
	if capacity == nil {
		return nil
	}
	return &executionv1.Capacity{
		MaxSlots: capacity.GetMaxSlots(), MaxActiveCli: capacity.GetMaxActiveCli(), MaxActiveApi: capacity.GetMaxActiveApi(),
		MaxActiveTotal: capacity.GetMaxActiveTotal(), AllocatableCpuMillis: capacity.GetAllocatableCpuMillis(),
		AllocatableMemoryBytes: capacity.GetAllocatableMemoryBytes(),
	}
}

func terminalControlError(err error) bool {
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented:
		return true
	default:
		return false
	}
}
