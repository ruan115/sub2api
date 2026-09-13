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

func TestRuntimeEventRouterSeparatesAuthorityOnboardingAndLifecycleBehindOneHandler(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	runtimeRepository := store.NewMemoryRepository()
	triggerRepository := &recordingStartTriggerRepository{}
	source := &runtimeDesiredSource{desired: AccountRuntimeDesired{
		AccountID: 7, SlotID: "ccmax-account-7", Provider: "docker", DesiredGeneration: 1,
		ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500, MemoryRequestBytes: 128 << 20,
	}}
	router, err := NewRuntimeEventRouter(source, runtimeRepository, triggerRepository, runtimeRepository, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	grant := outbox.Event{
		Sequence: 1, EventID: "grant-1", AccountID: 7, EventType: ProxyReservationGrantedEvent,
		DesiredGeneration: 1,
		PayloadJSON:       []byte(`{"reservation_id":"reservation-1","proxy_binding_id":"19","binding_revision":1}`),
		CreatedAt:         now,
	}
	if err := router.ApplyRuntimeEvent(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	onboardingEvent := outbox.Event{
		Sequence: 2, EventID: "onboarding-1", AccountID: 7, EventType: "account.runtime.provision_requested",
		DesiredGeneration: 1, PayloadJSON: []byte(`{"onboarding_intent_id":"11111111-2222-4333-8444-555555555555"}`), CreatedAt: now,
	}
	if err := router.ApplyRuntimeEvent(context.Background(), onboardingEvent); err != nil {
		t.Fatal(err)
	}
	if triggerRepository.calls != 1 {
		t.Fatalf("onboarding trigger calls = %d", triggerRepository.calls)
	}
	source.desired.DesiredGeneration = 2
	drain := outbox.Event{
		Sequence: 3, EventID: "drain-2", AccountID: 7, EventType: "account.runtime.drain_requested",
		DesiredGeneration: 2, PayloadJSON: []byte(`{}`), CreatedAt: now,
	}
	if err := router.ApplyRuntimeEvent(context.Background(), drain); err != nil {
		t.Fatal(err)
	}
	slot, err := runtimeRepository.GetSlot(context.Background(), "ccmax-account-7")
	if err != nil || slot.DesiredState != "drained" || slot.DesiredGeneration != 2 {
		t.Fatalf("drained slot = %+v, %v", slot, err)
	}
	if source.calls != 2 {
		t.Fatalf("runtime source calls after onboarding and lifecycle = %d", source.calls)
	}
	source.err = errors.New("CCMAX account deleted after apply")
	if err := router.ApplyRuntimeEvent(context.Background(), drain); err != nil {
		t.Fatalf("router lifecycle receipt replay: %v", err)
	}
	if source.calls != 2 {
		t.Fatalf("router replay consulted mutable source: calls=%d", source.calls)
	}
	conflict := drain
	conflict.PayloadJSON = []byte(`{"changed":true}`)
	if err := router.ApplyRuntimeEvent(context.Background(), conflict); !errors.Is(err, store.ErrLifecycleEventConflict) {
		t.Fatalf("router lifecycle anchor conflict = %v", err)
	}
}

func TestRuntimeEventRouterFailsClosedWhenAnyRouteDependencyIsMissing(t *testing.T) {
	now := time.Now
	runtimeRepository := store.NewMemoryRepository()
	triggerRepository := &recordingStartTriggerRepository{}
	source := &runtimeDesiredSource{}
	for _, build := range []func() error{
		func() error {
			_, err := NewRuntimeEventRouter(nil, runtimeRepository, triggerRepository, runtimeRepository, now)
			return err
		},
		func() error {
			_, err := NewRuntimeEventRouter(source, nil, triggerRepository, runtimeRepository, now)
			return err
		},
		func() error {
			_, err := NewRuntimeEventRouter(source, runtimeRepository, nil, runtimeRepository, now)
			return err
		},
		func() error {
			_, err := NewRuntimeEventRouter(source, runtimeRepository, triggerRepository, nil, now)
			return err
		},
	} {
		if err := build(); !errors.Is(err, ErrRuntimeEventRouter) {
			t.Fatalf("configuration error = %v", err)
		}
	}
}
