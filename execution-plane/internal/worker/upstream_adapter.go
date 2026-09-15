package worker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker/upstream"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker/upstreamusage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The adapter owns authentication and RPC mapping. Transport and usage modules
// neither read credentials nor construct a route or retry a request.
type upstreamExecutor struct {
	state            processLifecycle
	credentialSource activeCredentialSource
	client           *http.Client
	baseURL          *url.URL
}

func (e *upstreamExecutor) Execute(stream ExecutionStream) error {
	if stream == nil || stream.Context() == nil || stream.Begin() == nil {
		return status.Error(codes.InvalidArgument, "execution begin is required")
	}
	if e == nil || e.state == nil || !e.state.Ready() {
		return status.Error(codes.FailedPrecondition, "worker is not ready")
	}
	begin := stream.Begin()
	if begin.GetMode() != executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API {
		return status.Error(codes.Unimplemented, "execution mode is not available")
	}
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	response, err := e.openRequest(ctx, "/v1/messages", begin.GetAnthropicRequestJson(), begin.GetRequestHeaders())
	if err != nil {
		return err
	}
	success := response.StatusCode >= 200 && response.StatusCode < 300
	streaming := upstream.IsStreaming(response)
	observer := upstreamusage.NewSSE()
	var usage []byte
	result, err := upstream.Relay(ctx, response, executionResponseSink{stream}, func(chunk []byte) {
		if streaming {
			observer.Feed(chunk)
		} else if success {
			// Unary observation is one complete bounded body, not arbitrary
			// streaming chunks. Missing/invalid usage remains absent.
			usage = upstreamusage.FromJSON(chunk)
		}
	})
	if err != nil {
		return upstreamRPCError(ctx, err)
	}
	if result.Streaming {
		if err := observer.Finish(); err != nil {
			return status.Error(codes.DataLoss, "upstream event stream did not complete safely")
		}
		usage = observer.UsageJSON()
	}
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	// Completed means the HTTP response ended, not HTTP 2xx success. Non-2xx
	// status/body remain visible to the caller and never acquire synthetic usage.
	if err := stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Completed{Completed: &executionv1.ExecutionCompleted{
		UpstreamRequestId: result.RequestID, UsageJson: usage,
	}}}); err != nil {
		return upstreamRPCError(ctx, upstream.ErrSend)
	}
	return nil
}

func (e *upstreamExecutor) CountTokens(ctx context.Context, input *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	if ctx == nil || input == nil {
		return nil, status.Error(codes.InvalidArgument, "count_tokens request is required")
	}
	if e == nil || e.state == nil || !e.state.Ready() {
		return nil, status.Error(codes.FailedPrecondition, "worker is not ready")
	}
	if input.GetMode() != executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API {
		return nil, status.Error(codes.Unimplemented, "execution mode is not available")
	}
	requestContext, cancel := context.WithCancel(ctx)
	defer cancel()
	response, err := e.openRequest(requestContext, "/v1/messages/count_tokens", input.GetAnthropicRequestJson(), nil)
	if err != nil {
		return nil, err
	}
	body, err := upstream.ReadUnary(requestContext, response)
	if err != nil {
		return nil, upstreamRPCError(requestContext, err)
	}
	return &executionv1.CountTokensResponse{
		StatusCode: int32(response.StatusCode), AnthropicResponseJson: body,
		UpstreamRequestId: response.Header.Get("X-Request-Id"),
	}, nil
}

func (e *upstreamExecutor) openRequest(ctx context.Context, path string, body []byte, headers map[string]string) (*http.Response, error) {
	if len(body) == 0 || len(body) > maxWorkerRequestBytes {
		return nil, status.Error(codes.InvalidArgument, "upstream request body size is invalid")
	}
	if e.client == nil || e.baseURL == nil {
		return nil, status.Error(codes.FailedPrecondition, "upstream client is unavailable")
	}
	endpoint := e.baseURL.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, status.Error(codes.Internal, "create upstream request failed")
	}
	// Prevent body replay. Redirect responses are returned to the caller, never
	// followed to another route with this account's credentials.
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "sub2api-execution-worker/1")
	copySafeRequestHeaders(request.Header, headers)
	if e.credentialSource != nil {
		active, err := e.credentialSource.ActiveCredential()
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, "worker credential is not active")
		}
		defer active.Destroy()
		if err := applyActiveCredential(request.Header, active); err != nil {
			return nil, status.Error(codes.FailedPrecondition, "worker credential is invalid")
		}
	}
	client := *e.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.Unavailable, "upstream request failed")
	}
	return response, nil
}

type executionResponseSink struct{ stream ExecutionStream }

func (s executionResponseSink) Headers(ctx context.Context, code int, headers map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Headers{Headers: &executionv1.ResponseHeaders{
		StatusCode: int32(code), Headers: headers,
	}}})
}

func (s executionResponseSink) Chunk(ctx context.Context, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Relay lends a reusable read buffer, while gRPC interceptors/stats may
	// retain a sent protobuf. Transfer an owned, bounded chunk to that boundary.
	owned := append([]byte(nil), body...)
	return s.stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_BodyChunk{BodyChunk: &executionv1.ResponseBodyChunk{Data: owned}}})
}

func upstreamRPCError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return status.FromContextError(err).Err()
	case errors.Is(err, upstream.ErrTooLarge):
		return status.Error(codes.ResourceExhausted, "upstream response exceeded size limit")
	case errors.Is(err, upstream.ErrEncoding):
		return status.Error(codes.Unimplemented, "upstream response encoding is not supported")
	default:
		return status.Error(codes.Unavailable, "upstream response transfer failed")
	}
}
