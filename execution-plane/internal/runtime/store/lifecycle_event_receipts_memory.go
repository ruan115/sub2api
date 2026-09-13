package store

import "context"

func (r *MemoryRepository) LookupLifecycleEventApply(
	_ context.Context,
	anchor LifecycleEventAnchor,
) (LifecycleEventApplyReceipt, bool, error) {
	if err := validateLifecycleEventAnchor(anchor); err != nil {
		return LifecycleEventApplyReceipt{}, false, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if receipt, exists := r.lifecycleEventReceipts[anchor.EventID]; exists {
		if !sameLifecycleEventAnchor(receipt.Anchor, anchor) {
			return LifecycleEventApplyReceipt{}, false, ErrLifecycleEventConflict
		}
		return cloneLifecycleEventApplyReceipt(receipt), true, nil
	}
	if _, exists := r.lifecycleEventSequences[anchor.SourceSequence]; exists {
		return LifecycleEventApplyReceipt{}, false, ErrLifecycleEventConflict
	}
	return LifecycleEventApplyReceipt{}, false, nil
}

func (r *MemoryRepository) ApplyLifecycleEvent(
	_ context.Context,
	apply LifecycleEventApply,
) (LifecycleEventApplyReceipt, bool, error) {
	if _, err := validateLifecycleEventApply(apply); err != nil {
		return LifecycleEventApplyReceipt{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if receipt, exists := r.lifecycleEventReceipts[apply.Anchor.EventID]; exists {
		if !sameLifecycleEventAnchor(receipt.Anchor, apply.Anchor) {
			return LifecycleEventApplyReceipt{}, false, ErrLifecycleEventConflict
		}
		return cloneLifecycleEventApplyReceipt(receipt), false, nil
	}
	if _, exists := r.lifecycleEventSequences[apply.Anchor.SourceSequence]; exists {
		return LifecycleEventApplyReceipt{}, false, ErrLifecycleEventConflict
	}

	candidate := cloneSlot(apply.Slot)
	existing, exists := r.slots[candidate.ID]
	if exists {
		if candidate.AccountID != existing.AccountID || candidate.Provider != existing.Provider ||
			candidate.DesiredGeneration < existing.DesiredGeneration {
			return LifecycleEventApplyReceipt{}, false, ErrStaleGeneration
		}
		if candidate.DesiredGeneration == existing.DesiredGeneration {
			if !sameDesiredSlot(existing, candidate) {
				return LifecycleEventApplyReceipt{}, false, ErrStaleGeneration
			}
			candidate = cloneSlot(existing)
		} else {
			candidate.NextExecutionEpoch = existing.NextExecutionEpoch
			candidate.CreatedAt = existing.CreatedAt
			r.slots[candidate.ID] = cloneSlot(candidate)
		}
	} else {
		candidate.NextExecutionEpoch = 1
		r.slots[candidate.ID] = cloneSlot(candidate)
	}
	// A same-generation exact slot may predate this receipt. The receipt still
	// freezes the policy supplied by this first event apply, not mutable state.
	policy := cloneSlot(apply.Slot)
	policy.CreatedAt = normalizeLifecycleTime(policy.CreatedAt)
	policy.UpdatedAt = normalizeLifecycleTime(policy.UpdatedAt)
	receipt := LifecycleEventApplyReceipt{
		Anchor:    apply.Anchor,
		Slot:      policy,
		AppliedAt: normalizeLifecycleTime(apply.AppliedAt),
	}
	r.lifecycleEventReceipts[apply.Anchor.EventID] = cloneLifecycleEventApplyReceipt(receipt)
	r.lifecycleEventSequences[apply.Anchor.SourceSequence] = apply.Anchor.EventID
	return cloneLifecycleEventApplyReceipt(receipt), true, nil
}

func cloneLifecycleEventApplyReceipt(receipt LifecycleEventApplyReceipt) LifecycleEventApplyReceipt {
	receipt.Slot = cloneSlot(receipt.Slot)
	return receipt
}

var _ LifecycleEventApplyRepository = (*MemoryRepository)(nil)
