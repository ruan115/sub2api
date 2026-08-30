package hostagent

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type scriptedNodeControlServer struct {
	executionv1.UnimplementedNodeControlServiceServer
	now       time.Time
	hello     chan *executionv1.NodeHello
	heartbeat chan *executionv1.NodeHeartbeat
	result    chan *executionv1.CommandResult
}

func (s *scriptedNodeControlServer) Control(stream grpc.BidiStreamingServer[executionv1.NodeControlServiceControlRequest, executionv1.NodeControlServiceControlResponse]) error {
	request, err := stream.Recv()
	if err != nil {
		return err
	}
	if request.GetHello() == nil {
		return errors.New("missing hello")
	}
	s.hello <- request.GetHello()
	if err := stream.Send(&executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
		CommandId: "cmd-control-create", Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE,
		SlotId: "slot-control", AccountId: "account-control", ExecutionEpoch: 4,
		ImageDigest: "sha256:" + strings.Repeat("b", 64), Deadline: timestamppb.New(s.now.Add(time.Minute)),
	}}}); err != nil {
		return err
	}
	for {
		request, err = stream.Recv()
		if err != nil {
			return err
		}
		if heartbeat := request.GetHeartbeat(); heartbeat != nil {
			select {
			case s.heartbeat <- heartbeat:
			default:
			}
		}
		if result := request.GetCommandResult(); result != nil {
			s.result <- result
			<-stream.Context().Done()
			return stream.Context().Err()
		}
	}
}

