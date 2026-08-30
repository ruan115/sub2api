package onboarding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

const (
	IntentPending  = "pending"
	IntentClaimed  = "claimed"
	IntentConsumed = "consumed"
)

var (
	ErrIntentRejected    = errors.New("onboarding intent rejected")
	ErrIntentNotFound    = errors.New("onboarding intent was not found")
	ErrIntentExpired     = errors.New("onboarding intent has expired")
	ErrIntentUnavailable = errors.New("onboarding intent is unavailable")
)

type Intent struct {
	ID                string
	IdempotencyKey    string
	AccountID         string
	DesiredGeneration uint64
	Source            string
	AuthType          string
	Envelope          credential.Envelope
	Status            string
	ClaimOwner        string
	ClaimExpiresAt    *time.Time
	ExpiresAt         time.Time
	ConsumedAt        *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (i Intent) String() string {
	return fmt.Sprintf("Intent{ID:%q IdempotencyKey:%q AccountID:%q DesiredGeneration:%d Source:%q AuthType:%q Status:%q ClaimOwner:%q ExpiresAt:%s Envelope:[REDACTED]}",
		i.ID, i.IdempotencyKey, i.AccountID, i.DesiredGeneration, i.Source, i.AuthType, i.Status, i.ClaimOwner, i.ExpiresAt.UTC().Format(time.RFC3339Nano))
}

func (i Intent) GoString() string { return i.String() }

func (i Intent) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID                string `json:"intent_id"`
		IdempotencyKey    string `json:"idempotency_key"`
		AccountID         string `json:"account_id"`
		DesiredGeneration uint64 `json:"desired_generation"`
		Source            string `json:"source"`
		AuthType          string `json:"auth_type"`
		Status            string `json:"status"`
	}{i.ID, i.IdempotencyKey, i.AccountID, i.DesiredGeneration, i.Source, i.AuthType, i.Status})
}

func (i Intent) Metadata() credential.OnboardingIntentMetadata {
	return credential.OnboardingIntentMetadata{
		IntentID: i.ID, AccountID: i.AccountID, DesiredGeneration: i.DesiredGeneration,
		Source: i.Source, AuthType: i.AuthType,
	}
}

func (i Intent) Validate() error {
	if credential.ValidateTransportID(i.ID) != nil || credential.ValidateTransportID(i.IdempotencyKey) != nil ||
		credential.ValidateTransportID(i.AccountID) != nil || i.DesiredGeneration == 0 ||
		i.CreatedAt.IsZero() || !i.ExpiresAt.After(i.CreatedAt) || i.UpdatedAt.Before(i.CreatedAt) ||
		i.Envelope.Validate() != nil {
		return ErrIntentRejected
	}
	if i.Metadata().Validate() != nil {
		return ErrIntentRejected
	}
	switch i.Status {
	case IntentPending:
		if i.ClaimOwner != "" || i.ClaimExpiresAt != nil || i.ConsumedAt != nil {
			return ErrIntentRejected
		}
	case IntentClaimed:
		if credential.ValidateTransportID(i.ClaimOwner) != nil || i.ClaimExpiresAt == nil ||
			!i.ClaimExpiresAt.After(i.UpdatedAt) || i.ConsumedAt != nil {
			return ErrIntentRejected
		}
	case IntentConsumed:
		if credential.ValidateTransportID(i.ClaimOwner) != nil || i.ClaimExpiresAt != nil ||
			i.ConsumedAt == nil || i.ConsumedAt.Before(i.CreatedAt) || i.UpdatedAt.Before(*i.ConsumedAt) {
			return ErrIntentRejected
		}
	default:
		return ErrIntentRejected
	}
	return nil
}

func (i Intent) Clone() Intent {
	i.Envelope = i.Envelope.Clone()
	if i.ClaimExpiresAt != nil {
		value := i.ClaimExpiresAt.UTC()
		i.ClaimExpiresAt = &value
	}
	if i.ConsumedAt != nil {
		value := i.ConsumedAt.UTC()
		i.ConsumedAt = &value
	}
	return i
}

