package control

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	boot "github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent/bootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/storage"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type enrollmentBindingAdapter struct {
	runtimeenrollment.BindingSource
}

type enrollmentFixture struct {
	*controlHarness
	backend     *lease.MemoryBackend
	client      executionv1.NodeControlServiceClient
	certificate tls.Certificate
	stream      executionv1.NodeControlService_ControlClient
	binding     runtimeidentity.Binding
}

func newEnrollmentFixture(t *testing.T) *enrollmentFixture {
	t.Helper()
	source := &enrollmentBindingAdapter{}
	receipts, err := storage.NewMemory(16)
	if err != nil {
		t.Fatal(err)
	}
	backend := lease.NewMemoryBackend(time.Now)
	h := newControlHarnessWithConfig(t, 15*time.Second, func(c *Config) {
		c.Now = time.Now
		c.RuntimeEnrollment = &RuntimeEnrollmentConfig{Bindings: source, Receipts: receipts, Leases: backend}
	})
	source.BindingSource = h.repository
	token, err := h.server.CreateEnrollment(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	_, key, public := generateNodeKey(t)
	unauthed := executionv1.NewNodeControlServiceClient(h.dial(t, nil))
	enrolled, err := unauthed.EnrollNode(context.Background(), &executionv1.EnrollNodeRequest{EnrollmentToken: token.Token, NodeId: "srv74", PublicKeyPem: string(public), ProtocolVersion: &executionv1.ProtocolVersion{Major: 1, Minor: 1}})
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair([]byte(enrolled.CertificatePem), key)
	if err != nil {
		t.Fatal(err)
	}
	client := executionv1.NewNodeControlServiceClient(h.dial(t, &certificate))
	if _, err := client.EnrollRuntimeCertificate(context.Background(), &executionv1.EnrollRuntimeCertificateRequest{SlotId: "slot-1", ExecutionEpoch: 1, RuntimeGeneration: 1, CsrPem: []byte("public-invalid")}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("no control stream accepted", err)
	}
	stream, err := client.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		n, e := h.repository.GetNode(context.Background(), "srv74")
		return e == nil && h.server.ValidateControlSession(context.Background(), "srv74", n.ControlSessionID) == nil
	})
	// Reserve using the actual hello clock; no provider observation is written.
	h.now = time.Now().UTC()
	a := reserveControlTestAssignment(t, h, "sha256:"+strings.Repeat("a", 64))
	if a.ProviderRef != "" {
		t.Fatal("fixture must not require provider readiness")
	}
	_, err = h.repository.GrantExecutionLease(context.Background(), store.ExecutionLease{ID: "lease-1", SlotID: a.SlotID, NodeID: a.NodeID, ExecutionEpoch: a.ExecutionEpoch, OwnerID: "owner-1", CreatedAt: h.now, UpdatedAt: h.now, ExpiresAt: h.now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Acquire(context.Background(), lease.Claim{SlotID: a.SlotID, NodeID: a.NodeID, ExecutionEpoch: a.ExecutionEpoch, OwnerID: "owner-1"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	return &enrollmentFixture{h, backend, client, certificate, stream, runtimeidentity.Binding{AccountHash: provider.RuntimeAccountID("account-1"), SlotID: a.SlotID, NodeID: a.NodeID, Epoch: a.ExecutionEpoch, Generation: a.DesiredGeneration}}
}

func enrollmentDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(dir, 0700) != nil {
		t.Fatal("private directory unavailable")
	}
	return dir
}

func TestRuntimeEnrollmentRPCIdentityAndLeaseBoundaries(t *testing.T) {
	f := newEnrollmentFixture(t)
	identity, err := runtimeidentity.Open(enrollmentDir(t), f.binding, true)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.CSR()
	if err != nil {
		t.Fatal(err)
	}
	request := &executionv1.EnrollRuntimeCertificateRequest{SlotId: f.binding.SlotID, ExecutionEpoch: f.binding.Epoch, RuntimeGeneration: f.binding.Generation, CsrPem: csr}
	first, err := f.client.EnrollRuntimeCertificate(context.Background(), request)
	if err != nil {
		t.Fatal("authenticated first enrollment", err)
	}
	request.CsrPem, err = identity.CSR()
	if err != nil {
		t.Fatal(err)
	}
	retry, err := f.client.EnrollRuntimeCertificate(context.Background(), request)
	if err != nil || !bytes.Equal(first.GetCertificatePem(), retry.GetCertificatePem()) {
		t.Fatal("same SPKI retry changed leaf", err)
	}
	for _, name := range []string{"anonymous", "generation", "slot", "other-key", "redis-unavailable"} {
		t.Run(name, func(t *testing.T) {
			r := &executionv1.EnrollRuntimeCertificateRequest{SlotId: request.SlotId, ExecutionEpoch: request.ExecutionEpoch, RuntimeGeneration: request.RuntimeGeneration, CsrPem: request.CsrPem}
			client := f.client
			switch name {
			case "anonymous":
				client = executionv1.NewNodeControlServiceClient(f.dial(t, nil))
			case "generation":
				r.RuntimeGeneration++
			case "slot":
				r.SlotId = "other-slot"
			case "other-key":
				other, e := runtimeidentity.Open(enrollmentDir(t), f.binding, true)
				if e != nil {
					t.Fatal(e)
				}
				r.CsrPem, e = other.CSR()
				if e != nil {
					t.Fatal(e)
				}
			case "redis-unavailable":
				f.backend.SetAvailable(false)
				defer f.backend.SetAvailable(true)
			}
			if response, err := client.EnrollRuntimeCertificate(context.Background(), r); err == nil || len(response.GetCertificatePem()) != 0 {
				t.Fatal("unauthorized certificate escaped")
			}
		})
	}
	if err := f.stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	_, _ = f.stream.Recv()
	eventually(t, func() bool {
		n, _ := f.repository.GetNode(context.Background(), "srv74")
		return n.Status == "disconnected"
	})
	if _, err := f.client.EnrollRuntimeCertificate(context.Background(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatal("disconnected stream accepted", err)
	}
}

// The provider/exec transport is synthetic. The control TLS RPC, authority
// gates, local install, actual worker listener and Controller mTLS are real.
// This does not claim cross-container networking or a production deployment.
type enrollmentProcessProvider struct {
	*fake.Provider
	endpoint string
}

func (p *enrollmentProcessProvider) RuntimeEndpoint(context.Context, string) (string, error) {
	return p.endpoint, nil
}

type enrollmentLocalTransport struct{ config runtimebootstrap.Config }

func (p enrollmentLocalTransport) BootstrapRequest(context.Context, provider.Instance, provider.SlotSpec) (runtimebootstrap.Request, error) {
	return runtimebootstrap.RequestIdentity(p.config)
}
func (p enrollmentLocalTransport) BootstrapInstall(_ context.Context, _ provider.Instance, _ provider.SlotSpec, b runtimebootstrap.PublicBundle) error {
	return runtimebootstrap.Install(p.config, b)
}

type enrollmentTickets struct{ issuer *ticket.Issuer }

func (s enrollmentTickets) Issue(_ context.Context, r hostagent.TicketRequest) (string, error) {
	claims, e := ticket.NewClaims(r.AccountID, r.SlotID, r.NodeID, r.Epoch, []string{r.Scope}, time.Now(), time.Minute)
	if e != nil {
		return "", e
	}
	return s.issuer.Sign(claims)
}

func TestRuntimeEnrollmentBootstrapToWorkerMTLS(t *testing.T) {
	f := newEnrollmentFixture(t)
	trust := f.authority.CertificatePEM()
	parent := enrollmentDir(t)
	c := runtimebootstrap.Config{IdentityDirectory: filepath.Join(parent, "runtime", "identity"), TrustFile: filepath.Join(parent, "runtime", "runtime-ca.pem"), TrustSHA256: fmt.Sprintf("%x", sha256.Sum256(trust)), Binding: f.binding}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ticket.NewIssuer(private)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	origin, _ := url.Parse("https://api.anthropic.com")
	config := worker.ProcessConfig{ListenAddress: address, Identity: worker.Identity{AccountID: f.binding.AccountHash, SlotID: f.binding.SlotID, NodeID: f.binding.NodeID, Epoch: f.binding.Epoch}, RuntimeGeneration: f.binding.Generation, IdentityDirectory: c.IdentityDirectory, RuntimeTrustFile: c.TrustFile, BootstrapCASHA256: c.TrustSHA256, TicketPublicKey: public, UpstreamBaseURL: origin, EgressProxyURL: "http://host-agent.execution.internal:8094", ImageDigest: "sha256:" + strings.Repeat("a", 64), AllowFakeActivation: true}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.RunProcess(ctx, config, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Error("worker process", err)
			}
		case <-time.After(12 * time.Second):
			t.Error("worker shutdown timed out")
		}
	})
	eventually(t, func() bool { _, err := runtimebootstrap.RequestIdentity(c); return err == nil })
	if connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond); err == nil {
		connection.Close()
		t.Fatal("worker listened before certificate installation")
	}
	client, err := boot.NewRPCClient(f.client, trust)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := boot.New(boot.Config{Transport: enrollmentLocalTransport{c}, Client: client, NodeID: f.binding.NodeID, TrustPEM: trust})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := hostagent.NewController(hostagent.ControllerConfig{Provider: &enrollmentProcessProvider{fake.New(), address}, TicketSource: enrollmentTickets{issuer}, NodeID: f.binding.NodeID, ReadyTimeout: 5 * time.Second, RuntimeTrustPEM: trust, NodeCertificate: f.certificate, Bootstrap: coordinator})
	if err != nil {
		t.Fatal(err)
	}
	spec := provider.SlotSpec{SlotID: f.binding.SlotID, AccountID: "account-1", Epoch: f.binding.Epoch, RuntimeGeneration: f.binding.Generation, ImageDigest: config.ImageDigest, Resources: provider.ResourceLimits{CPUMilli: 1000, MemoryBytes: 1 << 30, PIDs: 128, TmpfsBytes: 1 << 20}, Security: provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "default", AppArmorProfile: "docker-default"}, Network: provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: config.EgressProxyURL}}
	runtime, err := controller.Start(ctx, spec)
	if err != nil {
		t.Fatal("bootstrap/controller", err)
	}
	defer runtime.Close()
	healthCtx, healthCancel := context.WithTimeout(ctx, 3*time.Second)
	defer healthCancel()
	response, err := runtime.Health(healthCtx)
	if err != nil || response.GetSlotId() != f.binding.SlotID {
		t.Fatal("authenticated worker health", err)
	}
}

