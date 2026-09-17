package lifecycle

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/control"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent/bootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/storage"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Real TLS NodeControl + enrollment broker + worker.RunProcess; only the
// physical container provider is fake. No Docker, CLI, account or model calls.
func TestAuthenticatedSTARTControlToWorkerAndRevokedLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	startMust(t, err)
	repository := store.NewMemoryRepository()
	backend := lease.NewMemoryBackend(time.Now)
	receipts, err := storage.NewMemory(16)
	startMust(t, err)
	config := control.DefaultConfig()
	config.CertificateTTL, config.RotateBefore = time.Hour, 10*time.Minute
	config.RuntimeEnrollment = &control.RuntimeEnrollmentConfig{Bindings: repository, Receipts: receipts, Leases: backend}
	server, err := control.NewServer(repository, authority, config)
	startMust(t, err)
	serverCertificate, _, err := authority.IssueServer([]string{"orchestrator.local"})
	startMust(t, err)
	serverTLS, err := control.ServerTLSConfig(serverCertificate, authority)
	startMust(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	startMust(t, err)
	var enrollments, authenticated, business atomic.Int32
	rpc := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)), grpc.UnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasSuffix(info.FullMethod, "/EnrollRuntimeCertificate") {
			enrollments.Add(1)
			if remote, ok := peer.FromContext(ctx); ok {
				if auth, ok := remote.AuthInfo.(credentials.TLSInfo); ok && auth.State.Version == tls.VersionTLS13 && len(auth.State.VerifiedChains) > 0 && len(auth.State.PeerCertificates) == 1 {
					authenticated.Add(1)
				}
			}
		}
		return handler(ctx, req)
	}), grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, startObservedStream{ServerStream: stream, business: &business})
	}))
	executionv1.RegisterNodeControlServiceServer(rpc, server)
	go func() { _ = rpc.Serve(listener) }()
	t.Cleanup(func() { rpc.Stop(); _ = listener.Close() })
	dial := func(certificate *tls.Certificate) executionv1.NodeControlServiceClient {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: authority.CertificatePool(), ServerName: "orchestrator.local"}
		if certificate != nil {
			tlsConfig.Certificates = []tls.Certificate{*certificate}
		}
		conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithNoProxy())
		startMust(t, err)
		t.Cleanup(func() { _ = conn.Close() })
		return executionv1.NewNodeControlServiceClient(conn)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	startMust(t, err)
	public, err := pki.PublicKeyPEM(key.Public())
	startMust(t, err)
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	startMust(t, err)
	enrollment, err := server.CreateEnrollment(ctx, "node-1")
	startMust(t, err)
	enrolled, err := dial(nil).EnrollNode(ctx, &executionv1.EnrollNodeRequest{EnrollmentToken: enrollment.Token, NodeId: "node-1", PublicKeyPem: string(public), ProtocolVersion: &executionv1.ProtocolVersion{Major: 1, Minor: 1}})
	startMust(t, err)
	certificate, err := tls.X509KeyPair([]byte(enrolled.CertificatePem), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}))
	startMust(t, err)
	client := dial(&certificate)
	enroller, err := bootstrap.NewRPCClient(client, authority.CertificatePEM())
	startMust(t, err)
	p := newStartProcessProvider(t, ctx, authority.CertificatePEM())
	executor, err := New(Config{Commands: hostagent.SlotCommandExecutorConfig{Provider: p, Resources: p.spec.Resources, Security: p.spec.Security, Network: p.spec.Network, DrainTimeout: time.Second, MaxSlots: 2}, NodeID: "node-1", TrustPEM: authority.CertificatePEM(), NodeCertificate: certificate, Enrollment: enroller, ReadyTimeout: 3 * time.Second})
	startMust(t, err)
	controlClient, err := hostagent.NewControlClient(hostagent.ControlClientConfig{Client: client, Executor: executor, NodeID: "node-1", Capabilities: []string{"docker"}, Capacity: &executionv1.Capacity{MaxSlots: 2, MaxActiveCli: 1, MaxActiveApi: 1, MaxActiveTotal: 2, AllocatableCpuMillis: 2000, AllocatableMemoryBytes: 2 << 30}, HeartbeatInterval: time.Second, ReconnectMin: time.Millisecond, ReconnectMax: 10 * time.Millisecond, MaxConcurrentCommands: 1, CommandQueue: 4})
	startMust(t, err)
	controlDone := make(chan error, 1)
	go func() { controlDone <- controlClient.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-controlDone:
			startMust(t, err)
		case <-time.After(time.Second):
			t.Error("control client did not stop")
		}
	})
	startEventually(t, ctx, func() bool {
		node, err := repository.GetNode(ctx, "node-1")
		return err == nil && server.ValidateControlSession(ctx, "node-1", node.ControlSessionID) == nil
	})
	node, err := repository.GetNode(ctx, "node-1")
	startMust(t, err)
	now := time.Now().UTC()
	_, err = repository.PutDesiredSlot(ctx, store.Slot{ID: p.spec.SlotID, AccountID: p.spec.AccountID, Provider: "docker", DesiredState: "ready", DesiredGeneration: 1, ImageDigest: p.spec.ImageDigest, CPURequestMillis: 100, MemoryRequestBytes: 1 << 20, CreatedAt: now, UpdatedAt: now})
	startMust(t, err)
	assignment, err := repository.ReserveAssignment(ctx, store.AssignmentReservation{ID: "assignment-1", SlotID: p.spec.SlotID, NodeID: "node-1", ExpectedNodeSessionID: node.ControlSessionID, NodeSeenAfter: now.Add(-time.Second), ReservedAt: now})
	startMust(t, err)
	if assignment.ExecutionEpoch != p.spec.Epoch || assignment.DesiredGeneration != p.spec.RuntimeGeneration {
		t.Fatal("fixture assignment mismatch")
	}
	_, err = repository.GrantExecutionLease(ctx, store.ExecutionLease{ID: "lease-1", SlotID: p.spec.SlotID, NodeID: "node-1", ExecutionEpoch: 1, OwnerID: "owner-1", CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Minute)})
	startMust(t, err)
	claim := lease.Claim{SlotID: p.spec.SlotID, NodeID: "node-1", ExecutionEpoch: 1, OwnerID: "owner-1"}
	startMust(t, backend.Acquire(ctx, claim, time.Minute))
	dispatch := func(id string, action executionv1.SlotCommandAction) store.CommandResult {
		command := &executionv1.SlotCommand{CommandId: id, SlotId: p.spec.SlotID, AccountId: p.spec.AccountID, ExecutionEpoch: 1, ImageDigest: p.spec.ImageDigest, Action: action, Deadline: timestamppb.New(time.Now().Add(4 * time.Second)), Metadata: map[string]string{"desired_generation": "1", "target_runtime_generation": "1"}}
		startMust(t, server.Dispatch(ctx, "node-1", &executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: command}}))
		var result store.CommandResult
		startEventually(t, ctx, func() bool { var ok bool; result, ok = repository.GetCommandResult(id); return ok })
		return result
	}
	for _, id := range []string{"start-1", "start-replay"} {
		result := dispatch(id, executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START)
		if !result.Succeeded || result.Observation == nil || !result.Observation.Healthy || result.Observation.ProviderRef != p.instance.ProviderRef {
			t.Fatalf("authenticated START failed: %s", result.ErrorCode)
		}
	}
	startMust(t, backend.Revoke(ctx, claim))
	failed := dispatch("start-revoked", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START)
	if failed.Succeeded || failed.Observation == nil || failed.Observation.Healthy {
		t.Fatal("revoked lease was accepted by authenticated START")
	}
	inspected := dispatch("inspect-after-failure", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT)
	snapshot := executor.Snapshot()
	if !inspected.Succeeded || inspected.Observation == nil || inspected.Observation.Healthy || len(snapshot.Slots) != 1 || snapshot.Slots[0].Healthy {
		t.Fatal("raw provider readiness restored failed authentication proof")
	}
	raw, err := p.InspectSlot(ctx, p.spec.SlotID)
	startMust(t, err)
	if !raw.Healthy || p.creates.Load() != 0 || p.starts.Load() != 3 || p.installs.Load() != 2 || enrollments.Load() != 3 || authenticated.Load() != 3 || business.Load() != 0 {
		t.Fatalf("unexpected boundaries: raw=%v creates=%d starts=%d installs=%d enroll=%d authenticated=%d business=%d", raw.Healthy, p.creates.Load(), p.starts.Load(), p.installs.Load(), enrollments.Load(), authenticated.Load(), business.Load())
	}
}

