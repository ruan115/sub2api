package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"io"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxCredentialBundleBytes = 2 << 20

type Activation struct {
	CredentialLeaseID         string
	EncryptedCredentialBundle []byte
	ProxyLeaseID              string
}

type Activator interface {
	Activate(ctx context.Context, activation Activation) ([]executionv1.ExecutionMode, error)
}

type PerActivationCommitterActivator interface {
	ActivateWithCommitter(ctx context.Context, activation Activation, committer CredentialCommitter) ([]executionv1.ExecutionMode, error)
}

type CredentialTransportKeySource interface {
	CredentialTransportKey() (keyID string, publicKey []byte, err error)
}

type ExecutionStream interface {
	Context() context.Context
	Begin() *executionv1.BeginExecution
	Recv() (*executionv1.WorkerRuntimeServiceExecuteRequest, error)
	Send(response *executionv1.ExecuteResponse) error
}

type Executor interface {
	Execute(stream ExecutionStream) error
	CountTokens(ctx context.Context, request *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error)
}

type ModeHealthSource interface {
	ModeHealth(ctx context.Context) []ModeHealth
}

type ModeHealth struct {
	Mode          executionv1.ExecutionMode
	Healthy       bool
	ReasonCode    string
	ReasonMessage string
}

type RuntimeServer struct {
	executionv1.UnimplementedWorkerRuntimeServiceServer

	guard        *Guard
	identity     Identity
	activator    Activator
	executor     Executor
	healthSource ModeHealthSource
	imageDigest  string
}

type RuntimeServerConfig struct {
	Guard        *Guard
	Identity     Identity
	Activator    Activator
	Executor     Executor
	HealthSource ModeHealthSource
	ImageDigest  string
}

func NewRuntimeServer(config RuntimeServerConfig) (*RuntimeServer, error) {
	if config.Guard == nil {
		return nil, errors.New("worker guard is required")
	}
	if err := config.Identity.Validate(); err != nil {
		return nil, err
	}
	if config.Activator == nil || config.Executor == nil || config.HealthSource == nil {
		return nil, errors.New("worker activator, executor and health source are required")
	}
	if config.ImageDigest == "" {
		return nil, errors.New("worker image digest is required")
	}
	return &RuntimeServer{
		guard:        config.Guard,
		identity:     config.Identity,
		activator:    config.Activator,
		executor:     config.Executor,
		healthSource: config.HealthSource,
		imageDigest:  config.ImageDigest,
	}, nil
}

func (s *RuntimeServer) Register(registrar grpc.ServiceRegistrar) {
	executionv1.RegisterWorkerRuntimeServiceServer(registrar, s)
}

func (s *RuntimeServer) CredentialTransportKey(ctx context.Context, request *executionv1.CredentialTransportKeyRequest) (*executionv1.CredentialTransportKeyResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "credential transport key request is required")
	}
	if _, err := s.guard.Authorize(request.GetExecutionTicket(), "credential_key"); err != nil {
		return nil, authorizationError(err)
	}
	source, ok := s.activator.(CredentialTransportKeySource)
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "credential transport key is unavailable")
	}
	keyID, publicKey, err := source.CredentialTransportKey()
	if err != nil || keyID == "" || len(keyID) > 128 || len(publicKey) != 32 {
		return nil, status.Error(codes.Internal, "credential transport key is unavailable")
	}
	return &executionv1.CredentialTransportKeyResponse{
		SlotId: s.identity.SlotID, ExecutionEpoch: s.identity.Epoch,
		KeyId: keyID, PublicKey: append([]byte(nil), publicKey...),
	}, nil
}

func (s *RuntimeServer) Activate(ctx context.Context, request *executionv1.ActivateRequest) (*executionv1.ActivateResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "activation request is required")
	}
	if _, err := s.guard.Authorize(request.GetExecutionTicket(), "activate"); err != nil {
		return nil, authorizationError(err)
	}
	if request.GetCredentialLeaseId() == "" || request.GetProxyLeaseId() == "" {
		return nil, status.Error(codes.InvalidArgument, "credential and proxy lease ids are required")
	}
	if len(request.GetEncryptedCredentialBundle()) == 0 || len(request.GetEncryptedCredentialBundle()) > maxCredentialBundleBytes {
		return nil, status.Error(codes.InvalidArgument, "credential bundle size is invalid")
	}
	bundle := append([]byte(nil), request.GetEncryptedCredentialBundle()...)
	defer zero(bundle)
	defer zero(request.EncryptedCredentialBundle)
	modes, err := s.activator.Activate(ctx, Activation{
		CredentialLeaseID:         request.GetCredentialLeaseId(),
		EncryptedCredentialBundle: bundle,
		ProxyLeaseID:              request.GetProxyLeaseId(),
	})
	if err != nil {
		return nil, status.Error(codes.Internal, "worker activation failed")
	}
	return &executionv1.ActivateResponse{
		SlotId:         s.identity.SlotID,
		ExecutionEpoch: s.identity.Epoch,
		HealthyModes:   append([]executionv1.ExecutionMode(nil), modes...),
	}, nil
}

