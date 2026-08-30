package service

import (
	"bytes"
	"context"
	"errors"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type OnboardingIntentCreator interface {
	Create(ctx context.Context, request onboarding.CreateRequest) (onboarding.Receipt, error)
}

type OnboardingIntentRecoverer interface {
	Recover(ctx context.Context, request onboarding.RecoverRequest) (onboarding.Receipt, error)
}

type OnboardingIntentIntake interface {
	OnboardingIntentCreator
	OnboardingIntentRecoverer
}

type OnboardingResultReader interface {
	GetProvisioningResult(ctx context.Context, intentID, accountID string, desiredGeneration uint64) (onboarding.ProvisioningOutcome, error)
}

type OnboardingIntakeAuthorizer interface {
	AuthorizeOnboardingIntake(ctx context.Context) error
}

type MTLSServiceAuthorizer struct {
	ExpectedServiceID string
}

func (a MTLSServiceAuthorizer) AuthorizeOnboardingIntake(ctx context.Context) error {
	if a.ExpectedServiceID == "" || ctx == nil {
		return errors.New("onboarding intake peer is unauthorized")
	}
	remote, ok := peer.FromContext(ctx)
	if !ok || remote.AuthInfo == nil {
		return errors.New("onboarding intake peer is unauthorized")
	}
	tlsInfo, ok := remote.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 || len(tlsInfo.State.VerifiedChains) == 0 ||
		len(tlsInfo.State.VerifiedChains[0]) == 0 ||
		!bytes.Equal(tlsInfo.State.PeerCertificates[0].Raw, tlsInfo.State.VerifiedChains[0][0].Raw) {
		return errors.New("onboarding intake peer is unauthorized")
	}
	serviceID, err := pki.ServiceIDFromCertificate(tlsInfo.State.PeerCertificates[0])
	if err != nil || serviceID != a.ExpectedServiceID {
		return errors.New("onboarding intake peer is unauthorized")
	}
	return nil
}

type OnboardingIntakeServer struct {
	executionv1.UnimplementedOnboardingIntakeServiceServer
	intents    OnboardingIntentIntake
	results    OnboardingResultReader
	authorizer OnboardingIntakeAuthorizer
}

func NewOnboardingIntakeServer(intents OnboardingIntentIntake, results OnboardingResultReader, authorizer OnboardingIntakeAuthorizer) (*OnboardingIntakeServer, error) {
	if intents == nil || results == nil || authorizer == nil {
		return nil, errors.New("onboarding intake dependencies are required")
	}
	return &OnboardingIntakeServer{intents: intents, results: results, authorizer: authorizer}, nil
}

func (s *OnboardingIntakeServer) GetOnboardingResult(
	ctx context.Context,
	request *executionv1.GetOnboardingResultRequest,
) (*executionv1.GetOnboardingResultResponse, error) {
	if s == nil || s.results == nil || s.authorizer == nil || request == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid onboarding result request")
	}
	if err := s.authorizer.AuthorizeOnboardingIntake(ctx); err != nil {
		return nil, status.Error(codes.PermissionDenied, "onboarding intake peer is unauthorized")
	}
	if credential.ValidateTransportID(request.GetIntentId()) != nil ||
		credential.ValidateTransportID(request.GetAccountId()) != nil || request.GetDesiredGeneration() == 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid onboarding result request")
	}
	outcome, err := s.results.GetProvisioningResult(ctx, request.GetIntentId(), request.GetAccountId(), request.GetDesiredGeneration())
	if err != nil {
		switch {
		case errors.Is(err, onboarding.ErrResultPending):
			return nil, status.Error(codes.FailedPrecondition, "onboarding result is not available")
		case errors.Is(err, context.Canceled):
			return nil, status.Error(codes.Canceled, "onboarding result request was canceled")
		case errors.Is(err, context.DeadlineExceeded):
			return nil, status.Error(codes.DeadlineExceeded, "onboarding result request timed out")
		case errors.Is(err, onboarding.ErrResultProjectionRejected):
			return nil, status.Error(codes.Internal, "onboarding result is invalid")
		default:
			return nil, status.Error(codes.Unavailable, "onboarding result storage is unavailable")
		}
	}
	if outcome.Validate() != nil || outcome.IntentID != request.GetIntentId() || outcome.AccountID != request.GetAccountId() ||
		outcome.DesiredGeneration != request.GetDesiredGeneration() {
		return nil, status.Error(codes.Internal, "onboarding result is invalid")
	}
	finishedAt := timestamppb.New(outcome.FinishedAt.UTC())
	if finishedAt.CheckValid() != nil {
		return nil, status.Error(codes.Internal, "onboarding result is invalid")
	}
	response := &executionv1.GetOnboardingResultResponse{
		IntentId: outcome.IntentID, AccountId: outcome.AccountID, DesiredGeneration: outcome.DesiredGeneration,
		SlotId: outcome.SlotID, ExecutionEpoch: outcome.ExecutionEpoch, FinishedAt: finishedAt,
	}
	switch outcome.Status {
	case onboarding.ProvisioningOutcomeSucceeded:
		result := outcome.Result
		if result == nil {
			return nil, status.Error(codes.Internal, "onboarding result is invalid")
		}
		authType, ok := intakeProtoAuthType(result.Projection.AuthType)
		if !ok {
			return nil, status.Error(codes.Internal, "onboarding result is invalid")
		}
		projectedAt := timestamppb.New(result.CreatedAt.UTC())
		if projectedAt.CheckValid() != nil {
			return nil, status.Error(codes.Internal, "onboarding result is invalid")
		}
		response.Status = executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_SUCCEEDED
		response.AuthType = authType
		response.EmailAddress = result.Projection.EmailAddress
		response.OrganizationId = result.Projection.OrganizationID
		response.UpstreamAccountId = result.Projection.UpstreamAccountID
		response.Scope = result.Projection.Scope
		response.SubscriptionType = result.Projection.SubscriptionType
		response.RateLimitTier = result.Projection.RateLimitTier
		response.ProjectedAt = projectedAt
		if !result.Projection.ExpiresAt.IsZero() {
			response.ExpiresAt = timestamppb.New(result.Projection.ExpiresAt.UTC())
			if response.ExpiresAt.CheckValid() != nil {
				return nil, status.Error(codes.Internal, "onboarding result is invalid")
			}
		}
	case onboarding.ProvisioningOutcomeFailed:
		response.Status = executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_FAILED
		response.ErrorCode = outcome.ErrorCode
		response.ErrorSummary = outcome.ErrorSummary
	case onboarding.ProvisioningOutcomeExpired:
		response.Status = executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_EXPIRED
		response.ErrorCode = outcome.ErrorCode
		response.ErrorSummary = outcome.ErrorSummary
	default:
		return nil, status.Error(codes.Internal, "onboarding result is invalid")
	}
	return response, nil
}