type startObservedStream struct {
	grpc.ServerStream
	business *atomic.Int32
}

func (s startObservedStream) RecvMsg(message any) error {
	err := s.ServerStream.RecvMsg(message)
	if request, ok := message.(*executionv1.NodeControlServiceControlRequest); ok && (request.GetProbeTicketRequest() != nil || request.GetCredentialCommit() != nil) {
		s.business.Add(1)
	}
	return err
}

type startProcessProvider struct {
	spec                      provider.SlotSpec
	instance                  provider.Instance
	process                   worker.ProcessConfig
	ctx                       context.Context
	mu                        sync.Mutex
	started                   bool
	result                    chan error
	creates, starts, installs atomic.Int32
}

func newStartProcessProvider(t *testing.T, ctx context.Context, trust []byte) *startProcessProvider {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	startMust(t, err)
	startMust(t, os.Chmod(dir, 0700))
	port, err := net.Listen("tcp", "127.0.0.1:0")
	startMust(t, err)
	address := port.Addr().String()
	startMust(t, port.Close())
	public, _, err := ed25519.GenerateKey(rand.Reader)
	startMust(t, err)
	spec := provider.SlotSpec{SlotID: "slot-1", AccountID: "account-1", Epoch: 1, RuntimeGeneration: 1, ImageDigest: "sha256:" + strings.Repeat("a", 64), Resources: provider.ResourceLimits{CPUMilli: 100, MemoryBytes: 1 << 20, PIDs: 16, TmpfsBytes: 1 << 20}, Security: provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "default", AppArmorProfile: "default"}, Network: provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:8094"}}
	origin, err := url.Parse("https://upstream.invalid")
	startMust(t, err)
	workerCtx, stop := context.WithCancel(ctx)
	p := &startProcessProvider{spec: spec, ctx: workerCtx, result: make(chan error, 1), instance: provider.Instance{ProviderRef: "logical-slot-1", RuntimeID: strings.Repeat("b", 64), SlotID: spec.SlotID, Epoch: 1, RuntimeGeneration: 1, State: slot.StateStarting}}
	p.process = worker.ProcessConfig{ListenAddress: address, Identity: worker.Identity{AccountID: provider.RuntimeAccountID(spec.AccountID), SlotID: spec.SlotID, NodeID: "node-1", Epoch: 1}, RuntimeGeneration: 1, IdentityDirectory: filepath.Join(dir, "runtime", "identity"), RuntimeTrustFile: filepath.Join(dir, "runtime", "runtime-ca.pem"), BootstrapCASHA256: fmt.Sprintf("%x", sha256.Sum256(trust)), TicketPublicKey: public, UpstreamBaseURL: origin, EgressProxyURL: spec.Network.EgressProxyEndpoint, ImageDigest: spec.ImageDigest, AllowFakeActivation: true}
	t.Cleanup(func() {
		stop()
		p.mu.Lock()
		started := p.started
		p.mu.Unlock()
		if started {
			select {
			case err := <-p.result:
				if p.installs.Load() > 0 {
					startMust(t, err)
				}
			case <-time.After(12 * time.Second):
				t.Error("worker did not stop")
			}
		}
	})
	return p
}

