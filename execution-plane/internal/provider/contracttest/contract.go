// Package contracttest contains the behavior every ExecutionProvider must
// satisfy. Provider implementations should invoke Exercise from their tests.
package contracttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

func Exercise(t *testing.T, factory func() provider.ExecutionProvider, spec provider.SlotSpec) {
	t.Helper()
	ctx := context.Background()
	implementation := factory()

	instance, err := implementation.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if instance.ProviderRef == "" || instance.SlotID != spec.SlotID || instance.Epoch != spec.Epoch {
		t.Fatalf("create returned invalid identity: %+v", instance)
	}
	repeated, err := implementation.Create(ctx, spec)
	if err != nil || repeated.ProviderRef != instance.ProviderRef {
		t.Fatalf("create is not idempotent: instance=%+v err=%v", repeated, err)
	}

	if err := implementation.Start(ctx, instance.ProviderRef); err != nil {
		t.Fatalf("start: %v", err)
	}
	status, err := implementation.Inspect(ctx, instance.ProviderRef)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if status.State != slot.StateReady || !status.Healthy {
		t.Fatalf("started instance is not healthy: %+v", status)
	}

	if err := implementation.Drain(ctx, instance.ProviderRef, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := implementation.Stop(ctx, instance.ProviderRef); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := implementation.Destroy(ctx, instance.ProviderRef); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if err := implementation.Destroy(ctx, instance.ProviderRef); err != nil {
		t.Fatalf("destroy is not idempotent: %v", err)
	}
	if _, err := implementation.Inspect(ctx, instance.ProviderRef); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("destroyed instance is still inspectable: %v", err)
	}
}
