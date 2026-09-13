package route

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunnerPublishesUntilCanceled(t *testing.T) {
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
	runner, err := NewRunner(reconciler, 10*time.Millisecond, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := publisher.Get(context.Background(), "slot-1")
		if err == nil && got.Epoch == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("route was not published: %+v err=%v", got, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("route runner did not stop")
	}
}

func TestRunnerReportsTransientSyncErrors(t *testing.T) {
	store := newMemoryRouteStore()
	publisher, err := NewPublisher(store, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	source := &failingRouteSource{err: errors.New("mysql unavailable")}
	reconciler, err := NewReconciler(source, publisher, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var failures atomic.Int32
	runner, err := NewRunner(reconciler, 10*time.Millisecond, func(error) { failures.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for failures.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("route runner did not report a sync error")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("failed route runner did not stop")
	}
}

type failingRouteSource struct {
	err error
}

func (s *failingRouteSource) ListPublishableRoutes(context.Context, time.Time) ([]PublishableRoute, error) {
	return nil, s.err
}
