package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestOutboxHandlerFreezesLifecycleProjectionBeforeAckReplay(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := store.NewMemoryRepository()
	source := &runtimeDesiredSource{desired: AccountRuntimeDesired{
		AccountID: 7, SlotID: "ccmax-account-7", Provider: "docker", DesiredGeneration: 1,
		RequiredLabels: map[string]string{"region": "ap-shanghai"}, ImageDigest: "sha256:" + strings.Repeat("a", 64),
		CPURequestMillis: 500, MemoryRequestBytes: 256 << 20,
	}}
	handler, err := NewOutboxHandler(source, repository, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	restore := outbox.Event{
		Sequence: 1, EventID: "event-1", AccountID: 7, EventType: "account.runtime.restore_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{"reason":"restore"}`), CreatedAt: now,
	}
	if err := handler.ApplyRuntimeEvent(context.Background(), restore); err != nil {
		t.Fatal(err)
	}
	if source.calls != 1 {
		t.Fatalf("first apply source calls = %d", source.calls)
	}

	// Simulate an ACK loss followed by account deletion and changed deployment
	// defaults. Exact replay must finish from worker_runtime alone.
	source.err = errors.New("account deleted")
	source.desired.RequiredLabels = map[string]string{"region": "changed"}
	source.desired.ImageDigest = "sha256:" + strings.Repeat("b", 64)
	source.desired.CPURequestMillis = 9_000
	if err := handler.ApplyRuntimeEvent(context.Background(), restore); err != nil {
		t.Fatalf("durable exact replay: %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("exact replay consulted mutable source: calls=%d", source.calls)
	}
	slot, err := repository.GetSlot(context.Background(), "ccmax-account-7")
	if err != nil {
		t.Fatal(err)
	}
	if slot.RequiredLabels["region"] != "ap-shanghai" || slot.ImageDigest != "sha256:"+strings.Repeat("a", 64) || slot.CPURequestMillis != 500 {
		t.Fatalf("slot policy was not frozen: %+v", slot)
	}

	conflictingID := restore
	conflictingID.PayloadJSON = []byte(`{"reason":"different"}`)
	if err := handler.ApplyRuntimeEvent(context.Background(), conflictingID); !errors.Is(err, store.ErrLifecycleEventConflict) {
		t.Fatalf("same event id with changed anchor error = %v", err)
	}
	conflictingSequence := restore
	conflictingSequence.EventID = "different-event-id"
	if err := handler.ApplyRuntimeEvent(context.Background(), conflictingSequence); !errors.Is(err, store.ErrLifecycleEventConflict) {
		t.Fatalf("same sequence with changed event id error = %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("conflicting replay consulted mutable source: calls=%d", source.calls)
	}
}

func TestOutboxHandlerProjectsOrderedLifecycleGenerations(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := store.NewMemoryRepository()
	source := &runtimeDesiredSource{desired: AccountRuntimeDesired{
		AccountID: 7, SlotID: "ccmax-account-7", Provider: "docker", DesiredGeneration: 1,
		ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500, MemoryRequestBytes: 256 << 20,
	}}
	handler, _ := NewOutboxHandler(source, repository, func() time.Time { return now })
	restore := outbox.Event{
		Sequence: 1, EventID: "restore-1", AccountID: 7, EventType: "account.runtime.restore_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: now,
	}
	if err := handler.ApplyRuntimeEvent(context.Background(), restore); err != nil {
		t.Fatal(err)
	}
	source.desired.DesiredGeneration = 2
	drain := restore
	drain.Sequence = 2
	drain.EventID = "drain-2"
	drain.EventType = "account.runtime.drain_requested"
	drain.DesiredGeneration = 2
	if err := handler.ApplyRuntimeEvent(context.Background(), drain); err != nil {
		t.Fatal(err)
	}
	slot, _ := repository.GetSlot(context.Background(), "ccmax-account-7")
	if slot.DesiredState != "drained" || slot.DesiredGeneration != 2 {
		t.Fatalf("drained slot = %+v", slot)
	}
	// A delayed duplicate is acknowledged from its receipt even after the slot
	// has advanced; it must not be re-projected as a stale generation.
	if err := handler.ApplyRuntimeEvent(context.Background(), restore); err != nil {
		t.Fatalf("historical exact replay: %v", err)
	}
	if source.calls != 2 {
		t.Fatalf("historical replay source calls = %d", source.calls)
	}
}

func TestOutboxHandlerRejectsMismatchedDesiredGeneration(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	source := &runtimeDesiredSource{desired: AccountRuntimeDesired{
		AccountID: 7, SlotID: "ccmax-account-7", Provider: "docker", DesiredGeneration: 2,
		ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500, MemoryRequestBytes: 256 << 20,
	}}
	handler, _ := NewOutboxHandler(source, store.NewMemoryRepository(), func() time.Time { return now })
	err := handler.ApplyRuntimeEvent(context.Background(), outbox.Event{
		Sequence: 1, EventID: "event-1", AccountID: 7, EventType: "account.runtime.restore_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: now,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("generation mismatch error = %v", err)
	}
}

func TestOutboxHandlerRejectsEventsOwnedByOtherRoutes(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	source := &runtimeDesiredSource{}
	handler, _ := NewOutboxHandler(source, store.NewMemoryRepository(), func() time.Time { return now })
	for index, eventType := range []string{
		"account.runtime.provision_requested",
		"account.credential.migrate_requested",
		"account.credential.rotate_requested",
		ProxyReservationGrantedEvent,
		ProxyReservationRevokedEvent,
	} {
		event := outbox.Event{
			Sequence: int64(index + 1), EventID: "foreign-event-" + eventType, AccountID: 7,
			EventType: eventType, DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: now,
		}
		if err := handler.ApplyRuntimeEvent(context.Background(), event); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("event %s error = %v", eventType, err)
		}
	}
	if source.calls != 0 {
		t.Fatalf("foreign route consulted runtime source: %d", source.calls)
	}
}

type runtimeDesiredSource struct {
	desired AccountRuntimeDesired
	err     error
	calls   int
}

func (s *runtimeDesiredSource) LoadAccountRuntimeDesired(context.Context, int64, uint64) (AccountRuntimeDesired, error) {
	s.calls++
	return s.desired, s.err
}
