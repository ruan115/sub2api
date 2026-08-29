package fake

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/contracttest"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

func spec() base.SlotSpec {
	return base.SlotSpec{
		SlotID:      "slot-1",
		AccountID:   "account-1",
		Epoch:       7,
		ImageDigest: "registry.example/execution-worker@sha256:" + strings.Repeat("a", 64),
		Resources: base.ResourceLimits{
			CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 128 << 20,
		},
		Security: base.SecurityPolicy{
			RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true,
			DropAllCapabilities: true, SeccompProfile: "worker", AppArmorProfile: "worker",
		},
		Network: base.NetworkPolicy{
			DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080",
		},
	}
}

func TestProviderLifecycleAndIdempotency(t *testing.T) {
	ctx := context.Background()
	p := New()

	instance, err := p.Create(ctx, spec())
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Create(ctx, spec())
	if err != nil || again.ProviderRef != instance.ProviderRef {
		t.Fatalf("idempotent create failed: instance=%+v err=%v", again, err)
	}

	if err := p.Start(ctx, instance.ProviderRef); err != nil {
		t.Fatal(err)
	}
	status, err := p.Inspect(ctx, instance.ProviderRef)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != slot.StateReady || !status.Healthy {
		t.Fatalf("unexpected started status: %+v", status)
	}

	if err := p.Drain(ctx, instance.ProviderRef, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx, instance.ProviderRef); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(ctx, instance.ProviderRef); err != nil {
		t.Fatal(err)
	}
	if err := p.Destroy(ctx, instance.ProviderRef); err != nil {
		t.Fatalf("destroy must be idempotent: %v", err)
	}
	if _, err := p.Inspect(ctx, instance.ProviderRef); !errors.Is(err, base.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestProviderContract(t *testing.T) {
	contracttest.Exercise(t, func() base.ExecutionProvider { return New() }, spec())
}

func TestProviderRejectsEpochReuse(t *testing.T) {
	ctx := context.Background()
	p := New()
	if _, err := p.Create(ctx, spec()); err != nil {
		t.Fatal(err)
	}
	newEpoch := spec()
	newEpoch.Epoch++
	if _, err := p.Create(ctx, newEpoch); err == nil {
		t.Fatal("expected epoch conflict")
	}
}
