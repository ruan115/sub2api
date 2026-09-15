package control

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/executionauthority"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type sessionRepository struct {
	store.NodeRepository
	validate func(context.Context, string, string, time.Time) error
	node     func(context.Context, string) (store.Node, error)
}

func reserveControlTestAssignment(t *testing.T, h *controlHarness, image string) store.Assignment {
	t.Helper()
	ctx := context.Background()
	node, err := h.repository.GetNode(ctx, "srv74")
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.repository.PutDesiredSlot(ctx, store.Slot{ID: "slot-1", AccountID: "account-1", Provider: "docker", DesiredState: "ready", DesiredGeneration: 1,
		ImageDigest: image, CPURequestMillis: 100, MemoryRequestBytes: 1 << 20, CreatedAt: h.now, UpdatedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := h.repository.ReserveAssignment(ctx, store.AssignmentReservation{ID: "assignment-1", SlotID: "slot-1", NodeID: "srv74",
		ExpectedNodeSessionID: node.ControlSessionID, NodeSeenAfter: h.now.Add(-time.Second), ReservedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	return assignment
}

func (r sessionRepository) ValidateCertificate(ctx context.Context, node, serial string, now time.Time) error {
	return r.validate(ctx, node, serial, now)
}
func (r sessionRepository) GetNode(ctx context.Context, node string) (store.Node, error) {
	return r.node(ctx, node)
}

func standaloneSession(t *testing.T) (*Server, *nodeSession, context.CancelFunc) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	authority, _, err := pki.NewEphemeralAuthority(func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, _, key := generateNodeKey(t)
	issued, err := authority.IssueNode("node-1", key)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
		Version: tls.VersionTLS13, PeerCertificates: []*x509.Certificate{issued.Certificate}, VerifiedChains: [][]*x509.Certificate{{issued.Certificate}},
	}}}))
	t.Cleanup(cancel)
	session := &nodeSession{id: strings.Repeat("a", 32), accepted: true, context: ctx, done: make(chan struct{})}
	repository := sessionRepository{
		validate: func(context.Context, string, string, time.Time) error { return nil },
		node: func(context.Context, string) (store.Node, error) {
			return store.Node{ID: "node-1", Status: "connected", ControlSessionID: session.id}, nil
		},
	}
	server := &Server{repository: repository, config: Config{Now: func() time.Time { return now }}, sessions: map[string]*nodeSession{"node-1": session}}
	return server, session, cancel
}

