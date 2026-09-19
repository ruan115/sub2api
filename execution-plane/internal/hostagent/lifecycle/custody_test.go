package lifecycle

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeregistry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type custodyConnection struct{ closes atomic.Int32 }

func (c *custodyConnection) Close() error { c.closes.Add(1); return nil }

func (c *custodyConnection) closed() bool { return c.closes.Load() > 0 }

func custodyClaim(epoch uint64) lease.Claim {
	return lease.Claim{SlotID: "slot-1", NodeID: "node-1", ExecutionEpoch: epoch, OwnerID: "owner-1"}
}

func custodyFixture(t *testing.T, interval time.Duration) (*Custody, *lease.MemoryBackend) {
	t.Helper()
	backend := lease.NewMemoryBackend(time.Now)
	if err := backend.Acquire(context.Background(), custodyClaim(1), time.Minute); err != nil {
		t.Fatal(err)
	}
	custody, err := NewCustody(CustodyConfig{
		Validator: backend, NodeID: "node-1", RevalidateInterval: interval,
	})
	if err != nil {
		t.Fatal(err)
	}
	return custody, backend
}

// adopt drives the registry directly, because a *hostagent.Runtime cannot be
// built outside its own package. The claim construction that take performs is
// covered end to end by the authenticated START test.
func adopt(t *testing.T, custody *Custody, epoch, generation uint64, runtimeID string, connection runtimeregistry.Connection) {
	t.Helper()
	err := custody.registry.Adopt(context.Background(), runtimeregistry.Entry{
		Claim: custodyClaim(epoch), Generation: generation, RuntimeID: runtimeID, Connection: connection,
	})
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
}

func TestNewCustodyRejectsIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	backend := lease.NewMemoryBackend(time.Now)
	valid := CustodyConfig{Validator: backend, NodeID: "node-1", RevalidateInterval: time.Second}
	for name, mutate := range map[string]func(*CustodyConfig){
		"no validator":      func(c *CustodyConfig) { c.Validator = nil },
		"no node":           func(c *CustodyConfig) { c.NodeID = "" },
		"zero interval":     func(c *CustodyConfig) { c.RevalidateInterval = 0 },
		"negative interval": func(c *CustodyConfig) { c.RevalidateInterval = -time.Second },
		"interval too long": func(c *CustodyConfig) { c.RevalidateInterval = time.Hour },
	} {
		broken := valid
		mutate(&broken)
		if _, err := NewCustody(broken); !errors.Is(err, ErrCustody) {
			t.Fatalf("NewCustody(%s) = %v, want ErrCustody", name, err)
		}
	}
	if _, err := NewCustody(valid); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
}

// A custodian that was never configured must not pretend to hold anything, and
// must not panic on a teardown path.
func TestNilCustodyIsInertRatherThanPanicking(t *testing.T) {
	t.Parallel()
	var custody *Custody
	if _, err := custody.take(context.Background(), provider.Instance{}, "owner-1", &custodyConnection{}); !errors.Is(err, ErrCustody) {
		t.Fatalf("take on nil custody = %v", err)
	}
	if err := custody.Revalidate(context.Background()); !errors.Is(err, ErrCustody) {
		t.Fatalf("Revalidate on nil custody = %v", err)
	}
	if err := custody.Run(context.Background()); !errors.Is(err, ErrCustody) {
		t.Fatalf("Run on nil custody = %v", err)
	}
	if err := custody.Release("slot-1", 1, 1); !errors.Is(err, ErrCustody) {
		t.Fatalf("Release on nil custody = %v", err)
	}
	custody.Revoke("slot-1", 1)
	if custody.Drain() != 0 || custody.Len() != 0 {
		t.Fatal("nil custody reported holdings")
	}
	if _, exists := custody.Held("slot-1"); exists {
		t.Fatal("nil custody reported a held slot")
	}
}

func custodyInstance(epoch, generation uint64, runtimeID string) provider.Instance {
	return provider.Instance{
		ProviderRef: "execution-slot-1", RuntimeID: runtimeID, SlotID: "slot-1",
		Epoch: epoch, RuntimeGeneration: generation,
	}
}

// take must build the claim from the instance plus the owner the command
// carried, and must prove it against the authority before holding anything.
func TestCustodyTakeBuildsTheClaimFromTheCommandOwner(t *testing.T) {
	t.Parallel()
	custody, _ := custodyFixture(t, time.Second)
	connection := &custodyConnection{}
	adopted, err := custody.take(context.Background(), custodyInstance(1, 3, "cid-1"), "owner-1", connection)
	if err != nil || !adopted {
		t.Fatalf("take = %v, %v", adopted, err)
	}
	held, exists := custody.Held("slot-1")
	if !exists || held.Claim != custodyClaim(1) || held.Generation != 3 || held.RuntimeID != "cid-1" {
		t.Fatalf("held = %+v %v", held, exists)
	}
	if connection.closed() {
		t.Fatal("take closed a connection it accepted")
	}
}

