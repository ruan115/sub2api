package lifecycle

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
)

type compositionProvider struct{ *providerfake.Provider }

func (*compositionProvider) RuntimeEndpoint(context.Context, string) (string, error) {
	panic("constructor accessed runtime")
}
func (*compositionProvider) ValidateExisting(context.Context, provider.Instance, provider.SlotSpec) error {
	panic("constructor accessed provider")
}
func (*compositionProvider) BootstrapRequest(context.Context, provider.Instance, provider.SlotSpec) (runtimebootstrap.Request, error) {
	panic("constructor exported CSR")
}
func (*compositionProvider) BootstrapInstall(context.Context, provider.Instance, provider.SlotSpec, runtimebootstrap.PublicBundle) error {
	panic("constructor installed certificate")
}

type compositionEnrollment struct{}

func (*compositionEnrollment) Enroll(context.Context, runtimebootstrap.Request) (runtimebootstrap.PublicBundle, error) {
	panic("constructor requested certificate")
}

type injectedStartup struct{}

func (injectedStartup) Start(context.Context, provider.SlotSpec, provider.Instance, string) error {
	panic("injected startup accepted")
}

func compositionConfig(t *testing.T) Config {
	t.Helper()
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal("fixture CA")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal("fixture key")
	}
	public, err := pki.PublicKeyPEM(&key.PublicKey)
	if err != nil {
		t.Fatal("fixture public key")
	}
	issued, err := authority.IssueNode("node-start", public)
	if err != nil {
		t.Fatal("fixture leaf")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal("fixture private key")
	}
	certificate, err := tls.X509KeyPair(issued.CertificatePEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatal("fixture TLS")
	}
	return Config{
		Commands: hostagent.SlotCommandExecutorConfig{
			Provider:     &compositionProvider{providerfake.New()},
			Resources:    provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 64 << 20},
			Security:     provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "builtin", AppArmorProfile: "docker-default"},
			Network:      provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:8094"},
			DrainTimeout: time.Second, MaxSlots: 2,
		},
		NodeID: "node-start", TrustPEM: authority.CertificatePEM(), NodeCertificate: certificate,
		Enrollment: &compositionEnrollment{}, ReadyTimeout: time.Second,
	}
}

func TestCompositionRequiresOneFullyCapableProviderAndAuthenticatedDependencies(t *testing.T) {
	for _, mode := range []string{"valid", "nil-provider", "typed-nil-provider", "incomplete-provider", "nil-enrollment", "typed-nil-enrollment", "injected-startup", "bad-node", "no-trust", "no-cert", "zero-timeout", "unbounded-timeout", "bad-command-policy"} {
		t.Run(mode, func(t *testing.T) {
			c := compositionConfig(t)
			switch mode {
			case "nil-provider":
				c.Commands.Provider = nil
			case "typed-nil-provider":
				c.Commands.Provider = (*compositionProvider)(nil)
			case "incomplete-provider":
				c.Commands.Provider = providerfake.New()
			case "nil-enrollment":
				c.Enrollment = nil
			case "typed-nil-enrollment":
				c.Enrollment = (*compositionEnrollment)(nil)
			case "injected-startup":
				c.Commands.Startup = injectedStartup{}
			case "bad-node":
				c.NodeID = "another-node"
			case "no-trust":
				c.TrustPEM = nil
			case "no-cert":
				c.NodeCertificate = tls.Certificate{}
			case "zero-timeout":
				c.ReadyTimeout = 0
			case "unbounded-timeout":
				c.ReadyTimeout = time.Minute + time.Second
			case "bad-command-policy":
				c.Commands.Security.RunAsUser = 0
			}
			e, err := New(c)
			if mode == "valid" {
				if e == nil || err != nil {
					t.Fatal("valid composition rejected")
				}
			} else if e != nil || err != ErrComposition {
				t.Fatal("incomplete composition accepted")
			}
		})
	}
}

func TestStartupCompositionNeverIssuesBusinessTickets(t *testing.T) {
	for _, scope := range []string{"", "health", "execute", "activate", "credential_key"} {
		if value, err := (denyTickets{}).Issue(context.Background(), hostagent.TicketRequest{Scope: scope}); value != "" || err == nil {
			t.Fatal("startup composition issued a ticket")
		}
	}
}
