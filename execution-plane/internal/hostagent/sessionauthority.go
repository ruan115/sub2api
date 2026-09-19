package hostagent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

// SessionAuthority answers "may this node still hold this runtime?" from what
// the control plane has told it, and from nothing else.
//
// It exists because the node must not hold a database credential. The execution
// lease authority is the control plane's two-store coordinator — the Redis
// fencing token and the durable record together. A node cannot consult either,
// so it derives a weaker, strictly fail-closed signal from two control-plane
// facts it does observe:
//
//   - the authenticated control session is still fresh, and
//   - the control plane has not revoked this slot's epoch.
//
// This is deliberately weaker than the real lease. It cannot notice a lease
// that ended without the control plane reaching this node, which is exactly why
// staleness alone ends custody: a node that cannot hear the authority stops
// being entitled to act on what the authority last said. It is not a substitute
// for the authority and must never be presented as one.
type SessionAuthority struct {
	revoked      func(slotID string, epoch uint64) bool
	sessionState func() (open bool, closedAt time.Time)
	offlineAfter time.Duration
	now          func() time.Time
}

type SessionAuthorityConfig struct {
	// Revoked reports whether the control plane has revoked this slot at or
	// below the epoch. The executor keeps this watermark as revocations arrive.
	Revoked func(slotID string, epoch uint64) bool
	// SessionState reports whether an authenticated control session is open and
	// when the last one closed. ControlClient.ControlSessionState satisfies it.
	//
	// "Open" is only meaningful because the control dial configures HTTP/2
	// keepalive; without it a blackholed peer would look connected for as long
	// as TCP kept retransmitting.
	SessionState func() (open bool, closedAt time.Time)
	// OfflineAfter is how long a node may keep acting on the authority's last
	// word once it can no longer hear the authority.
	OfflineAfter time.Duration
	Now          func() time.Time
}

func NewSessionAuthority(config SessionAuthorityConfig) (*SessionAuthority, error) {
	if config.Revoked == nil || config.SessionState == nil ||
		config.OfflineAfter <= 0 || config.OfflineAfter > 5*time.Minute {
		return nil, errors.New("session authority configuration is invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &SessionAuthority{
		revoked: config.Revoked, sessionState: config.SessionState,
		offlineAfter: config.OfflineAfter, now: config.Now,
	}, nil
}

// Validate reports lease.ErrBackendUnavailable when the node cannot currently
// vouch for its authorization, and lease.ErrLeaseNotCurrent when the control
// plane has ended this epoch. Both end custody; the distinction is only so an
// operator can tell "we lost the control plane" from "we were revoked".
func (a *SessionAuthority) Validate(_ context.Context, claim lease.Claim) error {
	if a == nil {
		return errors.New("session authority is unavailable")
	}
	if err := claim.Validate(); err != nil {
		return err
	}
	open, closedAt := a.sessionState()
	if !open {
		if closedAt.IsZero() {
			return fmt.Errorf("%w: this node has never held a control session", lease.ErrBackendUnavailable)
		}
		if age := a.now().Sub(closedAt); age >= a.offlineAfter {
			return fmt.Errorf("%w: control session has been gone for %s", lease.ErrBackendUnavailable, age)
		}
	}
	if a.revoked(claim.SlotID, claim.ExecutionEpoch) {
		return lease.ErrLeaseNotCurrent
	}
	return nil
}

// EpochRevoked exposes the revocation watermark this executor maintains as
// RevokeEpoch commands arrive, so a session authority composed in another
// package can read it without reaching inside.
//
// This was removed once as unused public surface and is back because the daemon
// composition now needs it; it is not reachable from outside the module.
func (e *SlotCommandExecutor) EpochRevoked(slotID string, epoch uint64) bool {
	if e == nil {
		return false
	}
	return e.epochRevoked(slotID, epoch)
}

var _ lease.Validator = (*SessionAuthority)(nil)