func TestRuntimeEnrollmentDisabledByDefault(t *testing.T) {
	h := newControlHarness(t, time.Second)
	client := executionv1.NewNodeControlServiceClient(h.dial(t, nil))
	if _, err := client.EnrollRuntimeCertificate(context.Background(), &executionv1.EnrollRuntimeCertificateRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatal("enrollment unexpectedly enabled", err)
	}
}

func TestRuntimeEnrollmentRPCDoesNotBorrowAnotherLiveLeaf(t *testing.T) {
	f := newEnrollmentFixture(t)
	_, _, public := generateNodeKey(t)
	other, err := f.authority.IssueNode("srv74", public)
	if err != nil {
		t.Fatal(err)
	}
	f.server.mu.RLock()
	live := f.server.sessions["srv74"]
	f.server.mu.RUnlock()
	// Two leaves can be CA-valid for one node. Even if the durable certificate
	// registry permits both, a unary call cannot borrow the other's live stream.
	s := &Server{authority: f.authority, config: f.server.config, runtimeEnrollment: f.server.runtimeEnrollment, sessions: map[string]*nodeSession{"srv74": live}, repository: sessionRepository{
		validate: func(context.Context, string, string, time.Time) error { return nil },
		node:     f.repository.GetNode,
	}}
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{other.Certificate}, VerifiedChains: [][]*x509.Certificate{{other.Certificate}}}}})
	request := &executionv1.EnrollRuntimeCertificateRequest{SlotId: f.binding.SlotID, ExecutionEpoch: f.binding.Epoch, RuntimeGeneration: f.binding.Generation, CsrPem: []byte("never reaches issuer")}
	if _, err := s.EnrollRuntimeCertificate(ctx, request); status.Code(err) != codes.PermissionDenied {
		t.Fatal("different leaf borrowed live control stream", err)
	}
}
