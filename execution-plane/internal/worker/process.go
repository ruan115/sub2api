package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxUpstreamResponseBytes = 2 << 20

type ProcessConfig struct {
	ListenAddress       string
	Identity            Identity
	TicketPublicKey     ed25519.PublicKey
	UpstreamBaseURL     *url.URL
	ImageDigest         string
	AllowFakeActivation bool
	Onboarding          OnboardingConfig
}

func LoadProcessConfig(getenv func(string) string) (ProcessConfig, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	epoch, err := strconv.ParseUint(strings.TrimSpace(getenv("EXECUTION_EPOCH")), 10, 64)
	if err != nil || epoch == 0 {
		return ProcessConfig{}, errors.New("EXECUTION_EPOCH must be a positive integer")
	}
	publicKey, err := decodePublicKey(strings.TrimSpace(getenv("EXECUTION_TICKET_PUBLIC_KEY")))
	if err != nil {
		return ProcessConfig{}, err
	}
	baseURL, err := url.Parse(strings.TrimSpace(getenv("EXECUTION_UPSTREAM_BASE_URL")))
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" || baseURL.User != nil {
		return ProcessConfig{}, errors.New("EXECUTION_UPSTREAM_BASE_URL must be a URL origin")
	}
	if (baseURL.Path != "" && baseURL.Path != "/") || baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return ProcessConfig{}, errors.New("EXECUTION_UPSTREAM_BASE_URL must not contain path, query or fragment")
	}
	allowFake, err := strconv.ParseBool(strings.TrimSpace(getenv("EXECUTION_ALLOW_FAKE_ACTIVATION")))
	if err != nil {
		return ProcessConfig{}, errors.New("EXECUTION_ALLOW_FAKE_ACTIVATION must be true or false")
	}
	onboarding := DefaultOnboardingConfig()
	for value, target := range map[string]*string{
		"EXECUTION_ONBOARDING_ORGANIZATIONS_URL":          &onboarding.OrganizationsURL,
		"EXECUTION_ONBOARDING_SESSION_AUTHORIZE_BASE_URL": &onboarding.SessionAuthorizeBaseURL,
		"EXECUTION_ONBOARDING_TOKEN_URL":                  &onboarding.TokenURL,
		"EXECUTION_ONBOARDING_PROFILE_URL":                &onboarding.ProfileURL,
		"EXECUTION_ONBOARDING_API_KEY_VALIDATION_URL":     &onboarding.APIKeyValidationURL,
	} {
		if configured := strings.TrimSpace(getenv(value)); configured != "" {
			*target = configured
		}
	}
	config := ProcessConfig{
		ListenAddress: strings.TrimSpace(getenv("EXECUTION_LISTEN_ADDRESS")),
		Identity: Identity{
			AccountID: strings.TrimSpace(getenv("EXECUTION_ACCOUNT_HASH")),
			SlotID:    strings.TrimSpace(getenv("EXECUTION_SLOT_ID")),
			NodeID:    strings.TrimSpace(getenv("EXECUTION_NODE_ID")),
			Epoch:     epoch,
		},
		TicketPublicKey:     publicKey,
		UpstreamBaseURL:     baseURL,
		ImageDigest:         strings.TrimSpace(getenv("EXECUTION_IMAGE_DIGEST")),
		AllowFakeActivation: allowFake,
		Onboarding:          onboarding,
	}
	if err := config.Validate(); err != nil {
		return ProcessConfig{}, err
	}
	return config, nil
}

func (c ProcessConfig) Validate() error {
	if _, _, err := net.SplitHostPort(c.ListenAddress); err != nil {
		return fmt.Errorf("worker listen address: %w", err)
	}
	if err := c.Identity.Validate(); err != nil {
		return err
	}
	accountHash, err := hex.DecodeString(c.Identity.AccountID)
	if err != nil || len(accountHash) != 16 {
		return errors.New("worker account identity must be a 128-bit hex digest")
	}
	if len(c.TicketPublicKey) != ed25519.PublicKeySize {
		return errors.New("worker ticket public key is invalid")
	}
	if c.UpstreamBaseURL == nil || c.UpstreamBaseURL.Scheme == "" || c.UpstreamBaseURL.Host == "" {
		return errors.New("worker upstream base URL is required")
	}
	if c.ImageDigest == "" {
		return errors.New("worker image digest is required")
	}
	if !c.AllowFakeActivation {
		if _, err := NewOnboarder(c.Onboarding); err != nil {
			return fmt.Errorf("worker onboarding configuration: %w", err)
		}
	}
	return nil
}

