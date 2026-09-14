package dataplane

import (
	"context"
	"errors"
	"io"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type inbound struct {
	request *executionv1.ExecuteRequest
	err     error
}
type outbound struct {
	response *executionv1.ExecuteResponse
	err      error
}

func (s *Server) Execute(stream grpc.BidiStreamingServer[executionv1.ExecuteRequest, executionv1.ExecuteResponse]) error {
	if err := authorize(stream.Context()); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(stream.Context(), s.config.MaxExecutionDuration)
	defer cancel()
	first := make(chan inbound, 1)
	go func() { request, err := stream.Recv(); first <- inbound{request, err} }()
	timer := time.NewTimer(s.config.FirstFrameTimeout)
	defer timer.Stop()
	var initial inbound
	select {
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-timer.C:
		return status.Error(codes.DeadlineExceeded, "execution begin timeout")
	case initial = <-first:
	}
	if initial.err != nil {
		if errors.Is(initial.err, io.EOF) {
			return invalidRequest()
		}
		return workerError(ctx, initial.err)
	}
	begin, binding, err := s.begin(initial.request)
	if err != nil {
		return err
	}
	runtime, err := s.config.Resolver.Resolve(ctx, binding)
	if err != nil || runtime == nil {
		return bindingError(ctx, err)
	}
	fence := s.watchFence(ctx, binding)
	// Ticket issuance and the first worker Send can themselves block. Keep the
	// authority watcher active during that opening phase, not only after it.
	type openedExecution struct {
		execution Execution
		err       error
	}
	opened := make(chan openedExecution)
	go func() {
		execution, err := runtime.OpenExecution(ctx, begin)
		select {
		case opened <- openedExecution{execution, err}:
		case <-ctx.Done():
			if execution != nil {
				_ = execution.Close()
			}
		}
	}()
	var execution Execution
	select {
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	case <-fence:
		return status.Error(codes.FailedPrecondition, "execution binding is no longer active")
	case result := <-opened:
		if result.err != nil || result.execution == nil {
			if result.execution != nil {
				_ = result.execution.Close()
			}
			return workerError(ctx, result.err)
		}
		execution = result.execution
	}
	defer execution.Close()
	clientResult := make(chan error, 1)
	go func() { clientResult <- s.receiveClient(ctx, stream, execution) }()
	workerResults := make(chan outbound, 1)
	go func() {
		for {
			response, err := execution.Recv()
			select {
			case workerResults <- outbound{response, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var state responseState
	var sendResult <-chan error
	terminal := false
	for {
		workerReady := workerResults
		if sendResult != nil {
			workerReady = nil
		}
		select {
		case <-ctx.Done():
			return status.FromContextError(ctx.Err()).Err()
		case <-fence:
			return status.Error(codes.FailedPrecondition, "execution binding is no longer active")
		case err := <-clientResult:
			clientResult = nil
			if err != nil {
				return err
			}
		case err := <-sendResult:
			sendResult = nil
			if err != nil {
				return workerError(ctx, err)
			}
			if terminal {
				return nil
			}
		case received := <-workerReady:
			if received.err != nil {
				if errors.Is(received.err, io.EOF) {
					return status.Error(codes.Unavailable, "worker stream ended without a terminal event")
				}
				return workerError(ctx, received.err)
			}
			response, done, err := s.response(received.response, &state)
			if err != nil {
				return err
			}
			terminal = done
			sent := make(chan error, 1)
			sendResult = sent
			// Send can block on HTTP/2 flow control. Returning this handler lets
			// gRPC cancel its transport; do not wait for this pump during teardown.
			go func() { sent <- stream.Send(response) }()
		}
	}
}

func (s *Server) receiveClient(ctx context.Context, stream grpc.BidiStreamingServer[executionv1.ExecuteRequest, executionv1.ExecuteResponse], execution Execution) error {
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			if err := execution.CloseSend(); err != nil {
				return workerError(ctx, err)
			}
			return nil
		}
		if err != nil {
			return workerError(ctx, err)
		}
		if err := s.followup(request); err != nil {
			return err
		}
		if request.GetCancel() != nil {
			return status.Error(codes.Canceled, "execution canceled by client")
		}
		if err := execution.SendToolResult(request.GetToolResult()); err != nil {
			return workerError(ctx, err)
		}
	}
}

// Authority reads run with a bounded deadline. At most one read is outstanding
// per execution; if an implementation fails to honor cancellation, no further
// reads are launched and the stream still fails closed on the timeout.
func (s *Server) watchFence(ctx context.Context, binding Binding) <-chan struct{} {
	failed := make(chan struct{})
	go func() {
		ticker := time.NewTicker(s.config.FenceInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			check, cancel := context.WithTimeout(ctx, s.config.FenceInterval)
			result := make(chan error, 1)
			go func() { result <- s.config.Resolver.Validate(check, binding) }()
			select {
			case <-ctx.Done():
				cancel()
				return
			case <-check.Done():
				cancel()
				close(failed)
				return
			case err := <-result:
				cancel()
				if err != nil {
					close(failed)
					return
				}
			}
		}
	}()
	return failed
}
