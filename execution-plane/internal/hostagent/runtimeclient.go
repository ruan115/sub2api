package hostagent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type RuntimeProvider interface {
	provider.ExecutionProvider
	RuntimeEndpoint(ctx context.Context, providerRef string) (string, error)
}

type ControllerConfig struct {
	Provider        RuntimeProvider
	TicketSource    TicketSource
	NodeID          string
	ReadyTimeout    time.Duration
	RuntimeTrustPEM []byte
	NodeCertificate tls.Certificate
}

type TicketRequest struct {
	AccountID string
	SlotID    string
	NodeID    string
	Epoch     uint64
	Scope     string
}

// TicketSource is implemented by the authenticated orchestrator/control
// channel. A host-agent must not own the execution-ticket signing key.
type TicketSource interface {
	Issue(ctx context.Context, request TicketRequest) (string, error)
}

type ActivationLease struct {
	CredentialLeaseID         string
	EncryptedCredentialBundle []byte
	ProxyLeaseID              string
}

func (l ActivationLease) Validate() error {
	if l.CredentialLeaseID == "" || l.ProxyLeaseID == "" || len(l.EncryptedCredentialBundle) == 0 {
		return errors.New("activation credential lease, encrypted bundle and proxy lease are required")
	}
	return nil
}

type Controller struct {
	provider        RuntimeProvider
	ticketSource    TicketSource
	nodeID          string
	readyTimeout    time.Duration
	runtimeTrustPEM []byte
	nodeCertificate tls.Certificate
}

func NewController(config ControllerConfig) (*Controller, error) {
	if config.Provider == nil || config.TicketSource == nil {
		return nil, errors.New("runtime provider and control-plane ticket source are required")
	}
	if config.NodeID == "" {
		return nil, errors.New("node id is required")
	}
	// Validate the trust anchor and exact node certificate before there can be
	// any provider side effects. The placeholder identifies no real instance;
	// Start builds and checks the authoritative binding for every connection.
	validatedTLS, err := runtimeidentity.ClientTLS(runtimeidentity.Binding{
		AccountHash: "00000000000000000000000000000000", SlotID: "validation", NodeID: config.NodeID, Epoch: 1, Generation: 1,
	}, config.RuntimeTrustPEM, config.NodeCertificate)
	if err != nil {
		return nil, errors.New("runtime mTLS configuration is invalid")
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = 45 * time.Second
	}
	return &Controller{
		provider: config.Provider, ticketSource: config.TicketSource, nodeID: config.NodeID,
		readyTimeout:    config.ReadyTimeout,
		runtimeTrustPEM: append([]byte(nil), config.RuntimeTrustPEM...), nodeCertificate: validatedTLS.Certificates[0],
	}, nil
}

type Runtime struct {
	Instance     provider.Instance
	client       executionv1.WorkerRuntimeServiceClient
	connection   *grpc.ClientConn
	ticketSource TicketSource
	identity     runtimeIdentity
}

type runtimeIdentity struct {
	AccountID string
	SlotID    string
	NodeID    string
	Epoch     uint64
}

func (c *Controller) Provision(ctx context.Context, spec provider.SlotSpec, activation ActivationLease) (*Runtime, error) {
	if err := activation.Validate(); err != nil {
		return nil, err
	}
	runtime, err := c.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	readyContext, cancel := context.WithTimeout(ctx, c.readyTimeout)
	defer cancel()
	if err := runtime.Activate(readyContext, activation); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	if _, err := runtime.Health(readyContext); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	return runtime, nil
}

// ProvisionSecure is the production two-stage activation path. The worker
// returns only orchestrator-sealed credential material; the supplied sink must
// synchronously persist it before the activation acknowledgement is sent.
func (c *Controller) ProvisionSecure(ctx context.Context, spec provider.SlotSpec, activation ActivationLease, sink worker.SealedCredentialSink) (*Runtime, error) {
	if err := activation.Validate(); err != nil {
		return nil, err
	}
	if sink == nil {
		return nil, errors.New("secure credential commit sink is required")
	}
	runtime, err := c.Start(ctx, spec)
	if err != nil {
		return nil, err
	}
	readyContext, cancel := context.WithTimeout(ctx, c.readyTimeout)
	defer cancel()
	if _, err := runtime.ActivateSecure(readyContext, activation, sink); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	if _, err := runtime.Health(readyContext); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	return runtime, nil
}