func decodePublicKey(encoded string) (ed25519.PublicKey, error) {
	if encoded == "" {
		return nil, errors.New("EXECUTION_TICKET_PUBLIC_KEY is required")
	}
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, errors.New("EXECUTION_TICKET_PUBLIC_KEY must contain a base64 Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}

type processState struct {
	mu        sync.RWMutex
	activated bool
	draining  bool
}

func (s *processState) Activate(context.Context, Activation) ([]executionv1.ExecutionMode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return nil, status.Error(codes.Unavailable, "worker is draining")
	}
	s.activated = true
	return []executionv1.ExecutionMode{executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API}, nil
}

func (s *processState) ModeHealth(context.Context) []ModeHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	reason := ""
	if s.draining {
		reason = "draining"
	} else if !s.activated {
		reason = "not_activated"
	}
	return []ModeHealth{
		{Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE, Healthy: false, ReasonCode: "not_implemented"},
		{Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, Healthy: reason == "", ReasonCode: reason},
	}
}

func (s *processState) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activated && !s.draining
}

func (s *processState) Drain() {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
}

type upstreamExecutor struct {
	state            processLifecycle
	credentialSource activeCredentialSource
	client           *http.Client
	baseURL          *url.URL
}

type processLifecycle interface {
	Activator
	ModeHealthSource
	Ready() bool
	Drain()
}

type activeCredentialSource interface {
	ActiveCredential() (ActiveCredential, error)
}

func (e *upstreamExecutor) Execute(stream ExecutionStream) error {
	if !e.state.Ready() {
		return status.Error(codes.FailedPrecondition, "worker is not ready")
	}
	begin := stream.Begin()
	if begin.GetMode() != executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API {
		return status.Error(codes.Unimplemented, "execution mode is not available")
	}
	response, body, err := e.request(stream.Context(), "/v1/messages", begin.GetAnthropicRequestJson(), begin.GetRequestHeaders())
	if err != nil {
		return err
	}
	if err := stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Headers{Headers: &executionv1.ResponseHeaders{
		StatusCode: int32(response.StatusCode), Headers: safeResponseHeaders(response.Header),
	}}}); err != nil {
		return err
	}
	if len(body) > 0 {
		if err := stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_BodyChunk{BodyChunk: &executionv1.ResponseBodyChunk{Data: body}}}); err != nil {
			return err
		}
	}
	return stream.Send(&executionv1.ExecuteResponse{Event: &executionv1.ExecuteResponse_Completed{Completed: &executionv1.ExecutionCompleted{
		UpstreamRequestId: response.Header.Get("X-Request-Id"),
	}}})
}

func (e *upstreamExecutor) CountTokens(ctx context.Context, request *executionv1.CountTokensRequest) (*executionv1.CountTokensResponse, error) {
	if !e.state.Ready() {
		return nil, status.Error(codes.FailedPrecondition, "worker is not ready")
	}
	if request.GetMode() != executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API {
		return nil, status.Error(codes.Unimplemented, "execution mode is not available")
	}
	response, body, err := e.request(ctx, "/v1/messages/count_tokens", request.GetAnthropicRequestJson(), nil)
	if err != nil {
		return nil, err
	}
	return &executionv1.CountTokensResponse{
		StatusCode: int32(response.StatusCode), AnthropicResponseJson: body,
		UpstreamRequestId: response.Header.Get("X-Request-Id"),
	}, nil
}

func (e *upstreamExecutor) request(ctx context.Context, path string, body []byte, headers map[string]string) (*http.Response, []byte, error) {
	if len(body) == 0 || len(body) > maxUpstreamResponseBytes {
		return nil, nil, status.Error(codes.InvalidArgument, "upstream request body size is invalid")
	}
	endpoint := e.baseURL.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, status.Error(codes.Internal, "create upstream request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "sub2api-execution-worker/1")
	copySafeRequestHeaders(request.Header, headers)
	if e.credentialSource != nil {
		active, activeErr := e.credentialSource.ActiveCredential()
		if activeErr != nil {
			return nil, nil, status.Error(codes.FailedPrecondition, "worker credential is not active")
		}
		defer active.Destroy()
		if err := applyActiveCredential(request.Header, active); err != nil {
			return nil, nil, status.Error(codes.FailedPrecondition, "worker credential is invalid")
		}
	}
	response, err := e.client.Do(request)
	if err != nil {
		return nil, nil, status.Error(codes.Unavailable, "upstream request failed")
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxUpstreamResponseBytes+1))
	if err != nil {
		return nil, nil, status.Error(codes.Unavailable, "read upstream response failed")
	}
	if len(payload) > maxUpstreamResponseBytes {
		return nil, nil, status.Error(codes.ResourceExhausted, "upstream response exceeded size limit")
	}
	return response, payload, nil
}

