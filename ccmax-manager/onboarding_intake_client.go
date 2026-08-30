package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/ccmax-manager/internal/executionv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxRuntimeOnboardingMaterialBytes  = 1 << 20
	defaultRuntimeOnboardingRPCTimeout = 30 * time.Second
	minRuntimeOnboardingRPCTimeout     = time.Second
	maxRuntimeOnboardingRPCTimeout     = 2 * time.Minute
)

var (
	errRuntimeOnboardingIntake           = errors.New("runtime onboarding intake failed")
	errRuntimeOnboardingUnavailable      = errors.New("runtime onboarding intake is unavailable")
	errRuntimeOnboardingTimeout          = errors.New("runtime onboarding intake timed out")
	errRuntimeOnboardingReceiptNotFound  = errors.New("runtime onboarding receipt was not found")
	errRuntimeOnboardingReceiptExpired   = errors.New("runtime onboarding receipt expired")
	errRuntimeOnboardingReceiptConflict  = errors.New("runtime onboarding receipt conflicts with durable state")
	errRuntimeOnboardingMaterialRequired = errors.New("runtime onboarding material must be resubmitted")
	errRuntimeOnboardingResultPending    = errors.New("runtime onboarding result is pending")
)

const (
	runtimeOnboardingResultSucceeded = "succeeded"
	runtimeOnboardingResultFailed    = "failed"
	runtimeOnboardingResultExpired   = "expired"
	runtimeOnboardingErrorFailed     = "onboarding_failed"
	runtimeOnboardingErrorExpired    = "onboarding_expired"
	runtimeOnboardingSummaryFailed   = "onboarding workflow failed"
	runtimeOnboardingSummaryExpired  = "onboarding request expired"
)

type runtimeOnboardingMaterial struct {
	Source    string
	AuthType  string
	Secret    []byte
	Auxiliary []byte
}

func (m *runtimeOnboardingMaterial) Destroy() {
	if m == nil {
		return
	}
	zeroRuntimeOnboardingBytes(m.Secret)
	zeroRuntimeOnboardingBytes(m.Auxiliary)
	m.Secret = nil
	m.Auxiliary = nil
}

type onboardingIntakeRPC interface {
	CreateOnboardingIntent(ctx context.Context, in *executionv1.CreateOnboardingIntentRequest, opts ...grpc.CallOption) (*executionv1.CreateOnboardingIntentResponse, error)
	RecoverOnboardingIntent(ctx context.Context, in *executionv1.RecoverOnboardingIntentRequest, opts ...grpc.CallOption) (*executionv1.CreateOnboardingIntentResponse, error)
	GetOnboardingResult(ctx context.Context, in *executionv1.GetOnboardingResultRequest, opts ...grpc.CallOption) (*executionv1.GetOnboardingResultResponse, error)
}

type runtimeOnboardingIntentIntake interface {
	Create(ctx context.Context, idempotencyKey string, accountID int64, desiredGeneration uint64, material *runtimeOnboardingMaterial) (runtimeOnboardingIntentReceipt, error)
	Recover(ctx context.Context, idempotencyKey string, accountID int64, desiredGeneration uint64, source, authType string) (runtimeOnboardingIntentReceipt, error)
}

type runtimeOnboardingResult struct {
	IntentID          string
	AccountID         int64
	DesiredGeneration uint64
	SlotID            string
	ExecutionEpoch    uint64
	AuthType          string
	EmailAddress      string
	OrganizationID    string
	UpstreamAccountID string
	Scope             string
	SubscriptionType  string
	RateLimitTier     string
	ExpiresAt         time.Time
	ProjectedAt       time.Time
	Status            string
	ErrorCode         string
	ErrorSummary      string
	FinishedAt        time.Time
}

type runtimeOnboardingIntakeClient struct {
	service    onboardingIntakeRPC
	close      func() error
	now        func() time.Time
	rpcTimeout time.Duration
}

