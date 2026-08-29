package lease

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMemoryBackendAllowsExactlyOneCurrentEpoch(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	backend := NewMemoryBackend(func() time.Time { return now })
	first := testClaim(1, "node-a")
	second := testClaim(2, "node-b")
	const contenders = 32
	var acquired atomic.Int32
	var wait sync.WaitGroup
	wait.Add(contenders)
	for index := range contenders {
		index := index
		go func() {
			defer wait.Done()
			claim := first
			claim.OwnerID = "owner-" + string(rune('a'+index))
			if backend.Acquire(context.Background(), claim, 45*time.Second) == nil {
				acquired.Add(1)
			}
		}()
	}
	wait.Wait()
	if acquired.Load() != 1 {
		t.Fatalf("successful concurrent lease owners = %d", acquired.Load())
	}
	if err := backend.Acquire(context.Background(), second, 45*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("higher epoch before expiry error = %v", err)
	}
	now = now.Add(45 * time.Second)
	if err := backend.Acquire(context.Background(), second, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := backend.Validate(context.Background(), first); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("old epoch validation error = %v", err)
	}
	if err := backend.Validate(context.Background(), second); err != nil {
		t.Fatal(err)
	}
}

func TestFencerFailsClosedAndClosesExistingEgressOnBackendFailure(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	backend := NewMemoryBackend(func() time.Time { return now })
	claim := testClaim(1, "node-a")
	if err := backend.Acquire(context.Background(), claim, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	fencer, err := NewFencer(backend)
	if err != nil {
		t.Fatal(err)
	}
	var closed atomic.Int32
	release, err := fencer.Admit(context.Background(), claim, func() { closed.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	backend.SetAvailable(false)
	if _, err := fencer.Admit(context.Background(), claim, func() {}); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("new egress during backend failure error = %v", err)
	}
	if err := fencer.Revalidate(context.Background()); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("revalidate backend failure error = %v", err)
	}
	if closed.Load() != 1 {
		t.Fatalf("closed protected connections = %d", closed.Load())
	}
	release()
	if closed.Load() != 1 {
		t.Fatalf("release double-closed protected connection: %d", closed.Load())
	}
}

func TestFencerClosesOldEpochAfterFailover(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	backend := NewMemoryBackend(func() time.Time { return now })
	oldClaim := testClaim(1, "node-a")
	newClaim := testClaim(2, "node-b")
	if err := backend.Acquire(context.Background(), oldClaim, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	fencer, _ := NewFencer(backend)
	var closed atomic.Bool
	if _, err := fencer.Admit(context.Background(), oldClaim, func() { closed.Store(true) }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(45 * time.Second)
	if err := backend.Acquire(context.Background(), newClaim, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := fencer.Revalidate(context.Background()); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("old epoch revalidation error = %v", err)
	}
	if !closed.Load() {
		t.Fatal("old epoch connection remained open")
	}
}

func TestAvailabilityBoundariesAre15_45_90ByDefault(t *testing.T) {
	timing := DefaultTiming()
	if err := timing.Validate(); err != nil {
		t.Fatal(err)
	}
	last := time.Unix(2_000_000_000, 0).UTC()
	tests := []struct {
		age         time.Duration
		canRoute    bool
		canFailover bool
	}{
		{age: 44 * time.Second, canRoute: true, canFailover: false},
		{age: 45 * time.Second, canRoute: false, canFailover: false},
		{age: 89 * time.Second, canRoute: false, canFailover: false},
		{age: 90 * time.Second, canRoute: false, canFailover: true},
	}
	for _, test := range tests {
		availability, err := EvaluateAvailability(last, last.Add(test.age), timing)
		if err != nil {
			t.Fatal(err)
		}
		if availability.CanRouteNew != test.canRoute || availability.CanFailover != test.canFailover {
			t.Fatalf("age %s availability = %+v", test.age, availability)
		}
	}
}

func TestRenewerStopsAndSignalsOnFirstFailedRenewal(t *testing.T) {
	backend := NewMemoryBackend(time.Now)
	claim := testClaim(1, "node-a")
	timing := Timing{RenewEvery: 10 * time.Millisecond, OfflineAfter: 30 * time.Millisecond, FailoverAfter: 40 * time.Millisecond}
	if err := backend.Acquire(context.Background(), claim, timing.OfflineAfter); err != nil {
		t.Fatal(err)
	}
	backend.SetAvailable(false)
	lost := make(chan error, 1)
	renewer, err := NewRenewer(backend, claim, timing, func(err error) { lost <- err })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = renewer.Run(ctx)
	if !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("renewer error = %v", err)
	}
	select {
	case err := <-lost:
		if !errors.Is(err, ErrBackendUnavailable) {
			t.Fatalf("loss callback error = %v", err)
		}
	default:
		t.Fatal("lease loss callback was not invoked")
	}
}

func testClaim(epoch uint64, nodeID string) Claim {
	return Claim{SlotID: "slot-1", NodeID: nodeID, ExecutionEpoch: epoch, OwnerID: "host-agent"}
}
