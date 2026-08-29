package reconcile

import (
	"context"
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
	if len(command.GetMetadata()) != 1 {
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

type capturingDispatcher struct {
	nodeID   string
	response *executionv1.NodeControlServiceControlResponse
}

func (d *capturingDispatcher) Dispatch(_ context.Context, nodeID string, response *executionv1.NodeControlServiceControlResponse) error {
	d.nodeID = nodeID
	d.response = response
	return nil
}
