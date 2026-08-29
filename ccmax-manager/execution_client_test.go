package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/ccmax-manager/internal/executionv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func TestExecutionRouteCacheFencesStaleAndConflictingRoutes(t *testing.T) {
	current := time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)
	cache, err := newExecutionRouteCache(10*time.Second, 2, func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	active := executionRoute{
		SlotID: "ccmax-account-7", NodeID: "node-1", Endpoint: "127.0.0.1:8092",
		Epoch: 9, Generation: 4, ExpiresAt: current.Add(time.Minute),
	}
	if err := cache.Put(active); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.Get(active.SlotID, active.Generation, active.Epoch); !ok {
		t.Fatal("active execution route was not cached")
	}
	stale := active
	stale.Epoch--
	if err := cache.Put(stale); !errors.Is(err, errExecutionRouteStale) {
		t.Fatalf("stale route error=%v", err)
	}
	conflicting := active
	conflicting.NodeID = "node-2"
	conflicting.Endpoint = "127.0.0.2:8092"
	if err := cache.Put(conflicting); !errors.Is(err, errExecutionRouteConflict) {
		t.Fatalf("conflicting route error=%v", err)
	}
	current = current.Add(11 * time.Second)
	if _, ok := cache.Get(active.SlotID, active.Generation, active.Epoch); ok {
		t.Fatal("expired execution route remained cached")
	}
}

func TestExecutionRouteRejectsPublicEndpoint(t *testing.T) {
	route := executionRoute{
		SlotID: "ccmax-account-7", NodeID: "node-1", Endpoint: "8.8.8.8:443",
		Epoch: 1, Generation: 1, ExpiresAt: time.Now().Add(time.Minute),
	}
	if err := route.validate(time.Now()); err == nil {
		t.Fatal("public data-plane endpoint was accepted")
	}
}

type staticExecutionRouteResolver struct {
	mu    sync.Mutex
	route executionRoute
	calls int
}

func (resolver *staticExecutionRouteResolver) ResolveExecutionRoute(_ context.Context, slotID string) (executionRoute, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.calls++
	if resolver.route.SlotID != slotID {
		return executionRoute{}, errExecutionRouteNotFound
	}
	return resolver.route, nil
}

func (resolver *staticExecutionRouteResolver) Calls() int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return resolver.calls
}

type testExecutionDataPlaneServer struct {
	executionv1.UnimplementedExecutionDataPlaneServiceServer
	beginSeen  chan *executionv1.BeginExecution
	cancelled  chan struct{}
	cancelOnce sync.Once
}

func (server *testExecutionDataPlaneServer) Execute(stream grpc.BidiStreamingServer[executionv1.ExecuteRequest, executionv1.ExecuteResponse]) error {
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	begin := request.GetBegin()
	if begin == nil {
		return status.Error(codes.InvalidArgument, "begin is required")
	}
	server.beginSeen <- begin
	<-stream.Context().Done()
	server.cancelOnce.Do(func() { close(server.cancelled) })
	return status.FromContextError(stream.Context().Err()).Err()
}

func (server *testExecutionDataPlaneServer) CountTokens(_ context.Context, request *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	if request.GetSlotId() != "ccmax-account-7" || request.GetExecutionEpoch() != 9 || request.GetRouteGeneration() != 4 {
		return nil, status.Error(codes.FailedPrecondition, "route fence mismatch")
	}
	return &executionv1.CountTokensResponse{
		StatusCode: 200, AnthropicResponseJson: []byte(`{"input_tokens":17}`), UpstreamRequestId: "upstream-test",
	}, nil
}

func (server *testExecutionDataPlaneServer) ListModels(context.Context, *executionv1.ListModelsRequest) (*executionv1.ListModelsResponse, error) {
	return &executionv1.ListModelsResponse{}, nil
}