func (s *RuntimeServer) SecureActivate(stream grpc.BidiStreamingServer[executionv1.SecureActivateRequest, executionv1.SecureActivateResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "secure activation begin event is required")
		}
		return err
	}
	begin := first.GetBegin()
	if begin == nil {
		return status.Error(codes.InvalidArgument, "first secure activation event must be begin")
	}
	if _, err := s.guard.Authorize(begin.GetExecutionTicket(), "secure_activate"); err != nil {
		return authorizationError(err)
	}
	if begin.GetCredentialLeaseId() == "" || begin.GetProxyLeaseId() == "" ||
		len(begin.GetEncryptedCredentialBundle()) == 0 || len(begin.GetEncryptedCredentialBundle()) > maxCredentialBundleBytes {
		return status.Error(codes.InvalidArgument, "secure activation lease or credential bundle is invalid")
	}
	activator, ok := s.activator.(PerActivationCommitterActivator)
	if !ok {
		return status.Error(codes.FailedPrecondition, "secure activation is unavailable")
	}
	sink := &secureActivationStreamSink{
		stream:            stream,
		identity:          s.identity,
		credentialLeaseID: begin.GetCredentialLeaseId(),
		proxyLeaseID:      begin.GetProxyLeaseId(),
	}
	committer, err := NewEncryptedCredentialCommitter(rand.Reader, sink)
	if err != nil {
		return status.Error(codes.Internal, "secure activation is unavailable")
	}
	bundle := append([]byte(nil), begin.GetEncryptedCredentialBundle()...)
	defer zero(bundle)
	defer zero(begin.EncryptedCredentialBundle)
	modes, err := activator.ActivateWithCommitter(stream.Context(), Activation{
		CredentialLeaseID: begin.GetCredentialLeaseId(), EncryptedCredentialBundle: bundle,
		ProxyLeaseID: begin.GetProxyLeaseId(),
	}, committer)
	if err != nil {
		if contextErr := stream.Context().Err(); contextErr != nil {
			return status.FromContextError(contextErr).Err()
		}
		return status.Error(codes.Internal, "secure worker activation failed")
	}
	return stream.Send(&executionv1.SecureActivateResponse{Event: &executionv1.SecureActivateResponse_Completed{Completed: &executionv1.SecureActivationCompleted{
		SlotId: s.identity.SlotID, ExecutionEpoch: s.identity.Epoch,
		HealthyModes: append([]executionv1.ExecutionMode(nil), modes...),
	}}})
}

type secureActivationStreamSink struct {
	stream            executionv1.WorkerRuntimeService_SecureActivateServer
	identity          Identity
	credentialLeaseID string
	proxyLeaseID      string
	used              bool
}

func (s *secureActivationStreamSink) CommitSealedCredential(ctx context.Context, request SealedCredentialCommitRequest) (string, error) {
	if s == nil || s.stream == nil || s.used || ctx == nil || ctx.Err() != nil ||
		!sameString(request.AccountBinding, s.identity.AccountID) || request.SlotID != s.identity.SlotID ||
		request.ExecutionEpoch != s.identity.Epoch || request.CredentialLeaseID != s.credentialLeaseID ||
		request.ProxyLeaseID != s.proxyLeaseID || len(request.SealedCredentialBundle) == 0 ||
		len(request.SealedCredentialBundle) > maxCredentialBundleBytes {
		return "", ErrCredentialCommitRejected
	}
	s.used = true
	sealed := append([]byte(nil), request.SealedCredentialBundle...)
	defer zero(sealed)
	response := &executionv1.SecureActivateResponse{Event: &executionv1.SecureActivateResponse_CredentialCommit{CredentialCommit: &executionv1.SealedCredentialCommit{
		AccountBinding: request.AccountBinding, SlotId: request.SlotID, ExecutionEpoch: request.ExecutionEpoch,
		CredentialLeaseId: request.CredentialLeaseID, ProxyLeaseId: request.ProxyLeaseID,
		SealedCredentialBundle: sealed,
	}}}
	if err := s.stream.Send(response); err != nil {
		return "", ErrCredentialCommitRejected
	}
	acknowledgement, err := s.stream.Recv()
	if err != nil || acknowledgement.GetCredentialCommitAck() == nil || !validCredentialVersionID(acknowledgement.GetCredentialCommitAck().GetVersionId()) {
		return "", ErrCredentialCommitRejected
	}
	return acknowledgement.GetCredentialCommitAck().GetVersionId(), nil
}

