package hostagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/stats"
)

type existingProviderProbe struct {
	*providerfake.Provider
	wantSpec      provider.SlotSpec
	wantInstance  provider.Instance
	endpoint      string
	inspectMutate func(*provider.Status)
	hook          func(string, int) error
	mu            sync.Mutex
	events        []string
	counts        map[string]int
}

func (p *existingProviderProbe) record(event string) error {
	p.mu.Lock()
	p.events = append(p.events, event)
	p.counts[event]++
	n := p.counts[event]
	p.mu.Unlock()
	if p.hook != nil {
		return p.hook(event, n)
	}
	return nil
}

func (p *existingProviderProbe) Create(context.Context, provider.SlotSpec) (provider.Instance, error) {
	_ = p.record("create")
	return provider.Instance{}, errors.New("create is forbidden")
}

func (p *existingProviderProbe) ValidateExisting(_ context.Context, expected provider.Instance, spec provider.SlotSpec) error {
	if !reflect.DeepEqual(spec, p.wantSpec) || expected != p.wantInstance {
		return errors.New("private provider specification mismatch")
	}
	return p.record("validate")
}

func (p *existingProviderProbe) Start(_ context.Context, ref string) error {
	if ref != p.wantInstance.RuntimeID {
		return errors.New("only the physical ID may be started")
	}
	return p.record("start")
}

func (p *existingProviderProbe) Inspect(_ context.Context, ref string) (provider.Status, error) {
	if ref != p.wantInstance.RuntimeID {
		return provider.Status{}, errors.New("only the physical ID may be inspected")
	}
	if err := p.record("inspect"); err != nil {
		return provider.Status{}, err
	}
	value := provider.Status{Instance: p.wantInstance, Healthy: true, ImageDigest: p.wantSpec.ImageDigest}
	value.ProviderRef = ref
	if p.inspectMutate != nil {
		p.inspectMutate(&value)
	}
	return value, nil
}

func (p *existingProviderProbe) RuntimeEndpoint(_ context.Context, ref string) (string, error) {
	if ref != p.wantInstance.RuntimeID {
		return "", errors.New("only the physical ID may resolve an endpoint")
	}
	return p.endpoint, p.record("endpoint")
}

func (p *existingProviderProbe) Stop(context.Context, string) error { return p.record("stop") }
func (p *existingProviderProbe) Destroy(context.Context, string) error {
	return p.record("destroy")
}
func (p *existingProviderProbe) Drain(context.Context, string, time.Time) error {
	return p.record("drain")
}

type existingBootstrapProbe struct{ p *existingProviderProbe }

func (b existingBootstrapProbe) Prepare(_ context.Context, spec provider.SlotSpec, instance provider.Instance) error {
	want := b.p.wantInstance
	want.ProviderRef = want.RuntimeID
	if instance != want || !reflect.DeepEqual(spec, b.p.wantSpec) {
		return errors.New("bootstrap requires the exact physical identity and original specification")
	}
	return b.p.record("bootstrap")
}

type existingConnStats struct {
	ended chan struct{}
	rpcs  atomic.Int32
}

