package docker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

type bootstrapFakeEngine struct {
	*fakeEngine
	request   []byte
	afterExec func()
	execs     int
	wantCID   string
}

func (e *bootstrapFakeEngine) BootstrapRequestExec(_ context.Context, cid string, uid uint32) ([]byte, error) {
	e.execs++
	if cid != e.wantCID || uid != 1000 {
		return nil, runtimebootstrap.ErrBootstrap
	}
	if e.afterExec != nil {
		e.afterExec()
	}
	return e.request, nil
}
func (e *bootstrapFakeEngine) BootstrapInstallExec(_ context.Context, cid string, uid uint32, _ runtimebootstrap.PublicBundle) error {
	e.execs++
	if cid != e.wantCID || uid != 1000 {
		return runtimebootstrap.ErrBootstrap
	}
	if e.afterExec != nil {
		e.afterExec()
	}
	return nil
}

func bootstrapProviderFixture(t *testing.T) (*Provider, *bootstrapFakeEngine, base.Instance, base.SlotSpec, runtimebootstrap.PublicBundle) {
	t.Helper()
	engine := sandboxTestEngine(t)
	p := sandboxProviderWithBootstrap(t, engine)
	spec := dockerSpec()
	spec.SlotID = "slot-bootstrap"
	spec.Security.RunAsUser = 1000
	name, networkName := containerName(spec.SlotID), p.networkName(spec.SlotID)
	engine.container.Name, engine.container.Config.Hostname = "/"+name, name
	engine.container.Config.User = "1000:1000"
	engine.container.Config.Labels[labelSlotID] = spec.SlotID
	replaceSandboxEnv(&engine.container, "EXECUTION_SLOT_ID", spec.SlotID)
	var endpoint NetworkEndpoint
	for _, value := range engine.container.NetworkSettings.Networks {
		endpoint = value
	}
	engine.container.NetworkSettings.Networks = map[string]NetworkEndpoint{networkName: endpoint}
	engine.container.HostConfig.NetworkMode = networkName
	engine.network.Name, engine.network.Labels[labelSlotID] = networkName, spec.SlotID
	member := engine.network.Containers[engine.container.ID]
	member.Name = name
	engine.network.Containers[engine.container.ID] = member
	ca, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(ca.CertificatePEM())
	p.config.WorkerBootstrap.TrustSHA256 = hex.EncodeToString(pin[:])
	engine.container.Config.Env = append(engine.container.Config.Env, "EXECUTION_BOOTSTRAP_CA_SHA256="+p.config.WorkerBootstrap.TrustSHA256)
	engine.container.HostConfig.Tmpfs["/run"] += ",mode=1777"
	binding := runtimeidentity.Binding{AccountHash: base.RuntimeAccountID(spec.AccountID), SlotID: spec.SlotID, NodeID: p.config.WorkerBootstrap.NodeID, Epoch: spec.Epoch, Generation: spec.RuntimeGeneration}
	uri, _ := binding.URI()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{uri}}, key)
	if err != nil {
		t.Fatal(err)
	}
	request := runtimebootstrap.Request{Binding: binding, CSRPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})}
	encoded, _ := json.Marshal(request)
	issued, err := ca.IssueRuntime(binding, request.CSRPEM)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &bootstrapFakeEngine{fakeEngine: engine, request: encoded, wantCID: engine.container.ID}
	p.engine = wrapped
	instance := base.Instance{ProviderRef: name, SlotID: spec.SlotID, Epoch: spec.Epoch, RuntimeGeneration: spec.RuntimeGeneration}
	return p, wrapped, instance, spec, runtimebootstrap.PublicBundle{CertificatePEM: issued.CertificatePEM, CAPEM: ca.CertificatePEM()}
}

func TestBootstrapProviderPinsExactContainerUIDBindingAndCA(t *testing.T) {
	p, engine, instance, spec, bundle := bootstrapProviderFixture(t)
	request, err := p.BootstrapRequest(context.Background(), instance, spec)
	if err != nil || request.Binding.SlotID != spec.SlotID || engine.execs != 1 {
		t.Fatal("request failed", err)
	}
	if err := p.BootstrapInstall(context.Background(), instance, spec, bundle); err != nil || engine.execs != 2 {
		t.Fatal("install failed", err)
	}
	engine.afterExec = func() { engine.container.ID = strings.Repeat("e", 64) }
	if _, err := p.BootstrapRequest(context.Background(), instance, spec); err == nil {
		t.Fatal("replaced container accepted")
	}
}

func TestBootstrapProviderRejectsDriftBeforeExec(t *testing.T) {
	for _, name := range []string{"pin", "uid", "generation", "image", "slot", "capability", "wrong-request"} {
		t.Run(name, func(t *testing.T) {
			p, engine, instance, spec, bundle := bootstrapProviderFixture(t)
			switch name {
			case "pin":
				bundle.CAPEM = append(bundle.CAPEM, '\n')
			case "uid":
				engine.container.Config.User = "65532:65532"
			case "generation":
				instance.RuntimeGeneration++
			case "image":
				spec.ImageDigest = "sha256:" + strings.Repeat("f", 64)
			case "slot":
				instance.SlotID = "slot-other"
			case "capability":
				engine.container.HostConfig.CapAdd = []string{"NET_ADMIN"}
			case "wrong-request":
				engine.request = []byte(`{"binding":{},"csr_pem":""}`)
			}
			if name == "wrong-request" {
				if _, err := p.BootstrapRequest(context.Background(), instance, spec); err == nil {
					t.Fatal("invalid CSR accepted")
				}
			} else if err := p.BootstrapInstall(context.Background(), instance, spec, bundle); err == nil || engine.execs != 0 {
				t.Fatal("invalid binding reached exec", err)
			}
		})
	}
}
