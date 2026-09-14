package hostagent

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The inner protocol message remains bounded separately. The extra wire budget
// accommodates the worker envelope and its short-lived execution ticket.
const runtimeStreamWireBytes = dataplane.MaxMessageBytes + 64<<10

// OpenExecution starts one worker RPC without reading or accumulating its
// responses. The caller owns Close, including after EOF. AccountId at this port
// is the authoritative external account; only its opaque binding crosses into
// the worker. The supplied protobuf is never changed.
func (r *Runtime) OpenExecution(ctx context.Context, request *executionv1.BeginExecution) (dataplane.Execution, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "execution begin is required")
	}
	if err := r.validateRuntimeRequest(request.GetAccountId(), request.GetSlotId(), request.GetExecutionEpoch(),
		request.GetRouteGeneration(), request.GetRequestId(), request.GetMode(), request.GetAnthropicRequestJson(), request); err != nil {
		return nil, err
	}
	if len(request.GetSessionKey()) > 512 || !utf8.ValidString(request.GetSessionKey()) {
		return nil, status.Error(codes.InvalidArgument, "execution session key is invalid")
	}
	if err := validateRuntimeHeaders(request.GetRequestHeaders()); err != nil {
		return nil, err
	}
	begin := proto.Clone(request).(*executionv1.BeginExecution)
	begin.AccountId = r.identity.AccountID
	if proto.Size(begin) > dataplane.MaxMessageBytes {
		return nil, status.Error(codes.InvalidArgument, "execution request exceeds the size limit")
	}
	child, cancel := context.WithCancel(ctx)
	if err := child.Err(); err != nil {
		cancel()
		return nil, status.FromContextError(err).Err()
	}
	rawTicket, err := r.issue(child, "messages")
	if err != nil || rawTicket == "" || len(rawTicket) > 16<<10 {
		cancel()
		return nil, runtimeTicketError(err)
	}
	stream, err := r.client.Execute(child, grpc.MaxCallSendMsgSize(runtimeStreamWireBytes), grpc.MaxCallRecvMsgSize(runtimeStreamWireBytes))
	if err != nil {
		cancel()
		return nil, runtimeStreamError(err)
	}
	if err := stream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{Event: &executionv1.WorkerRuntimeServiceExecuteRequest_Begin{
		Begin: &executionv1.WorkerBeginExecution{ExecutionTicket: rawTicket, Request: begin},
	}}); err != nil {
		cancel()
		return nil, runtimeStreamError(err)
	}
	return &runtimeExecution{stream: stream, cancel: cancel}, nil
}

// CountTokensRequest preserves the selected mode and all binding/fencing fields.
// The legacy CountTokens convenience method remains a separate compatibility
// helper; new data-plane calls must use this explicit-request port.
func (r *Runtime) CountTokensRequest(ctx context.Context, request *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "count_tokens request is required")
	}
	if err := r.validateRuntimeRequest(request.GetAccountId(), request.GetSlotId(), request.GetExecutionEpoch(),
		request.GetRouteGeneration(), request.GetRequestId(), request.GetMode(), request.GetAnthropicRequestJson(), request); err != nil {
		return nil, err
	}
	cloned := proto.Clone(request).(*executionv1.CountTokensRequest)
	cloned.AccountId = r.identity.AccountID
	if proto.Size(cloned) > dataplane.MaxMessageBytes {
		return nil, status.Error(codes.InvalidArgument, "count_tokens request exceeds the size limit")
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := child.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	rawTicket, err := r.issue(child, "count_tokens")
	if err != nil || rawTicket == "" || len(rawTicket) > 16<<10 {
		return nil, runtimeTicketError(err)
	}
	response, err := r.client.CountTokens(child, &executionv1.WorkerRuntimeServiceCountTokensRequest{
		ExecutionTicket: rawTicket, Request: cloned,
	}, grpc.MaxCallSendMsgSize(runtimeStreamWireBytes), grpc.MaxCallRecvMsgSize(runtimeStreamWireBytes))
	if err != nil {
		return nil, runtimeStreamError(err)
	}
	if response == nil || response.GetResponse() == nil || proto.Size(response.GetResponse()) > dataplane.MaxMessageBytes {
		return nil, status.Error(codes.Internal, "worker returned an invalid count_tokens response")
	}
	return response.GetResponse(), nil
}

