// Package runtimeregistry owns the authorized lifetime of connections to
// runtime instances that already exist. It adopts a connection the caller has
// already established and established correctly; it never creates, starts,
// dials or recreates anything, and it holds no credentials.
//
// Two rules follow from the rest of the execution plane:
//
//   - A connection is bound to one physical container. Adoption records the
//     RuntimeID and refuses to let a different container inherit a slot's
//     generation, because peer verification is exact URI match and a reused
//     binding cannot be told apart at the TLS layer.
//   - Losing the execution lease must reclaim the connection, not merely refuse
//     the next request. Every adopted connection is admitted to a lease.Fencer,
//     so a lease that stops validating closes it on the next revalidation, and
//     an explicit revocation closes it immediately.
package runtimeregistry

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

var (
	ErrRegistryEntry = errors.New("runtime registry entry is invalid")
	ErrSuperseded    = errors.New("runtime registry holds a newer generation for this slot")
	ErrAlreadyHeld   = errors.New("runtime registry already holds this slot generation")
	ErrIdentityDrift = errors.New("runtime registry entry names a different container")
	ErrRevoked       = errors.New("runtime registry has revoked this slot generation")
	// ErrNotHeld means the registry no longer holds that generation. It is the
	// normal outcome of releasing something the fencer or a supersede already
	// reclaimed, so a caller's cleanup path should treat it as benign rather
	// than as a failure.
	ErrNotHeld = errors.New("runtime registry does not hold this slot generation")
)

// Connection is the only thing the registry may do to a runtime: let it go.
// There is deliberately no create, dial or restart verb here — a registry that
// could reconnect would be able to resurrect a revoked generation.
type Connection interface {
	Close() error
}

// Entry is one adopted runtime. Claim carries slot, node, epoch and owner;
// Generation is the runtime generation that the connection's mTLS identity was
// issued for; RuntimeID is the physical container ID it was established to.
type Entry struct {
	Claim      lease.Claim
	Generation uint64
	RuntimeID  string
	Connection Connection
}

func (e Entry) validate() error {
	if err := e.Claim.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrRegistryEntry, err)
	}
	if e.Generation == 0 || e.RuntimeID == "" || len(e.RuntimeID) > 128 || e.Connection == nil {
		return ErrRegistryEntry
	}
	return nil
}

// Held describes what the registry currently holds for a slot, without exposing
// the connection itself.
type Held struct {
	Claim      lease.Claim
	Generation uint64
	RuntimeID  string
}

type registered struct {
	entry   Entry
	release func()
}

type Registry struct {
	fencer *lease.Fencer

	mu     sync.Mutex
	bySlot map[string]registered
	// revokedThrough is what makes Revoke a bar and not just a close. Without
	// it an adoption admitted just before a revocation could store itself
	// afterwards and re-arm the epoch that was just ended.
	revokedThrough map[string]uint64
	drained        bool
}

func New(fencer *lease.Fencer) (*Registry, error) {
	if fencer == nil {
		return nil, errors.New("runtime registry requires an execution lease fencer")
	}
	return &Registry{
		fencer: fencer, bySlot: make(map[string]registered),
		revokedThrough: make(map[string]uint64),
	}, nil
}

// Adopt takes ownership of an already-established connection. After it returns
// nil the registry closes the connection, whether through Release, through a
// superseding generation, through Revoke, or through the fencer when the lease
// stops validating.
//
// A rejected adoption never disturbs what the registry already holds, and never
// closes the connection it refused: that belongs to the caller, which still has
// to decide what to do with it.
func (r *Registry) Adopt(ctx context.Context, entry Entry) error {
	if err := entry.validate(); err != nil {
		return err
	}
	// Admitted before the registry lock is taken so that the fencer's close
	// callback, which reaches back into the registry, can never wait on a lock
	// this call is holding.
	release, err := r.fencer.Admit(ctx, entry.Claim, func() { r.closeSlot(entry.Claim.SlotID, entry.Claim.ExecutionEpoch, entry.Generation) })
	if err != nil {
		return fmt.Errorf("admit runtime under its execution lease: %w", err)
	}
	superseded, err := r.store(entry, release)
	if err != nil {
		release()
		return err
	}
	if superseded != nil {
		superseded.release()
		_ = superseded.entry.Connection.Close()
	}
	return nil
}

