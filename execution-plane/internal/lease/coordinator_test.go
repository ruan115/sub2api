package lease

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestCoordinatorRevokesAuthoritativeBackendWhenDurableGrantFails(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	backend := NewMemoryBackend(func() time.Time { return now })
	durable := &stubLeaseRepository{grantErr: errors.New("mysql unavailable")}
	coordinator, err := NewCoordinator(backend, durable, 45*time.Second, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claim := testClaim(1, "node-a")
	if err := coordinator.Grant(context.Background(), "lease-1", claim); err == nil {
		t.Fatal("expected durable grant failure")
	}
	if err := backend.Validate(context.Background(), claim); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("backend remained active after durable failure: %v", err)
	}
}

func TestCoordinatorRenewalFailsClosedWhenDurableRenewFails(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	backend := NewMemoryBackend(func() time.Time { return now })
	durable := &stubLeaseRepository{renewErr: errors.New("mysql unavailable")}
	coordinator, err := NewCoordinator(backend, durable, 45*time.Second, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claim := testClaim(1, "node-a")
	if err := backend.Acquire(context.Background(), claim, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Renew(context.Background(), claim); err == nil {
		t.Fatal("expected durable renewal failure")
	}
	if err := backend.Validate(context.Background(), claim); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("backend remained active after durable renewal failure: %v", err)
	}
}

type stubLeaseRepository struct {
	grantErr  error
	renewErr  error
	revokeErr error
}

func (r *stubLeaseRepository) GrantExecutionLease(_ context.Context, candidate store.ExecutionLease) (store.ExecutionLease, error) {
	return candidate, r.grantErr
}

func (r *stubLeaseRepository) RenewExecutionLease(context.Context, string, uint64, string, time.Time, time.Time) error {
	return r.renewErr
}

func (r *stubLeaseRepository) RevokeExecutionLease(context.Context, string, uint64, string, time.Time) error {
	return r.revokeErr
}

func (r *stubLeaseRepository) GetExecutionLease(context.Context, string, uint64) (store.ExecutionLease, error) {
	return store.ExecutionLease{}, store.ErrExecutionLeaseNotFound
}