func (i *Intent) Destroy() {
	if i == nil {
		return
	}
	i.Envelope.Destroy()
}

type Claim struct {
	IntentID          string
	AccountID         string
	DesiredGeneration uint64
	Owner             string
	ClaimedAt         time.Time
	ClaimExpiresAt    time.Time
}

func (c Claim) Validate() error { return validateClaim(c) }

type Repository interface {
	CreateIntent(ctx context.Context, candidate Intent) (intent Intent, created bool, err error)
	RecoverIntentReceipt(ctx context.Context, lookup ReceiptLookup) (Receipt, error)
	ClaimIntent(ctx context.Context, claim Claim) (Intent, error)
	CompleteIntent(ctx context.Context, intentID, owner string, completedAt time.Time) error
}

type VaultConfig struct {
	Crypto     *credential.Service
	Repository Repository
	Random     io.Reader
	Now        func() time.Time
	IntentTTL  time.Duration
	ClaimTTL   time.Duration
}

type Vault struct {
	crypto     *credential.Service
	repository Repository
	random     io.Reader
	now        func() time.Time
	intentTTL  time.Duration
	claimTTL   time.Duration
}

type CreateRequest struct {
	IdempotencyKey    string
	AccountID         string
	DesiredGeneration uint64
	Input             *worker.OnboardingInput
}

type RecoverRequest struct {
	IdempotencyKey    string
	AccountID         string
	DesiredGeneration uint64
	Source            worker.OnboardingSource
	AuthType          string
}

func (r RecoverRequest) Validate() error {
	if credential.ValidateTransportID(r.IdempotencyKey) != nil ||
		credential.ValidateTransportID(r.AccountID) != nil || r.DesiredGeneration == 0 ||
		!validOnboardingIdentity(r.Source, r.AuthType) {
		return ErrIntentUnavailable
	}
	return nil
}

// ReceiptLookup is the exact, non-secret repository key for recovering a
// durable receipt. Repository implementations must not load the encrypted
// payload while servicing this lookup.
type ReceiptLookup struct {
	IdempotencyKey    string
	AccountID         string
	DesiredGeneration uint64
	Source            string
	AuthType          string
	Now               time.Time
}

func (l ReceiptLookup) Validate() error {
	if !validReceiptLookup(l) {
		return ErrIntentUnavailable
	}
	return nil
}

type Receipt struct {
	IntentID          string    `json:"intent_id"`
	AccountID         string    `json:"account_id"`
	DesiredGeneration uint64    `json:"desired_generation"`
	Source            string    `json:"source"`
	AuthType          string    `json:"auth_type"`
	ExpiresAt         time.Time `json:"expires_at"`
}

