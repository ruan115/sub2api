package placement

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestSelectKeepsEligibleCurrentNodeStable(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	current := placementNode("node-b", "zone-b", now)
	current.AllocatedSlots = 20
	current.AllocatedCPUMillis = current.Capacity.AllocatableCPUMillis
	current.AllocatedMemoryBytes = current.Capacity.AllocatableMemoryBytes
	empty := placementNode("node-a", "zone-a", now)
	decision, err := Select(Snapshot{
		Nodes: []store.Node{empty, current}, Now: now, OfflineAfter: 45 * time.Second,
	}, placementRequest("node-b"))
	if err != nil {
		t.Fatal(err)
	}
	if decision.NodeID != "node-b" || !decision.Sticky {
		t.Fatalf("stable placement decision = %+v", decision)
	}
}

func TestSelectFiltersLabelsCapabilitiesImagesResourcesAndOfflineNodes(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	offline := placementNode("offline", "zone-a", now.Add(-46*time.Second))
	wrongRegion := placementNode("wrong-region", "zone-a", now)
	wrongRegion.Labels["region"] = "us-west"
	missingImage := placementNode("missing-image", "zone-a", now)
	missingImage.Capabilities = []string{"docker", "oauth_api"}
	resourceFull := placementNode("resource-full", "zone-a", now)
	resourceFull.AllocatedMemoryBytes = resourceFull.Capacity.AllocatableMemoryBytes
	eligible := placementNode("eligible", "zone-a", now)

	decision, err := Select(Snapshot{
		Nodes: []store.Node{offline, wrongRegion, missingImage, resourceFull, eligible},
		Now:   now, OfflineAfter: 45 * time.Second,
	}, placementRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if decision.NodeID != "eligible" || decision.Sticky {
		t.Fatalf("filtered placement decision = %+v", decision)
	}
}

func TestSelectSpreadsAcrossFailureDomainBeforeLeastLoad(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	zoneA1 := placementNode("zone-a-1", "zone-a", now)
	zoneA2 := placementNode("zone-a-2", "zone-a", now)
	zoneB := placementNode("zone-b-1", "zone-b", now)
	zoneB.AllocatedSlots = 10
	zoneB.AllocatedCPUMillis = 1_600
	zoneB.AllocatedMemoryBytes = 3 << 30
	request := placementRequest("")
	request.SpreadBy = []string{"zone"}
	decision, err := Select(Snapshot{
		Nodes: []store.Node{zoneA1, zoneA2, zoneB}, Now: now, OfflineAfter: 45 * time.Second,
		Assignments: []Assignment{
			{SlotID: "existing-1", NodeID: "zone-a-1"},
			{SlotID: "existing-2", NodeID: "zone-a-2"},
			{SlotID: "existing-3", NodeID: "zone-a-1"},
		},
	}, request)
	if err != nil {
		t.Fatal(err)
	}
	if decision.NodeID != "zone-b-1" || decision.SpreadCounts["zone"] != 0 {
		t.Fatalf("spread placement decision = %+v", decision)
	}
}

func TestSelectUsesProjectedLeastLoadAndDeterministicNodeIDTieBreak(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	loaded := placementNode("node-c", "zone-a", now)
	loaded.ActiveTotal = 6
	emptyB := placementNode("node-b", "zone-a", now)
	emptyA := placementNode("node-a", "zone-a", now)
	decision, err := Select(Snapshot{
		Nodes: []store.Node{loaded, emptyB, emptyA}, Now: now, OfflineAfter: 45 * time.Second,
	}, placementRequest(""))
	if err != nil {
		t.Fatal(err)
	}
	if decision.NodeID != "node-a" {
		t.Fatalf("least-loaded deterministic decision = %+v", decision)
	}
}

func TestSelectReturnsNoEligibleNode(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	node := placementNode("node-a", "zone-a", now.Add(-time.Minute))
	_, err := Select(Snapshot{
		Nodes: []store.Node{node}, Now: now, OfflineAfter: 45 * time.Second,
	}, placementRequest(""))
	if !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("no eligible node error = %v", err)
	}
}

func TestRatioPPMHandlesLargeMemoryWithoutOverflow(t *testing.T) {
	capacity := uint64(1 << 60)
	if got := ratioPPM(capacity-1, capacity); got < 999_999 || got > 1_000_000 {
		t.Fatalf("large ratio = %d", got)
	}
}

func placementNode(id, zone string, lastSeen time.Time) store.Node {
	digest := "sha256:" + strings.Repeat("a", 64)
	return store.Node{
		ID: id, Status: "connected", LastSeenAt: &lastSeen,
		Labels:       map[string]string{"region": "ap-shanghai", "zone": zone},
		Capabilities: []string{"docker", "oauth_api", ImageCapability(digest)},
		Capacity: store.Capacity{
			MaxSlots: 20, MaxActiveCLI: 4, MaxActiveAPI: 12, MaxActiveTotal: 12,
			AllocatableCPUMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
		},
	}
}

func placementRequest(currentNodeID string) Request {
	return Request{
		SlotID: "slot-new", CurrentNodeID: currentNodeID,
		RequiredLabels:       map[string]string{"region": "ap-shanghai"},
		RequiredCapabilities: []string{"docker", "oauth_api"},
		ImageDigest:          "sha256:" + strings.Repeat("a", 64),
		CPURequestMillis:     500, MemoryRequestBytes: 128 << 20,
		ReserveAPI: 1, ReserveTotal: 1,
	}
}
