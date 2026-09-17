package runtimeenrollment_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	enrollment "github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/storage"
)

type bindingFunc func(context.Context, string, time.Time, time.Duration) (enrollment.Grant, error)

func (f bindingFunc) ReadRuntimeEnrollmentBinding(c context.Context, s string, n time.Time, a time.Duration) (enrollment.Grant, error) {
	return f(c, s, n, a)
}

type sessionFunc func(context.Context, string, string) error

func (f sessionFunc) ValidateControlSession(c context.Context, n, s string) error { return f(c, n, s) }

type leaseFunc func(context.Context, lease.Claim) error

func (f leaseFunc) Validate(c context.Context, l lease.Claim) error { return f(c, l) }

type receiptHooks struct {
	enrollment.ReceiptStore
	load   func()
	commit func()
}

func (r receiptHooks) Load(c context.Context, id string) (enrollment.Receipt, error) {
	v, e := r.ReceiptStore.Load(c, id)
	if r.load != nil {
		r.load()
	}
	return v, e
}
func (r receiptHooks) GetOrCreate(c context.Context, v enrollment.Receipt) (enrollment.Receipt, error) {
	v, e := r.ReceiptStore.GetOrCreate(c, v)
	if r.commit != nil {
		r.commit()
	}
	return v, e
}

type brokerFixture struct {
	clock     atomic.Int64
	reads     atomic.Int32
	grant     enrollment.Grant
	principal enrollment.Principal
	key       *ecdsa.PrivateKey
	receipts  *storage.Memory
	config    enrollment.Config
	readHook  func(int32, enrollment.Grant) enrollment.Grant
}

