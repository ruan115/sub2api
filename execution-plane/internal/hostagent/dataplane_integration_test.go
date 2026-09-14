package hostagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// This is a real two-hop loopback RPC test, not a Docker or real-model test.
type relayEngine struct {
	binding  dataplane.Binding
	calls    atomic.Int32
	canceled chan struct{}
}

func (e *relayEngine) Activate(context.Context, worker.Activation) ([]executionv1.ExecutionMode, error) {
	return nil, errors.New("not used")
}
func (e *relayEngine) ModeHealth(context.Context) []worker.ModeHealth {
	return []worker.ModeHealth{{Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE, Healthy: true}}
}
func (e *relayEngine) Execute(stream worker.ExecutionStream) error {
	e.calls.Add(1)
	b := stream.Begin()
	if b.GetAccountId() != provider.RuntimeAccountID(e.binding.AccountID) || b.GetSlotId() != e.binding.SlotID || b.GetExecutionEpoch() != e.binding.ExecutionEpoch || b.GetRouteGeneration() != e.binding.RouteGeneration || b.GetMode() != executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE {
		return errors.New("synthetic worker binding mismatch")
	}
	if err := stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Headers{Headers: &executionv1.ResponseHeaders{StatusCode: 200, Headers: map[string]string{"content-type": "text/event-stream"}}}}); err != nil {
		return err
	}
	if err := stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_BodyChunk{BodyChunk: &executionv1.ResponseBodyChunk{Data: []byte("first")}}}); err != nil {
		return err
	}
	switch b.RequestId {
	case "large":
		// Cumulative response exceeds the old 2 MiB whole-body limit, while
		// every individual message remains bounded. This tests the relay only.
		for range 64 {
			if err := stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_BodyChunk{BodyChunk: &executionv1.ResponseBodyChunk{Data: make([]byte, 64<<10)}}}); err != nil {
				return err
			}
		}
	case "hold":
		<-stream.Context().Done()
		select {
		case e.canceled <- struct{}{}:
		default:
		}
		return stream.Context().Err()
	case "tool":
		if err := stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_ToolUse{ToolUse: &executionv1.ToolUseRequired{ToolUseBlocksJson: [][]byte{[]byte(`{"type":"tool_use","id":"tool-1","name":"echo","input":{}}`)}, ResumeToken: "synthetic-resume"}}}); err != nil {
			return err
		}
		next, err := stream.Recv()
		if err != nil {
			return err
		}
		if next.GetToolResult().GetToolUseId() != "tool-1" || string(next.GetToolResult().GetContentJson()) != `"synthetic result"` {
			return errors.New("tool result mismatch")
		}
	case "half":
		_, err := stream.Recv()
		if !errors.Is(err, io.EOF) {
			return errors.New("expected half-close")
		}
	}
	return stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Completed{Completed: &executionv1.ExecutionCompleted{UpstreamRequestId: "synthetic-message", UsageJson: []byte(`{"input_tokens":7,"output_tokens":1}`)}}})
}
func (e *relayEngine) CountTokens(_ context.Context, request *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	e.calls.Add(1)
	if request.GetAccountId() != provider.RuntimeAccountID(e.binding.AccountID) || request.GetMode() != executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE {
		return nil, errors.New("count binding mismatch")
	}
	return &executionv1.CountTokensResponse{StatusCode: 200, AnthropicResponseJson: []byte(`{"input_tokens":7}`)}, nil
}

type relaySnapshot struct {
	binding    dataplane.Binding
	generation atomic.Uint64
}

func (s *relaySnapshot) Snapshot(context.Context, string) (dataplane.Snapshot, error) {
	b := s.binding
	b.RouteGeneration = s.generation.Load()
	return dataplane.Snapshot{Binding: b, ProviderRef: "synthetic-runtime", LeaseOwnerID: "synthetic-owner", Ready: true, ObservedAt: time.Now()}, nil
}

type relayLookup struct{ runtime *Runtime }

func (l relayLookup) LookupRuntime(context.Context, dataplane.Binding, string) (dataplane.Runtime, error) {
	return l.runtime, nil
}

type relayFixture struct {
	client  executionv1.ExecutionDataPlaneServiceClient
	binding dataplane.Binding
	engine  *relayEngine
	source  *relaySnapshot
	leases  *lease.MemoryBackend
	claim   lease.Claim
	connect func(string) executionv1.ExecutionDataPlaneServiceClient
}

