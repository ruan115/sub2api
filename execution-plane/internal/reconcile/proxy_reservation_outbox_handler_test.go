package reconcile

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestProxyReservationOutboxHandlerGrantRevokeAndExactReplay(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := store.NewMemoryRepository()
	handler, err := NewProxyReservationOutboxHandler(repository)
	if err != nil {
		t.Fatal(err)
	}
	grant := proxyReservationEvent(now, ProxyReservationGrantedEvent, "event-grant-7")
	if err := handler.ApplyRuntimeEvent(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	if err := handler.ApplyRuntimeEvent(context.Background(), grant); err != nil {
		t.Fatalf("grant replay: %v", err)
	}
	stored, err := repository.GetProxyReservation(context.Background(), "reservation-7")
	if err != nil || stored.AccountID != "7" || stored.DesiredGeneration != 4 || stored.BindingRevision != 3 || stored.RevokedAt != nil {
		t.Fatalf("stored grant = %+v, %v", stored, err)
	}

	wrongBinding := grant
	wrongBinding.EventID = "event-grant-wrong"
	wrongBinding.PayloadJSON = []byte(`{"reservation_id":"reservation-7","proxy_binding_id":"8","binding_revision":3}`)
	if err := handler.ApplyRuntimeEvent(context.Background(), wrongBinding); !errors.Is(err, store.ErrProxyReservationConflict) {
		t.Fatalf("changed grant binding = %v", err)
	}

	revoke := proxyReservationEvent(now.Add(time.Second), ProxyReservationRevokedEvent, "event-revoke-7")
	if err := handler.ApplyRuntimeEvent(context.Background(), revoke); err != nil {
		t.Fatal(err)
	}
	if err := handler.ApplyRuntimeEvent(context.Background(), revoke); err != nil {
		t.Fatalf("revoke replay: %v", err)
	}
	if err := repository.ValidateCurrentProxyReservation(context.Background(), "7", 4, "reservation-7", 3, now.Add(time.Second)); !errors.Is(err, store.ErrProxyReservationNotFound) {
		t.Fatalf("revoked grant validation = %v", err)
	}
}

func TestProxyReservationOutboxHandlerRejectsUnknownDuplicateAndSecretPayload(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	handler, _ := NewProxyReservationOutboxHandler(store.NewMemoryRepository())
	for name, payload := range map[string]string{
		"unknown":      `{"reservation_id":"reservation-7","proxy_binding_id":"7","binding_revision":3,"endpoint":"proxy.example"}`,
		"duplicate":    `{"reservation_id":"reservation-7","reservation_id":"reservation-8","proxy_binding_id":"7","binding_revision":3}`,
		"missing":      `{"reservation_id":"reservation-7","binding_revision":3}`,
		"secret":       `{"reservation_id":"reservation-7","proxy_binding_id":"sk-ant-secret","binding_revision":3}`,
		"url":          `{"reservation_id":"reservation-7","proxy_binding_id":"https://proxy.example","binding_revision":3}`,
		"host":         `{"reservation_id":"reservation-7","proxy_binding_id":"proxy.example.com","binding_revision":3}`,
		"ip":           `{"reservation_id":"reservation-7","proxy_binding_id":"1.2.3.4","binding_revision":3}`,
		"leading_zero": `{"reservation_id":"reservation-7","proxy_binding_id":"007","binding_revision":3}`,
		"overflow":     `{"reservation_id":"reservation-7","proxy_binding_id":"9223372036854775808","binding_revision":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			event := proxyReservationEvent(now, ProxyReservationGrantedEvent, "event-grant-7")
			event.PayloadJSON = []byte(payload)
			if err := handler.ApplyRuntimeEvent(context.Background(), event); !errors.Is(err, ErrProxyReservationEvent) {
				t.Fatalf("payload error = %v", err)
			}
		})
	}
	unknownEvent := proxyReservationEvent(now, "account.proxy_reservation.changed", "event-change-7")
	if err := handler.ApplyRuntimeEvent(context.Background(), unknownEvent); !errors.Is(err, ErrProxyReservationEvent) {
		t.Fatalf("unknown event error = %v", err)
	}
}

func TestProxyReservationOutboxHandlerRejectsWrongRevocationFence(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := store.NewMemoryRepository()
	handler, _ := NewProxyReservationOutboxHandler(repository)
	grant := proxyReservationEvent(now, ProxyReservationGrantedEvent, "event-grant-7")
	if err := handler.ApplyRuntimeEvent(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	revoke := proxyReservationEvent(now.Add(time.Second), ProxyReservationRevokedEvent, "event-revoke-7")
	revoke.PayloadJSON = []byte(`{"reservation_id":"reservation-7","proxy_binding_id":"7","binding_revision":4}`)
	if err := handler.ApplyRuntimeEvent(context.Background(), revoke); !errors.Is(err, store.ErrProxyReservationConflict) {
		t.Fatalf("wrong revision error = %v", err)
	}
	if err := repository.ValidateCurrentProxyReservation(context.Background(), "7", 4, "reservation-7", 3, now.Add(time.Second)); err != nil {
		t.Fatalf("wrong revoke changed grant: %v", err)
	}
}

func proxyReservationEvent(at time.Time, eventType, eventID string) outbox.Event {
	return outbox.Event{
		Sequence: 7, EventID: eventID, AccountID: 7, EventType: eventType, DesiredGeneration: 4,
		PayloadJSON: []byte(`{"reservation_id":"reservation-7","proxy_binding_id":"7","binding_revision":3}`),
		CreatedAt:   at.UTC(),
	}
}
