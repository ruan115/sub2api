package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"net"
	"sync"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/control"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/storage"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type controlWorld struct {
	authority   *pki.Authority
	repository  *store.MemoryRepository
	leases      *lease.MemoryBackend
	client      executionv1.NodeControlServiceClient
	server      *control.Server
	listener    *bufconn.Listener
	grpcServer  *grpc.Server
	connections []*grpc.ClientConn
	cancel      context.CancelFunc
	heartbeat   sync.WaitGroup
}

func (w *controlWorld) close() {
	if w.cancel != nil {
		w.cancel()
	}
	for _, c := range w.connections {
		_ = c.Close()
	}
	if w.grpcServer != nil {
		w.grpcServer.Stop()
	}
	if w.listener != nil {
		_ = w.listener.Close()
	}
	w.heartbeat.Wait()
}

func newControlWorld(authority *pki.Authority) (_ *controlWorld, returned error) {
	ctx, cancel := context.WithCancel(context.Background())
	w := &controlWorld{authority: authority, repository: store.NewMemoryRepository(), leases: lease.NewMemoryBackend(time.Now), cancel: cancel}
	defer func() {
		if returned != nil {
			w.close()
		}
	}()
	receipts, err := storage.NewMemory(4)
	if err != nil {
		return nil, rejected
	}
	c := control.DefaultConfig()
	c.CertificateTTL, c.RotateBefore = time.Hour, time.Hour
	c.RuntimeEnrollment = &control.RuntimeEnrollmentConfig{Bindings: w.repository, Receipts: receipts, Leases: w.leases}
	w.server, err = control.NewServer(w.repository, authority, c)
	if err != nil {
		return nil, rejected
	}
	serverCert, _, err := authority.IssueServer([]string{"orchestrator.local"})
	if err != nil {
		return nil, rejected
	}
	tlsConfig, err := control.ServerTLSConfig(serverCert, authority)
	if err != nil {
		return nil, rejected
	}
	w.listener = bufconn.Listen(1 << 20)
	w.grpcServer = grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)), grpc.MaxRecvMsgSize(64<<10), grpc.MaxSendMsgSize(64<<10))
	executionv1.RegisterNodeControlServiceServer(w.grpcServer, w.server)
	go func() { _ = w.grpcServer.Serve(w.listener) }()
	setup, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	anonymous, err := w.dial(setup, nil)
	if err != nil {
		return nil, rejected
	}
	token, err := w.server.CreateEnrollment(setup, nodeID)
	if err != nil {
		return nil, rejected
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, rejected
	}
	public, err := pki.PublicKeyPEM(&key.PublicKey)
	if err != nil {
		return nil, rejected
	}
	response, err := executionv1.NewNodeControlServiceClient(anonymous).EnrollNode(setup, &executionv1.EnrollNodeRequest{
		EnrollmentToken: token.Token, NodeId: nodeID, PublicKeyPem: string(public),
		ProtocolVersion: &executionv1.ProtocolVersion{Major: control.CurrentProtocolMajor, Minor: control.CurrentProtocolMinor}, Capabilities: []string{"docker"},
	})
	if err != nil {
		return nil, rejected
	}
	// Parse public leaf only. The private key remains the original in-memory
	// signer; no PEM private-key encoding or file is needed.
	leaf, err := parseLeaf([]byte(response.GetCertificatePem()))
	if err != nil {
		return nil, rejected
	}
	certificate := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	connection, err := w.dial(setup, &certificate)
	if err != nil {
		return nil, rejected
	}
	w.client = executionv1.NewNodeControlServiceClient(connection)
	stream, err := w.client.Control(ctx)
	if err != nil {
		return nil, rejected
	}
	if err := stream.Send(&executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_Hello{Hello: &executionv1.NodeHello{
		NodeId: nodeID, ProtocolVersion: &executionv1.ProtocolVersion{Major: control.CurrentProtocolMajor, Minor: control.CurrentProtocolMinor}, Capabilities: []string{"docker"},
		Capacity: &executionv1.Capacity{MaxSlots: 2, MaxActiveCli: 2, MaxActiveApi: 2, MaxActiveTotal: 2, AllocatableCpuMillis: 2000, AllocatableMemoryBytes: 2 << 30},
	}}}); err != nil {
		return nil, rejected
	}
	if err := until(setup, func() (bool, error) {
		n, err := w.repository.GetNode(setup, nodeID)
		return err == nil && w.server.ValidateControlSession(setup, nodeID, n.ControlSessionID) == nil, nil
	}); err != nil {
		return nil, rejected
	}
	w.heartbeat.Add(1)
	go func() {
		defer w.heartbeat.Done()
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if stream.Send(&executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_Heartbeat{Heartbeat: &executionv1.NodeHeartbeat{NodeId: nodeID, ObservedAt: timestamppb.Now()}}}) != nil {
					cancel()
					return
				}
			}
		}
	}()
	return w, nil
}

