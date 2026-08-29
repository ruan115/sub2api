package reconcile

import (
	"context"
	"errors"
	"strconv"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type ControlDispatcher interface {
	Dispatch(ctx context.Context, nodeID string, response *executionv1.NodeControlServiceControlResponse) error
}

type ControlExecutor struct {
	dispatcher ControlDispatcher
	deadline   time.Duration
	now        func() time.Time
}

func NewControlExecutor(dispatcher ControlDispatcher, deadline time.Duration, now func() time.Time) (*ControlExecutor, error) {
	if dispatcher == nil || deadline <= 0 {
		return nil, errors.New("control dispatcher and positive command deadline are required")
	}
	if now == nil {
		now = time.Now
	}
	return &ControlExecutor{dispatcher: dispatcher, deadline: deadline, now: now}, nil
}

func (e *ControlExecutor) Execute(ctx context.Context, action Action) error {
	commandAction, ok := controlAction(action.Kind)
	if !ok || action.CommandID == "" || action.NodeID == "" || action.SlotID == "" ||
		action.AccountID == "" || action.ExecutionEpoch == 0 {
		return errors.New("reconcile action cannot be dispatched to a node")
	}
	response := &executionv1.NodeControlServiceControlResponse{
		Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: &executionv1.SlotCommand{
			CommandId: action.CommandID, Action: commandAction, SlotId: action.SlotID,
			AccountId: action.AccountID, ExecutionEpoch: action.ExecutionEpoch, ImageDigest: action.ImageDigest,
			Deadline: timestamppb.New(e.now().UTC().Add(e.deadline)),
			Metadata: map[string]string{"desired_generation": strconv.FormatUint(action.DesiredGeneration, 10)},
		}},
	}
	return e.dispatcher.Dispatch(ctx, action.NodeID, response)
}

func controlAction(kind ActionKind) (executionv1.SlotCommandAction, bool) {
	switch kind {
	case ActionCreate:
		return executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_CREATE, true
	case ActionStart:
		return executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START, true
	case ActionDrain:
		return executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DRAIN, true
	case ActionDestroy:
		return executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DESTROY, true
	case ActionInspect:
		return executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, true
	default:
		return executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_UNSPECIFIED, false
	}
}

var _ Executor = (*ControlExecutor)(nil)