func (s *OnboardingIntakeServer) CreateOnboardingIntent(
	ctx context.Context,
	request *executionv1.CreateOnboardingIntentRequest,
) (*executionv1.CreateOnboardingIntentResponse, error) {
	if request != nil {
		defer destroyOnboardingIntakeRequest(request)
	}
	if s == nil || s.intents == nil || s.authorizer == nil || request == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid onboarding intent request")
	}
	if err := s.authorizer.AuthorizeOnboardingIntake(ctx); err != nil {
		return nil, status.Error(codes.PermissionDenied, "onboarding intake peer is unauthorized")
	}
	source, authType, ok := intakeWorkerTypes(request.GetSource(), request.GetAuthType())
	if !ok || credential.ValidateTransportID(request.GetIdempotencyKey()) != nil ||
		credential.ValidateTransportID(request.GetAccountId()) != nil || request.GetDesiredGeneration() == 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid onboarding intent request")
	}
	input := &worker.OnboardingInput{
		Source: source, AuthType: authType, Secret: request.Secret, Auxiliary: request.Auxiliary,
	}
	if input.Validate() != nil {
		input.Destroy()
		return nil, status.Error(codes.InvalidArgument, "invalid onboarding intent request")
	}
	receipt, err := s.intents.Create(ctx, onboarding.CreateRequest{
		IdempotencyKey: request.GetIdempotencyKey(), AccountID: request.GetAccountId(),
		DesiredGeneration: request.GetDesiredGeneration(), Input: input,
	})
	if err != nil {
		return nil, status.Error(codes.Unavailable, "onboarding intent could not be created")
	}
	responseSource, responseAuth, ok := intakeProtoTypes(receipt.Source, receipt.AuthType)
	if !ok || responseSource != request.GetSource() || responseAuth != request.GetAuthType() ||
		receipt.AccountID != request.GetAccountId() || receipt.DesiredGeneration != request.GetDesiredGeneration() ||
		credential.ValidateTransportID(receipt.IntentID) != nil || receipt.ExpiresAt.IsZero() {
		return nil, status.Error(codes.Internal, "onboarding intent receipt is invalid")
	}
	expiresAt := timestamppb.New(receipt.ExpiresAt.UTC())
	if expiresAt.CheckValid() != nil {
		return nil, status.Error(codes.Internal, "onboarding intent receipt is invalid")
	}
	return &executionv1.CreateOnboardingIntentResponse{
		IntentId: receipt.IntentID, AccountId: receipt.AccountID, DesiredGeneration: receipt.DesiredGeneration,
		Source: responseSource, AuthType: responseAuth, ExpiresAt: expiresAt,
	}, nil
}

