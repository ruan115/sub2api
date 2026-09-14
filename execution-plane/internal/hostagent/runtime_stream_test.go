package hostagent

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
)

type streamTicketRecorder struct {
	mu       sync.Mutex
	requests []TicketRequest
	err      error
}

func (s *streamTicketRecorder) Issue(_ context.Context, request TicketRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, request)
	return "synthetic-stream-ticket", s.err
}

func (s *streamTicketRecorder) snapshot() []TicketRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TicketRequest(nil), s.requests...)
}

type streamAdapterWorker struct {
	executionv1.UnimplementedWorkerRuntimeServiceServer
	execute func(grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error
	count   func(context.Context, *executionv1.WorkerRuntimeServiceCountTokensRequest) (*executionv1.WorkerRuntimeServiceCountTokensResponse, error)
}

func (s *streamAdapterWorker) Execute(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
	if s.execute == nil {
		return status.Error(codes.Unimplemented, "synthetic execution unavailable")
	}
	return s.execute(stream)
}

func (s *streamAdapterWorker) CountTokens(ctx context.Context, request *executionv1.WorkerRuntimeServiceCountTokensRequest) (*executionv1.WorkerRuntimeServiceCountTokensResponse, error) {
	if s.count == nil {
		return nil, status.Error(codes.Unimplemented, "synthetic count unavailable")
	}
	return s.count(ctx, request)
}

func streamRuntimeFixture(t *testing.T, backend *streamAdapterWorker, tickets *streamTicketRecorder) *Runtime {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.MaxRecvMsgSize(runtimeStreamWireBytes), grpc.MaxSendMsgSize(runtimeStreamWireBytes))
	executionv1.RegisterWorkerRuntimeServiceServer(server, backend)
	go func() { _ = server.Serve(listener) }()
	connection, err := grpc.NewClient("passthrough:///runtime-stream-fixture",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
	if err != nil {
		server.Stop()
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = connection.Close()
		server.Stop()
		_ = listener.Close()
	})
	return &Runtime{client: executionv1.NewWorkerRuntimeServiceClient(connection), connection: connection,
		ticketSource: tickets, identity: runtimeIdentity{
			AccountID: provider.RuntimeAccountID("account-stream"), SlotID: "slot-stream", NodeID: "node-stream", Epoch: 7,
		}}
}

func streamBeginFixture() *executionv1.BeginExecution {
	return &executionv1.BeginExecution{
		RequestId: "request-stream", AccountId: "account-stream", SlotId: "slot-stream",
		ExecutionEpoch: 7, RouteGeneration: 9, Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE,
		SessionKey: "session-stream", AnthropicRequestJson: []byte(`{"messages":[],"stream":true}`),
		RequestHeaders: map[string]string{"anthropic-version": "2023-06-01", "user-agent": "synthetic-client"},
	}
}

func streamCountFixture() *executionv1.CountTokensRequest {
	return &executionv1.CountTokensRequest{
		RequestId: "count-stream", AccountId: "account-stream", SlotId: "slot-stream",
		ExecutionEpoch: 7, RouteGeneration: 9, Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE,
		AnthropicRequestJson: []byte(`{"messages":[]}`),
	}
}

func streamTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func awaitStreamValue[T any](t *testing.T, ctx context.Context, channel <-chan T) T {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-ctx.Done():
		t.Fatalf("synthetic stream did not progress: %v", ctx.Err())
		var zero T
		return zero
	}
}

func sendStreamBody(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse], body string) error {
	return stream.Send(&executionv1.WorkerRuntimeServiceExecuteResponse{Response: &executionv1.ExecuteResponse{
		Event: &executionv1.ExecuteResponse_BodyChunk{BodyChunk: &executionv1.ResponseBodyChunk{Data: []byte(body)}},
	}})
}

func sendStreamCompleted(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
	return stream.Send(&executionv1.WorkerRuntimeServiceExecuteResponse{Response: &executionv1.ExecuteResponse{
		Event: &executionv1.ExecuteResponse_Completed{Completed: &executionv1.ExecutionCompleted{UpstreamRequestId: "synthetic-upstream"}},
	}})
}

