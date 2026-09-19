package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent/bootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent/lifecycle"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/nodepolicy"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

var ErrRuntime = errors.New("host-agent lifecycle runtime failed")

type runner interface{ Run(context.Context) error }
type commandDrain interface {
	Seal()
	Wait(context.Context) error
}

type components struct {
	control   runner
	commands  commandDrain
	custody   *lifecycle.Custody
	expiresAt time.Time
	close     func()
}

// Validate both configuration layers before loading identity or accessing any
// external service. Generic health configuration alone permits public binds.
func validateRuntime(health config.Config, cfg Config) error {
	if health.Validate() != nil || health.Role != config.RoleHostAgent || !cfg.Enabled || cfg.Validate() != nil {
		return ErrRuntime
	}
	address, err := netip.ParseAddrPort(health.ListenAddress)
	if err != nil || !address.Addr().IsLoopback() || address.Addr().Zone() != "" || address.Port() == 0 {
		return ErrRuntime
	}
	binding := runtimeidentity.Binding{AccountHash: "00000000000000000000000000000000", SlotID: "config-check", NodeID: health.NodeID, Epoch: 1, Generation: 1}
	// Capacity is a configured ceiling, not a claim about currently free host
	// resources. No production scheduler capability is advertised below.
	if binding.Validate() != nil || health.Limits.MaxSlots > 1024 || health.Timings.NodeHeartbeat > time.Minute {
		return ErrRuntime
	}
	return nil
}