func dialRuntimeOnboardingIntake(endpoint string, files executionTLSFiles) (*runtimeOnboardingIntakeClient, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, errRuntimeOnboardingIntake
	}
	transport, err := loadExecutionTransportCredentials(files)
	if err != nil {
		return nil, errRuntimeOnboardingIntake
	}
	connection, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(transport),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(maxRuntimeOnboardingMaterialBytes+(64<<10)),
			grpc.MaxCallRecvMsgSize(64<<10),
		),
	)
	if err != nil {
		return nil, errRuntimeOnboardingIntake
	}
	return &runtimeOnboardingIntakeClient{
		service: executionv1.NewOnboardingIntakeServiceClient(connection), close: connection.Close,
		now: time.Now, rpcTimeout: defaultRuntimeOnboardingRPCTimeout,
	}, nil
}

func newRuntimeOnboardingIntakeClientFromEnv() (*runtimeOnboardingIntakeClient, error) {
	if !envEnabled("CCMAX_EXECUTION_ONBOARDING_ENABLED") {
		return nil, nil
	}
	endpoint := strings.TrimSpace(os.Getenv("CCMAX_EXECUTION_ONBOARDING_ENDPOINT"))
	if endpoint == "" {
		return nil, errors.New("CCMAX_EXECUTION_ONBOARDING_ENABLED requires CCMAX_EXECUTION_ONBOARDING_ENDPOINT")
	}
	timeout, err := parseRuntimeOnboardingRPCTimeout(os.Getenv("CCMAX_EXECUTION_ONBOARDING_RPC_TIMEOUT"))
	if err != nil {
		return nil, err
	}
	client, err := dialRuntimeOnboardingIntake(endpoint, executionTLSFiles{
		CAFile: os.Getenv("CCMAX_EXECUTION_CA_FILE"), ClientCertFile: os.Getenv("CCMAX_EXECUTION_CLIENT_CERT_FILE"),
		ClientKeyFile: os.Getenv("CCMAX_EXECUTION_CLIENT_KEY_FILE"), ServerName: os.Getenv("CCMAX_EXECUTION_SERVER_NAME"),
	})
	if err != nil {
		return nil, err
	}
	client.rpcTimeout = timeout
	return client, nil
}

func parseRuntimeOnboardingRPCTimeout(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultRuntimeOnboardingRPCTimeout, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < minRuntimeOnboardingRPCTimeout || timeout > maxRuntimeOnboardingRPCTimeout {
		return 0, fmt.Errorf("CCMAX_EXECUTION_ONBOARDING_RPC_TIMEOUT must be between %s and %s",
			minRuntimeOnboardingRPCTimeout, maxRuntimeOnboardingRPCTimeout)
	}
	return timeout, nil
}

func (c *runtimeOnboardingIntakeClient) Close() error {
	if c == nil || c.close == nil {
		return nil
	}
	return c.close()
}

// Create consumes and erases material on every path. The returned value is the
// only object permitted to enter the CCMAX account/outbox transaction.
func (c *runtimeOnboardingIntakeClient) Create(
	ctx context.Context,
	idempotencyKey string,
	accountID int64,
	desiredGeneration uint64,
	material *runtimeOnboardingMaterial,
) (runtimeOnboardingIntentReceipt, error) {
	if material != nil {
		defer material.Destroy()
	}
	if c == nil || c.service == nil || ctx == nil || ctx.Err() != nil || material == nil ||
		!runtimeOpaqueIntentIDPattern.MatchString(idempotencyKey) || runtimeSecretString(idempotencyKey) ||
		accountID <= 0 || desiredGeneration == 0 ||
		len(material.Secret) == 0 || len(material.Secret) > maxRuntimeOnboardingMaterialBytes ||
		len(material.Auxiliary) > maxRuntimeOnboardingMaterialBytes {
		return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingIntake
	}
	source, authType, ok := runtimeOnboardingProtoTypes(material.Source, material.AuthType)
	if !ok {
		return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingIntake
	}
	request := &executionv1.CreateOnboardingIntentRequest{
		IdempotencyKey: idempotencyKey, AccountId: strconv.FormatInt(accountID, 10), DesiredGeneration: desiredGeneration,
		Source: source, AuthType: authType, Secret: material.Secret, Auxiliary: material.Auxiliary,
	}
	defer destroyRuntimeOnboardingRequest(request)
	rpcCtx, cancel := c.rpcContext(ctx)
	defer cancel()
	response, err := c.service.CreateOnboardingIntent(rpcCtx, request)
	if err != nil {
		return runtimeOnboardingIntentReceipt{}, classifyRuntimeOnboardingRPCError(rpcCtx, err)
	}
	return c.validateReceiptResponse(response, idempotencyKey, accountID, desiredGeneration, source, authType)
}