func TestRuntimeOpenExecutionStreamsBeforeCompletionAndClonesBinding(t *testing.T) {
	ctx := streamTestContext(t)
	received := make(chan *executionv1.WorkerBeginExecution, 1)
	finish := make(chan struct{})
	tickets := &streamTicketRecorder{}
	backend := &streamAdapterWorker{execute: func(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
		first, err := stream.Recv()
		if err != nil {
			return err
		}
		received <- first.GetBegin()
		if err := sendStreamBody(stream, "early-chunk"); err != nil {
			return err
		}
		select {
		case <-finish:
			return sendStreamCompleted(stream)
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}}
	runtime := streamRuntimeFixture(t, backend, tickets)
	request := streamBeginFixture()
	before := proto.Clone(request)
	stream, err := runtime.OpenExecution(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	first, err := stream.Recv()
	if err != nil || string(first.GetBodyChunk().GetData()) != "early-chunk" {
		t.Fatalf("first event before allowing completion = %v, %v", first, err)
	}
	workerBegin := awaitStreamValue(t, ctx, received)
	want := proto.Clone(request).(*executionv1.BeginExecution)
	want.AccountId = runtime.identity.AccountID
	if workerBegin.GetExecutionTicket() != "synthetic-stream-ticket" || !proto.Equal(workerBegin.GetRequest(), want) {
		t.Fatal("worker begin lost selected mode, request fields, or ticket")
	}
	if !proto.Equal(request, before) {
		t.Fatal("caller begin request was mutated")
	}
	issued := tickets.snapshot()
	if len(issued) != 1 || issued[0] != (TicketRequest{AccountID: runtime.identity.AccountID, SlotID: "slot-stream", NodeID: "node-stream", Epoch: 7, Scope: "messages"}) {
		t.Fatalf("ticket calls = %+v", issued)
	}
	close(finish)
	last, err := stream.Recv()
	if err != nil || last.GetCompleted() == nil {
		t.Fatalf("completed = %v, %v", last, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("EOF = %v", err)
	}
}

func TestRuntimeExecutionToolResultsAndHalfCloseDoNotCancelResponse(t *testing.T) {
	ctx := streamTestContext(t)
	tools := make(chan *executionv1.ToolResult, 1)
	halfClosed := make(chan bool, 1)
	backend := &streamAdapterWorker{execute: func(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		tool, err := stream.Recv()
		if err != nil {
			return err
		}
		tools <- tool.GetToolResult()
		_, err = stream.Recv()
		halfClosed <- errors.Is(err, io.EOF) && stream.Context().Err() == nil
		if !errors.Is(err, io.EOF) {
			return err
		}
		if err := sendStreamBody(stream, "after-half-close"); err != nil {
			return err
		}
		return sendStreamCompleted(stream)
	}}
	runtime := streamRuntimeFixture(t, backend, &streamTicketRecorder{})
	stream, err := runtime.OpenExecution(ctx, streamBeginFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	tool := &executionv1.ToolResult{ToolUseId: "tool-one", ContentJson: []byte(`{"text":"synthetic"}`), IsError: true}
	before := proto.Clone(tool)
	if err := stream.SendToolResult(tool); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(tool, before) {
		t.Fatal("caller tool result changed")
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if !awaitStreamValue(t, ctx, halfClosed) {
		t.Fatal("half-close cancelled worker context")
	}
	if !proto.Equal(awaitStreamValue(t, ctx, tools), before) {
		t.Fatal("tool result changed across worker RPC")
	}
	response, err := stream.Recv()
	if err != nil || string(response.GetBodyChunk().GetData()) != "after-half-close" {
		t.Fatalf("response = %v, %v", response, err)
	}
	if err := stream.SendToolResult(tool); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("send after half-close = %v", err)
	}
	response, err = stream.Recv()
	if err != nil || response.GetCompleted() == nil {
		t.Fatalf("completion = %v, %v", response, err)
	}
}

func TestRuntimeExecutionCloseCancelsOnlyItsRPC(t *testing.T) {
	ctx := streamTestContext(t)
	cancelled := make(chan struct{}, 1)
	backend := &streamAdapterWorker{
		execute: func(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			if err := sendStreamBody(stream, "started"); err != nil {
				return err
			}
			<-stream.Context().Done()
			cancelled <- struct{}{}
			return stream.Context().Err()
		},
		count: func(context.Context, *executionv1.WorkerRuntimeServiceCountTokensRequest) (*executionv1.WorkerRuntimeServiceCountTokensResponse, error) {
			return &executionv1.WorkerRuntimeServiceCountTokensResponse{Response: &executionv1.CountTokensResponse{StatusCode: 200, AnthropicResponseJson: []byte(`{"input_tokens":1}`)}}, nil
		},
	}
	runtime := streamRuntimeFixture(t, backend, &streamTicketRecorder{})
	stream, err := runtime.OpenExecution(ctx, streamBeginFixture())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("closed receive = %v", err)
	}
	awaitStreamValue(t, ctx, cancelled)
	if _, err := runtime.CountTokensRequest(ctx, streamCountFixture()); err != nil {
		t.Fatalf("shared worker connection was closed: %v", err)
	}
}

func TestRuntimeExecutionParentCancellationReachesWorker(t *testing.T) {
	outer := streamTestContext(t)
	ctx, cancel := context.WithCancel(outer)
	defer cancel()
	cancelled := make(chan struct{}, 1)
	backend := &streamAdapterWorker{execute: func(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if err := sendStreamBody(stream, "started"); err != nil {
			return err
		}
		<-stream.Context().Done()
		cancelled <- struct{}{}
		return stream.Context().Err()
	}}
	runtime := streamRuntimeFixture(t, backend, &streamTicketRecorder{})
	stream, err := runtime.OpenExecution(ctx, streamBeginFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	awaitStreamValue(t, outer, cancelled)
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("parent-cancel receive = %v", err)
	}
}

func TestRuntimeExecutionRejectsInvalidBindingBeforeTicket(t *testing.T) {
	tickets := &streamTicketRecorder{}
	runtime := streamRuntimeFixture(t, &streamAdapterWorker{}, tickets)
	mutations := []struct {
		name   string
		mutate func(*executionv1.BeginExecution)
		code   codes.Code
	}{
		{"account", func(r *executionv1.BeginExecution) { r.AccountId = "another-account" }, codes.PermissionDenied},
		{"slot", func(r *executionv1.BeginExecution) { r.SlotId = "another-slot" }, codes.PermissionDenied},
		{"epoch", func(r *executionv1.BeginExecution) { r.ExecutionEpoch++ }, codes.PermissionDenied},
		{"generation", func(r *executionv1.BeginExecution) { r.RouteGeneration = 0 }, codes.InvalidArgument},
		{"mode", func(r *executionv1.BeginExecution) { r.Mode = executionv1.ExecutionMode_EXECUTION_MODE_UNSPECIFIED }, codes.InvalidArgument},
		{"request-id", func(r *executionv1.BeginExecution) { r.RequestId = "" }, codes.InvalidArgument},
		{"body", func(r *executionv1.BeginExecution) { r.AnthropicRequestJson = nil }, codes.InvalidArgument},
		{"authorization", func(r *executionv1.BeginExecution) { r.RequestHeaders["authorization"] = "synthetic-secret" }, codes.InvalidArgument},
		{"header-injection", func(r *executionv1.BeginExecution) { r.RequestHeaders["user-agent"] = "a\r\nb" }, codes.InvalidArgument},
		{"duplicate-header", func(r *executionv1.BeginExecution) { r.RequestHeaders["User-Agent"] = "second" }, codes.InvalidArgument},
		{"header-size", func(r *executionv1.BeginExecution) {
			r.RequestHeaders["user-agent"] = strings.Repeat("x", dataplane.MaxHeaderBytes)
		}, codes.InvalidArgument},
		{"body-size", func(r *executionv1.BeginExecution) {
			r.AnthropicRequestJson = make([]byte, dataplane.MaxMessageBytes+1)
		}, codes.InvalidArgument},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			request := streamBeginFixture()
			test.mutate(request)
			if _, err := runtime.OpenExecution(streamTestContext(t), request); status.Code(err) != test.code {
				t.Fatalf("rejection = %v; want %v", err, test.code)
			}
		})
	}
	if _, err := runtime.OpenExecution(context.Background(), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil begin = %v", err)
	}
	if len(tickets.snapshot()) != 0 {
		t.Fatal("rejected requests acquired tickets")
	}
}

func TestRuntimeCountTokensPreservesModeAndRequestAndSanitizesErrors(t *testing.T) {
	received := make(chan *executionv1.WorkerRuntimeServiceCountTokensRequest, 2)
	tickets := &streamTicketRecorder{}
	backend := &streamAdapterWorker{count: func(_ context.Context, request *executionv1.WorkerRuntimeServiceCountTokensRequest) (*executionv1.WorkerRuntimeServiceCountTokensResponse, error) {
		received <- request
		if request.GetRequest().GetRequestId() == "fail" {
			return nil, status.Error(codes.PermissionDenied, "sensitive backend detail")
		}
		return &executionv1.WorkerRuntimeServiceCountTokensResponse{Response: &executionv1.CountTokensResponse{StatusCode: 200, AnthropicResponseJson: []byte(`{"input_tokens":2}`)}}, nil
	}}
	runtime := streamRuntimeFixture(t, backend, tickets)
	ctx := streamTestContext(t)
	request := streamCountFixture()
	before := proto.Clone(request)
	response, err := runtime.CountTokensRequest(ctx, request)
	if err != nil || response.GetStatusCode() != 200 {
		t.Fatalf("count response = %v, %v", response, err)
	}
	got := awaitStreamValue(t, ctx, received)
	want := proto.Clone(request).(*executionv1.CountTokensRequest)
	want.AccountId = runtime.identity.AccountID
	if !proto.Equal(got.GetRequest(), want) || got.GetExecutionTicket() != "synthetic-stream-ticket" {
		t.Fatal("count request fields changed")
	}
	if !proto.Equal(request, before) {
		t.Fatal("caller count request changed")
	}
	if issued := tickets.snapshot(); len(issued) != 1 || issued[0].Scope != "count_tokens" {
		t.Fatalf("tickets = %+v", issued)
	}
	request.RequestId = "fail"
	if _, err := runtime.CountTokensRequest(ctx, request); status.Code(err) != codes.PermissionDenied || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("count error not sanitized = %v", err)
	}
}

