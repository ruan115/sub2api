package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker/upstream"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const syntheticSSEStart = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"content\":[],\"usage\":{\"input_tokens\":17,\"output_tokens\":1,\"cache_creation_input_tokens\":3,\"cache_creation\":{\"ephemeral_5m_input_tokens\":1,\"ephemeral_1h_input_tokens\":2}}}}\n\n"
const syntheticSSEEnd = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":8}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func openHTTPWorkerRPC(t *testing.T, fixture rpcFixture, ctx context.Context) executionv1.WorkerRuntimeService_ExecuteClient {
	t.Helper()
	stream, err := fixture.client.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{
		Event: &executionv1.WorkerRuntimeServiceExecuteRequest_Begin{Begin: &executionv1.WorkerBeginExecution{
			ExecutionTicket: fixture.ticket(t, "messages"),
			Request: &executionv1.BeginExecution{RequestId: "synthetic-http-rpc", AccountId: fixture.identity.AccountID,
				SlotId: fixture.identity.SlotID, ExecutionEpoch: fixture.identity.Epoch, RouteGeneration: 1,
				Mode:                 executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API,
				AnthropicRequestJson: []byte(`{"model":"synthetic","messages":[],"stream":true}`)},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// Half-closing the request side must not cancel a streaming HTTP response.
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestHTTPWorkerRPCFirstChunkBeforeEndAndLargeStream(t *testing.T) {
	allowEnd := make(chan struct{})
	ended := make(chan struct{})
	const paddingCount = 96
	padding := ":" + strings.Repeat("x", 32<<10) + "\r\n\r\n"
	executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(ended)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("X-Request-Id", "synthetic-stream-id")
		_, _ = io.WriteString(w, syntheticSSEStart)
		w.(http.Flusher).Flush()
		select {
		case <-allowEnd:
		case <-r.Context().Done():
			return
		}
		for range paddingCount {
			if _, err := io.WriteString(w, padding); err != nil {
				return
			}
		}
		_, _ = io.WriteString(w, syntheticSSEEnd)
	})
	fixture := newRPCFixtureWithExecutor(t, executor)
	defer fixture.close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := openHTTPWorkerRPC(t, fixture, ctx)
	headers, err := stream.Recv()
	if err != nil || headers.GetResponse().GetHeaders().GetStatusCode() != 200 {
		t.Fatalf("first headers: %v", err)
	}
	first, err := stream.Recv()
	if err != nil || len(first.GetResponse().GetBodyChunk().GetData()) == 0 {
		t.Fatalf("first body: %v", err)
	}
	select {
	case <-ended:
		t.Fatal("first chunk did not arrive before HTTP completion")
	default:
	}
	close(allowEnd)
	actual := sha256.New()
	_, _ = actual.Write(first.GetResponse().GetBodyChunk().GetData())
	var total int
	total += len(first.GetResponse().GetBodyChunk().GetData())
	var completed *executionv1.ExecutionCompleted
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		response := event.GetResponse()
		if chunk := response.GetBodyChunk(); chunk != nil {
			if len(chunk.GetData()) > upstream.ChunkBytes || completed != nil {
				t.Fatal("invalid chunk boundary/order")
			}
			_, _ = actual.Write(chunk.GetData())
			total += len(chunk.GetData())
		}
		if value := response.GetCompleted(); value != nil {
			if completed != nil {
				t.Fatal("duplicate completion")
			}
			completed = value
		}
	}
	want := sha256.New()
	_, _ = io.WriteString(want, syntheticSSEStart)
	for range paddingCount {
		_, _ = io.WriteString(want, padding)
	}
	_, _ = io.WriteString(want, syntheticSSEEnd)
	if total <= 2<<20 || string(actual.Sum(nil)) != string(want.Sum(nil)) || completed == nil || completed.GetUpstreamRequestId() != "synthetic-stream-id" {
		t.Fatal("large stream bytes or completion incorrect")
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(completed.GetUsageJson(), &usage); err != nil {
		t.Fatal(err)
	}
	if string(usage["input_tokens"]) != "17" || string(usage["output_tokens"]) != "8" || string(usage["cache_creation_input_tokens"]) != "3" {
		t.Fatal("SSE usage lost or added cumulatives")
	}
}

func TestHTTPWorkerRPCCancelClosesUpstream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, syntheticSSEStart)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	})
	fixture := newRPCFixtureWithExecutor(t, executor)
	defer fixture.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := openHTTPWorkerRPC(t, fixture, ctx)
	for {
		event, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if event.GetResponse().GetBodyChunk() != nil {
			break
		}
	}
	cancel()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request did not cancel")
	}
}

func TestHTTPWorkerIncompleteOrErrorSSEHasNoCompleted(t *testing.T) {
	for name, body := range map[string]string{
		"truncated": syntheticSSEStart,
		"error":     syntheticSSEStart + "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"synthetic private diagnostic\"}}\n\n",
		"malformed": syntheticSSEStart + "event: message_delta\ndata: {not-json}\n\n" + syntheticSSEEnd,
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, body)
			})
			stream := testExecutionStream()
			err := executor.Execute(stream)
			if status.Code(err) != codes.DataLoss || strings.Contains(err.Error(), "private") || stream.completed() != nil || calls.Load() != 1 {
				t.Fatalf("invalid SSE terminal: %v", err)
			}
			if string(stream.body()) != body {
				t.Fatal("SSE bytes rewritten")
			}
		})
	}
}

func TestHTTPWorkerActualHTTPReadFailureHasNoCompleted(t *testing.T) {
	var calls atomic.Int32
	executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.WriteString(buffer, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100000\r\n\r\n"+syntheticSSEStart)
		_ = buffer.Flush()
	})
	stream := testExecutionStream()
	err := executor.Execute(stream)
	if status.Code(err) != codes.Unavailable || stream.completed() != nil || calls.Load() != 1 {
		t.Fatalf("truncated HTTP transfer: %v", err)
	}
}

func TestHTTPWorkerMalformedContentTypeNeverBypassesSSETerminalCheck(t *testing.T) {
	for _, body := range []string{syntheticSSEStart, syntheticSSEStart + "event: error\ndata: {\"type\":\"error\"}\n\n", `{"usage":{"input_tokens":9}}`} {
		executor, _ := testUpstreamExecutor(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream; broken")
			_, _ = io.WriteString(w, body)
		})
		stream := testExecutionStream()
		if err := executor.Execute(stream); status.Code(err) != codes.Unavailable || len(stream.events) != 0 {
			t.Fatalf("malformed media type reached caller: %v", err)
		}
	}
}
