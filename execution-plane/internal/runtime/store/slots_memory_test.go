package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDesiredSlotGenerationIsIdempotentAndMonotonic(t *testing.T) {
	repository, now := connectedMemoryRepository(t)
	slot := desiredSlot("slot-1", "account-1", 1, now)
	created, err := repository.PutDesiredSlot(context.Background(), slot)
	if err != nil {
		t.Fatal(err)
	}
	if created.NextExecutionEpoch != 1 {
		t.Fatalf("next epoch = %d", created.NextExecutionEpoch)
	}
	if _, err := repository.PutDesiredSlot(context.Background(), slot); err != nil {
		t.Fatalf("idempotent desired slot: %v", err)
	}
	conflict := slot
	conflict.ImageDigest = "sha256:" + strings.Repeat("b", 64)
	if _, err := repository.PutDesiredSlot(context.Background(), conflict); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("same-generation conflict error = %v", err)
	}
	conflict.DesiredGeneration = 2
	conflict.UpdatedAt = now.Add(time.Second)
	updated, err := repository.PutDesiredSlot(context.Background(), conflict)
	if err != nil {
		t.Fatal(err)
	}
	if updated.DesiredGeneration != 2 || updated.ImageDigest != conflict.ImageDigest {
		t.Fatalf("updated desired slot = %+v", updated)
	}
}

