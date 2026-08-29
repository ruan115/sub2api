package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestEnrollmentControlHeartbeatDispatchAndCertificateRotation(t *testing.T) {
	test := newControlHarness(t, 2*time.Second)
	token, err := test.server.CreateEnrollment(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	nodeKey, nodeKeyPEM, nodePublicKeyPEM := generateNodeKey(t)
	enrollmentClient := executionv1.NewNodeControlServiceClient(test.dial(t, nil))
	enrollment, err := enrollmentClient.EnrollNode(context.Background(), &executionv1.EnrollNodeRequest{
		EnrollmentToken: token.Token, NodeId: "srv74", PublicKeyPem: string(nodePublicKeyPEM),
		Labels:          map[string]string{"region": "ap-shanghai", "host": "srv74"},
		ProtocolVersion: &executionv1.ProtocolVersion{Major: CurrentProtocolMajor, Minor: CurrentProtocolMinor},
		Capabilities:    []string{"docker", "cli_native", "oauth_api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if enrollment.GetNodeId() != "srv74" || enrollment.GetCertificateExpiresAt() == nil {
		t.Fatalf("invalid enrollment response: %+v", enrollment)
	}
	if _, err := enrollmentClient.EnrollNode(context.Background(), &executionv1.EnrollNodeRequest{
		EnrollmentToken: token.Token, NodeId: "srv74", PublicKeyPem: string(nodePublicKeyPEM),
		ProtocolVersion: &executionv1.ProtocolVersion{Major: CurrentProtocolMajor},
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("reused enrollment token code = %s, err = %v", status.Code(err), err)
	}

	nodeTLSCertificate, err := tls.X509KeyPair([]byte(enrollment.GetCertificatePem()), nodeKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	nodeClient := executionv1.NewNodeControlServiceClient(test.dial(t, &nodeTLSCertificate))
	stream, err := nodeClient.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		node, err := test.repository.GetNode(context.Background(), "srv74")
		return err == nil && node.Status == "connected" && node.Capacity.MaxSlots == 20 && node.Labels["region"] == "ap-shanghai"
	})
	duplicate, err := nodeClient.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := duplicate.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	if _, err := duplicate.Recv(); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("duplicate control stream code = %s, err = %v", status.Code(err), err)
	}
	if err := stream.Send(heartbeatEvent("srv74", test.now, 3, 2, 3, 4)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		node, err := test.repository.GetNode(context.Background(), "srv74")
		return err == nil && node.ActiveCLI == 3 && node.ActiveAPI == 2 && node.ActiveTotal == 3 && node.AllocatedSlots == 4 &&
			node.AllocatedCPUMillis == 1_000 && node.AllocatedMemoryBytes == 1<<30
	})

	command := &executionv1.NodeControlServiceControlResponse{
		Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
			CommandId: "cmd-1", Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START,
			SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: 1,
			ImageDigest: "sha256:" + strings.Repeat("a", 64), Deadline: timestamppb.New(test.now.Add(time.Minute)),
			Metadata: map[string]string{"reason_code": "scheduled"},
		}},
	}
	if err := test.server.Dispatch(context.Background(), "srv74", command); err != nil {
		t.Fatal(err)
	}
	if err := test.server.Dispatch(context.Background(), "srv74", command); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate command dispatch error = %v", err)
	}
	delivered, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if delivered.GetSlotCommand().GetCommandId() != "cmd-1" {
		t.Fatalf("delivered command = %+v", delivered)
	}
	if err := stream.Send(&executionv1.NodeControlServiceControlRequest{
		Event: &executionv1.NodeControlServiceControlRequest_CommandResult{CommandResult: &executionv1.CommandResult{
			CommandId: "cmd-1", Succeeded: false, ErrorCode: "UPSTREAM",
			ErrorMessage: "authorization bearer secret-value",
			Slot:         &executionv1.SlotObservation{SlotId: "slot-1", ProviderRef: "container-1", ExecutionEpoch: 1, ActualState: "running", Healthy: true},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		result, exists := test.repository.GetCommandResult("cmd-1")
		return exists && result.ErrorMessage == "redacted internal error" && len(result.SlotObservationJSON) > 0
	})

	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("close control stream: %v", err)
	}
	eventually(t, func() bool {
		node, err := test.repository.GetNode(context.Background(), "srv74")
		return err == nil && node.Status == "disconnected"
	})

	newNodeKey, newNodeKeyPEM, newNodePublicKeyPEM := generateNodeKey(t)
	renewal, err := nodeClient.RenewNodeCertificate(context.Background(), &executionv1.RenewNodeCertificateRequest{
		NodeId: "srv74", PublicKeyPem: string(newNodePublicKeyPEM),
	})
	if err != nil {
		t.Fatal(err)
	}
	if renewal.GetCertificatePem() == enrollment.GetCertificatePem() {
		t.Fatal("certificate rotation returned the previous certificate")
	}
	if _, err := nodeClient.RenewNodeCertificate(context.Background(), &executionv1.RenewNodeCertificateRequest{
		NodeId: "srv74", PublicKeyPem: string(nodePublicKeyPEM),
	}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("rotated certificate reuse code = %s, err = %v", status.Code(err), err)
	}
	newNodeTLSCertificate, err := tls.X509KeyPair([]byte(renewal.GetCertificatePem()), newNodeKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	_ = nodeKey
	_ = newNodeKey
	newStream, err := executionv1.NewNodeControlServiceClient(test.dial(t, &newNodeTLSCertificate)).Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := newStream.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		node, err := test.repository.GetNode(context.Background(), "srv74")
		return err == nil && node.Status == "connected"
	})
	_ = newStream.CloseSend()
}