// An owner the authority does not recognise is refused. The host agent cannot
// substitute one of its own, and a refusal leaves the connection with the
// caller.
func TestCustodyTakeRefusesAnOwnerTheAuthorityDoesNotConfirm(t *testing.T) {
	t.Parallel()
	custody, _ := custodyFixture(t, time.Second)
	connection := &custodyConnection{}
	adopted, err := custody.take(context.Background(), custodyInstance(1, 1, "cid-1"), "owner-someone-else", connection)
	if adopted || !errors.Is(err, ErrCustody) {
		t.Fatalf("take with a foreign owner = %v, %v", adopted, err)
	}
	if custody.Len() != 0 {
		t.Fatal("a refused claim was taken into custody")
	}
	if connection.closed() {
		t.Fatal("take closed a connection it refused")
	}
	// An absent owner cannot even be stated, let alone proved.
	if adopted, err := custody.take(context.Background(), custodyInstance(1, 1, "cid-1"), "", connection); adopted || !errors.Is(err, ErrCustody) {
		t.Fatalf("take with no owner = %v, %v", adopted, err)
	}
}

// START is replayable. A replay of the same instance keeps the incumbent and
// tells the caller to close its duplicate, rather than failing the command.
func TestCustodyTakeTreatsAnIdenticalReplayAsANoOp(t *testing.T) {
	t.Parallel()
	custody, _ := custodyFixture(t, time.Second)
	first, replay := &custodyConnection{}, &custodyConnection{}
	if adopted, err := custody.take(context.Background(), custodyInstance(1, 1, "cid-1"), "owner-1", first); err != nil || !adopted {
		t.Fatalf("first take = %v, %v", adopted, err)
	}
	adopted, err := custody.take(context.Background(), custodyInstance(1, 1, "cid-1"), "owner-1", replay)
	if err != nil {
		t.Fatalf("replayed take = %v", err)
	}
	if adopted {
		t.Fatal("a replay claimed to transfer ownership a second time")
	}
	if first.closed() {
		t.Fatal("a replay reclaimed the incumbent")
	}
	if replay.closed() {
		t.Fatal("take closed the duplicate itself; that is the caller's job")
	}
	if custody.Len() != 1 {
		t.Fatalf("custody holds %d slots after a replay, want 1", custody.Len())
	}
}

// A different container arriving under a generation the slot already used is
// refused, whatever it calls itself.
func TestCustodyTakeRefusesADifferentContainer(t *testing.T) {
	t.Parallel()
	custody, _ := custodyFixture(t, time.Second)
	if adopted, err := custody.take(context.Background(), custodyInstance(1, 1, "cid-1"), "owner-1", &custodyConnection{}); err != nil || !adopted {
		t.Fatalf("first take = %v, %v", adopted, err)
	}
	impostor := &custodyConnection{}
	adopted, err := custody.take(context.Background(), custodyInstance(1, 1, "cid-other"), "owner-1", impostor)
	if adopted || !errors.Is(err, ErrCustody) {
		t.Fatalf("take of a different container = %v, %v", adopted, err)
	}
	if impostor.closed() {
		t.Fatal("take closed a connection it refused")
	}
	if held, _ := custody.Held("slot-1"); held.RuntimeID != "cid-1" {
		t.Fatalf("the incumbent was replaced: %+v", held)
	}
}

// After shutdown nothing may be taken back into custody: the revalidation loop
// has stopped, so anything stored afterwards would never be reclaimed.
func TestCustodyTakeIsBarredAfterDrain(t *testing.T) {
	t.Parallel()
	custody, _ := custodyFixture(t, time.Second)
	custody.Drain()
	connection := &custodyConnection{}
	adopted, err := custody.take(context.Background(), custodyInstance(1, 1, "cid-1"), "owner-1", connection)
	if adopted || !errors.Is(err, ErrCustody) {
		t.Fatalf("take after drain = %v, %v", adopted, err)
	}
	if custody.Len() != 0 {
		t.Fatal("a drained custodian took a runtime back")
	}
	if connection.closed() {
		t.Fatal("take closed a connection it refused")
	}
}

