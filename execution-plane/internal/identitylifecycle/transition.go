// Package identitylifecycle states the transition contract for a runtime
// instance's identity across restart, upgrade, account change, adoption and
// destruction. It is a pure rule set: it stores nothing, reads nothing and is
// not a second state authority. The caller supplies the previous binding and
// the floor from whichever authority already owns them.
//
// The rules follow from three facts about the current implementation:
//
//   - Instance identity lives in /run/execution/identity, which the provider
//     mounts as tmpfs. Stopping the container destroys the private key, the
//     machine ID and the installed certificate. A restarted container is a new
//     instance holding no prior material.
//   - Peer verification is exact URI match plus NotBefore/NotAfter and EKU.
//     There is no CRL and no freshness field beyond the certificate validity
//     window, and the URI is built solely from the binding. A reused binding
//     tuple therefore lets an earlier, still-unexpired certificate verify as a
//     later instance.
//   - The certificate receipt store is unique on (slot_id, execution_epoch) —
//     in the SQL migration and in the memory implementation alike. Generation
//     is not part of that key, so a re-enrollment that advances only the
//     generation cannot obtain a certificate at all.
//
// Hence: every event that loses the instance key must advance the epoch *and*
// the generation, and generation is a per-slot counter that never resets, not
// even when the slot changes account.
package identitylifecycle