func (s *OnboardingIntakeServer) RecoverOnboardingIntent(
	ctx context.Context,
	request *executionv1.RecoverOnboardingIntentRequest,
) (*executionv1.CreateOnboardingIntentResponse, error) {
	if s == nil || s.intents == nil || s.authorizer == nil || request == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid onboarding intent recovery request")
	}
	if err := s.authorizer.AuthorizeOnboardingIntake(ctx); err != nil {
		return nil, status.Error(codes.PermissionDenied, "onboarding intake peer is unauthorized")
	}
	source, authType, ok := intakeWorkerTypes(request.GetSource(), request.GetAuthType())
	if !ok || credential.ValidateTransportID(request.GetIdempotencyKey()) != nil ||
		credential.ValidateTransportID(request.GetAccountId()) != nil || request.GetDesiredGeneration() == 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid onboarding intent recovery request")
	}
	recovery := onboarding.RecoverRequest{
		IdempotencyKey: request.GetIdempotencyKey(), AccountID: request.GetAccountId(),
		DesiredGeneration: request.GetDesiredGeneration(), Source: source, AuthType: authType,
	}
	if recovery.Validate() != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid onboarding intent recovery request")
	}
	receipt, err := s.intents.Recover(ctx, recovery)
	if err != nil {
		switch {
		case errors.Is(err, onboarding.ErrIntentNotFound):
			return nil, status.Error(codes.NotFound, "onboarding intent receipt was not found")
		case errors.Is(err, onboarding.ErrIntentExpired):
			return nil, status.Error(codes.Aborted, "onboarding intent receipt has expired")
		case errors.Is(err, onboarding.ErrIntentUnavailable):
			return nil, status.Error(codes.FailedPrecondition, "onboarding intent receipt is not recoverable")
		case errors.Is(err, context.Canceled):
			return nil, status.Error(codes.Canceled, "onboarding intent recovery was canceled")
		case errors.Is(err, context.DeadlineExceeded):
			return nil, status.Error(codes.DeadlineExceeded, "onboarding intent recovery timed out")
		default:
			return nil, status.Error(codes.Unavailable, "onboarding intent receipt storage is unavailable")
		}
	}
	responseSource, responseAuth, ok := intakeProtoTypes(receipt.Source, receipt.AuthType)
	if !ok || responseSource != request.GetSource() || responseAuth != request.GetAuthType() ||
		receipt.AccountID != request.GetAccountId() || receipt.DesiredGeneration != request.GetDesiredGeneration() ||
		credential.ValidateTransportID(receipt.IntentID) != nil || receipt.ExpiresAt.IsZero() {
		return nil, status.Error(codes.Internal, "onboarding intent receipt is invalid")
	}
	expiresAt := timestamppb.New(receipt.ExpiresAt.UTC())
	if expiresAt.CheckValid() != nil {
		return nil, status.Error(codes.Internal, "onboarding intent receipt is invalid")
	}
	return &executionv1.CreateOnboardingIntentResponse{
		IntentId: receipt.IntentID, AccountId: receipt.AccountID, DesiredGeneration: receipt.DesiredGeneration,
		Source: responseSource, AuthType: responseAuth, ExpiresAt: expiresAt,
	}, nil
}

