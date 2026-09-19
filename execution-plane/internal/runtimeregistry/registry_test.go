package runtimeregistry

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

type fakeConnection struct {
	closes atomic.Int32
	err    error
}

func (c *fakeConnection) Close() error {
	c.closes.Add(1)
	return c.err
}

func (c *fakeConnection) closed() bool { return c.closes.Load() > 0 }

func claimFor(epoch uint64) lease.Claim {
	return lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: epoch, OwnerID: "host-agent-1"}
}

func entryFor(epoch, generation uint64, runtimeID string, connection Connection) Entry {
	return Entry{Claim: claimFor(epoch), Generation: generation, RuntimeID: runtimeID, Connection: connection}
}

// A slot holds at most one execution lease at a time: MemoryBackend keys by
// slot and refuses a different claim, which mirrors the real invariant. Tests
// that move a slot to a new epoch therefore have to end the old lease first.
func fixture(t *testing.T, epochs ...uint64) (*Registry, *lease.MemoryBackend, *lease.Fencer) {
	t.Helper()
	now := time.Unix(2_000_000_000, 0).UTC()
	backend := lease.NewMemoryBackend(func() time.Time { return now })
	for _, epoch := range epochs {
		if err := backend.Acquire(context.Background(), claimFor(epoch), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	fencer, err := lease.NewFencer(backend)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := New(fencer)
	if err != nil {
		t.Fatal(err)
	}
	return registry, backend, fencer
}

// rollLease ends the slot's current lease and grants the next epoch, which is
// what has to happen before a newer generation can be adopted.
func rollLease(t *testing.T, backend *lease.MemoryBackend, from, to uint64) {
	t.Helper()
	if err := backend.Revoke(context.Background(), claimFor(from)); err != nil {
		t.Fatal(err)
	}
	if err := backend.Acquire(context.Background(), claimFor(to), time.Minute); err != nil {
		t.Fatal(err)
	}
}

// Neither the connection nor the registry may resurrect a runtime. Pinning both
// surfaces means a create, dial or restart verb cannot be added without this
// failing and forcing the question to be argued.
func TestNeitherConnectionNorRegistryCanResurrectARuntime(t *testing.T) {
	t.Parallel()
	connection := reflect.TypeOf((*Connection)(nil)).Elem()
	if connection.NumMethod() != 1 || connection.Method(0).Name != "Close" {
		var names []string
		for index := 0; index < connection.NumMethod(); index++ {
			names = append(names, connection.Method(index).Name)
		}
		t.Fatalf("Connection exposes %v, want only Close", names)
	}
	want := map[string]bool{"Adopt": true, "Release": true, "Revoke": true, "Current": true, "Len": true}
	registry := reflect.TypeOf(&Registry{})
	got := make(map[string]bool, registry.NumMethod())
	for index := 0; index < registry.NumMethod(); index++ {
		got[registry.Method(index).Name] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Registry exposes %v, want exactly %v", got, want)
	}
}

func TestAdoptRejectsIncompleteEntries(t *testing.T) {
	t.Parallel()
	registry, _, _ := fixture(t, 1)
	valid := entryFor(1, 1, "cid-1", &fakeConnection{})
	for name, broken := range map[string]Entry{
		"no connection": {Claim: valid.Claim, Generation: 1, RuntimeID: "cid-1"},
		"no runtime id": {Claim: valid.Claim, Generation: 1, Connection: &fakeConnection{}},
		"no generation": {Claim: valid.Claim, RuntimeID: "cid-1", Connection: &fakeConnection{}},
		"no epoch": {Claim: lease.Claim{SlotID: "slot-1", NodeID: "srv74", OwnerID: "host-agent-1"},
			Generation: 1, RuntimeID: "cid-1", Connection: &fakeConnection{}},
		"no slot": {Claim: lease.Claim{NodeID: "srv74", ExecutionEpoch: 1, OwnerID: "host-agent-1"},
			Generation: 1, RuntimeID: "cid-1", Connection: &fakeConnection{}},
	} {
		if err := registry.Adopt(context.Background(), broken); !errors.Is(err, ErrRegistryEntry) {
			t.Fatalf("Adopt(%s) = %v, want ErrRegistryEntry", name, err)
		}
	}
	if registry.Len() != 0 {
		t.Fatal("a rejected adoption was stored")
	}
}

// Without a current lease there is nothing to hold the runtime on behalf of.
func TestAdoptRequiresACurrentLease(t *testing.T) {
	t.Parallel()
	registry, _, _ := fixture(t)
	connection := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(1, 1, "cid-1", connection)); err == nil {
		t.Fatal("adopted a runtime with no execution lease")
	}
	if registry.Len() != 0 {
		t.Fatal("a refused adoption was stored")
	}
	// The refused connection still belongs to the caller.
	if connection.closed() {
		t.Fatal("a refused adoption closed a connection it does not own")
	}
}

func TestAdoptIsOwnershipTransferAndReleaseGivesItBack(t *testing.T) {
	t.Parallel()
	registry, _, _ := fixture(t, 1)
	connection := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(1, 4, "cid-1", connection)); err != nil {
		t.Fatal(err)
	}
	held, exists := registry.Current("slot-1")
	if !exists || held.Generation != 4 || held.RuntimeID != "cid-1" || held.Claim != claimFor(1) {
		t.Fatalf("Current() = %+v, %v", held, exists)
	}
	if connection.closed() {
		t.Fatal("adoption closed the connection")
	}
	if err := registry.Release("slot-1", 1, 4); err != nil {
		t.Fatal(err)
	}
	if !connection.closed() {
		t.Fatal("release did not close the connection")
	}
	if _, exists := registry.Current("slot-1"); exists {
		t.Fatal("released slot is still held")
	}
	// Releasing something already reclaimed reports ErrNotHeld, which a
	// deferred cleanup can treat as benign rather than as a failure.
	if err := registry.Release("slot-1", 1, 4); !errors.Is(err, ErrNotHeld) {
		t.Fatalf("second Release = %v, want ErrNotHeld", err)
	}
}

