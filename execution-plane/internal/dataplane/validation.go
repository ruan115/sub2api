package dataplane

import (
	"strings"
	"unicode/utf8"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func validID(value string, limit int) bool {
	if value == "" || len(value) > limit || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if c < 33 || c == 127 {
			return false
		}
	}
	return true
}

func (s *Server) binding(account, slot string, epoch, generation uint64) (Binding, error) {
	if !validID(account, 64) || !validID(slot, 128) || epoch == 0 || generation == 0 {
		return Binding{}, invalidRequest()
	}
	return Binding{AccountID: account, SlotID: slot, NodeID: s.config.NodeID, ExecutionEpoch: epoch, RouteGeneration: generation}, nil
}

func validMode(mode executionv1.ExecutionMode) bool {
	return mode == executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE || mode == executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API
}

func invalidRequest() error {
	return status.Error(codes.InvalidArgument, "execution request is invalid")
}

func (s *Server) headers(input map[string]string) (map[string]string, error) {
	allowed := map[string]bool{"accept": true, "anthropic-beta": true, "anthropic-version": true, "content-type": true, "user-agent": true, "x-request-id": true}
	result := make(map[string]string, len(input))
	total := 0
	for raw, value := range input {
		name := strings.ToLower(raw)
		if !allowed[name] || raw != strings.TrimSpace(raw) || !utf8.ValidString(value) {
			return nil, invalidRequest()
		}
		if _, duplicate := result[name]; duplicate {
			return nil, invalidRequest()
		}
		for _, c := range value {
			if c < 32 || c == 127 {
				return nil, invalidRequest()
			}
		}
		total += len(name) + len(value)
		if total > s.config.MaxHeaderBytes {
			return nil, status.Error(codes.ResourceExhausted, "execution headers exceed limit")
		}
		result[name] = value
	}
	return result, nil
}

func (s *Server) begin(request *executionv1.ExecuteRequest) (*executionv1.BeginExecution, Binding, error) {
	if request == nil || proto.Size(request) > s.config.MaxMessageBytes {
		return nil, Binding{}, invalidRequest()
	}
	begin := request.GetBegin()
	if begin == nil || !validID(begin.GetRequestId(), 128) || !validMode(begin.GetMode()) || len(begin.GetAnthropicRequestJson()) == 0 ||
		len(begin.GetSessionKey()) > 512 || !utf8.ValidString(begin.GetSessionKey()) {
		return nil, Binding{}, invalidRequest()
	}
	binding, err := s.binding(begin.GetAccountId(), begin.GetSlotId(), begin.GetExecutionEpoch(), begin.GetRouteGeneration())
	if err != nil {
		return nil, Binding{}, err
	}
	headers, err := s.headers(begin.GetRequestHeaders())
	if err != nil {
		return nil, Binding{}, err
	}
	cloned := proto.Clone(begin).(*executionv1.BeginExecution)
	cloned.RequestHeaders = headers
	return cloned, binding, nil
}

func (s *Server) followup(request *executionv1.ExecuteRequest) error {
	if request == nil || proto.Size(request) > s.config.MaxMessageBytes {
		return invalidRequest()
	}
	if tool := request.GetToolResult(); tool != nil {
		if !validID(tool.GetToolUseId(), 128) || len(tool.GetContentJson()) == 0 {
			return invalidRequest()
		}
		return nil
	}
	if cancel := request.GetCancel(); cancel != nil {
		if len(cancel.GetReason()) > 256 {
			return invalidRequest()
		}
		return nil
	}
	return invalidRequest()
}

type responseState struct{ headers bool }

func (s *Server) response(response *executionv1.ExecuteResponse, state *responseState) (*executionv1.ExecuteResponse, bool, error) {
	bad := status.Error(codes.Internal, "worker response is invalid")
	if response == nil || proto.Size(response) > s.config.MaxMessageBytes {
		return nil, false, bad
	}
	switch event := response.Event.(type) {
	case *executionv1.ExecuteResponse_Headers:
		if event.Headers == nil || state.headers || event.Headers.StatusCode < 100 || event.Headers.StatusCode > 599 {
			return nil, false, bad
		}
		total := 0
		seen := map[string]bool{}
		for key, value := range event.Headers.Headers {
			name := strings.ToLower(key)
			if (name != "content-type" && name != "x-request-id") || seen[name] || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
				return nil, false, bad
			}
			seen[name] = true
			total += len(key) + len(value)
			if total > s.config.MaxHeaderBytes {
				return nil, false, bad
			}
		}
		state.headers = true
	case *executionv1.ExecuteResponse_BodyChunk:
		if event.BodyChunk == nil || !state.headers || len(event.BodyChunk.Data) == 0 {
			return nil, false, bad
		}
	case *executionv1.ExecuteResponse_ToolUse:
		if event.ToolUse == nil || len(event.ToolUse.ToolUseBlocksJson) == 0 || len(event.ToolUse.ResumeToken) > 4096 {
			return nil, false, bad
		}
	case *executionv1.ExecuteResponse_Completed:
		if event.Completed == nil || len(event.Completed.UpstreamRequestId) > 256 {
			return nil, false, bad
		}
		return response, true, nil
	case *executionv1.ExecuteResponse_Error:
		if event.Error == nil {
			return nil, false, bad
		}
		return &executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Error{Error: &executionv1.ExecutionError{
			Code: "worker_execution_failed", Message: "worker execution failed", Retryable: event.Error.Retryable, ResponseStarted: state.headers,
		}}}, true, nil
	default:
		return nil, false, bad
	}
	return response, false, nil
}
