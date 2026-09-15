package executionauthority

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

type bindingReader func(context.Context, string, time.Time, time.Duration) (store.ExecutionBinding, error)

func (f bindingReader) ReadExecutionBinding(ctx context.Context, slot string, now time.Time, age time.Duration) (store.ExecutionBinding, error) {
	return f(ctx, slot, now, age)
}

type sessionValidator func(context.Context, string, string) error

func (f sessionValidator) ValidateControlSession(ctx context.Context, node, session string) error {
	return f(ctx, node, session)
}

func candidate(now time.Time) store.ExecutionBinding {
	return store.ExecutionBinding{AccountID: "account-1", SlotID: "slot-1", NodeID: "node-1",
		ProviderRef: "container-1", LeaseOwnerID: "owner-1", ControlSessionID: strings.Repeat("a", 32),
		ImageDigest: "sha256:" + strings.Repeat("a", 64), ExecutionEpoch: 3, RouteGeneration: 5,
		ObservedAt: now.Add(-time.Second), NodeSeenAt: now, LeaseExpiresAt: now.Add(time.Minute)}
}

func TestSourcePreservesObservationAndUsesOnlyReadAuthority(t *testing.T) {
	now := time.Now().UTC()
	value := candidate(now)
	reads, sessions := 0, 0
	source, err := NewSource(Config{NodeID: value.NodeID, Now: func() time.Time { return now },
		Repository: bindingReader(func(_ context.Context, slot string, checked time.Time, age time.Duration) (store.ExecutionBinding, error) {
			reads++
			if slot != value.SlotID || !checked.Equal(now) || age != 45*time.Second {
				t.Fatal("reader input mismatch")
			}
			return value, nil
		}), Sessions: sessionValidator(func(_ context.Context, node, session string) error {
			sessions++
			if node != value.NodeID || session != value.ControlSessionID {
				t.Fatal("session check mismatch")
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, err := source.Snapshot(context.Background(), value.SlotID)
		if err != nil || !got.Ready || got.AccountID != value.AccountID || got.RouteGeneration != 5 || got.ExecutionEpoch != 3 ||
			got.ProviderRef != value.ProviderRef || got.LeaseOwnerID != value.LeaseOwnerID || !got.ObservedAt.Equal(value.ObservedAt) {
			t.Fatalf("projection=%+v error=%v", got, err)
		}
	}
	if reads != 2 || sessions != 2 {
		t.Fatalf("authority was cached: reads=%d sessions=%d", reads, sessions)
	}
}

func TestSourceRejectsInvalidStaleAndCrossNodeCandidates(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name   string
		change func(*store.ExecutionBinding)
	}{
		{"wrong slot", func(v *store.ExecutionBinding) { v.SlotID = "slot-other" }},
		{"wrong node", func(v *store.ExecutionBinding) { v.NodeID = "node-other" }},
		{"invalid account", func(v *store.ExecutionBinding) { v.AccountID = "" }},
		{"missing epoch", func(v *store.ExecutionBinding) { v.ExecutionEpoch = 0 }},
		{"missing generation", func(v *store.ExecutionBinding) { v.RouteGeneration = 0 }},
		{"missing owner", func(v *store.ExecutionBinding) { v.LeaseOwnerID = "" }},
		{"missing ref", func(v *store.ExecutionBinding) { v.ProviderRef = "" }},
		{"huge ref", func(v *store.ExecutionBinding) { v.ProviderRef = strings.Repeat("x", 257) }},
		{"legacy observation", func(v *store.ExecutionBinding) { v.ControlSessionID = "" }},
		{"zero observation", func(v *store.ExecutionBinding) { v.ObservedAt = time.Time{} }},
		{"old observation", func(v *store.ExecutionBinding) { v.ObservedAt = now.Add(-45 * time.Second) }},
		{"future observation", func(v *store.ExecutionBinding) { v.ObservedAt = now.Add(time.Nanosecond) }},
		{"zero heartbeat", func(v *store.ExecutionBinding) { v.NodeSeenAt = time.Time{} }},
		{"old heartbeat", func(v *store.ExecutionBinding) { v.NodeSeenAt = now.Add(-45 * time.Second) }},
		{"future heartbeat", func(v *store.ExecutionBinding) { v.NodeSeenAt = now.Add(time.Nanosecond) }},
		{"expired lease", func(v *store.ExecutionBinding) { v.LeaseExpiresAt = now }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := candidate(now)
			test.change(&value)
			source, err := NewSource(Config{NodeID: "node-1", Now: func() time.Time { return now },
				Repository: bindingReader(func(context.Context, string, time.Time, time.Duration) (store.ExecutionBinding, error) {
					return value, nil
				}),
				Sessions: sessionValidator(func(context.Context, string, string) error {
					t.Fatal("invalid candidate reached session gate")
					return nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := source.Snapshot(context.Background(), "slot-1")
			if err != dataplane.ErrBindingUnavailable || got != (dataplane.Snapshot{}) {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}

func TestSourceRechecksTimeAndCancellationAfterBlockingDependencies(t *testing.T) {
	for _, stage := range []string{"read stale", "session stale", "read lease", "session lease", "read cancel", "session cancel", "read error", "session error"} {
		t.Run(stage, func(t *testing.T) {
			now := time.Now().UTC()
			value := candidate(now)
			value.LeaseExpiresAt = now.Add(2 * time.Second)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mutate := func() error {
				switch {
				case strings.HasSuffix(stage, "stale"):
					now = now.Add(45 * time.Second)
				case strings.HasSuffix(stage, "lease"):
					now = value.LeaseExpiresAt
				case strings.HasSuffix(stage, "cancel"):
					cancel()
				case strings.HasSuffix(stage, "error"):
					return errors.New("private dependency detail")
				}
				return nil
			}
			source, err := NewSource(Config{NodeID: value.NodeID, Now: func() time.Time { return now },
				Repository: bindingReader(func(context.Context, string, time.Time, time.Duration) (store.ExecutionBinding, error) {
					if strings.HasPrefix(stage, "read") {
						return value, mutate()
					}
					return value, nil
				}), Sessions: sessionValidator(func(context.Context, string, string) error {
					if strings.HasPrefix(stage, "session") {
						return mutate()
					}
					return nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := source.Snapshot(ctx, value.SlotID)
			if err != dataplane.ErrBindingUnavailable || got != (dataplane.Snapshot{}) {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}

func TestSourceConfigurationAndCancelledRequest(t *testing.T) {
	read := bindingReader(func(context.Context, string, time.Time, time.Duration) (store.ExecutionBinding, error) {
		t.Fatal("unexpected read")
		return store.ExecutionBinding{}, nil
	})
	sessions := sessionValidator(func(context.Context, string, string) error { t.Fatal("unexpected session check"); return nil })
	for _, config := range []Config{{}, {NodeID: "node-1", Repository: read}, {NodeID: "node-1", Sessions: sessions},
		{NodeID: "node-1", Repository: read, Sessions: sessions, MaxSnapshotAge: 46 * time.Second},
		{NodeID: "node-1", Repository: read, Sessions: sessions, MaxSnapshotAge: -time.Second}} {
		if _, err := NewSource(config); err != dataplane.ErrBindingUnavailable {
			t.Fatalf("error=%v", err)
		}
	}
	source, err := NewSource(Config{NodeID: "node-1", Repository: read, Sessions: sessions, MaxSnapshotAge: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, ctx := range []context.Context{nil, ctx} {
		if _, err := source.Snapshot(ctx, "slot-1"); err != dataplane.ErrBindingUnavailable {
			t.Fatal(err)
		}
	}
	if _, err := source.Snapshot(context.Background(), ""); err != dataplane.ErrBindingUnavailable {
		t.Fatal(err)
	}
	var absent *Source
	if _, err := absent.Snapshot(context.Background(), "slot-1"); err != dataplane.ErrBindingUnavailable {
		t.Fatal(err)
	}
}

type lookup func(context.Context, dataplane.Binding, string) (dataplane.Runtime, error)

func (f lookup) LookupRuntime(ctx context.Context, b dataplane.Binding, ref string) (dataplane.Runtime, error) {
	return f(ctx, b, ref)
}

type inertRuntime struct{}

func (inertRuntime) OpenExecution(context.Context, *executionv1.BeginExecution) (dataplane.Execution, error) {
	return nil, errors.New("unused")
}
func (inertRuntime) CountTokensRequest(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	return nil, errors.New("unused")
}

func TestSourceWithRealFencedResolverNeverResurrectsReplacedSession(t *testing.T) {
	now := time.Now().UTC()
	value := candidate(now)
	activeSession := value.ControlSessionID
	source, err := NewSource(Config{NodeID: value.NodeID, Now: func() time.Time { return now },
		Repository: bindingReader(func(context.Context, string, time.Time, time.Duration) (store.ExecutionBinding, error) {
			return value, nil
		}),
		Sessions: sessionValidator(func(_ context.Context, node, id string) error {
			if id != activeSession {
				return errors.New("disconnected")
			}
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := lease.NewMemoryBackend(func() time.Time { return now })
	claim := lease.Claim{SlotID: value.SlotID, NodeID: value.NodeID, ExecutionEpoch: value.ExecutionEpoch, OwnerID: value.LeaseOwnerID}
	if err := backend.Acquire(context.Background(), claim, time.Minute); err != nil {
		t.Fatal(err)
	}
	calls := 0
	mutate := false
	replaceWith := strings.Repeat("b", 32)
	confirmReplacement := false
	resolver, err := dataplane.NewFencedResolver(dataplane.FencedResolverConfig{NodeID: value.NodeID, Source: source, Leases: backend,
		Now: func() time.Time { return now }, Runtimes: lookup(func(context.Context, dataplane.Binding, string) (dataplane.Runtime, error) {
			calls++
			if mutate {
				activeSession = replaceWith
				if confirmReplacement {
					value.ControlSessionID = activeSession
				}
			}
			return inertRuntime{}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := dataplane.Binding{AccountID: value.AccountID, SlotID: value.SlotID, NodeID: value.NodeID, ExecutionEpoch: value.ExecutionEpoch, RouteGeneration: value.RouteGeneration}
	if _, err := resolver.Resolve(context.Background(), binding); err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	for _, change := range []func(*dataplane.Binding){func(b *dataplane.Binding) { b.AccountID = "other" }, func(b *dataplane.Binding) { b.NodeID = "other" }, func(b *dataplane.Binding) { b.ExecutionEpoch++ }, func(b *dataplane.Binding) { b.RouteGeneration++ }} {
		other := binding
		change(&other)
		if _, err := resolver.Resolve(context.Background(), other); err != dataplane.ErrBindingUnavailable || calls != 1 {
			t.Fatalf("err=%v calls=%d", err, calls)
		}
	}
	backend.SetAvailable(false)
	if _, err := resolver.Resolve(context.Background(), binding); err != dataplane.ErrBindingUnavailable || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	backend.SetAvailable(true)
	mutate = true
	if _, err := resolver.Resolve(context.Background(), binding); err != dataplane.ErrBindingUnavailable || calls != 2 {
		t.Fatalf("lookup replacement err=%v calls=%d", err, calls)
	}
	if _, err := resolver.Resolve(context.Background(), binding); err != dataplane.ErrBindingUnavailable || calls != 2 {
		t.Fatalf("old observation resurrected: err=%v calls=%d", err, calls)
	}
	// Even if a new session has already obtained its own fresh observation,
	// an in-flight lookup begun in the previous session is not reusable.
	value.ControlSessionID = activeSession
	replaceWith, confirmReplacement = strings.Repeat("c", 32), true
	if _, err := resolver.Resolve(context.Background(), binding); err != dataplane.ErrBindingUnavailable || calls != 3 {
		t.Fatalf("reconfirmed session replacement: err=%v calls=%d", err, calls)
	}
}
