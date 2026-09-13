package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrBusy              = errors.New("runtime outbox checkpoint is leased by another consumer")
	ErrNotClaimed        = errors.New("runtime outbox event is not claimed by this consumer")
	ErrCheckpointMissing = errors.New("runtime outbox checkpoint is missing")
)

type Event struct {
	Sequence          int64
	EventID           string
	AccountID         int64
	EventType         string
	DesiredGeneration uint64
	PayloadJSON       []byte
	CreatedAt         time.Time
}

type ClaimedEvent struct {
	Event
	ConsumerName   string
	Owner          string
	ClaimVersion   uint64
	LeaseExpiresAt time.Time
}

func (c ClaimedEvent) Validate() error {
	if c.ValidateToken() != nil || c.Event.Validate() != nil {
		return errors.New("invalid runtime outbox claim token")
	}
	return nil
}

// ValidateToken deliberately validates only the checkpoint fence. Fail must
// be able to persist a blocked state for a malformed event payload; requiring
// Event.Validate there would make the poison row impossible to quarantine.
func (c ClaimedEvent) ValidateToken() error {
	if c.Event.Sequence <= 0 || c.ConsumerName == "" || c.Owner == "" || c.ClaimVersion == 0 ||
		c.LeaseExpiresAt.IsZero() {
		return errors.New("invalid runtime outbox claim token")
	}
	return nil
}

func (e Event) Validate() error {
	if e.Sequence <= 0 || e.EventID == "" || e.AccountID <= 0 || e.EventType == "" || e.DesiredGeneration == 0 || len(e.PayloadJSON) == 0 || len(e.PayloadJSON) > 64<<10 || e.CreatedAt.IsZero() {
		return errors.New("invalid runtime outbox event")
	}
	var payload map[string]any
	if err := json.Unmarshal(e.PayloadJSON, &payload); err != nil || payload == nil {
		return errors.New("runtime outbox payload must be a JSON object")
	}
	if containsSensitivePayload(payload) {
		return errors.New("runtime outbox payload contains sensitive data")
	}
	return nil
}

type Source interface {
	Claim(ctx context.Context, consumerName, owner string, now time.Time, leaseTTL time.Duration) (ClaimedEvent, bool, error)
	Ack(ctx context.Context, claim ClaimedEvent, now time.Time) error
	Fail(ctx context.Context, claim ClaimedEvent, failure Failure) error
}

type Handler interface {
	ApplyRuntimeEvent(ctx context.Context, event Event) error
}

type Consumer struct {
	source       Source
	handler      Handler
	consumerName string
	owner        string
	leaseTTL     time.Duration
	retryLimit   uint64
	now          func() time.Time
}

func NewConsumer(
	source Source,
	handler Handler,
	consumerName string,
	owner string,
	leaseTTL time.Duration,
	now func() time.Time,
	maxRetryFailures ...uint64,
) (*Consumer, error) {
	retryLimit := DefaultMaxRetryFailures
	if len(maxRetryFailures) == 1 {
		retryLimit = maxRetryFailures[0]
	}
	if source == nil || handler == nil || consumerName == "" || owner == "" || leaseTTL <= 0 || now == nil ||
		len(maxRetryFailures) > 1 || retryLimit == 0 || retryLimit > 1000 {
		return nil, errors.New("runtime outbox consumer configuration is incomplete")
	}
	return &Consumer{
		source: source, handler: handler, consumerName: consumerName, owner: owner,
		leaseTTL: leaseTTL, retryLimit: retryLimit, now: now,
	}, nil
}

func (c *Consumer) RunOnce(ctx context.Context) (bool, error) {
	if c == nil || c.source == nil || c.handler == nil || ctx == nil || ctx.Err() != nil {
		return false, errors.New("runtime outbox consumer context is invalid")
	}
	now := c.now().UTC().Truncate(time.Millisecond)
	claim, claimed, err := c.source.Claim(ctx, c.consumerName, c.owner, now, c.leaseTTL)
	if err != nil || !claimed {
		return false, err
	}
	if err := claim.Event.Validate(); err != nil {
		failure := Failure{Class: FailureSecurity, Code: "invalid_event", FailedAt: c.now().UTC().Truncate(time.Millisecond)}
		if failErr := c.source.Fail(ctx, claim, failure); failErr != nil {
			return true, errors.Join(err, failErr)
		}
		return true, errors.Join(err, blockedErrorFor(claim, failure))
	}
	if err := c.handler.ApplyRuntimeEvent(ctx, claim.Event); err != nil {
		if ctx.Err() != nil {
			// A shutdown/cancellation is not an event failure. Leave the claim to
			// expire so another healthy owner can replay it.
			return true, ctx.Err()
		}
		failure := failureForHandlerError(err, c.now().UTC().Truncate(time.Millisecond))
		if failure.Class == FailureRetryable {
			failure.RetryLimit = c.retryLimit
		}
		if failErr := c.source.Fail(ctx, claim, failure); failErr != nil {
			return true, errors.Join(err, failErr)
		}
		if failure.Class != FailureRetryable {
			return true, errors.Join(err, blockedErrorFor(claim, failure))
		}
		return true, err
	}
	if err := c.source.Ack(ctx, claim, c.now().UTC().Truncate(time.Millisecond)); err != nil {
		return true, err
	}
	return true, nil
}

