package lease

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

// A durable store that actually holds rows, so the divergence between the
// fencing token and the durable record can be driven from both sides.
// Mirrors the real SQL semantics closely enough to be worth trusting: rows are
// keyed by slot AND epoch, renewal and revocation match on owner, renewal
// refuses a revoked row, and re-revoking keeps the first timestamp rather than
// overwriting it.
type leaseKey struct {
	slotID string
	epoch  uint64
}

type statefulLeaseRepository struct {
	mu      sync.Mutex
	leases  map[leaseKey]store.ExecutionLease
	readErr error
}

func newStatefulLeaseRepository() *statefulLeaseRepository {
	return &statefulLeaseRepository{leases: make(map[leaseKey]store.ExecutionLease)}
}

func (r *statefulLeaseRepository) GrantExecutionLease(_ context.Context, candidate store.ExecutionLease) (store.ExecutionLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leases[leaseKey{candidate.SlotID, candidate.ExecutionEpoch}] = candidate
	return candidate, nil
}

func (r *statefulLeaseRepository) RenewExecutionLease(_ context.Context, slotID string, epoch uint64, ownerID string, expiresAt, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := leaseKey{slotID, epoch}
	current, exists := r.leases[key]
	if !exists || current.RevokedAt != nil || current.OwnerID != ownerID {
		return store.ErrExecutionLeaseNotFound
	}
	current.ExpiresAt = expiresAt
	r.leases[key] = current
	return nil
}

func (r *statefulLeaseRepository) RevokeExecutionLease(_ context.Context, slotID string, epoch uint64, ownerID string, revokedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := leaseKey{slotID, epoch}
	current, exists := r.leases[key]
	if !exists || current.OwnerID != ownerID {
		return store.ErrExecutionLeaseNotFound
	}
	if current.RevokedAt != nil {
		return nil
	}
	stamp := revokedAt
	current.RevokedAt = &stamp
	r.leases[key] = current
	return nil
}

func (r *statefulLeaseRepository) GetExecutionLease(_ context.Context, slotID string, epoch uint64) (store.ExecutionLease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.readErr != nil {
		return store.ExecutionLease{}, r.readErr
	}
	current, exists := r.leases[leaseKey{slotID, epoch}]
	if !exists {
		return store.ExecutionLease{}, store.ErrExecutionLeaseNotFound
	}
	return current, nil
}

func (r *statefulLeaseRepository) setReadError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readErr = err
}

func (r *statefulLeaseRepository) expiresAt(slotID string, epoch uint64) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leases[leaseKey{slotID, epoch}].ExpiresAt
}

func (r *statefulLeaseRepository) mutate(slotID string, epoch uint64, change func(*store.ExecutionLease)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := leaseKey{slotID, epoch}
	current := r.leases[key]
	change(&current)
	r.leases[key] = current
}

func validateFixture(t *testing.T) (*Coordinator, *MemoryBackend, *statefulLeaseRepository, Claim, time.Time) {
	t.Helper()
	now := time.Unix(2_000_000_000, 0).UTC()
	backend := NewMemoryBackend(func() time.Time { return now })
	durable := newStatefulLeaseRepository()
	coordinator, err := NewCoordinator(backend, durable, 45*time.Second, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claim := testClaim(1, "node-a")
	if err := coordinator.Grant(context.Background(), "lease-1", claim); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Validate(context.Background(), claim); err != nil {
		t.Fatalf("freshly granted lease did not validate: %v", err)
	}
	return coordinator, backend, durable, claim, now
}

// Either store alone can still authorise a lease the other has already ended,
// so validation is the conjunction of both.
func TestCoordinatorValidateIsTheConjunctionOfBothStores(t *testing.T) {
	t.Parallel()
	for name, divergence := range map[string]func(*MemoryBackend, *statefulLeaseRepository, Claim, time.Time){
		"durable revoked while the fencing token survives": func(_ *MemoryBackend, durable *statefulLeaseRepository, claim Claim, now time.Time) {
			if err := durable.RevokeExecutionLease(context.Background(), claim.SlotID, claim.ExecutionEpoch, claim.OwnerID, now); err != nil {
				panic(err)
			}
		},
		"durable row missing": func(_ *MemoryBackend, durable *statefulLeaseRepository, claim Claim, _ time.Time) {
			durable.mu.Lock()
			delete(durable.leases, leaseKey{claim.SlotID, claim.ExecutionEpoch})
			durable.mu.Unlock()
		},
		"durable owner drifted": func(_ *MemoryBackend, durable *statefulLeaseRepository, claim Claim, _ time.Time) {
			durable.mutate(claim.SlotID, claim.ExecutionEpoch, func(l *store.ExecutionLease) { l.OwnerID = "host-agent-other" })
		},
		"durable node drifted": func(_ *MemoryBackend, durable *statefulLeaseRepository, claim Claim, _ time.Time) {
			durable.mutate(claim.SlotID, claim.ExecutionEpoch, func(l *store.ExecutionLease) { l.NodeID = "node-other" })
		},
		"fencing token dropped while the durable row survives": func(backend *MemoryBackend, _ *statefulLeaseRepository, claim Claim, _ time.Time) {
			if err := backend.Revoke(context.Background(), claim); err != nil {
				panic(err)
			}
		},
	} {
		divergence := divergence
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			coordinator, backend, durable, claim, now := validateFixture(t)
			divergence(backend, durable, claim, now)
			if err := coordinator.Validate(context.Background(), claim); !errors.Is(err, ErrLeaseNotCurrent) {
				t.Fatalf("Validate() = %v, want ErrLeaseNotCurrent", err)
			}
		})
	}
}

