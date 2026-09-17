package daemon

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
)

var (
	ErrLifecycleExecutorConfig    = errors.New("host lifecycle executor is invalid")
	ErrLifecycleExecutorSealed    = errors.New("host lifecycle executor is sealed")
	ErrLifecycleExecutorNotSealed = errors.New("host lifecycle executor must be sealed before waiting")
	ErrLifecycleWaitContext       = errors.New("host lifecycle wait context is invalid")
)

// LifecycleExecutor tracks both running and queued lifecycle calls across
// control sessions. A cancelled old session cannot leave a queued RevokeEpoch
// entering the delegate after a new session connects. No activation or ticket
// interface is exposed. Snapshot remains a read-only delegate operation.
type LifecycleExecutor struct {
	delegate hostagent.ControlCommandExecutor
	gate     chan struct{}
	stopping chan struct{}
	done     chan struct{}
	mu       sync.Mutex
	sealed   bool
	active   uint64
}

func NewLifecycleExecutor(delegate hostagent.ControlCommandExecutor) (*LifecycleExecutor, error) {
	if nilLifecycleDelegate(delegate) {
		return nil, ErrLifecycleExecutorConfig
	}
	e := &LifecycleExecutor{delegate: delegate, gate: make(chan struct{}, 1), stopping: make(chan struct{}), done: make(chan struct{})}
	e.gate <- struct{}{}
	return e, nil
}

func (e *LifecycleExecutor) ExecuteSlotCommand(ctx context.Context, command *executionv1.SlotCommand) *executionv1.CommandResult {
	if ctx == nil || command == nil {
		return lifecycleFailure(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), ErrLifecycleWaitContext)
	}
	// ControlClient's lifecycle-only workers carry the session context, not a
	// per-command deadline. Bound time spent waiting before the real executor.
	if deadline := command.GetDeadline(); deadline != nil && deadline.CheckValid() == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.AsTime())
		defer cancel()
	}
	if err := e.begin(ctx); err != nil {
		return lifecycleFailure(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), err)
	}
	defer e.end()
	if err := e.acquire(ctx); err != nil {
		return lifecycleFailure(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), err)
	}
	defer e.release()
	result := e.delegate.ExecuteSlotCommand(ctx, command)
	if err := ctx.Err(); err != nil {
		return lifecycleFailure(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), command.GetImageDigest(), err)
	}
	return result
}

func (e *LifecycleExecutor) RevokeEpoch(ctx context.Context, command *executionv1.RevokeEpochCommand) *executionv1.CommandResult {
	if ctx == nil || command == nil {
		return lifecycleFailure(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "", ErrLifecycleWaitContext)
	}
	if err := e.begin(ctx); err != nil {
		return lifecycleFailure(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "", err)
	}
	defer e.end()
	if err := e.acquire(ctx); err != nil {
		return lifecycleFailure(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "", err)
	}
	defer e.release()
	result := e.delegate.RevokeEpoch(ctx, command)
	if err := ctx.Err(); err != nil {
		return lifecycleFailure(command.GetCommandId(), command.GetSlotId(), command.GetExecutionEpoch(), "", err)
	}
	return result
}

func (e *LifecycleExecutor) Snapshot() hostagent.NodeSnapshot {
	if e == nil || e.delegate == nil {
		return hostagent.NodeSnapshot{}
	}
	return e.delegate.Snapshot()
}

// Seal atomically forbids admission and wakes queued calls. It does not pretend
// to cancel a delegate already executing: the supervisor must cancel its run
// context first and treat a Wait timeout as an unclean shutdown.
func (e *LifecycleExecutor) Seal() {
	if e == nil || e.delegate == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return
	}
	e.sealed = true
	close(e.stopping)
	if e.active == 0 {
		close(e.done)
	}
}

// Wait never reports quiescence while new calls can still enter. The caller
// supplies the independent shutdown deadline, not the cancelled run context.
// A timeout leaves the executor sealed and may be followed by another Wait.
func (e *LifecycleExecutor) Wait(ctx context.Context) error {
	if e == nil || e.delegate == nil {
		return ErrLifecycleExecutorConfig
	}
	if ctx == nil {
		return ErrLifecycleWaitContext
	}
	e.mu.Lock()
	sealed := e.sealed
	e.mu.Unlock()
	if !sealed {
		return ErrLifecycleExecutorNotSealed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("host lifecycle calls did not quiesce: %w", err)
	}
	select {
	case <-e.done:
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("host lifecycle calls did not quiesce: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("host lifecycle calls did not quiesce: %w", ctx.Err())
	}
}

func (e *LifecycleExecutor) CloseAndWait(ctx context.Context) error {
	e.Seal()
	return e.Wait(ctx)
}

func (e *LifecycleExecutor) begin(ctx context.Context) error {
	if e == nil || e.delegate == nil {
		return ErrLifecycleExecutorConfig
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return ErrLifecycleExecutorSealed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.active++
	return nil
}

func (e *LifecycleExecutor) end() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.active--
	if e.sealed && e.active == 0 {
		close(e.done)
	}
}

func (e *LifecycleExecutor) acquire(ctx context.Context) error {
	select {
	case <-e.stopping:
		return ErrLifecycleExecutorSealed
	case <-ctx.Done():
		return ctx.Err()
	case <-e.gate:
	}
	// The token and cancellation/seal can become ready together. Recheck after
	// acquiring, before a queued call is allowed to touch the provider.
	e.mu.Lock()
	err := ctx.Err()
	if e.sealed {
		err = ErrLifecycleExecutorSealed
	}
	e.mu.Unlock()
	if err != nil {
		e.release()
	}
	return err
}

func (e *LifecycleExecutor) release() { e.gate <- struct{}{} }

func nilLifecycleDelegate(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func lifecycleFailure(commandID, slotID string, epoch uint64, image string, err error) *executionv1.CommandResult {
	code := "host_lifecycle_stopping"
	if errors.Is(err, context.DeadlineExceeded) {
		code = "command_deadline_exceeded"
	} else if errors.Is(err, context.Canceled) {
		code = "command_canceled"
	} else if errors.Is(err, ErrLifecycleWaitContext) || errors.Is(err, ErrLifecycleExecutorConfig) {
		code = "invalid_command"
	}
	if epoch == 0 {
		epoch = 1
	}
	return &executionv1.CommandResult{CommandId: commandID, Succeeded: false, ErrorCode: code,
		ErrorMessage: "host lifecycle command did not complete",
		Slot:         &executionv1.SlotObservation{SlotId: slotID, ExecutionEpoch: epoch, ActualState: "failed", Healthy: false, Reason: code, ImageDigest: image}}
}

var _ hostagent.ControlCommandExecutor = (*LifecycleExecutor)(nil)
