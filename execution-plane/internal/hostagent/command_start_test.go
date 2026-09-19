package hostagent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	providerfake "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/fake"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

type startupFunc func(context.Context, provider.SlotSpec, provider.Instance, string) error

func (f startupFunc) Start(ctx context.Context, spec provider.SlotSpec, instance provider.Instance, owner string) error {
	return f(ctx, spec, instance, owner)
}

type strictCommandProvider struct {
	*providerfake.Provider
	status     provider.Status
	starts     int
	inspect    func()
	inspectErr error
}

func (p *strictCommandProvider) InspectSlot(context.Context, string) (provider.Status, error) {
	if p.inspect != nil {
		p.inspect()
	}
	return p.status, p.inspectErr
}

func (p *strictCommandProvider) Start(context.Context, string) error { p.starts++; return nil }

func strictCommandFixture(t *testing.T) (*SlotCommandExecutor, *strictCommandProvider, *executionv1.SlotCommand) {
	t.Helper()
	now := time.Now()
	cmd := slotCommand(now, "start-auth", executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START, 9)
	p := &strictCommandProvider{Provider: providerfake.New(), status: provider.Status{
		Instance: provider.Instance{ProviderRef: "managed-logical-name", RuntimeID: strings.Repeat("a", 64),
			SlotID: cmd.SlotId, Epoch: 9, RuntimeGeneration: 3, State: slot.StateReady},
		Healthy: true, ImageDigest: cmd.ImageDigest,
	}}
	e := newTestSlotCommandExecutor(t, p, now)
	e.startup = startupFunc(func(context.Context, provider.SlotSpec, provider.Instance, string) error { return nil })
	return e, p, cmd
}

func assertUnhealthyInspect(t *testing.T, e *SlotCommandExecutor, cmd *executionv1.SlotCommand) {
	t.Helper()
	cmd.Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT
	result := e.ExecuteSlotCommand(context.Background(), cmd)
	if result.GetSlot().GetHealthy() {
		t.Fatal("unverified instance became healthy through INSPECT")
	}
	for _, observation := range e.Snapshot().Slots {
		if observation.GetHealthy() {
			t.Fatal("snapshot resurrected failed authentication")
		}
	}
}

func TestAuthenticatedStartRejectsFailureDriftAndCancellationWithoutFallback(t *testing.T) {
	for _, mode := range []string{"bootstrap-failure", "missing-id", "replacement", "logical-ref", "unhealthy", "cancel", "inspect-error"} {
		t.Run(mode, func(t *testing.T) {
			e, p, cmd := strictCommandFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			if mode == "missing-id" {
				p.status.RuntimeID = ""
			}
			e.startup = startupFunc(func(_ context.Context, spec provider.SlotSpec, instance provider.Instance, _ string) error {
				calls++
				if spec.AccountID != cmd.AccountId || instance.RuntimeID != strings.Repeat("a", 64) {
					t.Fatal("wrong startup binding")
				}
				switch mode {
				case "bootstrap-failure":
					return errors.New("synthetic rejection")
				case "replacement":
					p.status.RuntimeID = strings.Repeat("b", 64)
				case "logical-ref":
					p.status.ProviderRef = "replacement-name"
				case "unhealthy":
					p.status.Healthy = false
				case "cancel":
					p.inspect = cancel
				case "inspect-error":
					p.inspectErr = errors.New("inspection failed")
				}
				return nil
			})
			result := e.ExecuteSlotCommand(ctx, cmd)
			if result.GetSucceeded() || result.GetSlot().GetHealthy() || p.starts != 0 {
				t.Fatal("failed authenticated startup passed or fell back")
			}
			if mode == "missing-id" && calls != 0 {
				t.Fatal("missing immutable ID reached startup")
			}
			p.inspect, p.inspectErr, p.status.Healthy = nil, nil, true
			assertUnhealthyInspect(t, e, cmd)
		})
	}
}

func TestAuthenticatedStartProofCannotSurviveFailedRestartOrContainerReplacement(t *testing.T) {
	e, p, cmd := strictCommandFixture(t)
	assertUnhealthyInspect(t, e, cmd)
	cmd.Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START
	result := e.ExecuteSlotCommand(context.Background(), cmd)
	if !result.GetSucceeded() || !result.GetSlot().GetHealthy() || p.starts != 0 {
		t.Fatal("valid authenticated start failed")
	}
	cmd.Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT
	if !e.ExecuteSlotCommand(context.Background(), cmd).GetSlot().GetHealthy() {
		t.Fatal("matching proof was lost")
	}
	p.status.RuntimeID = strings.Repeat("b", 64)
	assertUnhealthyInspect(t, e, cmd)
	p.status.RuntimeID = strings.Repeat("a", 64)
	assertUnhealthyInspect(t, e, cmd) // drift consumes proof; restoring metadata does not resurrect it
	cmd.Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_START
	if !e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
		t.Fatal("reauthentication failed")
	}
	e.startup = startupFunc(func(context.Context, provider.SlotSpec, provider.Instance, string) error {
		for _, observation := range e.Snapshot().Slots {
			if observation.GetHealthy() {
				t.Fatal("START in progress retained healthy observation")
			}
		}
		return errors.New("authentication unavailable")
	})
	if e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
		t.Fatal("failed restart accepted")
	}
	assertUnhealthyInspect(t, e, cmd)
}