func (s *RuntimeServer) Execute(stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return status.Error(codes.InvalidArgument, "begin event is required")
		}
		return err
	}
	begin := first.GetBegin()
	if begin == nil || begin.GetRequest() == nil {
		return status.Error(codes.InvalidArgument, "first event must be begin")
	}
	if _, err := s.guard.Authorize(begin.GetExecutionTicket(), "messages"); err != nil {
		return authorizationError(err)
	}
	if !sameString(begin.GetRequest().GetAccountId(), s.identity.AccountID) {
		return status.Error(codes.PermissionDenied, "execution account is not assigned to this worker")
	}
	return executionError(s.executor.Execute(&authorizedExecutionStream{
		stream: stream,
		begin:  begin.GetRequest(),
	}))
}

func (s *RuntimeServer) CountTokens(ctx context.Context, request *executionv1.WorkerRuntimeServiceCountTokensRequest) (*executionv1.WorkerRuntimeServiceCountTokensResponse, error) {
	if request == nil || request.GetRequest() == nil {
		return nil, status.Error(codes.InvalidArgument, "count_tokens request is required")
	}
	if _, err := s.guard.Authorize(request.GetExecutionTicket(), "count_tokens"); err != nil {
		return nil, authorizationError(err)
	}
	if !sameString(request.GetRequest().GetAccountId(), s.identity.AccountID) {
		return nil, status.Error(codes.PermissionDenied, "execution account is not assigned to this worker")
	}
	response, err := s.executor.CountTokens(ctx, request.GetRequest())
	if err != nil {
		return nil, executionError(err)
	}
	if response == nil {
		return nil, status.Error(codes.Internal, "count_tokens executor returned no response")
	}
	return &executionv1.WorkerRuntimeServiceCountTokensResponse{Response: response}, nil
}

func executionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	type grpcStatus interface {
		GRPCStatus() *status.Status
	}
	var classified grpcStatus
	if errors.As(err, &classified) {
		return err
	}
	return status.Error(codes.Internal, "worker execution failed")
}

func (s *RuntimeServer) Health(ctx context.Context, request *executionv1.HealthRequest) (*executionv1.HealthResponse, error) {
	if request == nil {
		return nil, status.Error(codes.InvalidArgument, "health request is required")
	}
	if _, err := s.guard.Authorize(request.GetExecutionTicket(), "health"); err != nil {
		return nil, authorizationError(err)
	}
	modeHealth := s.healthSource.ModeHealth(ctx)
	response := &executionv1.HealthResponse{
		SlotId:         s.identity.SlotID,
		ExecutionEpoch: s.identity.Epoch,
		ImageDigest:    s.imageDigest,
		Modes:          make([]*executionv1.ModeHealth, 0, len(modeHealth)),
	}
	for _, mode := range modeHealth {
		response.Modes = append(response.Modes, &executionv1.ModeHealth{
			Mode:          mode.Mode,
			Healthy:       mode.Healthy,
			ReasonCode:    mode.ReasonCode,
			ReasonMessage: mode.ReasonMessage,
		})
	}
	return response, nil
}

type authorizedExecutionStream struct {
	stream grpc.BidiStreamingServer[executionv1.WorkerRuntimeServiceExecuteRequest, executionv1.WorkerRuntimeServiceExecuteResponse]
	begin  *executionv1.BeginExecution
}

func (s *authorizedExecutionStream) Context() context.Context {
	return s.stream.Context()
}

func (s *authorizedExecutionStream) Begin() *executionv1.BeginExecution {
	return s.begin
}

func (s *authorizedExecutionStream) Recv() (*executionv1.WorkerRuntimeServiceExecuteRequest, error) {
	request, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}
	if request.GetBegin() != nil {
		return nil, status.Error(codes.InvalidArgument, "begin event may only appear once")
	}
	if request.GetToolResult() == nil && request.GetCancel() == nil {
		return nil, status.Error(codes.InvalidArgument, "tool_result or cancel event is required")
	}
	return request, nil
}

func (s *authorizedExecutionStream) Send(response *executionv1.ExecuteResponse) error {
	if response == nil {
		return status.Error(codes.Internal, "executor attempted to send an empty response")
	}
	return s.stream.Send(&executionv1.WorkerRuntimeServiceExecuteResponse{Response: response})
}

func authorizationError(err error) error {
	switch {
	case errors.Is(err, ErrIdentityMismatch), errors.Is(err, ErrScopeDenied):
		return status.Error(codes.PermissionDenied, "execution ticket is not authorized")
	case errors.Is(err, ErrReplay):
		return status.Error(codes.AlreadyExists, "execution ticket was already used")
	case errors.Is(err, ticket.ErrExpired), errors.Is(err, ticket.ErrMalformed), errors.Is(err, ticket.ErrSignature):
		return status.Error(codes.Unauthenticated, "execution ticket is invalid")
	default:
		return status.Error(codes.Unauthenticated, "execution ticket is invalid")
	}
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ executionv1.WorkerRuntimeServiceServer = (*RuntimeServer)(nil)
