package dataplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// This fabricates a transport result for method-level unit tests only. The
// two-hop integration tests separately exercise actual certificate handshakes.
func serverTestPeer(service string) context.Context {
	leaf := &x509.Certificate{Raw: []byte("synthetic-leaf"), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		URIs: []*url.URL{{Scheme: "spiffe", Host: "sub2api.execution", Path: "/service/" + service}}}
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
		Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}},
	}}})
}

type serverTestResolver struct {
	runtime  Runtime
	resolves atomic.Int32
	validate func(context.Context, Binding) error
}

func (r *serverTestResolver) Resolve(ctx context.Context, b Binding) (Runtime, error) {
	r.resolves.Add(1)
	if r.validate != nil {
		if err := r.validate(ctx, b); err != nil {
			return nil, err
		}
	}
	return r.runtime, nil
}
func (r *serverTestResolver) Validate(ctx context.Context, b Binding) error {
	if r.validate != nil {
		return r.validate(ctx, b)
	}
	return nil
}

type serverTestRuntime struct {
	execution *serverTestExecution
	begin     *executionv1.BeginExecution
	count     func(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error)
	open      func(context.Context, *executionv1.BeginExecution) (Execution, error)
}

func (r *serverTestRuntime) OpenExecution(ctx context.Context, b *executionv1.BeginExecution) (Execution, error) {
	if r.open != nil {
		return r.open(ctx, b)
	}
	r.begin = b
	r.execution.ctx, r.execution.cancel = context.WithCancel(ctx)
	return r.execution, nil
}
func (r *serverTestRuntime) CountTokensRequest(ctx context.Context, b *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	return r.count(ctx, b)
}

type serverTestExecution struct {
	ctx        context.Context
	cancel     context.CancelFunc
	events     chan outbound
	tools      chan *executionv1.ToolResult
	closed     chan struct{}
	once       sync.Once
	halfClosed atomic.Bool
}

func newServerTestExecution() *serverTestExecution {
	return &serverTestExecution{events: make(chan outbound, 8), tools: make(chan *executionv1.ToolResult, 1), closed: make(chan struct{})}
}
func (e *serverTestExecution) Recv() (*executionv1.ExecuteResponse, error) {
	select {
	case <-e.ctx.Done():
		return nil, e.ctx.Err()
	case r := <-e.events:
		return r.response, r.err
	}
}
func (e *serverTestExecution) SendToolResult(t *executionv1.ToolResult) error {
	select {
	case <-e.ctx.Done():
		return e.ctx.Err()
	case e.tools <- t:
		return nil
	}
}
func (e *serverTestExecution) CloseSend() error { e.halfClosed.Store(true); return nil }
func (e *serverTestExecution) Close() error {
	e.once.Do(func() { e.cancel(); close(e.closed) })
	return nil
}

type serverTestStream struct {
	ctx      context.Context
	input    chan *executionv1.ExecuteRequest
	output   chan *executionv1.ExecuteResponse
	sendGate chan struct{}
}

func (s *serverTestStream) Context() context.Context { return s.ctx }
func (s *serverTestStream) Recv() (*executionv1.ExecuteRequest, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case r, ok := <-s.input:
		if !ok {
			return nil, io.EOF
		}
		return r, nil
	}
}
func (s *serverTestStream) Send(r *executionv1.ExecuteResponse) error {
	if s.sendGate != nil {
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-s.sendGate:
		}
	}
	select {
	case <-s.ctx.Done():
		return s.ctx.Err()
	case s.output <- r:
		return nil
	}
}
func (*serverTestStream) SetHeader(metadata.MD) error  { return nil }
func (*serverTestStream) SendHeader(metadata.MD) error { return nil }
func (*serverTestStream) SetTrailer(metadata.MD)       {}
func (*serverTestStream) SendMsg(any) error            { return nil }
func (*serverTestStream) RecvMsg(any) error            { return nil }