func (c *runtimeOnboardingIntakeClient) Recover(
	ctx context.Context,
	idempotencyKey string,
	accountID int64,
	desiredGeneration uint64,
	sourceType string,
	authTypeName string,
) (runtimeOnboardingIntentReceipt, error) {
	if c == nil || c.service == nil || ctx == nil || ctx.Err() != nil ||
		!runtimeOpaqueIntentIDPattern.MatchString(idempotencyKey) || runtimeSecretString(idempotencyKey) ||
		accountID <= 0 || desiredGeneration == 0 {
		return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingIntake
	}
	source, authType, ok := runtimeOnboardingProtoTypes(sourceType, authTypeName)
	if !ok {
		return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingIntake
	}
	rpcCtx, cancel := c.rpcContext(ctx)
	defer cancel()
	response, err := c.service.RecoverOnboardingIntent(rpcCtx, &executionv1.RecoverOnboardingIntentRequest{
		IdempotencyKey:    idempotencyKey,
		AccountId:         strconv.FormatInt(accountID, 10),
		DesiredGeneration: desiredGeneration,
		Source:            source,
		AuthType:          authType,
	})
	if err != nil {
		switch status.Code(err) {
		case codes.NotFound:
			return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingReceiptNotFound
		case codes.Aborted:
			return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingReceiptExpired
		case codes.FailedPrecondition:
			return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingReceiptConflict
		default:
			return runtimeOnboardingIntentReceipt{}, classifyRuntimeOnboardingRPCError(rpcCtx, err)
		}
	}
	return c.validateReceiptResponse(response, idempotencyKey, accountID, desiredGeneration, source, authType)
}

func (c *runtimeOnboardingIntakeClient) validateReceiptResponse(
	response *executionv1.CreateOnboardingIntentResponse,
	idempotencyKey string,
	accountID int64,
	desiredGeneration uint64,
	source executionv1.OnboardingSource,
	authType executionv1.OnboardingAuthType,
) (runtimeOnboardingIntentReceipt, error) {
	if c == nil || response == nil || !runtimeOpaqueIntentIDPattern.MatchString(response.GetIntentId()) ||
		runtimeSecretString(response.GetIntentId()) || response.GetAccountId() != strconv.FormatInt(accountID, 10) ||
		response.GetDesiredGeneration() != desiredGeneration || response.GetSource() != source ||
		response.GetAuthType() != authType || response.GetExpiresAt() == nil || response.GetExpiresAt().CheckValid() != nil {
		return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingIntake
	}
	expiresAt := response.GetExpiresAt().AsTime().UTC()
	now := c.now
	if now == nil {
		now = time.Now
	}
	if !expiresAt.After(now().UTC()) {
		return runtimeOnboardingIntentReceipt{}, errRuntimeOnboardingIntake
	}
	return runtimeOnboardingIntentReceipt{
		IdempotencyKey: idempotencyKey, IntentID: response.GetIntentId(), AccountID: accountID,
		DesiredGeneration: desiredGeneration, ExpiresAt: expiresAt,
	}, nil
}