func newRelayFixture(t *testing.T) relayFixture {
	t.Helper()
	binding := dataplane.Binding{AccountID: "synthetic-account", SlotID: "slot-local", NodeID: "node-local", ExecutionEpoch: 3, RouteGeneration: 5}
	engine := &relayEngine{binding: binding, canceled: make(chan struct{}, 1)}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ticket.NewIssuer(private)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := ticket.NewVerifier(public)
	if err != nil {
		t.Fatal(err)
	}
	identity := worker.Identity{AccountID: provider.RuntimeAccountID(binding.AccountID), SlotID: binding.SlotID, NodeID: binding.NodeID, Epoch: binding.ExecutionEpoch}
	guard, err := worker.NewGuard(verifier, identity, nil)
	if err != nil {
		t.Fatal(err)
	}
	workerServer, err := worker.NewRuntimeServer(worker.RuntimeServerConfig{Guard: guard, Identity: identity, Activator: engine, Executor: engine, HealthSource: engine, ImageDigest: "synthetic-test-image"})
	if err != nil {
		t.Fatal(err)
	}
	local := grpc.NewServer()
	workerServer.Register(local)
	workerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Stop(); _ = workerListener.Close() })
	go func() { _ = local.Serve(workerListener) }()
	connection, err := grpc.NewClient(workerListener.Addr().String(), grpc.WithNoProxy(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	runtime := &Runtime{client: executionv1.NewWorkerRuntimeServiceClient(connection), connection: connection, ticketSource: bridgeTicketSource{issuer: issuer, now: time.Now()}, identity: runtimeIdentity{AccountID: identity.AccountID, SlotID: identity.SlotID, NodeID: identity.NodeID, Epoch: identity.Epoch}}
	source := &relaySnapshot{binding: binding}
	source.generation.Store(binding.RouteGeneration)
	leases := lease.NewMemoryBackend(nil)
	claim := lease.Claim{SlotID: binding.SlotID, NodeID: binding.NodeID, ExecutionEpoch: binding.ExecutionEpoch, OwnerID: "synthetic-owner"}
	if err = leases.Acquire(context.Background(), claim, time.Minute); err != nil {
		t.Fatal(err)
	}
	resolver, err := dataplane.NewFencedResolver(dataplane.FencedResolverConfig{NodeID: binding.NodeID, Source: source, Leases: leases, Runtimes: relayLookup{runtime}})
	if err != nil {
		t.Fatal(err)
	}
	authority, _, err := pki.NewEphemeralAuthority(nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate, _, err := authority.IssueServer([]string{"local-dataplane.test"})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := dataplane.NewGRPCServer(dataplane.Config{NodeID: binding.NodeID, Resolver: resolver, FenceInterval: 10 * time.Millisecond, MaxExecutionDuration: 3 * time.Second}, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate}, ClientCAs: authority.CertificatePool(), ClientAuth: tls.RequireAndVerifyClientCert})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { outer.Stop(); _ = listener.Close() })
	go func() { _ = outer.Serve(listener) }()
	connect := func(serviceID string) executionv1.ExecutionDataPlaneServiceClient {
		var certificates []tls.Certificate
		if serviceID != "" {
			public, private, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			der, err := x509.MarshalPKIXPublicKey(public)
			if err != nil {
				t.Fatal(err)
			}
			issued, err := authority.IssueServiceClient(serviceID, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
			if err != nil {
				t.Fatal(err)
			}
			key, err := x509.MarshalPKCS8PrivateKey(private)
			if err != nil {
				t.Fatal(err)
			}
			certificate, err := tls.X509KeyPair(issued.CertificatePEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
			if err != nil {
				t.Fatal(err)
			}
			certificates = []tls.Certificate{certificate}
		}
		client, err := grpc.NewClient(listener.Addr().String(), grpc.WithNoProxy(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, ServerName: "local-dataplane.test", RootCAs: authority.CertificatePool(), Certificates: certificates})))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.Close() })
		return executionv1.NewExecutionDataPlaneServiceClient(client)
	}
	return relayFixture{client: connect("ccmax"), binding: binding, engine: engine, source: source, leases: leases, claim: claim, connect: connect}
}

func (f relayFixture) begin(requestID string) *executionv1.BeginExecution {
	return &executionv1.BeginExecution{RequestId: requestID, AccountId: f.binding.AccountID, SlotId: f.binding.SlotID, ExecutionEpoch: f.binding.ExecutionEpoch, RouteGeneration: f.binding.RouteGeneration, Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE, AnthropicRequestJson: []byte(`{"model":"synthetic","messages":[]}`)}
}
func relayStart(t *testing.T, ctx context.Context, f relayFixture, id string) executionv1.ExecutionDataPlaneService_ExecuteClient {
	t.Helper()
	stream, err := f.client.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(&executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_Begin{Begin: f.begin(id)}}); err != nil {
		t.Fatal(err)
	}
	return stream
}
func relayFirst(t *testing.T, stream executionv1.ExecutionDataPlaneService_ExecuteClient) {
	t.Helper()
	headers, err := stream.Recv()
	if err != nil || headers.GetHeaders().GetStatusCode() != 200 {
		t.Fatalf("headers=%v err=%v", headers, err)
	}
	first, err := stream.Recv()
	if err != nil || string(first.GetBodyChunk().GetData()) != "first" {
		t.Fatalf("first=%v err=%v", first, err)
	}
}