func validServerTestBegin() *executionv1.ExecuteRequest {
	return &executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_Begin{Begin: &executionv1.BeginExecution{
		RequestId: "request-1", AccountId: "account-1", SlotId: "slot-1", ExecutionEpoch: 3, RouteGeneration: 4,
		Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, AnthropicRequestJson: []byte(`{"model":"synthetic"}`), RequestHeaders: map[string]string{"Content-Type": "application/json"},
	}}}
}
func serverTestHeaders() *executionv1.ExecuteResponse {
	return &executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Headers{Headers: &executionv1.ResponseHeaders{StatusCode: 200, Headers: map[string]string{"content-type": "text/event-stream"}}}}
}
func serverTestCompleted() *executionv1.ExecuteResponse {
	return &executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Completed{Completed: &executionv1.ExecutionCompleted{UpstreamRequestId: "synthetic"}}}
}
func startServerTest(t *testing.T, s *Server, ctx context.Context) (*serverTestStream, <-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	stream := &serverTestStream{ctx: ctx, input: make(chan *executionv1.ExecuteRequest, 8), output: make(chan *executionv1.ExecuteResponse, 8)}
	done := make(chan error, 1)
	go func() { err := s.Execute(stream); cancel(); done <- err }()
	return stream, done, cancel
}
func awaitServerTest(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not terminate")
		return nil
	}
}
func testServer(t *testing.T, r Resolver) *Server {
	t.Helper()
	s, err := NewServer(Config{NodeID: "node-1", Resolver: r, FenceInterval: 5 * time.Millisecond, MaxExecutionDuration: time.Second, FirstFrameTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDataPlaneRejectsUnauthenticatedPeersBeforeReadingOrResolving(t *testing.T) {
	for _, ctx := range []context.Context{context.Background(), serverTestPeer("other-service")} {
		r := &serverTestResolver{}
		_, done, _ := startServerTest(t, testServer(t, r), ctx)
		if err := awaitServerTest(t, done); status.Code(err) != codes.PermissionDenied || r.resolves.Load() != 0 {
			t.Fatalf("code=%v resolves=%d", status.Code(err), r.resolves.Load())
		}
	}
	ctx := serverTestPeer("ccmax")
	p, _ := peer.FromContext(ctx)
	tlsInfo := p.AuthInfo.(credentials.TLSInfo)
	for _, mutate := range []func(*tls.ConnectionState){func(s *tls.ConnectionState) { s.Version = tls.VersionTLS12 }, func(s *tls.ConnectionState) { s.VerifiedChains = nil }, func(s *tls.ConnectionState) { s.VerifiedChains = [][]*x509.Certificate{{{Raw: []byte("other-leaf")}}} }, func(s *tls.ConnectionState) {
		leaf := *s.PeerCertificates[0]
		leaf.URIs = []*url.URL{{Scheme: "spiffe", Host: "sub2api.execution", Path: "/node/node-1"}}
		s.PeerCertificates = []*x509.Certificate{&leaf}
		s.VerifiedChains = [][]*x509.Certificate{{&leaf}}
	}} {
		state := tlsInfo.State
		mutate(&state)
		bad := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: state}})
		if status.Code(authorize(bad)) != codes.PermissionDenied {
			t.Fatal("unverified peer was accepted")
		}
	}
}