func blockedErrorFor(claim ClaimedEvent, failure Failure) *BlockedError {
	return &BlockedError{
		ConsumerName: claim.ConsumerName, Sequence: claim.Event.Sequence,
		Class: failure.Class, Code: failure.Code, BlockedClaimVersion: claim.ClaimVersion,
	}
}

type memoryCheckpoint struct {
	lastSequence        int64
	claimedSequence     int64
	lockedBy            string
	leaseExpiresAt      time.Time
	claimVersion        uint64
	failureState        string
	failureSequence     int64
	failureClass        FailureClass
	failureCode         string
	failureCount        uint64
	firstFailedAt       time.Time
	lastFailedAt        time.Time
	nextAttemptAt       time.Time
	blockedClaimVersion uint64
}

type MemorySource struct {
	mu          sync.Mutex
	events      []Event
	checkpoints map[string]memoryCheckpoint
}

func NewMemorySource(events []Event) (*MemorySource, error) {
	copyEvents := append([]Event(nil), events...)
	for index, event := range copyEvents {
		if err := event.Validate(); err != nil {
			return nil, err
		}
		if index > 0 && event.Sequence <= copyEvents[index-1].Sequence {
			return nil, errors.New("runtime outbox events are not strictly ordered")
		}
		copyEvents[index].PayloadJSON = append([]byte(nil), event.PayloadJSON...)
	}
	return &MemorySource{events: copyEvents, checkpoints: make(map[string]memoryCheckpoint)}, nil
}

