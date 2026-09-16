package workerproof

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

type bindingReader func(context.Context, string, time.Time, time.Duration) (store.WorkerReadinessBinding, error)

func (f bindingReader) ReadWorkerReadinessBinding(c context.Context, s string, n time.Time, a time.Duration) (store.WorkerReadinessBinding, error) {
	return f(c, s, n, a)
}

type sessionsFunc func(context.Context, string, string) error

func (f sessionsFunc) ValidateControlSession(c context.Context, n, s string) error { return f(c, n, s) }

type leasesFunc func(context.Context, lease.Claim) error

func (f leasesFunc) Validate(c context.Context, l lease.Claim) error { return f(c, l) }

type healthFunc func(context.Context, store.WorkerReadinessBinding, string) (*executionv1.HealthResponse, error)

func (f healthFunc) ReadHealth(c context.Context, b store.WorkerReadinessBinding, n string) (*executionv1.HealthResponse, error) {
	return f(c, b, n)
}

func candidate(now time.Time) store.WorkerReadinessBinding {
	return store.WorkerReadinessBinding{ExecutionBinding: store.ExecutionBinding{AccountID: "account-1", SlotID: "slot-1", NodeID: "node-1", ProviderRef: "container-1", LeaseOwnerID: "owner-1",
		ControlSessionID: strings.Repeat("a", 32), ImageDigest: "sha256:" + strings.Repeat("a", 64), ExecutionEpoch: 1, RouteGeneration: 2,
		ObservedAt: now.Add(-time.Second), NodeSeenAt: now, LeaseExpiresAt: now.Add(time.Minute)}, CredentialVersionID: "version-1", CredentialVersionNumber: 3, AuthType: "oauth", ProxyLeaseID: "proxy-1", ProxyReservationID: "reservation-1", ProxyBindingID: "123", ProxyBindingRevision: 4}
}
func binding(b store.WorkerReadinessBinding) dataplane.Binding {
	return dataplane.Binding{AccountID: b.AccountID, SlotID: b.SlotID, NodeID: b.NodeID, ExecutionEpoch: b.ExecutionEpoch, RouteGeneration: b.RouteGeneration}
}
func healthy(b store.WorkerReadinessBinding, nonce string) *executionv1.HealthResponse {
	return &executionv1.HealthResponse{SlotId: b.SlotID, ExecutionEpoch: b.ExecutionEpoch, ImageDigest: b.ImageDigest, Challenge: nonce,
		LoadedState: &executionv1.LoadedRuntimeState{AccountBinding: provider.RuntimeAccountID(b.AccountID), NodeId: b.NodeID, CredentialVersionId: b.CredentialVersionID, AuthType: b.AuthType, ProxyLeaseId: b.ProxyLeaseID, ActivationRevision: 5},
		Modes:       []*executionv1.ModeHealth{{Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE, ReasonCode: "not_implemented"}, {Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, Healthy: true}}}
}
func configFor(now time.Time, b store.WorkerReadinessBinding) Config {
	return Config{Now: func() time.Time { return now },
		Repository: bindingReader(func(context.Context, string, time.Time, time.Duration) (store.WorkerReadinessBinding, error) {
			return b, nil
		}),
		Sessions: sessionsFunc(func(context.Context, string, string) error { return nil }), Leases: leasesFunc(func(context.Context, lease.Claim) error { return nil }),
		Health: healthFunc(func(_ context.Context, b store.WorkerReadinessBinding, nonce string) (*executionv1.HealthResponse, error) {
			return healthy(b, nonce), nil
		})}
}
func requireDenied(t *testing.T, receipt Receipt, err error) {
	t.Helper()
	if receipt != (Receipt{}) || err != ErrUnavailable {
		t.Fatalf("expected empty denial, got %+v/%v", receipt, err)
	}
}