// Registry bookkeeping is not evidence. This adopts a real gRPC transport to a
// real listener and proves that reclaiming it actually tears the transport
// down: an RPC that would otherwise reach the server fails because the client
// connection is closing, and the connection reports Shutdown.
func TestReclaimTearsDownTheRealTransportNotJustTheBookkeeping(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })

	connection, err := grpc.NewClient(listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
	if err != nil {
		t.Fatal(err)
	}
	connection.Connect()
	ready, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for connection.GetState() != connectivity.Ready {
		if !connection.WaitForStateChange(ready, connection.GetState()) {
			t.Fatalf("transport never became ready: %s", connection.GetState())
		}
	}

	// While held, the transport is live: the server answers, even if only to
	// say the method is not implemented.
	client := executionv1.NewWorkerRuntimeServiceClient(connection)
	callCtx, callCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer callCancel()
	_, err = client.Health(callCtx, &executionv1.HealthRequest{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("held transport did not reach the server: %v", err)
	}

	custody, _ := custodyFixture(t, time.Second)
	adopted, err := custody.take(context.Background(), custodyInstance(1, 1, "cid-1"), "owner-1", connection)
	if err != nil || !adopted {
		t.Fatalf("take = %v, %v", adopted, err)
	}

	custody.Revoke("slot-1", 1)

	if state := connection.GetState(); state != connectivity.Shutdown {
		t.Fatalf("reclaimed transport state = %s, want Shutdown", state)
	}
	afterCtx, afterCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer afterCancel()
	_, err = client.Health(afterCtx, &executionv1.HealthRequest{})
	if err == nil {
		t.Fatal("an RPC succeeded on a reclaimed transport")
	}
	if status.Code(err) == codes.Unimplemented {
		t.Fatalf("the reclaimed transport still reached the server: %v", err)
	}
}

// Custody ends on either signal the node actually has: the control session
// going away, or the control plane revoking the epoch. This exercises the
// SessionAuthority wiring; it is not by itself a statement about where
// credentials live.
func TestCustodyEndsWhenTheSessionAuthorityGoesStaleOrRevokes(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000_000_000, 0).UTC()
	sessionOpen := true
	closedAt := time.Time{}
	revoked := map[string]uint64{}
	authority, err := hostagent.NewSessionAuthority(hostagent.SessionAuthorityConfig{
		Revoked:      func(slotID string, epoch uint64) bool { return epoch <= revoked[slotID] },
		SessionState: func() (bool, time.Time) { return sessionOpen, closedAt },
		OfflineAfter: 45 * time.Second,
		Now:          func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	custody, err := NewCustody(CustodyConfig{Validator: authority, NodeID: "node-1", RevalidateInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}

	connection := &custodyConnection{}
	adopted, err := custody.take(context.Background(), custodyInstance(7, 1, "cid-1"), "owner-1", connection)
	if err != nil || !adopted {
		t.Fatalf("take under a fresh session = %v, %v", adopted, err)
	}
	if err := custody.Revalidate(context.Background()); err != nil {
		t.Fatalf("a fresh session did not hold: %v", err)
	}
	if connection.closed() {
		t.Fatal("a fresh session reclaimed its runtime")
	}

	// The control session goes away. Nothing else changes.
	sessionOpen, closedAt = false, now.Add(-time.Minute)
	if err := custody.Revalidate(context.Background()); !errors.Is(err, lease.ErrBackendUnavailable) {
		t.Fatalf("stale session revalidate = %v, want ErrBackendUnavailable", err)
	}
	if !connection.closed() || custody.Len() != 0 {
		t.Fatal("a node that lost the control plane kept holding its runtime")
	}

	// With the session restored, an explicit revocation is the other way custody
	// ends, and it ends only the slot the control plane named.
	sessionOpen, closedAt = true, time.Time{}
	kept, ended := &custodyConnection{}, &custodyConnection{}
	if _, err := custody.take(context.Background(), custodyInstance(7, 2, "cid-2"), "owner-1", ended); err != nil {
		t.Fatalf("re-take after restore: %v", err)
	}
	otherSlot := custodyInstance(7, 1, "cid-3")
	otherSlot.SlotID = "slot-2"
	if _, err := custody.take(context.Background(), otherSlot, "owner-1", kept); err != nil {
		t.Fatalf("take of a second slot: %v", err)
	}
	revoked["slot-1"] = 7
	if err := custody.Revalidate(context.Background()); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("revoked revalidate = %v, want ErrLeaseNotCurrent", err)
	}
	if !ended.closed() {
		t.Fatal("the revoked slot kept its runtime")
	}
	if kept.closed() {
		t.Fatal("revoking one slot reclaimed another")
	}
}

func TestCustodyRevalidateReclaimsOnLeaseLoss(t *testing.T) {
	t.Parallel()
	custody, backend := custodyFixture(t, time.Second)
	connection := &custodyConnection{}
	adopt(t, custody, 1, 1, "cid-1", connection)
	if err := custody.Revalidate(context.Background()); err != nil {
		t.Fatalf("revalidate under a current lease: %v", err)
	}
	if connection.closed() {
		t.Fatal("a current lease closed its runtime")
	}
	if err := backend.Revoke(context.Background(), custodyClaim(1)); err != nil {
		t.Fatal(err)
	}
	if err := custody.Revalidate(context.Background()); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("revalidate after revocation = %v", err)
	}
	if !connection.closed() || custody.Len() != 0 {
		t.Fatal("lease loss did not reclaim the runtime")
	}
}

// An unreachable authority is not evidence that a lease is still held.
func TestCustodyRevalidateReclaimsWhenAuthorityIsUnreachable(t *testing.T) {
	t.Parallel()
	custody, backend := custodyFixture(t, time.Second)
	connection := &custodyConnection{}
	adopt(t, custody, 1, 1, "cid-1", connection)
	backend.SetAvailable(false)
	if err := custody.Revalidate(context.Background()); !errors.Is(err, lease.ErrBackendUnavailable) {
		t.Fatalf("revalidate with an unreachable authority = %v", err)
	}
	if !connection.closed() {
		t.Fatal("an unverifiable lease was left in custody")
	}
}

func TestCustodyRunRevalidatesOnIntervalThenDrains(t *testing.T) {
	t.Parallel()
	custody, backend := custodyFixture(t, 20*time.Millisecond)
	reclaimed := &custodyConnection{}
	adopt(t, custody, 1, 1, "cid-1", reclaimed)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- custody.Run(ctx) }()

	// A lease that ends between ticks must be noticed by the loop itself.
	if err := backend.Revoke(context.Background(), custodyClaim(1)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for !reclaimed.closed() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !reclaimed.closed() {
		t.Fatal("the revalidation loop did not reclaim a lost lease")
	}

	// Shutdown drains whatever is still held.
	if err := backend.Acquire(context.Background(), custodyClaim(2), time.Minute); err != nil {
		t.Fatal(err)
	}
	surviving := &custodyConnection{}
	adopt(t, custody, 2, 2, "cid-2", surviving)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not stop")
	}
	if !surviving.closed() || custody.Len() != 0 {
		t.Fatal("shutdown left a runtime in custody")
	}
}

