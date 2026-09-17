package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/nodepolicy"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type entryControl struct {
	executionv1.UnimplementedNodeControlServiceServer
	hello  chan *executionv1.NodeHello
	result chan *executionv1.CommandResult
	mutual atomic.Bool
}

func (c *entryControl) Control(stream grpc.BidiStreamingServer[executionv1.NodeControlServiceControlRequest, executionv1.NodeControlServiceControlResponse]) error {
	if remote, ok := peer.FromContext(stream.Context()); ok {
		if info, ok := remote.AuthInfo.(credentials.TLSInfo); ok && info.State.Version == tls.VersionTLS13 && len(info.State.PeerCertificates) == 1 && len(info.State.VerifiedChains) > 0 {
			c.mutual.Store(true)
		}
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	select {
	case c.hello <- first.GetHello():
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
	// A missing existing instance must fail without create/start fallback. This
	// is the real daemon executor and HTTP adapter, not a replacement runner.
	err = stream.Send(&executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
		CommandId: "entry-start", SlotId: "entry-slot", AccountId: "synthetic-account", ExecutionEpoch: 1,
		ImageDigest: "sha256:" + strings.Repeat("a", 64), Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START,
		Deadline: timestamppb.New(time.Now().Add(3 * time.Second)), Metadata: map[string]string{"desired_generation": "1", "target_runtime_generation": "1"},
	}}})
	if err != nil {
		return err
	}
	for {
		event, err := stream.Recv()
		if err != nil {
			return err
		}
		if result := event.GetCommandResult(); result != nil {
			select {
			case c.result <- result:
			case <-stream.Context().Done():
				return stream.Context().Err()
			}
		}
	}
}

type entryFixture struct {
	health                    config.Config
	cfg                       Config
	control                   *entryControl
	dockerReads, dockerWrites atomic.Int32
}

func newEntryFixture(t *testing.T) *entryFixture {
	t.Helper()
	health, cfg := runtimeTestConfig(t)
	fixture := &entryFixture{health: health, cfg: cfg, control: &entryControl{hello: make(chan *executionv1.NodeHello, 1), result: make(chan *executionv1.CommandResult, 1)}}
	// Short synthetic paths keep the Unix socket within Linux/macOS limits.
	base, err := os.MkdirTemp("/tmp", "host-entry-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	base, err = filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := pki.PublicKeyPEM(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueNode(health.NodeID, public)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	fixture.cfg.TrustFile, fixture.cfg.NodeCertFile, fixture.cfg.NodeKeyFile = filepath.Join(base, "ca.pem"), filepath.Join(base, "node.pem"), filepath.Join(base, "node.key")
	for path, data := range map[string][]byte{fixture.cfg.TrustFile: authority.CertificatePEM(), fixture.cfg.NodeCertFile: issued.CertificatePEM, fixture.cfg.NodeKeyFile: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixture.cfg.DockerSocket = filepath.Join(base, "docker.sock")
	socket, err := net.Listen("unix", fixture.cfg.DockerSocket)
	if err != nil {
		t.Fatal(err)
	}
	dockerServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" && r.Method != "GET" {
			fixture.dockerWrites.Add(1)
			http.Error(w, "writes forbidden", 500)
			return
		}
		fixture.dockerReads.Add(1)
		switch r.URL.Path {
		case "/_ping":
			w.WriteHeader(200)
		case "/version":
			_, _ = w.Write([]byte(`{"ApiVersion":"1.41"}`))
		default:
			http.Error(w, `{"message":"not found"}`, 404)
		}
	})}
	go func() { _ = dockerServer.Serve(socket) }()
	t.Cleanup(func() { _ = dockerServer.Close(); _ = socket.Close() })
	serverCertificate, _, err := authority.IssueServer([]string{fixture.cfg.ControlServerName})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.cfg.ControlAddress = listener.Addr().String()
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate}, ClientCAs: authority.CertificatePool(), ClientAuth: tls.RequireAndVerifyClientCert})))
	executionv1.RegisterNodeControlServiceServer(server, fixture.control)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return fixture
}

func TestRealDaemonCompositionUsesTLSAndNoStartFallback(t *testing.T) {
	f := newEntryFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	parts, err := prepare(ctx, f.health, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer parts.close()
	done := make(chan error, 1)
	go func() { done <- parts.control.Run(ctx) }()
	defer func() {
		parts.commands.Seal()
		cancel()
		shutdown, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := parts.commands.Wait(shutdown); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-shutdown.Done():
			t.Error("control not joined")
		}
	}()
	var hello *executionv1.NodeHello
	select {
	case hello = <-f.control.hello:
	case <-ctx.Done():
		t.Fatal("no real control hello")
	}
	if hello == nil || hello.NodeId != f.health.NodeID || !nodepolicy.LifecycleOnly(hello.Labels, hello.Capabilities) || !f.control.mutual.Load() {
		t.Fatal("missing authenticated lifecycle-only hello")
	}
	if len(hello.Capabilities) != 1 || hello.Labels["dataplane_endpoint"] != "" {
		t.Fatal("business feature advertised")
	}
	select {
	case result := <-f.control.result:
		if result.Succeeded || result.GetSlot().GetHealthy() {
			t.Fatal("missing instance authenticated")
		}
	case <-ctx.Done():
		t.Fatal("no actual command result")
	}
	if f.dockerReads.Load() < 3 || f.dockerWrites.Load() != 0 {
		t.Fatal("startup bypassed inspection or wrote Docker")
	}
}

func TestDaemonIdentityAndServerNameFailClosed(t *testing.T) {
	for _, kind := range []string{"wrong-node", "wrong-server-name"} {
		t.Run(kind, func(t *testing.T) {
			f := newEntryFixture(t)
			f.cfg.StartupTimeout = 150 * time.Millisecond
			if kind == "wrong-node" {
				f.health.NodeID = "different-node"
			} else {
				f.cfg.ControlServerName = "wrong.test"
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			parts, err := prepare(ctx, f.health, f.cfg)
			if parts != nil || !errors.Is(err, ErrRuntime) {
				t.Fatal("untrusted identity connected")
			}
			if f.dockerWrites.Load() != 0 {
				t.Fatal("startup wrote Docker")
			}
			if kind == "wrong-node" && f.dockerReads.Load() != 0 {
				t.Fatal("node mismatch reached Docker")
			}
			select {
			case <-f.control.hello:
				t.Fatal("untrusted control stream opened")
			default:
			}
		})
	}
}