// Every Admit must be released, including on the paths that refuse the entry.
// A leaked admission would keep validating a slot the registry does not hold.
func TestRejectedAdoptionLeavesNoFencerAdmission(t *testing.T) {
	t.Parallel()
	registry, _, fencer := fixture(t, 2)
	if err := registry.Adopt(context.Background(), entryFor(2, 5, "cid-2", &fakeConnection{})); err != nil {
		t.Fatal(err)
	}
	if fencer.Len() != 1 {
		t.Fatalf("admissions after one adoption = %d, want 1", fencer.Len())
	}
	for _, rejected := range []Entry{
		entryFor(2, 4, "cid-2", &fakeConnection{}),     // superseded
		entryFor(2, 5, "cid-2", &fakeConnection{}),     // already held
		entryFor(2, 5, "cid-other", &fakeConnection{}), // identity drift
	} {
		if err := registry.Adopt(context.Background(), rejected); err == nil {
			t.Fatalf("Adopt(%+v) was accepted", rejected)
		}
		if fencer.Len() != 1 {
			t.Fatalf("rejected adoption leaked an admission: %d", fencer.Len())
		}
	}
}

// A generation names one container for the life of the slot, whatever the
// epoch, so a different container may not inherit it even while moving forward.
func TestAdoptRefusesADifferentContainerUnderAUsedGeneration(t *testing.T) {
	t.Parallel()
	registry, backend, _ := fixture(t, 1)
	if err := registry.Adopt(context.Background(), entryFor(1, 7, "cid-1", &fakeConnection{})); err != nil {
		t.Fatal(err)
	}
	rollLease(t, backend, 1, 2)
	// Newer epoch, but reusing generation 7 under a different container.
	if err := registry.Adopt(context.Background(), entryFor(2, 7, "cid-other", &fakeConnection{})); !errors.Is(err, ErrIdentityDrift) {
		t.Fatalf("Adopt() = %v, want ErrIdentityDrift", err)
	}
	if held, _ := registry.Current("slot-1"); held.RuntimeID != "cid-1" {
		t.Fatalf("refused adoption replaced the incumbent: %+v", held)
	}
}

// Revoke must bar the epoch, not merely close what is held: an adoption already
// admitted against the old lease must not be able to re-arm it afterwards.
func TestRevokeBarsLaterAdoptionOfTheSameEpoch(t *testing.T) {
	t.Parallel()
	registry, _, _ := fixture(t, 3)
	registry.Revoke("slot-1", 3)
	connection := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(3, 1, "cid-3", connection)); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Adopt() after Revoke = %v, want ErrRevoked", err)
	}
	if registry.Len() != 0 {
		t.Fatal("a revoked epoch was re-armed")
	}
	if connection.closed() {
		t.Fatal("a refused adoption closed a connection it does not own")
	}
}