func TestControlClientSendsHelloHeartbeatAndExecutesCommands(t *testing.T) {
	now := time.Now().UTC()
	server := &scriptedNodeControlServer{
		now: now, hello: make(chan *executionv1.NodeHello, 1), heartbeat: make(chan *executionv1.NodeHeartbeat, 4), result: make(chan *executionv1.CommandResult, 1),
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executionv1.RegisterNodeControlServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	defer listener.Close()
	connection, err := grpc.NewClient(
		"passthrough:///node-control-bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	executor := newTestSlotCommandExecutor(t, providerfake.New(), now)
	client, err := NewControlClient(ControlClientConfig{
		Client: executionv1.NewNodeControlServiceClient(connection), Executor: executor, NodeID: "srv74",
		Labels: map[string]string{"region": "ap-shanghai"}, Capabilities: []string{"oauth_api", "docker", "cli_native"},
		Capacity: &executionv1.Capacity{
			MaxSlots: 20, MaxActiveCli: 4, MaxActiveApi: 12, MaxActiveTotal: 12,
			AllocatableCpuMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
		},
		HeartbeatInterval: 20 * time.Millisecond, ReconnectMin: 10 * time.Millisecond, ReconnectMax: 50 * time.Millisecond,
		MaxConcurrentCommands: 2, CommandQueue: 8, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- client.Run(ctx) }()
	select {
	case hello := <-server.hello:
		if hello.GetNodeId() != "srv74" || hello.GetProtocolVersion().GetMajor() != 1 || hello.GetCapacity().GetMaxSlots() != 20 || hello.GetCapabilities()[0] != "cli_native" {
			t.Fatalf("hello = %+v", hello)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for hello")
	}
	select {
	case heartbeat := <-server.heartbeat:
		if heartbeat.GetNodeId() != "srv74" || heartbeat.GetObservedAt() == nil {
			t.Fatalf("heartbeat = %+v", heartbeat)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for heartbeat")
	}
	select {
	case result := <-server.result:
		if !result.GetSucceeded() || result.GetCommandId() != "cmd-control-create" || result.GetSlot().GetActualState() != "created" {
			t.Fatalf("command result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for command result")
	}
	cancel()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("control client shutdown error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control client did not stop")
	}
}

type scriptedSecureControlServer struct {
	executionv1.UnimplementedNodeControlServiceServer
	now        time.Time
	keyResult  chan *executionv1.CommandResult
	commit     chan *executionv1.ControlCredentialCommit
	activation chan *executionv1.CommandResult
}

func (s *scriptedSecureControlServer) Control(stream grpc.BidiStreamingServer[executionv1.NodeControlServiceControlRequest, executionv1.NodeControlServiceControlResponse]) error {
	request, err := stream.Recv()
	if err != nil || request.GetHello() == nil {
		return errors.New("missing secure control hello")
	}
	if err := stream.Send(&executionv1.NodeControlServiceControlResponse{
		Event: &executionv1.NodeControlServiceControlResponse_CredentialKeyCommand{CredentialKeyCommand: &executionv1.CredentialKeyCommand{
			CommandId: "cmd-key-control", SlotId: "slot-control", AccountId: "account-control", ExecutionEpoch: 4,
			ImageDigest: "sha256:" + strings.Repeat("b", 64), Deadline: timestamppb.New(s.now.Add(time.Minute)),
		}},
	}); err != nil {
		return err
	}
	for {
		request, err = stream.Recv()
		if err != nil {
			return err
		}
		if result := request.GetCommandResult(); result != nil && result.GetCommandId() == "cmd-key-control" {
			s.keyResult <- result
			break
		}
	}
	if err := stream.Send(&executionv1.NodeControlServiceControlResponse{
		Event: &executionv1.NodeControlServiceControlResponse_SecureActivationCommand{SecureActivationCommand: &executionv1.SecureActivationCommand{
			CommandId: "cmd-activate-control", SlotId: "slot-control", AccountId: "account-control", ExecutionEpoch: 4,
			ImageDigest: "sha256:" + strings.Repeat("b", 64), CredentialLeaseId: "lease-control", ProxyLeaseId: "proxy-control",
			EncryptedCredentialBundle: []byte("worker-ciphertext"), Deadline: timestamppb.New(s.now.Add(time.Minute)),
		}},
	}); err != nil {
		return err
	}
	for {
		request, err = stream.Recv()
		if err != nil {
			return err
		}
		if commit := request.GetCredentialCommit(); commit != nil {
			s.commit <- commit
			if err := stream.Send(&executionv1.NodeControlServiceControlResponse{
				Event: &executionv1.NodeControlServiceControlResponse_CredentialCommitAck{CredentialCommitAck: &executionv1.ControlCredentialCommitAck{
					CommandId: commit.GetCommandId(), Accepted: true, VersionId: "44444444-5555-4666-8777-888888888888",
				}},
			}); err != nil {
				return err
			}
		}
		if result := request.GetCommandResult(); result != nil && result.GetCommandId() == "cmd-activate-control" {
			s.activation <- result
			<-stream.Context().Done()
			return stream.Context().Err()
		}
	}
}

type scriptedActivationExecutor struct {
	keyID     string
	publicKey []byte
}

func (e scriptedActivationExecutor) CredentialTransportKey(_ context.Context, command *executionv1.CredentialKeyCommand) *executionv1.CommandResult {
	return &executionv1.CommandResult{
		CommandId: command.GetCommandId(), Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: command.GetSlotId(), ProviderRef: "container-control", ExecutionEpoch: command.GetExecutionEpoch(),
			ActualState: "running", Healthy: true, ImageDigest: command.GetImageDigest(),
		},
		CredentialTransportKey: &executionv1.CredentialTransportKeyOutput{KeyId: e.keyID, PublicKey: append([]byte(nil), e.publicKey...)},
	}
}

func (scriptedActivationExecutor) SecureActivate(ctx context.Context, command *executionv1.SecureActivationCommand, sink worker.SealedCredentialSink) *executionv1.CommandResult {
	versionID, err := sink.CommitSealedCredential(ctx, worker.SealedCredentialCommitRequest{
		AccountBinding: provider.RuntimeAccountID(command.GetAccountId()), SlotID: command.GetSlotId(), ExecutionEpoch: command.GetExecutionEpoch(),
		CredentialLeaseID: command.GetCredentialLeaseId(), ProxyLeaseID: command.GetProxyLeaseId(),
		SealedCredentialBundle: []byte("rotation-ciphertext"),
	})
	if err != nil || versionID == "" {
		return failedCommandResult(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "secure_activation_failed", "secure activation failed", nil)
	}
	return &executionv1.CommandResult{
		CommandId: command.GetCommandId(), Succeeded: true,
		Slot: &executionv1.SlotObservation{
			SlotId: command.GetSlotId(), ProviderRef: "container-control", ExecutionEpoch: command.GetExecutionEpoch(),
			ActualState: "running", Healthy: true, ImageDigest: command.GetImageDigest(),
		},
	}
}

func TestControlClientBridgesSecureActivationCredentialCommit(t *testing.T) {
	now := time.Now().UTC()
	server := &scriptedSecureControlServer{
		now: now, keyResult: make(chan *executionv1.CommandResult, 1), commit: make(chan *executionv1.ControlCredentialCommit, 1), activation: make(chan *executionv1.CommandResult, 1),
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	executionv1.RegisterNodeControlServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	defer listener.Close()
	connection, err := grpc.NewClient(
		"passthrough:///secure-node-control-bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	recipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	executor := newTestSlotCommandExecutor(t, providerfake.New(), now)
	client, err := NewControlClient(ControlClientConfig{
		Client: executionv1.NewNodeControlServiceClient(connection), Executor: executor,
		ActivationExecutor: scriptedActivationExecutor{keyID: keyID, publicKey: publicKey}, NodeID: "srv74",
		Capabilities: []string{"docker", "secure_activation"},
		Capacity: &executionv1.Capacity{
			MaxSlots: 20, MaxActiveCli: 4, MaxActiveApi: 12, MaxActiveTotal: 12,
			AllocatableCpuMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
		},
		HeartbeatInterval: 20 * time.Millisecond, ReconnectMin: 10 * time.Millisecond, ReconnectMax: 50 * time.Millisecond,
		MaxConcurrentCommands: 2, CommandQueue: 8, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() { runResult <- client.Run(ctx) }()
	select {
	case result := <-server.keyResult:
		if !result.GetSucceeded() || credential.ValidateRecipientKey(result.GetCredentialTransportKey().GetKeyId(), result.GetCredentialTransportKey().GetPublicKey()) != nil {
			t.Fatalf("credential-key result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for credential-key result")
	}
	select {
	case commit := <-server.commit:
		if commit.GetCommandId() != "cmd-activate-control" || commit.GetAccountBinding() != provider.RuntimeAccountID("account-control") || string(commit.GetSealedCredentialBundle()) != "rotation-ciphertext" {
			t.Fatalf("bridged credential commit = %+v", commit)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for bridged credential commit")
	}
	select {
	case result := <-server.activation:
		if !result.GetSucceeded() {
			t.Fatalf("secure activation result = %+v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for secure activation result")
	}
	cancel()
	select {
	case err := <-runResult:
		if err != nil {
			t.Fatalf("control client shutdown error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("control client did not stop")
	}
}

func TestControlCredentialSinkReturnsOnlyValidatedAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	forwards := make(chan credentialCommitForward, 1)
	sink := controlCredentialSink{ctx: ctx, commandID: "cmd-activate", forwards: forwards}
	result := make(chan error, 1)
	go func() {
		_, err := sink.CommitSealedCredential(ctx, worker.SealedCredentialCommitRequest{
			AccountBinding: provider.RuntimeAccountID("account-1"), SlotID: "slot-1", ExecutionEpoch: 3,
			CredentialLeaseID: "lease-1", ProxyLeaseID: "proxy-1", SealedCredentialBundle: []byte("sealed"),
		})
		result <- err
	}()
	forward := <-forwards
	forward.result <- credentialCommitResult{err: worker.ErrCredentialCommitRejected}
	if err := <-result; !errors.Is(err, worker.ErrCredentialCommitRejected) {
		t.Fatalf("rejected sink ack error = %v", err)
	}
	if !validCredentialCommitAck(&executionv1.ControlCredentialCommitAck{CommandId: "cmd-activate", Accepted: false, ErrorCode: "commit_rejected"}) {
		t.Fatal("generic rejected credential commit ack was not accepted by the host-agent validator")
	}
	if validCredentialCommitAck(&executionv1.ControlCredentialCommitAck{CommandId: "cmd-activate", Accepted: true, VersionId: "bad\nversion"}) {
		t.Fatal("invalid credential version acknowledgement was accepted")
	}
}
