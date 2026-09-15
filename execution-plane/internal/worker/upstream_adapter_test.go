package worker

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type observedExecutionStream struct {
	ctx    context.Context
	begin  *executionv1.BeginExecution
	events []*executionv1.ExecuteResponse
	failAt int
}

type retainingExecutionStream struct {
	observedExecutionStream
}

func (s *retainingExecutionStream) Send(event *executionv1.ExecuteResponse) error {
	s.events = append(s.events, event)
	return nil
}

func TestHTTPWorkerRPCSinkOwnsBorrowedChunk(t *testing.T) {
	stream := &retainingExecutionStream{}
	sink := executionResponseSink{stream}
	chunk := []byte("first")
	if err := sink.Chunk(context.Background(), chunk); err != nil {
		t.Fatal(err)
	}
	copy(chunk, "later")
	if got := string(stream.events[0].GetBodyChunk().GetData()); got != "first" {
		t.Fatal("RPC message retained the relay's reusable buffer")
	}
}

func (s *observedExecutionStream) Context() context.Context           { return s.ctx }
func (s *observedExecutionStream) Begin() *executionv1.BeginExecution { return s.begin }
func (s *observedExecutionStream) Recv() (*executionv1.WorkerRuntimeServiceExecuteRequest, error) {
	return nil, io.EOF
}
func (s *observedExecutionStream) Send(event *executionv1.ExecuteResponse) error {
	if s.failAt > 0 && len(s.events)+1 == s.failAt {
		return errors.New("synthetic private downstream diagnostic")
	}
	s.events = append(s.events, proto.Clone(event).(*executionv1.ExecuteResponse))
	return nil
}
func (s *observedExecutionStream) body() []byte {
	var body []byte
	for _, event := range s.events {
		body = append(body, event.GetBodyChunk().GetData()...)
	}
	return body
}
func (s *observedExecutionStream) completed() *executionv1.ExecutionCompleted {
	for _, event := range s.events {
		if value := event.GetCompleted(); value != nil {
			return value
		}
	}
	return nil
}

func testUpstreamExecutor(t *testing.T, handler http.HandlerFunc) (*upstreamExecutor, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	baseURL, _ := url.Parse(server.URL)
	return &upstreamExecutor{state: &processState{activated: true}, client: server.Client(), baseURL: baseURL}, server
}

func testExecutionStream() *observedExecutionStream {
	return &observedExecutionStream{ctx: context.Background(), begin: &executionv1.BeginExecution{
		RequestId: "synthetic-http", Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API,
		AnthropicRequestJson: []byte(`{"model":"synthetic","messages":[],"stream":true}`),
	}}
}

type syntheticCredentialSource struct{}

func (syntheticCredentialSource) ActiveCredential() (ActiveCredential, error) {
	return ActiveCredential{VersionID: "version-synthetic", AuthType: AuthTypeOAuth,
		CredentialJSON: []byte(`{"access_token":"synthetic-test-access","refresh_token":"synthetic-test-refresh"}`)}, nil
}

func TestHTTPWorkerJSONUsageRequestPreservationAndCountTokens(t *testing.T) {
	const responseBody = `{"type":"message","content":[{"type":"text","text":"synthetic"}],"usage":{"input_tokens":12,"output_tokens":8,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":3},"private":"must-not-enter-usage"}}`
	type requestObservation struct {
		path    string
		body    []byte
		headers http.Header
	}
	observations := make(chan requestObservation, 2)
	executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		observations <- requestObservation{r.URL.Path, body, r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "request-synthetic")
		w.Header().Set("Set-Cookie", "must-not-forward")
		if strings.HasSuffix(r.URL.Path, "count_tokens") {
			_, _ = io.WriteString(w, `{"input_tokens":19}`)
			return
		}
		_, _ = io.WriteString(w, responseBody)
	})
	executor.credentialSource = syntheticCredentialSource{}
	stream := testExecutionStream()
	stream.begin.RequestHeaders = map[string]string{"authorization": "incoming-must-not-forward", "cookie": "incoming-cookie", "anthropic-beta": "synthetic-beta"}
	if err := executor.Execute(stream); err != nil {
		t.Fatal(err)
	}
	if string(stream.body()) != responseBody || stream.completed() == nil {
		t.Fatal("JSON bytes or completion lost")
	}
	if stream.events[0].GetHeaders().GetHeaders()["set-cookie"] != "" {
		t.Fatal("unsafe response header")
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(stream.completed().GetUsageJson(), &usage); err != nil {
		t.Fatal(err)
	}
	if string(usage["input_tokens"]) != "12" || string(usage["output_tokens"]) != "8" || usage["private"] != nil {
		t.Fatal("incorrect observed usage")
	}
	if stream.completed().GetUpstreamRequestId() != "request-synthetic" {
		t.Fatal("request ID lost")
	}
	first := <-observations
	if first.path != "/v1/messages" || !bytes.Equal(first.body, stream.begin.AnthropicRequestJson) || first.headers.Get("Authorization") != "Bearer synthetic-test-access" || first.headers.Get("Cookie") != "" || first.headers.Get("Anthropic-Beta") != "synthetic-beta" {
		t.Fatal("request boundary changed")
	}
	count, err := executor.CountTokens(context.Background(), &executionv1.CountTokensRequest{Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, AnthropicRequestJson: []byte(`{"messages":[]}`)})
	if err != nil || string(count.GetAnthropicResponseJson()) != `{"input_tokens":19}` || count.GetUpstreamRequestId() != "request-synthetic" {
		t.Fatalf("count result: %v", err)
	}
	if next := <-observations; next.path != "/v1/messages/count_tokens" || next.headers.Get("Authorization") != "Bearer synthetic-test-access" {
		t.Fatal("count path or credential")
	}
}