// Start creates and connects to a healthy but not yet credential-activated
// worker. The caller can retrieve its process-local transport public key,
// request a sealed credential bundle from the orchestrator, then call Activate.
func (c *Controller) Start(ctx context.Context, spec provider.SlotSpec) (*Runtime, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	tlsConfig, err := runtimeidentity.ClientTLS(runtimeidentity.Binding{
		AccountHash: provider.RuntimeAccountID(spec.AccountID), SlotID: spec.SlotID,
		NodeID: c.nodeID, Epoch: spec.Epoch, Generation: spec.RuntimeGeneration,
	}, c.runtimeTrustPEM, c.nodeCertificate)
	if err != nil {
		return nil, errors.New("runtime mTLS binding is invalid")
	}
	instance, err := c.provider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	if instance.SlotID != spec.SlotID || instance.Epoch != spec.Epoch || instance.RuntimeGeneration != spec.RuntimeGeneration {
		return nil, errors.New("worker provider returned a mismatched runtime binding")
	}
	if err := c.provider.Start(ctx, instance.ProviderRef); err != nil {
		return nil, fmt.Errorf("start worker slot: %w", err)
	}
	readyContext, cancel := context.WithTimeout(ctx, c.readyTimeout)
	defer cancel()
	if err := c.waitReady(readyContext, instance.ProviderRef, spec); err != nil {
		return nil, err
	}
	endpoint, err := c.provider.RuntimeEndpoint(readyContext, instance.ProviderRef)
	if err != nil {
		return nil, fmt.Errorf("resolve worker endpoint: %w", err)
	}
	host, _, err := net.SplitHostPort(endpoint)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return nil, fmt.Errorf("worker endpoint is not node-private: %q", endpoint)
	}
	connection, err := grpc.NewClient(endpoint, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithNoProxy())
	if err != nil {
		return nil, fmt.Errorf("dial worker runtime: %w", err)
	}
	// NewClient is lazy. A TLS-authenticated HTTP/2 transport must actually be
	// established before returning success. This consumes no business ticket
	// and does not broaden a command's one-shot, scope-specific authorization.
	connection.Connect()
	for state := connection.GetState(); state != connectivity.Ready; state = connection.GetState() {
		if state == connectivity.Shutdown || !connection.WaitForStateChange(readyContext, state) {
			_ = connection.Close()
			return nil, errors.New("worker authenticated transport did not become ready")
		}
	}
	runtime := &Runtime{
		Instance: instance, client: executionv1.NewWorkerRuntimeServiceClient(connection), connection: connection,
		ticketSource: c.ticketSource,
		identity: runtimeIdentity{
			AccountID: provider.RuntimeAccountID(spec.AccountID), SlotID: spec.SlotID,
			NodeID: c.nodeID, Epoch: spec.Epoch,
		},
	}
	return runtime, nil
}

