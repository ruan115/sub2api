package daemon

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type lifecycleDelegate struct {
	execute  func(context.Context, *executionv1.SlotCommand) *executionv1.CommandResult
	revoke   func(context.Context, *executionv1.RevokeEpochCommand) *executionv1.CommandResult
	snapshot hostagent.NodeSnapshot
	executes atomic.Int32
	revokes  atomic.Int32
}

func (d *lifecycleDelegate) ExecuteSlotCommand(ctx context.Context, command *executionv1.SlotCommand) *executionv1.CommandResult {
	d.executes.Add(1)
	if d.execute != nil {
		return d.execute(ctx, command)
	}
	return &executionv1.CommandResult{CommandId: command.GetCommandId(), Succeeded: true}
}
func (d *lifecycleDelegate) RevokeEpoch(ctx context.Context, command *executionv1.RevokeEpochCommand) *executionv1.CommandResult {
	d.revokes.Add(1)
	if d.revoke != nil {
		return d.revoke(ctx, command)
	}
	return &executionv1.CommandResult{CommandId: command.GetCommandId(), Succeeded: true}
}
func (d *lifecycleDelegate) Snapshot() hostagent.NodeSnapshot { return d.snapshot }

func lifecycleTestCommand() *executionv1.SlotCommand {
	return &executionv1.SlotCommand{CommandId: "command-1", SlotId: "slot-1", ExecutionEpoch: 7,
		Deadline: timestamppb.New(time.Now().Add(time.Minute))}
}
func lifecycleTestExecutor(t *testing.T, d *lifecycleDelegate) *LifecycleExecutor {
	t.Helper()
	e, err := NewLifecycleExecutor(d)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func lifecycleReceive(t *testing.T, result <-chan *executionv1.CommandResult) *executionv1.CommandResult {
	t.Helper()
	select {
	case response := <-result:
		return response
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle call did not return")
		return nil
	}
}
func lifecycleActive(t *testing.T, e *LifecycleExecutor, count uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.Lock()
		active := e.active
		e.mu.Unlock()
		if active == count {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("lifecycle active count did not converge")
}

func TestLifecycleExecutorRejectsNilAndRequiresSealBeforeWait(t *testing.T) {
	var typedNil *lifecycleDelegate
	for _, delegate := range []hostagent.ControlCommandExecutor{nil, typedNil} {
		if e, err := NewLifecycleExecutor(delegate); e != nil || !errors.Is(err, ErrLifecycleExecutorConfig) {
			t.Fatal("nil delegate accepted")
		}
	}
	d := &lifecycleDelegate{snapshot: hostagent.NodeSnapshot{AllocatedSlots: 3, ActiveAPI: 2}}
	e := lifecycleTestExecutor(t, d)
	if !reflect.DeepEqual(e.Snapshot(), d.snapshot) || !errors.Is(e.Wait(context.Background()), ErrLifecycleExecutorNotSealed) {
		t.Fatal("snapshot forwarding or unsealed wait failed")
	}
	if e.ExecuteSlotCommand(nil, lifecycleTestCommand()).GetSucceeded() || e.ExecuteSlotCommand(context.Background(), nil).GetSucceeded() ||
		e.RevokeEpoch(nil, nil).GetSucceeded() || d.executes.Load() != 0 || d.revokes.Load() != 0 {
		t.Fatal("invalid input reached delegate")
	}
	e.Seal()
	e.Seal()
	if err := e.Wait(context.Background()); err != nil || !errors.Is(e.Wait(nil), ErrLifecycleWaitContext) {
		t.Fatal("sealed empty wait failed")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(e.Wait(cancelled), context.Canceled) {
		t.Fatal("cancelled wait was reported clean")
	}
	if e.ExecuteSlotCommand(context.Background(), lifecycleTestCommand()).GetErrorCode() != "host_lifecycle_stopping" ||
		e.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{}).GetSucceeded() || d.executes.Load() != 0 || d.revokes.Load() != 0 {
		t.Fatal("sealed executor admitted a new mutation")
	}
	if !reflect.DeepEqual(e.Snapshot(), d.snapshot) {
		t.Fatal("seal altered snapshot forwarding")
	}
	var absent *LifecycleExecutor
	absent.Seal()
	if !errors.Is(absent.Wait(context.Background()), ErrLifecycleExecutorConfig) || absent.ExecuteSlotCommand(context.Background(), lifecycleTestCommand()).GetSucceeded() {
		t.Fatal("nil wrapper did not fail closed")
	}
	zero := &LifecycleExecutor{}
	zero.Seal()
	if !errors.Is(zero.CloseAndWait(context.Background()), ErrLifecycleExecutorConfig) || zero.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{}).GetSucceeded() ||
		!reflect.DeepEqual(zero.Snapshot(), hostagent.NodeSnapshot{}) {
		t.Fatal("uninitialized wrapper did not fail closed")
	}
}

func TestLifecycleExecutorCancelledOldSessionCannotMutateAfterReconnect(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	d := &lifecycleDelegate{execute: func(context.Context, *executionv1.SlotCommand) *executionv1.CommandResult {
		close(entered)
		<-release // deliberately context-unaware in-flight provider
		return &executionv1.CommandResult{Succeeded: true, Slot: &executionv1.SlotObservation{Healthy: true}}
	}}
	e := lifecycleTestExecutor(t, d)
	old, cancelOld := context.WithCancel(context.Background())
	defer cancelOld()
	first := make(chan *executionv1.CommandResult, 1)
	go func() { first <- e.ExecuteSlotCommand(old, lifecycleTestCommand()) }()
	<-entered
	queuedRevoke := make(chan *executionv1.CommandResult, 1)
	go func() {
		queuedRevoke <- e.RevokeEpoch(old, &executionv1.RevokeEpochCommand{CommandId: "revoke-old", SlotId: "slot-1", ExecutionEpoch: 7})
	}()
	lifecycleActive(t, e, 2)
	cancelOld()
	if result := lifecycleReceive(t, queuedRevoke); result.GetSucceeded() || result.GetErrorCode() != "command_canceled" || d.revokes.Load() != 0 {
		t.Fatal("cancelled old-session revoke reached delegate")
	}
	lifecycleActive(t, e, 1)
	newSession := make(chan *executionv1.CommandResult, 1)
	go func() { newSession <- e.ExecuteSlotCommand(context.Background(), lifecycleTestCommand()) }()
	lifecycleActive(t, e, 2)
	if d.executes.Load() != 1 {
		t.Fatal("new session overlapped an old in-flight mutation")
	}
	e.Seal()
	if result := lifecycleReceive(t, newSession); result.GetSucceeded() || result.GetErrorCode() != "host_lifecycle_stopping" {
		t.Fatal("seal failed to release queued new-session command")
	}
	bounded, stop := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer stop()
	if err := e.Wait(bounded); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("context-unaware in-flight call was falsely reported stopped")
	}
	once.Do(func() { close(release) })
	if result := lifecycleReceive(t, first); result.GetSucceeded() || result.GetSlot().GetHealthy() || result.GetErrorCode() != "command_canceled" {
		t.Fatal("late success from cancelled provider escaped")
	}
	if err := e.Wait(context.Background()); err != nil || d.executes.Load() != 1 || d.revokes.Load() != 0 {
		t.Fatal("executor did not quiesce after real completion")
	}
}

