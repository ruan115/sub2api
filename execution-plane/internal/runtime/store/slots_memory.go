package store

import (
	"context"
	"errors"
	"time"
)

func (r *MemoryRepository) PutDesiredSlot(_ context.Context, candidate Slot) (Slot, error) {
	if _, err := validateDesiredSlot(candidate); err != nil {
		return Slot{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, exists := r.slots[candidate.ID]
	if exists {
		if candidate.AccountID != existing.AccountID || candidate.Provider != existing.Provider || candidate.DesiredGeneration < existing.DesiredGeneration {
			return Slot{}, ErrStaleGeneration
		}
		if candidate.DesiredGeneration == existing.DesiredGeneration {
			if !sameDesiredSlot(existing, candidate) {
				return Slot{}, ErrStaleGeneration
			}
			return cloneSlot(existing), nil
		}
		candidate.NextExecutionEpoch = existing.NextExecutionEpoch
		candidate.CreatedAt = existing.CreatedAt
	} else {
		candidate.NextExecutionEpoch = 1
	}
	candidate.RequiredLabels = cloneLabels(candidate.RequiredLabels)
	r.slots[candidate.ID] = candidate
	return cloneSlot(candidate), nil
}

func (r *MemoryRepository) GetSlot(_ context.Context, slotID string) (Slot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	slot, exists := r.slots[slotID]
	if !exists {
		return Slot{}, ErrSlotNotFound
	}
	return cloneSlot(slot), nil
}

func (r *MemoryRepository) ReserveAssignment(_ context.Context, reservation AssignmentReservation) (Assignment, error) {
	if reservation.ID == "" || reservation.SlotID == "" || reservation.NodeID == "" || reservation.ExpectedNodeSessionID == "" ||
		reservation.NodeSeenAfter.IsZero() || reservation.ReservedAt.IsZero() {
		return Assignment{}, errors.New("invalid slot assignment reservation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	slot, exists := r.slots[reservation.SlotID]
	if !exists {
		return Assignment{}, ErrSlotNotFound
	}
	if slot.DesiredState != "ready" {
		return Assignment{}, errors.New("slot is not desired ready")
	}
	for _, assignment := range r.assignments[slot.ID] {
		if assignment.ReleasedAt == nil {
			if assignment.ID == reservation.ID && assignment.NodeID == reservation.NodeID {
				return cloneAssignment(assignment), nil
			}
			return Assignment{}, ErrAssignmentConflict
		}
	}
	node, exists := r.nodes[reservation.NodeID]
	if !exists || node.Status != "connected" || node.ControlSessionID != reservation.ExpectedNodeSessionID ||
		node.LastSeenAt == nil || node.LastSeenAt.Before(reservation.NodeSeenAfter) {
		return Assignment{}, ErrNodeCapacity
	}
	usedSlots := max(node.ReservedSlots, node.AllocatedSlots)
	usedCPU := max(node.ReservedCPUMillis, node.AllocatedCPUMillis)
	usedMemory := max(node.ReservedMemoryBytes, node.AllocatedMemoryBytes)
	if usedSlots >= node.Capacity.MaxSlots || slot.CPURequestMillis > node.Capacity.AllocatableCPUMillis-usedCPU ||
		slot.MemoryRequestBytes > node.Capacity.AllocatableMemoryBytes-usedMemory {
		return Assignment{}, ErrNodeCapacity
	}
	node.ReservedSlots = usedSlots + 1
	node.ReservedCPUMillis = usedCPU + slot.CPURequestMillis
	node.ReservedMemoryBytes = usedMemory + slot.MemoryRequestBytes
	node.UpdatedAt = reservation.ReservedAt.UTC()
	r.nodes[node.ID] = node
	assignment := Assignment{
		ID: reservation.ID, SlotID: slot.ID, NodeID: node.ID, ExecutionEpoch: slot.NextExecutionEpoch,
		ImageDigest: slot.ImageDigest, CPURequestMillis: slot.CPURequestMillis, MemoryRequestBytes: slot.MemoryRequestBytes,
		ActualState: "missing", ActualGeneration: 1, AssignedAt: reservation.ReservedAt.UTC(),
	}
	slot.NextExecutionEpoch++
	slot.UpdatedAt = reservation.ReservedAt.UTC()
	r.slots[slot.ID] = slot
	r.assignments[slot.ID] = append(r.assignments[slot.ID], assignment)
	return cloneAssignment(assignment), nil
}

func (r *MemoryRepository) GetActiveAssignment(_ context.Context, slotID string) (Assignment, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	assignment, exists := activeMemoryAssignment(r.assignments[slotID])
	if !exists {
		return Assignment{}, ErrAssignmentNotFound
	}
	return cloneAssignment(assignment), nil
}

func (r *MemoryRepository) ObserveAssignment(_ context.Context, observation AssignmentObservation) (Assignment, error) {
	if err := validateObservation(observation); err != nil {
		return Assignment{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	assignments := r.assignments[observation.SlotID]
	for index := range assignments {
		assignment := &assignments[index]
		if assignment.ReleasedAt == nil && assignment.ExecutionEpoch == observation.ExecutionEpoch {
			if assignment.ActualState != observation.ActualState {
				assignment.ActualGeneration++
			}
			assignment.ProviderRef = observation.ProviderRef
			assignment.ActualState = observation.ActualState
			assignment.Healthy = observation.Healthy
			assignment.ReasonCode = observation.ReasonCode
			observed := observation.ObservedAt.UTC()
			assignment.LastObservedAt = &observed
			r.assignments[observation.SlotID] = assignments
			return cloneAssignment(*assignment), nil
		}
	}
	return Assignment{}, ErrAssignmentNotFound
}

func (r *MemoryRepository) ReleaseAssignment(_ context.Context, slotID string, executionEpoch uint64, releasedAt time.Time) error {
	if slotID == "" || executionEpoch == 0 || releasedAt.IsZero() {
		return errors.New("invalid slot assignment release")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	assignments := r.assignments[slotID]
	for index := range assignments {
		assignment := &assignments[index]
		if assignment.ReleasedAt == nil && assignment.ExecutionEpoch == executionEpoch {
			if assignment.ActualState != "destroyed" && assignment.ActualState != "missing" {
				return errors.New("slot assignment must be destroyed before release")
			}
			if assignment.ActualState != "destroyed" {
				assignment.ActualGeneration++
			}
			assignment.ActualState = "destroyed"
			assignment.Healthy = false
			released := releasedAt.UTC()
			assignment.ReleasedAt = &released
			r.assignments[slotID] = assignments
			node := r.nodes[assignment.NodeID]
			if node.ReservedSlots > 0 {
				node.ReservedSlots--
			}
			if node.ReservedCPUMillis >= assignment.CPURequestMillis {
				node.ReservedCPUMillis -= assignment.CPURequestMillis
			} else {
				node.ReservedCPUMillis = 0
			}
			if node.ReservedMemoryBytes >= assignment.MemoryRequestBytes {
				node.ReservedMemoryBytes -= assignment.MemoryRequestBytes
			} else {
				node.ReservedMemoryBytes = 0
			}
			node.UpdatedAt = released
			r.nodes[node.ID] = node
			return nil
		}
	}
	for _, assignment := range assignments {
		if assignment.ExecutionEpoch == executionEpoch && assignment.ReleasedAt != nil {
			return nil
		}
	}
	return ErrAssignmentNotFound
}

func (r *MemoryRepository) ForceReleaseAssignment(_ context.Context, slotID string, executionEpoch uint64, reasonCode string, releasedAt time.Time) error {
	if slotID == "" || executionEpoch == 0 || reasonCode == "" || len(reasonCode) > 64 || releasedAt.IsZero() {
		return errors.New("invalid forced slot assignment release")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	assignments := r.assignments[slotID]
	for index := range assignments {
		assignment := &assignments[index]
		if assignment.ExecutionEpoch != executionEpoch {
			continue
		}
		if assignment.ReleasedAt != nil {
			return nil
		}
		if assignment.ActualState != "fenced" {
			assignment.ActualGeneration++
		}
		assignment.ActualState = "fenced"
		assignment.Healthy = false
		assignment.ReasonCode = reasonCode
		released := releasedAt.UTC()
		assignment.ReleasedAt = &released
		r.assignments[slotID] = assignments
		node := r.nodes[assignment.NodeID]
		if node.ReservedSlots > 0 {
			node.ReservedSlots--
		}
		if node.ReservedCPUMillis >= assignment.CPURequestMillis {
			node.ReservedCPUMillis -= assignment.CPURequestMillis
		} else {
			node.ReservedCPUMillis = 0
		}
		if node.ReservedMemoryBytes >= assignment.MemoryRequestBytes {
			node.ReservedMemoryBytes -= assignment.MemoryRequestBytes
		} else {
			node.ReservedMemoryBytes = 0
		}
		node.UpdatedAt = released
		r.nodes[node.ID] = node
		return nil
	}
	return ErrAssignmentNotFound
}

func activeMemoryAssignment(assignments []Assignment) (Assignment, bool) {
	for _, assignment := range assignments {
		if assignment.ReleasedAt == nil {
			return assignment, true
		}
	}
	return Assignment{}, false
}

func sameDesiredSlot(left, right Slot) bool {
	if left.DesiredState != right.DesiredState || left.ImageDigest != right.ImageDigest ||
		left.CPURequestMillis != right.CPURequestMillis || left.MemoryRequestBytes != right.MemoryRequestBytes ||
		len(left.RequiredLabels) != len(right.RequiredLabels) {
		return false
	}
	for key, value := range left.RequiredLabels {
		if right.RequiredLabels[key] != value {
			return false
		}
	}
	return true
}

func cloneSlot(slot Slot) Slot {
	slot.RequiredLabels = cloneLabels(slot.RequiredLabels)
	return slot
}

func cloneAssignment(assignment Assignment) Assignment {
	if assignment.LastObservedAt != nil {
		value := *assignment.LastObservedAt
		assignment.LastObservedAt = &value
	}
	if assignment.ReleasedAt != nil {
		value := *assignment.ReleasedAt
		assignment.ReleasedAt = &value
	}
	return assignment
}

var _ SlotRepository = (*MemoryRepository)(nil)
