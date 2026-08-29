package reconcile

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/placement"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

type PlacementSource interface {
	PlacementInput(ctx context.Context, slotID string) (placement.Snapshot, placement.Request, error)
}

type RuntimeExecutor struct {
	slots        store.SlotRepository
	placements   PlacementSource
	control      *ControlExecutor
	offlineAfter time.Duration
	now          func() time.Time
}

func NewRuntimeExecutor(slots store.SlotRepository, placements PlacementSource, control *ControlExecutor, offlineAfter time.Duration, now func() time.Time) (*RuntimeExecutor, error) {
	if slots == nil || placements == nil || control == nil || offlineAfter <= 0 {
		return nil, errors.New("slot repository, placement source, control executor and offline threshold are required")
	}
	if now == nil {
		now = time.Now
	}
	return &RuntimeExecutor{slots: slots, placements: placements, control: control, offlineAfter: offlineAfter, now: now}, nil
}

func (e *RuntimeExecutor) Execute(ctx context.Context, action Action) error {
	switch action.Kind {
	case ActionPlace:
		snapshot, request, err := e.placements.PlacementInput(ctx, action.SlotID)
		if err != nil {
			return err
		}
		now := e.now().UTC()
		snapshot.Now = now
		snapshot.OfflineAfter = e.offlineAfter
		decision, err := placement.Select(snapshot, request)
		if err != nil {
			return err
		}
		var selected *store.Node
		for index := range snapshot.Nodes {
			if snapshot.Nodes[index].ID == decision.NodeID {
				selected = &snapshot.Nodes[index]
				break
			}
		}
		if selected == nil || selected.ControlSessionID == "" {
			return store.ErrNodeCapacity
		}
		_, err = e.slots.ReserveAssignment(ctx, store.AssignmentReservation{
			ID: action.CommandID, SlotID: action.SlotID, NodeID: selected.ID,
			ExpectedNodeSessionID: selected.ControlSessionID,
			NodeSeenAfter:         now.Add(-e.offlineAfter), ReservedAt: now,
		})
		return err
	case ActionRelease:
		return e.slots.ReleaseAssignment(ctx, action.SlotID, action.ExecutionEpoch, e.now().UTC())
	default:
		return e.control.Execute(ctx, action)
	}
}

var _ Executor = (*RuntimeExecutor)(nil)