func TestDataPlaneRejectsInvalidBeginBeforeResolve(t *testing.T) {
	for _, mutate := range []func(*executionv1.ExecuteRequest){
		func(r *executionv1.ExecuteRequest) {
			r.Event = &executionv1.ExecuteRequest_Cancel{Cancel: &executionv1.CancelExecution{}}
		},
		func(r *executionv1.ExecuteRequest) { r.GetBegin().ExecutionEpoch = 0 }, func(r *executionv1.ExecuteRequest) { r.GetBegin().RouteGeneration = 0 },
		func(r *executionv1.ExecuteRequest) { r.GetBegin().AccountId = " " }, func(r *executionv1.ExecuteRequest) {
			r.GetBegin().RequestHeaders = map[string]string{"Authorization": "synthetic-secret"}
		},
		func(r *executionv1.ExecuteRequest) {
			r.GetBegin().RequestHeaders = map[string]string{"Accept": "a", "accept": "b"}
		},
		func(r *executionv1.ExecuteRequest) {
			r.GetBegin().RequestHeaders = map[string]string{"accept": "a\r\nb"}
		},
		func(r *executionv1.ExecuteRequest) { r.GetBegin().AnthropicRequestJson = make([]byte, 1024) },
	} {
		r := &serverTestResolver{}
		s := testServer(t, r)
		s.config.MaxMessageBytes = 512
		stream, done, _ := startServerTest(t, s, serverTestPeer("ccmax"))
		request := validServerTestBegin()
		mutate(request)
		stream.input <- request
		if err := awaitServerTest(t, done); status.Code(err) != codes.InvalidArgument || r.resolves.Load() != 0 {
			t.Fatalf("code=%v resolves=%d", status.Code(err), r.resolves.Load())
		}
	}
}

func TestDataPlaneStreamsBeforeCompletionAndAcceptsToolsAndHalfClose(t *testing.T) {
	e := newServerTestExecution()
	runtime := &serverTestRuntime{execution: e}
	resolver := &serverTestResolver{runtime: runtime}
	stream, done, _ := startServerTest(t, testServer(t, resolver), serverTestPeer("ccmax"))
	stream.input <- validServerTestBegin()
	e.events <- outbound{response: serverTestHeaders()}
	e.events <- outbound{response: &executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_BodyChunk{BodyChunk: &executionv1.ResponseBodyChunk{Data: []byte("first")}}}}
	for i := 0; i < 2; i++ {
		select {
		case <-stream.output:
		case <-time.After(time.Second):
			t.Fatal("first events were buffered until completion")
		}
	}
	stream.input <- &executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_ToolResult{ToolResult: &executionv1.ToolResult{ToolUseId: "tool-1", ContentJson: []byte(`{}`)}}}
	select {
	case tool := <-e.tools:
		if tool.ToolUseId != "tool-1" {
			t.Fatal("tool mismatch")
		}
	case <-time.After(time.Second):
		t.Fatal("tool not forwarded")
	}
	close(stream.input)
	deadline := time.After(time.Second)
	for !e.halfClosed.Load() {
		select {
		case <-deadline:
			t.Fatal("half close not forwarded")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("half close terminated execution: %v", err)
	default:
	}
	e.events <- outbound{response: serverTestCompleted()}
	if err := awaitServerTest(t, done); err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.closed:
	default:
		t.Fatal("terminal event did not close worker")
	}
	if runtime.begin.AccountId != "account-1" || runtime.begin.RequestHeaders["content-type"] != "application/json" {
		t.Fatal("external begin not normalized")
	}
}

func TestDataPlaneCancelInvalidFollowupAndMissingTerminalCloseWorker(t *testing.T) {
	for _, name := range []string{"cancel", "duplicate-begin", "empty", "eof"} {
		t.Run(name, func(t *testing.T) {
			e := newServerTestExecution()
			stream, done, _ := startServerTest(t, testServer(t, &serverTestResolver{runtime: &serverTestRuntime{execution: e}}), serverTestPeer("ccmax"))
			stream.input <- validServerTestBegin()
			want := codes.InvalidArgument
			switch name {
			case "cancel":
				want = codes.Canceled
				stream.input <- &executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_Cancel{Cancel: &executionv1.CancelExecution{Reason: "synthetic"}}}
			case "duplicate-begin":
				stream.input <- validServerTestBegin()
			case "empty":
				stream.input <- &executionv1.ExecuteRequest{}
			case "eof":
				want = codes.Unavailable
				e.events <- outbound{err: io.EOF}
			}
			if err := awaitServerTest(t, done); status.Code(err) != want {
				t.Fatalf("code=%v want=%v", status.Code(err), want)
			}
			select {
			case <-e.closed:
			default:
				t.Fatal("worker was not canceled")
			}
		})
	}
}

