//go:build docker_e2e

package hostagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	dockerprovider "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
)

type localTicketSource struct {
	issuer *ticket.Issuer
}

func (s localTicketSource) Issue(_ context.Context, request TicketRequest) (string, error) {
	claims, err := ticket.NewClaims(
		request.AccountID, request.SlotID, request.NodeID, request.Epoch,
		[]string{request.Scope}, time.Now(), time.Minute,
	)
	if err != nil {
		return "", err
	}
	return s.issuer.Sign(claims)
}

func TestDockerWorkerFakeUpstreamE2E(t *testing.T) {
	socket := requiredEnvironment(t, "EXECUTION_E2E_DOCKER_SOCKET")
	image := requiredEnvironment(t, "EXECUTION_E2E_WORKER_IMAGE")

	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	var upstreamCalls atomic.Int64
	proxy := &http.Server{Handler: fakeProxyHandler(&upstreamCalls), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = proxy.Serve(listener) }()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = proxy.Shutdown(shutdownContext)
	}()
	proxyPort := listener.Addr().(*net.TCPAddr).Port

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ticket.NewIssuer(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := dockerprovider.NewHTTPEngine(dockerprovider.HTTPConfig{SocketPath: socket, UserAgent: "sub2api-e2e/1"})
	if err != nil {
		t.Fatal(err)
	}
	providerConfig := dockerprovider.DefaultConfig()
	providerConfig.WorkerBootstrap = &dockerprovider.WorkerBootstrap{
		NodeID: "local-e2e-node", TicketPublicKey: base64.RawStdEncoding.EncodeToString(publicKey),
		UpstreamBaseURL: "http://fake.anthropic.local", RuntimePort: 8093,
		AllowFakeActivation: true,
	}
	dockerRuntime, err := dockerprovider.New(providerConfig, engine)
	if err != nil {
		t.Fatal(err)
	}
	spec := base.SlotSpec{
		SlotID: "local-e2e-slot", AccountID: "account-plaintext-must-not-enter-container", Epoch: 1, ImageDigest: image,
		Resources: base.ResourceLimits{CPUMilli: 500, MemoryBytes: 256 << 20, PIDs: 64, TmpfsBytes: 64 << 20},
		Security: base.SecurityPolicy{
			RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true,
			DropAllCapabilities: true, SeccompProfile: "builtin", AppArmorProfile: "docker-default",
		},
		Network: base.NetworkPolicy{
			DenyDirectInternet:  true,
			EgressProxyEndpoint: fmt.Sprintf("http://host-agent.execution.internal:%d", proxyPort),
		},
	}
	instance, err := dockerRuntime.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := dockerRuntime.Destroy(context.Background(), instance.ProviderRef); err != nil {
			t.Errorf("destroy E2E slot: %v", err)
		}
	}()

	controller, err := NewController(ControllerConfig{
		Provider: dockerRuntime, TicketSource: localTicketSource{issuer: issuer}, NodeID: "local-e2e-node",
		ReadyTimeout: 45 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	runtime, err := controller.Provision(ctx, spec, ActivationLease{
		CredentialLeaseID: "local-e2e-credential-lease", EncryptedCredentialBundle: []byte("encrypted-e2e-bundle"),
		ProxyLeaseID: "local-e2e-proxy-lease",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()

	container, err := engine.InspectContainer(ctx, instance.ProviderRef)
	if err != nil {
		t.Fatal(err)
	}
	inspectText := strings.Join(container.Config.Env, " ") + " " + fmt.Sprint(container.Config.Labels)
	for _, forbidden := range []string{spec.AccountID, "encrypted-e2e-bundle", "ACCESS_TOKEN", "REFRESH_TOKEN"} {
		if strings.Contains(inspectText, forbidden) {
			t.Fatalf("Docker inspect exposed forbidden value %q", forbidden)
		}
	}

	body := []byte(`{"model":"claude-fake-1","messages":[{"role":"user","content":"hello isolated worker"}]}`)
	responses, err := runtime.Execute(ctx, "request-e2e-messages", body)
	if err != nil {
		t.Fatal(err)
	}
	var responseBody []byte
	var upstreamRequestID string
	for _, response := range responses {
		if response.GetBodyChunk() != nil {
			responseBody = append(responseBody, response.GetBodyChunk().GetData()...)
		}
		if response.GetCompleted() != nil {
			upstreamRequestID = response.GetCompleted().GetUpstreamRequestId()
		}
	}
	if !strings.Contains(string(responseBody), "fake-anthropic-response") || !strings.HasPrefix(upstreamRequestID, "msg_e2e_") {
		t.Fatalf("unexpected worker response: id=%q body=%s", upstreamRequestID, responseBody)
	}
	count, err := runtime.CountTokens(ctx, "request-e2e-count", body)
	if err != nil {
		t.Fatal(err)
	}
	if count.GetStatusCode() != http.StatusOK || !strings.Contains(string(count.GetAnthropicResponseJson()), "input_tokens") {
		t.Fatalf("unexpected count_tokens response: %+v", count)
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("fake upstream call count = %d, want 2", upstreamCalls.Load())
	}

	directTarget, directReached, stopDirectTarget := startOutsideSlotNetworkTarget(t)
	defer stopDirectTarget()
	probe, err := engine.ExecContainer(ctx, instance.ProviderRef, []string{"/networkprobe", directTarget})
	if err != nil {
		t.Fatalf("execute direct-network probe: %v", err)
	}
	var probeResult struct {
		Reachable bool `json:"reachable"`
	}
	if probe.ExitCode != 0 || len(probe.Stderr) != 0 || json.Unmarshal(probe.Stdout, &probeResult) != nil {
		t.Fatalf("unexpected direct-network probe result: exit=%d stdout=%q stderr=%q", probe.ExitCode, probe.Stdout, probe.Stderr)
	}
	if probeResult.Reachable {
		t.Fatalf("worker bypassed host-agent egress and reached %s directly", directTarget)
	}
	select {
	case <-directReached:
		t.Fatalf("outside-slot target %s accepted a direct worker connection", directTarget)
	default:
	}

	if err := dockerRuntime.Drain(ctx, instance.ProviderRef, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	health, err := runtime.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range health.GetModes() {
		if mode.GetMode() == executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API && (mode.GetHealthy() || mode.GetReasonCode() != "draining") {
			t.Fatalf("worker did not enter drain mode: %+v", mode)
		}
	}
}

func startOutsideSlotNetworkTarget(t *testing.T) (string, <-chan struct{}, func()) {
	t.Helper()
	var selected net.IP
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, networkInterface := range interfaces {
		addresses, addressErr := networkInterface.Addrs()
		if addressErr != nil {
			continue
		}
		for _, address := range addresses {
			ipText, _, _ := strings.Cut(address.String(), "/")
			ip := net.ParseIP(ipText).To4()
			if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				selected = ip
				break
			}
		}
		if selected != nil {
			break
		}
	}
	if selected == nil {
		t.Fatal("no non-loopback IPv4 address is available for the bypass target")
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: selected})
	if err != nil {
		t.Fatal(err)
	}
	reached := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			select {
			case reached <- struct{}{}:
			default:
			}
			_ = connection.Close()
		}
	}()
	stop := func() {
		_ = listener.Close()
		<-done
	}
	return listener.Addr().String(), reached, stop
}

func fakeProxyHandler(calls *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Host != "fake.anthropic.local" {
			http.Error(response, "unexpected proxy target", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, 2<<20))
		if err != nil || !json.Valid(body) {
			http.Error(response, "invalid body", http.StatusBadRequest)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		response.Header().Set("X-Request-Id", "msg_e2e_proxy")
		if request.URL.Path == "/v1/messages/count_tokens" {
			_, _ = io.WriteString(response, `{"input_tokens":7}`)
			return
		}
		if request.URL.Path != "/v1/messages" {
			http.Error(response, "unexpected path", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(response, `{"id":"msg_e2e_proxy","type":"message","content":[{"type":"text","text":"fake-anthropic-response"}],"usage":{"input_tokens":7,"output_tokens":3}}`)
	})
}

func requiredEnvironment(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required", name)
	}
	return value
}