func NewVault(config VaultConfig) (*Vault, error) {
	if config.Crypto == nil || config.Repository == nil || config.IntentTTL <= 0 || config.IntentTTL > 24*time.Hour ||
		config.ClaimTTL <= 0 || config.ClaimTTL > config.IntentTTL {
		return nil, ErrIntentRejected
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Vault{
		crypto: config.Crypto, repository: config.Repository, random: config.Random,
		now: config.Now, intentTTL: config.IntentTTL, claimTTL: config.ClaimTTL,
	}, nil
}

// Create consumes request.Input on all paths. Only its KMS envelope is handed
// to the repository; the receipt is safe for CCMAX and runtime_outbox.
func (v *Vault) Create(ctx context.Context, request CreateRequest) (Receipt, error) {
	if request.Input != nil {
		defer request.Input.Destroy()
	}
	if v == nil || ctx == nil || ctx.Err() != nil || request.Input == nil ||
		credential.ValidateTransportID(request.IdempotencyKey) != nil ||
		credential.ValidateTransportID(request.AccountID) != nil || request.DesiredGeneration == 0 ||
		request.Input.Validate() != nil {
		return Receipt{}, ErrIntentRejected
	}
	payload, err := worker.EncodeOnboardingInput(*request.Input)
	if err != nil {
		return Receipt{}, ErrIntentRejected
	}
	defer eraseBytes(payload)
	intentID, err := randomIntentID(v.random)
	if err != nil {
		return Receipt{}, ErrIntentRejected
	}
	now := v.now().UTC()
	record := Intent{
		ID: intentID, IdempotencyKey: request.IdempotencyKey, AccountID: request.AccountID,
		DesiredGeneration: request.DesiredGeneration, Source: string(request.Input.Source), AuthType: request.Input.AuthType,
		Status: IntentPending, ExpiresAt: now.Add(v.intentTTL), CreatedAt: now, UpdatedAt: now,
	}
	record.Envelope, err = v.crypto.SealOnboardingIntent(ctx, record.Metadata(), payload)
	if err != nil {
		return Receipt{}, ErrIntentRejected
	}
	defer record.Destroy()
	stored, _, err := v.repository.CreateIntent(ctx, record)
	if err != nil {
		return Receipt{}, ErrIntentRejected
	}
	defer stored.Destroy()
	if !sameIntentIdentity(stored, record) || !stored.ExpiresAt.After(now) {
		return Receipt{}, ErrIntentRejected
	}
	return receiptFromIntent(stored), nil
}

// Recover returns only the non-secret receipt for an exact, pending and
// unexpired durable intent. It never opens the KMS envelope.
func (v *Vault) Recover(ctx context.Context, request RecoverRequest) (Receipt, error) {
	if v == nil || ctx == nil || request.Validate() != nil {
		return Receipt{}, ErrIntentUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Receipt{}, err
	}
	lookup := ReceiptLookup{
		IdempotencyKey: request.IdempotencyKey, AccountID: request.AccountID,
		DesiredGeneration: request.DesiredGeneration, Source: string(request.Source), AuthType: request.AuthType,
		Now: v.now().UTC(),
	}
	receipt, err := v.repository.RecoverIntentReceipt(ctx, lookup)
	if err != nil {
		if errors.Is(err, ErrIntentNotFound) {
			return Receipt{}, ErrIntentNotFound
		}
		if errors.Is(err, ErrIntentExpired) {
			return Receipt{}, ErrIntentExpired
		}
		if errors.Is(err, ErrIntentUnavailable) {
			return Receipt{}, ErrIntentUnavailable
		}
		return Receipt{}, fmt.Errorf("recover onboarding intent receipt: %w", err)
	}
	if !sameReceiptLookup(receipt, lookup) || !receipt.ExpiresAt.After(lookup.Now) {
		return Receipt{}, ErrIntentUnavailable
	}
	return receipt, nil
}

// ClaimAndOpen returns one caller-owned plaintext input. A repeated call by the
// same provisioning owner is allowed until claim expiry; another owner fails.
func (v *Vault) ClaimAndOpen(ctx context.Context, intentID, accountID string, desiredGeneration uint64, owner string) (worker.OnboardingInput, error) {
	if v == nil || ctx == nil || ctx.Err() != nil || credential.ValidateTransportID(intentID) != nil ||
		credential.ValidateTransportID(accountID) != nil || desiredGeneration == 0 || credential.ValidateTransportID(owner) != nil {
		return worker.OnboardingInput{}, ErrIntentUnavailable
	}
	now := v.now().UTC()
	record, err := v.repository.ClaimIntent(ctx, Claim{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: desiredGeneration,
		Owner: owner, ClaimedAt: now, ClaimExpiresAt: now.Add(v.claimTTL),
	})
	if err != nil {
		return worker.OnboardingInput{}, ErrIntentUnavailable
	}
	defer record.Destroy()
	plaintext, err := v.crypto.OpenOnboardingIntent(ctx, record.Metadata(), record.Envelope)
	if err != nil {
		return worker.OnboardingInput{}, ErrIntentUnavailable
	}
	defer eraseBytes(plaintext)
	input, err := worker.DecodeOnboardingInput(plaintext)
	if err != nil || string(input.Source) != record.Source || input.AuthType != record.AuthType {
		input.Destroy()
		return worker.OnboardingInput{}, ErrIntentUnavailable
	}
	return input, nil
}

func (v *Vault) Complete(ctx context.Context, intentID, owner string) error {
	if v == nil || ctx == nil || ctx.Err() != nil || credential.ValidateTransportID(intentID) != nil ||
		credential.ValidateTransportID(owner) != nil {
		return ErrIntentUnavailable
	}
	if err := v.repository.CompleteIntent(ctx, intentID, owner, v.now().UTC()); err != nil {
		return ErrIntentUnavailable
	}
	return nil
}

func randomIntentID(random io.Reader) (string, error) {
	buffer := make([]byte, 16)
	if _, err := io.ReadFull(random, buffer); err != nil {
		eraseBytes(buffer)
		return "", err
	}
	buffer[6] = buffer[6]&0x0f | 0x40
	buffer[8] = buffer[8]&0x3f | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], buffer[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], buffer[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], buffer[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], buffer[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], buffer[10:16])
	eraseBytes(buffer)
	return string(encoded), nil
}

