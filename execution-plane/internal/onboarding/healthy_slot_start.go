package onboarding

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

var ErrHealthySlotStartRejected = errors.New("healthy-slot onboarding start rejected")

// HealthySlotStartSpec contains only caller-selected, secret-free identity and
// timing bounds. Account, generation, node, epoch and image are read from the
// locked intent/runtime projection by the repository.
type HealthySlotStartSpec struct {
	IntentID                 string
	SlotID                   string
	ReservationID            string
	BindingRevision          uint64
	WorkflowID               string
	IdempotencyKey           string
	Owner                    string
	CredentialLeaseID        string
	ProxyLeaseID             string
	KeyCommandID             string
	ActivationCommandID      string
	StartedAt                time.Time
	ObservationFreshAfter    time.Time
	RequestedCommandDeadline time.Time
}

func (s HealthySlotStartSpec) Validate() error {
	for _, value := range []string{
		s.IntentID, s.SlotID, s.ReservationID, s.WorkflowID, s.IdempotencyKey, s.Owner,
		s.CredentialLeaseID, s.ProxyLeaseID, s.KeyCommandID, s.ActivationCommandID,
	} {
		if credential.ValidateTransportID(value) != nil {
			return ErrHealthySlotStartRejected
		}
	}
	if s.BindingRevision == 0 || s.Owner != s.WorkflowID || s.KeyCommandID == s.ActivationCommandID ||
		s.StartedAt.IsZero() || s.ObservationFreshAfter.IsZero() || s.ObservationFreshAfter.After(s.StartedAt) ||
		!s.RequestedCommandDeadline.After(s.StartedAt) ||
		!s.StartedAt.Equal(s.StartedAt.UTC().Truncate(time.Microsecond)) ||
		!s.ObservationFreshAfter.Equal(s.ObservationFreshAfter.UTC().Truncate(time.Microsecond)) ||
		!s.RequestedCommandDeadline.Equal(s.RequestedCommandDeadline.UTC().Truncate(time.Microsecond)) {
		return ErrHealthySlotStartRejected
	}
	return nil
}

// HealthySlotStartRepository atomically creates the first durable workflow
// and its trusted proxy lease. It must not claim or decrypt the intent.
type HealthySlotStartRepository interface {
	StartHealthySlotOnboarding(ctx context.Context, spec HealthySlotStartSpec) (workflow Provisioning, created bool, err error)
}
