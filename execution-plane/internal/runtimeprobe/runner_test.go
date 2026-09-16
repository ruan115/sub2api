package runtimeprobe

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

type repositoryStub struct {
	list func(context.Context, string, time.Time, time.Duration, int) (store.ProbeBindingPage, error)
	read func(context.Context, string, time.Time, time.Duration) (store.ProbeBinding, error)
}

func (s repositoryStub) ListProbeBindings(c context.Context, a string, n time.Time, d time.Duration, l int) (store.ProbeBindingPage, error) {
	return s.list(c, a, n, d, l)
}
func (s repositoryStub) ReadProbeBinding(c context.Context, id string, n time.Time, d time.Duration) (store.ProbeBinding, error) {
	return s.read(c, id, n, d)
}

type sessionFunc func(context.Context, string, string) error

func (f sessionFunc) ValidateControlSession(c context.Context, n, s string) error { return f(c, n, s) }

type leaseFunc func(context.Context, lease.Claim) error

func (f leaseFunc) Validate(c context.Context, claim lease.Claim) error { return f(c, claim) }

type dispatchFunc func(context.Context, string, string, *executionv1.NodeControlServiceControlResponse) error

func (f dispatchFunc) DispatchToSession(c context.Context, n, s string, v *executionv1.NodeControlServiceControlResponse) error {
	return f(c, n, s, v)
}

func testBinding(now time.Time) store.ProbeBinding {
	return store.ProbeBinding{AccountID: "account-1", SlotID: "slot-1", NodeID: "node-1", ProviderRef: "container-1", LeaseOwnerID: "owner-1",
		ControlSessionID: strings.Repeat("a", 32), ImageDigest: "sha256:" + strings.Repeat("a", 64), ExecutionEpoch: 1, RouteGeneration: 2,
		NodeSeenAt: now, LeaseExpiresAt: now.Add(time.Minute)}
}

func testConfig(now time.Time, b store.ProbeBinding, calls *[]*executionv1.SlotCommand) Config {
	return Config{Now: func() time.Time { return now }, Repository: repositoryStub{
		list: func(context.Context, string, time.Time, time.Duration, int) (store.ProbeBindingPage, error) {
			return store.ProbeBindingPage{Bindings: []store.ProbeBinding{b}}, nil
		},
		read: func(context.Context, string, time.Time, time.Duration) (store.ProbeBinding, error) { return b, nil },
	}, Sessions: sessionFunc(func(context.Context, string, string) error { return nil }), Leases: leaseFunc(func(context.Context, lease.Claim) error { return nil }),
		Dispatcher: dispatchFunc(func(ctx context.Context, node, session string, v *executionv1.NodeControlServiceControlResponse) error {
			if ctx.Err() != nil || node != b.NodeID || session != b.ControlSessionID {
				return errors.New("invalid test dispatch")
			}
			*calls = append(*calls, v.GetSlotCommand())
			return nil
		})}
}

func TestProbeUsesExactBindingFreshNonceAndLeaseCappedDeadline(t *testing.T) {
	now := time.Now().UTC()
	b := testBinding(now)
	b.LeaseExpiresAt = now.Add(3 * time.Second)
	var commands []*executionv1.SlotCommand
	c := testConfig(now, b, &commands)
	validations := 0
	c.Leases = leaseFunc(func(_ context.Context, claim lease.Claim) error {
		validations++
		if claim != (lease.Claim{SlotID: b.SlotID, NodeID: b.NodeID, ExecutionEpoch: b.ExecutionEpoch, OwnerID: b.LeaseOwnerID}) {
			t.Fatal(claim)
		}
		return nil
	})
	r, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := r.Step(context.Background())
		if err != nil || result.Dispatched != 1 {
			t.Fatal(result, err)
		}
	}
	if len(commands) != 2 || commands[0].CommandId == commands[1].CommandId || validations != 4 {
		t.Fatal("rounds reused authority or command id")
	}
	for _, cmd := range commands {
		if cmd.Action != executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT || cmd.AccountId != b.AccountID || cmd.SlotId != b.SlotID || cmd.ExecutionEpoch != b.ExecutionEpoch || cmd.ImageDigest != b.ImageDigest || cmd.Metadata["desired_generation"] != "2" || !cmd.Deadline.AsTime().Equal(b.LeaseExpiresAt) {
			t.Fatal(cmd)
		}
	}
}