func applyActiveCredential(headers http.Header, active ActiveCredential) error {
	if headers == nil || !validCredentialVersionID(active.VersionID) || len(active.CredentialJSON) == 0 || len(active.CredentialJSON) > maxUpstreamResponseBytes {
		return ErrActivationRejected
	}
	decoder := json.NewDecoder(bytes.NewReader(active.CredentialJSON))
	decoder.DisallowUnknownFields()
	var normalized normalizedCredential
	if err := decoder.Decode(&normalized); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ErrActivationRejected
	}
	switch active.AuthType {
	case AuthTypeOAuth, AuthTypeSetupToken:
		if strings.TrimSpace(normalized.AccessToken) == "" || normalized.APIKey != "" || strings.ContainsAny(normalized.AccessToken, "\x00\r\n") {
			return ErrActivationRejected
		}
		headers.Set("Authorization", "Bearer "+normalized.AccessToken)
		headers.Del("x-api-key")
	case AuthTypeAPIKey:
		if strings.TrimSpace(normalized.APIKey) == "" || normalized.AccessToken != "" || strings.ContainsAny(normalized.APIKey, "\x00\r\n") {
			return ErrActivationRejected
		}
		headers.Set("x-api-key", normalized.APIKey)
		headers.Del("Authorization")
	default:
		return ErrActivationRejected
	}
	if headers.Get("anthropic-version") == "" {
		headers.Set("anthropic-version", "2023-06-01")
	}
	return nil
}

func copySafeRequestHeaders(target http.Header, source map[string]string) {
	for name, value := range source {
		switch strings.ToLower(name) {
		case "accept", "anthropic-beta", "anthropic-version", "x-fake-scenario":
			target.Set(name, value)
		}
	}
}

func safeResponseHeaders(source http.Header) map[string]string {
	result := make(map[string]string)
	for _, name := range []string{"Content-Type", "X-Request-Id"} {
		if value := source.Get(name); value != "" {
			result[strings.ToLower(name)] = value
		}
	}
	return result
}

func RunProcess(ctx context.Context, config ProcessConfig, logger *slog.Logger) error {
	if err := config.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}
	verifier, err := ticket.NewVerifier(config.TicketPublicKey)
	if err != nil {
		return err
	}
	guard, err := NewGuard(verifier, config.Identity, time.Now)
	if err != nil {
		return err
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
	}
	executionClient := &http.Client{Transport: transport}
	onboardingClient := &http.Client{Transport: transport, Timeout: 60 * time.Second}
	var lifecycle processLifecycle
	var source activeCredentialSource
	var transportRecipient *credential.Recipient
	if config.AllowFakeActivation {
		lifecycle = &processState{}
	} else {
		transportRecipient, err = credential.NewRecipient(rand.Reader)
		if err != nil {
			return errors.New("create worker credential recipient")
		}
		defer transportRecipient.Destroy()
		onboardingConfig := config.Onboarding
		onboardingConfig.HTTPClient = onboardingClient
		onboarder, onboardErr := NewOnboarder(onboardingConfig)
		if onboardErr != nil {
			return onboardErr
		}
		secureActivator, activatorErr := NewSecureActivator(SecureActivatorConfig{
			Identity: config.Identity, Recipient: transportRecipient, Onboarder: onboarder,
		})
		if activatorErr != nil {
			return activatorErr
		}
		lifecycle = secureActivator
		source = secureActivator
		defer secureActivator.Drain()
	}
	executor := &upstreamExecutor{
		state: lifecycle, credentialSource: source, client: executionClient, baseURL: config.UpstreamBaseURL,
	}
	runtimeServer, err := NewRuntimeServer(RuntimeServerConfig{
		Guard: guard, Identity: config.Identity, Activator: lifecycle, Executor: executor,
		HealthSource: lifecycle, ImageDigest: config.ImageDigest,
	})
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for worker runtime: %w", err)
	}
	defer listener.Close()
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxUpstreamResponseBytes+(64<<10)),
		grpc.MaxSendMsgSize(maxUpstreamResponseBytes+(64<<10)),
	)
	runtimeServer.Register(grpcServer)
	serveResult := make(chan error, 1)
	go func() { serveResult <- grpcServer.Serve(listener) }()

	drainSignals := make(chan os.Signal, 1)
	signal.Notify(drainSignals, syscall.SIGUSR1)
	defer signal.Stop(drainSignals)
	go func() {
		select {
		case <-ctx.Done():
		case <-drainSignals:
			lifecycle.Drain()
			logger.Info("worker entered drain mode", "slot_id", config.Identity.SlotID, "epoch", config.Identity.Epoch)
		}
	}()
	logger.Info("worker runtime listening", "slot_id", config.Identity.SlotID, "epoch", config.Identity.Epoch, "address", config.ListenAddress)

	select {
	case err := <-serveResult:
		return err
	case <-ctx.Done():
		stopped := make(chan struct{})
		go func() {
			grpcServer.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			grpcServer.Stop()
		}
		return nil
	}
}

func Healthcheck(address string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	connection, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return err
	}
	return connection.Close()
}