func TestCurrentSessionRequiresVerifiedLiveTransportAndDurableIdentity(t *testing.T) {
	for _, test := range []string{"valid", "not accepted", "missing context", "cancelled stream", "closed", "tls12", "no chain", "different leaf", "no tls", "expired leaf", "revoked", "storage failure", "durable old session", "durable disconnected"} {
		t.Run(test, func(t *testing.T) {
			server, session, cancel := standaloneSession(t)
			repository := server.repository.(sessionRepository)
			remote, _ := peer.FromContext(session.context)
			info := remote.AuthInfo.(credentials.TLSInfo)
			switch test {
			case "not accepted":
				session.accepted = false
			case "missing context":
				session.context = nil
			case "cancelled stream":
				cancel()
			case "closed":
				close(session.done)
			case "tls12":
				info.State.Version = tls.VersionTLS12
			case "no chain":
				info.State.VerifiedChains = nil
			case "different leaf":
				info.State.VerifiedChains = [][]*x509.Certificate{{{Raw: []byte("different")}}}
			case "no tls":
				session.context = context.Background()
			case "expired leaf":
				server.config.Now = func() time.Time { return info.State.PeerCertificates[0].NotAfter }
			case "revoked":
				repository.validate = func(context.Context, string, string, time.Time) error { return store.ErrCertificateNotActive }
			case "storage failure":
				repository.validate = func(context.Context, string, string, time.Time) error { return errors.New("private database detail") }
			case "durable old session":
				repository.node = func(context.Context, string) (store.Node, error) {
					return store.Node{ID: "node-1", Status: "connected", ControlSessionID: strings.Repeat("b", 32)}, nil
				}
			case "durable disconnected":
				repository.node = func(context.Context, string) (store.Node, error) {
					return store.Node{ID: "node-1", Status: "disconnected", ControlSessionID: session.id}, nil
				}
			}
			if test == "tls12" || test == "no chain" || test == "different leaf" {
				session.context = peer.NewContext(session.context, &peer.Peer{AuthInfo: info})
			}
			server.repository = repository
			err := server.ValidateControlSession(context.Background(), "node-1", session.id)
			if test == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != ErrControlSessionUnavailable {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestCurrentSessionRejectsChangesDuringStorageValidation(t *testing.T) {
	for _, change := range []string{"detach", "stream cancel", "replace", "caller cancel", "expiry"} {
		t.Run(change, func(t *testing.T) {
			server, session, cancelStream := standaloneSession(t)
			entered, release := make(chan struct{}), make(chan struct{})
			repository := server.repository.(sessionRepository)
			repository.validate = func(ctx context.Context, _, _ string, _ time.Time) error {
				close(entered)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			server.repository = repository
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- server.ValidateControlSession(ctx, "node-1", session.id) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("storage check not reached")
			}
			switch change {
			case "detach":
				server.detach("node-1", session.id)
			case "stream cancel":
				cancelStream()
			case "replace":
				server.mu.Lock()
				server.sessions["node-1"] = &nodeSession{id: strings.Repeat("b", 32), accepted: true, context: context.Background(), done: make(chan struct{})}
				server.mu.Unlock()
			case "caller cancel":
				cancel()
			case "expiry":
				// The clock closure is changed only while validation is held in
				// the channel-synchronized repository callback.
				remote, _ := peer.FromContext(session.context)
				expiry := remote.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0].NotAfter
				server.config.Now = func() time.Time { return expiry }
			}
			close(release)
			select {
			case err := <-result:
				if err != ErrControlSessionUnavailable {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("validation held session lock during I/O")
			}
		})
	}
}

func TestCurrentSessionInvalidInputsNeverReachStorage(t *testing.T) {
	server, session, _ := standaloneSession(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, request := range []struct {
		ctx      context.Context
		node, id string
	}{
		{nil, "node-1", session.id}, {ctx, "node-1", session.id}, {context.Background(), "other-node", session.id},
		{context.Background(), "node-1", strings.Repeat("b", 32)}, {context.Background(), "node-1", ""},
	} {
		if err := server.ValidateControlSession(request.ctx, request.node, request.id); err != ErrControlSessionUnavailable {
			t.Fatal(err)
		}
	}
	var absent *Server
	if absent.ValidateControlSession(context.Background(), "node-1", session.id) != ErrControlSessionUnavailable {
		t.Fatal("nil server accepted")
	}
}

func TestAuthenticatedCommandObservationSnapshotRequiresNewSessionConfirmation(t *testing.T) {
	h := newControlHarness(t, 5*time.Second)
	stream := enrollAndOpenControl(t, h, helloEvent("srv74"))
	ctx := context.Background()
	node, err := h.repository.GetNode(ctx, "srv74")
	if err != nil {
		t.Fatal(err)
	}
	sessionID := node.ControlSessionID
	eventually(t, func() bool { return h.server.ValidateControlSession(ctx, "srv74", sessionID) == nil })
	image := "sha256:" + strings.Repeat("a", 64)
	_, err = h.repository.PutDesiredSlot(ctx, store.Slot{ID: "slot-1", AccountID: "account-1", Provider: "docker", DesiredState: "ready", DesiredGeneration: 1,
		ImageDigest: image, CPURequestMillis: 100, MemoryRequestBytes: 1 << 20, CreatedAt: h.now, UpdatedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := h.repository.ReserveAssignment(ctx, store.AssignmentReservation{ID: "assignment-1", SlotID: "slot-1", NodeID: "srv74",
		ExpectedNodeSessionID: sessionID, NodeSeenAfter: h.now.Add(-time.Second), ReservedAt: h.now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.repository.GrantExecutionLease(ctx, store.ExecutionLease{ID: "lease-1", SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: assignment.ExecutionEpoch,
		OwnerID: "owner-1", CreatedAt: h.now, UpdatedAt: h.now, ExpiresAt: h.now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	source, err := executionauthority.NewSource(executionauthority.Config{NodeID: "srv74", Repository: h.repository, Sessions: h.server, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	check := func(allowed bool) {
		t.Helper()
		got, err := source.Snapshot(ctx, "slot-1")
		if allowed {
			if err != nil || !got.Ready || got.AccountID != "account-1" || !got.ObservedAt.Equal(h.now) {
				t.Fatalf("snapshot=%+v err=%v", got, err)
			}
		} else if err != dataplane.ErrBindingUnavailable || got != (dataplane.Snapshot{}) {
			t.Fatalf("unexpected snapshot=%+v err=%v", got, err)
		}
	}
	inspect := func(id string) {
		t.Helper()
		command := &executionv1.SlotCommand{CommandId: id, Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT,
			SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: assignment.ExecutionEpoch, ImageDigest: image, Deadline: timestamppb.New(h.now.Add(time.Minute))}
		if err := h.server.Dispatch(ctx, "srv74", &executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: command}}); err != nil {
			t.Fatal(err)
		}
		if received, err := stream.Recv(); err != nil || received.GetSlotCommand().GetCommandId() != id {
			t.Fatalf("command=%+v err=%v", received, err)
		}
		if err := stream.Send(&executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_CommandResult{CommandResult: &executionv1.CommandResult{
			CommandId: id, Succeeded: true, Slot: &executionv1.SlotObservation{SlotId: "slot-1", ExecutionEpoch: assignment.ExecutionEpoch, ProviderRef: "container-1", ImageDigest: image, ActualState: "running", Healthy: true},
		}}}); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { _, exists := h.repository.GetCommandResult(id); return exists })
	}
	check(false)
	// Matching the issued command is insufficient if that command's image
	// is not the authoritative assignment image. No proof may be persisted.
	wrongImage := "sha256:" + strings.Repeat("b", 64)
	wrongPending := &nodeSession{id: sessionID, pendingCommands: map[string]pendingCommand{"wrong-image": {
		kind: pendingSlotCommand, slotID: "slot-1", executionEpoch: assignment.ExecutionEpoch, imageDigest: wrongImage, deadline: h.now.Add(time.Minute),
	}}}
	err = h.server.recordCommandResult(ctx, "srv74", wrongPending, &executionv1.CommandResult{CommandId: "wrong-image", Succeeded: true,
		Slot: &executionv1.SlotObservation{SlotId: "slot-1", ExecutionEpoch: assignment.ExecutionEpoch, ProviderRef: "container-1", ImageDigest: wrongImage, ActualState: "running", Healthy: true}})
	if err == nil {
		t.Fatal("pending/result image mismatch with assignment accepted")
	}
	if _, exists := h.repository.GetCommandResult("wrong-image"); exists {
		t.Fatal("wrong image result partially persisted")
	}
	check(false)
	inspect("inspect-old")
	check(true)
	record, _ := h.repository.GetCommandResult("inspect-old")
	if record.ControlSessionID != sessionID {
		t.Fatal("control did not attach its own session identity")
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("close stream=%v", err)
	}
	check(false)
	// Reuse the same node certificate on a fresh authenticated control stream.
	stream, err = executionv1.NewNodeControlServiceClient(h.connections[len(h.connections)-1]).Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		node, _ := h.repository.GetNode(ctx, "srv74")
		return node.Status == "connected" && node.ControlSessionID != sessionID
	})
	check(false)
	if err := stream.Send(heartbeatEvent("srv74", h.now, 0, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { node, _ := h.repository.GetNode(ctx, "srv74"); return node.AllocatedSlots == 1 })
	check(false)
	// A late old-session result must be rejected, not persisted in the new session.
	late := &nodeSession{id: sessionID, pendingCommands: map[string]pendingCommand{"late-old": {kind: pendingSlotCommand, slotID: "slot-1",
		executionEpoch: assignment.ExecutionEpoch, imageDigest: image, deadline: h.now.Add(time.Minute)}}}
	err = h.server.recordCommandResult(ctx, "srv74", late, &executionv1.CommandResult{CommandId: "late-old", Succeeded: true,
		Slot: &executionv1.SlotObservation{SlotId: "slot-1", ExecutionEpoch: assignment.ExecutionEpoch, ProviderRef: "container-1", ImageDigest: image, ActualState: "running", Healthy: true}})
	if err == nil {
		t.Fatal("late result accepted")
	}
	if _, exists := h.repository.GetCommandResult("late-old"); exists {
		t.Fatal("late result partially persisted")
	}
	check(false)
	inspect("inspect-new")
	check(true)
	// Revocation must reject even while the old transport remains connected.
	h.server.mu.RLock()
	active := h.server.sessions["srv74"]
	h.server.mu.RUnlock()
	remote, _ := peer.FromContext(active.context)
	leaf := remote.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
	_, _, key := generateNodeKey(t)
	issued, err := h.authority.IssueNode("srv74", key)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.repository.RotateCertificate(ctx, "srv74", pki.SerialString(leaf.SerialNumber), certificateRecord("srv74", issued, h.now), h.now); err != nil {
		t.Fatal(err)
	}
	check(false)
}

func TestHealthyObservationMustMatchCurrentIssuedImageAndDeadline(t *testing.T) {
	now := time.Now().UTC()
	image := "sha256:" + strings.Repeat("a", 64)
	for _, name := range []string{"missing image", "wrong image", "expired command", "missing deadline", "revoke", "credential key expired", "activation expired"} {
		t.Run(name, func(t *testing.T) {
			pending := pendingFromResponse(&executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
				SlotId: "slot-1", ExecutionEpoch: 1, ImageDigest: image, Deadline: timestamppb.New(now.Add(time.Second)),
			}}})
			if pending.imageDigest != image || !pending.deadline.Equal(now.Add(time.Second)) {
				t.Fatal("pending command lost image/deadline")
			}
			result := &executionv1.CommandResult{CommandId: "cmd-1", Succeeded: true, Slot: &executionv1.SlotObservation{
				SlotId: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ActualState: "running", Healthy: true, ImageDigest: image,
			}}
			switch name {
			case "missing image":
				result.Slot.ImageDigest = ""
			case "wrong image":
				result.Slot.ImageDigest = "sha256:" + strings.Repeat("b", 64)
			case "expired command":
				pending.deadline = now
			case "missing deadline":
				pending.deadline = time.Time{}
			case "revoke":
				pending.kind = pendingEpochRevocation
			case "credential key expired":
				pending.kind, pending.deadline = pendingCredentialKey, now
			case "activation expired":
				pending.kind, pending.deadline = pendingSecureActivation, now
			}
			// No repository: reaching a write would panic and fail the test.
			server := &Server{config: Config{Now: func() time.Time { return now }}}
			session := &nodeSession{id: strings.Repeat("a", 32), pendingCommands: map[string]pendingCommand{"cmd-1": pending}}
			if err := server.recordCommandResult(context.Background(), "node-1", session, result); err == nil {
				t.Fatal("unconfirmed healthy result accepted")
			}
		})
	}
}

type commandObserverFunc func(context.Context, string, *executionv1.CommandResult) error

func (f commandObserverFunc) ObserveCommandResult(ctx context.Context, node string, result *executionv1.CommandResult) error {
	return f(ctx, node, result)
}

type proofRepository struct {
	store.NodeRepository
	apply func(context.Context, store.CommandResult) error
}

func (r proofRepository) ApplyCommandResult(ctx context.Context, result store.CommandResult) error {
	return r.apply(ctx, result)
}

func TestHealthyObservationDoesNotRenewTimeAcrossObserverIO(t *testing.T) {
	for _, test := range []string{"within deadline", "observer expired", "observer cancelled"} {
		t.Run(test, func(t *testing.T) {
			observedAt := time.Now().UTC()
			now := observedAt
			deadline := observedAt.Add(time.Minute)
			image := "sha256:" + strings.Repeat("a", 64)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writes := 0
			session := &nodeSession{id: strings.Repeat("a", 32), pendingCommands: map[string]pendingCommand{"cmd-1": {
				kind: pendingSecureActivation, slotID: "slot-1", executionEpoch: 1, imageDigest: image, deadline: deadline,
			}}}
			server := &Server{config: Config{Now: func() time.Time { return now }, CommandRetryDelay: time.Second,
				CommandObserver: commandObserverFunc(func(ctx context.Context, _ string, _ *executionv1.CommandResult) error {
					if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
						t.Fatal("observer lost command deadline")
					}
					switch test {
					case "within deadline":
						now = now.Add(10 * time.Second)
					case "observer expired":
						now = deadline
					case "observer cancelled":
						cancel()
					}
					return nil
				}),
			}, repository: proofRepository{apply: func(ctx context.Context, result store.CommandResult) error {
				writes++
				if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
					t.Fatal("storage lost command deadline")
				}
				if !result.ReceivedAt.Equal(observedAt) || !result.Observation.ObservedAt.Equal(observedAt) || result.ControlSessionID != session.id || result.ExpectedImageDigest != image {
					t.Fatal("I/O refreshed observation time or dropped identity")
				}
				return nil
			}}}
			err := server.recordCommandResult(ctx, "node-1", session, &executionv1.CommandResult{CommandId: "cmd-1", Succeeded: true,
				Slot: &executionv1.SlotObservation{SlotId: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ActualState: "running", Healthy: true, ImageDigest: image}})
			if test == "within deadline" {
				if err != nil || writes != 1 {
					t.Fatalf("error=%v writes=%d", err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatalf("error=%v writes=%d", err, writes)
			}
		})
	}
}