func TestDataPlaneFenceInvalidationTerminatesBlockedDownstreamSend(t *testing.T) {
	e := newServerTestExecution()
	var revoked atomic.Bool
	r := &serverTestResolver{runtime: &serverTestRuntime{execution: e}, validate: func(context.Context, Binding) error {
		if revoked.Load() {
			return errors.New("private-authority-detail")
		}
		return nil
	}}
	s := testServer(t, r)
	ctx, cancel := context.WithCancel(serverTestPeer("ccmax"))
	defer cancel()
	stream := &serverTestStream{ctx: ctx, input: make(chan *executionv1.ExecuteRequest, 1), output: make(chan *executionv1.ExecuteResponse), sendGate: make(chan struct{})}
	done := make(chan error, 1)
	go func() { err := s.Execute(stream); cancel(); done <- err }()
	stream.input <- validServerTestBegin()
	e.events <- outbound{response: serverTestHeaders()}
	for r.resolves.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	revoked.Store(true)
	err := awaitServerTest(t, done)
	if status.Code(err) != codes.FailedPrecondition || strings.Contains(err.Error(), "private-authority-detail") {
		t.Fatalf("unexpected error %v", err)
	}
	select {
	case <-e.closed:
	default:
		t.Fatal("worker not closed while downstream stalled")
	}
}