func prepare(ctx context.Context, health config.Config, cfg Config) (*components, error) {
	if ctx == nil || ctx.Err() != nil || validateRuntime(health, cfg) != nil {
		return nil, ErrRuntime
	}
	identity, err := LoadIdentity(health.NodeID, cfg)
	if err != nil {
		return nil, ErrRuntime
	}
	startup, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
	defer cancel()
	startup, expire := context.WithDeadline(startup, identity.ExpiresAt)
	defer expire()
	engine, err := docker.NewHTTPEngine(docker.HTTPConfig{SocketPath: cfg.DockerSocket})
	if err != nil {
		return nil, ErrRuntime
	}
	owned := true
	defer func() {
		if owned {
			_ = engine.Close()
		}
	}()
	// Read-only API negotiation; no image pulls, container inspection or writes.
	if engine.Ping(startup) != nil || startup.Err() != nil {
		return nil, ErrRuntime
	}
	dialer := &net.Dialer{Timeout: cfg.StartupTimeout, KeepAlive: 30 * time.Second}
	connection, err := grpc.DialContext(startup, "passthrough:///"+cfg.ControlAddress,
		grpc.WithTransportCredentials(credentials.NewTLS(identity.ControlTLS)), grpc.WithNoProxy(), grpc.WithBlock(),
		// HTTP/2 keepalive is what makes "the control session is open" mean
		// anything. TCP keepalive is suppressed while unacknowledged data is in
		// flight, and heartbeats guarantee that it is, so a blackholed peer
		// would otherwise go unnoticed until TCP retransmission gives up, which
		// on Linux defaults to roughly fifteen minutes. A node must not keep
		// holding runtimes on an authorization it can no longer confirm.
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time: 10 * time.Second, Timeout: 20 * time.Second, PermitWithoutStream: false,
		}),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			// Neither DNS/service config nor process-wide proxy variables choose
			// the dial destination. TLS still verifies the configured server name.
			return dialer.DialContext(ctx, "tcp", cfg.ControlAddress)
		}))
	if err != nil {
		return nil, ErrRuntime
	}
	defer func() {
		if owned {
			_ = connection.Close()
		}
	}()
	client := executionv1.NewNodeControlServiceClient(connection)
	enrollment, err := bootstrap.NewRPCClient(client, identity.TrustPEM)
	if err != nil {
		return nil, ErrRuntime
	}
	pin := sha256.Sum256(identity.TrustPEM)
	policy := docker.DefaultConfig()
	policy.WorkerBootstrap = &docker.WorkerBootstrap{
		NodeID: health.NodeID, TicketPublicKey: cfg.TicketPublicKey, UpstreamBaseURL: cfg.UpstreamBaseURL,
		RuntimePort: cfg.RuntimePort, IdentityDirectory: docker.WorkerIdentityDirectory,
		RuntimeTrustFile: docker.WorkerRuntimeTrustFile, TrustSHA256: hex.EncodeToString(pin[:]),
		AllowFakeActivation: false,
	}
	provider, err := docker.New(policy, engine)
	if err != nil {
		return nil, ErrRuntime
	}
	// The executor needs the custodian, custody needs the authority, and the
	// authority needs the executor's revocation watermark and the client's
	// session state. The cycle is broken by binding the last two late: the
	// closures below are only ever called after prepare has returned, and
	// prepare assigns both fields before it does.
	facts := &nodeFacts{}
	authority, err := hostagent.NewSessionAuthority(hostagent.SessionAuthorityConfig{
		Revoked:      facts.epochRevoked,
		SessionState: facts.controlSessionState,
		// The control plane's own definition of an offline node is the right
		// window for a node to stop acting on what that plane last said.
		OfflineAfter: health.Timings.NodeOffline,
	})
	if err != nil {
		return nil, ErrRuntime
	}
	custody, err := lifecycle.NewCustody(lifecycle.CustodyConfig{
		Validator: authority, NodeID: health.NodeID,
		RevalidateInterval: health.Timings.NodeHeartbeat,
	})
	if err != nil {
		return nil, ErrRuntime
	}
	executor, err := lifecycle.New(lifecycle.Config{
		Commands: hostagent.SlotCommandExecutorConfig{Provider: provider, Resources: cfg.Resources,
			Security: cfg.Security, Network: cfg.Network, DrainTimeout: cfg.ReadyTimeout,
			MaxSlots: uint32(health.Limits.MaxSlots)},
		NodeID: health.NodeID, TrustPEM: identity.TrustPEM, NodeCertificate: identity.NodeCertificate,
		Enrollment: enrollment, ReadyTimeout: cfg.ReadyTimeout, Custody: custody,
	})
	if err != nil {
		return nil, ErrRuntime
	}
	facts.executor = executor
	gate, err := NewLifecycleExecutor(executor)
	if err != nil {
		return nil, ErrRuntime
	}
	control, err := hostagent.NewControlClient(hostagent.ControlClientConfig{
		Client: client, Executor: gate, NodeID: health.NodeID,
		Labels: map[string]string{nodepolicy.ModeLabel: nodepolicy.LifecycleOnlyMode}, Capabilities: []string{nodepolicy.LifecycleOnlyCapability},
		// No docker/image capability, activation executor, probe tickets or
		// data-plane endpoint: this node must not enter business placement.
		Capacity: &executionv1.Capacity{
			MaxSlots: uint32(health.Limits.MaxSlots), MaxActiveCli: uint32(health.Limits.MaxActiveCLI),
			MaxActiveApi: uint32(health.Limits.MaxActiveAPI), MaxActiveTotal: uint32(health.Limits.MaxActiveTotal),
			AllocatableCpuMillis:   uint64(cfg.Resources.CPUMilli) * uint64(health.Limits.MaxSlots),
			AllocatableMemoryBytes: uint64(cfg.Resources.MemoryBytes) * uint64(health.Limits.MaxSlots),
		},
		HeartbeatInterval: health.Timings.NodeHeartbeat, ReconnectMin: time.Second, ReconnectMax: 15 * time.Second,
		MaxConcurrentCommands: 1, CommandQueue: 8,
	})
	if err != nil || startup.Err() != nil {
		return nil, ErrRuntime
	}
	facts.control = control
	owned = false
	return &components{control: control, commands: gate, custody: custody, expiresAt: identity.ExpiresAt,
		close: func() { _ = connection.Close(); _ = engine.Close() }}, nil
}

// nodeFacts carries the two control-plane observations the session authority
// needs. Both are assigned during prepare, before anything can read them: the
// authority is only consulted once custody is running, and custody only runs
// after prepare returns.
type nodeFacts struct {
	executor *hostagent.SlotCommandExecutor
	control  *hostagent.ControlClient
}

func (f *nodeFacts) epochRevoked(slotID string, epoch uint64) bool {
	if f == nil || f.executor == nil {
		// Not yet composed. Reporting "revoked" is the fail-closed answer.
		return true
	}
	return f.executor.EpochRevoked(slotID, epoch)
}

func (f *nodeFacts) controlSessionState() (bool, time.Time) {
	if f == nil || f.control == nil {
		// No client means no session, and no session this node ever held.
		return false, time.Time{}
	}
	return f.control.ControlSessionState()
}
