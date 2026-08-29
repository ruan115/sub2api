package lease

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestFailoverWaits90SecondsFencesOldEpochAndAllocatesHigherEpoch(t *testing.T) {
	base := time.Unix(2_000_000_000, 0).UTC()
	now := base
	repository := store.NewMemoryRepository()
	enrollLeaseNode(t, repository, "node-a", "session-a", "01", base)
	enrollLeaseNode(t, repository, "node-b", "session-b", "02", base)
	digest := "sha256:" + strings.Repeat("a", 64)
	if _, err := repository.PutDesiredSlot(context.Background(), store.Slot{
		ID: "slot-1", AccountID: "account-1", Provider: "docker", DesiredState: "ready", DesiredGeneration: 1,
		RequiredLabels: map[string]string{"region": "ap-shanghai"}, ImageDigest: digest,
		CPURequestMillis: 500, MemoryRequestBytes: 128 << 20, CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	firstAssignment, err := repository.ReserveAssignment(context.Background(), store.AssignmentReservation{
		ID: "assignment-1", SlotID: "slot-1", NodeID: "node-a", ExpectedNodeSessionID: "session-a",
		NodeSeenAfter: base.Add(-45 * time.Second), ReservedAt: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ObserveAssignment(context.Background(), store.AssignmentObservation{
		SlotID: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-old", ActualState: "running", Healthy: true, ObservedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	backend := NewMemoryBackend(func() time.Time { return now })
	coordinator, err := NewCoordinator(backend, repository, 45*time.Second, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	oldClaim := Claim{SlotID: "slot-1", NodeID: "node-a", ExecutionEpoch: firstAssignment.ExecutionEpoch, OwnerID: "agent-a"}
	if err := coordinator.Grant(context.Background(), "lease-1", oldClaim); err != nil {
		t.Fatal(err)
	}
	fencer, _ := NewFencer(backend)
	var oldConnectionClosed atomic.Bool
	if _, err := fencer.Admit(context.Background(), oldClaim, func() { oldConnectionClosed.Store(true) }); err != nil {
		t.Fatal(err)
	}
	failover, err := NewFailoverController(coordinator, repository, DefaultTiming(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	now = base.Add(89 * time.Second)
	if err := failover.FenceAndRelease(context.Background(), oldClaim, base); !errors.Is(err, ErrFailoverTooEarly) {
		t.Fatalf("early failover error = %v", err)
	}
	if _, err := repository.GetActiveAssignment(context.Background(), "slot-1"); err != nil {
		t.Fatalf("early failover released assignment: %v", err)
	}
	now = base.Add(90 * time.Second)
	if err := failover.FenceAndRelease(context.Background(), oldClaim, base); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetActiveAssignment(context.Background(), "slot-1"); !errors.Is(err, store.ErrAssignmentNotFound) {
		t.Fatalf("fenced old assignment error = %v", err)
	}
	if err := fencer.Revalidate(context.Background()); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("old egress revalidation error = %v", err)
	}
	if !oldConnectionClosed.Load() {
		t.Fatal("old epoch protected connection remained open")
	}
	if err := repository.RecordHeartbeat(context.Background(), store.Heartbeat{
		NodeID: "node-b", SessionID: "session-b", ReceivedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	secondAssignment, err := repository.ReserveAssignment(context.Background(), store.AssignmentReservation{
		ID: "assignment-2", SlotID: "slot-1", NodeID: "node-b", ExpectedNodeSessionID: "session-b",
		NodeSeenAfter: now.Add(-45 * time.Second), ReservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if secondAssignment.ExecutionEpoch != 2 {
		t.Fatalf("failover epoch = %d", secondAssignment.ExecutionEpoch)
	}
	newClaim := Claim{SlotID: "slot-1", NodeID: "node-b", ExecutionEpoch: 2, OwnerID: "agent-b"}
	if err := coordinator.Grant(context.Background(), "lease-2", newClaim); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Validate(context.Background(), oldClaim); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("old epoch became current after failover: %v", err)
	}
	if err := coordinator.Validate(context.Background(), newClaim); err != nil {
		t.Fatal(err)
	}
}

func enrollLeaseNode(t *testing.T, repository *store.MemoryRepository, nodeID, sessionID, serial string, now time.Time) {
	t.Helper()
	token := store.HashToken("enroll-" + nodeID)
	if err := repository.CreateEnrollment(context.Background(), store.Enrollment{
		ID: "enrollment-" + nodeID, TokenSHA256: token, ExpectedNodeID: nodeID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitEnrollment(context.Background(), token, store.Node{
		ID: nodeID, Status: "active", ProtocolMajor: 1, CreatedAt: now, UpdatedAt: now,
	}, store.Certificate{
		SerialNumber: serial, NodeID: nodeID, CertificateSHA256: sha256.Sum256([]byte("certificate-" + nodeID)),
		PublicKeySHA256: sha256.Sum256([]byte("key-" + nodeID)), Status: "active",
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.AcceptHello(context.Background(), store.Hello{
		NodeID: nodeID, SessionID: sessionID, ProtocolMajor: 1,
		Labels: map[string]string{"region": "ap-shanghai"}, Capabilities: []string{"docker"},
		Capacity: store.Capacity{
			MaxSlots: 20, MaxActiveCLI: 4, MaxActiveAPI: 12, MaxActiveTotal: 12,
			AllocatableCPUMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
		},
		ReceivedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}
