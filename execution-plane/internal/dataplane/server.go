package dataplane

import (
	"context"
	"errors"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Server struct {
	executionv1.UnimplementedExecutionDataPlaneServiceServer
	config Config
}

func NewServer(config Config) (*Server, error) {
	if !validID(config.NodeID, 128) || config.Resolver == nil {
		return nil, errors.New("data-plane configuration is invalid")
	}
	if config.FenceInterval == 0 {
		config.FenceInterval = time.Second
	}
	if config.MaxExecutionDuration == 0 {
		config.MaxExecutionDuration = 10 * time.Minute
	}
	if config.FirstFrameTimeout == 0 {
		config.FirstFrameTimeout = 10 * time.Second
	}
	if config.MaxMessageBytes == 0 {
		config.MaxMessageBytes = MaxMessageBytes
	}
	if config.MaxHeaderBytes == 0 {
		config.MaxHeaderBytes = MaxHeaderBytes
	}
	if config.FenceInterval <= 0 || config.FenceInterval > time.Second || config.MaxExecutionDuration <= 0 || config.MaxExecutionDuration > 10*time.Minute ||
		config.FirstFrameTimeout <= 0 || config.FirstFrameTimeout > 10*time.Second || config.MaxMessageBytes <= 0 || config.MaxMessageBytes > MaxMessageBytes ||
		config.MaxHeaderBytes <= 0 || config.MaxHeaderBytes > MaxHeaderBytes {
		return nil, errors.New("data-plane limits are invalid")
	}
	return &Server{config: config}, nil
}

func (s *Server) Register(registrar grpc.ServiceRegistrar) {
	executionv1.RegisterExecutionDataPlaneServiceServer(registrar, s)
}

func (s *Server) ListModels(ctx context.Context, _ *executionv1.ListModelsRequest) (*executionv1.ListModelsResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.Unimplemented, "model catalog is not available")
}

func (s *Server) CountTokens(ctx context.Context, request *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	if err := authorize(ctx); err != nil {
		return nil, err
	}
	if request == nil || proto.Size(request) > s.config.MaxMessageBytes || !validID(request.GetRequestId(), 128) || !validMode(request.GetMode()) || len(request.GetAnthropicRequestJson()) == 0 {
		return nil, invalidRequest()
	}
	binding, err := s.binding(request.GetAccountId(), request.GetSlotId(), request.GetExecutionEpoch(), request.GetRouteGeneration())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, s.config.MaxExecutionDuration)
	defer cancel()
	runtime, err := s.config.Resolver.Resolve(ctx, binding)
	if err != nil || runtime == nil {
		return nil, bindingError(ctx, err)
	}
	fence := s.watchFence(ctx, binding)
	result := make(chan countResult, 1)
	go func() {
		response, err := runtime.CountTokensRequest(ctx, proto.Clone(request).(*executionv1.CountTokensRequest))
		result <- countResult{response, err}
	}()
	select {
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	case <-fence:
		return nil, status.Error(codes.FailedPrecondition, "execution binding is no longer active")
	case received := <-result:
		if received.err != nil {
			return nil, workerError(ctx, received.err)
		}
		if received.response == nil || proto.Size(received.response) > s.config.MaxMessageBytes || received.response.StatusCode < 100 || received.response.StatusCode > 599 {
			return nil, status.Error(codes.Internal, "worker response is invalid")
		}
		if err := s.config.Resolver.Validate(ctx, binding); err != nil {
			return nil, bindingError(ctx, err)
		}
		return received.response, nil
	}
}

type countResult struct {
	response *executionv1.CountTokensResponse
	err      error
}

func bindingError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	return status.Error(codes.FailedPrecondition, "execution binding is not active")
}

func workerError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return status.Error(codes.Canceled, "execution canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return status.Error(codes.DeadlineExceeded, "execution timed out")
	}
	return status.Error(codes.Unavailable, "worker execution failed")
}