func (s *MemorySource) Claim(_ context.Context, consumerName, owner string, now time.Time, leaseTTL time.Duration) (ClaimedEvent, bool, error) {
	if err := validateClaim(consumerName, owner, now, leaseTTL); err != nil {
		return ClaimedEvent{}, false, err
	}
	now = now.UTC().Truncate(time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint := s.checkpoints[consumerName]
	if checkpoint.failureState == "blocked" {
		return ClaimedEvent{}, false, &BlockedError{
			ConsumerName: consumerName, Sequence: checkpoint.failureSequence,
			Class: checkpoint.failureClass, Code: checkpoint.failureCode,
			BlockedClaimVersion: checkpoint.blockedClaimVersion,
		}
	}
	if checkpoint.failureState == "retry_wait" && checkpoint.nextAttemptAt.After(now) {
		return ClaimedEvent{}, false, nil
	}
	if checkpoint.lockedBy != "" && checkpoint.lockedBy != owner && checkpoint.leaseExpiresAt.After(now) {
		return ClaimedEvent{}, false, ErrBusy
	}
	sequence := checkpoint.claimedSequence
	if sequence <= checkpoint.lastSequence {
		sequence = 0
		for _, event := range s.events {
			if event.Sequence > checkpoint.lastSequence {
				sequence = event.Sequence
				break
			}
		}
	}
	if sequence == 0 {
		return ClaimedEvent{}, false, nil
	}
	var claimedEvent Event
	found := false
	for _, event := range s.events {
		if event.Sequence == sequence {
			claimedEvent = event
			claimedEvent.PayloadJSON = append([]byte(nil), event.PayloadJSON...)
			found = true
			break
		}
	}
	if !found {
		return ClaimedEvent{}, false, errors.New("claimed runtime outbox event is missing")
	}
	checkpoint.claimedSequence = sequence
	checkpoint.lockedBy = owner
	checkpoint.leaseExpiresAt = now.Add(leaseTTL).UTC().Truncate(time.Millisecond)
	checkpoint.claimVersion++
	checkpoint.failureState = "ready"
	s.checkpoints[consumerName] = checkpoint
	return ClaimedEvent{
		Event: claimedEvent, ConsumerName: consumerName, Owner: owner,
		ClaimVersion: checkpoint.claimVersion, LeaseExpiresAt: checkpoint.leaseExpiresAt,
	}, true, nil
}

func (s *MemorySource) Ack(_ context.Context, claim ClaimedEvent, now time.Time) error {
	if claim.Validate() != nil || now.IsZero() {
		return errors.New("invalid runtime outbox completion")
	}
	now = now.UTC().Truncate(time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, exists := s.checkpoints[claim.ConsumerName]
	if !exists {
		return ErrNotClaimed
	}
	if checkpoint.lastSequence >= claim.Event.Sequence {
		return nil
	}
	if !sameMemoryClaim(checkpoint, claim) || !checkpoint.leaseExpiresAt.After(now) {
		return ErrNotClaimed
	}
	checkpoint.lastSequence = claim.Event.Sequence
	checkpoint.claimedSequence = 0
	checkpoint.lockedBy = ""
	checkpoint.leaseExpiresAt = time.Time{}
	checkpoint.failureState = "ready"
	checkpoint.failureSequence = 0
	checkpoint.failureClass = ""
	checkpoint.failureCode = ""
	checkpoint.failureCount = 0
	checkpoint.firstFailedAt = time.Time{}
	checkpoint.lastFailedAt = time.Time{}
	checkpoint.nextAttemptAt = time.Time{}
	checkpoint.blockedClaimVersion = 0
	s.checkpoints[claim.ConsumerName] = checkpoint
	return nil
}

func (s *MemorySource) Fail(_ context.Context, claim ClaimedEvent, failure Failure) error {
	if claim.ValidateToken() != nil || failure.Validate() != nil || !claim.LeaseExpiresAt.After(failure.FailedAt) {
		return errors.New("invalid runtime outbox failure")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, exists := s.checkpoints[claim.ConsumerName]
	if !exists || !sameMemoryClaim(checkpoint, claim) || !checkpoint.leaseExpiresAt.After(failure.FailedAt) {
		return ErrNotClaimed
	}
	checkpoint.claimedSequence = 0
	checkpoint.lockedBy = ""
	checkpoint.leaseExpiresAt = time.Time{}
	checkpoint.failureSequence = claim.Event.Sequence
	checkpoint.failureClass = failure.Class
	checkpoint.failureCode = failure.Code
	checkpoint.failureCount++
	if checkpoint.firstFailedAt.IsZero() {
		checkpoint.firstFailedAt = failure.FailedAt
	}
	checkpoint.lastFailedAt = failure.FailedAt
	retryExhausted := failure.Class == FailureRetryable && checkpoint.failureCount >= failure.RetryLimit
	if failure.Class == FailureRetryable {
		checkpoint.failureState = "retry_wait"
		checkpoint.nextAttemptAt = failure.RetryAfter
		checkpoint.blockedClaimVersion = 0
	} else {
		checkpoint.failureState = "blocked"
		checkpoint.nextAttemptAt = time.Time{}
		checkpoint.blockedClaimVersion = claim.ClaimVersion
	}
	s.checkpoints[claim.ConsumerName] = checkpoint
	if retryExhausted {
		return &RetryBudgetError{
			ConsumerName: claim.ConsumerName, Sequence: claim.Event.Sequence,
			Code: failure.Code, FailureCount: checkpoint.failureCount,
		}
	}
	return nil
}

func (s *MemorySource) retryBlocked(
	consumerName string,
	expectedSequence int64,
	expectedClaimVersion uint64,
) error {
	if consumerName == "" || expectedSequence <= 0 || expectedClaimVersion == 0 {
		return errors.New("invalid runtime outbox retry authorization")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, exists := s.checkpoints[consumerName]
	if !exists || checkpoint.failureState != "blocked" || checkpoint.failureSequence != expectedSequence ||
		checkpoint.blockedClaimVersion != expectedClaimVersion || checkpoint.claimedSequence != 0 || checkpoint.lockedBy != "" {
		return ErrNotClaimed
	}
	checkpoint.failureState = "ready"
	checkpoint.failureSequence = 0
	checkpoint.failureClass = ""
	checkpoint.failureCode = ""
	checkpoint.failureCount = 0
	checkpoint.firstFailedAt = time.Time{}
	checkpoint.lastFailedAt = time.Time{}
	checkpoint.nextAttemptAt = time.Time{}
	checkpoint.blockedClaimVersion = 0
	s.checkpoints[consumerName] = checkpoint
	return nil
}

func sameMemoryClaim(checkpoint memoryCheckpoint, claim ClaimedEvent) bool {
	return checkpoint.claimedSequence == claim.Event.Sequence && checkpoint.lockedBy == claim.Owner &&
		checkpoint.claimVersion == claim.ClaimVersion && checkpoint.leaseExpiresAt.Equal(claim.LeaseExpiresAt)
}

func validateClaim(consumerName, owner string, now time.Time, leaseTTL time.Duration) error {
	if consumerName == "" || owner == "" || len(consumerName) > 128 || len(owner) > 128 || now.IsZero() || leaseTTL <= 0 {
		return errors.New("invalid runtime outbox claim")
	}
	return nil
}

func containsSensitivePayload(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			lower := strings.ToLower(key)
			for _, sensitive := range []string{"authorization", "credential", "password", "cookie", "secret", "session_key", "access_token", "refresh_token", "api_key", "proxy_url"} {
				if strings.Contains(lower, sensitive) {
					return true
				}
			}
			if containsSensitivePayload(child) {
				return true
			}
		}
	case []any:
		for _, child := range current {
			if containsSensitivePayload(child) {
				return true
			}
		}
	case string:
		return sensitiveString(current)
	}
	return false
}

func sensitiveString(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "bearer ") || strings.Contains(lower, "sk-ant-") || strings.Contains(lower, "sk-")
}

var _ Source = (*MemorySource)(nil)