func intakeWorkerTypes(source executionv1.OnboardingSource, authType executionv1.OnboardingAuthType) (worker.OnboardingSource, string, bool) {
	sources := map[executionv1.OnboardingSource]worker.OnboardingSource{
		executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY:       worker.OnboardingSessionKey,
		executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE:        worker.OnboardingOAuthCode,
		executionv1.OnboardingSource_ONBOARDING_SOURCE_SETUP_TOKEN:       worker.OnboardingSetupToken,
		executionv1.OnboardingSource_ONBOARDING_SOURCE_API_KEY:           worker.OnboardingAPIKey,
		executionv1.OnboardingSource_ONBOARDING_SOURCE_COOKIE:            worker.OnboardingCookie,
		executionv1.OnboardingSource_ONBOARDING_SOURCE_CREDENTIAL_IMPORT: worker.OnboardingCredentialImport,
	}
	authTypes := map[executionv1.OnboardingAuthType]string{
		executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH:       worker.AuthTypeOAuth,
		executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_SETUP_TOKEN: worker.AuthTypeSetupToken,
		executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_API_KEY:     worker.AuthTypeAPIKey,
	}
	mappedSource, sourceOK := sources[source]
	mappedAuth, authOK := authTypes[authType]
	return mappedSource, mappedAuth, sourceOK && authOK
}

func intakeProtoTypes(source, authType string) (executionv1.OnboardingSource, executionv1.OnboardingAuthType, bool) {
	sources := map[string]executionv1.OnboardingSource{
		string(worker.OnboardingSessionKey):       executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY,
		string(worker.OnboardingOAuthCode):        executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE,
		string(worker.OnboardingSetupToken):       executionv1.OnboardingSource_ONBOARDING_SOURCE_SETUP_TOKEN,
		string(worker.OnboardingAPIKey):           executionv1.OnboardingSource_ONBOARDING_SOURCE_API_KEY,
		string(worker.OnboardingCookie):           executionv1.OnboardingSource_ONBOARDING_SOURCE_COOKIE,
		string(worker.OnboardingCredentialImport): executionv1.OnboardingSource_ONBOARDING_SOURCE_CREDENTIAL_IMPORT,
	}
	authTypes := map[string]executionv1.OnboardingAuthType{
		worker.AuthTypeOAuth:      executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		worker.AuthTypeSetupToken: executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_SETUP_TOKEN,
		worker.AuthTypeAPIKey:     executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_API_KEY,
	}
	mappedSource, sourceOK := sources[source]
	mappedAuth, authOK := authTypes[authType]
	return mappedSource, mappedAuth, sourceOK && authOK
}

func intakeProtoAuthType(authType string) (executionv1.OnboardingAuthType, bool) {
	authTypes := map[string]executionv1.OnboardingAuthType{
		worker.AuthTypeOAuth:      executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		worker.AuthTypeSetupToken: executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_SETUP_TOKEN,
		worker.AuthTypeAPIKey:     executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_API_KEY,
	}
	mapped, ok := authTypes[authType]
	return mapped, ok
}

func destroyOnboardingIntakeRequest(request *executionv1.CreateOnboardingIntentRequest) {
	if request == nil {
		return
	}
	for index := range request.Secret {
		request.Secret[index] = 0
	}
	for index := range request.Auxiliary {
		request.Auxiliary[index] = 0
	}
	request.Secret = nil
	request.Auxiliary = nil
}

var _ executionv1.OnboardingIntakeServiceServer = (*OnboardingIntakeServer)(nil)