func (p *startProcessProvider) Create(context.Context, provider.SlotSpec) (provider.Instance, error) {
	p.creates.Add(1)
	return provider.Instance{}, errors.New("CREATE is forbidden in START integration")
}
func (p *startProcessProvider) Start(ctx context.Context, ref string) error {
	if ref != p.instance.RuntimeID || ctx.Err() != nil {
		return errors.New("nonphysical START")
	}
	p.starts.Add(1)
	p.mu.Lock()
	if !p.started {
		p.started = true
		go func() {
			p.result <- worker.RunProcess(p.ctx, p.process, slog.New(slog.NewTextHandler(io.Discard, nil)))
		}()
	}
	p.mu.Unlock()
	for {
		if _, err := os.Stat(filepath.Join(p.process.IdentityDirectory, "instance-identity.json")); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}
func (p *startProcessProvider) Inspect(ctx context.Context, ref string) (provider.Status, error) {
	if ctx.Err() != nil {
		return provider.Status{}, ctx.Err()
	}
	if ref != p.instance.ProviderRef && ref != p.instance.RuntimeID {
		return provider.Status{}, provider.ErrNotFound
	}
	status := provider.Status{Instance: p.instance, ImageDigest: p.spec.ImageDigest}
	conn, err := (&net.Dialer{Timeout: 20 * time.Millisecond}).DialContext(ctx, "tcp", p.process.ListenAddress)
	if err == nil {
		_ = conn.Close()
		status.Healthy = true
		status.State = slot.StateReady
	}
	return status, nil
}
func (p *startProcessProvider) InspectSlot(ctx context.Context, id string) (provider.Status, error) {
	if id != p.spec.SlotID {
		return provider.Status{}, provider.ErrNotFound
	}
	return p.Inspect(ctx, p.instance.ProviderRef)
}
func (p *startProcessProvider) ValidateExisting(ctx context.Context, instance provider.Instance, spec provider.SlotSpec) error {
	if ctx.Err() != nil || instance.ProviderRef != p.instance.ProviderRef || instance.RuntimeID != p.instance.RuntimeID || instance.SlotID != p.spec.SlotID || instance.Epoch != 1 || instance.RuntimeGeneration != 1 || !reflect.DeepEqual(spec, p.spec) {
		return errors.New("existing binding mismatch")
	}
	return nil
}
func (p *startProcessProvider) RuntimeEndpoint(ctx context.Context, ref string) (string, error) {
	if ctx.Err() != nil || ref != p.instance.RuntimeID {
		return "", errors.New("nonphysical endpoint")
	}
	return p.process.ListenAddress, nil
}
func (p *startProcessProvider) BootstrapRequest(ctx context.Context, instance provider.Instance, spec provider.SlotSpec) (runtimebootstrap.Request, error) {
	if ctx.Err() != nil || instance.ProviderRef != p.instance.RuntimeID || instance.RuntimeID != p.instance.RuntimeID || !reflect.DeepEqual(spec, p.spec) {
		return runtimebootstrap.Request{}, errors.New("bootstrap binding mismatch")
	}
	return runtimebootstrap.RequestIdentity(p.process.BootstrapConfig())
}
func (p *startProcessProvider) BootstrapInstall(ctx context.Context, instance provider.Instance, spec provider.SlotSpec, bundle runtimebootstrap.PublicBundle) error {
	if ctx.Err() != nil || instance.ProviderRef != p.instance.RuntimeID || instance.RuntimeID != p.instance.RuntimeID || !reflect.DeepEqual(spec, p.spec) {
		return errors.New("install binding mismatch")
	}
	if err := runtimebootstrap.Install(p.process.BootstrapConfig(), bundle); err != nil {
		return err
	}
	p.installs.Add(1)
	return nil
}
func (*startProcessProvider) Drain(context.Context, string, time.Time) error {
	return errors.New("unexpected DRAIN")
}
func (*startProcessProvider) Stop(context.Context, string) error {
	return errors.New("unexpected STOP")
}
func (*startProcessProvider) Destroy(context.Context, string) error {
	return errors.New("unexpected DESTROY")
}

func startMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func startEventually(t *testing.T, ctx context.Context, check func() bool) {
	t.Helper()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("integration condition timed out")
		case <-time.After(5 * time.Millisecond):
		}
	}
}