func (c *runtimeOnboardingIntakeClient) GetResult(
	ctx context.Context,
	intentID string,
	accountID int64,
	desiredGeneration uint64,
) (runtimeOnboardingResult, error) {
	if c == nil || c.service == nil || ctx == nil || ctx.Err() != nil ||
		!runtimeOpaqueIntentIDPattern.MatchString(intentID) || runtimeSecretString(intentID) ||
		accountID <= 0 || desiredGeneration == 0 {
		return runtimeOnboardingResult{}, errRuntimeOnboardingIntake
	}
	request := &executionv1.GetOnboardingResultRequest{
		IntentId: intentID, AccountId: strconv.FormatInt(accountID, 10), DesiredGeneration: desiredGeneration,
	}
	rpcCtx, cancel := c.rpcContext(ctx)
	defer cancel()
	response, err := c.service.GetOnboardingResult(rpcCtx, request)
	if status.Code(err) == codes.FailedPrecondition {
		return runtimeOnboardingResult{}, errRuntimeOnboardingResultPending
	}
	if err != nil {
		return runtimeOnboardingResult{}, classifyRuntimeOnboardingRPCError(rpcCtx, err)
	}
	if response == nil || response.GetIntentId() != intentID || response.GetAccountId() != request.GetAccountId() ||
		response.GetDesiredGeneration() != desiredGeneration {
		return runtimeOnboardingResult{}, errRuntimeOnboardingIntake
	}
	finishedAt := time.Time{}
	if response.GetFinishedAt() != nil {
		if response.GetFinishedAt().CheckValid() != nil {
			return runtimeOnboardingResult{}, errRuntimeOnboardingIntake
		}
		finishedAt = response.GetFinishedAt().AsTime().UTC()
	}
	result := runtimeOnboardingResult{
		IntentID: intentID, AccountID: accountID, DesiredGeneration: desiredGeneration,
		SlotID: strings.TrimSpace(response.GetSlotId()), ExecutionEpoch: response.GetExecutionEpoch(),
		ErrorCode: strings.TrimSpace(response.GetErrorCode()), ErrorSummary: strings.TrimSpace(response.GetErrorSummary()),
		FinishedAt: finishedAt,
	}
	switch response.GetStatus() {
	case executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_UNSPECIFIED,
		executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_SUCCEEDED:
		result.Status = runtimeOnboardingResultSucceeded
	case executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_FAILED:
		result.Status = runtimeOnboardingResultFailed
	case executionv1.OnboardingResultStatus_ONBOARDING_RESULT_STATUS_EXPIRED:
		result.Status = runtimeOnboardingResultExpired
	default:
		return runtimeOnboardingResult{}, errRuntimeOnboardingIntake
	}
	if result.Status == runtimeOnboardingResultSucceeded {
		if response.GetProjectedAt() == nil || response.GetProjectedAt().CheckValid() != nil {
			return runtimeOnboardingResult{}, errRuntimeOnboardingIntake
		}
		authType, ok := runtimeOnboardingAuthTypeFromProto(response.GetAuthType())
		if !ok {
			return runtimeOnboardingResult{}, errRuntimeOnboardingIntake
		}
		result.AuthType = authType
		result.EmailAddress = strings.ToLower(strings.TrimSpace(response.GetEmailAddress()))
		result.OrganizationID = strings.TrimSpace(response.GetOrganizationId())
		result.UpstreamAccountID = strings.TrimSpace(response.GetUpstreamAccountId())
		result.Scope = strings.TrimSpace(response.GetScope())
		result.SubscriptionType = strings.TrimSpace(response.GetSubscriptionType())
		result.RateLimitTier = strings.TrimSpace(response.GetRateLimitTier())
		result.ProjectedAt = response.GetProjectedAt().AsTime().UTC()
		if response.GetExpiresAt() != nil {
			if response.GetExpiresAt().CheckValid() != nil {
				return runtimeOnboardingResult{}, errRuntimeOnboardingIntake
			}
			result.ExpiresAt = response.GetExpiresAt().AsTime().UTC()
		}
	}
	if !validRuntimeOnboardingResult(result) {
		return runtimeOnboardingResult{}, errRuntimeOnboardingIntake
	}
	return result, nil
}

func (c *runtimeOnboardingIntakeClient) rpcContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := c.rpcTimeout
	if timeout <= 0 {
		timeout = defaultRuntimeOnboardingRPCTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func classifyRuntimeOnboardingRPCError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return errRuntimeOnboardingTimeout
	}
	if status.Code(err) == codes.Unavailable {
		return errRuntimeOnboardingUnavailable
	}
	return errRuntimeOnboardingIntake
}