func receiptFromIntent(intent Intent) Receipt {
	return Receipt{
		IntentID: intent.ID, AccountID: intent.AccountID, DesiredGeneration: intent.DesiredGeneration,
		Source: intent.Source, AuthType: intent.AuthType, ExpiresAt: intent.ExpiresAt.UTC(),
	}
}

func sameReceiptLookup(receipt Receipt, lookup ReceiptLookup) bool {
	return receipt.AccountID == lookup.AccountID && receipt.DesiredGeneration == lookup.DesiredGeneration &&
		receipt.Source == lookup.Source && receipt.AuthType == lookup.AuthType &&
		credential.ValidateTransportID(receipt.IntentID) == nil && !receipt.ExpiresAt.IsZero()
}

func validReceiptLookup(lookup ReceiptLookup) bool {
	return credential.ValidateTransportID(lookup.IdempotencyKey) == nil &&
		credential.ValidateTransportID(lookup.AccountID) == nil && lookup.DesiredGeneration > 0 &&
		!lookup.Now.IsZero() && validOnboardingIdentity(worker.OnboardingSource(lookup.Source), lookup.AuthType)
}

func validOnboardingIdentity(source worker.OnboardingSource, authType string) bool {
	switch source {
	case worker.OnboardingSessionKey, worker.OnboardingOAuthCode, worker.OnboardingCookie:
		return authType == worker.AuthTypeOAuth || authType == worker.AuthTypeSetupToken
	case worker.OnboardingSetupToken:
		return authType == worker.AuthTypeSetupToken
	case worker.OnboardingAPIKey:
		return authType == worker.AuthTypeAPIKey
	case worker.OnboardingCredentialImport:
		return authType == worker.AuthTypeOAuth || authType == worker.AuthTypeSetupToken || authType == worker.AuthTypeAPIKey
	default:
		return false
	}
}

func sameIntentIdentity(left, right Intent) bool {
	return left.IdempotencyKey == right.IdempotencyKey && left.AccountID == right.AccountID &&
		left.DesiredGeneration == right.DesiredGeneration && left.Source == right.Source && left.AuthType == right.AuthType
}

func eraseBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

// MemoryRepository provides the exact claim/replay semantics used by local and
// race tests. Production persistence is implemented by runtime/store.
type MemoryRepository struct {
	mu              sync.Mutex
	byID            map[string]Intent
	idByIdempotency map[string]string
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{byID: make(map[string]Intent), idByIdempotency: make(map[string]string)}
}