func (r *Registry) store(entry Entry, release func()) (*registered, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.drained || entry.Claim.ExecutionEpoch <= r.revokedThrough[entry.Claim.SlotID] {
		return nil, ErrRevoked
	}
	current, exists := r.bySlot[entry.Claim.SlotID]
	if !exists {
		r.bySlot[entry.Claim.SlotID] = registered{entry: entry, release: release}
		return nil, nil
	}
	// A generation names one container for the life of the slot, whatever the
	// epoch. Letting a different container arrive under a generation this slot
	// has already used is the reuse that URI-only peer verification cannot see.
	if entry.Generation == current.entry.Generation && entry.RuntimeID != current.entry.RuntimeID {
		return nil, ErrIdentityDrift
	}
	switch {
	case entry.Claim.ExecutionEpoch < current.entry.Claim.ExecutionEpoch,
		entry.Claim.ExecutionEpoch == current.entry.Claim.ExecutionEpoch && entry.Generation < current.entry.Generation:
		return nil, ErrSuperseded
	case entry.Claim.ExecutionEpoch == current.entry.Claim.ExecutionEpoch && entry.Generation == current.entry.Generation:
		// The caller must release before adopting again; silently replacing the
		// handle would leak the one already held.
		return nil, ErrAlreadyHeld
	}
	r.bySlot[entry.Claim.SlotID] = registered{entry: entry, release: release}
	previous := current
	return &previous, nil
}

// Release gives up one exact generation and closes its connection. It is the
// normal way to hand a runtime back. It reports ErrNotHeld when the fencer or a
// supersede already reclaimed the entry, which is benign: a deferred Release on
// a revocation path should not be treated as a failure.
func (r *Registry) Release(slotID string, epoch, generation uint64) error {
	held := r.take(slotID, func(current registered) bool {
		return current.entry.Claim.ExecutionEpoch == epoch && current.entry.Generation == generation
	}, 0)
	if held == nil {
		return ErrNotHeld
	}
	held.release()
	return held.entry.Connection.Close()
}

// Revoke ends every generation of the slot at or below the given epoch: it
// closes what is held now and bars anything at or below that epoch from being
// adopted later. A newer generation that has already been adopted survives,
// because it is not the one being revoked.
func (r *Registry) Revoke(slotID string, epoch uint64) {
	held := r.take(slotID, func(current registered) bool {
		return current.entry.Claim.ExecutionEpoch <= epoch
	}, epoch)
	if held == nil {
		return
	}
	held.release()
	_ = held.entry.Connection.Close()
}

// closeSlot is the fencer's callback. It closes only the exact generation that
// was admitted, so a lease that expired for an old generation cannot take down
// the one that replaced it.
func (r *Registry) closeSlot(slotID string, epoch, generation uint64) {
	held := r.take(slotID, func(current registered) bool {
		return current.entry.Claim.ExecutionEpoch == epoch && current.entry.Generation == generation
	}, 0)
	if held == nil {
		return
	}
	held.release()
	_ = held.entry.Connection.Close()
}

// take removes and returns the slot's entry when it matches, so that closing
// always happens outside the registry lock. A non-zero revokeThrough raises the
// slot's revocation watermark in the same critical section, so an adoption
// racing a revocation cannot slip in between the two.
func (r *Registry) take(slotID string, matches func(registered) bool, revokeThrough uint64) *registered {
	r.mu.Lock()
	defer r.mu.Unlock()
	if revokeThrough > r.revokedThrough[slotID] {
		r.revokedThrough[slotID] = revokeThrough
	}
	current, exists := r.bySlot[slotID]
	if !exists || !matches(current) {
		return nil
	}
	delete(r.bySlot, slotID)
	return &current
}

// Drain releases and closes everything the registry holds, and reports how many
// runtimes it let go. It is for shutdown: leaving connections open when the
// service stops would strand authenticated transports the fencer can no longer
// revalidate.
//
// It also bars every later adoption. An Adopt that cleared the fencer just
// before Drain took the lock would otherwise store itself into the drained
// registry, and with the revalidation loop already stopped nothing would ever
// close it — the exact stranding Drain exists to prevent.
func (r *Registry) Drain() int {
	r.mu.Lock()
	r.drained = true
	held := make([]registered, 0, len(r.bySlot))
	for _, current := range r.bySlot {
		held = append(held, current)
	}
	r.bySlot = make(map[string]registered)
	r.mu.Unlock()
	for _, current := range held {
		current.release()
		_ = current.entry.Connection.Close()
	}
	return len(held)
}

// Current reports what the slot holds. It never returns the connection.
func (r *Registry) Current(slotID string) (Held, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.bySlot[slotID]
	if !exists {
		return Held{}, false
	}
	return Held{Claim: current.entry.Claim, Generation: current.entry.Generation, RuntimeID: current.entry.RuntimeID}, true
}

// Len reports how many slots are held.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bySlot)
}