func TestAuthenticatedStartProofClearedBeforeFailedLifecycleAndRevocation(t *testing.T) {
	for _, action := range []executionv1.SlotCommandAction{
		executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DRAIN,
		executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_STOP,
		executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_DESTROY,
	} {
		t.Run(action.String(), func(t *testing.T) {
			e, _, cmd := strictCommandFixture(t)
			if !e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
				t.Fatal("fixture start")
			}
			cmd.Action = action
			_ = e.ExecuteSlotCommand(context.Background(), cmd)
			assertUnhealthyInspect(t, e, cmd)
		})
	}
	e, p, cmd := strictCommandFixture(t)
	if !e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
		t.Fatal("fixture start")
	}
	old := &executionv1.RevokeEpochCommand{CommandId: "old-revoke", SlotId: cmd.SlotId, ExecutionEpoch: 8}
	_ = e.RevokeEpoch(context.Background(), old)
	cmd.Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT
	if !e.ExecuteSlotCommand(context.Background(), cmd).GetSlot().GetHealthy() {
		t.Fatal("old revoke cleared newer proof")
	}
	p.inspectErr = errors.New("unavailable")
	old.ExecutionEpoch = 9
	_ = e.RevokeEpoch(context.Background(), old)
	p.inspectErr = nil
	assertUnhealthyInspect(t, e, cmd)
}

func TestAuthenticatedStartProofClearedWhenRestartCannotInspectOrHealthIsLost(t *testing.T) {
	for _, mode := range []string{"start-inspect-error", "health-lost"} {
		t.Run(mode, func(t *testing.T) {
			e, p, cmd := strictCommandFixture(t)
			if !e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
				t.Fatal("fixture start")
			}
			if mode == "start-inspect-error" {
				p.inspectErr = errors.New("no current observation")
				if e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
					t.Fatal("missing start observation accepted")
				}
				p.inspectErr = nil
			} else {
				p.status.Healthy = false
				assertUnhealthyInspect(t, e, cmd)
				p.status.Healthy = true
			}
			assertUnhealthyInspect(t, e, cmd)
		})
	}
}

func TestSlotCommandRechecksCancellationAfterOperationLock(t *testing.T) {
	e, p, cmd := strictCommandFixture(t)
	e.operationMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *executionv1.CommandResult, 1)
	go func() { done <- e.ExecuteSlotCommand(ctx, cmd) }()
	cancel()
	e.operationMu.Unlock()
	select {
	case result := <-done:
		if result.GetSucceeded() || p.starts != 0 {
			t.Fatal("cancelled waiter started worker")
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not finish")
	}
}

func TestAuthenticatedInspectConsumesCancelledAndDriftedProof(t *testing.T) {
	for _, mode := range []string{"cancel", "slot", "epoch", "generation", "image"} {
		t.Run(mode, func(t *testing.T) {
			e, p, cmd := strictCommandFixture(t)
			if !e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
				t.Fatal("fixture start")
			}
			original := p.status
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd.Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT
			switch mode {
			case "cancel":
				p.inspect = cancel
			case "slot":
				p.status.SlotID = "another-slot"
			case "epoch":
				p.status.Epoch++
			case "generation":
				p.status.RuntimeGeneration++
			case "image":
				p.status.ImageDigest = "sha256:" + strings.Repeat("b", 64)
			}
			result := e.ExecuteSlotCommand(ctx, cmd)
			if result.GetSucceeded() || result.GetSlot().GetHealthy() {
				t.Fatal("late or drifted INSPECT accepted")
			}
			p.inspect, p.status = nil, original
			assertUnhealthyInspect(t, e, cmd)
		})
	}
}

func TestOldInspectCommandDoesNotClearNewerProof(t *testing.T) {
	e, _, cmd := strictCommandFixture(t)
	if !e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
		t.Fatal("fixture start")
	}
	cmd.Action = executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT
	cmd.ExecutionEpoch--
	if e.ExecuteSlotCommand(context.Background(), cmd).GetSucceeded() {
		t.Fatal("old command accepted newer instance")
	}
	cmd.ExecutionEpoch++
	if !e.ExecuteSlotCommand(context.Background(), cmd).GetSlot().GetHealthy() {
		t.Fatal("old command erased newer proof")
	}
}
