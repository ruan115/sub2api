package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

func wiringClaim(epoch uint64) lease.Claim {
	return lease.Claim{SlotID: "slot-1", NodeID: "node-1", ExecutionEpoch: epoch, OwnerID: "owner-1"}
}

// The cycle is broken by binding two fields late, so the window before they are
// bound must answer fail-closed rather than "nothing is revoked, no session
// problem".
func TestNodeFactsFailClosedBeforeComposition(t *testing.T) {
	t.Parallel()
	for name, facts := range map[string]*nodeFacts{
		"nil holder":  nil,
		"unbound":     {},
		"no executor": {control: &hostagent.ControlClient{}},
		"no client":   {executor: &hostagent.SlotCommandExecutor{}},
	} {
		facts := facts
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if facts == nil || facts.executor == nil {
				if !facts.epochRevoked("slot-1", 1) {
					t.Fatal("an unbound executor reported the epoch as live")
				}
			}
			if facts == nil || facts.control == nil {
				open, closedAt := facts.controlSessionState()
				if open || !closedAt.IsZero() {
					t.Fatalf("an unbound client reported a session: %v %v", open, closedAt)
				}
			}
		})
	}
}

// An authority built on an unbound holder must refuse, so a custodian that
// somehow ran early could not hold anything.
func TestAuthorityOverUnboundFactsRefuses(t *testing.T) {
	t.Parallel()
	facts := &nodeFacts{}
	authority, err := hostagent.NewSessionAuthority(hostagent.SessionAuthorityConfig{
		Revoked: facts.epochRevoked, SessionState: facts.controlSessionState,
		OfflineAfter: 45 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Validate(context.Background(), wiringClaim(1)); !errors.Is(err, lease.ErrBackendUnavailable) {
		t.Fatalf("Validate over unbound facts = %v, want ErrBackendUnavailable", err)
	}
}

// Once bound, the authority reads the executor's real watermark: a revocation
// the control plane pushed ends the claim.
func TestBoundFactsTrackTheExecutorRevocationWatermark(t *testing.T) {
	t.Parallel()
	executor, err := hostagent.NewSlotCommandExecutor(hostagent.SlotCommandExecutorConfig{
		Provider:  wiringProvider{},
		Resources: provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 256 << 20},
		Security: provider.SecurityPolicy{
			RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true,
			SeccompProfile: "worker", AppArmorProfile: "worker",
		},
		Network: provider.NetworkPolicy{
			DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080",
		},
		DrainTimeout: time.Minute, MaxSlots: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	facts := &nodeFacts{executor: executor}
	if facts.epochRevoked("slot-1", 1) {
		t.Fatal("nothing was revoked yet")
	}
	result := executor.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{
		CommandId: "revoke-1", SlotId: "slot-1", ExecutionEpoch: 3,
	})
	if !result.GetSucceeded() {
		t.Fatalf("revoke failed: %s", result.GetErrorCode())
	}
	if !facts.epochRevoked("slot-1", 3) {
		t.Fatal("the pushed revocation did not reach the authority's view")
	}
	if facts.epochRevoked("slot-1", 4) {
		t.Fatal("a later epoch was reported revoked")
	}
	if facts.epochRevoked("slot-2", 3) {
		t.Fatal("another slot was reported revoked")
	}
}

// A provider that reports nothing: RevokeEpoch only needs InspectSlot to say
// the slot is absent for the watermark to be recorded.
type wiringProvider struct{}

func (wiringProvider) Create(context.Context, provider.SlotSpec) (provider.Instance, error) {
	return provider.Instance{}, provider.ErrNotFound
}
func (wiringProvider) Inspect(context.Context, string) (provider.Status, error) {
	return provider.Status{}, provider.ErrNotFound
}
func (wiringProvider) InspectSlot(context.Context, string) (provider.Status, error) {
	return provider.Status{}, provider.ErrNotFound
}
func (wiringProvider) Start(context.Context, string) error            { return nil }
func (wiringProvider) Drain(context.Context, string, time.Time) error { return nil }
func (wiringProvider) Stop(context.Context, string) error             { return nil }
func (wiringProvider) Destroy(context.Context, string) error          { return nil }