func (c *Controller) waitReady(ctx context.Context, providerRef string, spec provider.SlotSpec) error {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastReason string
	for {
		status, err := c.provider.Inspect(ctx, providerRef)
		if err == nil {
			lastReason = status.Reason
			if status.SlotID != spec.SlotID || status.Epoch != spec.Epoch || status.RuntimeGeneration != spec.RuntimeGeneration || status.ImageDigest != spec.ImageDigest {
				return errors.New("worker readiness returned a mismatched runtime binding")
			}
			if status.Healthy {
				return nil
			}
		} else if !errors.Is(err, provider.ErrNotFound) {
			lastReason = err.Error()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for worker readiness (%s): %w", lastReason, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (r *Runtime) Close() error {
	return r.connection.Close()
}

func (r *Runtime) Activate(ctx context.Context, activation ActivationLease) error {
	if err := activation.Validate(); err != nil {
		return err
	}
	rawTicket, err := r.issue(ctx, "activate")
	if err != nil {
		return err
	}
	bundle := append([]byte(nil), activation.EncryptedCredentialBundle...)
	defer zero(bundle)
	response, err := r.client.Activate(ctx, &executionv1.ActivateRequest{
		ExecutionTicket: rawTicket, CredentialLeaseId: activation.CredentialLeaseID,
		EncryptedCredentialBundle: bundle, ProxyLeaseId: activation.ProxyLeaseID,
	})
	if err != nil {
		return fmt.Errorf("activate worker: %w", err)
	}
	if response.GetSlotId() != r.identity.SlotID || response.GetExecutionEpoch() != r.identity.Epoch {
		return errors.New("worker activation returned mismatched slot identity")
	}
	return nil
}

func (r *Runtime) ActivateSecure(ctx context.Context, activation ActivationLease, sink worker.SealedCredentialSink) ([]executionv1.ExecutionMode, error) {
	if err := activation.Validate(); err != nil {
		return nil, err
	}
	if sink == nil {
		return nil, errors.New("secure credential commit sink is required")
	}
	activationContext, cancel := context.WithCancel(ctx)
	defer cancel()
	rawTicket, err := r.issue(activationContext, "secure_activate")
	if err != nil {
		return nil, err
	}
	stream, err := r.client.SecureActivate(activationContext)
	if err != nil {
		return nil, secureActivationClientError(activationContext, "open secure worker activation failed", err)
	}
	bundle := append([]byte(nil), activation.EncryptedCredentialBundle...)
	defer zero(bundle)
	if err := stream.Send(&executionv1.SecureActivateRequest{Event: &executionv1.SecureActivateRequest_Begin{Begin: &executionv1.SecureActivateBegin{
		ExecutionTicket: rawTicket, CredentialLeaseId: activation.CredentialLeaseID,
		EncryptedCredentialBundle: bundle, ProxyLeaseId: activation.ProxyLeaseID,
	}}}); err != nil {
		return nil, secureActivationClientError(activationContext, "send secure worker activation failed", err)
	}
	response, err := stream.Recv()
	if err != nil {
		return nil, secureActivationClientError(activationContext, "receive worker credential commit failed", err)
	}
	commit := response.GetCredentialCommit()
	if commit == nil || commit.GetAccountBinding() != r.identity.AccountID || commit.GetSlotId() != r.identity.SlotID ||
		commit.GetExecutionEpoch() != r.identity.Epoch || commit.GetCredentialLeaseId() != activation.CredentialLeaseID ||
		commit.GetProxyLeaseId() != activation.ProxyLeaseID || len(commit.GetSealedCredentialBundle()) == 0 ||
		len(commit.GetSealedCredentialBundle()) > 2<<20 {
		return nil, errors.New("worker credential commit identity is invalid")
	}
	sealed := append([]byte(nil), commit.GetSealedCredentialBundle()...)
	defer zero(sealed)
	defer zero(commit.SealedCredentialBundle)
	versionID, err := sink.CommitSealedCredential(activationContext, worker.SealedCredentialCommitRequest{
		AccountBinding: commit.GetAccountBinding(), SlotID: commit.GetSlotId(), ExecutionEpoch: commit.GetExecutionEpoch(),
		CredentialLeaseID: commit.GetCredentialLeaseId(), ProxyLeaseID: commit.GetProxyLeaseId(),
		SealedCredentialBundle: sealed,
	})
	if err != nil || !validRuntimeVersionID(versionID) {
		return nil, secureActivationClientError(activationContext, "orchestrator credential commit failed", err)
	}
	if err := stream.Send(&executionv1.SecureActivateRequest{Event: &executionv1.SecureActivateRequest_CredentialCommitAck{CredentialCommitAck: &executionv1.CredentialCommitAck{
		VersionId: versionID,
	}}}); err != nil {
		return nil, secureActivationClientError(activationContext, "acknowledge worker credential commit failed", err)
	}
	if err := stream.CloseSend(); err != nil {
		return nil, secureActivationClientError(activationContext, "close secure worker activation request failed", err)
	}
	response, err = stream.Recv()
	if err != nil {
		return nil, secureActivationClientError(activationContext, "receive secure worker activation result failed", err)
	}
	completed := response.GetCompleted()
	if completed == nil || completed.GetSlotId() != r.identity.SlotID || completed.GetExecutionEpoch() != r.identity.Epoch || len(completed.GetHealthyModes()) == 0 {
		return nil, errors.New("secure worker activation result identity is invalid")
	}
	return append([]executionv1.ExecutionMode(nil), completed.GetHealthyModes()...), nil
}

func validRuntimeVersionID(value string) bool {
	return credential.ValidateTransportID(value) == nil
}

func secureActivationClientError(ctx context.Context, message string, err error) error {
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded {
		return context.DeadlineExceeded
	}
	return errors.New(message)
}

type CredentialTransportKey struct {
	KeyID     string
	PublicKey []byte
}

func (k CredentialTransportKey) String() string {
	return fmt.Sprintf("CredentialTransportKey{KeyID:%q PublicKey:[PUBLIC]}", k.KeyID)
}

func (r *Runtime) CredentialTransportKey(ctx context.Context) (CredentialTransportKey, error) {
	rawTicket, err := r.issue(ctx, "credential_key")
	if err != nil {
		return CredentialTransportKey{}, err
	}
	response, err := r.client.CredentialTransportKey(ctx, &executionv1.CredentialTransportKeyRequest{ExecutionTicket: rawTicket})
	if err != nil {
		return CredentialTransportKey{}, fmt.Errorf("get worker credential transport key: %w", err)
	}
	if response.GetSlotId() != r.identity.SlotID || response.GetExecutionEpoch() != r.identity.Epoch ||
		response.GetKeyId() == "" || len(response.GetKeyId()) > 128 || len(response.GetPublicKey()) != 32 {
		return CredentialTransportKey{}, errors.New("worker credential transport key identity is invalid")
	}
	return CredentialTransportKey{KeyID: response.GetKeyId(), PublicKey: append([]byte(nil), response.GetPublicKey()...)}, nil
}

func (r *Runtime) Health(ctx context.Context) (*executionv1.HealthResponse, error) {
	rawTicket, err := r.issue(ctx, "health")
	if err != nil {
		return nil, err
	}
	return r.client.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: rawTicket})
}

