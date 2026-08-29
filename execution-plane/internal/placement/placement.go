package placement

import (
	"errors"
	"math/bits"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

var (
	ErrNoEligibleNode  = errors.New("no eligible execution node")
	imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Assignment struct {
	SlotID string
	NodeID string
}

type Snapshot struct {
	Nodes        []store.Node
	Assignments  []Assignment
	Now          time.Time
	OfflineAfter time.Duration
}

type Request struct {
	SlotID               string
	CurrentNodeID        string
	RequiredLabels       map[string]string
	RequiredCapabilities []string
	ImageDigest          string
	CPURequestMillis     uint64
	MemoryRequestBytes   uint64
	ReserveCLI           uint32
	ReserveAPI           uint32
	ReserveTotal         uint32
	SpreadBy             []string
}

type Decision struct {
	NodeID       string
	Sticky       bool
	SpreadCounts map[string]int
	MaxLoadPPM   uint64
	MeanLoadPPM  uint64
}

func Select(snapshot Snapshot, request Request) (Decision, error) {
	if err := validate(snapshot, request); err != nil {
		return Decision{}, err
	}
	nodes := make(map[string]store.Node, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if node.ID != "" {
			nodes[node.ID] = node
		}
	}
	requiredCapabilities := append([]string(nil), request.RequiredCapabilities...)
	if request.ImageDigest != "" {
		requiredCapabilities = append(requiredCapabilities, ImageCapability(request.ImageDigest))
	}

	if current, exists := nodes[request.CurrentNodeID]; exists && eligible(current, snapshot, request, requiredCapabilities, true) {
		score := scoreNode(current, snapshot, request, nodes, true)
		score.Sticky = true
		return score, nil
	}

	candidates := make([]Decision, 0, len(nodes))
	for _, node := range nodes {
		if eligible(node, snapshot, request, requiredCapabilities, false) {
			candidates = append(candidates, scoreNode(node, snapshot, request, nodes, false))
		}
	}
	if len(candidates) == 0 {
		return Decision{}, ErrNoEligibleNode
	}
	sort.Slice(candidates, func(left, right int) bool {
		return less(candidates[left], candidates[right], request.SpreadBy)
	})
	return candidates[0], nil
}

func ImageCapability(digest string) string {
	return "image." + strings.Replace(digest, ":", ".", 1)
}

func validate(snapshot Snapshot, request Request) error {
	if strings.TrimSpace(request.SlotID) == "" || snapshot.Now.IsZero() || snapshot.OfflineAfter <= 0 {
		return errors.New("placement slot, clock and offline threshold are required")
	}
	if request.CPURequestMillis == 0 || request.MemoryRequestBytes == 0 {
		return errors.New("placement CPU and memory requests must be positive")
	}
	if request.ReserveCLI > request.ReserveTotal || request.ReserveAPI > request.ReserveTotal {
		return errors.New("placement mode reservations are inconsistent")
	}
	if request.ImageDigest != "" && !imageDigestPattern.MatchString(request.ImageDigest) {
		return errors.New("placement image digest is invalid")
	}
	for key := range request.RequiredLabels {
		if strings.TrimSpace(key) == "" {
			return errors.New("placement required label is invalid")
		}
	}
	seenCapabilities := make(map[string]struct{}, len(request.RequiredCapabilities))
	for _, capability := range request.RequiredCapabilities {
		if strings.TrimSpace(capability) == "" {
			return errors.New("placement required capability is invalid")
		}
		if _, exists := seenCapabilities[capability]; exists {
			return errors.New("placement required capabilities contain duplicates")
		}
		seenCapabilities[capability] = struct{}{}
	}
	seenSpread := make(map[string]struct{}, len(request.SpreadBy))
	for _, key := range request.SpreadBy {
		if key == "" {
			return errors.New("placement spread label is invalid")
		}
		if _, exists := seenSpread[key]; exists {
			return errors.New("placement spread labels contain duplicates")
		}
		seenSpread[key] = struct{}{}
	}
	seenNodes := make(map[string]struct{}, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if node.ID == "" {
			continue
		}
		if _, exists := seenNodes[node.ID]; exists {
			return errors.New("placement snapshot contains duplicate nodes")
		}
		seenNodes[node.ID] = struct{}{}
	}
	seenSlots := make(map[string]struct{}, len(snapshot.Assignments))
	for _, assignment := range snapshot.Assignments {
		if assignment.SlotID == "" || assignment.NodeID == "" {
			return errors.New("placement snapshot contains an invalid assignment")
		}
		if _, exists := seenSlots[assignment.SlotID]; exists {
			return errors.New("placement snapshot contains duplicate active slot assignments")
		}
		seenSlots[assignment.SlotID] = struct{}{}
	}
	return nil
}

func eligible(node store.Node, snapshot Snapshot, request Request, requiredCapabilities []string, existing bool) bool {
	if node.Status != "connected" || node.LastSeenAt == nil || snapshot.Now.Sub(*node.LastSeenAt) > snapshot.OfflineAfter {
		return false
	}
	if node.Capacity.Validate() != nil || node.AllocatedSlots > node.Capacity.MaxSlots ||
		node.ReservedSlots > node.Capacity.MaxSlots || node.ReservedCPUMillis > node.Capacity.AllocatableCPUMillis ||
		node.ReservedMemoryBytes > node.Capacity.AllocatableMemoryBytes ||
		node.AllocatedCPUMillis > node.Capacity.AllocatableCPUMillis || node.AllocatedMemoryBytes > node.Capacity.AllocatableMemoryBytes ||
		node.ActiveCLI > node.Capacity.MaxActiveCLI || node.ActiveAPI > node.Capacity.MaxActiveAPI || node.ActiveTotal > node.Capacity.MaxActiveTotal {
		return false
	}
	for key, value := range request.RequiredLabels {
		if node.Labels[key] != value {
			return false
		}
	}
	capabilities := make(map[string]struct{}, len(node.Capabilities))
	for _, capability := range node.Capabilities {
		capabilities[capability] = struct{}{}
	}
	for _, required := range requiredCapabilities {
		if _, exists := capabilities[required]; !exists {
			return false
		}
	}
	if existing {
		return true
	}
	usedSlots := max(node.AllocatedSlots, node.ReservedSlots)
	usedCPU := max(node.AllocatedCPUMillis, node.ReservedCPUMillis)
	usedMemory := max(node.AllocatedMemoryBytes, node.ReservedMemoryBytes)
	return request.CPURequestMillis <= node.Capacity.AllocatableCPUMillis-usedCPU &&
		request.MemoryRequestBytes <= node.Capacity.AllocatableMemoryBytes-usedMemory &&
		request.ReserveCLI <= node.Capacity.MaxActiveCLI-node.ActiveCLI &&
		request.ReserveAPI <= node.Capacity.MaxActiveAPI-node.ActiveAPI &&
		request.ReserveTotal <= node.Capacity.MaxActiveTotal-node.ActiveTotal &&
		usedSlots < node.Capacity.MaxSlots
}

func scoreNode(node store.Node, snapshot Snapshot, request Request, nodes map[string]store.Node, existing bool) Decision {
	spread := make(map[string]int, len(request.SpreadBy))
	for _, key := range request.SpreadBy {
		value := node.Labels[key]
		for _, assignment := range snapshot.Assignments {
			if assignment.SlotID == request.SlotID {
				continue
			}
			assignedNode, exists := nodes[assignment.NodeID]
			if exists && assignedNode.Labels[key] == value {
				spread[key]++
			}
		}
	}
	allocatedSlots := uint64(max(node.AllocatedSlots, node.ReservedSlots))
	allocatedCPU := max(node.AllocatedCPUMillis, node.ReservedCPUMillis)
	allocatedMemory := max(node.AllocatedMemoryBytes, node.ReservedMemoryBytes)
	activeCLI := uint64(node.ActiveCLI)
	activeAPI := uint64(node.ActiveAPI)
	activeTotal := uint64(node.ActiveTotal)
	if !existing {
		allocatedSlots++
		allocatedCPU += request.CPURequestMillis
		allocatedMemory += request.MemoryRequestBytes
		activeCLI += uint64(request.ReserveCLI)
		activeAPI += uint64(request.ReserveAPI)
		activeTotal += uint64(request.ReserveTotal)
	}
	loads := []uint64{
		ratioPPM(allocatedSlots, uint64(node.Capacity.MaxSlots)),
		ratioPPM(allocatedCPU, node.Capacity.AllocatableCPUMillis),
		ratioPPM(allocatedMemory, node.Capacity.AllocatableMemoryBytes),
		ratioPPM(activeCLI, uint64(node.Capacity.MaxActiveCLI)),
		ratioPPM(activeAPI, uint64(node.Capacity.MaxActiveAPI)),
		ratioPPM(activeTotal, uint64(node.Capacity.MaxActiveTotal)),
	}
	var maximum, total uint64
	for _, load := range loads {
		if load > maximum {
			maximum = load
		}
		total += load
	}
	return Decision{
		NodeID: node.ID, SpreadCounts: spread,
		MaxLoadPPM: maximum, MeanLoadPPM: total / uint64(len(loads)),
	}
}

func less(left, right Decision, spreadBy []string) bool {
	for _, key := range spreadBy {
		if left.SpreadCounts[key] != right.SpreadCounts[key] {
			return left.SpreadCounts[key] < right.SpreadCounts[key]
		}
	}
	if left.MaxLoadPPM != right.MaxLoadPPM {
		return left.MaxLoadPPM < right.MaxLoadPPM
	}
	if left.MeanLoadPPM != right.MeanLoadPPM {
		return left.MeanLoadPPM < right.MeanLoadPPM
	}
	return left.NodeID < right.NodeID
}

func ratioPPM(used, capacity uint64) uint64 {
	if capacity == 0 {
		return 1_000_000
	}
	high, low := bits.Mul64(used, 1_000_000)
	quotient, _ := bits.Div64(high, low, capacity)
	return quotient
}