func (w *controlWorld) dial(ctx context.Context, certificate *tls.Certificate) (*grpc.ClientConn, error) {
	c := &tls.Config{RootCAs: w.authority.CertificatePool(), ServerName: "orchestrator.local", MinVersion: tls.VersionTLS13}
	if certificate != nil {
		c.Certificates = []tls.Certificate{*certificate}
	}
	connection, err := grpc.DialContext(ctx, "passthrough:///bufnet", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return w.listener.DialContext(ctx) }), grpc.WithTransportCredentials(credentials.NewTLS(c)), grpc.WithNoProxy(), grpc.WithBlock())
	if err != nil {
		return nil, rejected
	}
	w.connections = append(w.connections, connection)
	return connection, nil
}

func (w *controlWorld) seed(ctx context.Context) error {
	n, err := w.repository.GetNode(ctx, nodeID)
	if err != nil {
		return rejected
	}
	for index := 0; index < 2; index++ {
		b := binding(index)
		suffix := []string{"a", "b"}[index]
		now := time.Now().UTC()
		if _, err := w.repository.PutDesiredSlot(ctx, store.Slot{ID: b.SlotID, AccountID: "account-live-" + suffix, Provider: "docker", DesiredState: "ready", DesiredGeneration: 1, ImageDigest: baseImage, CPURequestMillis: 1000, MemoryRequestBytes: 1 << 30, CreatedAt: now, UpdatedAt: now}); err != nil {
			return rejected
		}
		a, err := w.repository.ReserveAssignment(ctx, store.AssignmentReservation{ID: "assignment-live-" + suffix, SlotID: b.SlotID, NodeID: nodeID, ExpectedNodeSessionID: n.ControlSessionID, NodeSeenAfter: now.Add(-5 * time.Second), ReservedAt: now})
		if err != nil || a.ExecutionEpoch != 1 || a.DesiredGeneration != 1 {
			return rejected
		}
		if _, err := w.repository.GrantExecutionLease(ctx, store.ExecutionLease{ID: "lease-live-" + suffix, SlotID: b.SlotID, NodeID: nodeID, ExecutionEpoch: 1, OwnerID: "owner-live", ExpiresAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now}); err != nil {
			return rejected
		}
		if w.leases.Acquire(ctx, claim(index), time.Minute) != nil {
			return rejected
		}
	}
	return nil
}

func claim(index int) lease.Claim {
	return lease.Claim{SlotID: binding(index).SlotID, NodeID: nodeID, ExecutionEpoch: 1, OwnerID: "owner-live"}
}

func until(ctx context.Context, f func() (bool, error)) error {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		if ctx.Err() != nil {
			return rejected
		}
		done, err := f()
		if err != nil {
			return rejected
		}
		if done {
			if ctx.Err() != nil {
				return rejected
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return rejected
		case <-t.C:
		}
	}
}