func TestLoadedProofFreshChallengeAndFrozenCheckTime(t *testing.T) {
	now := time.Now().UTC()
	b := candidate(now)
	c := configFor(now, b)
	current := now
	c.Now = func() time.Time { return current }
	reads, sessions, leases := 0, 0, 0
	var nonces []string
	var last *executionv1.HealthResponse
	c.Repository = bindingReader(func(_ context.Context, id string, checked time.Time, age time.Duration) (store.WorkerReadinessBinding, error) {
		reads++
		if id != b.SlotID || age != 45*time.Second {
			t.Fatal("wrong query")
		}
		if reads%2 == 0 {
			current = current.Add(100 * time.Millisecond)
			last.LoadedState.ActivationRevision = 0
		}
		return b, nil
	})
	c.Sessions = sessionsFunc(func(_ context.Context, node, session string) error {
		sessions++
		if node != b.NodeID || session != b.ControlSessionID {
			t.Fatal("wrong session")
		}
		return nil
	})
	c.Leases = leasesFunc(func(_ context.Context, claim lease.Claim) error {
		leases++
		if claim != (lease.Claim{SlotID: b.SlotID, NodeID: b.NodeID, ExecutionEpoch: b.ExecutionEpoch, OwnerID: b.LeaseOwnerID}) {
			t.Fatal("wrong claim")
		}
		return nil
	})
	c.Health = healthFunc(func(ctx context.Context, authority store.WorkerReadinessBinding, nonce string) (*executionv1.HealthResponse, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded health")
		}
		if !sessionPattern.MatchString(nonce) {
			t.Fatal("invalid challenge")
		}
		nonces = append(nonces, nonce)
		last = healthy(authority, nonce)
		return last, nil
	})
	v, _ := New(c)
	for range 2 {
		started := current
		receipt, err := v.Check(context.Background(), binding(b), executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API)
		if err != nil || receipt.Authority != b || receipt.ActivationRevision != 5 || !receipt.CheckedAt.Equal(started) {
			t.Fatal(receipt, err)
		}
	}
	if reads != 4 || sessions != 4 || leases != 4 || len(nonces) != 2 || nonces[0] == nonces[1] {
		t.Fatal("authority was cached or challenge reused")
	}
}

func TestLoadedProofRejectsMalformedStaleOrLegacyWorkerReports(t *testing.T) {
	now := time.Now().UTC()
	b := candidate(now)
	for _, name := range []string{"nil", "legacy", "challenge", "slot", "epoch", "image", "raw account", "node", "version", "auth", "proxy", "revision", "unhealthy", "missing mode", "duplicate mode", "unknown mode", "nil mode", "healthy reason", "oversize", "read error"} {
		t.Run(name, func(t *testing.T) {
			c := configFor(now, b)
			c.Health = healthFunc(func(_ context.Context, _ store.WorkerReadinessBinding, nonce string) (*executionv1.HealthResponse, error) {
				r := healthy(b, nonce)
				switch name {
				case "nil":
					return nil, nil
				case "legacy":
					r.LoadedState = nil
				case "challenge":
					r.Challenge = strings.Repeat("b", 32)
				case "slot":
					r.SlotId = "other-slot"
				case "epoch":
					r.ExecutionEpoch++
				case "image":
					r.ImageDigest = "sha256:" + strings.Repeat("b", 64)
				case "raw account":
					r.LoadedState.AccountBinding = b.AccountID
				case "node":
					r.LoadedState.NodeId = "other-node"
				case "version":
					r.LoadedState.CredentialVersionId = "version-old"
				case "auth":
					r.LoadedState.AuthType = "api_key"
				case "proxy":
					r.LoadedState.ProxyLeaseId = "old-proxy"
				case "revision":
					r.LoadedState.ActivationRevision = 0
				case "unhealthy":
					r.Modes[1].Healthy = false
				case "missing mode":
					r.Modes = r.Modes[:1]
				case "duplicate mode":
					r.Modes[0] = r.Modes[1]
				case "unknown mode":
					r.Modes[0].Mode = 99
				case "nil mode":
					r.Modes[0] = nil
				case "healthy reason":
					r.Modes[1].ReasonCode = "failed"
				case "oversize":
					r.Modes[0].ReasonMessage = strings.Repeat("x", 5000)
				case "read error":
					return r, errors.New("private upstream detail")
				}
				return r, nil
			})
			v, _ := New(c)
			receipt, err := v.Check(context.Background(), binding(b), executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API)
			requireDenied(t, receipt, err)
		})
	}
}

