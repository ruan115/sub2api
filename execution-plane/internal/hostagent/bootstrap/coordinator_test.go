package bootstrap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

type testTransport struct {
	request                  runtimebootstrap.Request
	requestErr, installErr   error
	requestBusy, installBusy int
	reads, installs          int
	bundles                  []runtimebootstrap.PublicBundle
}

func (s *testTransport) BootstrapRequest(context.Context, provider.Instance, provider.SlotSpec) (runtimebootstrap.Request, error) {
	s.reads++
	if s.requestBusy > 0 {
		s.requestBusy--
		return runtimebootstrap.Request{}, runtimebootstrap.ErrNotReady
	}
	return s.request, s.requestErr
}
func (s *testTransport) BootstrapInstall(_ context.Context, _ provider.Instance, _ provider.SlotSpec, b runtimebootstrap.PublicBundle) error {
	s.installs++
	s.bundles = append(s.bundles, b)
	if s.installBusy > 0 {
		s.installBusy--
		return runtimebootstrap.ErrNotReady
	}
	return s.installErr
}

type testClient struct {
	bundle runtimebootstrap.PublicBundle
	err    error
	calls  int
}

func (s *testClient) Enroll(context.Context, runtimebootstrap.Request) (runtimebootstrap.PublicBundle, error) {
	s.calls++
	return s.bundle, s.err
}

func TestCoordinatorPinsBindingKeyTrustAndRetriesOnlyBusy(t *testing.T) {
	for _, mode := range []string{"success", "busy", "bad-binding", "bad-csr", "bad-key", "bad-ca", "issue-denied", "install-denied", "request-denied", "cancel", "wrong-instance", "busy-timeout"} {
		t.Run(mode, func(t *testing.T) {
			a, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			binding := runtimeidentity.Binding{AccountHash: provider.RuntimeAccountID("account-1"), SlotID: "slot-1", NodeID: "node-1", Epoch: 1, Generation: 1}
			csr := func() []byte {
				dir, e := filepath.EvalSymlinks(t.TempDir())
				if e != nil || os.Chmod(dir, 0700) != nil {
					t.Fatal("test directory")
				}
				i, e := runtimeidentity.Open(dir, binding, true)
				if e != nil {
					t.Fatal(e)
				}
				c, e := i.CSR()
				if e != nil {
					t.Fatal(e)
				}
				return c
			}
			request := runtimebootstrap.Request{Binding: binding, CSRPEM: csr()}
			issued, err := a.IssueRuntime(binding, request.CSRPEM)
			if err != nil {
				t.Fatal(err)
			}
			transport := &testTransport{request: request}
			client := &testClient{bundle: runtimebootstrap.PublicBundle{CertificatePEM: issued.CertificatePEM, CAPEM: a.CertificatePEM()}}
			spec := provider.SlotSpec{SlotID: binding.SlotID, AccountID: "account-1", Epoch: 1, RuntimeGeneration: 1, ImageDigest: "sha256:" + strings.Repeat("a", 64), Resources: provider.ResourceLimits{CPUMilli: 100, MemoryBytes: 1 << 20, PIDs: 16, TmpfsBytes: 1 << 20}, Security: provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "default", AppArmorProfile: "default"}, Network: provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:8094"}}
			instance := provider.Instance{ProviderRef: "synthetic", SlotID: binding.SlotID, Epoch: 1, RuntimeGeneration: 1}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			switch mode {
			case "busy":
				transport.requestBusy = 1
				transport.installBusy = 1
			case "bad-binding":
				transport.request.Binding.Generation++
			case "bad-csr":
				transport.request.CSRPEM = []byte("invalid")
			case "bad-key":
				other, e := a.IssueRuntime(binding, csr())
				if e != nil {
					t.Fatal(e)
				}
				client.bundle.CertificatePEM = other.CertificatePEM
			case "bad-ca":
				client.bundle.CAPEM = append(bytes.Clone(client.bundle.CAPEM), '\n')
			case "issue-denied":
				client.err = errors.New("private backend error")
			case "install-denied":
				transport.installErr = errors.New("private exec error")
			case "request-denied":
				transport.requestErr = errors.New("private exec error")
			case "cancel":
				cancel()
			case "wrong-instance":
				instance.RuntimeGeneration++
			case "busy-timeout":
				transport.installBusy = 100
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 80*time.Millisecond)
				defer stop()
			}
			coordinator, err := New(Config{Transport: transport, Client: client, NodeID: binding.NodeID, TrustPEM: a.CertificatePEM()})
			if err != nil {
				t.Fatal(err)
			}
			err = coordinator.Prepare(ctx, spec, instance)
			if mode == "success" || mode == "busy" {
				if err != nil || client.calls != 1 || transport.installs == 0 {
					t.Fatal("bootstrap success path", err)
				}
				if mode == "busy" && (transport.reads != 2 || transport.installs != 2 || !bytes.Equal(transport.bundles[0].CertificatePEM, transport.bundles[1].CertificatePEM)) {
					t.Fatal("busy retry changed issuance or certificate")
				}
			} else {
				if err != ErrBootstrap {
					t.Fatal("bootstrap boundary did not return fixed rejection")
				}
				if mode != "install-denied" && mode != "busy-timeout" && transport.installs != 0 {
					t.Fatal("invalid material reached instance install")
				}
			}
		})
	}
}
