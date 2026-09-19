package runtimeenrollment

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func issuanceClaim() lease.Claim {
	return lease.Claim{SlotID: "slot-1", NodeID: "node-1", ExecutionEpoch: 4, OwnerID: "owner-1"}
}

// durableLeases is a minimal ExecutionLeaseRepository. The real repository
// requires a live node session and an active assignment before it will grant,
// which is out of scope here: this test is about whether issuance consults the
// durable record at all, not about how that record is written.
type durableLeases struct {
	lease   store.ExecutionLease
	present bool
}

func (d *durableLeases) GrantExecutionLease(_ context.Context, candidate store.ExecutionLease) (store.ExecutionLease, error) {
	d.lease, d.present = candidate, true
	return candidate, nil
}

func (d *durableLeases) RenewExecutionLease(context.Context, string, uint64, string, time.Time, time.Time) error {
	return nil
}

func (d *durableLeases) RevokeExecutionLease(_ context.Context, _ string, _ uint64, _ string, revokedAt time.Time) error {
	if !d.present {
		return store.ErrExecutionLeaseNotFound
	}
	stamp := revokedAt
	d.lease.RevokedAt = &stamp
	return nil
}

func (d *durableLeases) GetExecutionLease(_ context.Context, _ string, _ uint64) (store.ExecutionLease, error) {
	if !d.present {
		return store.ExecutionLease{}, store.ErrExecutionLeaseNotFound
	}
	return d.lease, nil
}

// authorityFixture grants a lease in both stores, which is the state issuance
// is supposed to accept.
func authorityFixture(t *testing.T) (leaseValidator, *lease.MemoryBackend, *durableLeases, *atomic.Bool, time.Time) {
	t.Helper()
	now := time.Unix(2_000_000_000, 0).UTC()
	backend := lease.NewMemoryBackend(func() time.Time { return now })
	repository := &durableLeases{}
	coordinator, err := lease.NewCoordinator(backend, repository, time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claim := issuanceClaim()
	if err := backend.Acquire(context.Background(), claim, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.GrantExecutionLease(context.Background(), store.ExecutionLease{
		ID: "lease-1", SlotID: claim.SlotID, NodeID: claim.NodeID, ExecutionEpoch: claim.ExecutionEpoch,
		OwnerID: claim.OwnerID, ExpiresAt: now.Add(time.Minute), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	closed := &atomic.Bool{}
	return leaseValidator{authority: coordinator, closed: closed}, backend, repository, closed, now
}

func TestIssuanceAcceptsALeaseBothStoresAgreeOn(t *testing.T) {
	t.Parallel()
	validator, _, _, _, _ := authorityFixture(t)
	if err := validator.Validate(context.Background(), issuanceClaim()); err != nil {
		t.Fatalf("issuance refused a current lease: %v", err)
	}
}

// This is the hole the coordinator closes. Revoke writes the durable record
// first and then drops the fencing token; when the token drop fails, the
// backend keeps calling the lease current until its TTL runs out. Issuing a
// certificate on that basis would hand out an identity for a lease that was
// already revoked.
func TestIssuanceRefusesADurablyRevokedLeaseWhoseTokenSurvives(t *testing.T) {
	t.Parallel()
	validator, backend, repository, _, now := authorityFixture(t)
	claim := issuanceClaim()
	if err := repository.RevokeExecutionLease(context.Background(),
		claim.SlotID, claim.ExecutionEpoch, claim.OwnerID, now); err != nil {
		t.Fatal(err)
	}
	// The token is deliberately left alive: a backend-only gate would still
	// call this lease current.
	if err := backend.Validate(context.Background(), claim); err != nil {
		t.Fatalf("the fencing token was supposed to survive: %v", err)
	}
	if err := validator.Validate(context.Background(), claim); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("issuance accepted a durably revoked lease: %v", err)
	}
}

// The other direction was already covered by the backend, and must stay covered.
func TestIssuanceRefusesALeaseWhoseTokenIsGone(t *testing.T) {
	t.Parallel()
	validator, backend, _, _, _ := authorityFixture(t)
	claim := issuanceClaim()
	if err := backend.Revoke(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate(context.Background(), claim); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("issuance accepted a lease with no fencing token: %v", err)
	}
}

// An unreadable store is not evidence that a lease is still held, and issuance
// must report it as unavailability rather than as a refusal of the claim.
func TestIssuanceFailsClosedWhenTheAuthorityIsUnreachable(t *testing.T) {
	t.Parallel()
	validator, backend, _, closed, _ := authorityFixture(t)
	backend.SetAvailable(false)
	if err := validator.Validate(context.Background(), issuanceClaim()); !errors.Is(err, lease.ErrBackendUnavailable) {
		t.Fatalf("Validate = %v, want ErrBackendUnavailable", err)
	}
	backend.SetAvailable(true)

	// Shutdown also stops issuance, whatever the stores would have said.
	closed.Store(true)
	if err := validator.Validate(context.Background(), issuanceClaim()); !errors.Is(err, lease.ErrBackendUnavailable) {
		t.Fatalf("a closed dependency set still validated: %v", err)
	}
}

func TestIssuanceRejectsAnUnusableContext(t *testing.T) {
	t.Parallel()
	validator, _, _, _, _ := authorityFixture(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := validator.Validate(cancelled, issuanceClaim()); !errors.Is(err, lease.ErrBackendUnavailable) {
		t.Fatalf("Validate on a cancelled context = %v", err)
	}
	//nolint:staticcheck // a nil context is exactly what this guard is for
	if err := validator.Validate(nil, issuanceClaim()); !errors.Is(err, lease.ErrBackendUnavailable) {
		t.Fatalf("Validate on a nil context = %v", err)
	}
}