func TestRuntimeCountRejectsBindingAndCancelledRequestBeforeTicket(t *testing.T) {
	tickets := &streamTicketRecorder{}
	runtime := streamRuntimeFixture(t, &streamAdapterWorker{}, tickets)
	request := streamCountFixture()
	request.AccountId = "other-account"
	if _, err := runtime.CountTokensRequest(context.Background(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("binding rejection = %v", err)
	}
	if _, err := runtime.CountTokensRequest(context.Background(), nil); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("nil count = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runtime.CountTokensRequest(ctx, streamCountFixture()); status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled count = %v", err)
	}
	if _, err := runtime.OpenExecution(ctx, streamBeginFixture()); status.Code(err) != codes.Canceled {
		t.Fatalf("cancelled open = %v", err)
	}
	if len(tickets.snapshot()) != 0 {
		t.Fatal("invalid/cancelled requests acquired tickets")
	}
}

func TestRuntimeExecutionConcurrentSendsAreSerialized(t *testing.T) {
	ctx := streamTestContext(t)
	const total = 12
	backend := &streamAdapterWorker{execute: func(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		for range total {
			message, err := stream.Recv()
			if err != nil {
				return err
			}
			if message.GetToolResult() == nil {
				return status.Error(codes.InvalidArgument, "expected synthetic tool")
			}
		}
		if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "expected half close")
		}
		return sendStreamCompleted(stream)
	}}
	runtime := streamRuntimeFixture(t, backend, &streamTicketRecorder{})
	stream, err := runtime.OpenExecution(ctx, streamBeginFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	errorsFound := make(chan error, total)
	var group sync.WaitGroup
	for range total {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsFound <- stream.SendToolResult(&executionv1.ToolResult{ToolUseId: "synthetic-tool", ContentJson: []byte(`{}`)})
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	response, err := stream.Recv()
	if err != nil || response.GetCompleted() == nil {
		t.Fatalf("completion = %v, %v", response, err)
	}
}

func TestRuntimeExecutionTicketErrorsNeverExposeControlDetails(t *testing.T) {
	tickets := &streamTicketRecorder{err: errors.New("sensitive control ticket detail")}
	runtime := streamRuntimeFixture(t, &streamAdapterWorker{}, tickets)
	if _, err := runtime.OpenExecution(context.Background(), streamBeginFixture()); status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("ticket error = %v", err)
	}
	if _, err := runtime.CountTokensRequest(context.Background(), streamCountFixture()); status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("count ticket error = %v", err)
	}
}