// requestRuntimeOnboardingWithMaterial resumes a CCMAX-side durable submission
// before crossing the execution intake boundary. A lost HTTP response can
// therefore replay the original account/generation binding instead of
// advancing to a new generation or creating a second account.
func (a *app) requestRuntimeOnboardingWithMaterial(
	ctx context.Context,
	intake runtimeOnboardingIntentIntake,
	idempotencyKey string,
	request runtimeTransitionRequest,
	material *runtimeOnboardingMaterial,
) (runtimeOutboxEvent, error) {
	if material != nil {
		defer material.Destroy()
	}
	if a == nil || a.db == nil || ctx == nil || ctx.Err() != nil || request.AccountID <= 0 ||
		!runtimeOpaqueIntentIDPattern.MatchString(idempotencyKey) || runtimeSecretString(idempotencyKey) || material == nil {
		return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
	}
	var submission runtimeOnboardingSubmission
	var err error
	if intake == nil {
		// A queued submission or already-persisted receipt is authoritative in
		// CCMAX and can be replayed while intake is offline. Never create a new
		// pending claim without an intake connection.
		submission, err = a.getRuntimeOnboardingSubmission(ctx, idempotencyKey)
		if err != nil {
			return runtimeOutboxEvent{}, errRuntimeOnboardingUnavailable
		}
		if !runtimeOnboardingSubmissionMatchesRequest(submission, idempotencyKey, request, material) {
			return runtimeOutboxEvent{}, errRuntimeMigration
		}
		submission, err = a.validateRuntimeOnboardingSubmissionAccountOrRefreshQueued(
			ctx, submission, idempotencyKey, request, material,
		)
		if err != nil {
			return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
		}
	} else {
		submission, err = a.ensureRuntimeOnboardingSubmission(ctx, idempotencyKey, request, material)
		if err != nil {
			if errors.Is(err, errRuntimeOnboardingIdempotency) {
				return runtimeOutboxEvent{}, errRuntimeMigration
			}
			return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
		}
	}

	for replayRound := 0; replayRound < 8; replayRound++ {
		if submission.Status == runtimeOnboardingSubmissionQueued {
			event, eventErr := a.queuedRuntimeOnboardingEvent(ctx, submission)
			if eventErr != nil {
				return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
			}
			return event, nil
		}

		now := time.Now().UTC()
		receipt, hasReceipt := runtimeOnboardingReceiptFromSubmission(submission)
		if hasReceipt && !runtimeOnboardingReceiptHasCommitMargin(receipt.ExpiresAt, now) {
			// No outbox event references an accepted-but-unusable receipt. The
			// attempt CAS fences its eventual late response.
			submission, err = a.advanceRuntimeOnboardingAttempt(ctx, submission)
			if err != nil {
				if errors.Is(err, errRuntimeOnboardingIdempotency) {
					return runtimeOutboxEvent{}, errRuntimeMigration
				}
				return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
			}
			if submission.Status == runtimeOnboardingSubmissionQueued {
				continue
			}
			if len(material.Secret) == 0 {
				return runtimeOutboxEvent{}, errRuntimeOnboardingMaterialRequired
			}
			continue
		}

		if !hasReceipt {
			if intake == nil {
				return runtimeOutboxEvent{}, errRuntimeOnboardingUnavailable
			}
			receipt, err = intake.Recover(ctx, submission.IntakeIdempotencyKey, submission.AccountID,
				submission.DesiredGeneration, submission.SourceType, submission.AuthType)
			switch {
			case err == nil:
			case errors.Is(err, errRuntimeOnboardingUnavailable), errors.Is(err, errRuntimeOnboardingTimeout):
				return runtimeOutboxEvent{}, err
			case errors.Is(err, errRuntimeOnboardingReceiptNotFound):
				if len(material.Secret) == 0 {
					return runtimeOutboxEvent{}, errRuntimeOnboardingMaterialRequired
				}
				receipt, err = intake.Create(ctx, submission.IntakeIdempotencyKey, submission.AccountID,
					submission.DesiredGeneration, material)
				if err != nil {
					if errors.Is(err, errRuntimeOnboardingUnavailable) || errors.Is(err, errRuntimeOnboardingTimeout) {
						return runtimeOutboxEvent{}, err
					}
					return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
				}
			case errors.Is(err, errRuntimeOnboardingReceiptExpired):
				submission, err = a.advanceRuntimeOnboardingAttempt(ctx, submission)
				if err != nil {
					if errors.Is(err, errRuntimeOnboardingIdempotency) {
						return runtimeOutboxEvent{}, errRuntimeMigration
					}
					return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
				}
				if submission.Status == runtimeOnboardingSubmissionQueued {
					continue
				}
				if len(material.Secret) == 0 {
					return runtimeOutboxEvent{}, errRuntimeOnboardingMaterialRequired
				}
				continue
			case errors.Is(err, errRuntimeOnboardingReceiptConflict):
				return runtimeOutboxEvent{}, errRuntimeMigration
			default:
				return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
			}
			submission, err = a.persistRuntimeOnboardingReceipt(ctx, idempotencyKey, submission, receipt, now)
			if err != nil {
				if errors.Is(err, errRuntimeOnboardingAttemptSuperseded) {
					// The successor may already have its own durable receipt or an
					// execution-plane intent recoverable without plaintext. Re-enter
					// the state machine first; only an exact Recover NotFound is
					// allowed to require material or issue another Create.
					continue
				}
				if replay, replayErr := a.getRuntimeOnboardingSubmission(ctx, idempotencyKey); replayErr == nil && replay.Status == runtimeOnboardingSubmissionQueued {
					if queued, queuedErr := a.queuedRuntimeOnboardingEvent(ctx, replay); queuedErr == nil {
						return queued, nil
					}
				}
				if errors.Is(err, errRuntimeOnboardingIdempotency) {
					return runtimeOutboxEvent{}, errRuntimeMigration
				}
				return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
			}
			receipt, hasReceipt = runtimeOnboardingReceiptFromSubmission(submission)
			if !hasReceipt {
				return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
			}
		}

		if !runtimeOnboardingReceiptHasCommitMargin(receipt.ExpiresAt, time.Now().UTC()) {
			submission, err = a.advanceRuntimeOnboardingAttempt(ctx, submission)
			if err != nil {
				if errors.Is(err, errRuntimeOnboardingIdempotency) {
					return runtimeOutboxEvent{}, errRuntimeMigration
				}
				return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
			}
			if submission.Status == runtimeOnboardingSubmissionQueued {
				continue
			}
			// Recover does not consume input material, so it can safely rotate an
			// unusably short receipt and continue. Create does consume and erase
			// its input; that path must require an explicit resubmission.
			if len(material.Secret) == 0 {
				return runtimeOutboxEvent{}, errRuntimeOnboardingMaterialRequired
			}
			continue
		}

		request.AccountID = submission.AccountID
		request.EventType = submission.EventType
		request.MigrationStatus = submission.MigrationStatus
		request.ExpectedProxyID = submission.ProxyID.Int64
		request.OnboardingKey = submission.IdempotencyKey
		event, transitionErr := a.requestRuntimeOnboardingTransition(ctx, request, receipt)
		if transitionErr == nil {
			return event, nil
		}
		// Another replica may have committed the exact transition after this
		// caller loaded the pending record. Return that durable event as the
		// successful idempotent replay.
		if replay, replayErr := a.getRuntimeOnboardingSubmission(ctx, idempotencyKey); replayErr == nil && replay.Status == runtimeOnboardingSubmissionQueued {
			if queued, queuedErr := a.queuedRuntimeOnboardingEvent(ctx, replay); queuedErr == nil {
				return queued, nil
			}
		}
		return runtimeOutboxEvent{}, transitionErr
	}
	return runtimeOutboxEvent{}, errRuntimeOnboardingIntake
}

