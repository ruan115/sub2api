package route

import (
	"context"
	"testing"
	"time"
)

func TestReconcilerPublishesLiveRoutesAndUnpublishesDrained(t *testing.T) {
	store := newMemoryRouteStore()
	publisher, err := NewPublisher(store, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	source := &staticRouteSource{routes: []PublishableRoute{{
		SlotID: "slot-1", NodeID: "srv74", Endpoint: "10.8.0.12:8091", Epoch: 2, Generation: 5,
	}}}
	reconciler, err := NewReconciler(source, publisher, func() time.Time {
		return time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := publisher.Get(context.Background(), "slot-1")
	if err != nil || got.Epoch != 2 {
		t.Fatalf("published = %+v err=%v", got, err)
	}
	source.routes = nil
	if err := reconciler.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Get(context.Background(), "slot-1"); err != ErrRouteMissing {
		t.Fatalf("drained route still published: %v", err)
	}
}

type staticRouteSource struct {
	routes []PublishableRoute
}

func (s *staticRouteSource) ListPublishableRoutes(context.Context, time.Time) ([]PublishableRoute, error) {
	return append([]PublishableRoute(nil), s.routes...), nil
}