// An unverifiable state is not evidence that the lease is still held.
func TestCoordinatorValidateFailsClosedWhenEitherStoreIsUnreachable(t *testing.T) {
	t.Parallel()
	t.Run("backend unavailable", func(t *testing.T) {
		t.Parallel()
		coordinator, backend, _, claim, _ := validateFixture(t)
		backend.SetAvailable(false)
		if err := coordinator.Validate(context.Background(), claim); !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("Validate() = %v, want ErrBackendUnavailable", err)
		}
	})
	t.Run("durable store unreadable", func(t *testing.T) {
		t.Parallel()
		coordinator, _, durable, claim, _ := validateFixture(t)
		durable.setReadError(errors.New("mysql unavailable"))
		if err := coordinator.Validate(context.Background(), claim); !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("Validate() = %v, want ErrBackendUnavailable", err)
		}
	})
}

// The fencing token TTL is the expiry authority. Re-deriving expiry from a
// database timestamp would turn clock skew into spurious revocation, so a
// durable row whose expires_at has passed must not by itself end the lease.
func TestCoordinatorValidateDoesNotReDeriveExpiryFromTheDurableRow(t *testing.T) {
	t.Parallel()
	coordinator, _, durable, claim, now := validateFixture(t)
	durable.mutate(claim.SlotID, claim.ExecutionEpoch, func(l *store.ExecutionLease) { l.ExpiresAt = now.Add(-time.Hour) })
	if err := coordinator.Validate(context.Background(), claim); err != nil {
		t.Fatalf("Validate() = %v, want the fencing token to remain the expiry authority", err)
	}
}

// Renewal must extend both stores. If the renewal loop could only refresh the
// fencing token, the durable expires_at would go stale while the token lived
// on, and ValidateCurrentProxyLease — which reads expires_at — would refuse a
// lease that this Validate still allows. That is why Renewer takes a Refresher
// the Coordinator can satisfy.
func TestRenewerDrivingTheCoordinatorKeepsBothStoresFresh(t *testing.T) {
	t.Parallel()
	coordinator, backend, durable, claim, now := validateFixture(t)
	granted := durable.expiresAt(claim.SlotID, claim.ExecutionEpoch)
	if granted.IsZero() {
		t.Fatal("grant did not record a durable expiry")
	}

	// The coordinator must be usable as the renewal target; a backend-only
	// interface here is what produced the split authority.
	var refresher Refresher = coordinator
	renewer, err := NewRenewer(refresher, claim, DefaultTiming(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if renewer == nil {
		t.Fatal("renewer was not constructed")
	}

	ttl := 90 * time.Second
	if err := coordinator.Renew(context.Background(), claim, ttl); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed := durable.expiresAt(claim.SlotID, claim.ExecutionEpoch); !renewed.Equal(now.Add(ttl)) {
		t.Fatalf("durable expiry = %s, want it extended to %s", renewed, now.Add(ttl))
	}
	if err := backend.Validate(context.Background(), claim); err != nil {
		t.Fatalf("fencing token was not extended: %v", err)
	}

	// A lease revoked durably cannot be renewed back to life, and the failed
	// durable step must drop the token too.
	if err := durable.RevokeExecutionLease(context.Background(), claim.SlotID, claim.ExecutionEpoch, claim.OwnerID, now); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Renew(context.Background(), claim, ttl); err == nil {
		t.Fatal("renewed a durably revoked lease")
	}
	if err := backend.Validate(context.Background(), claim); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("fencing token survived a failed durable renewal: %v", err)
	}
}

func TestCoordinatorValidateRejectsMalformedClaims(t *testing.T) {
	t.Parallel()
	coordinator, _, _, claim, _ := validateFixture(t)
	for name, broken := range map[string]Claim{
		"empty slot":  {NodeID: claim.NodeID, ExecutionEpoch: claim.ExecutionEpoch, OwnerID: claim.OwnerID},
		"zero epoch":  {SlotID: claim.SlotID, NodeID: claim.NodeID, OwnerID: claim.OwnerID},
		"empty owner": {SlotID: claim.SlotID, NodeID: claim.NodeID, ExecutionEpoch: claim.ExecutionEpoch},
	} {
		if err := coordinator.Validate(context.Background(), broken); err == nil {
			t.Fatalf("Validate(%s) accepted a malformed claim", name)
		}
	}
}

// Fencing must be able to run on the conjunction rather than the backend alone,
// otherwise a durable revocation that could not drop the token leaves protected
// connections relaying until the TTL runs out.
func TestFencerAcceptsTheCoordinatorAndClosesOnDurableRevocation(t *testing.T) {
	t.Parallel()
	coordinator, _, durable, claim, now := validateFixture(t)
	fencer, err := NewFencer(coordinator)
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Bool
	release, err := fencer.Admit(context.Background(), claim, func() { closed.Store(true) })
	if err != nil {
		t.Fatalf("admit under a current lease: %v", err)
	}
	defer release()
	if err := fencer.Revalidate(context.Background()); err != nil {
		t.Fatalf("revalidate under a current lease: %v", err)
	}
	if closed.Load() {
		t.Fatal("a current lease closed its protected connection")
	}

	// Revoke durably only; the fencing token is deliberately left in place to
	// stand in for a revocation whose backend step failed.
	if err := durable.RevokeExecutionLease(context.Background(), claim.SlotID, claim.ExecutionEpoch, claim.OwnerID, now); err != nil {
		t.Fatal(err)
	}
	if err := fencer.Revalidate(context.Background()); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("revalidate after durable revocation = %v, want ErrLeaseNotCurrent", err)
	}
	if !closed.Load() {
		t.Fatal("durable revocation left the protected connection open")
	}
}