func TestProbeDueDoesNotTrustHeartbeatOrOldSession(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name    string
		ago     time.Duration
		session string
		want    int
	}{
		{"recent current", time.Second, strings.Repeat("a", 32), 0},
		{"exact interval", 15 * time.Second, strings.Repeat("a", 32), 1},
		{"old session", time.Second, strings.Repeat("b", 32), 1},
		{"no proof", time.Second, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := testBinding(now)
			at := now.Add(-tc.ago)
			b.LastObservedAt = &at
			b.ObservedControlSessionID = tc.session
			var commands []*executionv1.SlotCommand
			r, _ := New(testConfig(now, b, &commands))
			got, err := r.Step(context.Background())
			if err != nil || got.Dispatched != tc.want || len(commands) != tc.want {
				t.Fatal(got, err)
			}
		})
	}
}

func TestProbeRejectsChangesAndDependencyFailures(t *testing.T) {
	now := time.Now().UTC()
	for _, name := range []string{"session unavailable", "lease unavailable", "lease revoked during read", "read failure", "account", "slot", "node", "session", "epoch", "generation", "image", "provider", "owner", "lease expires during read", "node expires during read", "context expires during read", "future observation"} {
		t.Run(name, func(t *testing.T) {
			b := testBinding(now)
			var commands []*executionv1.SlotCommand
			c := testConfig(now, b, &commands)
			current := now
			c.Now = func() time.Time { return current }
			backend := lease.NewMemoryBackend(func() time.Time { return current })
			claim := lease.Claim{SlotID: b.SlotID, NodeID: b.NodeID, ExecutionEpoch: b.ExecutionEpoch, OwnerID: b.LeaseOwnerID}
			if err := backend.Acquire(context.Background(), claim, time.Minute); err != nil {
				t.Fatal(err)
			}
			c.Leases = backend
			repo := c.Repository.(repositoryStub)
			switch name {
			case "session unavailable":
				c.Sessions = sessionFunc(func(context.Context, string, string) error { return errors.New("sensitive dependency detail") })
			case "lease unavailable":
				backend.SetAvailable(false)
			default:
				repo.read = func(ctx context.Context, _ string, _ time.Time, _ time.Duration) (store.ProbeBinding, error) {
					fresh := b
					switch name {
					case "lease revoked during read":
						_ = backend.Revoke(ctx, claim)
					case "read failure":
						return store.ProbeBinding{}, errors.New("sensitive dependency detail")
					case "account":
						fresh.AccountID = "other-account"
					case "slot":
						fresh.SlotID = "other-slot"
					case "node":
						fresh.NodeID = "other-node"
					case "session":
						fresh.ControlSessionID = strings.Repeat("b", 32)
					case "epoch":
						fresh.ExecutionEpoch++
					case "generation":
						fresh.RouteGeneration++
					case "image":
						fresh.ImageDigest = "sha256:" + strings.Repeat("b", 64)
					case "provider":
						fresh.ProviderRef = "other-runtime"
					case "owner":
						fresh.LeaseOwnerID = "other-owner"
					case "lease expires during read":
						current = now.Add(time.Minute)
					case "node expires during read":
						current = now.Add(45 * time.Second)
					case "context expires during read":
						<-ctx.Done()
					case "future observation":
						future := now.Add(time.Second)
						fresh.LastObservedAt = &future
					}
					return fresh, nil
				}
			}
			c.Repository = repo
			if name == "context expires during read" {
				c.CheckTimeout = time.Millisecond
			}
			r, _ := New(c)
			got, err := r.Step(context.Background())
			if err != nil || got.Denied != 1 || len(commands) != 0 {
				t.Fatal(got, err)
			}
		})
	}
}