func TestExecutionDataPlaneMTLSRouteCacheAndCancellation(t *testing.T) {
	tlsFiles, serverTLS := makeExecutionTLSFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	implementation := &testExecutionDataPlaneServer{beginSeen: make(chan *executionv1.BeginExecution, 1), cancelled: make(chan struct{})}
	executionv1.RegisterExecutionDataPlaneServiceServer(server, implementation)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	transportCredentials, err := loadExecutionTransportCredentials(tlsFiles)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &staticExecutionRouteResolver{route: executionRoute{
		SlotID: "ccmax-account-7", NodeID: "node-1", Endpoint: listener.Addr().String(),
		Epoch: 9, Generation: 4, ExpiresAt: time.Now().Add(time.Minute),
	}}
	client, err := newExecutionDataPlaneClient(executionDataPlaneClientConfig{
		Resolver: resolver, TransportCredentials: transportCredentials, RouteTTL: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	call := executionCall{
		RequestID: "request-1", AccountID: "7", SlotID: "ccmax-account-7",
		Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, Body: []byte(`{"messages":[]}`),
		ExecutionEpoch: 9, RouteGeneration: 4,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	count, err := client.CountTokens(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	if count.GetStatusCode() != 200 || string(count.GetAnthropicResponseJson()) != `{"input_tokens":17}` {
		t.Fatalf("unexpected count_tokens response: %+v", count)
	}

	streamContext, cancelStream := context.WithCancel(ctx)
	stream, err := client.Execute(streamContext, call)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case begin := <-implementation.beginSeen:
		if begin.GetSlotId() != call.SlotID || begin.GetExecutionEpoch() != call.ExecutionEpoch || begin.GetRouteGeneration() != call.RouteGeneration {
			t.Fatalf("begin route fence mismatch: %+v", begin)
		}
	case <-ctx.Done():
		t.Fatal("host data-plane did not receive begin event")
	}
	cancelStream()
	if _, err := stream.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("stream cancellation error=%v code=%s", err, status.Code(err))
	}
	select {
	case <-implementation.cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("host data-plane did not observe client context cancellation")
	}
	if calls := resolver.Calls(); calls != 1 {
		t.Fatalf("route resolver calls=%d want=1; short-TTL cache was bypassed", calls)
	}
}

func TestExecutionDataPlaneRejectsCredentialHeaders(t *testing.T) {
	if _, err := sanitizeExecutionHeaders(map[string]string{"authorization": "Bearer secret"}); err == nil {
		t.Fatal("authorization header was accepted for data-plane forwarding")
	}
	if _, err := sanitizeExecutionHeaders(map[string]string{"x-api-key": "secret"}); err == nil {
		t.Fatal("API key header was accepted for data-plane forwarding")
	}
	headers, err := sanitizeExecutionHeaders(map[string]string{"Anthropic-Version": "2023-06-01"})
	if err != nil || headers["anthropic-version"] != "2023-06-01" {
		t.Fatalf("safe header normalization failed: headers=%v err=%v", headers, err)
	}
}

func makeExecutionTLSFixture(t *testing.T) (executionTLSFiles, *tls.Config) {
	t.Helper()
	now := time.Now()
	caKey := newExecutionTestKey(t)
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "execution test CA"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER := createExecutionTestCertificate(t, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	caCertificate, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate := issueExecutionTestLeaf(t, caCertificate, caKey, "host-agent.test", []string{"host-agent.test"}, x509.ExtKeyUsageServerAuth, 2)
	clientCertificate := issueExecutionTestLeaf(t, caCertificate, caKey, "ccmax.test", nil, x509.ExtKeyUsageClientAuth, 3)

	directory := t.TempDir()
	caPath := filepath.Join(directory, "ca.pem")
	clientCertPath := filepath.Join(directory, "client.pem")
	clientKeyPath := filepath.Join(directory, "client-key.pem")
	writeExecutionTestPEM(t, caPath, "CERTIFICATE", caDER)
	writeExecutionTestPEM(t, clientCertPath, "CERTIFICATE", clientCertificate.certificateDER)
	writeExecutionTestKey(t, clientKeyPath, clientCertificate.key)

	serverPair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCertificate.certificateDER}),
		marshalExecutionTestKey(t, serverCertificate.key),
	)
	if err != nil {
		t.Fatal(err)
	}
	clientRoots := x509.NewCertPool()
	clientRoots.AddCert(caCertificate)
	return executionTLSFiles{
			CAFile: caPath, ClientCertFile: clientCertPath, ClientKeyFile: clientKeyPath, ServerName: "host-agent.test",
		}, &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair},
			ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clientRoots,
		}
}

type executionTestLeaf struct {
	certificateDER []byte
	key            *ecdsa.PrivateKey
}

func issueExecutionTestLeaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string, dnsNames []string, usage x509.ExtKeyUsage, serial int64) executionTestLeaf {
	t.Helper()
	key := newExecutionTestKey(t)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: commonName}, DNSNames: dnsNames,
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
	}
	return executionTestLeaf{certificateDER: createExecutionTestCertificate(t, template, ca, &key.PublicKey, caKey), key: key}
}

func newExecutionTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func createExecutionTestCertificate(t *testing.T, template, parent *x509.Certificate, publicKey any, signer any) []byte {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, publicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func marshalExecutionTestKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func writeExecutionTestPEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeExecutionTestKey(t *testing.T, path string, key *ecdsa.PrivateKey) {
	t.Helper()
	if err := os.WriteFile(path, marshalExecutionTestKey(t, key), 0o600); err != nil {
		t.Fatal(err)
	}
}