func runtimeOnboardingProtoTypes(source, authType string) (executionv1.OnboardingSource, executionv1.OnboardingAuthType, bool) {
	sources := map[string]executionv1.OnboardingSource{
		"session_key":       executionv1.OnboardingSource_ONBOARDING_SOURCE_SESSION_KEY,
		"oauth_code":        executionv1.OnboardingSource_ONBOARDING_SOURCE_OAUTH_CODE,
		"setup_token":       executionv1.OnboardingSource_ONBOARDING_SOURCE_SETUP_TOKEN,
		"api_key":           executionv1.OnboardingSource_ONBOARDING_SOURCE_API_KEY,
		"cookie":            executionv1.OnboardingSource_ONBOARDING_SOURCE_COOKIE,
		"credential_import": executionv1.OnboardingSource_ONBOARDING_SOURCE_CREDENTIAL_IMPORT,
	}
	authTypes := map[string]executionv1.OnboardingAuthType{
		"oauth":       executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH,
		"setup_token": executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_SETUP_TOKEN,
		"api_key":     executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_API_KEY,
	}
	mappedSource, sourceOK := sources[source]
	mappedAuth, authOK := authTypes[authType]
	return mappedSource, mappedAuth, sourceOK && authOK
}

func runtimeOnboardingAuthTypeFromProto(authType executionv1.OnboardingAuthType) (string, bool) {
	values := map[executionv1.OnboardingAuthType]string{
		executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_OAUTH:       "oauth",
		executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_SETUP_TOKEN: "setup_token",
		executionv1.OnboardingAuthType_ONBOARDING_AUTH_TYPE_API_KEY:     "api_key",
	}
	value, ok := values[authType]
	return value, ok
}