func newBrokerFixture(t *testing.T) *brokerFixture {
	t.Helper()
	f := new(brokerFixture)
	now := time.Now().UTC().Truncate(time.Second)
	f.clock.Store(now.UnixNano())
	f.grant = enrollment.Grant{AssignmentID: "assignment-1", AccountID: "synthetic-account", SlotID: "slot-a", NodeID: "node-a", ImageDigest: "sha256:" + strings.Repeat("a", 64), ControlSessionID: strings.Repeat("b", 32), LeaseOwnerID: "owner-a", Epoch: 7, Generation: 11, NodeSeenAt: now, LeaseExpiresAt: now.Add(45 * time.Second)}
	f.principal = enrollment.Principal{NodeID: f.grant.NodeID, SessionID: f.grant.ControlSessionID}
	var err error
	f.key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.receipts, err = storage.NewMemory(32)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := pki.NewEphemeralAuthority(func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.config = enrollment.Config{Authority: a, Receipts: f.receipts, Now: func() time.Time { return time.Unix(0, f.clock.Load()).UTC() },
		Bindings: bindingFunc(func(ctx context.Context, id string, now time.Time, age time.Duration) (enrollment.Grant, error) {
			if id != f.grant.SlotID || age != 45*time.Second {
				return enrollment.Grant{}, errors.New("synthetic rejected")
			}
			g := f.grant
			n := f.reads.Add(1)
			if f.readHook != nil {
				g = f.readHook(n, g)
			}
			return g, nil
		}),
		Sessions: sessionFunc(func(ctx context.Context, n, s string) error {
			if n != f.principal.NodeID || s != f.principal.SessionID {
				return errors.New("synthetic rejected")
			}
			return nil
		}),
		Leases: leaseFunc(func(ctx context.Context, c lease.Claim) error {
			if c != (lease.Claim{SlotID: f.grant.SlotID, NodeID: f.grant.NodeID, ExecutionEpoch: f.grant.Epoch, OwnerID: f.grant.LeaseOwnerID}) {
				return errors.New("synthetic rejected")
			}
			return nil
		})}
	return f
}
func (f *brokerFixture) request(t *testing.T, key *ecdsa.PrivateKey) enrollment.Request {
	t.Helper()
	u, _ := f.grant.RuntimeBinding().URI()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{URIs: []*url.URL{u}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return enrollment.Request{SlotID: f.grant.SlotID, Epoch: f.grant.Epoch, Generation: f.grant.Generation, CSRPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})}
}
func (f *brokerFixture) broker(t *testing.T) *enrollment.Broker {
	t.Helper()
	b, e := enrollment.New(f.config)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func denied(t *testing.T, raw []byte, err error) {
	t.Helper()
	if len(raw) != 0 || err != enrollment.ErrRejected {
		t.Fatalf("expected fixed empty denial, got %d bytes / %v", len(raw), err)
	}
}

func TestEnrollmentSameKeyDifferentCSRReturnsFirstLeaf(t *testing.T) {
	f := newBrokerFixture(t)
	b := f.broker(t)
	firstRequest := f.request(t, f.key)
	secondRequest := f.request(t, f.key)
	if bytes.Equal(firstRequest.CSRPEM, secondRequest.CSRPEM) {
		t.Fatal("fixture CSR signatures should differ")
	}
	first, err := b.Enroll(context.Background(), f.principal, firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.Enroll(context.Background(), f.principal, secondRequest)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatal("same-key retry reissued/changed leaf", err)
	}
	first[0] ^= 1
	third, err := b.Enroll(context.Background(), f.principal, secondRequest)
	if err != nil || !bytes.Equal(second, third) {
		t.Fatal("response aliases persistent receipt")
	}
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	raw, err := b.Enroll(context.Background(), f.principal, f.request(t, other))
	denied(t, raw, err)
	if f.reads.Load() < 8 {
		t.Fatal("authority not revalidated around storage")
	}
}

func TestEnrollmentConcurrentSameKeyPinsExactFirstLeaf(t *testing.T) {
	f := newBrokerFixture(t)
	b := f.broker(t)
	requests := make([]enrollment.Request, 16)
	for i := range requests {
		requests[i] = f.request(t, f.key)
	}
	results := make(chan []byte, len(requests))
	errs := make(chan error, len(requests))
	var wg sync.WaitGroup
	for _, r := range requests {
		wg.Add(1)
		go func(r enrollment.Request) {
			defer wg.Done()
			v, e := b.Enroll(context.Background(), f.principal, r)
			results <- v
			errs <- e
		}(r)
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first []byte
	for v := range results {
		if first == nil {
			first = v
		} else if !bytes.Equal(first, v) {
			t.Fatal("concurrent issuance returned different leaves")
		}
	}
}

func TestEnrollmentConcurrentDifferentKeysHaveOneWinner(t *testing.T) {
	f := newBrokerFixture(t)
	b := f.broker(t)
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	requests := []enrollment.Request{f.request(t, f.key), f.request(t, other)}
	var wg sync.WaitGroup
	var successes atomic.Int32
	for _, r := range requests {
		wg.Add(1)
		go func(r enrollment.Request) {
			defer wg.Done()
			v, e := b.Enroll(context.Background(), f.principal, r)
			if e == nil {
				if len(v) == 0 {
					t.Error("empty success")
				}
				successes.Add(1)
			} else if e != enrollment.ErrRejected || len(v) != 0 {
				t.Error("nonfixed denial")
			}
		}(r)
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatal("first-key pin did not select exactly one winner")
	}
}

func TestEnrollmentRejectsAuthorityChangesDuringStorage(t *testing.T) {
	for _, when := range []string{"load", "commit"} {
		for name, change := range map[string]func(*enrollment.Grant){
			"assignment": func(g *enrollment.Grant) { g.AssignmentID = "other-assignment" }, "account": func(g *enrollment.Grant) { g.AccountID = "other-account" },
			"node": func(g *enrollment.Grant) { g.NodeID = "other-node" }, "session": func(g *enrollment.Grant) { g.ControlSessionID = strings.Repeat("c", 32) },
			"owner": func(g *enrollment.Grant) { g.LeaseOwnerID = "other-owner" }, "image": func(g *enrollment.Grant) { g.ImageDigest = "sha256:" + strings.Repeat("c", 64) },
			"epoch": func(g *enrollment.Grant) { g.Epoch++ }, "generation": func(g *enrollment.Grant) { g.Generation++ },
		} {
			t.Run(when+"/"+name, func(t *testing.T) {
				f := newBrokerFixture(t)
				req := f.request(t, f.key)
				hooks := receiptHooks{ReceiptStore: f.receipts}
				if when == "load" {
					hooks.load = func() { change(&f.grant) }
				} else {
					hooks.commit = func() { change(&f.grant) }
				}
				f.config.Receipts = hooks
				raw, err := f.broker(t).Enroll(context.Background(), f.principal, req)
				denied(t, raw, err)
			})
		}
	}
}

func TestEnrollmentFailsClosedForSessionLeaseCancellationAndTime(t *testing.T) {
	for _, mode := range []string{"session", "lease", "cancel-load", "cancel-commit", "timeout", "clock-rollback", "lease-expiry-refresh", "node-age-refresh", "grant-during-lease"} {
		t.Run(mode, func(t *testing.T) {
			f := newBrokerFixture(t)
			req := f.request(t, f.key)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "session":
				f.config.Sessions = sessionFunc(func(context.Context, string, string) error { return errors.New("synthetic") })
			case "lease":
				f.config.Leases = leaseFunc(func(context.Context, lease.Claim) error { return errors.New("synthetic") })
			case "cancel-load":
				f.config.Receipts = receiptHooks{ReceiptStore: f.receipts, load: cancel}
			case "cancel-commit":
				f.config.Receipts = receiptHooks{ReceiptStore: f.receipts, commit: cancel}
			case "timeout":
				f.config.Receipts = receiptHooks{ReceiptStore: f.receipts, load: func() { f.clock.Add(int64(2 * time.Second)) }}
			case "clock-rollback":
				f.config.Receipts = receiptHooks{ReceiptStore: f.receipts, load: func() { f.clock.Add(-int64(time.Second)) }}
			case "lease-expiry-refresh":
				f.grant.LeaseExpiresAt = f.config.Now().Add(time.Second)
				f.config.Receipts = receiptHooks{ReceiptStore: f.receipts, load: func() { f.clock.Add(int64(time.Second)); f.grant.LeaseExpiresAt = f.config.Now().Add(time.Hour) }}
			case "node-age-refresh":
				f.grant.NodeSeenAt = f.config.Now().Add(-44 * time.Second)
				f.config.Receipts = receiptHooks{ReceiptStore: f.receipts, load: func() { f.clock.Add(int64(time.Second)); f.grant.NodeSeenAt = f.config.Now() }}
			case "grant-during-lease":
				f.config.Leases = leaseFunc(func(context.Context, lease.Claim) error { f.grant.Generation++; return nil })
			}
			raw, err := f.broker(t).Enroll(ctx, f.principal, req)
			denied(t, raw, err)
		})
	}
}

func TestEnrollmentExpiredReceiptNeverImplicitlyRotates(t *testing.T) {
	f := newBrokerFixture(t)
	b := f.broker(t)
	req := f.request(t, f.key)
	if _, err := b.Enroll(context.Background(), f.principal, req); err != nil {
		t.Fatal(err)
	}
	f.clock.Add(int64(2 * time.Hour))
	f.grant.NodeSeenAt = f.config.Now()
	f.grant.LeaseExpiresAt = f.config.Now().Add(45 * time.Second)
	raw, err := b.Enroll(context.Background(), f.principal, f.request(t, f.key))
	denied(t, raw, err)
}

func TestEnrollmentInvalidRequestsNeverReachStorage(t *testing.T) {
	for _, mode := range []string{"nil-context", "cancelled", "empty-csr", "wrong-slot", "wrong-epoch", "wrong-generation", "wrong-session", "invalid-csr", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			f := newBrokerFixture(t)
			req := f.request(t, f.key)
			p := f.principal
			ctx := context.Background()
			switch mode {
			case "nil-context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "empty-csr":
				req.CSRPEM = nil
			case "wrong-slot":
				req.SlotID = "other-slot"
			case "wrong-epoch":
				req.Epoch++
			case "wrong-generation":
				req.Generation++
			case "wrong-session":
				p.SessionID = strings.Repeat("d", 32)
			case "invalid-csr":
				req.CSRPEM = []byte("synthetic")
			case "oversize":
				req.CSRPEM = bytes.Repeat([]byte("a"), 16385)
			}
			f.config.Receipts = receiptHooks{ReceiptStore: f.receipts, load: func() { t.Error("invalid request reached storage") }}
			raw, err := f.broker(t).Enroll(ctx, p, req)
			denied(t, raw, err)
		})
	}
}