func (r *MemoryRepository) CreateIntent(_ context.Context, candidate Intent) (Intent, bool, error) {
	if candidate.Validate() != nil {
		return Intent{}, false, ErrIntentRejected
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if id, exists := r.idByIdempotency[candidate.IdempotencyKey]; exists {
		existing := r.byID[id]
		if existing.AccountID != candidate.AccountID || existing.DesiredGeneration != candidate.DesiredGeneration ||
			existing.Source != candidate.Source || existing.AuthType != candidate.AuthType {
			return Intent{}, false, ErrIntentRejected
		}
		return existing.Clone(), false, nil
	}
	if _, exists := r.byID[candidate.ID]; exists {
		return Intent{}, false, ErrIntentRejected
	}
	r.byID[candidate.ID] = candidate.Clone()
	r.idByIdempotency[candidate.IdempotencyKey] = candidate.ID
	return candidate.Clone(), true, nil
}

func (r *MemoryRepository) RecoverIntentReceipt(_ context.Context, lookup ReceiptLookup) (Receipt, error) {
	if !validReceiptLookup(lookup) {
		return Receipt{}, ErrIntentUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	id, exists := r.idByIdempotency[lookup.IdempotencyKey]
	if !exists {
		return Receipt{}, ErrIntentNotFound
	}
	intent, exists := r.byID[id]
	if !exists || intent.AccountID != lookup.AccountID || intent.DesiredGeneration != lookup.DesiredGeneration ||
		intent.Source != lookup.Source || intent.AuthType != lookup.AuthType || intent.Status != IntentPending {
		return Receipt{}, ErrIntentUnavailable
	}
	if !intent.ExpiresAt.After(lookup.Now) {
		return Receipt{}, ErrIntentExpired
	}
	return receiptFromIntent(intent), nil
}

func (r *MemoryRepository) ClaimIntent(_ context.Context, claim Claim) (Intent, error) {
	if validateClaim(claim) != nil {
		return Intent{}, ErrIntentUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	intent, exists := r.byID[claim.IntentID]
	if !exists || intent.AccountID != claim.AccountID || intent.DesiredGeneration != claim.DesiredGeneration ||
		intent.Status == IntentConsumed || !intent.ExpiresAt.After(claim.ClaimedAt) ||
		(intent.Status == IntentClaimed && intent.ClaimOwner != claim.Owner && intent.ClaimExpiresAt != nil && intent.ClaimExpiresAt.After(claim.ClaimedAt)) {
		return Intent{}, ErrIntentUnavailable
	}
	intent.Status = IntentClaimed
	intent.ClaimOwner = claim.Owner
	claimExpiry := claim.ClaimExpiresAt.UTC()
	intent.ClaimExpiresAt = &claimExpiry
	intent.UpdatedAt = claim.ClaimedAt.UTC()
	r.byID[intent.ID] = intent
	return intent.Clone(), nil
}

func (r *MemoryRepository) CompleteIntent(_ context.Context, intentID, owner string, completedAt time.Time) error {
	if credential.ValidateTransportID(intentID) != nil || credential.ValidateTransportID(owner) != nil || completedAt.IsZero() {
		return ErrIntentUnavailable
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	intent, exists := r.byID[intentID]
	if !exists || intent.ClaimOwner != owner {
		return ErrIntentUnavailable
	}
	if intent.Status == IntentConsumed {
		return nil
	}
	if intent.Status != IntentClaimed || intent.ClaimExpiresAt == nil ||
		!intent.ClaimExpiresAt.After(completedAt) || !intent.ExpiresAt.After(completedAt) {
		return ErrIntentUnavailable
	}
	completed := completedAt.UTC()
	intent.Status = IntentConsumed
	intent.ConsumedAt = &completed
	intent.ClaimExpiresAt = nil
	intent.UpdatedAt = completed
	r.byID[intent.ID] = intent
	return nil
}

func validateClaim(claim Claim) error {
	if credential.ValidateTransportID(claim.IntentID) != nil || credential.ValidateTransportID(claim.AccountID) != nil ||
		claim.DesiredGeneration == 0 || credential.ValidateTransportID(claim.Owner) != nil || claim.ClaimedAt.IsZero() ||
		!claim.ClaimExpiresAt.After(claim.ClaimedAt) {
		return ErrIntentUnavailable
	}
	return nil
}

var _ Repository = (*MemoryRepository)(nil)