import (
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

var ErrTransition = errors.New("runtime identity transition rejected")

// Event names why a slot's identity is moving. It is supplied by the caller
// that performed the underlying action; it is never inferred from the
// bindings, because inferring it would let a silent account swap present
// itself as an ordinary restart.
type Event string

const (
	// EventAdopt re-attaches to the same physical container that is still
	// running and still holds its tmpfs identity. This is the only event that
	// preserves the instance key, and therefore the only one that keeps the
	// binding identical.
	EventAdopt Event = "adopt"

	// EventRestart covers stop/start and crash recovery by recreating the
	// container. The tmpfs identity is gone, so a new key must be enrolled.
	EventRestart Event = "restart"

	// EventUpgrade replaces the image digest. Mechanically the same identity
	// consequence as a restart; kept distinct so operators can tell them apart.
	EventUpgrade Event = "upgrade"

	// EventAccountChange reassigns the slot to a different account.
	EventAccountChange Event = "account-change"

	// EventDestroy is terminal for the binding. Nothing may follow it, and a
	// later instance on the same slot must start above the destroyed floor.
	EventDestroy Event = "destroy"
)

func (e Event) valid() bool {
	switch e {
	case EventAdopt, EventRestart, EventUpgrade, EventAccountChange, EventDestroy:
		return true
	}
	return false
}

// PreservesInstanceKey reports whether the instance's private key survives the
// event. Only adoption of a still-running container does.
func PreservesInstanceKey(event Event) bool { return event == EventAdopt }

// RequiresEnrollment reports whether the event must be followed by a fresh CSR
// and certificate issuance before the instance can serve mTLS.
func RequiresEnrollment(event Event) bool {
	switch event {
	case EventRestart, EventUpgrade, EventAccountChange:
		return true
	}
	return false
}

// Transition is one step of a slot's identity history. Previous is the binding
// that was last authoritative for the slot; Next is the binding being moved to,
// and is the zero value for EventDestroy.
type Transition struct {
	Event    Event
	Previous runtimeidentity.Binding
	Next     runtimeidentity.Binding
}

func (t Transition) Validate() error {
	if !t.Event.valid() {
		return fmt.Errorf("%w: unknown event %q", ErrTransition, t.Event)
	}
	if err := t.Previous.Validate(); err != nil {
		return fmt.Errorf("%w: previous binding invalid", ErrTransition)
	}
	if t.Event == EventDestroy {
		if t.Next != (runtimeidentity.Binding{}) {
			return fmt.Errorf("%w: destroy must not name a successor binding", ErrTransition)
		}
		return nil
	}
	if err := t.Next.Validate(); err != nil {
		return fmt.Errorf("%w: next binding invalid", ErrTransition)
	}
	// A transition is always of one slot on one node. Moving elsewhere is a
	// placement decision with its own binding history, and lease.Claim is keyed
	// on slot and node as well as epoch.
	if t.Next.SlotID != t.Previous.SlotID {
		return fmt.Errorf("%w: slot changed from %q to %q", ErrTransition, t.Previous.SlotID, t.Next.SlotID)
	}
	if t.Next.NodeID != t.Previous.NodeID {
		return fmt.Errorf("%w: node changed from %q to %q", ErrTransition, t.Previous.NodeID, t.Next.NodeID)
	}
	if t.Event == EventAdopt {
		// Same live container, same material, therefore the same binding down
		// to the generation. Anything else is a new instance wearing an old
		// name, which is exactly what the URI-only verifier cannot detect.
		if t.Next != t.Previous {
			return fmt.Errorf("%w: adoption must keep the binding identical", ErrTransition)
		}
		return nil
	}
	if err := t.validateAccount(); err != nil {
		return err
	}
	// The instance key did not survive, so a certificate must be re-issued. The
	// receipt store is unique on (slot, epoch), so the epoch has to move; and
	// the tuple must not become reusable by the certificate issued to the
	// instance that just went away, so the generation has to move too.
	if t.Next.Epoch <= t.Previous.Epoch {
		return fmt.Errorf("%w: %s must advance the epoch past %d, got %d", ErrTransition, t.Event, t.Previous.Epoch, t.Next.Epoch)
	}
	if t.Next.Generation <= t.Previous.Generation {
		return fmt.Errorf("%w: %s must advance the generation past %d, got %d", ErrTransition, t.Event, t.Previous.Generation, t.Next.Generation)
	}
	return nil
}

func (t Transition) validateAccount() error {
	changed := t.Next.AccountHash != t.Previous.AccountHash
	if t.Event == EventAccountChange {
		if !changed {
			return fmt.Errorf("%w: account-change kept account %q", ErrTransition, t.Previous.AccountHash)
		}
		return nil
	}
	if changed {
		return fmt.Errorf("%w: %s must not change account", ErrTransition, t.Event)
	}
	return nil
}

// Floor is the highest epoch and generation ever issued for a slot, including
// bindings whose instances have been destroyed. It must come from the existing
// authority; this package does not remember anything between calls.
//
// Without it, a slot could be destroyed and reborn at generation 1 while the
// destroyed instance's certificate is still inside its validity window — the
// precise reuse this contract exists to prevent.
type Floor struct {
	Epoch      uint64
	Generation uint64
}

// ValidateHistory walks a slot's transitions in order. The first step must
// stand at or above the floor, each step must continue the previous one, and
// destruction ends the history.
func ValidateHistory(floor Floor, transitions []Transition) error {
	if len(transitions) == 0 {
		return fmt.Errorf("%w: empty history", ErrTransition)
	}
	start := transitions[0].Previous
	if start.Epoch < floor.Epoch || start.Generation < floor.Generation {
		return fmt.Errorf("%w: history starts at epoch %d generation %d, below the floor of %d/%d",
			ErrTransition, start.Epoch, start.Generation, floor.Epoch, floor.Generation)
	}
	destroyedAt := -1
	for index, transition := range transitions {
		if destroyedAt >= 0 {
			return fmt.Errorf("%w: step %d follows the destroy at step %d", ErrTransition, index, destroyedAt)
		}
		if err := transition.Validate(); err != nil {
			return fmt.Errorf("step %d: %w", index, err)
		}
		if index > 0 {
			if previous := transitions[index-1]; transition.Previous != previous.Next {
				return fmt.Errorf("%w: step %d does not continue step %d", ErrTransition, index, index-1)
			}
		}
		if transition.Event == EventDestroy {
			destroyedAt = index
		}
	}
	return nil
}
