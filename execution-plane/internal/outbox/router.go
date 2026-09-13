package outbox

import (
	"context"
	"errors"
	"strings"
)

var ErrRouteUnavailable = errors.New("runtime outbox event route is unavailable")

// Router dispatches every event claimed by one ordered consumer to exactly
// one handler. Keeping authority and lifecycle events behind the same
// checkpoint preserves CCMAX sequence ordering (for example proxy grant before
// the onboarding event that consumes it).
type Router struct {
	routes map[string]Handler
}

func NewRouter(routes map[string]Handler) (*Router, error) {
	if len(routes) == 0 {
		return nil, ErrRouteUnavailable
	}
	copyRoutes := make(map[string]Handler, len(routes))
	for eventType, handler := range routes {
		if strings.TrimSpace(eventType) == "" || len(eventType) > 96 || handler == nil {
			return nil, ErrRouteUnavailable
		}
		copyRoutes[eventType] = handler
	}
	return &Router{routes: copyRoutes}, nil
}

func (r *Router) ApplyRuntimeEvent(ctx context.Context, event Event) error {
	if r == nil || ctx == nil || ctx.Err() != nil {
		return ErrRouteUnavailable
	}
	if event.Validate() != nil {
		return BlockingHandlerError(FailureIntegrity, "runtime_event_invalid", ErrRouteUnavailable)
	}
	handler, exists := r.routes[event.EventType]
	if !exists || handler == nil {
		return BlockingHandlerError(FailureIntegrity, "runtime_event_route_unavailable", ErrRouteUnavailable)
	}
	if err := handler.ApplyRuntimeEvent(ctx, event); err != nil {
		return err
	}
	return nil
}

var _ Handler = (*Router)(nil)