// The fencer callback names an exact generation. A callback left over from a
// generation that has already been replaced must not close its successor.
func TestStaleCloseCallbackCannotCloseADifferentGeneration(t *testing.T) {
	t.Parallel()
	registry, backend, _ := fixture(t, 1)
	if err := registry.Adopt(context.Background(), entryFor(1, 1, "cid-1", &fakeConnection{})); err != nil {
		t.Fatal(err)
	}
	rollLease(t, backend, 1, 2)
	next := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(2, 2, "cid-2", next)); err != nil {
		t.Fatal(err)
	}
	// Fire the superseded generation's callback directly.
	registry.closeSlot("slot-1", 1, 1)
	if next.closed() {
		t.Fatal("a stale callback closed the current generation")
	}
	// Same epoch as the current entry but the previous generation: the epoch
	// alone must not be enough to match.
	registry.closeSlot("slot-1", 2, 1)
	if next.closed() {
		t.Fatal("a callback for an older generation closed the current one")
	}
	if held, exists := registry.Current("slot-1"); !exists || held.RuntimeID != "cid-2" {
		t.Fatalf("current generation was dropped: %+v %v", held, exists)
	}
}

func TestAdoptRefusesStaleAndDuplicateGenerations(t *testing.T) {
	t.Parallel()
	registry, _, _ := fixture(t, 2)
	incumbent := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(2, 5, "cid-2", incumbent)); err != nil {
		t.Fatal(err)
	}
	for name, testCase := range map[string]struct {
		entry Entry
		want  error
	}{
		// An older epoch has no lease of its own, so it is refused before the
		// registry's own ordering rules are even consulted.
		"older epoch":               {entryFor(1, 9, "cid-1", &fakeConnection{}), lease.ErrLeaseNotCurrent},
		"older generation":          {entryFor(2, 4, "cid-2", &fakeConnection{}), ErrSuperseded},
		"same generation held":      {entryFor(2, 5, "cid-2", &fakeConnection{}), ErrAlreadyHeld},
		"same generation other cid": {entryFor(2, 5, "cid-other", &fakeConnection{}), ErrIdentityDrift},
	} {
		if err := registry.Adopt(context.Background(), testCase.entry); !errors.Is(err, testCase.want) {
			t.Fatalf("Adopt(%s) = %v, want %v", name, err, testCase.want)
		}
		if incumbent.closed() {
			t.Fatalf("Adopt(%s) disturbed the entry it failed to replace", name)
		}
		if held, _ := registry.Current("slot-1"); held.RuntimeID != "cid-2" || held.Generation != 5 {
			t.Fatalf("Adopt(%s) replaced the incumbent: %+v", name, held)
		}
	}
}

func TestAdoptingANewerGenerationClosesTheOneItReplaces(t *testing.T) {
	t.Parallel()
	registry, backend, _ := fixture(t, 1)
	previous := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(1, 1, "cid-1", previous)); err != nil {
		t.Fatal(err)
	}
	rollLease(t, backend, 1, 2)
	next := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(2, 2, "cid-2", next)); err != nil {
		t.Fatal(err)
	}
	if !previous.closed() {
		t.Fatal("the superseded generation was left connected")
	}
	if next.closed() {
		t.Fatal("the adopted generation was closed")
	}
	if held, _ := registry.Current("slot-1"); held.RuntimeID != "cid-2" || held.Claim.ExecutionEpoch != 2 {
		t.Fatalf("Current() = %+v", held)
	}
}

// Revocation must reclaim the connection, not merely refuse the next request.
func TestRevokeClosesAtOrBelowTheEpochAndSparesNewerOnes(t *testing.T) {
	t.Parallel()
	t.Run("closes the revoked generation", func(t *testing.T) {
		t.Parallel()
		registry, _, _ := fixture(t, 3)
		connection := &fakeConnection{}
		if err := registry.Adopt(context.Background(), entryFor(3, 1, "cid-3", connection)); err != nil {
			t.Fatal(err)
		}
		registry.Revoke("slot-1", 3)
		if !connection.closed() {
			t.Fatal("revocation left the runtime connected")
		}
		if _, exists := registry.Current("slot-1"); exists {
			t.Fatal("revoked slot is still held")
		}
	})
	t.Run("closes a generation strictly below the epoch", func(t *testing.T) {
		t.Parallel()
		registry, _, _ := fixture(t, 2)
		connection := &fakeConnection{}
		if err := registry.Adopt(context.Background(), entryFor(2, 1, "cid-2", connection)); err != nil {
			t.Fatal(err)
		}
		registry.Revoke("slot-1", 5)
		if !connection.closed() {
			t.Fatal("revoking through a later epoch spared an older generation")
		}
	})
	t.Run("spares a generation that already replaced it", func(t *testing.T) {
		t.Parallel()
		registry, _, _ := fixture(t, 4)
		connection := &fakeConnection{}
		if err := registry.Adopt(context.Background(), entryFor(4, 1, "cid-4", connection)); err != nil {
			t.Fatal(err)
		}
		registry.Revoke("slot-1", 3)
		if connection.closed() {
			t.Fatal("revoking an older epoch closed the newer generation")
		}
		if _, exists := registry.Current("slot-1"); !exists {
			t.Fatal("revoking an older epoch dropped the newer generation")
		}
	})
	t.Run("unknown slot is a no-op", func(t *testing.T) {
		t.Parallel()
		registry, _, _ := fixture(t)
		registry.Revoke("slot-absent", 9)
	})
}

