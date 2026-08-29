package worker

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
)

func testKeys(t *testing.T) (*ticket.Issuer, *ticket.Verifier) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(strings.NewReader(strings.Repeat("g", ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ticket.NewIssuer(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := ticket.NewVerifier(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	return issuer, verifier
}

func signedTicket(t *testing.T, issuer *ticket.Issuer, now time.Time, identity Identity, scope string) string {
	t.Helper()
	claims, err := ticket.NewClaims(identity.AccountID, identity.SlotID, identity.NodeID, identity.Epoch, []string{scope}, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	rawTicket, err := issuer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	return rawTicket
}

func TestGuardBindsIdentityScopeAndNonce(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	identity := Identity{AccountID: "account-1", SlotID: "slot-1", NodeID: "srv74", Epoch: 8}
	issuer, verifier := testKeys(t)
	guard, err := NewGuard(verifier, identity, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	rawTicket := signedTicket(t, issuer, now, identity, "messages")
	if _, err := guard.Authorize(rawTicket, "messages"); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.Authorize(rawTicket, "messages"); !errors.Is(err, ErrReplay) {
		t.Fatalf("expected replay error, got %v", err)
	}

	wrongScope := signedTicket(t, issuer, now, identity, "count_tokens")
	if _, err := guard.Authorize(wrongScope, "messages"); !errors.Is(err, ErrScopeDenied) {
		t.Fatalf("expected scope error, got %v", err)
	}

	wrongEpoch := identity
	wrongEpoch.Epoch++
	if _, err := guard.Authorize(signedTicket(t, issuer, now, wrongEpoch, "messages"), "messages"); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("expected identity error, got %v", err)
	}
}

func TestGuardAllowsExactlyOneConcurrentUse(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	identity := Identity{AccountID: "account-1", SlotID: "slot-1", NodeID: "srv74", Epoch: 8}
	issuer, verifier := testKeys(t)
	guard, _ := NewGuard(verifier, identity, func() time.Time { return now })
	rawTicket := signedTicket(t, issuer, now, identity, "messages")

	var succeeded atomic.Int32
	var waitGroup sync.WaitGroup
	for range 32 {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			if _, err := guard.Authorize(rawTicket, "messages"); err == nil {
				succeeded.Add(1)
			} else if !errors.Is(err, ErrReplay) {
				t.Errorf("unexpected authorization error: %v", err)
			}
		}()
	}
	waitGroup.Wait()
	if succeeded.Load() != 1 {
		t.Fatalf("expected exactly one successful authorization, got %d", succeeded.Load())
	}
}