func validRuntimeOnboardingResult(result runtimeOnboardingResult) bool {
	if result.Status == "" {
		// Internal fakes and callers predating the terminal-status protocol carry
		// the same safe success projection and remain source compatible.
		result.Status = runtimeOnboardingResultSucceeded
	}
	if result.AccountID <= 0 || result.DesiredGeneration == 0 ||
		!runtimeOpaqueIntentIDPattern.MatchString(result.IntentID) || runtimeSecretString(result.IntentID) {
		return false
	}
	if result.Status == runtimeOnboardingResultFailed || result.Status == runtimeOnboardingResultExpired {
		if result.FinishedAt.IsZero() || !result.ProjectedAt.IsZero() || !result.ExpiresAt.IsZero() ||
			result.AuthType != "" || result.EmailAddress != "" || result.OrganizationID != "" ||
			result.UpstreamAccountID != "" || result.Scope != "" || result.SubscriptionType != "" || result.RateLimitTier != "" {
			return false
		}
		if (result.SlotID == "" && result.ExecutionEpoch != 0) || (result.SlotID != "" &&
			(!runtimeOpaqueIntentIDPattern.MatchString(result.SlotID) || runtimeSecretString(result.SlotID) || result.ExecutionEpoch == 0)) {
			return false
		}
		if result.Status == runtimeOnboardingResultFailed {
			return result.ErrorCode == runtimeOnboardingErrorFailed && result.ErrorSummary == runtimeOnboardingSummaryFailed && result.SlotID != ""
		}
		return result.ErrorCode == runtimeOnboardingErrorExpired && result.ErrorSummary == runtimeOnboardingSummaryExpired
	}
	if result.Status != runtimeOnboardingResultSucceeded || result.ExecutionEpoch == 0 || result.ProjectedAt.IsZero() ||
		result.ErrorCode != "" || result.ErrorSummary != "" ||
		!runtimeOpaqueIntentIDPattern.MatchString(result.SlotID) || runtimeSecretString(result.SlotID) {
		return false
	}
	// Keep the projection inside the narrowest CCMAX persistence boundary.
	// SQLite accepts arbitrary text, but production MySQL stores account names
	// in VARCHAR(255) and subscription_type in VARCHAR(64).
	if result.EmailAddress != "" && (len(result.EmailAddress) > 255 || strings.Count(result.EmailAddress, "@") != 1 ||
		strings.HasPrefix(result.EmailAddress, "@") || strings.HasSuffix(result.EmailAddress, "@")) {
		return false
	}
	for _, value := range []struct {
		text string
		max  int
	}{
		{result.OrganizationID, 128}, {result.UpstreamAccountID, 128}, {result.Scope, 1024},
		{result.SubscriptionType, 64}, {result.RateLimitTier, 128},
	} {
		if len(value.text) > value.max || strings.ContainsAny(value.text, "\x00\r\n") {
			return false
		}
	}
	return result.AuthType == "oauth" || result.AuthType == "setup_token" || result.AuthType == "api_key"
}

func destroyRuntimeOnboardingRequest(request *executionv1.CreateOnboardingIntentRequest) {
	if request == nil {
		return
	}
	zeroRuntimeOnboardingBytes(request.Secret)
	zeroRuntimeOnboardingBytes(request.Auxiliary)
	request.Secret = nil
	request.Auxiliary = nil
}

func zeroRuntimeOnboardingBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func (m runtimeOnboardingMaterial) String() string {
	return fmt.Sprintf("runtimeOnboardingMaterial{Source:%q AuthType:%q Secret:[REDACTED] Auxiliary:[REDACTED]}", m.Source, m.AuthType)
}

func (m runtimeOnboardingMaterial) GoString() string { return m.String() }

func (m runtimeOnboardingMaterial) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Source   string `json:"source"`
		AuthType string `json:"auth_type"`
	}{m.Source, m.AuthType})
}

var _ runtimeOnboardingIntentIntake = (*runtimeOnboardingIntakeClient)(nil)