func TestConcurrentAssignmentReservationFencesCapacityAndEpoch(t *testing.T) {
	repository, now := connectedMemoryRepository(t)
	const slots = 12
	for index := range slots {
		slotID := "slot-" + string(rune('a'+index))
		if _, err := repository.PutDesiredSlot(context.Background(), desiredSlot(slotID, "account-"+slotID, 1, now)); err != nil {
			t.Fatal(err)
		}
	}
	var successes atomic.Int32
	var wait sync.WaitGroup
	wait.Add(slots)
	for index := range slots {
		index := index
		go func() {
			defer wait.Done()
			slotID := "slot-" + string(rune('a'+index))
			_, err := repository.ReserveAssignment(context.Background(), AssignmentReservation{
				ID: "assignment-" + slotID, SlotID: slotID, NodeID: "srv74",
				ExpectedNodeSessionID: "session-1", NodeSeenAfter: now.Add(-45 * time.Second), ReservedAt: now,
			})
			if err == nil {
				successes.Add(1)
				return
			}
			if !errors.Is(err, ErrNodeCapacity) {
				t.Errorf("reserve %s: %v", slotID, err)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 6 {
		t.Fatalf("resource-fenced reservations = %d, want 6", successes.Load())
	}
	node, err := repository.GetNode(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	if node.ReservedSlots != 6 || node.ReservedCPUMillis != 3_000 || node.ReservedMemoryBytes != 6<<30 {
		t.Fatalf("node reservations exceed or underuse capacity: %+v", node)
	}
}

func TestAssignmentObservationAndReleaseAreEpochFenced(t *testing.T) {
	repository, now := connectedMemoryRepository(t)
	if _, err := repository.PutDesiredSlot(context.Background(), desiredSlot("slot-1", "account-1", 1, now)); err != nil {
		t.Fatal(err)
	}
	assignment, err := repository.ReserveAssignment(context.Background(), AssignmentReservation{
		ID: "assignment-1", SlotID: "slot-1", NodeID: "srv74",
		ExpectedNodeSessionID: "session-1", NodeSeenAfter: now.Add(-45 * time.Second), ReservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if assignment.ExecutionEpoch != 1 || assignment.DesiredGeneration != 1 || assignment.ActualGeneration != 1 {
		t.Fatalf("initial assignment = %+v", assignment)
	}
	if _, err := repository.ObserveAssignment(context.Background(), AssignmentObservation{
		SlotID: "slot-1", ExecutionEpoch: 2, ActualState: "running", ObservedAt: now,
	}); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("wrong epoch observation error = %v", err)
	}
	running, err := repository.ObserveAssignment(context.Background(), AssignmentObservation{
		SlotID: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ActualState: "running", Healthy: true, ObservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if running.ActualGeneration != 2 {
		t.Fatalf("running actual generation = %d", running.ActualGeneration)
	}
	runningAgain, err := repository.ObserveAssignment(context.Background(), AssignmentObservation{
		SlotID: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ActualState: "running", Healthy: true, ObservedAt: now.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if runningAgain.ActualGeneration != 2 {
		t.Fatalf("unchanged state advanced generation to %d", runningAgain.ActualGeneration)
	}
	if err := repository.ReleaseAssignment(context.Background(), "slot-1", 1, now); err == nil {
		t.Fatal("running assignment was released without destroy")
	}
	if _, err := repository.ObserveAssignment(context.Background(), AssignmentObservation{
		SlotID: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ActualState: "destroyed", ObservedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReleaseAssignment(context.Background(), "slot-1", 1, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GetActiveAssignment(context.Background(), "slot-1"); !errors.Is(err, ErrAssignmentNotFound) {
		t.Fatalf("released active assignment error = %v", err)
	}
	node, _ := repository.GetNode(context.Background(), "srv74")
	if node.ReservedSlots != 0 || node.ReservedCPUMillis != 0 || node.ReservedMemoryBytes != 0 {
		t.Fatalf("released node reservation = %+v", node)
	}
	second, err := repository.ReserveAssignment(context.Background(), AssignmentReservation{
		ID: "assignment-2", SlotID: "slot-1", NodeID: "srv74",
		ExpectedNodeSessionID: "session-1", NodeSeenAfter: now.Add(-45 * time.Second), ReservedAt: now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ExecutionEpoch != 2 || second.DesiredGeneration != 1 {
		t.Fatalf("replacement epoch = %d", second.ExecutionEpoch)
	}
}

func TestExecutionLeaseIsBoundToCurrentAssignmentEpochAndOwner(t *testing.T) {
	repository, now := connectedMemoryRepository(t)
	if _, err := repository.PutDesiredSlot(context.Background(), desiredSlot("slot-1", "account-1", 1, now)); err != nil {
		t.Fatal(err)
	}
	assignment, err := repository.ReserveAssignment(context.Background(), AssignmentReservation{
		ID: "assignment-1", SlotID: "slot-1", NodeID: "srv74",
		ExpectedNodeSessionID: "session-1", NodeSeenAfter: now.Add(-45 * time.Second), ReservedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := repository.GrantExecutionLease(context.Background(), ExecutionLease{
		ID: "lease-1", SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: assignment.ExecutionEpoch,
		OwnerID: "host-agent-1", ExpiresAt: now.Add(45 * time.Second), CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.ExecutionEpoch != 1 || lease.OwnerID != "host-agent-1" {
		t.Fatalf("granted execution lease = %+v", lease)
	}
	if _, err := repository.GrantExecutionLease(context.Background(), ExecutionLease{
		ID: "lease-2", SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 1,
		OwnerID: "other-owner", ExpiresAt: now.Add(45 * time.Second), CreatedAt: now, UpdatedAt: now,
	}); !errors.Is(err, ErrExecutionLeaseConflict) {
		t.Fatalf("conflicting owner grant error = %v", err)
	}
	if err := repository.RenewExecutionLease(context.Background(), "slot-1", 1, "host-agent-1", now.Add(60*time.Second), now.Add(15*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repository.RevokeExecutionLease(context.Background(), "slot-1", 1, "host-agent-1", now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repository.RevokeExecutionLease(context.Background(), "slot-1", 1, "host-agent-1", now.Add(21*time.Second)); err != nil {
		t.Fatalf("idempotent lease revoke: %v", err)
	}
	if err := repository.RenewExecutionLease(context.Background(), "slot-1", 1, "host-agent-1", now.Add(90*time.Second), now.Add(30*time.Second)); !errors.Is(err, ErrExecutionLeaseNotFound) {
		t.Fatalf("revoked lease renewal error = %v", err)
	}
}

func connectedMemoryRepository(t *testing.T) (*MemoryRepository, time.Time) {
	t.Helper()
	repository := NewMemoryRepository()
	now := time.Unix(2_000_000_000, 0).UTC()
	token := HashToken("slot-test-enrollment")
	if err := repository.CreateEnrollment(context.Background(), Enrollment{
		ID: "enrollment-1", TokenSHA256: token, ExpectedNodeID: "srv74", CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitEnrollment(context.Background(), token, testNode(now), testCertificate(now), now); err != nil {
		t.Fatal(err)
	}
	if err := repository.AcceptHello(context.Background(), Hello{
		NodeID: "srv74", SessionID: "session-1", ProtocolMajor: 1,
		Capacity: Capacity{
			MaxSlots: 20, MaxActiveCLI: 4, MaxActiveAPI: 12, MaxActiveTotal: 12,
			AllocatableCPUMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
		},
		ReceivedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return repository, now
}

func desiredSlot(slotID, accountID string, generation uint64, now time.Time) Slot {
	return Slot{
		ID: slotID, AccountID: accountID, Provider: "docker", DesiredState: "ready", DesiredGeneration: generation,
		RequiredLabels:   map[string]string{"region": "ap-shanghai"},
		ImageDigest:      "sha256:" + strings.Repeat("a", 64),
		CPURequestMillis: 500, MemoryRequestBytes: 1 << 30,
		CreatedAt: now, UpdatedAt: now,
	}
}
