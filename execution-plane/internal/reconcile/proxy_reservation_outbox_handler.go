package reconcile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

const (
	ProxyReservationGrantedEvent = "account.proxy_reservation.granted"
	ProxyReservationRevokedEvent = "account.proxy_reservation.revoked"
)

var ErrProxyReservationEvent = errors.New("trusted proxy reservation event is invalid")

type ProxyReservationOutboxHandler struct {
	repository store.ProxyReservationGrantRepository
}

func NewProxyReservationOutboxHandler(repository store.ProxyReservationGrantRepository) (*ProxyReservationOutboxHandler, error) {
	if repository == nil {
		return nil, ErrProxyReservationEvent
	}
	return &ProxyReservationOutboxHandler{repository: repository}, nil
}

func (h *ProxyReservationOutboxHandler) ApplyRuntimeEvent(ctx context.Context, event outbox.Event) error {
	if h == nil || h.repository == nil || ctx == nil || ctx.Err() != nil || event.Validate() != nil ||
		(event.EventType != ProxyReservationGrantedEvent && event.EventType != ProxyReservationRevokedEvent) {
		return ErrProxyReservationEvent
	}
	payload, err := decodeProxyReservationPayload(event.PayloadJSON)
	if err != nil {
		return ErrProxyReservationEvent
	}
	accountID := strconv.FormatInt(event.AccountID, 10)
	for _, value := range []string{accountID, event.EventID, payload.ReservationID} {
		if store.ValidateProxyReservationOpaqueID(value) != nil {
			return ErrProxyReservationEvent
		}
	}
	if store.ValidateProxyBindingID(payload.ProxyBindingID) != nil {
		return ErrProxyReservationEvent
	}
	switch event.EventType {
	case ProxyReservationGrantedEvent:
		_, err = h.repository.GrantProxyReservation(ctx, store.ProxyReservationGrant{
			ReservationID: payload.ReservationID, AccountID: accountID, DesiredGeneration: event.DesiredGeneration,
			ProxyBindingID: payload.ProxyBindingID, BindingRevision: payload.BindingRevision,
			GrantEventID: event.EventID, CreatedAt: event.CreatedAt.UTC(), UpdatedAt: event.CreatedAt.UTC(),
		})
	case ProxyReservationRevokedEvent:
		_, err = h.repository.RevokeProxyReservation(ctx, store.ProxyReservationRevocation{
			ReservationID: payload.ReservationID, AccountID: accountID, DesiredGeneration: event.DesiredGeneration,
			ProxyBindingID: payload.ProxyBindingID, BindingRevision: payload.BindingRevision,
			RevokeEventID: event.EventID, RevokedAt: event.CreatedAt.UTC(),
		})
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrProxyReservationEvent, err)
	}
	return nil
}

type proxyReservationPayload struct {
	ReservationID   string
	ProxyBindingID  string
	BindingRevision uint64
}

func decodeProxyReservationPayload(payloadJSON []byte) (proxyReservationPayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(payloadJSON))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return proxyReservationPayload{}, ErrProxyReservationEvent
	}
	var payload proxyReservationPayload
	seen := make(map[string]struct{}, 3)
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return proxyReservationPayload{}, ErrProxyReservationEvent
		}
		if _, duplicate := seen[key]; duplicate {
			return proxyReservationPayload{}, ErrProxyReservationEvent
		}
		seen[key] = struct{}{}
		switch key {
		case "reservation_id":
			err = decoder.Decode(&payload.ReservationID)
		case "proxy_binding_id":
			err = decoder.Decode(&payload.ProxyBindingID)
		case "binding_revision":
			err = decoder.Decode(&payload.BindingRevision)
		default:
			return proxyReservationPayload{}, ErrProxyReservationEvent
		}
		if err != nil {
			return proxyReservationPayload{}, ErrProxyReservationEvent
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF || len(seen) != 3 ||
		payload.BindingRevision == 0 || store.ValidateProxyReservationOpaqueID(payload.ReservationID) != nil ||
		store.ValidateProxyBindingID(payload.ProxyBindingID) != nil {
		return proxyReservationPayload{}, ErrProxyReservationEvent
	}
	return payload, nil
}

var _ outbox.Handler = (*ProxyReservationOutboxHandler)(nil)
