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
	ErrBusy       = errors.New("runtime outbox checkpoint is leased by another consumer")
	ErrNotClaimed = errors.New("runtime outbox event is not claimed by this consumer")
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

func (e Event) Validate() error {
	if e.Sequence <= 0 || e.EventID == "" || e.AccountID <= 0 || e.EventType == "" || e.DesiredGeneration == 0 || len(e.PayloadJSON) == 0 || len(e.PayloadJSON) > 64<<10 || e.CreatedAt.IsZero() {
		return errors.New("invalid runtime outbox event")
	}
	var payload map[string]any
	if err := json.Unmarshal(e.PayloadJSON, &payload); err != nil {
		return errors.New("runtime outbox payload must be a JSON object")
	}
	if containsSensitivePayload(payload) {
		return errors.New("runtime outbox payload contains sensitive data")
	}
	return nil
}

type Source interface {
	Claim(ctx context.Context, consumerName, owner string, now time.Time, leaseTTL time.Duration) (Event, bool, error)
	Ack(ctx context.Context, consumerName, owner string, sequence int64, now time.Time) error
	Nack(ctx context.Context, consumerName, owner string, sequence int64, errorCode string, now time.Time) error
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
	now          func() time.Time
}

func NewConsumer(source Source, handler Handler, consumerName, owner string, leaseTTL time.Duration, now func() time.Time) (*Consumer, error) {
	if source == nil || handler == nil || consumerName == "" || owner == "" || leaseTTL <= 0 || now == nil {
		return nil, errors.New("runtime outbox consumer configuration is incomplete")
	}
	return &Consumer{source: source, handler: handler, consumerName: consumerName, owner: owner, leaseTTL: leaseTTL, now: now}, nil
}

func (c *Consumer) RunOnce(ctx context.Context) (bool, error) {
	now := c.now().UTC()
	event, claimed, err := c.source.Claim(ctx, c.consumerName, c.owner, now, c.leaseTTL)
	if err != nil || !claimed {
		return false, err
	}
	if err := event.Validate(); err != nil {
		_ = c.source.Nack(ctx, c.consumerName, c.owner, event.Sequence, "invalid_event", c.now().UTC())
		return true, err
	}
	if err := c.handler.ApplyRuntimeEvent(ctx, event); err != nil {
		if nackErr := c.source.Nack(ctx, c.consumerName, c.owner, event.Sequence, "handler_failed", c.now().UTC()); nackErr != nil {
			return true, errors.Join(err, nackErr)
		}
		return true, err
	}
	if err := c.source.Ack(ctx, c.consumerName, c.owner, event.Sequence, c.now().UTC()); err != nil {
		return true, err
	}
	return true, nil
}

type memoryCheckpoint struct {
	lastSequence    int64
	claimedSequence int64
	lockedBy        string
	leaseExpiresAt  time.Time
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

func (s *MemorySource) Claim(_ context.Context, consumerName, owner string, now time.Time, leaseTTL time.Duration) (Event, bool, error) {
	if err := validateClaim(consumerName, owner, now, leaseTTL); err != nil {
		return Event{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint := s.checkpoints[consumerName]
	if checkpoint.lockedBy != "" && checkpoint.lockedBy != owner && checkpoint.leaseExpiresAt.After(now) {
		return Event{}, false, ErrBusy
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
		return Event{}, false, nil
	}
	checkpoint.claimedSequence = sequence
	checkpoint.lockedBy = owner
	checkpoint.leaseExpiresAt = now.Add(leaseTTL)
	s.checkpoints[consumerName] = checkpoint
	for _, event := range s.events {
		if event.Sequence == sequence {
			event.PayloadJSON = append([]byte(nil), event.PayloadJSON...)
			return event, true, nil
		}
	}
	return Event{}, false, errors.New("claimed runtime outbox event is missing")
}

func (s *MemorySource) Ack(_ context.Context, consumerName, owner string, sequence int64, now time.Time) error {
	return s.finish(consumerName, owner, sequence, now, true)
}

func (s *MemorySource) Nack(_ context.Context, consumerName, owner string, sequence int64, errorCode string, now time.Time) error {
	if !validErrorCode(errorCode) {
		return errors.New("invalid runtime outbox failure code")
	}
	return s.finish(consumerName, owner, sequence, now, false)
}

func (s *MemorySource) finish(consumerName, owner string, sequence int64, now time.Time, acknowledge bool) error {
	if consumerName == "" || owner == "" || sequence <= 0 || now.IsZero() {
		return errors.New("invalid runtime outbox completion")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	checkpoint, exists := s.checkpoints[consumerName]
	if !exists {
		return ErrNotClaimed
	}
	if acknowledge && checkpoint.lastSequence >= sequence {
		return nil
	}
	if checkpoint.claimedSequence != sequence || checkpoint.lockedBy != owner || (acknowledge && !checkpoint.leaseExpiresAt.After(now)) {
		return ErrNotClaimed
	}
	if acknowledge {
		checkpoint.lastSequence = sequence
	}
	checkpoint.claimedSequence = 0
	checkpoint.lockedBy = ""
	checkpoint.leaseExpiresAt = time.Time{}
	s.checkpoints[consumerName] = checkpoint
	return nil
}

func validateClaim(consumerName, owner string, now time.Time, leaseTTL time.Duration) error {
	if consumerName == "" || owner == "" || len(consumerName) > 128 || len(owner) > 128 || now.IsZero() || leaseTTL <= 0 {
		return errors.New("invalid runtime outbox claim")
	}
	return nil
}

func validErrorCode(value string) bool {
	return value != "" && len(value) <= 512 && !sensitiveString(value)
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
