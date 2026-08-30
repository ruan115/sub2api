package onboarding

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

const (
	ProvisioningPendingKey           = "pending_key"
	ProvisioningKeyDispatched        = "key_dispatched"
	ProvisioningKeyReady             = "key_ready"
	ProvisioningActivationDispatched = "activation_dispatched"
	ProvisioningActivationSucceeded  = "activation_succeeded"
	ProvisioningFailed               = "failed"
	ProvisioningCompleted            = "completed"
)

var (
	ErrProvisioningRejected = errors.New("onboarding provisioning workflow rejected")
	provisioningImageDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	provisioningErrorCode   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

// Provisioning binds one durable intent to exactly two NodeControl commands.
// KeyPublicKey is public process identity, never credential material.
type Provisioning struct {
	ID                  string
	IdempotencyKey      string
	IntentID            string
	Owner               string
	AccountID           string
	DesiredGeneration   uint64
	NodeID              string
	SlotID              string
	ExecutionEpoch      uint64
	ImageDigest         string
	CredentialLeaseID   string
	ProxyLeaseID        string
	KeyCommandID        string
	ActivationCommandID string
	CommandDeadline     time.Time
	Status              string
	KeyID               string
	KeyPublicKey        []byte
	ErrorCode           string
	LastCommandID       string
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func (p Provisioning) Clone() Provisioning {
	p.KeyPublicKey = append([]byte(nil), p.KeyPublicKey...)
	return p
}

func (p *Provisioning) Destroy() {
	if p == nil {
		return
	}
	for index := range p.KeyPublicKey {
		p.KeyPublicKey[index] = 0
	}
	p.KeyPublicKey = nil
}

func (p Provisioning) Validate() error {
	for _, value := range []string{
		p.ID, p.IdempotencyKey, p.IntentID, p.Owner, p.AccountID, p.NodeID, p.SlotID,
		p.CredentialLeaseID, p.ProxyLeaseID, p.KeyCommandID, p.ActivationCommandID,
	} {
		if credential.ValidateTransportID(value) != nil {
			return ErrProvisioningRejected
		}
	}
	if p.KeyCommandID == p.ActivationCommandID || p.DesiredGeneration == 0 || p.ExecutionEpoch == 0 ||
		!provisioningImageDigest.MatchString(p.ImageDigest) || p.CommandDeadline.IsZero() ||
		p.CreatedAt.IsZero() || !p.CommandDeadline.After(p.CreatedAt) || p.UpdatedAt.Before(p.CreatedAt) {
		return ErrProvisioningRejected
	}
	keyValid := credential.ValidateRecipientKey(p.KeyID, p.KeyPublicKey) == nil
	switch p.Status {
	case ProvisioningPendingKey:
		if p.KeyID != "" || len(p.KeyPublicKey) != 0 || p.ErrorCode != "" || p.LastCommandID != "" {
			return ErrProvisioningRejected
		}
	case ProvisioningKeyDispatched:
		if p.KeyID != "" || len(p.KeyPublicKey) != 0 || p.ErrorCode != "" || p.LastCommandID != p.KeyCommandID {
			return ErrProvisioningRejected
		}
	case ProvisioningKeyReady:
		if !keyValid || p.ErrorCode != "" || p.LastCommandID != p.KeyCommandID {
			return ErrProvisioningRejected
		}
	case ProvisioningActivationDispatched, ProvisioningActivationSucceeded, ProvisioningCompleted:
		if !keyValid || p.ErrorCode != "" || p.LastCommandID != p.ActivationCommandID {
			return ErrProvisioningRejected
		}
	case ProvisioningFailed:
		if !provisioningErrorCode.MatchString(p.ErrorCode) || p.LastCommandID == "" ||
			(p.LastCommandID != p.KeyCommandID && p.LastCommandID != p.ActivationCommandID) ||
			((p.KeyID != "" || len(p.KeyPublicKey) != 0) && !keyValid) {
			return ErrProvisioningRejected
		}
	default:
		return ErrProvisioningRejected
	}
	return nil
}

type ProvisioningCommandObservation struct {
	CommandID      string
	NodeID         string
	SlotID         string
	ExecutionEpoch uint64
	ImageDigest    string
	Healthy        bool
	Succeeded      bool
	ErrorCode      string
	KeyID          string
	KeyPublicKey   []byte
	ReceivedAt     time.Time
}

func (o ProvisioningCommandObservation) Clone() ProvisioningCommandObservation {
	o.KeyPublicKey = append([]byte(nil), o.KeyPublicKey...)
	return o
}

func (o *ProvisioningCommandObservation) Destroy() {
	if o == nil {
		return
	}
	for index := range o.KeyPublicKey {
		o.KeyPublicKey[index] = 0
	}
	o.KeyPublicKey = nil
}

func (o ProvisioningCommandObservation) Validate() error {
	for _, value := range []string{o.CommandID, o.NodeID, o.SlotID} {
		if credential.ValidateTransportID(value) != nil {
			return ErrProvisioningRejected
		}
	}
	if o.ExecutionEpoch == 0 || !provisioningImageDigest.MatchString(o.ImageDigest) || o.ReceivedAt.IsZero() {
		return ErrProvisioningRejected
	}
	if o.Succeeded {
		if o.ErrorCode != "" || !o.Healthy {
			return ErrProvisioningRejected
		}
	} else if !provisioningErrorCode.MatchString(o.ErrorCode) || o.KeyID != "" || len(o.KeyPublicKey) != 0 {
		return ErrProvisioningRejected
	}
	return nil
}

type ProvisioningRepository interface {
	ObserveProvisioningCommand(ctx context.Context, observation ProvisioningCommandObservation) (Provisioning, error)
	MarkProvisioningKeyDispatched(ctx context.Context, workflowID string, dispatchedAt time.Time) error
	MarkProvisioningActivationDispatched(ctx context.Context, workflowID string, dispatchedAt time.Time) error
	FailProvisioning(ctx context.Context, workflowID, errorCode string, failedAt time.Time) error
	CompleteProvisioning(ctx context.Context, workflowID string, completedAt time.Time) error
	DeferProvisioningRetry(ctx context.Context, workflowID string, retryAt time.Time) error
	GetProvisioning(ctx context.Context, workflowID string) (Provisioning, error)
}

// ProvisioningRepository deliberately has no workflow-create method. Durable
// workflows enter this state machine only through HealthySlotStartRepository,
// which atomically validates the intent, assignment, execution lease and
// trusted proxy reservation while creating the proxy lease. The memory
// repository retains CreateProvisioning solely as a deterministic test-fixture
// helper; the production MySQL repository does not expose that bypass.

// ActiveProvisioningRepository adds the bounded work scan used by the
// orchestrator polling runner. Terminal workflows never re-enter this list.
type ActiveProvisioningRepository interface {
	ProvisioningRepository
	ListActiveProvisioningIDs(ctx context.Context, limit int) ([]string, error)
}

func ApplyProvisioningObservation(current Provisioning, observation ProvisioningCommandObservation) (Provisioning, error) {
	if current.Validate() != nil || observation.Validate() != nil || observation.NodeID != current.NodeID ||
		observation.SlotID != current.SlotID || observation.ExecutionEpoch != current.ExecutionEpoch ||
		observation.ImageDigest != current.ImageDigest {
		return Provisioning{}, ErrProvisioningRejected
	}
	updated := current.Clone()
	isKey := observation.CommandID == current.KeyCommandID
	isActivation := observation.CommandID == current.ActivationCommandID
	if !isKey && !isActivation {
		updated.Destroy()
		return Provisioning{}, ErrProvisioningRejected
	}
	if isKey {
		if observation.Succeeded {
			if credential.ValidateRecipientKey(observation.KeyID, observation.KeyPublicKey) != nil {
				updated.Destroy()
				return Provisioning{}, ErrProvisioningRejected
			}
			switch current.Status {
			case ProvisioningPendingKey, ProvisioningKeyDispatched:
				updated.Status = ProvisioningKeyReady
				updated.KeyID = observation.KeyID
				updated.KeyPublicKey = append(updated.KeyPublicKey[:0], observation.KeyPublicKey...)
				updated.LastCommandID = observation.CommandID
			case ProvisioningKeyReady, ProvisioningActivationDispatched, ProvisioningActivationSucceeded, ProvisioningCompleted:
				if current.KeyID != observation.KeyID || !bytes.Equal(current.KeyPublicKey, observation.KeyPublicKey) {
					updated.Destroy()
					return Provisioning{}, ErrProvisioningRejected
				}
				return updated, nil
			default:
				updated.Destroy()
				return Provisioning{}, ErrProvisioningRejected
			}
		} else {
			if current.Status == ProvisioningFailed && current.LastCommandID == observation.CommandID && current.ErrorCode == observation.ErrorCode {
				return updated, nil
			}
			if current.Status != ProvisioningPendingKey && current.Status != ProvisioningKeyDispatched {
				updated.Destroy()
				return Provisioning{}, ErrProvisioningRejected
			}
			updated.Status = ProvisioningFailed
			updated.ErrorCode = observation.ErrorCode
			updated.LastCommandID = observation.CommandID
		}
	} else {
		if observation.KeyID != "" || len(observation.KeyPublicKey) != 0 ||
			(current.Status != ProvisioningKeyReady && current.Status != ProvisioningActivationDispatched &&
				current.Status != ProvisioningActivationSucceeded && current.Status != ProvisioningCompleted && current.Status != ProvisioningFailed) {
			updated.Destroy()
			return Provisioning{}, ErrProvisioningRejected
		}
		if observation.Succeeded {
			if current.Status == ProvisioningFailed {
				updated.Destroy()
				return Provisioning{}, ErrProvisioningRejected
			}
			if current.Status == ProvisioningActivationSucceeded || current.Status == ProvisioningCompleted {
				return updated, nil
			}
			updated.Status = ProvisioningActivationSucceeded
			updated.ErrorCode = ""
			updated.LastCommandID = observation.CommandID
		} else {
			if current.Status == ProvisioningFailed {
				if current.LastCommandID == observation.CommandID && current.ErrorCode == observation.ErrorCode {
					return updated, nil
				}
				updated.Destroy()
				return Provisioning{}, ErrProvisioningRejected
			}
			if current.Status == ProvisioningActivationSucceeded || current.Status == ProvisioningCompleted {
				updated.Destroy()
				return Provisioning{}, ErrProvisioningRejected
			}
			updated.Status = ProvisioningFailed
			updated.ErrorCode = observation.ErrorCode
			updated.LastCommandID = observation.CommandID
		}
	}
	updated.UpdatedAt = observation.ReceivedAt.UTC()
	if updated.Validate() != nil {
		updated.Destroy()
		return Provisioning{}, ErrProvisioningRejected
	}
	return updated, nil
}

type MemoryProvisioningRepository struct {
	mu        sync.Mutex
	byID      map[string]Provisioning
	byKey     map[string]string
	byIntent  map[string]string
	byCommand map[string]string
	results   map[string]ProvisioningResult
}

func NewMemoryProvisioningRepository() *MemoryProvisioningRepository {
	return &MemoryProvisioningRepository{
		byID: make(map[string]Provisioning), byKey: make(map[string]string), byIntent: make(map[string]string),
		byCommand: make(map[string]string),
		results:   make(map[string]ProvisioningResult),
	}
}

func (r *MemoryProvisioningRepository) ProjectProvisioningResult(_ context.Context, commit ResultProjectionCommit) (ProvisioningResult, error) {
	if commit.Validate() != nil {
		return ProvisioningResult{}, ErrResultProjectionRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var workflow Provisioning
	found := false
	for _, candidate := range r.byID {
		if candidate.CredentialLeaseID != commit.CredentialLeaseID {
			continue
		}
		if found {
			return ProvisioningResult{}, ErrResultProjectionRejected
		}
		workflow, found = candidate, true
	}
	if !found {
		return ProvisioningResult{}, ErrResultProjectionRejected
	}
	existing, exists := r.results[workflow.ID]
	var existingPointer *ProvisioningResult
	if exists {
		existingPointer = &existing
	}
	result, err := ApplyResultProjection(workflow, existingPointer, commit)
	if err != nil {
		return ProvisioningResult{}, err
	}
	r.results[workflow.ID] = result
	return result, nil
}

func (r *MemoryProvisioningRepository) GetProvisioningResult(ctx context.Context, intentID, accountID string, desiredGeneration uint64) (ProvisioningOutcome, error) {
	if ctx == nil || credential.ValidateTransportID(intentID) != nil || credential.ValidateTransportID(accountID) != nil || desiredGeneration == 0 {
		return ProvisioningOutcome{}, ErrResultProjectionRejected
	}
	if err := ctx.Err(); err != nil {
		return ProvisioningOutcome{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var workflow Provisioning
	found := false
	for _, candidate := range r.byID {
		if candidate.IntentID != intentID || candidate.AccountID != accountID || candidate.DesiredGeneration != desiredGeneration {
			continue
		}
		if !found || candidate.CreatedAt.After(workflow.CreatedAt) ||
			(candidate.CreatedAt.Equal(workflow.CreatedAt) && candidate.ID > workflow.ID) {
			workflow = candidate
			found = true
		}
	}
	if !found {
		return ProvisioningOutcome{}, ErrResultPending
	}
	if workflow.Validate() != nil {
		return ProvisioningOutcome{}, ErrResultProjectionRejected
	}
	switch workflow.Status {
	case ProvisioningCompleted:
		result, exists := r.results[workflow.ID]
		if !exists || result.Validate() != nil {
			return ProvisioningOutcome{}, ErrResultProjectionRejected
		}
		return NewSucceededProvisioningOutcome(result, workflow.UpdatedAt)
	case ProvisioningFailed:
		return NewTerminalProvisioningOutcome(
			&workflow, workflow.IntentID, workflow.AccountID, workflow.DesiredGeneration,
			ProvisioningFailureExpired(workflow.ErrorCode), workflow.UpdatedAt,
		)
	default:
		return ProvisioningOutcome{}, ErrResultPending
	}
}

func (r *MemoryProvisioningRepository) CreateProvisioning(_ context.Context, candidate Provisioning) (Provisioning, bool, error) {
	if candidate.Validate() != nil || candidate.Status != ProvisioningPendingKey {
		return Provisioning{}, false, ErrProvisioningRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, exists := r.byKey[candidate.IdempotencyKey]; exists {
		existing := r.byID[id]
		if !sameProvisioningIdentity(existing, candidate) {
			return Provisioning{}, false, ErrProvisioningRejected
		}
		return existing.Clone(), false, nil
	}
	if id, exists := r.byIntent[candidate.IntentID]; exists {
		existing := r.byID[id]
		if !sameProvisioningIdentity(existing, candidate) {
			return Provisioning{}, false, ErrProvisioningRejected
		}
		return existing.Clone(), false, nil
	}
	if _, exists := r.byID[candidate.ID]; exists || r.byCommand[candidate.KeyCommandID] != "" || r.byCommand[candidate.ActivationCommandID] != "" {
		return Provisioning{}, false, ErrProvisioningRejected
	}
	r.byID[candidate.ID] = candidate.Clone()
	r.byKey[candidate.IdempotencyKey] = candidate.ID
	r.byIntent[candidate.IntentID] = candidate.ID
	r.byCommand[candidate.KeyCommandID] = candidate.ID
	r.byCommand[candidate.ActivationCommandID] = candidate.ID
	return candidate.Clone(), true, nil
}

func (r *MemoryProvisioningRepository) ObserveProvisioningCommand(_ context.Context, observation ProvisioningCommandObservation) (Provisioning, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.byCommand[observation.CommandID]
	current, exists := r.byID[id]
	if !exists {
		return Provisioning{}, ErrProvisioningRejected
	}
	updated, err := ApplyProvisioningObservation(current, observation)
	if err != nil {
		return Provisioning{}, err
	}
	r.byID[id] = updated.Clone()
	return updated, nil
}

func (r *MemoryProvisioningRepository) MarkProvisioningActivationDispatched(_ context.Context, workflowID string, dispatchedAt time.Time) error {
	return r.transition(workflowID, dispatchedAt, func(record *Provisioning) bool {
		switch record.Status {
		case ProvisioningKeyReady:
			record.Status = ProvisioningActivationDispatched
			record.LastCommandID = record.ActivationCommandID
			touchProvisioning(record, dispatchedAt)
			return true
		case ProvisioningActivationDispatched:
			touchProvisioning(record, dispatchedAt)
			return true
		case ProvisioningActivationSucceeded, ProvisioningCompleted:
			return true
		default:
			return false
		}
	})
}

func (r *MemoryProvisioningRepository) MarkProvisioningKeyDispatched(_ context.Context, workflowID string, dispatchedAt time.Time) error {
	return r.transition(workflowID, dispatchedAt, func(record *Provisioning) bool {
		switch record.Status {
		case ProvisioningPendingKey:
			record.Status = ProvisioningKeyDispatched
			record.LastCommandID = record.KeyCommandID
			touchProvisioning(record, dispatchedAt)
			return true
		case ProvisioningKeyDispatched:
			touchProvisioning(record, dispatchedAt)
			return true
		case ProvisioningKeyReady, ProvisioningActivationDispatched,
			ProvisioningActivationSucceeded, ProvisioningCompleted:
			return true
		default:
			return false
		}
	})
}

func (r *MemoryProvisioningRepository) CompleteProvisioning(_ context.Context, workflowID string, completedAt time.Time) error {
	return r.transition(workflowID, completedAt, func(record *Provisioning) bool {
		if record.Status == ProvisioningCompleted {
			return true
		}
		if record.Status != ProvisioningActivationSucceeded {
			return false
		}
		record.Status = ProvisioningCompleted
		touchProvisioning(record, completedAt)
		return true
	})
}

func (r *MemoryProvisioningRepository) FailProvisioning(_ context.Context, workflowID, errorCode string, failedAt time.Time) error {
	if !provisioningErrorCode.MatchString(errorCode) {
		return ErrProvisioningRejected
	}
	return r.transition(workflowID, failedAt, func(record *Provisioning) bool {
		if record.Status == ProvisioningFailed {
			return record.ErrorCode == errorCode
		}
		switch record.Status {
		case ProvisioningPendingKey, ProvisioningKeyDispatched:
			record.LastCommandID = record.KeyCommandID
		case ProvisioningKeyReady, ProvisioningActivationDispatched, ProvisioningActivationSucceeded:
			record.LastCommandID = record.ActivationCommandID
		default:
			return false
		}
		record.Status = ProvisioningFailed
		record.ErrorCode = errorCode
		touchProvisioning(record, failedAt)
		return true
	})
}

func (r *MemoryProvisioningRepository) transition(workflowID string, at time.Time, apply func(*Provisioning) bool) error {
	if credential.ValidateTransportID(workflowID) != nil || at.IsZero() {
		return ErrProvisioningRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.byID[workflowID]
	if !exists || !apply(&record) {
		return ErrProvisioningRejected
	}
	before := r.byID[workflowID]
	if record.UpdatedAt.Equal(before.UpdatedAt) &&
		(record.Status != before.Status || record.LastCommandID != before.LastCommandID || record.ErrorCode != before.ErrorCode) {
		record.UpdatedAt = at.UTC()
	}
	if record.Validate() != nil {
		return ErrProvisioningRejected
	}
	r.byID[workflowID] = record
	return nil
}

func touchProvisioning(record *Provisioning, at time.Time) {
	if record != nil && at.After(record.UpdatedAt) {
		record.UpdatedAt = at.UTC()
	}
}

func (r *MemoryProvisioningRepository) GetProvisioning(_ context.Context, workflowID string) (Provisioning, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.byID[workflowID]
	if !exists {
		return Provisioning{}, ErrProvisioningRejected
	}
	return record.Clone(), nil
}

func (r *MemoryProvisioningRepository) DeferProvisioningRetry(_ context.Context, workflowID string, retryAt time.Time) error {
	if credential.ValidateTransportID(workflowID) != nil || retryAt.IsZero() {
		return ErrProvisioningRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	record, exists := r.byID[workflowID]
	if !exists || record.Status == ProvisioningCompleted || record.Status == ProvisioningFailed {
		return ErrProvisioningRejected
	}
	touchProvisioning(&record, retryAt)
	if record.Validate() != nil {
		return ErrProvisioningRejected
	}
	r.byID[workflowID] = record
	return nil
}

func (r *MemoryProvisioningRepository) ListActiveProvisioningIDs(_ context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrProvisioningRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	type candidate struct {
		id        string
		updatedAt time.Time
	}
	candidates := make([]candidate, 0, len(r.byID))
	for id, record := range r.byID {
		if record.Status == ProvisioningCompleted || record.Status == ProvisioningFailed {
			continue
		}
		candidates = append(candidates, candidate{id: id, updatedAt: record.UpdatedAt})
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].updatedAt.Equal(candidates[right].updatedAt) {
			return candidates[left].id < candidates[right].id
		}
		return candidates[left].updatedAt.Before(candidates[right].updatedAt)
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	ids := make([]string, len(candidates))
	for index := range candidates {
		ids[index] = candidates[index].id
	}
	return ids, nil
}

func sameProvisioningIdentity(left, right Provisioning) bool {
	return left.IdempotencyKey == right.IdempotencyKey && left.IntentID == right.IntentID && left.Owner == right.Owner &&
		left.AccountID == right.AccountID && left.DesiredGeneration == right.DesiredGeneration && left.NodeID == right.NodeID &&
		left.SlotID == right.SlotID && left.ExecutionEpoch == right.ExecutionEpoch && left.ImageDigest == right.ImageDigest &&
		left.CredentialLeaseID == right.CredentialLeaseID && left.ProxyLeaseID == right.ProxyLeaseID &&
		left.KeyCommandID == right.KeyCommandID && left.ActivationCommandID == right.ActivationCommandID &&
		left.CommandDeadline.Equal(right.CommandDeadline)
}

var _ ProvisioningRepository = (*MemoryProvisioningRepository)(nil)
var _ ActiveProvisioningRepository = (*MemoryProvisioningRepository)(nil)
var _ ResultProjectionRepository = (*MemoryProvisioningRepository)(nil)
