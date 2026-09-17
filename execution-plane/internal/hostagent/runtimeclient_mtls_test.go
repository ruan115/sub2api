package hostagent

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
)

type preflightRuntimeProvider struct {
	*providerfake.Provider
	creates int
}

func (p *preflightRuntimeProvider) Create(ctx context.Context, spec provider.SlotSpec) (provider.Instance, error) {
	p.creates++
	return p.Provider.Create(ctx, spec)
}

func (*preflightRuntimeProvider) RuntimeEndpoint(context.Context, string) (string, error) {
	return "127.0.0.1:1", nil
}

func TestControllerRejectsMissingOrUntrustedMTLSConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name        string
		trust       []byte
		certificate tls.Certificate
	}{
		{name: "missing"},
		{name: "invalid root", trust: []byte("not a trust anchor")},
		{name: "invalid certificate", trust: []byte("not a trust anchor"), certificate: tls.Certificate{Certificate: [][]byte{[]byte("not a certificate")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &preflightRuntimeProvider{Provider: providerfake.New()}
			controller, err := NewController(ControllerConfig{
				Provider: p, TicketSource: &streamTicketRecorder{}, NodeID: "node-a",
				RuntimeTrustPEM: tc.trust, NodeCertificate: tc.certificate,
			})
			if err == nil || controller != nil || p.creates != 0 {
				t.Fatal("invalid TLS configuration was accepted or caused provider side effects")
			}
		})
	}
}

func TestControllerStartRejectsInvalidBindingBeforeProviderSideEffects(t *testing.T) {
	for _, change := range []func(*provider.SlotSpec){
		func(s *provider.SlotSpec) { s.RuntimeGeneration = 0 },
		func(s *provider.SlotSpec) { s.Epoch = 0 },
		func(s *provider.SlotSpec) { s.SlotID = "slot/ambiguous" },
		func(s *provider.SlotSpec) { s.AccountID = "" },
	} {
		p := &preflightRuntimeProvider{Provider: providerfake.New()}
		controller := &Controller{provider: p, nodeID: "node-a"}
		spec := provider.SlotSpec{
			SlotID: "slot-a", AccountID: "account-a", Epoch: 7, RuntimeGeneration: 3,
			ImageDigest: "sha256:" + strings.Repeat("a", 64),
			Resources:   provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 128 << 20},
			Security:    provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "builtin", AppArmorProfile: "docker-default"},
			Network:     provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080"},
		}
		change(&spec)
		if runtime, err := controller.Start(context.Background(), spec); err == nil || runtime != nil || p.creates != 0 {
			t.Fatal("invalid runtime binding reached the provider")
		}
	}
}
