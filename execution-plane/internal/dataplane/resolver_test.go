package dataplane

import (
	"context"
	"errors"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

type snapshotFunc func(context.Context, string) (Snapshot, error)

func (f snapshotFunc) Snapshot(ctx context.Context, slot string) (Snapshot, error) {
	return f(ctx, slot)
}

type lookupFunc func(context.Context, Binding, string) (Runtime, error)

func (f lookupFunc) LookupRuntime(ctx context.Context, binding Binding, ref string) (Runtime, error) {
	return f(ctx, binding, ref)
}

type inertRuntime struct{}

type leaseValidateFunc func(context.Context, lease.Claim) error

func (f leaseValidateFunc) Validate(ctx context.Context, claim lease.Claim) error {
	return f(ctx, claim)
}

func (inertRuntime) OpenExecution(context.Context, *executionv1.BeginExecution) (Execution, error) {
	return nil, errors.New("unused")
}
func (inertRuntime) CountTokensRequest(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	return nil, errors.New("unused")
}

func TestFencedResolverRejectsUntrustedOrStaleSnapshotBeforeLookup(t *testing.T) {
	now := time.Now().UTC()
	binding := Binding{AccountID: "account-1", SlotID: "slot-1", NodeID: "node-1", ExecutionEpoch: 3, RouteGeneration: 4}
	base := Snapshot{Binding: binding, ProviderRef: "container-ref", LeaseOwnerID: "lease-owner", Ready: true, ObservedAt: now}
	for _, test := range []struct {
		name   string
		change func(*Snapshot)
	}{
		{"account", func(s *Snapshot) { s.AccountID = "account-2" }},
		{"slot", func(s *Snapshot) { s.SlotID = "slot-2" }},
		{"node", func(s *Snapshot) { s.NodeID = "node-2" }},
		{"epoch", func(s *Snapshot) { s.ExecutionEpoch++ }},
		{"generation", func(s *Snapshot) { s.RouteGeneration++ }},
		{"not ready", func(s *Snapshot) { s.Ready = false }},
		{"no observation", func(s *Snapshot) { s.ObservedAt = time.Time{} }},
		{"stale", func(s *Snapshot) { s.ObservedAt = now.Add(-45 * time.Second) }},
		{"future", func(s *Snapshot) { s.ObservedAt = now.Add(time.Second) }},
		{"no runtime", func(s *Snapshot) { s.ProviderRef = "" }},
		{"no owner", func(s *Snapshot) { s.LeaseOwnerID = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := base
			test.change(&current)
			backend := lease.NewMemoryBackend(func() time.Time { return now })
			lookups := 0
			resolver, err := NewFencedResolver(FencedResolverConfig{NodeID: binding.NodeID,
				Source: snapshotFunc(func(context.Context, string) (Snapshot, error) { return current, nil }), Leases: backend,
				Runtimes: lookupFunc(func(context.Context, Binding, string) (Runtime, error) { lookups++; return inertRuntime{}, nil }),
				Now:      func() time.Time { return now },
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.Resolve(context.Background(), binding); !errors.Is(err, ErrBindingUnavailable) || lookups != 0 {
				t.Fatalf("err=%v lookups=%d", err, lookups)
			}
		})
	}
}

func TestFencedResolverRequiresCurrentLeaseAndRechecksLookup(t *testing.T) {
	now := time.Now().UTC()
	binding := Binding{AccountID: "account-1", SlotID: "slot-1", NodeID: "node-1", ExecutionEpoch: 3, RouteGeneration: 4}
	current := Snapshot{Binding: binding, ProviderRef: "ref-1", LeaseOwnerID: "owner-1", Ready: true, ObservedAt: now}
	backend := lease.NewMemoryBackend(func() time.Time { return now })
	claim := lease.Claim{SlotID: binding.SlotID, NodeID: binding.NodeID, ExecutionEpoch: binding.ExecutionEpoch, OwnerID: current.LeaseOwnerID}
	lookups := 0
	mutate := false
	resolver, err := NewFencedResolver(FencedResolverConfig{NodeID: binding.NodeID,
		Source: snapshotFunc(func(context.Context, string) (Snapshot, error) { return current, nil }), Leases: backend,
		Runtimes: lookupFunc(func(_ context.Context, b Binding, ref string) (Runtime, error) {
			lookups++
			if b != binding || ref != "ref-1" {
				t.Fatal("lookup binding changed")
			}
			if mutate {
				current.RouteGeneration++
			}
			return inertRuntime{}, nil
		}), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(context.Background(), binding); err == nil || lookups != 0 {
		t.Fatal("missing lease was accepted")
	}
	if err = backend.Acquire(context.Background(), claim, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err = resolver.Resolve(context.Background(), binding); err != nil || lookups != 1 {
		t.Fatalf("current binding: %v", err)
	}
	wrongNode := binding
	wrongNode.NodeID = "node-other"
	if err = resolver.Validate(context.Background(), wrongNode); err == nil {
		t.Fatal("wrong host accepted")
	}
	backend.SetAvailable(false)
	if err = resolver.Validate(context.Background(), binding); err == nil {
		t.Fatal("lease backend failure accepted")
	}
	backend.SetAvailable(true)
	mutate = true
	if _, err = resolver.Resolve(context.Background(), binding); err == nil {
		t.Fatal("lookup race accepted")
	}
	current.Binding = binding
	if err = backend.Revoke(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err = resolver.Validate(context.Background(), binding); err == nil {
		t.Fatal("revoked lease accepted")
	}
}

func TestFencedResolverConfigurationAndSourceFailure(t *testing.T) {
	if _, err := NewFencedResolver(FencedResolverConfig{}); err == nil {
		t.Fatal("missing dependencies accepted")
	}
	resolver, err := NewFencedResolver(FencedResolverConfig{NodeID: "node-1", Source: snapshotFunc(func(context.Context, string) (Snapshot, error) {
		return Snapshot{}, errors.New("private storage detail")
	}), Leases: lease.NewMemoryBackend(nil), Runtimes: lookupFunc(func(context.Context, Binding, string) (Runtime, error) { t.Fatal("lookup on failure"); return nil, nil })})
	if err != nil {
		t.Fatal(err)
	}
	if err = resolver.Validate(context.Background(), Binding{AccountID: "a", SlotID: "s", NodeID: "node-1", ExecutionEpoch: 1, RouteGeneration: 1}); err != ErrBindingUnavailable {
		t.Fatalf("error leaked: %v", err)
	}
}

func TestFencedResolverRechecksFreshnessAfterLeaseIO(t *testing.T) {
	for _, operation := range []string{"validate", "resolve"} {
		t.Run(operation, func(t *testing.T) {
			now := time.Now().UTC()
			binding := Binding{AccountID: "account-1", SlotID: "slot-1", NodeID: "node-1", ExecutionEpoch: 1, RouteGeneration: 1}
			current := Snapshot{Binding: binding, ProviderRef: "ref-1", LeaseOwnerID: "owner-1", Ready: true, ObservedAt: now}
			leaseCalls := 0
			resolver, err := NewFencedResolver(FencedResolverConfig{
				NodeID: binding.NodeID, Now: func() time.Time { return now },
				Source: snapshotFunc(func(context.Context, string) (Snapshot, error) { return current, nil }),
				Leases: leaseValidateFunc(func(context.Context, lease.Claim) error {
					leaseCalls++
					if operation == "validate" || leaseCalls == 2 {
						now = now.Add(45 * time.Second)
					}
					return nil
				}),
				Runtimes: lookupFunc(func(context.Context, Binding, string) (Runtime, error) { return inertRuntime{}, nil }),
			})
			if err != nil {
				t.Fatal(err)
			}
			if operation == "validate" {
				err = resolver.Validate(context.Background(), binding)
			} else {
				_, err = resolver.Resolve(context.Background(), binding)
			}
			if !errors.Is(err, ErrBindingUnavailable) {
				t.Fatalf("expired observation was returned: %v", err)
			}
		})
	}
}
