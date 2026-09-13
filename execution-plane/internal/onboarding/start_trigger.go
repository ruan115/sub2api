package onboarding

import (
	"context"
	"errors"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

const (
	StartTriggerPending = "pending"
	StartTriggerClaimed = "claimed"
	StartTriggerStarted = "started"
	StartTriggerExpired = "expired"
)

var (
	ErrStartTriggerRejected  = errors.New("onboarding start trigger rejected")
	ErrStartTriggerClaimLost = errors.New("onboarding start trigger claim is no longer current")
	startTriggerImageDigest  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	startTriggerErrorCode    = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
)

// StartTriggerProjection is the secret-free, immutable projection of one
// exact CCMAX onboarding outbox event and its ready-slot desired state.
// ProjectedAt is the trusted authorization/application anchor, not part of
// replay identity. CCMAX outbox consumers use EventCreatedAt so delayed replay
// cannot erase an intent that was valid when the durable event committed.
type StartTriggerProjection struct {
	SourceSequence     int64
	EventID            string
	EventType          string
	EventCreatedAt     time.Time
	IntentID           string
	AccountID          string
	DesiredGeneration  uint64
	SlotID             string
	Provider           string
	RequiredLabels     map[string]string
	ImageDigest        string
	CPURequestMillis   uint64
	MemoryRequestBytes uint64
	ProjectedAt        time.Time
}

func (p StartTriggerProjection) Validate() error {
	if p.ValidateAnchor() != nil || p.CPURequestMillis == 0 || p.MemoryRequestBytes == 0 ||
		!startTriggerImageDigest.MatchString(p.ImageDigest) {
		return ErrStartTriggerRejected
	}
	return nil
}

// ValidateAnchor checks only the fields that identify the source event and
// its stable runtime target. Replay deliberately runs this narrower check
// before considering caller policy that was already frozen on first commit.
func (p StartTriggerProjection) ValidateAnchor() error {
	if p.SourceSequence <= 0 || !validStartTriggerEventType(p.EventType) || p.Provider != "docker" ||
		p.DesiredGeneration == 0 {
		return ErrStartTriggerRejected
	}
	for _, value := range []string{p.EventID, p.IntentID, p.AccountID, p.SlotID} {
		if credential.ValidateTransportID(value) != nil {
			return ErrStartTriggerRejected
		}
	}
	if !validStartTriggerTime(p.EventCreatedAt) || !validStartTriggerTime(p.ProjectedAt) ||
		p.EventCreatedAt.After(p.ProjectedAt) {
		return ErrStartTriggerRejected
	}
	return nil
}

// OnboardingStartTrigger is a durable queue item. The event, intent, slot and
// reservation fields are immutable; only the claim/retry/start state changes.
type OnboardingStartTrigger struct {
	StartTriggerProjection
	IntentExpiresAt   time.Time
	ReservationID     string
	BindingRevision   uint64
	Status            string
	ClaimOwner        string
	ClaimVersion      uint64
	ClaimExpiresAt    *time.Time
	NextAttemptAt     time.Time
	AttemptCount      uint32
	LastErrorCode     string
	StartedWorkflowID string
	StartedAt         *time.Time
}

func (t OnboardingStartTrigger) Validate() error {
	if t.StartTriggerProjection.Validate() != nil || !validStartTriggerTime(t.IntentExpiresAt) ||
		!t.IntentExpiresAt.After(t.ProjectedAt) || credential.ValidateTransportID(t.ReservationID) != nil ||
		t.BindingRevision == 0 || !validStartTriggerTime(t.NextAttemptAt) ||
		(t.LastErrorCode != "" && !startTriggerErrorCode.MatchString(t.LastErrorCode)) {
		return ErrStartTriggerRejected
	}
	if t.ClaimExpiresAt != nil && !validStartTriggerTime(*t.ClaimExpiresAt) {
		return ErrStartTriggerRejected
	}
	if t.StartedAt != nil && !validStartTriggerTime(*t.StartedAt) {
		return ErrStartTriggerRejected
	}
	switch t.Status {
	case StartTriggerPending:
		if t.ClaimOwner != "" || t.ClaimExpiresAt != nil || t.StartedWorkflowID != "" || t.StartedAt != nil {
			return ErrStartTriggerRejected
		}
	case StartTriggerExpired:
		if t.ClaimOwner != "" || t.ClaimExpiresAt != nil || t.StartedWorkflowID != "" || t.StartedAt != nil {
			return ErrStartTriggerRejected
		}
	case StartTriggerClaimed:
		if credential.ValidateTransportID(t.ClaimOwner) != nil || t.ClaimVersion == 0 || t.ClaimExpiresAt == nil ||
			t.StartedWorkflowID != "" || t.StartedAt != nil {
			return ErrStartTriggerRejected
		}
	case StartTriggerStarted:
		if credential.ValidateTransportID(t.ClaimOwner) != nil || t.ClaimVersion == 0 || t.ClaimExpiresAt == nil ||
			credential.ValidateTransportID(t.StartedWorkflowID) != nil || t.StartedAt == nil ||
			!t.ClaimExpiresAt.After(*t.StartedAt) || !t.IntentExpiresAt.After(*t.StartedAt) {
			return ErrStartTriggerRejected
		}
	default:
		return ErrStartTriggerRejected
	}
	return nil
}

type StartTriggerClaimQuery struct {
	Owner     string
	ClaimedAt time.Time
	ClaimTTL  time.Duration
	Limit     int
}

func (q StartTriggerClaimQuery) Validate() error {
	if credential.ValidateTransportID(q.Owner) != nil || !validStartTriggerTime(q.ClaimedAt) ||
		q.ClaimTTL <= 0 || q.ClaimTTL > 24*time.Hour || q.Limit <= 0 || q.Limit > 1000 {
		return ErrStartTriggerRejected
	}
	expiresAt := q.ClaimedAt.Add(q.ClaimTTL)
	if !validStartTriggerTime(expiresAt) || !expiresAt.After(q.ClaimedAt) {
		return ErrStartTriggerRejected
	}
	return nil
}

type StartTriggerRetry struct {
	EventID       string
	Owner         string
	ClaimVersion  uint64
	RetriedAt     time.Time
	NextAttemptAt time.Time
	ErrorCode     string
}

func (r StartTriggerRetry) Validate() error {
	if credential.ValidateTransportID(r.EventID) != nil || credential.ValidateTransportID(r.Owner) != nil ||
		r.ClaimVersion == 0 || !validStartTriggerTime(r.RetriedAt) || !validStartTriggerTime(r.NextAttemptAt) ||
		r.NextAttemptAt.Before(r.RetriedAt) || !startTriggerErrorCode.MatchString(r.ErrorCode) {
		return ErrStartTriggerRejected
	}
	return nil
}

type OnboardingStartTriggerRepository interface {
	LookupOnboardingStartTrigger(ctx context.Context, eventID string) (OnboardingStartTrigger, bool, error)
	ProjectOnboardingStartTrigger(ctx context.Context, projection StartTriggerProjection) (OnboardingStartTrigger, bool, error)
	ClaimDueOnboardingStartTriggers(ctx context.Context, query StartTriggerClaimQuery) ([]OnboardingStartTrigger, error)
	RetryOnboardingStartTrigger(ctx context.Context, retry StartTriggerRetry) error
}

func validStartTriggerEventType(value string) bool {
	switch value {
	case "account.runtime.provision_requested", "account.credential.migrate_requested", "account.credential.rotate_requested":
		return true
	default:
		return false
	}
}

func validStartTriggerTime(value time.Time) bool {
	return !value.IsZero() && value.Equal(value.UTC().Truncate(time.Microsecond))
}
