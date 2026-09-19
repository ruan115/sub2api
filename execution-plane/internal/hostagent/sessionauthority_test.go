package hostagent

import (
	"context"
	"errors"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

func authorityClaim() lease.Claim {
	return lease.Claim{SlotID: "slot-1", NodeID: "node-1", ExecutionEpoch: 7, OwnerID: "owner-1"}
}

func TestNewSessionAuthorityRejectsIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	valid := SessionAuthorityConfig{
		Revoked:      func(string, uint64) bool { return false },
		SessionState: func() (bool, time.Time) { return true, time.Time{} },
		// A node may not keep acting on the authority's last word indefinitely.
		OfflineAfter: 45 * time.Second,
	}
	for name, mutate := range map[string]func(*SessionAuthorityConfig){
		"no revocation source": func(c *SessionAuthorityConfig) { c.Revoked = nil },
		"no session source":    func(c *SessionAuthorityConfig) { c.SessionState = nil },
		"zero offline window":  func(c *SessionAuthorityConfig) { c.OfflineAfter = 0 },
		"negative window":      func(c *SessionAuthorityConfig) { c.OfflineAfter = -time.Second },
		"window too long":      func(c *SessionAuthorityConfig) { c.OfflineAfter = time.Hour },
	} {
		broken := valid
		mutate(&broken)
		if _, err := NewSessionAuthority(broken); err == nil {
			t.Fatalf("NewSessionAuthority(%s) was accepted", name)
		}
	}
	if _, err := NewSessionAuthority(valid); err != nil {
		t.Fatalf("valid configuration rejected: %v", err)
	}
}

func TestSessionAuthorityHoldsWhileTheSessionIsFresh(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, err := NewSessionAuthority(SessionAuthorityConfig{
		Revoked:      func(string, uint64) bool { return false },
		SessionState: func() (bool, time.Time) { return true, time.Time{} },
		OfflineAfter: 45 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Validate(context.Background(), authorityClaim()); err != nil {
		t.Fatalf("a fresh session did not hold: %v", err)
	}
}

// A node that cannot hear the authority stops being entitled to act on what the
// authority last said.
func TestSessionAuthorityFailsClosedWhenTheSessionGoesStale(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000_000_000, 0).UTC()
	for name, contact := range map[string]time.Time{
		"never held a control session": {},
		"closed exactly at the window": now.Add(-45 * time.Second),
		"closed well past the window":  now.Add(-10 * time.Minute),
	} {
		contact := contact
		authority, err := NewSessionAuthority(SessionAuthorityConfig{
			Revoked:      func(string, uint64) bool { return false },
			SessionState: func() (bool, time.Time) { return false, contact },
			OfflineAfter: 45 * time.Second, Now: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		err = authority.Validate(context.Background(), authorityClaim())
		if !errors.Is(err, lease.ErrBackendUnavailable) {
			t.Fatalf("%s: Validate = %v, want ErrBackendUnavailable", name, err)
		}
	}
}

func TestSessionAuthorityRefusesARevokedEpoch(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, err := NewSessionAuthority(SessionAuthorityConfig{
		Revoked:      func(slotID string, epoch uint64) bool { return slotID == "slot-1" && epoch <= 7 },
		SessionState: func() (bool, time.Time) { return true, time.Time{} },
		OfflineAfter: 45 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Validate(context.Background(), authorityClaim()); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("Validate = %v, want ErrLeaseNotCurrent", err)
	}
	// A different slot, and a later epoch on the same slot, are untouched.
	other := authorityClaim()
	other.SlotID = "slot-2"
	if err := authority.Validate(context.Background(), other); err != nil {
		t.Fatalf("revoking one slot refused another: %v", err)
	}
	later := authorityClaim()
	later.ExecutionEpoch = 8
	if err := authority.Validate(context.Background(), later); err != nil {
		t.Fatalf("revoking an epoch refused a later one: %v", err)
	}
}

func TestSessionAuthorityRejectsMalformedClaims(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, err := NewSessionAuthority(SessionAuthorityConfig{
		Revoked:      func(string, uint64) bool { return false },
		SessionState: func() (bool, time.Time) { return true, time.Time{} },
		OfflineAfter: 45 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Validate(context.Background(), lease.Claim{}); err == nil {
		t.Fatal("an empty claim was accepted")
	}
}

// The watermark the authority reads is the one revocation actually maintains.
func TestExecutorRevocationWatermarkFeedsTheAuthority(t *testing.T) {
	t.Parallel()
	executor, _, _, _ := custodianFixture(t)
	if executor.epochRevoked("slot-1", 7) {
		t.Fatal("nothing was revoked yet")
	}
	result := executor.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{
		CommandId: "revoke-1", SlotId: "slot-1", ExecutionEpoch: 7,
	})
	if !result.GetSucceeded() {
		t.Fatalf("revoke failed: %s", result.GetErrorCode())
	}
	if !executor.epochRevoked("slot-1", 7) {
		t.Fatal("the revoked epoch is not reflected in the watermark")
	}
	if executor.epochRevoked("slot-1", 8) {
		t.Fatal("a later epoch was marked revoked")
	}
	if executor.epochRevoked("slot-2", 7) {
		t.Fatal("another slot was marked revoked")
	}
}
