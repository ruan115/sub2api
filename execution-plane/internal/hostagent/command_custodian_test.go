package hostagent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const custodianImageDigest = "sha256:" + "cd" + "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd" + "cd"

var errCustodianStop = errors.New("provider stop failed")

// A stub rather than the shared fake, so stop and destroy outcomes can be
// driven directly.
type custodianProvider struct {
	status   provider.Status
	stopErr  error
	stops    int
	destroys int
}

func (p *custodianProvider) Create(context.Context, provider.SlotSpec) (provider.Instance, error) {
	return provider.Instance{}, errors.New("create is not part of this test")
}

func (p *custodianProvider) Inspect(context.Context, string) (provider.Status, error) {
	return p.status, nil
}

func (p *custodianProvider) InspectSlot(context.Context, string) (provider.Status, error) {
	return p.status, nil
}

func (p *custodianProvider) Start(context.Context, string) error { return nil }

func (p *custodianProvider) Drain(context.Context, string, time.Time) error { return nil }

func (p *custodianProvider) Stop(context.Context, string) error {
	p.stops++
	return p.stopErr
}

func (p *custodianProvider) Destroy(context.Context, string) error {
	p.destroys++
	return nil
}

type recordedReclaim struct {
	slotID string
	epoch  uint64
}

type recordingCustodian struct {
	mu        sync.Mutex
	reclaimed []recordedReclaim
}

func (c *recordingCustodian) Revoke(slotID string, epoch uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reclaimed = append(c.reclaimed, recordedReclaim{slotID, epoch})
}

func (c *recordingCustodian) calls() []recordedReclaim {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedReclaim(nil), c.reclaimed...)
}

func custodianFixture(t *testing.T) (*SlotCommandExecutor, *custodianProvider, *recordingCustodian, time.Time) {
	t.Helper()
	now := time.Unix(2_000_000_000, 0).UTC()
	implementation := &custodianProvider{
		status: provider.Status{
			Instance: provider.Instance{
				ProviderRef: "execution-slot-1", RuntimeID: "cid-1", SlotID: "slot-1",
				Epoch: 7, RuntimeGeneration: 2, State: "ready",
			},
			Healthy: true, ImageDigest: custodianImageDigest,
		},
	}
	custodian := &recordingCustodian{}
	executor, err := NewSlotCommandExecutor(SlotCommandExecutorConfig{
		Provider:  implementation,
		Custodian: custodian,
		Resources: provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 256 << 20},
		Security: provider.SecurityPolicy{
			RunAsUser: 65532, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true,
			SeccompProfile: "worker", AppArmorProfile: "worker",
		},
		Network: provider.NetworkPolicy{
			DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:18080",
		},
		DrainTimeout: time.Minute, MaxSlots: 20, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return executor, implementation, custodian, now
}

func slotCommandFor(action executionv1.SlotCommandAction, now time.Time) *executionv1.SlotCommand {
	return &executionv1.SlotCommand{
		CommandId: "command-1", SlotId: "slot-1", AccountId: "account-1", ExecutionEpoch: 7,
		ImageDigest: custodianImageDigest, Action: action,
		Deadline: timestamppb.New(now.Add(time.Minute)),
		Metadata: map[string]string{"desired_generation": "2", "target_runtime_generation": "2"},
	}
}

// Revoking an epoch must end the authenticated transport this node holds for
// that slot, not merely refuse the next command.
func TestRevokeEpochReclaimsTheHeldConnection(t *testing.T) {
	t.Parallel()
	executor, _, custodian, _ := custodianFixture(t)
	result := executor.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{
		CommandId: "revoke-1", SlotId: "slot-1", ExecutionEpoch: 7,
	})
	if !result.GetSucceeded() {
		t.Fatalf("revoke failed: %s", result.GetErrorCode())
	}
	calls := custodian.calls()
	if len(calls) != 1 || calls[0].slotID != "slot-1" || calls[0].epoch != 7 {
		t.Fatalf("reclaim calls = %+v, want one for slot-1 epoch 7", calls)
	}
}

// Reclamation must happen even when the provider work fails afterwards: a
// transport that outlives a failed revocation is the worst outcome.
func TestRevokeEpochReclaimsEvenWhenTheProviderFails(t *testing.T) {
	t.Parallel()
	executor, implementation, custodian, _ := custodianFixture(t)
	implementation.stopErr = errCustodianStop
	result := executor.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{
		CommandId: "revoke-1", SlotId: "slot-1", ExecutionEpoch: 7,
	})
	if result.GetSucceeded() {
		t.Fatal("revocation reported success despite a provider failure")
	}
	if calls := custodian.calls(); len(calls) != 1 {
		t.Fatalf("reclaim calls = %+v, want the connection reclaimed anyway", calls)
	}
}

// Stopping or destroying a slot ends the container, so the transport
// authenticated to it must end too.
func TestStopAndDestroyReclaimTheHeldConnection(t *testing.T) {
	t.Parallel()
	for name, action := range map[string]executionv1.SlotCommandAction{
		"stop":    executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_STOP,
		"destroy": executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DESTROY,
	} {
		action := action
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			executor, _, custodian, now := custodianFixture(t)
			result := executor.ExecuteSlotCommand(context.Background(), slotCommandFor(action, now))
			if !result.GetSucceeded() {
				t.Fatalf("%s failed: %s", name, result.GetErrorCode())
			}
			calls := custodian.calls()
			if len(calls) != 1 || calls[0].slotID != "slot-1" || calls[0].epoch != 7 {
				t.Fatalf("%s reclaim calls = %+v", name, calls)
			}
		})
	}
}

// A command that does not end the instance must not reclaim its connection.
func TestInspectDoesNotReclaimTheHeldConnection(t *testing.T) {
	t.Parallel()
	executor, _, custodian, now := custodianFixture(t)
	result := executor.ExecuteSlotCommand(context.Background(),
		slotCommandFor(executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT, now))
	if !result.GetSucceeded() {
		t.Fatalf("inspect failed: %s", result.GetErrorCode())
	}
	if calls := custodian.calls(); len(calls) != 0 {
		t.Fatalf("inspect reclaimed a connection: %+v", calls)
	}
}

// Without a custodian nothing is held, so nothing may be reclaimed and the
// executor must not panic reaching for one.
func TestExecutorWithoutCustodianStillRevokes(t *testing.T) {
	t.Parallel()
	executor, _, _, _ := custodianFixture(t)
	executor.custodian = nil
	result := executor.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{
		CommandId: "revoke-1", SlotId: "slot-1", ExecutionEpoch: 7,
	})
	if !result.GetSucceeded() {
		t.Fatalf("revoke without a custodian failed: %s", result.GetErrorCode())
	}
}