// Deprecated: local fixture helper. Data-plane callers must use
// CountTokensRequest with an explicit authoritative account and selected mode.
func (r *Runtime) CountTokens(ctx context.Context, requestID string, body []byte, routeGeneration uint64) (*executionv1.CountTokensResponse, error) {
	if routeGeneration == 0 {
		return nil, status.Error(codes.InvalidArgument, "route generation is required")
	}
	rawTicket, err := r.issue(ctx, "count_tokens")
	if err != nil {
		return nil, err
	}
	response, err := r.client.CountTokens(ctx, &executionv1.WorkerRuntimeServiceCountTokensRequest{
		ExecutionTicket: rawTicket,
		Request: &executionv1.CountTokensRequest{
			RequestId: requestID, AccountId: r.identity.AccountID,
			Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, AnthropicRequestJson: append([]byte(nil), body...),
			SlotId: r.identity.SlotID, ExecutionEpoch: r.identity.Epoch, RouteGeneration: routeGeneration,
		},
	})
	if err != nil {
		return nil, err
	}
	return response.GetResponse(), nil
}

// Deprecated: local fixture helper that collects the entire response. New
// data-plane forwarding must use OpenExecution and preserve streaming/backpressure.
func (r *Runtime) Execute(ctx context.Context, requestID string, body []byte, routeGeneration uint64) ([]*executionv1.ExecuteResponse, error) {
	if routeGeneration == 0 {
		return nil, status.Error(codes.InvalidArgument, "route generation is required")
	}
	rawTicket, err := r.issue(ctx, "messages")
	if err != nil {
		return nil, err
	}
	stream, err := r.client.Execute(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&executionv1.WorkerRuntimeServiceExecuteRequest{
		Event: &executionv1.WorkerRuntimeServiceExecuteRequest_Begin{Begin: &executionv1.WorkerBeginExecution{
			ExecutionTicket: rawTicket,
			Request: &executionv1.BeginExecution{
				RequestId: requestID, AccountId: r.identity.AccountID,
				Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, AnthropicRequestJson: append([]byte(nil), body...),
				SlotId: r.identity.SlotID, ExecutionEpoch: r.identity.Epoch, RouteGeneration: routeGeneration,
			},
		}},
	}); err != nil {
		return nil, err
	}
	if err := stream.CloseSend(); err != nil {
		return nil, err
	}
	var responses []*executionv1.ExecuteResponse
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return responses, nil
		}
		if err != nil {
			return nil, err
		}
		if response.GetResponse() == nil {
			return nil, errors.New("worker returned an empty execution event")
		}
		responses = append(responses, response.GetResponse())
	}
}

func (r *Runtime) issue(ctx context.Context, scope string) (string, error) {
	if ctx != nil {
		if source, ok := ctx.Value(probeCommandContextKey{}).(*probeCommandSource); ok {
			if source == nil || source.identity != r.identity {
				return "", ErrProbeTicketUnavailable
			}
			return source.request(ctx, scope)
		}
	}
	return r.ticketSource.Issue(ctx, TicketRequest{
		AccountID: r.identity.AccountID, SlotID: r.identity.SlotID,
		NodeID: r.identity.NodeID, Epoch: r.identity.Epoch, Scope: scope,
	})
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