func (r *Runtime) validateRuntimeRequest(accountID, slotID string, epoch, generation uint64, requestID string,
	mode executionv1.ExecutionMode, body []byte, message proto.Message) error {
	if r == nil || r.client == nil || r.ticketSource == nil || r.identity.AccountID == "" ||
		r.identity.SlotID == "" || r.identity.NodeID == "" || r.identity.Epoch == 0 {
		return status.Error(codes.FailedPrecondition, "worker runtime is unavailable")
	}
	if !validRuntimeStreamID(accountID, 64) || !validRuntimeStreamID(requestID, 128) || !validRuntimeStreamID(slotID, 128) ||
		epoch == 0 || generation == 0 ||
		len(body) == 0 || len(body) > dataplane.MaxMessageBytes || proto.Size(message) > dataplane.MaxMessageBytes ||
		(mode != executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE && mode != executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API) {
		return status.Error(codes.InvalidArgument, "execution request is invalid")
	}
	if provider.RuntimeAccountID(accountID) != r.identity.AccountID || slotID != r.identity.SlotID || epoch != r.identity.Epoch {
		return status.Error(codes.PermissionDenied, "execution request does not match this worker")
	}
	return nil
}

func validRuntimeStreamID(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 33 || char == 127 {
			return false
		}
	}
	return strings.TrimSpace(value) == value
}

func validateRuntimeHeaders(headers map[string]string) error {
	allowed := map[string]bool{"accept": true, "anthropic-beta": true, "anthropic-version": true,
		"content-type": true, "user-agent": true, "x-request-id": true}
	seen := make(map[string]bool, len(allowed))
	total := 0
	for name, value := range headers {
		key := strings.ToLower(name)
		if !allowed[key] || seen[key] || !utf8.ValidString(value) {
			return status.Error(codes.InvalidArgument, "execution headers are invalid")
		}
		for _, char := range value {
			if char < 32 || char == 127 {
				return status.Error(codes.InvalidArgument, "execution headers are invalid")
			}
		}
		seen[key] = true
		total += len(name) + len(value)
		if total > dataplane.MaxHeaderBytes {
			return status.Error(codes.InvalidArgument, "execution headers exceed the size limit")
		}
	}
	return nil
}

type runtimeExecution struct {
	stream     executionv1.WorkerRuntimeService_ExecuteClient
	cancel     context.CancelFunc
	sendMu     sync.Mutex
	halfClosed bool
}

func (s *runtimeExecution) Recv() (*executionv1.ExecuteResponse, error) {
	response, err := s.stream.Recv()
	if err != nil {
		return nil, runtimeStreamError(err)
	}
	if response == nil || response.GetResponse() == nil || response.GetResponse().GetEvent() == nil ||
		proto.Size(response.GetResponse()) > dataplane.MaxMessageBytes {
		s.cancel()
		return nil, status.Error(codes.Internal, "worker returned an invalid execution event")
	}
	return response.GetResponse(), nil
}

func (s *runtimeExecution) SendToolResult(result *executionv1.ToolResult) error {
	if result == nil || !validRuntimeStreamID(result.GetToolUseId(), 128) ||
		len(result.GetContentJson()) == 0 || proto.Size(result) > dataplane.MaxMessageBytes {
		return status.Error(codes.InvalidArgument, "execution tool result is invalid")
	}
	cloned := proto.Clone(result).(*executionv1.ToolResult)
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.halfClosed {
		return status.Error(codes.FailedPrecondition, "worker execution send side is closed")
	}
	if err := s.stream.Context().Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	return runtimeStreamError(s.stream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{
		Event: &executionv1.WorkerRuntimeServiceExecuteRequest_ToolResult{ToolResult: cloned},
	}))
}

func (s *runtimeExecution) CloseSend() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if s.halfClosed {
		return nil
	}
	if err := s.stream.CloseSend(); err != nil {
		return runtimeStreamError(err)
	}
	s.halfClosed = true
	return nil
}

// Cancel outside the send lock: Send may be flow-control blocked and must be
// interruptible. Closing this request never closes the shared gRPC connection.
func (s *runtimeExecution) Close() error {
	s.cancel()
	return nil
}

func runtimeStreamError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.EOF
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "worker execution cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "worker execution deadline exceeded")
	}
	code := status.Code(err)
	if code == codes.Unknown || code == codes.OK {
		code = codes.Unavailable
	}
	return status.Error(code, "worker execution RPC failed")
}

func runtimeTicketError(err error) error {
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return status.Error(codes.Canceled, "execution ticket request cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return status.Error(codes.DeadlineExceeded, "execution ticket request deadline exceeded")
	}
	return status.Error(codes.Unavailable, "execution ticket is unavailable")
}