func TestProbeSkipsProofThatArrivesDuringRead(t *testing.T) {
	now := time.Now().UTC()
	b := testBinding(now)
	var commands []*executionv1.SlotCommand
	c := testConfig(now, b, &commands)
	repo := c.Repository.(repositoryStub)
	repo.read = func(context.Context, string, time.Time, time.Duration) (store.ProbeBinding, error) {
		b.LastObservedAt = &now
		b.ObservedControlSessionID = b.ControlSessionID
		return b, nil
	}
	c.Repository = repo
	r, _ := New(c)
	got, err := r.Step(context.Background())
	if err != nil || got.Skipped != 1 || len(commands) != 0 {
		t.Fatal(got, err)
	}
}

func TestProbePageCursorProgressesAcrossEmptyFilteredPageAndDeniedNode(t *testing.T) {
	now := time.Now().UTC()
	b := testBinding(now)
	var commands []*executionv1.SlotCommand
	c := testConfig(now, b, &commands)
	c.BatchSize = 1
	var cursors []string
	repo := c.Repository.(repositoryStub)
	repo.list = func(_ context.Context, after string, _ time.Time, _ time.Duration, limit int) (store.ProbeBindingPage, error) {
		if limit != 1 {
			t.Fatal(limit)
		}
		cursors = append(cursors, after)
		switch after {
		case "":
			return store.ProbeBindingPage{NextAfterSlotID: "slot-0"}, nil
		case "slot-0":
			return store.ProbeBindingPage{Bindings: []store.ProbeBinding{b}, NextAfterSlotID: b.SlotID}, nil
		default:
			return store.ProbeBindingPage{}, nil
		}
	}
	c.Repository = repo
	c.Sessions = sessionFunc(func(context.Context, string, string) error { return errors.New("remote orchestrator") })
	r, _ := New(c)
	for range 4 {
		if _, err := r.Step(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(cursors, ",") != ",slot-0,slot-1," || len(commands) != 0 {
		t.Fatal(cursors)
	}
}

func TestProbeRejectsMalformedPaginationWithoutDispatch(t *testing.T) {
	now := time.Now().UTC()
	b := testBinding(now)
	for _, page := range []store.ProbeBindingPage{{Bindings: []store.ProbeBinding{b, b}}, {Bindings: []store.ProbeBinding{b}, NextAfterSlotID: "slot-0"}, {NextAfterSlotID: "bad\nkey"}} {
		var commands []*executionv1.SlotCommand
		c := testConfig(now, b, &commands)
		repo := c.Repository.(repositoryStub)
		repo.list = func(context.Context, string, time.Time, time.Duration, int) (store.ProbeBindingPage, error) {
			return page, nil
		}
		c.Repository = repo
		r, _ := New(c)
		if _, err := r.Step(context.Background()); err != ErrScan || len(commands) != 0 {
			t.Fatal(err)
		}
	}
}

func TestProbeRunFailureBudgetCancellationAndSingleStep(t *testing.T) {
	now := time.Now().UTC()
	b := testBinding(now)
	var commands []*executionv1.SlotCommand
	c := testConfig(now, b, &commands)
	var scans atomic.Int64
	c.PollInterval = time.Millisecond
	c.MaxConsecutiveFailures = 3
	repo := c.Repository.(repositoryStub)
	repo.list = func(context.Context, string, time.Time, time.Duration, int) (store.ProbeBindingPage, error) {
		scans.Add(1)
		return store.ProbeBindingPage{}, errors.New("sensitive scan detail")
	}
	c.Repository = repo
	r, _ := New(c)
	if err := r.Run(context.Background()); err != ErrScan || scans.Load() != 3 {
		t.Fatal(err, scans.Load())
	}
	entered := make(chan struct{})
	repo.list = func(ctx context.Context, _ string, _ time.Time, _ time.Duration, _ int) (store.ProbeBindingPage, error) {
		close(entered)
		<-ctx.Done()
		return store.ProbeBindingPage{}, ctx.Err()
	}
	c.Repository = repo
	r, _ = New(c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	<-entered
	if _, err := r.Step(context.Background()); err != ErrBusy {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop scan")
	}
}

func TestProbeScanDeadlineAndInvalidConfig(t *testing.T) {
	now := time.Now().UTC()
	b := testBinding(now)
	var commands []*executionv1.SlotCommand
	c := testConfig(now, b, &commands)
	c.CheckTimeout = time.Millisecond
	repo := c.Repository.(repositoryStub)
	repo.list = func(ctx context.Context, _ string, _ time.Time, _ time.Duration, _ int) (store.ProbeBindingPage, error) {
		<-ctx.Done()
		return store.ProbeBindingPage{}, nil
	}
	c.Repository = repo
	r, _ := New(c)
	if _, err := r.Step(context.Background()); err != ErrScan {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Repository = nil }, func(c *Config) { c.Sessions = nil },
		func(c *Config) { c.Leases = nil }, func(c *Config) { c.Dispatcher = nil },
		func(c *Config) { c.BatchSize = 101 }, func(c *Config) { c.CommandTTL = 11 * time.Second },
		func(c *Config) { c.MaxNodeAge = 46 * time.Second }, func(c *Config) { c.RefreshInterval = 45 * time.Second },
		func(c *Config) { c.CheckTimeout = -time.Second },
	} {
		cfg := testConfig(now, b, &commands)
		mutate(&cfg)
		if _, err := New(cfg); err != ErrConfiguration {
			t.Fatal("invalid config accepted", cfg)
		}
	}
}

func TestProbeCanceledAuthorityCheckCannotDispatch(t *testing.T) {
	now := time.Now().UTC()
	b := testBinding(now)
	var commands []*executionv1.SlotCommand
	c := testConfig(now, b, &commands)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Sessions = sessionFunc(func(context.Context, string, string) error { cancel(); return nil })
	c.Leases = leaseFunc(func(context.Context, lease.Claim) error { t.Fatal("lease called after cancellation"); return nil })
	r, _ := New(c)
	if result, err := r.Step(ctx); !errors.Is(err, context.Canceled) || result.Dispatched != 0 || len(commands) != 0 {
		t.Fatal(result, err)
	}
}

func TestProbeMalformedProjectionCannotDispatch(t *testing.T) {
	now := time.Now().UTC()
	for _, mutate := range []func(*store.ProbeBinding){
		func(b *store.ProbeBinding) { b.AccountID = "bad\naccount" },
		func(b *store.ProbeBinding) { b.ControlSessionID = "not-a-session" },
		func(b *store.ProbeBinding) { b.ImageDigest = "latest" },
		func(b *store.ProbeBinding) { b.ExecutionEpoch = 0 },
		func(b *store.ProbeBinding) { b.RouteGeneration = 0 },
		func(b *store.ProbeBinding) { b.ProviderRef = "" },
		func(b *store.ProbeBinding) { b.LeaseExpiresAt = now },
		func(b *store.ProbeBinding) { b.NodeSeenAt = now.Add(-45 * time.Second) },
		func(b *store.ProbeBinding) { b.NodeSeenAt = now.Add(time.Second) },
	} {
		b := testBinding(now)
		mutate(&b)
		var commands []*executionv1.SlotCommand
		r, _ := New(testConfig(now, b, &commands))
		if result, err := r.Step(context.Background()); err != nil || result.Denied != 1 || len(commands) != 0 {
			t.Fatal(result, err)
		}
	}
}