func (*existingConnStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}
func (s *existingConnStats) HandleRPC(_ context.Context, event stats.RPCStats) {
	if _, begin := event.(*stats.Begin); begin {
		s.rpcs.Add(1)
	}
}
func (*existingConnStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (s *existingConnStats) HandleConn(_ context.Context, event stats.ConnStats) {
	if _, ended := event.(*stats.ConnEnd); ended {
		select {
		case s.ended <- struct{}{}:
		default:
		}
	}
}

type existingRuntimeFixture struct {
	controller  *Controller
	provider    *existingProviderProbe
	spec        provider.SlotSpec
	expected    provider.Instance
	tickets     *streamTicketRecorder
	connections *existingConnStats
}

// The real TLS 1.3/HTTP2 transport needs no WorkerRuntime RPC or execution
// ticket. Full process/enrollment composition belongs to the control tests.
func newExistingRuntimeFixture(t *testing.T, peerKind string) *existingRuntimeFixture {
	t.Helper()
	spec := provider.SlotSpec{
		SlotID: "existing-slot", AccountID: "existing-account", Epoch: 7, RuntimeGeneration: 11,
		ImageDigest: "sha256:" + strings.Repeat("a", 64),
		Resources:   provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 128 << 20},
		Security:    provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "builtin", AppArmorProfile: "docker-default"},
		Network:     provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:8094"},
	}
	expected := provider.Instance{ProviderRef: "execution-slot-existing", RuntimeID: strings.Repeat("c", 64), SlotID: spec.SlotID, Epoch: spec.Epoch, RuntimeGeneration: spec.RuntimeGeneration}
	p := &existingProviderProbe{Provider: providerfake.New(), wantSpec: spec, wantInstance: expected, counts: make(map[string]int)}
	f := &existingRuntimeFixture{provider: p, spec: spec, expected: expected, tickets: &streamTicketRecorder{}, connections: &existingConnStats{ended: make(chan struct{}, 8)}}
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeidentity.Binding{AccountHash: provider.RuntimeAccountID(spec.AccountID), SlotID: spec.SlotID, NodeID: "existing-node", Epoch: spec.Epoch, Generation: spec.RuntimeGeneration}
	nodeKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := pki.PublicKeyPEM(&nodeKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	node, err := authority.IssueNode(binding.NodeID, public)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{node.Certificate.Raw}, PrivateKey: nodeKey, Leaf: node.Certificate}
	trust := authority.CertificatePEM()
	switch peerKind {
	case "slot":
		binding.SlotID = "wrong-slot"
	case "account":
		binding.AccountHash = provider.RuntimeAccountID("wrong-account")
	case "generation":
		binding.Generation++
	case "epoch":
		binding.Epoch++
	case "node":
		binding.NodeID = "wrong-node"
	case "CA":
		authority, _, err = pki.NewEphemeralAuthority(time.Now, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(directory, 0700) != nil {
		t.Fatal("private identity fixture unavailable")
	}
	identity, err := runtimeidentity.Open(directory, binding, true)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.CSR()
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := authority.IssueRuntime(binding, csr)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := identity.ServerTLS(leaf.CertificatePEM, authority.CertificatePEM())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.endpoint = listener.Addr().String()
	options := []grpc.ServerOption{grpc.StatsHandler(f.connections)}
	if peerKind != "plaintext" {
		options = append(options, grpc.Creds(credentials.NewTLS(serverTLS)))
	}
	server := grpc.NewServer(options...)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("existing runtime TLS fixture did not stop")
		}
	})
	f.controller, err = NewController(ControllerConfig{Provider: p, TicketSource: f.tickets, NodeID: "existing-node", ReadyTimeout: 500 * time.Millisecond,
		RuntimeTrustPEM: trust, NodeCertificate: certificate, Bootstrap: existingBootstrapProbe{p}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *existingRuntimeFixture) assertNoExtraAuthority(t *testing.T) {
	t.Helper()
	f.provider.mu.Lock()
	defer f.provider.mu.Unlock()
	for _, forbidden := range []string{"create", "drain", "stop", "destroy"} {
		if f.provider.counts[forbidden] != 0 {
			t.Fatalf("existing START unexpectedly invoked %s", forbidden)
		}
	}
	if len(f.tickets.snapshot()) != 0 || f.connections.rpcs.Load() != 0 {
		t.Fatal("existing START consumed a business ticket or called an RPC")
	}
}

func TestControllerStartExistingUsesPhysicalIDAndAuthenticatedTransport(t *testing.T) {
	t.Parallel()
	f := newExistingRuntimeFixture(t, "valid")
	runtime, err := f.controller.StartExisting(context.Background(), f.spec, f.expected)
	if err != nil || runtime == nil {
		t.Fatal("valid existing runtime did not start", err)
	}
	if runtime.Instance != f.expected || runtime.connection.GetState() != connectivity.Ready {
		t.Fatal("runtime lost its logical identity or returned a lazy connection")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.connections.ended:
	case <-time.After(time.Second):
		t.Fatal("caller close did not close the real transport")
	}
	want := []string{"validate", "start", "bootstrap", "validate", "inspect", "endpoint", "validate"}
	if !slices.Equal(f.provider.events, want) {
		t.Fatalf("unexpected startup ordering: %v", f.provider.events)
	}
	f.assertNoExtraAuthority(t)
}

func TestControllerStartExistingRejectsPeerMismatchWithoutFallback(t *testing.T) {
	for _, peerKind := range []string{"slot", "account", "generation", "epoch", "node", "CA", "plaintext"} {
		t.Run(peerKind, func(t *testing.T) {
			t.Parallel()
			f := newExistingRuntimeFixture(t, peerKind)
			runtime, err := f.controller.StartExisting(context.Background(), f.spec, f.expected)
			if err != ErrExistingRuntimeStart || runtime != nil {
				t.Fatal("untrusted runtime was accepted", err)
			}
			if f.provider.counts["validate"] != 2 {
				t.Fatal("peer mismatch was not rejected during the real handshake")
			}
			f.assertNoExtraAuthority(t)
		})
	}
}

func TestControllerStartExistingRejectsMissingPolicyAndPreflightMismatch(t *testing.T) {
	for _, change := range []string{"no controller", "nil context", "no bootstrap", "no verifier", "empty reference", "empty CID", "short CID", "upper CID", "slot", "epoch", "generation", "account", "resources", "network", "image", "invalid trust", "cancelled"} {
		t.Run(change, func(t *testing.T) {
			f := newExistingRuntimeFixture(t, "valid")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch change {
			case "no controller":
				f.controller = nil
			case "nil context":
				ctx = nil
			case "no bootstrap":
				f.controller.bootstrap = nil
			case "no verifier":
				f.controller.provider = &preflightRuntimeProvider{Provider: providerfake.New()}
			case "empty reference":
				f.expected.ProviderRef = ""
			case "empty CID":
				f.expected.RuntimeID = ""
			case "short CID":
				f.expected.RuntimeID = "abcd"
			case "upper CID":
				f.expected.RuntimeID = strings.Repeat("C", 64)
			case "slot":
				f.expected.SlotID = "other"
			case "epoch":
				f.expected.Epoch++
			case "generation":
				f.expected.RuntimeGeneration++
			case "account":
				f.spec.AccountID = "other-account"
			case "resources":
				f.spec.Resources.CPUMilli++
			case "network":
				f.spec.Network.EgressProxyEndpoint = "http://host-agent.execution.internal:18080"
			case "image":
				f.spec.ImageDigest = "sha256:" + strings.Repeat("b", 64)
			case "invalid trust":
				f.controller.runtimeTrustPEM = []byte("private invalid trust")
			case "cancelled":
				cancel()
			}
			if runtime, err := f.controller.StartExisting(ctx, f.spec, f.expected); err != ErrExistingRuntimeStart || runtime != nil {
				t.Fatal("invalid existing runtime accepted", err)
			}
			if f.provider.counts["start"] != 0 || f.provider.counts["bootstrap"] != 0 {
				t.Fatal("preflight rejection caused a provider side effect")
			}
			f.assertNoExtraAuthority(t)
		})
	}
}

func TestControllerStartExistingPostChecksAndCancellation(t *testing.T) {
	for _, stage := range []struct {
		operation  string
		occurrence int
	}{{"validate", 1}, {"start", 1}, {"bootstrap", 1}, {"validate", 2}, {"inspect", 1}, {"endpoint", 1}, {"validate", 3}} {
		for _, cancellation := range []bool{false, true} {
			name := stage.operation + strings.Repeat("-", stage.occurrence)
			if cancellation {
				name += "cancel"
			}
			t.Run(name, func(t *testing.T) {
				f := newExistingRuntimeFixture(t, "valid")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.provider.hook = func(operation string, occurrence int) error {
					if operation != stage.operation || (occurrence != stage.occurrence && stage.operation != "inspect") {
						return nil
					}
					if cancellation {
						cancel()
						return nil
					}
					return errors.New("Authorization: raw private provider failure")
				}
				if runtime, err := f.controller.StartExisting(ctx, f.spec, f.expected); err != ErrExistingRuntimeStart || runtime != nil {
					t.Fatal("failed or cancelled stage accepted", err)
				}
				if stage.operation == "validate" && stage.occurrence == 3 {
					select {
					case <-f.connections.ended:
					case <-time.After(time.Second):
						t.Fatal("post-TLS failure leaked an authenticated connection")
					}
				}
				f.assertNoExtraAuthority(t)
			})
		}
	}
}

func TestControllerStartExistingReadinessRejectsDrift(t *testing.T) {
	for _, field := range []string{"CID", "slot", "epoch", "generation", "image"} {
		t.Run(field, func(t *testing.T) {
			f := newExistingRuntimeFixture(t, "valid")
			f.provider.inspectMutate = func(status *provider.Status) {
				switch field {
				case "CID":
					status.RuntimeID = strings.Repeat("d", 64)
				case "slot":
					status.SlotID = "other"
				case "epoch":
					status.Epoch++
				case "generation":
					status.RuntimeGeneration++
				case "image":
					status.ImageDigest = "sha256:" + strings.Repeat("b", 64)
				}
			}
			if runtime, err := f.controller.StartExisting(context.Background(), f.spec, f.expected); err != ErrExistingRuntimeStart || runtime != nil {
				t.Fatal("readiness identity drift was accepted", err)
			}
			if f.provider.counts["endpoint"] != 0 {
				t.Fatal("identity drift reached the dial path")
			}
			f.assertNoExtraAuthority(t)
		})
	}
}
