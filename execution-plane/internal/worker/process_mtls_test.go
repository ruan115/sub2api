package worker_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
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
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// This composes the real process listener and Controller.Start, not a second
// test-only server. Provider creation is deliberately fake: certificates are
// preinstalled, no Docker bootstrap/enrollment or network isolation is claimed.
type processProvider struct {
	*fake.Provider
	endpoint string
}

func (p *processProvider) RuntimeEndpoint(context.Context, string) (string, error) {
	return p.endpoint, nil
}

type processTickets struct{ issuer *ticket.Issuer }

func (s processTickets) Issue(_ context.Context, request hostagent.TicketRequest) (string, error) {
	claims, err := ticket.NewClaims(request.AccountID, request.SlotID, request.NodeID, request.Epoch, []string{request.Scope}, time.Now(), time.Minute)
	if err != nil {
		return "", err
	}
	return s.issuer.Sign(claims)
}

func processPrivateDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(dir, 0700) != nil {
		t.Fatal("private test directory unavailable")
	}
	return dir
}

func processNodeCertificate(t *testing.T, authority *pki.Authority, node string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := pki.PublicKeyPEM(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueNode(node, pub)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{issued.Certificate.Raw}, PrivateKey: key, Leaf: issued.Certificate}
}

func TestProcessMTLSControllerAndTicketBoundaries(t *testing.T) {
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	trust := authority.CertificatePEM()
	binding := runtimeidentity.Binding{AccountHash: provider.RuntimeAccountID("synthetic-account"), SlotID: "slot-process", NodeID: "node-process", Epoch: 7, Generation: 11}
	dir := processPrivateDir(t)
	identity, err := runtimeidentity.Open(dir, binding, true)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.CSR()
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueRuntime(binding, csr)
	if err != nil || runtimeidentity.InstallCertificate(dir, binding, issued.CertificatePEM, trust) != nil {
		t.Fatal("synthetic certificate preinstallation failed")
	}
	trustPath := filepath.Join(processPrivateDir(t), "runtime-ca.pem")
	if err := os.WriteFile(trustPath, trust, 0600); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ticket.NewIssuer(private)
	if err != nil {
		t.Fatal(err)
	}
	// Reserve then release a loopback-only port. Actual readiness is asserted
	// through mTLS below; a bind race fails the test rather than passing.
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	reservation.Close()
	origin, _ := url.Parse("https://api.anthropic.com")
	config := worker.ProcessConfig{
		ListenAddress: address, Identity: worker.Identity{AccountID: binding.AccountHash, SlotID: binding.SlotID, NodeID: binding.NodeID, Epoch: binding.Epoch},
		RuntimeGeneration: binding.Generation, IdentityDirectory: dir, RuntimeTrustFile: trustPath,
		TicketPublicKey: public, UpstreamBaseURL: origin, EgressProxyURL: "http://host-agent.execution.internal:8094",
		ImageDigest: "sha256:" + strings.Repeat("a", 64), AllowFakeActivation: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- worker.RunProcess(ctx, config, slog.New(slog.NewTextHandler(io.Discard, nil))) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-result:
			if err != nil {
				t.Error("worker process failed:", err)
			}
		case <-time.After(12 * time.Second):
			t.Error("worker did not terminate")
		}
	})
	nodeCertificate := processNodeCertificate(t, authority, binding.NodeID)
	controller, err := hostagent.NewController(hostagent.ControllerConfig{
		Provider: &processProvider{fake.New(), address}, NodeID: binding.NodeID, TicketSource: processTickets{issuer},
		RuntimeTrustPEM: trust, NodeCertificate: nodeCertificate, ReadyTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := provider.SlotSpec{
		SlotID: binding.SlotID, AccountID: "synthetic-account", Epoch: binding.Epoch, RuntimeGeneration: binding.Generation, ImageDigest: config.ImageDigest,
		Resources: provider.ResourceLimits{CPUMilli: 1000, MemoryBytes: 1 << 30, PIDs: 128, TmpfsBytes: 1 << 20},
		Security:  provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "default", AppArmorProfile: "docker-default"},
		Network:   provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: config.EgressProxyURL},
	}
	runtime, err := controller.Start(context.Background(), spec)
	if err != nil {
		t.Fatal("real process/controller TLS connection:", err)
	}
	defer runtime.Close()
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer rpcCancel()
	health, err := runtime.Health(rpcCtx)
	if err != nil || health.GetSlotId() != binding.SlotID || health.GetExecutionEpoch() != binding.Epoch {
		t.Fatal("authenticated Health failed")
	}
	tlsConfig, err := runtimeidentity.ClientTLS(binding, trust, nodeCertificate)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithNoProxy())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := executionv1.NewWorkerRuntimeServiceClient(connection)
	issue := func(account, scope string) string {
		t.Helper()
		raw, err := (processTickets{issuer}).Issue(context.Background(), hostagent.TicketRequest{AccountID: account, SlotID: binding.SlotID, NodeID: binding.NodeID, Epoch: binding.Epoch, Scope: scope})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for name, raw := range map[string]string{"missing": "", "wrong-account": issue(strings.Repeat("b", 32), "health"), "wrong-scope": issue(binding.AccountHash, "begin")} {
		t.Run(name, func(t *testing.T) {
			if _, err := client.Health(rpcCtx, &executionv1.HealthRequest{ExecutionTicket: raw}); status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.Unauthenticated {
				t.Fatalf("valid TLS bypassed ticket boundary: %v", status.Code(err))
			}
		})
	}
	raw := issue(binding.AccountHash, "health")
	if _, err := client.Health(rpcCtx, &executionv1.HealthRequest{ExecutionTicket: raw}); err != nil {
		t.Fatal("fresh ticket rejected")
	}
	if _, err := client.Health(rpcCtx, &executionv1.HealthRequest{ExecutionTicket: raw}); status.Code(err) != codes.AlreadyExists {
		t.Fatal("replayed ticket not rejected")
	}
	for _, name := range []string{"slot", "generation", "wrong-node", "foreign-ca", "plaintext"} {
		t.Run(name, func(t *testing.T) {
			wrong, roots, cert := binding, trust, nodeCertificate
			switch name {
			case "slot":
				wrong.SlotID = "slot-other"
			case "generation":
				wrong.Generation++
			case "wrong-node":
				wrong.NodeID = "node-other"
				cert = processNodeCertificate(t, authority, wrong.NodeID)
			case "foreign-ca":
				other, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
				if err != nil {
					t.Fatal(err)
				}
				roots = other.CertificatePEM()
				cert = processNodeCertificate(t, other, wrong.NodeID)
			}
			var transport credentials.TransportCredentials
			if name == "plaintext" {
				transport = insecure.NewCredentials()
			} else {
				config, err := runtimeidentity.ClientTLS(wrong, roots, cert)
				if err != nil {
					t.Fatal(err)
				}
				transport = credentials.NewTLS(config)
			}
			conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(transport), grpc.WithNoProxy())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			deniedCtx, deniedCancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
			defer deniedCancel()
			if _, err := executionv1.NewWorkerRuntimeServiceClient(conn).Health(deniedCtx, &executionv1.HealthRequest{ExecutionTicket: issue(binding.AccountHash, "health")}); status.Code(err) != codes.Unavailable {
				t.Fatalf("expected transport rejection, not successful RPC or timeout: %v", status.Code(err))
			}
		})
	}
	if _, err := runtime.Health(rpcCtx); err != nil {
		t.Fatal("negative TLS tests interrupted the valid runtime")
	}
	// Exercise Controller.Start itself against the wrong peer, not merely the
	// shared ClientTLS constructor. Returning a lazy client would fail here.
	for _, name := range []string{"controller-generation", "controller-node"} {
		t.Run(name, func(t *testing.T) {
			wrongSpec, node, cert := spec, binding.NodeID, nodeCertificate
			if name == "controller-generation" {
				wrongSpec.RuntimeGeneration++
			} else {
				node = "node-other"
				cert = processNodeCertificate(t, authority, node)
			}
			wrongController, err := hostagent.NewController(hostagent.ControllerConfig{
				Provider: &processProvider{fake.New(), address}, NodeID: node, TicketSource: processTickets{issuer},
				RuntimeTrustPEM: trust, NodeCertificate: cert, ReadyTimeout: 300 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			wrongRuntime, err := wrongController.Start(context.Background(), wrongSpec)
			if wrongRuntime != nil {
				wrongRuntime.Close()
				t.Fatal("controller returned a lazy or incorrectly authenticated runtime")
			}
			if err == nil {
				t.Fatal("controller accepted wrong TLS peer")
			}
		})
	}
}

func TestProcessMTLSFailsBeforeListenWithoutInstalledCertificate(t *testing.T) {
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, fakeActivation := range []bool{false, true} {
		for _, material := range []string{"missing-root", "missing-identity", "not-installed", "wrong-binding"} {
			origin, _ := url.Parse("https://api.anthropic.com")
			dir, trustDir := processPrivateDir(t), processPrivateDir(t)
			trustPath := filepath.Join(trustDir, "ca.pem")
			if material != "missing-root" {
				if err := os.WriteFile(trustPath, authority.CertificatePEM(), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if material == "not-installed" || material == "wrong-binding" {
				binding := runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot-a", NodeID: "node-a", Epoch: 1, Generation: 3}
				if material == "wrong-binding" {
					binding.Generation++
				}
				identity, err := runtimeidentity.Open(dir, binding, true)
				if err != nil {
					t.Fatal(err)
				}
				if material == "wrong-binding" {
					csr, _ := identity.CSR()
					issued, err := authority.IssueRuntime(binding, csr)
					if err != nil || runtimeidentity.InstallCertificate(dir, binding, issued.CertificatePEM, authority.CertificatePEM()) != nil {
						t.Fatal("fixture install failed")
					}
				}
			}
			config := worker.ProcessConfig{
				ListenAddress: "203.0.113.1:8093", Identity: worker.Identity{AccountID: strings.Repeat("a", 32), SlotID: "slot-a", NodeID: "node-a", Epoch: 1},
				RuntimeGeneration: 3, IdentityDirectory: dir, RuntimeTrustFile: trustPath,
				TicketPublicKey: make(ed25519.PublicKey, ed25519.PublicKeySize), UpstreamBaseURL: origin,
				EgressProxyURL: "http://host-agent.execution.internal:8094", ImageDigest: "sha256:" + strings.Repeat("a", 64), AllowFakeActivation: fakeActivation,
				Onboarding: worker.DefaultOnboardingConfig(),
			}
			if err := worker.RunProcess(context.Background(), config, nil); err != runtimeidentity.ErrIdentity {
				t.Fatalf("missing TLS identity reached bind or enabled plaintext: %v", err)
			}
		}
	}
}