func TestControlHeartbeatTimeoutMarksNodeDisconnected(t *testing.T) {
	test := newControlHarness(t, 100*time.Millisecond)
	token, err := test.server.CreateEnrollment(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	_, keyPEM, publicKeyPEM := generateNodeKey(t)
	unauthenticated := executionv1.NewNodeControlServiceClient(test.dial(t, nil))
	enrollment, err := unauthenticated.EnrollNode(context.Background(), &executionv1.EnrollNodeRequest{
		EnrollmentToken: token.Token, NodeId: "srv74", PublicKeyPem: string(publicKeyPEM),
		ProtocolVersion: &executionv1.ProtocolVersion{Major: CurrentProtocolMajor},
	})
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair([]byte(enrollment.GetCertificatePem()), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	client := executionv1.NewNodeControlServiceClient(test.dial(t, &certificate))
	stream, err := client.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("heartbeat timeout code = %s, err = %v", status.Code(err), err)
	}
	eventually(t, func() bool {
		node, err := test.repository.GetNode(context.Background(), "srv74")
		return err == nil && node.Status == "disconnected" && node.DisconnectedAt != nil
	})
}

func TestControlRejectsMissingCertificateAndCapacityOverflow(t *testing.T) {
	test := newControlHarness(t, 2*time.Second)
	unauthenticated := executionv1.NewNodeControlServiceClient(test.dial(t, nil))
	stream, err := unauthenticated.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated control code = %s, err = %v", status.Code(err), err)
	}

	token, err := test.server.CreateEnrollment(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	_, keyPEM, publicKeyPEM := generateNodeKey(t)
	enrollment, err := unauthenticated.EnrollNode(context.Background(), &executionv1.EnrollNodeRequest{
		EnrollmentToken: token.Token, NodeId: "srv74", PublicKeyPem: string(publicKeyPEM),
		ProtocolVersion: &executionv1.ProtocolVersion{Major: CurrentProtocolMajor},
	})
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair([]byte(enrollment.GetCertificatePem()), keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	authenticated := executionv1.NewNodeControlServiceClient(test.dial(t, &certificate))
	stream, err = authenticated.Control(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		node, err := test.repository.GetNode(context.Background(), "srv74")
		return err == nil && node.Status == "connected"
	})
	if err := stream.Send(heartbeatEvent("srv74", test.now, 5, 1, 5, 4)); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("capacity overflow code = %s, err = %v", status.Code(err), err)
	}
}

func TestDispatchRejectsSensitiveMetadata(t *testing.T) {
	response := &executionv1.NodeControlServiceControlResponse{
		Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
			CommandId: "cmd-1", Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE,
			SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: 1,
			Deadline: timestamppb.New(time.Now().Add(time.Minute)),
			Metadata: map[string]string{"access_token": "must-not-pass"},
		}},
	}
	if err := validateControlResponse(response); err == nil || !strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("sensitive metadata error = %v", err)
	}
}