// This is the propagation that was missing: losing the execution lease has to
// reclaim the worker connection, and until now nothing closed it.
func TestLosingTheExecutionLeaseReclaimsTheConnection(t *testing.T) {
	t.Parallel()
	registry, backend, fencer := fixture(t, 1)
	connection := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(1, 1, "cid-1", connection)); err != nil {
		t.Fatal(err)
	}
	if err := fencer.Revalidate(context.Background()); err != nil {
		t.Fatalf("revalidate under a current lease: %v", err)
	}
	if connection.closed() {
		t.Fatal("a current lease closed its runtime")
	}

	if err := backend.Revoke(context.Background(), claimFor(1)); err != nil {
		t.Fatal(err)
	}
	if err := fencer.Revalidate(context.Background()); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("revalidate after revocation = %v", err)
	}
	if !connection.closed() {
		t.Fatal("lease revocation left the worker connection open")
	}
	if _, exists := registry.Current("slot-1"); exists {
		t.Fatal("registry still holds a runtime whose lease is gone")
	}
}

// An unreachable lease authority is not evidence that the lease is still held.
func TestUnavailableLeaseAuthorityReclaimsTheConnection(t *testing.T) {
	t.Parallel()
	registry, backend, fencer := fixture(t, 1)
	connection := &fakeConnection{}
	if err := registry.Adopt(context.Background(), entryFor(1, 1, "cid-1", connection)); err != nil {
		t.Fatal(err)
	}
	backend.SetAvailable(false)
	if err := fencer.Revalidate(context.Background()); !errors.Is(err, lease.ErrBackendUnavailable) {
		t.Fatalf("revalidate with an unavailable backend = %v", err)
	}
	if !connection.closed() {
		t.Fatal("an unverifiable lease left the worker connection open")
	}
}

// A stale fencer callback for a generation that has already been replaced must
// not take down its successor.

// Adopt, Release, Revoke and the fencer all race for the same slot. Whatever
// interleaving wins, no connection may be closed twice and none may be left
// open once the registry is empty.
func TestConcurrentAdoptReleaseRevokeAndRevalidateCloseAtMostOnce(t *testing.T) {
	t.Parallel()
	registry, backend, fencer := fixture(t, 1)
	connections := make([]*fakeConnection, 16)
	for index := range connections {
		connections[index] = &fakeConnection{}
	}
	var waiting sync.WaitGroup
	for worker := range connections {
		waiting.Add(1)
		go func(worker int) {
			defer waiting.Done()
			// Generations climb with the worker index so adoptions genuinely
			// contend to supersede one another.
			_ = registry.Adopt(context.Background(), entryFor(1, uint64(worker)+1, "cid-1", connections[worker]))
		}(worker)
	}
	for worker := 0; worker < 8; worker++ {
		waiting.Add(3)
		go func() { defer waiting.Done(); registry.Revoke("slot-1", 1) }()
		go func() { defer waiting.Done(); _ = registry.Release("slot-1", 1, 1) }()
		go func() { defer waiting.Done(); _ = fencer.Revalidate(context.Background()) }()
	}
	waiting.Wait()

	// Revoke barred epoch 1, so nothing can still be held for this slot.
	registry.Revoke("slot-1", 1)
	if registry.Len() != 0 {
		t.Fatalf("registry still holds %d slots after revocation", registry.Len())
	}
	if fencer.Len() != 0 {
		t.Fatalf("fencer still holds %d admissions", fencer.Len())
	}
	if err := backend.Revoke(context.Background(), claimFor(1)); err != nil {
		t.Fatal(err)
	}
	for index, connection := range connections {
		if got := connection.closes.Load(); got > 1 {
			t.Fatalf("connection %d closed %d times", index, got)
		}
	}
}

func TestNewRequiresAFencer(t *testing.T) {
	t.Parallel()
	if _, err := New(nil); err == nil {
		t.Fatal("registry was built without a lease fencer")
	}
}
