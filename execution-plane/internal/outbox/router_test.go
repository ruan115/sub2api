package outbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recordingHandler struct {
	name  string
	calls *[]string
	err   error
}

func (h recordingHandler) ApplyRuntimeEvent(_ context.Context, event Event) error {
	*h.calls = append(*h.calls, h.name+":"+event.EventID)
	return h.err
}

func TestRouterUsesOneOrderedCheckpointForAuthorityAndLifecycleEvents(t *testing.T) {
	var calls []string
	router, err := NewRouter(map[string]Handler{
		"account.proxy_reservation.granted":   recordingHandler{name: "authority", calls: &calls},
		"account.runtime.provision_requested": recordingHandler{name: "onboarding", calls: &calls},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0).UTC()
	source, err := NewMemorySource([]Event{
		{
			Sequence: 1, EventID: "grant-event", AccountID: 10380,
			EventType: "account.proxy_reservation.granted", DesiredGeneration: 7,
			PayloadJSON: []byte(`{"reservation_id":"reservation-7"}`), CreatedAt: now,
		},
		{
			Sequence: 2, EventID: "onboarding-event", AccountID: 10380,
			EventType: "account.runtime.provision_requested", DesiredGeneration: 7,
			PayloadJSON: []byte(`{"onboarding_intent_id":"intent-7"}`), CreatedAt: now,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := NewConsumer(source, router, "runtime-control", "orchestrator-a", time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		processed, err := consumer.RunOnce(context.Background())
		if err != nil || !processed {
			t.Fatalf("RunOnce() = %t, %v", processed, err)
		}
	}
	want := []string{"authority:grant-event", "onboarding:onboarding-event"}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("ordered calls = %v, want %v", calls, want)
	}
}

func TestRouterRejectsUnknownEventsAndPreservesHandlerFailure(t *testing.T) {
	var calls []string
	handlerErr := errors.New("projection failed")
	router, err := NewRouter(map[string]Handler{
		"account.runtime.provision_requested": recordingHandler{name: "onboarding", calls: &calls, err: handlerErr},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000_000, 0).UTC()
	event := Event{
		Sequence: 1, EventID: "event-1", AccountID: 7,
		EventType: "account.runtime.provision_requested", DesiredGeneration: 1,
		PayloadJSON: []byte(`{}`), CreatedAt: now,
	}
	if err := router.ApplyRuntimeEvent(context.Background(), event); !errors.Is(err, handlerErr) {
		t.Fatalf("handler error = %v", err)
	}
	event.EventType = "account.runtime.unknown"
	if err := router.ApplyRuntimeEvent(context.Background(), event); !errors.Is(err, ErrRouteUnavailable) {
		t.Fatalf("unknown route error = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("handler calls = %v", calls)
	}
	if _, err := NewRouter(map[string]Handler{"": recordingHandler{calls: &calls}}); !errors.Is(err, ErrRouteUnavailable) {
		t.Fatalf("invalid route error = %v", err)
	}
}
