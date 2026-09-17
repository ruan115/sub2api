package reconcile

import (
	"context"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
)

func TestControlExecutorMapsSafeBoundedSlotCommand(t *testing.T) {
	dispatcher := &capturingDispatcher{}
	now := time.Unix(2_000_000_000, 0).UTC()
	executor, err := NewControlExecutor(dispatcher, 2*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	action := newAction(ActionCreate, Input{Slot: testSlot(DesiredReady), Assignment: testAssignment(ActualMissing, false)})
	action.CommandID = "command-1"
	if err := executor.Execute(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	command := dispatcher.response.GetSlotCommand()
	if dispatcher.nodeID != "srv74" || command.GetAction() != executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE ||
		command.GetCommandId() != "command-1" || command.GetExecutionEpoch() != 7 ||
		command.GetMetadata()["desired_generation"] != "3" || !command.GetDeadline().AsTime().Equal(now.Add(2*time.Minute)) {
		t.Fatalf("dispatched command = %+v on %s", command, dispatcher.nodeID)
	}
	if len(command.GetMetadata()) != 2 || command.GetMetadata()["target_runtime_generation"] != "3" {
		t.Fatalf("unexpected command metadata: %+v", command.GetMetadata())
	}
}

func TestControlExecutorRejectsPlacementAsNodeCommand(t *testing.T) {
	executor, err := NewControlExecutor(&capturingDispatcher{}, time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := executor.Execute(context.Background(), Action{Kind: ActionPlace}); err == nil {
		t.Fatal("placement was incorrectly sent as a node command")
	}
}

func TestControlExecutorKeepsCleanupIntentAndRuntimeGenerationSeparate(t *testing.T) {
	dispatcher := &capturingDispatcher{}
	executor, err := NewControlExecutor(dispatcher, time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Slot: testSlot(DesiredAbsent), Assignment: testAssignment(ActualRunning, true)}
	input.Slot.DesiredGeneration = 4
	input.Slot.ImageDigest = "sha256:" + strings.Repeat("b", 64)
	input.Assignment.DesiredGeneration = 3
	action, err := Plan(input)
	if err != nil || action.Kind != ActionDrain || action.DesiredGeneration != 4 || action.RuntimeGeneration != 3 {
		t.Fatal("cleanup lost old assignment generation")
	}
	action.CommandID = "cleanup-generation-3"
	if err := executor.Execute(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	metadata := dispatcher.response.GetSlotCommand().GetMetadata()
	if metadata["desired_generation"] != "4" || metadata["target_runtime_generation"] != "3" ||
		dispatcher.response.GetSlotCommand().GetImageDigest() != input.Assignment.ImageDigest {
		t.Fatal("cleanup command conflated intent and target generations")
	}
	action.Kind = ActionStart
	if err := executor.Execute(context.Background(), action); err == nil {
		t.Fatal("cleanup binding was accepted as a start command")
	}
}

type capturingDispatcher struct {
	nodeID   string
	response *executionv1.NodeControlServiceControlResponse
}

func (d *capturingDispatcher) Dispatch(_ context.Context, nodeID string, response *executionv1.NodeControlServiceControlResponse) error {
	d.nodeID = nodeID
	d.response = response
	return nil
}
