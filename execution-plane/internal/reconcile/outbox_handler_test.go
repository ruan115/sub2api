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

func TestOutboxHandlerProjectsAccountEventsIntoIdempotentSlotDesiredState(t *testing.T) {
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
	provision := outbox.Event{
		Sequence: 1, EventID: "event-1", AccountID: 7, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{"provider":"docker"}`), CreatedAt: now,
	}
	if err := handler.ApplyRuntimeEvent(context.Background(), provision); err != nil {
		t.Fatal(err)
	}
	if err := handler.ApplyRuntimeEvent(context.Background(), provision); err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	slot, err := repository.GetSlot(context.Background(), "ccmax-account-7")
	if err != nil {
		t.Fatal(err)
	}
	if slot.AccountID != "7" || slot.DesiredState != "ready" || slot.DesiredGeneration != 1 {
		t.Fatalf("projected slot = %+v", slot)
	}
	source.desired.DesiredGeneration = 2
	drain := provision
	drain.Sequence = 2
	drain.EventID = "event-2"
	drain.EventType = "account.runtime.drain_requested"
	drain.DesiredGeneration = 2
	if err := handler.ApplyRuntimeEvent(context.Background(), drain); err != nil {
		t.Fatal(err)
	}
	slot, _ = repository.GetSlot(context.Background(), "ccmax-account-7")
	if slot.DesiredState != "drained" || slot.DesiredGeneration != 2 {
		t.Fatalf("drained slot = %+v", slot)
	}
	source.desired.DesiredGeneration = 1
	if err := handler.ApplyRuntimeEvent(context.Background(), provision); !errors.Is(err, store.ErrStaleGeneration) {
		t.Fatalf("stale outbox replay error = %v", err)
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
		Sequence: 1, EventID: "event-1", AccountID: 7, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{}`), CreatedAt: now,
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("generation mismatch error = %v", err)
	}
}

type runtimeDesiredSource struct {
	desired AccountRuntimeDesired
	err     error
}

func (s *runtimeDesiredSource) LoadAccountRuntimeDesired(context.Context, int64, uint64) (AccountRuntimeDesired, error) {
	return s.desired, s.err
}