func TestCommandResultMustBelongToCurrentSession(t *testing.T) {
	session := &nodeSession{
		pendingCommands:    make(map[string]pendingCommand),
		maxPendingCommands: 4,
	}
	err := (&Server{}).recordCommandResult(context.Background(), "srv74", session, &executionv1.CommandResult{
		CommandId: "not-issued", Succeeded: true,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unissued command result code = %s, err = %v", status.Code(err), err)
	}
}

type controlHarness struct {
	t           *testing.T
	now         time.Time
	authority   *pki.Authority
	repository  *store.MemoryRepository
	server      *Server
	listener    *bufconn.Listener
	grpcServer  *grpc.Server
	connections []*grpc.ClientConn
}

func newControlHarness(t *testing.T, heartbeatTimeout time.Duration) *controlHarness {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	authority, _, err := pki.NewEphemeralAuthority(func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate, _, err := authority.IssueServer([]string{"orchestrator.local"})
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig, err := ServerTLSConfig(serverCertificate, authority)
	if err != nil {
		t.Fatal(err)
	}
	repository := store.NewMemoryRepository()
	config := DefaultConfig()
	config.CertificateTTL = time.Hour
	config.RotateBefore = time.Hour
	config.HeartbeatTimeout = heartbeatTimeout
	config.Now = func() time.Time { return now }
	server, err := NewServer(repository, authority, config)
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tlsConfig)))
	executionv1.RegisterNodeControlServiceServer(grpcServer, server)
	go func() {
		if err := grpcServer.Serve(listener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("serve control gRPC: %v", err)
		}
	}()
	harness := &controlHarness{t: t, now: now, authority: authority, repository: repository, server: server, listener: listener, grpcServer: grpcServer}
	t.Cleanup(func() {
		for _, connection := range harness.connections {
			_ = connection.Close()
		}
		grpcServer.Stop()
		_ = listener.Close()
	})
	return harness
}

func (h *controlHarness) dial(t *testing.T, certificate *tls.Certificate) *grpc.ClientConn {
	t.Helper()
	tlsConfig := &tls.Config{
		RootCAs: h.authority.CertificatePool(), ServerName: "orchestrator.local", MinVersion: tls.VersionTLS13,
	}
	if certificate != nil {
		tlsConfig.Certificates = []tls.Certificate{*certificate}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return h.listener.Dial() }),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithBlock(),
	)
	if err != nil {
		t.Fatal(err)
	}
	h.connections = append(h.connections, connection)
	return connection
}

func generateNodeKey(t *testing.T) (*ecdsa.PrivateKey, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyPEM, err := pki.PublicKeyPEM(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}), publicKeyPEM
}

func helloEvent(nodeID string) *executionv1.NodeControlServiceControlRequest {
	return &executionv1.NodeControlServiceControlRequest{
		Event: &executionv1.NodeControlServiceControlRequest_Hello{Hello: &executionv1.NodeHello{
			NodeId: nodeID, ProtocolVersion: &executionv1.ProtocolVersion{Major: CurrentProtocolMajor, Minor: CurrentProtocolMinor},
			Capabilities: []string{"docker", "cli_native", "oauth_api"},
			Capacity: &executionv1.Capacity{
				MaxSlots: 20, MaxActiveCli: 4, MaxActiveApi: 12, MaxActiveTotal: 12,
				AllocatableCpuMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
			},
			Labels: map[string]string{"region": "ap-shanghai", "host": "srv74"},
		}},
	}
}

func heartbeatEvent(nodeID string, observedAt time.Time, cli, api, total, allocated uint32) *executionv1.NodeControlServiceControlRequest {
	return &executionv1.NodeControlServiceControlRequest{
		Event: &executionv1.NodeControlServiceControlRequest_Heartbeat{Heartbeat: &executionv1.NodeHeartbeat{
			NodeId: nodeID, ObservedAt: timestamppb.New(observedAt), ActiveCli: cli, ActiveApi: api,
			ActiveTotal: total, AllocatedSlots: allocated,
			AllocatedCpuMillis: 1_000, AllocatedMemoryBytes: 1 << 30,
		}},
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}