func TestLoadedProofRejectsAuthorityChangesDuringWorkerCall(t *testing.T) {
	now := time.Now().UTC()
	b := candidate(now)
	changes := map[string]func(*store.WorkerReadinessBinding){
		"account": func(b *store.WorkerReadinessBinding) { b.AccountID = "other-account" }, "slot": func(b *store.WorkerReadinessBinding) { b.SlotID = "other-slot" },
		"node": func(b *store.WorkerReadinessBinding) { b.NodeID = "other-node" }, "epoch": func(b *store.WorkerReadinessBinding) { b.ExecutionEpoch++ },
		"generation": func(b *store.WorkerReadinessBinding) { b.RouteGeneration++ }, "session": func(b *store.WorkerReadinessBinding) { b.ControlSessionID = strings.Repeat("b", 32) },
		"image": func(b *store.WorkerReadinessBinding) { b.ImageDigest = "sha256:" + strings.Repeat("b", 64) }, "ref": func(b *store.WorkerReadinessBinding) { b.ProviderRef = "other-container" },
		"owner": func(b *store.WorkerReadinessBinding) { b.LeaseOwnerID = "other-owner" }, "version": func(b *store.WorkerReadinessBinding) { b.CredentialVersionID = "version-next" },
		"version number": func(b *store.WorkerReadinessBinding) { b.CredentialVersionNumber++ }, "auth": func(b *store.WorkerReadinessBinding) { b.AuthType = "api_key" },
		"proxy": func(b *store.WorkerReadinessBinding) { b.ProxyLeaseID = "proxy-new" }, "reservation": func(b *store.WorkerReadinessBinding) { b.ProxyReservationID = "reservation-new" },
		"proxy binding": func(b *store.WorkerReadinessBinding) { b.ProxyBindingID = "456" }, "proxy revision": func(b *store.WorkerReadinessBinding) { b.ProxyBindingRevision++ },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			c := configFor(now, b)
			reads := 0
			c.Repository = bindingReader(func(context.Context, string, time.Time, time.Duration) (store.WorkerReadinessBinding, error) {
				reads++
				fresh := b
				if reads == 2 {
					change(&fresh)
				}
				return fresh, nil
			})
			v, _ := New(c)
			r, e := v.Check(context.Background(), binding(b), executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API)
			requireDenied(t, r, e)
		})
	}
}

