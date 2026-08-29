package hostagent

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
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
