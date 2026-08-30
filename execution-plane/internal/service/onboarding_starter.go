package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

var ErrHealthySlotOnboardingStart = errors.New("healthy-slot onboarding starter failed")

type HealthySlotOnboardingStarterConfig struct {
	Now               func() time.Time
	ObservationMaxAge time.Duration
	CommandTTL        time.Duration
}

type HealthySlotOnboardingStartRequest struct {
	IntentID        string
	SlotID          string
	ReservationID   string
	BindingRevision uint64
}

type HealthySlotOnboardingStarter struct {
	repository        onboarding.HealthySlotStartRepository
	now               func() time.Time
	observationMaxAge time.Duration
	commandTTL        time.Duration
}

func NewHealthySlotOnboardingStarter(
	repository onboarding.HealthySlotStartRepository,
	config HealthySlotOnboardingStarterConfig,
) (*HealthySlotOnboardingStarter, error) {
	if repository == nil || config.ObservationMaxAge <= 0 || config.ObservationMaxAge > time.Hour ||
		config.CommandTTL <= 0 || config.CommandTTL > 24*time.Hour {
		return nil, ErrHealthySlotOnboardingStart
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &HealthySlotOnboardingStarter{
		repository: repository, now: config.Now,
		observationMaxAge: config.ObservationMaxAge, commandTTL: config.CommandTTL,
	}, nil
}

// Start derives every mutable identity from the intent id, then delegates the
// complete read/validate/create boundary to one repository transaction.
func (s *HealthySlotOnboardingStarter) Start(
	ctx context.Context,
	request HealthySlotOnboardingStartRequest,
) (onboarding.Provisioning, bool, error) {
	if s == nil || s.repository == nil || s.now == nil || ctx == nil || ctx.Err() != nil ||
		credential.ValidateTransportID(request.IntentID) != nil ||
		credential.ValidateTransportID(request.SlotID) != nil ||
		credential.ValidateTransportID(request.ReservationID) != nil || request.BindingRevision == 0 {
		return onboarding.Provisioning{}, false, ErrHealthySlotOnboardingStart
	}
	// MySQL DATETIME(6) is the durable clock precision. Canonicalizing before
	// IDs are bound avoids nanosecond-only replay differences after a readback.
	now := s.now().UTC().Truncate(time.Microsecond)
	if now.IsZero() {
		return onboarding.Provisioning{}, false, ErrHealthySlotOnboardingStart
	}
	workflowID := healthySlotStarterID("workflow", request.IntentID)
	spec := onboarding.HealthySlotStartSpec{
		IntentID: request.IntentID, SlotID: request.SlotID, ReservationID: request.ReservationID,
		BindingRevision: request.BindingRevision, WorkflowID: workflowID,
		IdempotencyKey: healthySlotStarterID("start", request.IntentID), Owner: workflowID,
		CredentialLeaseID:        healthySlotStarterID("credential-lease", request.IntentID),
		ProxyLeaseID:             healthySlotStarterID("proxy-lease", request.IntentID),
		KeyCommandID:             healthySlotStarterID("key-command", request.IntentID),
		ActivationCommandID:      healthySlotStarterID("activation-command", request.IntentID),
		StartedAt:                now,
		ObservationFreshAfter:    now.Add(-s.observationMaxAge).UTC().Truncate(time.Microsecond),
		RequestedCommandDeadline: now.Add(s.commandTTL).UTC().Truncate(time.Microsecond),
	}
	workflow, created, err := s.repository.StartHealthySlotOnboarding(ctx, spec)
	if err != nil {
		return onboarding.Provisioning{}, false, fmt.Errorf("%w: %w", ErrHealthySlotOnboardingStart, err)
	}
	return workflow, created, nil
}

func healthySlotStarterID(kind, intentID string) string {
	digest := sha256.Sum256([]byte("sub2api/healthy-slot-onboarding/v1/" + kind + "/" + intentID))
	return kind + "-" + hex.EncodeToString(digest[:])
}