func TestLifecycleExecutorQueueRespectsCommandDeadlineAndThenResumes(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	d := &lifecycleDelegate{execute: func(context.Context, *executionv1.SlotCommand) *executionv1.CommandResult {
		close(entered)
		<-release
		return &executionv1.CommandResult{Succeeded: true}
	}}
	e := lifecycleTestExecutor(t, d)
	first := make(chan *executionv1.CommandResult, 1)
	go func() { first <- e.ExecuteSlotCommand(context.Background(), lifecycleTestCommand()) }()
	<-entered
	short := lifecycleTestCommand()
	short.Deadline = timestamppb.New(time.Now().Add(20 * time.Millisecond))
	if result := e.ExecuteSlotCommand(context.Background(), short); result.GetSucceeded() || result.GetErrorCode() != "command_deadline_exceeded" || d.executes.Load() != 1 {
		t.Fatal("queued command ignored its deadline")
	}
	queued := make(chan *executionv1.CommandResult, 1)
	go func() {
		queued <- e.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{CommandId: "new-session"})
	}()
	lifecycleActive(t, e, 2)
	once.Do(func() { close(release) })
	if !lifecycleReceive(t, first).GetSucceeded() || !lifecycleReceive(t, queued).GetSucceeded() || d.revokes.Load() != 1 {
		t.Fatal("live next-session call did not resume after previous completion")
	}
	if err := e.CloseAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleExecutorPassesContextAndCountsPanickingDelegate(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "synthetic")
	command := lifecycleTestCommand()
	d := &lifecycleDelegate{execute: func(received context.Context, _ *executionv1.SlotCommand) *executionv1.CommandResult {
		deadline, ok := received.Deadline()
		if !ok || !deadline.Equal(command.Deadline.AsTime()) || received.Value(key{}) != "synthetic" {
			t.Fatal("context values or deadline lost")
		}
		panic("synthetic delegate panic")
	}}
	e := lifecycleTestExecutor(t, d)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("delegate panic was silently hidden")
			}
		}()
		e.ExecuteSlotCommand(ctx, command)
	}()
	if err := e.CloseAndWait(context.Background()); err != nil {
		t.Fatal("panic retained an active admission")
	}
}

func TestLifecycleExecutorSealAdmissionRaceAndConcurrentWaiters(t *testing.T) {
	d := &lifecycleDelegate{}
	e := lifecycleTestExecutor(t, d)
	start := make(chan struct{})
	var calls sync.WaitGroup
	for index := 0; index < 128; index++ {
		calls.Add(1)
		go func(revoke bool) {
			defer calls.Done()
			<-start
			if revoke {
				e.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{})
			} else {
				e.ExecuteSlotCommand(context.Background(), lifecycleTestCommand())
			}
		}(index%2 == 0)
	}
	close(start)
	e.Seal()
	var waiters sync.WaitGroup
	for index := 0; index < 8; index++ {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			bounded, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := e.CloseAndWait(bounded); err != nil {
				t.Error(err)
			}
		}()
	}
	calls.Wait()
	waiters.Wait()
	before := d.executes.Load() + d.revokes.Load()
	for index := 0; index < 128; index++ {
		if e.ExecuteSlotCommand(context.Background(), lifecycleTestCommand()).GetSucceeded() || e.RevokeEpoch(context.Background(), &executionv1.RevokeEpochCommand{}).GetSucceeded() {
			t.Fatal("post-seal admission succeeded")
		}
	}
	if d.executes.Load()+d.revokes.Load() != before {
		t.Fatal("delegate changed after clean wait")
	}
	lifecycleActive(t, e, 0)
}
