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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker/fixedtransport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Begin/count_tokens payloads remain bounded independently of cumulative SSE
// output. Credential bundles keep their own maxCredentialBundleBytes limit.
const maxWorkerRequestBytes = 2 << 20
const maxWorkerRPCMessageBytes = maxWorkerRequestBytes + (64 << 10)

var errInvalidProcessURL = errors.New("invalid worker endpoint URL")

type ProcessConfig struct {
	RuntimeGeneration   uint64
	IdentityDirectory   string
	RuntimeTrustFile    string
	BootstrapCASHA256   string
	ListenAddress       string
	Identity            Identity
	TicketPublicKey     ed25519.PublicKey
	UpstreamBaseURL     *url.URL
	EgressProxyURL      string
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
	generation, err := strconv.ParseUint(getenv("EXECUTION_RUNTIME_GENERATION"), 10, 64)
	if err != nil || generation == 0 || strconv.FormatUint(generation, 10) != getenv("EXECUTION_RUNTIME_GENERATION") {
		return ProcessConfig{}, errors.New("EXECUTION_RUNTIME_GENERATION must be a canonical positive integer")
	}
	publicKey, err := decodePublicKey(strings.TrimSpace(getenv("EXECUTION_TICKET_PUBLIC_KEY")))
	if err != nil {
		return ProcessConfig{}, err
	}
	baseURL, err := parseProcessURL(getenv("EXECUTION_UPSTREAM_BASE_URL"))
	if err != nil {
		return ProcessConfig{}, errors.New("EXECUTION_UPSTREAM_BASE_URL must be a URL origin")
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
		if configured := getenv(value); configured != "" {
			*target = configured
		}
	}
	config := ProcessConfig{
		RuntimeGeneration: generation,
		IdentityDirectory: getenv("EXECUTION_IDENTITY_DIRECTORY"),
		RuntimeTrustFile:  getenv("EXECUTION_RUNTIME_TRUST_FILE"),
		BootstrapCASHA256: getenv("EXECUTION_BOOTSTRAP_CA_SHA256"),
		ListenAddress:     strings.TrimSpace(getenv("EXECUTION_LISTEN_ADDRESS")),
		Identity: Identity{
			AccountID: strings.TrimSpace(getenv("EXECUTION_ACCOUNT_HASH")),
			SlotID:    strings.TrimSpace(getenv("EXECUTION_SLOT_ID")),
			NodeID:    strings.TrimSpace(getenv("EXECUTION_NODE_ID")),
			Epoch:     epoch,
		},
		TicketPublicKey:     publicKey,
		UpstreamBaseURL:     baseURL,
		EgressProxyURL:      getenv("EXECUTION_EGRESS_PROXY_URL"),
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
	if err := fixedtransport.ValidateProxyURL(c.EgressProxyURL); err != nil {
		return err
	}
	if !validProcessURL(c.UpstreamBaseURL, true, c.AllowFakeActivation) {
		return errors.New("worker upstream base URL must be a secure URL origin (HTTP requires explicit fake activation)")
	}
	if c.ImageDigest == "" {
		return errors.New("worker image digest is required")
	}
	if !c.AllowFakeActivation {
		for _, raw := range []string{c.Onboarding.OrganizationsURL, c.Onboarding.SessionAuthorizeBaseURL, c.Onboarding.TokenURL, c.Onboarding.ProfileURL, c.Onboarding.APIKeyValidationURL} {
			u, err := parseProcessURL(raw)
			if err != nil || !validProcessURL(u, false, false) {
				return errors.New("worker onboarding endpoints must be secure HTTPS URLs")
			}
		}
		if _, err := NewOnboarder(c.Onboarding); err != nil {
			return fmt.Errorf("worker onboarding configuration: %w", err)
		}
	}
	if c.runtimeBinding().Validate() != nil {
		return runtimeidentity.ErrIdentity
	}
	for _, path := range []string{c.IdentityDirectory, c.RuntimeTrustFile} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return runtimeidentity.ErrIdentity
		}
	}
	if c.BootstrapCASHA256 != "" && c.BootstrapConfig().Validate() != nil {
		return runtimebootstrap.ErrBootstrap
	}
	return nil
}

func (c ProcessConfig) runtimeBinding() runtimeidentity.Binding {
	return runtimeidentity.Binding{AccountHash: c.Identity.AccountID, SlotID: c.Identity.SlotID,
		NodeID: c.Identity.NodeID, Epoch: c.Identity.Epoch, Generation: c.RuntimeGeneration}
}

func (c ProcessConfig) BootstrapConfig() runtimebootstrap.Config {
	return runtimebootstrap.Config{IdentityDirectory: c.IdentityDirectory, TrustFile: c.RuntimeTrustFile,
		TrustSHA256: c.BootstrapCASHA256, Binding: c.runtimeBinding(), Timeout: runtimebootstrap.DefaultTimeout}
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

type processLifecycle interface {
	Activator
	ModeHealthSource
	Ready() bool
	Drain()
}

type activeCredentialSource interface {
	ActiveCredential() (ActiveCredential, error)
}

func applyActiveCredential(headers http.Header, active ActiveCredential) error {
	if headers == nil || !validCredentialVersionID(active.VersionID) || len(active.CredentialJSON) == 0 || len(active.CredentialJSON) > maxCredentialBundleBytes {
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

func RunProcess(ctx context.Context, config ProcessConfig, logger *slog.Logger) error {
	if ctx == nil {
		return errors.New("worker context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if config.BootstrapCASHA256 != "" {
		if err := runtimebootstrap.Wait(ctx, config.BootstrapConfig()); err != nil {
			return runtimebootstrap.ErrBootstrap
		}
	}
	trust, err := runtimeidentity.ReadTrustFile(config.RuntimeTrustFile)
	if err != nil {
		return runtimeidentity.ErrIdentity
	}
	tlsConfig, err := runtimeidentity.LoadServerTLS(config.IdentityDirectory, config.runtimeBinding(), trust)
	if err != nil {
		return runtimeidentity.ErrIdentity
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
	transport, err := fixedtransport.New(config.EgressProxyURL)
	if err != nil {
		return err
	}
	defer transport.CloseIdleConnections()
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
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return fmt.Errorf("listen for worker runtime: %w", err)
	}
	defer listener.Close()
	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		grpc.MaxRecvMsgSize(maxWorkerRPCMessageBytes),
		grpc.MaxSendMsgSize(maxWorkerRPCMessageBytes),
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