func TestDataPlaneWorkerLoopbackStreamingAndToolResult(t *testing.T) {
	f := newRelayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream := relayStart(t, ctx, f, "tool")
	relayFirst(t, stream)
	tool, err := stream.Recv()
	if err != nil || tool.GetToolUse() == nil {
		t.Fatalf("tool=%v err=%v", tool, err)
	}
	// Worker cannot finish before this arrives: the preceding first chunk proves
	// neither host hop waited for the complete response before forwarding it.
	if err = stream.Send(&executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_ToolResult{ToolResult: &executionv1.ToolResult{ToolUseId: "tool-1", ContentJson: []byte(`"synthetic result"`)}}}); err != nil {
		t.Fatal(err)
	}
	completed, err := stream.Recv()
	if err != nil || completed.GetCompleted() == nil {
		t.Fatalf("completed=%v err=%v", completed, err)
	}
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal=%v", err)
	}
}

func TestDataPlaneWorkerLoopbackHalfClose(t *testing.T) {
	f := newRelayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream := relayStart(t, ctx, f, "half")
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	relayFirst(t, stream)
	completed, err := stream.Recv()
	if err != nil || completed.GetCompleted() == nil {
		t.Fatalf("completed=%v err=%v", completed, err)
	}
}

func TestDataPlaneWorkerLoopbackDoesNotAggregateResponse(t *testing.T) {
	f := newRelayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream := relayStart(t, ctx, f, "large")
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	relayFirst(t, stream)
	chunks, total := 0, 0
	for {
		response, err := stream.Recv()
		if err != nil {
			t.Fatalf("stream failed after %d chunks: %v", chunks, err)
		}
		if response.GetCompleted() != nil {
			break
		}
		if len(response.GetBodyChunk().GetData()) != 64<<10 {
			t.Fatal("chunk boundary changed")
		}
		chunks++
		total += len(response.GetBodyChunk().GetData())
	}
	if chunks != 64 || total != 4<<20 {
		t.Fatalf("chunks=%d bytes=%d", chunks, total)
	}
}

func TestDataPlaneWorkerLoopbackCancellationAndFencing(t *testing.T) {
	for _, kind := range []string{"cancel frame", "context", "lease revoked", "lease unavailable", "generation changed"} {
		t.Run(kind, func(t *testing.T) {
			f := newRelayFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			stream := relayStart(t, ctx, f, "hold")
			relayFirst(t, stream)
			switch kind {
			case "cancel frame":
				if err := stream.Send(&executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_Cancel{Cancel: &executionv1.CancelExecution{Reason: "client_cancelled"}}}); err != nil {
					t.Fatal(err)
				}
			case "context":
				cancel()
			case "lease revoked":
				if err := f.leases.Revoke(ctx, f.claim); err != nil {
					t.Fatal(err)
				}
			case "lease unavailable":
				f.leases.SetAvailable(false)
			case "generation changed":
				f.source.generation.Add(1)
			}
			if response, err := stream.Recv(); err == nil {
				t.Fatalf("expected terminated stream, got %v", response)
			}
			select {
			case <-f.engine.canceled:
			case <-time.After(time.Second):
				t.Fatal("worker did not receive cancellation")
			}
		})
	}
}

func TestDataPlaneWorkerLoopbackCountAndUnauthorized(t *testing.T) {
	f := newRelayFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := &executionv1.CountTokensRequest{RequestId: "count", AccountId: f.binding.AccountID, SlotId: f.binding.SlotID, ExecutionEpoch: f.binding.ExecutionEpoch, RouteGeneration: f.binding.RouteGeneration, Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE, AnthropicRequestJson: []byte(`{"model":"synthetic","messages":[]}`)}
	response, err := f.client.CountTokens(ctx, request)
	if err != nil || string(response.GetAnthropicResponseJson()) != `{"input_tokens":7}` {
		t.Fatalf("count=%v err=%v", response, err)
	}
	if request.AccountId != f.binding.AccountID {
		t.Fatal("caller account was overwritten")
	}
	if _, err = f.connect("not-ccmax").CountTokens(ctx, request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong service: %v", err)
	}
	if _, err = f.connect("").CountTokens(ctx, request); status.Code(err) != codes.Unavailable {
		t.Fatalf("missing client certificate did not fail TLS handshake: %v", err)
	}
	request.AccountId = "other-account"
	if _, err = f.client.CountTokens(ctx, request); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("wrong account: %v", err)
	}
	if f.engine.calls.Load() != 1 {
		t.Fatal("unauthorized call reached worker")
	}
}
