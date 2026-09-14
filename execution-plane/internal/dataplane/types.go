// Package dataplane exposes the private CCMAX-to-host execution boundary.
// It has no default listener, credential authority, or runtime provisioning.
package dataplane

import (
	"context"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
)

const (
	MaxMessageBytes = 32 << 20
	MaxHeaderBytes  = 32 << 10
)

type Binding struct {
	AccountID, SlotID, NodeID       string
	ExecutionEpoch, RouteGeneration uint64
}

// Implementations must honor context cancellation. Resolve must only connect
// an already assigned runtime; it must never create or start one.
type Resolver interface {
	Resolve(context.Context, Binding) (Runtime, error)
	Validate(context.Context, Binding) error
}

type Runtime interface {
	OpenExecution(context.Context, *executionv1.BeginExecution) (Execution, error)
	CountTokensRequest(context.Context, *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error)
}

// Close must cancel the execution's derived context and unblock pending worker
// Recv/Send calls. It must not destroy or stop the shared runtime instance.
type Execution interface {
	Recv() (*executionv1.ExecuteResponse, error)
	SendToolResult(*executionv1.ToolResult) error
	CloseSend() error
	Close() error
}

type Config struct {
	NodeID               string
	Resolver             Resolver
	FenceInterval        time.Duration
	MaxExecutionDuration time.Duration
	FirstFrameTimeout    time.Duration
	MaxMessageBytes      int
	MaxHeaderBytes       int
}
