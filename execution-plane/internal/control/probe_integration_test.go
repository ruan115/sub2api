package control

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/executionauthority"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeprobe"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

// A synthetic provider with no lifecycle implementation: accidental calls to
// create/start/drain/stop/destroy panic. Only InspectSlot may be used here.
type inspectOnlyProbeProvider struct {
	provider.ExecutionProvider
	status provider.Status
	calls  int
}

func (p *inspectOnlyProbeProvider) InspectSlot(ctx context.Context, id string) (provider.Status, error) {
	if ctx.Err() != nil {
		return provider.Status{}, ctx.Err()
	}
	p.calls++
	if id != p.status.SlotID {
		return provider.Status{}, provider.ErrNotFound
	}
	return p.status, nil
}

func TestRuntimeProbeTLSInspectRefreshReconnectAndUnhealthy(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Int64
	clock.Store(time.Now().UTC().Truncate(time.Second).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	h := newControlHarnessWithConfig(t, 5*time.Second, func(c *Config) { c.Now = now })
	clock.Store(h.now.UnixNano())
	stream := enrollAndOpenControl(t, h, helloEvent("srv74"))
	node, _ := h.repository.GetNode(ctx, "srv74")
	eventually(t, func() bool { return h.server.ValidateControlSession(ctx, "srv74", node.ControlSessionID) == nil })
	image := "sha256:" + strings.Repeat("a", 64)
	assignment := reserveControlTestAssignment(t, h, image)
	// Persist an existing provider reference, but deliberately not a current
	// authenticated observation. This is not a new provisioning operation.
	if _, err := h.repository.ObserveAssignment(ctx, store.AssignmentObservation{SlotID: "slot-1", ExecutionEpoch: assignment.ExecutionEpoch,
		ProviderRef: "container-1", ActualState: "running", Healthy: true, ObservedAt: now()}); err != nil {
		t.Fatal(err)
	}
	_, err := h.repository.GrantExecutionLease(ctx, store.ExecutionLease{ID: "lease-1", SlotID: "slot-1", NodeID: "srv74",
		ExecutionEpoch: assignment.ExecutionEpoch, OwnerID: "owner-1", CreatedAt: now(), UpdatedAt: now(), ExpiresAt: now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	backend := lease.NewMemoryBackend(now)
	claim := lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: assignment.ExecutionEpoch, OwnerID: "owner-1"}
	if err := backend.Acquire(ctx, claim, time.Minute); err != nil {
		t.Fatal(err)
	}
	source, err := executionauthority.NewSource(executionauthority.Config{NodeID: "srv74", Repository: h.repository, Sessions: h.server, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	runner, err := runtimeprobe.New(runtimeprobe.Config{Repository: h.repository, Sessions: h.server, Leases: backend, Dispatcher: h.server, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	implementation := &inspectOnlyProbeProvider{status: provider.Status{Instance: provider.Instance{ProviderRef: "container-1", SlotID: "slot-1", Epoch: assignment.ExecutionEpoch, RuntimeGeneration: assignment.DesiredGeneration, State: slot.StateReady}, Healthy: true, ImageDigest: image}}
	executor, err := hostagent.NewSlotCommandExecutor(hostagent.SlotCommandExecutorConfig{
		Provider: implementation, Resources: provider.ResourceLimits{CPUMilli: 100, MemoryBytes: 1 << 20, PIDs: 16, TmpfsBytes: 1 << 20},
		Security:     provider.SecurityPolicy{RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "worker", AppArmorProfile: "worker"},
		Network:      provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080"},
		DrainTimeout: time.Second, MaxSlots: 20, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	check := func(allowed bool) {
		t.Helper()
		snapshot, err := source.Snapshot(ctx, "slot-1")
		if allowed {
			if err != nil || !snapshot.Ready || !snapshot.ObservedAt.Equal(now()) {
				t.Fatalf("snapshot=%+v err=%v", snapshot, err)
			}
		} else if err != dataplane.ErrBindingUnavailable {
			t.Fatalf("expected denied snapshot, got %+v/%v", snapshot, err)
		}
	}
	inspect := func() string {
		t.Helper()
		result, err := runner.Step(ctx)
		if err != nil || result.Dispatched != 1 {
			t.Fatal(result, err)
		}
		delivered, err := stream.Recv()
		if err != nil || delivered.GetSlotCommand().GetAction() != executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT {
			t.Fatal(delivered, err)
		}
		// A second round before the first result is single-flight rejected.
		if duplicate, err := runner.Step(ctx); err != nil || duplicate.Denied != 1 {
			t.Fatal(duplicate, err)
		}
		commandResult := executor.ExecuteSlotCommand(ctx, delivered.GetSlotCommand())
		if !commandResult.Succeeded {
			t.Fatal(commandResult)
		}
		if err := stream.Send(&executionv1.NodeControlServiceControlRequest{Event: &executionv1.NodeControlServiceControlRequest_CommandResult{CommandResult: commandResult}}); err != nil {
			t.Fatal(err)
		}
		eventually(t, func() bool { _, exists := h.repository.GetCommandResult(commandResult.CommandId); return exists })
		return commandResult.CommandId
	}
	check(false)
	backend.SetAvailable(false)
	if result, err := runner.Step(ctx); err != nil || result.Denied != 1 {
		t.Fatal(result, err)
	}
	check(false)
	backend.SetAvailable(true)
	first := inspect()
	check(true)
	if result, err := runner.Step(ctx); err != nil || result.Skipped != 1 {
		t.Fatal(result, err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	check(false)
	stream, err = executionv1.NewNodeControlServiceClient(h.connections[len(h.connections)-1]).Control(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(helloEvent("srv74")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		current, _ := h.repository.GetNode(ctx, "srv74")
		return current.ControlSessionID != node.ControlSessionID && h.server.ValidateControlSession(ctx, "srv74", current.ControlSessionID) == nil
	})
	if err := stream.Send(heartbeatEvent("srv74", now(), 0, 0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { current, _ := h.repository.GetNode(ctx, "srv74"); return current.AllocatedSlots == 1 })
	check(false) // hello + cached heartbeat do not restore an execution proof.
	second := inspect()
	if first == second {
		t.Fatal("probe command id was reused after reconnect")
	}
	check(true)
	clock.Add(int64(16 * time.Second))
	implementation.status.Healthy = false
	implementation.status.Reason = "provider_unhealthy"
	// The cached host snapshot is still healthy, but fresh INSPECT is not.
	if !executor.Snapshot().Slots[0].Healthy {
		t.Fatal("fixture lost cached healthy observation")
	}
	inspect()
	check(false)
	if implementation.calls != 3 || executor.Snapshot().Slots[0].Healthy {
		t.Fatal("probe did not inspect the actual provider each round")
	}
	before, _ := h.repository.ReadProbeBinding(ctx, "slot-1", now(), 45*time.Second)
	if err := backend.Revoke(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if result, err := runner.Step(ctx); err != nil || result.Denied != 1 {
		t.Fatal(result, err)
	}
	after, _ := h.repository.ReadProbeBinding(ctx, "slot-1", now(), 45*time.Second)
	if !before.LeaseExpiresAt.Equal(after.LeaseExpiresAt) || !before.LastObservedAt.Equal(*after.LastObservedAt) {
		t.Fatal("denied probe changed lease or observation")
	}
}
