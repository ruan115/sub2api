package hostagent

import (
	"context"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newTestSlotCommandExecutor(t *testing.T, implementation SlotCommandProvider, now time.Time) *SlotCommandExecutor {
	t.Helper()
	executor, err := NewSlotCommandExecutor(SlotCommandExecutorConfig{
		Provider:  implementation,
		Resources: provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 256 << 20},
		Security: provider.SecurityPolicy{
			RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true,
			SeccompProfile: "worker", AppArmorProfile: "worker",
		},
		Network: provider.NetworkPolicy{
			DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080",
		},
		DrainTimeout: time.Minute, MaxSlots: 20, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor
}

func slotCommand(now time.Time, id string, action executionv1.SlotCommandAction, epoch uint64) *executionv1.SlotCommand {
	return &executionv1.SlotCommand{
		CommandId: id, Action: action, SlotId: "slot-10380", AccountId: "account-10380", ExecutionEpoch: epoch,
		ImageDigest: "sha256:" + strings.Repeat("a", 64), Deadline: timestamppb.New(now.Add(time.Minute)),
	}
}

func TestSlotCommandExecutorLifecycleRestartRecoveryAndEpochFence(t *testing.T) {
	now := time.Now().UTC()
	implementation := providerfake.New()
	executor := newTestSlotCommandExecutor(t, implementation, now)
	create := executor.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-create", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE, 9))
	if !create.GetSucceeded() || create.GetSlot().GetActualState() != "created" || create.GetSlot().GetProviderRef() == "" {
		t.Fatalf("create result = %+v", create)
	}
	start := executor.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-start", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START, 9))
	if !start.GetSucceeded() || start.GetSlot().GetActualState() != "running" || !start.GetSlot().GetHealthy() {
		t.Fatalf("start result = %+v", start)
	}
	snapshot := executor.Snapshot()
	if snapshot.AllocatedSlots != 1 || snapshot.AllocatedCPUMillis != 500 || snapshot.AllocatedMemoryBytes != 512<<20 || len(snapshot.Slots) != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	// A fresh host-agent process resolves the stable slot id through the
	// provider instead of relying on an in-memory container-id map.
	recovered := newTestSlotCommandExecutor(t, implementation, now)
	inspect := recovered.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-inspect", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, 9))
	if !inspect.GetSucceeded() || inspect.GetSlot().GetProviderRef() != create.GetSlot().GetProviderRef() || inspect.GetSlot().GetActualState() != "running" {
		t.Fatalf("recovered inspect result = %+v", inspect)
	}
	revoked := recovered.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{
		CommandId: "cmd-revoke", SlotId: "slot-10380", ExecutionEpoch: 9, Reason: "lease_expired",
	})
	if !revoked.GetSucceeded() || revoked.GetSlot().GetActualState() != "stopped" {
		t.Fatalf("revoke result = %+v", revoked)
	}
	blocked := recovered.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-restart-revoked", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START, 9))
	if blocked.GetSucceeded() || blocked.GetErrorCode() != "execution_epoch_revoked" {
		t.Fatalf("revoked start result = %+v", blocked)
	}
	destroyed := recovered.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-destroy", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DESTROY, 9))
	if !destroyed.GetSucceeded() || destroyed.GetSlot().GetActualState() != "destroyed" || recovered.Snapshot().AllocatedSlots != 0 {
		t.Fatalf("destroy result/snapshot = %+v/%+v", destroyed, recovered.Snapshot())
	}
}

func TestSlotCommandExecutorFailsClosedOnDeadlineAndEpochConflict(t *testing.T) {
	now := time.Now().UTC()
	implementation := providerfake.New()
	executor := newTestSlotCommandExecutor(t, implementation, now)
	if result := executor.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-create", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE, 1)); !result.GetSucceeded() {
		t.Fatal(result)
	}
	conflict := executor.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-conflict", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, 2))
	if conflict.GetSucceeded() || conflict.GetErrorCode() != "execution_epoch_conflict" || conflict.GetSlot().GetExecutionEpoch() != 2 {
		t.Fatalf("epoch conflict result = %+v", conflict)
	}
	expired := slotCommand(now, "cmd-expired", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, 1)
	expired.Deadline = timestamppb.New(now.Add(-time.Second))
	result := executor.ExecuteSlotCommand(context.Background(), expired)
	if result.GetSucceeded() || result.GetErrorCode() != "command_deadline_exceeded" {
		t.Fatalf("expired command result = %+v", result)
	}
}

func TestSlotCommandExecutorCapacityAndDelayedRevocation(t *testing.T) {
	now := time.Now().UTC()
	implementation := providerfake.New()
	executor := newTestSlotCommandExecutor(t, implementation, now)
	executor.maxSlots = 1
	if result := executor.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-create-new", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE, 10)); !result.GetSucceeded() {
		t.Fatal(result)
	}
	if result := executor.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-start-new", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START, 10)); !result.GetSucceeded() {
		t.Fatal(result)
	}
	delayed := executor.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{
		CommandId: "cmd-revoke-old", SlotId: "slot-10380", ExecutionEpoch: 9, Reason: "delayed",
	})
	if !delayed.GetSucceeded() || delayed.GetSlot().GetActualState() != "missing" {
		t.Fatalf("delayed revoke result = %+v", delayed)
	}
	inspect := executor.ExecuteSlotCommand(context.Background(), slotCommand(now, "cmd-inspect-new", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, 10))
	if !inspect.GetSucceeded() || inspect.GetSlot().GetActualState() != "running" {
		t.Fatalf("new epoch after delayed revoke = %+v", inspect)
	}
	second := slotCommand(now, "cmd-create-over-capacity", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE, 1)
	second.SlotId = "slot-second"
	second.AccountId = "account-second"
	result := executor.ExecuteSlotCommand(context.Background(), second)
	if result.GetSucceeded() || result.GetErrorCode() != "node_slot_capacity" {
		t.Fatalf("capacity result = %+v", result)
	}
}