func TestLoadedProofExpiryCancellationAndDependencyFailure(t *testing.T) {
	now := time.Now().UTC()
	b := candidate(now)
	for _, name := range []string{"first read", "second read", "first session", "second session", "first lease", "second lease", "worker timeout", "metadata timeout", "expired lease", "stale observation", "stale heartbeat", "clock reversed", "cancel during health", "cancel final check"} {
		t.Run(name, func(t *testing.T) {
			c := configFor(now, b)
			current := now
			c.Now = func() time.Time { return current }
			reads, sessions, leases := 0, 0, 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			privateErr := errors.New("private dependency state")
			c.Repository = bindingReader(func(op context.Context, _ string, _ time.Time, _ time.Duration) (store.WorkerReadinessBinding, error) {
				reads++
				if name == "metadata timeout" {
					<-op.Done()
					return b, nil
				}
				if name == "first read" && reads == 1 || name == "second read" && reads == 2 {
					return b, privateErr
				}
				return b, nil
			})
			c.Sessions = sessionsFunc(func(context.Context, string, string) error {
				sessions++
				if name == "cancel final check" && sessions == 2 {
					cancel()
				}
				if name == "first session" && sessions == 1 || name == "second session" && sessions == 2 {
					return privateErr
				}
				return nil
			})
			c.Leases = leasesFunc(func(context.Context, lease.Claim) error {
				leases++
				if name == "first lease" && leases == 1 || name == "second lease" && leases == 2 {
					return privateErr
				}
				return nil
			})
			c.Health = healthFunc(func(op context.Context, _ store.WorkerReadinessBinding, nonce string) (*executionv1.HealthResponse, error) {
				switch name {
				case "worker timeout":
					<-op.Done()
				case "expired lease":
					current = b.LeaseExpiresAt
				case "stale observation":
					current = b.ObservedAt.Add(45 * time.Second)
				case "stale heartbeat":
					current = b.NodeSeenAt.Add(45 * time.Second)
				case "clock reversed":
					current = now.Add(-time.Second)
				case "cancel during health":
					cancel()
				}
				return healthy(b, nonce), nil
			})
			if name == "worker timeout" || name == "metadata timeout" {
				c.Timeout = time.Millisecond
			}
			v, _ := New(c)
			r, e := v.Check(ctx, binding(b), executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API)
			requireDenied(t, r, e)
		})
	}
}

func TestLoadedProofRejectsCrossBindingBeforeWorkerAndMalformedConfig(t *testing.T) {
	now := time.Now().UTC()
	b := candidate(now)
	c := configFor(now, b)
	c.Health = healthFunc(func(context.Context, store.WorkerReadinessBinding, string) (*executionv1.HealthResponse, error) {
		t.Fatal("invalid request reached worker")
		return nil, nil
	})
	v, _ := New(c)
	for _, mutate := range []func(*dataplane.Binding){func(b *dataplane.Binding) { b.AccountID = "other-account" }, func(b *dataplane.Binding) { b.NodeID = "other-node" }, func(b *dataplane.Binding) { b.SlotID = "other-slot" }, func(b *dataplane.Binding) { b.ExecutionEpoch++ }, func(b *dataplane.Binding) { b.RouteGeneration++ }} {
		want := binding(b)
		mutate(&want)
		r, e := v.Check(context.Background(), want, executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API)
		requireDenied(t, r, e)
	}
	for _, mode := range []executionv1.ExecutionMode{0, 99} {
		r, e := v.Check(context.Background(), binding(b), mode)
		requireDenied(t, r, e)
	}
	for _, mutate := range []func(*Config){func(c *Config) { c.Repository = nil }, func(c *Config) { c.Sessions = nil }, func(c *Config) { c.Leases = nil }, func(c *Config) { c.Health = nil }, func(c *Config) { c.MaxAge = 46 * time.Second }, func(c *Config) { c.Timeout = 11 * time.Second }} {
		cfg := configFor(now, b)
		mutate(&cfg)
		if _, e := New(cfg); e != ErrConfiguration {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestLoadedProofCancellationDuringFinalClockReadIsDenied(t *testing.T) {
	now := time.Now().UTC()
	b := candidate(now)
	c := configFor(now, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sessions := 0
	c.Sessions = sessionsFunc(func(context.Context, string, string) error {
		sessions++
		return nil
	})
	finalClockRead := false
	c.Now = func() time.Time {
		if sessions == 2 {
			// All dependencies already returned successfully. Cancellation in
			// the final clock callback must still prevent a successful receipt.
			finalClockRead = true
			cancel()
		}
		return now
	}
	v, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := v.Check(ctx, binding(b), executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API)
	if !finalClockRead || ctx.Err() != context.Canceled {
		t.Fatal("did not exercise cancellation after the final dependency check")
	}
	requireDenied(t, receipt, err)
}
