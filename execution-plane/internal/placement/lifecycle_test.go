package placement

import (
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/nodepolicy"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestLifecycleOnlyNeverEntersBusinessPlacement(t *testing.T) {
	for _, marker := range []string{"label", "capability", "both"} {
		for _, sticky := range []bool{false, true} {
			t.Run(marker+map[bool]string{false: "/new", true: "/sticky"}[sticky], func(t *testing.T) {
				now := time.Now()
				node := placementNode("node-lifecycle", "zone-a", now)
				if marker != "capability" {
					node.Labels[nodepolicy.ModeLabel] = nodepolicy.LifecycleOnlyMode
				}
				if marker != "label" {
					node.Capabilities = append(node.Capabilities, nodepolicy.LifecycleOnlyCapability)
				}
				request := placementRequest("")
				// Even an unconstrained request, or one explicitly requiring the
				// deny marker, must not opt a lifecycle node into scheduling.
				request.RequiredLabels, request.RequiredCapabilities, request.ImageDigest = nil, nil, ""
				if sticky {
					request.CurrentNodeID = node.ID
				}
				snapshot := Snapshot{Nodes: []store.Node{node}, Now: now, OfflineAfter: time.Minute}
				if _, err := Select(snapshot, request); !errors.Is(err, ErrNoEligibleNode) {
					t.Fatal("lifecycle node selected")
				}
				request.RequiredCapabilities = []string{nodepolicy.LifecycleOnlyCapability}
				if _, err := Select(snapshot, request); !errors.Is(err, ErrNoEligibleNode) {
					t.Fatal("deny marker became opt-in")
				}
				request.RequiredCapabilities = nil
				snapshot.Nodes = append(snapshot.Nodes, placementNode("business", "zone-b", now))
				decision, err := Select(snapshot, request)
				if err != nil || decision.NodeID != "business" || decision.Sticky {
					t.Fatal("eligible business node lost")
				}
			})
		}
	}
}
