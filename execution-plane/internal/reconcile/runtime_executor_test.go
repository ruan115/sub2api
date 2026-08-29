package reconcile

import (
	"context"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/placement"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestRuntimeReconcilePlacesAtomicallyThenDispatchesCreate(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := connectedRuntimeRepository(t, now)
	digest := "sha256:" + strings.Repeat("a", 64)
	desired, err := repository.PutDesiredSlot(context.Background(), store.Slot{
		ID: "slot-1", AccountID: "account-1", Provider: "docker", DesiredState: "ready", DesiredGeneration: 1,
		RequiredLabels: map[string]string{"region": "ap-shanghai"}, ImageDigest: digest,
		CPURequestMillis: 500, MemoryRequestBytes: 128 << 20, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := repository.GetNode(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	source := staticPlacementSource{
		snapshot: placement.Snapshot{Nodes: []store.Node{node}},
		request: placement.Request{
			SlotID: desired.ID, RequiredLabels: desired.RequiredLabels,
			RequiredCapabilities: []string{"docker"}, ImageDigest: desired.ImageDigest,
			CPURequestMillis: desired.CPURequestMillis, MemoryRequestBytes: desired.MemoryRequestBytes,
		},
	}
	dispatcher := &capturingDispatcher{}
	controlExecutor, err := NewControlExecutor(dispatcher, time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	runtimeExecutor, err := NewRuntimeExecutor(repository, source, controlExecutor, 45*time.Second, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(repository, runtimeExecutor, Config{
		ClaimTTL: 30 * time.Second, RetryDelay: 5 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Slot: Slot{
		ID: desired.ID, AccountID: desired.AccountID, DesiredState: DesiredReady,
		DesiredGeneration: desired.DesiredGeneration, ImageDigest: desired.ImageDigest,
	}}
	placed, err := controller.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !placed.Claimed || !placed.Completed || placed.Dispatched || placed.Action.Kind != ActionPlace {
		t.Fatalf("placement result = %+v", placed)
	}
	assignment, err := repository.GetActiveAssignment(context.Background(), desired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if assignment.NodeID != "srv74" || assignment.ExecutionEpoch != 1 || assignment.ActualState != "missing" {
		t.Fatalf("reserved assignment = %+v", assignment)
	}
	input.Assignment = &Assignment{
		ID: assignment.ID, SlotID: assignment.SlotID, NodeID: assignment.NodeID,
		ExecutionEpoch: assignment.ExecutionEpoch, ActualGeneration: assignment.ActualGeneration,
		ImageDigest: assignment.ImageDigest, ActualState: ActualMissing,
	}
	created, err := controller.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Claimed || !created.Dispatched || created.Completed || created.Action.Kind != ActionCreate {
		t.Fatalf("create result = %+v", created)
	}
	command := dispatcher.response.GetSlotCommand()
	if dispatcher.nodeID != "srv74" || command.GetSlotId() != "slot-1" || command.GetExecutionEpoch() != 1 || command.GetCommandId() != created.Job.ID {
		t.Fatalf("create command = %+v on node %s", command, dispatcher.nodeID)
	}
	if err := repository.ApplyCommandResult(context.Background(), store.CommandResult{
		CommandID: created.Job.ID, NodeID: "srv74", Succeeded: true,
		SlotObservationJSON: []byte(`{"slotId":"slot-1","executionEpoch":"1","actualState":"created"}`),
		Observation: &store.AssignmentObservation{
			SlotID: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ActualState: "created", ObservedAt: now,
		},
		ReceivedAt: now, RetryAt: now.Add(5 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	observed, err := repository.GetActiveAssignment(context.Background(), "slot-1")
	if err != nil {
		t.Fatal(err)
	}
	if observed.ActualState != "created" || observed.ActualGeneration != 2 {
		t.Fatalf("applied create result = %+v", observed)
	}
	input.Assignment.ActualState = ActualCreated
	input.Assignment.ActualGeneration = observed.ActualGeneration
	started, err := controller.Reconcile(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if started.Action.Kind != ActionStart || !started.Dispatched || dispatcher.response.GetSlotCommand().GetAction() != executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START {
		t.Fatalf("start result = %+v, command = %+v", started, dispatcher.response.GetSlotCommand())
	}
}

type staticPlacementSource struct {
	snapshot placement.Snapshot
	request  placement.Request
	err      error
}

func (s staticPlacementSource) PlacementInput(context.Context, string) (placement.Snapshot, placement.Request, error) {
	return s.snapshot, s.request, s.err
}

func connectedRuntimeRepository(t *testing.T, now time.Time) *store.MemoryRepository {
	t.Helper()
	repository := store.NewMemoryRepository()
	token := store.HashToken("runtime-reconcile-token")
	if err := repository.CreateEnrollment(context.Background(), store.Enrollment{
		ID: "enrollment-1", TokenSHA256: token, ExpectedNodeID: "srv74", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitEnrollment(context.Background(), token, store.Node{
		ID: "srv74", Status: "active", ProtocolMajor: 1, CreatedAt: now, UpdatedAt: now,
	}, store.Certificate{
		SerialNumber: "01", NodeID: "srv74", CertificateSHA256: sha256.Sum256([]byte("certificate")),
		PublicKeySHA256: sha256.Sum256([]byte("public-key")), Status: "active",
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	digestCapability := "image.sha256." + strings.Repeat("a", 64)
	if err := repository.AcceptHello(context.Background(), store.Hello{
		NodeID: "srv74", SessionID: "session-1", ProtocolMajor: 1,
		Labels:       map[string]string{"region": "ap-shanghai", "zone": "az-1"},
		Capabilities: []string{"docker", digestCapability},
		Capacity: store.Capacity{
			MaxSlots: 20, MaxActiveCLI: 4, MaxActiveAPI: 12, MaxActiveTotal: 12,
			AllocatableCPUMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
		},
		ReceivedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return repository
}