func TestDataPlaneBoundsFirstFrameAndLeaseRead(t *testing.T) {
	s := testServer(t, &serverTestResolver{})
	_, done, _ := startServerTest(t, s, serverTestPeer("ccmax"))
	if err := awaitServerTest(t, done); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal(err)
	}
	e := newServerTestExecution()
	var validations atomic.Int32
	r := &serverTestResolver{runtime: &serverTestRuntime{execution: e}, validate: func(ctx context.Context, _ Binding) error {
		if validations.Add(1) > 1 {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	stream, done, _ := startServerTest(t, testServer(t, r), serverTestPeer("ccmax"))
	stream.input <- validServerTestBegin()
	if err := awaitServerTest(t, done); status.Code(err) != codes.FailedPrecondition {
		t.Fatal(err)
	}
}

func TestDataPlaneCountTokensFencesAndRedacts(t *testing.T) {
	request := &executionv1.CountTokensRequest{RequestId: "request-1", AccountId: "account-1", SlotId: "slot-1", ExecutionEpoch: 3, RouteGeneration: 4, Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, AnthropicRequestJson: []byte(`{}`)}
	runtime := &serverTestRuntime{count: func(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
		return &executionv1.CountTokensResponse{StatusCode: 200, AnthropicResponseJson: []byte(`{"input_tokens":1}`)}, nil
	}}
	r := &serverTestResolver{runtime: runtime}
	s := testServer(t, r)
	if response, err := s.CountTokens(serverTestPeer("ccmax"), request); err != nil || response.StatusCode != 200 {
		t.Fatalf("response=%v err=%v", response, err)
	}
	runtime.count = func(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
		return nil, status.Error(codes.Internal, "synthetic-secret")
	}
	if _, err := s.CountTokens(serverTestPeer("ccmax"), request); status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "synthetic-secret") {
		t.Fatal(err)
	}
	if _, err := s.CountTokens(context.Background(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	if _, err := s.ListModels(serverTestPeer("ccmax"), &executionv1.ListModelsRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatal(err)
	}
}

func TestDataPlaneFenceCancelsBlockedOpenAndCountTokens(t *testing.T) {
	for _, kind := range []string{"open", "count"} {
		t.Run(kind, func(t *testing.T) {
			var checks atomic.Int32
			started := make(chan struct{})
			canceled := make(chan struct{})
			runtime := &serverTestRuntime{}
			wait := func(ctx context.Context) error { close(started); <-ctx.Done(); close(canceled); return ctx.Err() }
			runtime.open = func(ctx context.Context, _ *executionv1.BeginExecution) (Execution, error) { return nil, wait(ctx) }
			runtime.count = func(ctx context.Context, _ *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
				return nil, wait(ctx)
			}
			resolver := &serverTestResolver{runtime: runtime, validate: func(context.Context, Binding) error {
				if checks.Add(1) > 1 {
					return errors.New("revoked")
				}
				return nil
			}}
			server := testServer(t, resolver)
			var done <-chan error
			if kind == "open" {
				stream, result, _ := startServerTest(t, server, serverTestPeer("ccmax"))
				done = result
				stream.input <- validServerTestBegin()
			} else {
				result := make(chan error, 1)
				done = result
				go func() {
					_, err := server.CountTokens(serverTestPeer("ccmax"), &executionv1.CountTokensRequest{
						RequestId: "request-1", AccountId: "account-1", SlotId: "slot-1", ExecutionEpoch: 3, RouteGeneration: 4,
						Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, AnthropicRequestJson: []byte(`{}`),
					})
					result <- err
				}()
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("worker call not started")
			}
			if err := awaitServerTest(t, done); status.Code(err) != codes.FailedPrecondition {
				t.Fatal(err)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("blocked worker call was not canceled")
			}
		})
	}
}

func TestDataPlaneTransportCancellationAndMaximumDurationCloseWorker(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		e := newServerTestExecution()
		s := testServer(t, &serverTestResolver{runtime: &serverTestRuntime{execution: e}})
		if deadline {
			s.config.MaxExecutionDuration = 30 * time.Millisecond
		}
		stream, done, cancel := startServerTest(t, s, serverTestPeer("ccmax"))
		stream.input <- validServerTestBegin()
		e.events <- outbound{response: serverTestHeaders()}
		select {
		case <-stream.output:
		case <-time.After(time.Second):
			t.Fatal("worker was not connected")
		}
		want := codes.DeadlineExceeded
		if !deadline {
			want = codes.Canceled
			cancel()
		}
		if err := awaitServerTest(t, done); status.Code(err) != want {
			t.Fatalf("code=%v want=%v", status.Code(err), want)
		}
		select {
		case <-e.closed:
		default:
			t.Fatal("canceled worker not closed")
		}
	}
}

func TestDataPlaneResponseValidationAndLimits(t *testing.T) {
	s := testServer(t, &serverTestResolver{})
	bad := serverTestHeaders()
	bad.GetHeaders().Headers["set-cookie"] = "synthetic"
	for _, r := range []*executionv1.ExecuteResponse{nil, {}, bad, {Event: &executionv1.ExecuteResponse_BodyChunk{BodyChunk: &executionv1.ResponseBodyChunk{Data: []byte("before-headers")}}}} {
		if _, _, err := s.response(r, &responseState{}); status.Code(err) != codes.Internal {
			t.Fatal("malformed response accepted")
		}
	}
	r := &executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Error{Error: &executionv1.ExecutionError{Code: "private", Message: "synthetic-secret", ResponseStarted: true}}}
	clean, terminal, err := s.response(r, &responseState{})
	if err != nil || !terminal || clean.GetError().Message != "worker execution failed" || clean.GetError().ResponseStarted {
		t.Fatal("worker error not normalized")
	}
	for _, c := range []Config{{NodeID: "node-1", Resolver: &serverTestResolver{}, FenceInterval: 2 * time.Second}, {NodeID: "node-1", Resolver: &serverTestResolver{}, MaxMessageBytes: MaxMessageBytes + 1}} {
		if _, err := NewServer(c); err == nil {
			t.Fatal("unsafe limit accepted")
		}
	}
	if _, err := NewGRPCServer(Config{NodeID: "node-1", Resolver: &serverTestResolver{}}, nil); err == nil {
		t.Fatal("missing TLS accepted")
	}
	begin := validServerTestBegin()
	s.config.MaxMessageBytes = proto.Size(begin) - 1
	if _, _, err := s.begin(begin); err == nil {
		t.Fatal("oversized proto accepted")
	}
}
