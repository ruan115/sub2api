package route

import (
	"context"
	"errors"
	"time"
)

type PublishableRoute struct {
	SlotID     string
	NodeID     string
	Endpoint   string
	Epoch      uint64
	Generation uint64
}

type Source interface {
	ListPublishableRoutes(ctx context.Context, checkedAt time.Time) ([]PublishableRoute, error)
}

type Reconciler struct {
	source     Source
	publisher  *Publisher
	published  map[string]Route
	now        func() time.Time
}

func NewReconciler(source Source, publisher *Publisher, now func() time.Time) (*Reconciler, error) {
	if source == nil || publisher == nil {
		return nil, errors.New("execution route source and publisher are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Reconciler{source: source, publisher: publisher, published: map[string]Route{}, now: now}, nil
}

func (r *Reconciler) Sync(ctx context.Context) error {
	if r == nil || r.source == nil || r.publisher == nil || ctx == nil || ctx.Err() != nil {
		return ErrRouteInvalid
	}
	current, err := r.source.ListPublishableRoutes(ctx, r.now())
	if err != nil {
		return err
	}
	live := make(map[string]Route, len(current))
	for _, item := range current {
		route := Route{
			SlotID: item.SlotID, NodeID: item.NodeID, Endpoint: item.Endpoint,
			Epoch: item.Epoch, Generation: item.Generation,
		}
		endpoint, err := EndpointFromLabels(map[string]string{DataplaneEndpointKey: item.Endpoint})
		if err != nil {
			continue
		}
		route.Endpoint = endpoint
		if err := r.publisher.Publish(ctx, route); err != nil {
			return err
		}
		live[route.SlotID] = route
	}
	for slotID, previous := range r.published {
		if _, ok := live[slotID]; ok {
			continue
		}
		if err := r.publisher.Unpublish(ctx, slotID, previous.Epoch, previous.Generation); err != nil && !errors.Is(err, ErrRouteStale) {
			return err
		}
	}
	r.published = live
	return nil
}
