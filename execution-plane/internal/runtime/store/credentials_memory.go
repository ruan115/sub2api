package store

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

func (r *MemoryRepository) NextCredentialVersionNumber(_ context.Context, accountID string) (uint64, error) {
	if err := validateCredentialAccountID(accountID); err != nil {
		return 0, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var current uint64
	for _, versionID := range r.credentialVersionIDs[accountID] {
		if version := r.credentialVersions[versionID]; version.VersionNumber > current {
			current = version.VersionNumber
		}
	}
	if current == math.MaxUint64 {
		return 0, credential.ErrCredentialVersionConflict
	}
	return current + 1, nil
}

func (r *MemoryRepository) CommitCredentialVersion(_ context.Context, version credential.VersionRecord) error {
	if err := version.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.credentialVersions[version.ID]; exists {
		return credential.ErrCredentialVersionConflict
	}
	var current uint64
	for _, versionID := range r.credentialVersionIDs[version.AccountID] {
		if existing := r.credentialVersions[versionID]; existing.VersionNumber > current {
			current = existing.VersionNumber
		}
	}
	if current == math.MaxUint64 || version.VersionNumber != current+1 {
		return credential.ErrCredentialVersionConflict
	}
	stored := cloneCredentialVersion(version)
	r.credentialVersions[version.ID] = stored
	r.credentialVersionIDs[version.AccountID] = append(r.credentialVersionIDs[version.AccountID], version.ID)
	vault, exists := r.credentialVaults[version.AccountID]
	if !exists {
		vault.CreatedAt = version.CreatedAt.UTC()
	}
	vault.ActiveVersionID = version.ID
	vault.AuthType = version.AuthType
	vault.UpdatedAt = version.CreatedAt.UTC()
	r.credentialVaults[version.AccountID] = vault
	for digest, lease := range r.credentialLeases {
		if lease.AccountID == version.AccountID && lease.VersionID != version.ID && lease.ConsumedAt == nil && lease.RevokedAt == nil {
			revoked := version.CreatedAt.UTC()
			lease.RevokedAt = &revoked
			r.credentialLeases[digest] = lease
		}
	}
	return nil
}

func (r *MemoryRepository) GetActiveCredentialVersion(_ context.Context, accountID string) (credential.VersionRecord, error) {
	if err := validateCredentialAccountID(accountID); err != nil {
		return credential.VersionRecord{}, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	vault, exists := r.credentialVaults[accountID]
	if !exists || vault.ActiveVersionID == "" {
		return credential.VersionRecord{}, credential.ErrCredentialVaultNotFound
	}
	version, exists := r.credentialVersions[vault.ActiveVersionID]
	if !exists {
		return credential.VersionRecord{}, credential.ErrCredentialVaultNotFound
	}
	return cloneCredentialVersion(version), nil
}

func (r *MemoryRepository) IssueCredentialLease(_ context.Context, candidate credential.LeaseRecord) (credential.LeaseRecord, error) {
	if err := candidate.ValidateForIssue(); err != nil {
		return credential.LeaseRecord{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.credentialLeases[candidate.TokenSHA256]; exists {
		return credential.LeaseRecord{}, credential.ErrCredentialLeaseRejected
	}
	slot, slotExists := r.slots[candidate.SlotID]
	assignment, assignmentExists := activeMemoryAssignment(r.assignments[candidate.SlotID])
	executionLease, executionLeaseExists := r.executionLeases[executionLeaseKey(candidate.SlotID, candidate.ExecutionEpoch)]
	vault, vaultExists := r.credentialVaults[candidate.AccountID]
	if !slotExists || slot.AccountID != candidate.AccountID || !assignmentExists || assignment.ExecutionEpoch != candidate.ExecutionEpoch ||
		!executionLeaseExists || executionLease.NodeID != assignment.NodeID || executionLease.RevokedAt != nil || !executionLease.ExpiresAt.After(candidate.CreatedAt) ||
		!vaultExists || vault.ActiveVersionID == "" {
		return credential.LeaseRecord{}, credential.ErrCredentialLeaseRejected
	}
	candidate.VersionID = vault.ActiveVersionID
	candidate.ExpiresAt = candidate.ExpiresAt.UTC()
	candidate.CreatedAt = candidate.CreatedAt.UTC()
	r.credentialLeases[candidate.TokenSHA256] = candidate
	return cloneCredentialLease(candidate), nil
}

func (r *MemoryRepository) ConsumeCredentialLease(_ context.Context, claim credential.LeaseClaim) (credential.VersionRecord, error) {
	if err := claim.Validate(); err != nil {
		return credential.VersionRecord{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	lease, exists := r.credentialLeases[claim.TokenSHA256]
	if !exists {
		r.rejectMemoryCredentialLease(claim, credential.LeaseRecord{}, "unknown_token")
		return credential.VersionRecord{}, credential.ErrCredentialLeaseRejected
	}
	if reason := rejectedCredentialLeaseReason(lease, claim); reason != "" {
		r.rejectMemoryCredentialLease(claim, lease, reason)
		return credential.VersionRecord{}, credential.ErrCredentialLeaseRejected
	}
	slot, slotExists := r.slots[lease.SlotID]
	assignment, assignmentExists := activeMemoryAssignment(r.assignments[lease.SlotID])
	executionLease, executionLeaseExists := r.executionLeases[executionLeaseKey(lease.SlotID, lease.ExecutionEpoch)]
	vault, vaultExists := r.credentialVaults[lease.AccountID]
	if !slotExists || slot.AccountID != lease.AccountID || !assignmentExists || assignment.ExecutionEpoch != lease.ExecutionEpoch ||
		!executionLeaseExists || executionLease.NodeID != assignment.NodeID || executionLease.RevokedAt != nil || !executionLease.ExpiresAt.After(claim.ConsumedAt) ||
		!vaultExists || vault.ActiveVersionID != lease.VersionID {
		r.rejectMemoryCredentialLease(claim, lease, "epoch_or_version_inactive")
		return credential.VersionRecord{}, credential.ErrCredentialLeaseRejected
	}
	consumed := claim.ConsumedAt.UTC()
	lease.ConsumedAt = &consumed
	r.credentialLeases[claim.TokenSHA256] = lease
	version, exists := r.credentialVersions[lease.VersionID]
	if !exists {
		return credential.VersionRecord{}, errors.New("leased credential version is missing")
	}
	return cloneCredentialVersion(version), nil
}

func (r *MemoryRepository) ListCredentialSecurityEvents(_ context.Context, accountID string, limit int) ([]credential.SecurityEvent, error) {
	if err := validateCredentialAccountID(accountID); err != nil || limit < 1 || limit > 1000 {
		return nil, errors.New("invalid credential security event query")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	events := make([]credential.SecurityEvent, 0)
	for _, event := range r.credentialSecurityEvents {
		if event.AccountID == accountID {
			events = append(events, event)
		}
	}
	sort.Slice(events, func(left, right int) bool {
		if events[left].CreatedAt.Equal(events[right].CreatedAt) {
			return events[left].ID > events[right].ID
		}
		return events[left].CreatedAt.After(events[right].CreatedAt)
	})
	if len(events) > limit {
		events = events[:limit]
	}
	return append([]credential.SecurityEvent(nil), events...), nil
}

func (r *MemoryRepository) rejectMemoryCredentialLease(claim credential.LeaseClaim, lease credential.LeaseRecord, reason string) {
	event := credential.SecurityEvent{
		ID: claim.SecurityEventID, EventType: "credential_lease_rejected", ReasonCode: reason,
		AccountID: claim.AccountID, SlotID: claim.SlotID, ExecutionEpoch: claim.ExecutionEpoch,
		CreatedAt: claim.ConsumedAt.UTC(),
	}
	if lease.ID != "" {
		event.AccountID = lease.AccountID
		event.SlotID = lease.SlotID
		event.ExecutionEpoch = lease.ExecutionEpoch
		event.LeaseID = lease.ID
	}
	r.credentialSecurityEvents = append(r.credentialSecurityEvents, event)
}

func cloneCredentialVersion(version credential.VersionRecord) credential.VersionRecord {
	version.Envelope.Ciphertext = append([]byte(nil), version.Envelope.Ciphertext...)
	version.Envelope.EncryptedDEK = append([]byte(nil), version.Envelope.EncryptedDEK...)
	version.Envelope.Nonce = append([]byte(nil), version.Envelope.Nonce...)
	version.Envelope.AADJSON = append([]byte(nil), version.Envelope.AADJSON...)
	return version
}

var _ CredentialVaultRepository = (*MemoryRepository)(nil)