func TestLegacyRuntimeHelpersRequireExplicitGeneration(t *testing.T) {
	tickets := &streamTicketRecorder{}
	runtime := streamRuntimeFixture(t, &streamAdapterWorker{}, tickets)
	if _, err := runtime.Execute(context.Background(), "request", []byte(`{}`), 0); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("legacy execute zero generation = %v", err)
	}
	if _, err := runtime.CountTokens(context.Background(), "request", []byte(`{}`), 0); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("legacy count zero generation = %v", err)
	}
	if len(tickets.snapshot()) != 0 {
		t.Fatal("zero generation acquired tickets")
	}
}

func TestRuntimeExecutionRejectsMalformedWorkerEnvelope(t *testing.T) {
	ctx := streamTestContext(t)
	backend := &streamAdapterWorker{execute: func(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		return stream.Send(&executionv1.WorkerRuntimeServiceExecuteResponse{})
	}}
	runtime := streamRuntimeFixture(t, backend, &streamTicketRecorder{})
	stream, err := runtime.OpenExecution(ctx, streamBeginFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Recv(); status.Code(err) != codes.Internal {
		t.Fatalf("malformed envelope = %v", err)
	}
}

func TestRuntimeExecutionInvalidToolResultsNeverReachWorker(t *testing.T) {
	ctx := streamTestContext(t)
	backend := &streamAdapterWorker{execute: func(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
		if _, err := stream.Recv(); err != nil {
			return err
		}
		if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
			return status.Error(codes.Internal, "unexpected tool result")
		}
		return sendStreamCompleted(stream)
	}}
	runtime := streamRuntimeFixture(t, backend, &streamTicketRecorder{})
	stream, err := runtime.OpenExecution(ctx, streamBeginFixture())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for _, invalid := range []*executionv1.ToolResult{nil, {}, {ToolUseId: "tool"}, {ToolUseId: "tool\nname", ContentJson: []byte(`{}`)}} {
		if err := stream.SendToolResult(invalid); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("invalid tool = %v", err)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	result, err := stream.Recv()
	if err != nil || result.GetCompleted() == nil {
		t.Fatalf("invalid tool reached worker = %v, %v", result, err)
	}
}

func TestRuntimeErrorSanitizationIncludesWrappedCancellation(t *testing.T) {
	for _, input := range []error{
		errors.Join(errors.New("sensitive execution detail"), context.Canceled),
		errors.Join(errors.New("sensitive execution detail"), context.DeadlineExceeded),
		status.Error(codes.PermissionDenied, "sensitive execution detail"),
	} {
		if got := runtimeStreamError(input); strings.Contains(got.Error(), "sensitive") {
			t.Fatalf("stream error leaked: %v", got)
		}
		if got := runtimeTicketError(input); strings.Contains(got.Error(), "sensitive") {
			t.Fatalf("ticket error leaked: %v", got)
		}
	}
	if got := runtimeStreamError(errors.Join(errors.New("sensitive EOF detail"), io.EOF)); got != io.EOF {
		t.Fatalf("wrapped EOF not canonicalized: %v", got)
	}
}