func TestHTTPWorkerNon2xxPreservesResponseWithoutUsage(t *testing.T) {
	for _, code := range []int{400, 401, 429, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var calls atomic.Int32
			executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, `{"type":"error","error":{"message":"synthetic"},"usage":{"output_tokens":999}}`)
			})
			stream := testExecutionStream()
			if err := executor.Execute(stream); err != nil {
				t.Fatal(err)
			}
			if stream.events[0].GetHeaders().GetStatusCode() != int32(code) || stream.completed() == nil || len(stream.completed().GetUsageJson()) != 0 || calls.Load() != 1 {
				t.Fatal("non-2xx status/usage/retry boundary")
			}
		})
	}
}

func TestHTTPWorkerRedirectNeverFollowsOrReplays(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { followed.Add(1) }))
	defer target.Close()
	for _, code := range []int{302, 307, 308} {
		executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, code) })
		executor.credentialSource = syntheticCredentialSource{}
		stream := testExecutionStream()
		if err := executor.Execute(stream); err != nil {
			t.Fatal(err)
		}
		if stream.events[0].GetHeaders().GetStatusCode() != int32(code) {
			t.Fatal("redirect was not returned")
		}
	}
	if followed.Load() != 0 {
		t.Fatal("redirect target was contacted")
	}
}

func TestHTTPWorkerUnaryAndRequestLimitsRemainIndependent(t *testing.T) {
	var calls atomic.Int32
	executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Repeat("x", (2<<20)+1))
	})
	stream := testExecutionStream()
	if err := executor.Execute(stream); status.Code(err) != codes.ResourceExhausted || len(stream.events) != 0 {
		t.Fatalf("large unary: %v", err)
	}
	if _, err := executor.CountTokens(context.Background(), &executionv1.CountTokensRequest{Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, AnthropicRequestJson: []byte(`{}`)}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("large count response: %v", err)
	}
	stream.begin.AnthropicRequestJson = bytes.Repeat([]byte("x"), maxWorkerRequestBytes+1)
	if err := executor.Execute(stream); status.Code(err) != codes.InvalidArgument || calls.Load() != 2 {
		t.Fatalf("request limit: %v", err)
	}
}

func TestHTTPWorkerGzipDecodedOrExplicitlyRejected(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport-decodes", true: "unsupported-encoding"}[disable], func(t *testing.T) {
			executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Encoding", "gzip")
				writer := gzip.NewWriter(w)
				_, _ = io.WriteString(writer, `{"usage":{"input_tokens":9}}`)
				_ = writer.Close()
			})
			transport := executor.client.Transport.(*http.Transport).Clone()
			transport.DisableCompression = disable
			t.Cleanup(transport.CloseIdleConnections)
			executor.client = &http.Client{Transport: transport}
			stream := testExecutionStream()
			err := executor.Execute(stream)
			if disable {
				if status.Code(err) != codes.Unimplemented || len(stream.events) != 0 {
					t.Fatalf("encoded response: %v", err)
				}
			} else if err != nil || string(stream.body()) != `{"usage":{"input_tokens":9}}` || stream.completed() == nil {
				t.Fatalf("decoded response: %v", err)
			}
		})
	}
}

func TestHTTPWorkerSinkFailureHasNoCompletionOrPrivateError(t *testing.T) {
	executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	for _, failAt := range []int{1, 2, 3} {
		stream := testExecutionStream()
		stream.failAt = failAt
		err := executor.Execute(stream)
		if status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "private") || stream.completed() != nil {
			t.Fatalf("send failure %d: %v", failAt, err)
		}
	}
}

func TestHTTPWorkerUnsupportedModeAndNotReadyNeverCallUpstream(t *testing.T) {
	var calls atomic.Int32
	executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) })
	stream := testExecutionStream()
	stream.begin.Mode = executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE
	if err := executor.Execute(stream); status.Code(err) != codes.Unimplemented {
		t.Fatalf("mode: %v", err)
	}
	executor.state = &processState{}
	stream.begin.Mode = executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API
	if err := executor.Execute(stream); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("readiness: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("rejected request reached upstream")
	}
}