// Revoking one slot must not disturb another slot on the same node.
func TestCustodyRevokeIsScopedToOneSlot(t *testing.T) {
	t.Parallel()
	custody, backend := custodyFixture(t, time.Second)
	other := lease.Claim{SlotID: "slot-2", NodeID: "node-1", ExecutionEpoch: 1, OwnerID: "owner-1"}
	if err := backend.Acquire(context.Background(), other, time.Minute); err != nil {
		t.Fatal(err)
	}
	first, second := &custodyConnection{}, &custodyConnection{}
	adopt(t, custody, 1, 1, "cid-1", first)
	if err := custody.registry.Adopt(context.Background(), runtimeregistry.Entry{
		Claim: other, Generation: 1, RuntimeID: "cid-2", Connection: second,
	}); err != nil {
		t.Fatal(err)
	}
	custody.Revoke("slot-1", 1)
	if !first.closed() {
		t.Fatal("revoking a slot did not reclaim it")
	}
	if second.closed() {
		t.Fatal("revoking one slot reclaimed another slot on the same node")
	}
	if custody.Len() != 1 {
		t.Fatalf("custody holds %d slots, want the untouched one", custody.Len())
	}
	if _, exists := custody.Held("slot-2"); !exists {
		t.Fatal("the untouched slot was dropped")
	}
}

func TestCustodyReleaseReportsAlreadyReclaimedAsBenign(t *testing.T) {
	t.Parallel()
	custody, _ := custodyFixture(t, time.Second)
	connection := &custodyConnection{}
	adopt(t, custody, 1, 1, "cid-1", connection)
	if err := custody.Release("slot-1", 1, 1); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !connection.closed() {
		t.Fatal("Release did not close the runtime")
	}
	if err := custody.Release("slot-1", 1, 1); !errors.Is(err, runtimeregistry.ErrNotHeld) {
		t.Fatalf("second Release = %v, want ErrNotHeld", err)
	}
	if got := connection.closes.Load(); got != 1 {
		t.Fatalf("connection closed %d times, want exactly 1", got)
	}
}
