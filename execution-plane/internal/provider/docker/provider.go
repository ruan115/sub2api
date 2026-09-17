package docker

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

const (
	labelManaged           = "com.sub2api.execution.managed"
	labelSlotID            = "com.sub2api.execution.slot_id"
	labelAccountHash       = "com.sub2api.execution.account_hash"
	labelEpoch             = "com.sub2api.execution.epoch"
	labelRuntimeGeneration = "com.sub2api.execution.runtime_generation"
	labelImageDigest       = "com.sub2api.execution.image_digest"
)

type Config struct {
	NetworkPrefix           string
	AllowedSeccompProfiles  []string
	AllowedAppArmorProfiles []string
	StopTimeout             time.Duration
	Now                     func() time.Time
	WorkerBootstrap         *WorkerBootstrap
}

// WorkerBootstrap contains non-secret node bootstrap values. Credentials are
// deliberately absent and are delivered only after the worker channel is up.
type WorkerBootstrap struct {
	NodeID              string
	TicketPublicKey     string
	UpstreamBaseURL     string
	RuntimePort         uint16
	AllowFakeActivation bool
	// These paths name per-instance, independently enrolled material. The
	// provider never generates, embeds or mounts a shared server private key.
	IdentityDirectory string
	RuntimeTrustFile  string
}

const WorkerIdentityDirectory = "/run/execution/identity"
const WorkerRuntimeTrustFile = "/run/execution/runtime-ca.pem"

func DefaultConfig() Config {
	return Config{
		NetworkPrefix:           "execution-net-",
		AllowedSeccompProfiles:  []string{"builtin"},
		AllowedAppArmorProfiles: []string{"docker-default"},
		StopTimeout:             30 * time.Second,
		Now:                     time.Now,
	}
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.NetworkPrefix) == "" {
		return errors.New("Docker network prefix is required")
	}
	if len(c.AllowedSeccompProfiles) == 0 || len(c.AllowedAppArmorProfiles) == 0 {
		return errors.New("Docker seccomp and AppArmor allowlists are required")
	}
	if !validSandboxProfileAllowlist(c.AllowedSeccompProfiles) || !validSandboxProfileAllowlist(c.AllowedAppArmorProfiles) {
		return errors.New("Docker sandbox profile allowlists contain an empty, malformed or disabled profile")
	}
	if c.StopTimeout <= 0 {
		return errors.New("Docker stop timeout must be positive")
	}
	if c.Now == nil {
		return errors.New("clock is required")
	}
	if c.WorkerBootstrap != nil {
		if err := c.WorkerBootstrap.Validate(); err != nil {
			return fmt.Errorf("worker bootstrap: %w", err)
		}
	}
	return nil
}

func (c WorkerBootstrap) Validate() error {
	if c.IdentityDirectory != WorkerIdentityDirectory || c.RuntimeTrustFile != WorkerRuntimeTrustFile {
		return errors.New("worker TLS bootstrap requires the fixed per-instance identity and trust paths; certificate installation is not implemented by the Docker provider")
	}
	if strings.TrimSpace(c.NodeID) == "" || strings.TrimSpace(c.TicketPublicKey) == "" {
		return errors.New("node id and ticket public key are required")
	}
	publicKey, err := base64.RawStdEncoding.DecodeString(c.TicketPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("ticket public key must be a base64 Ed25519 public key")
	}
	endpoint, err := url.Parse(c.UpstreamBaseURL)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || endpoint.User != nil {
		return errors.New("upstream base URL is invalid")
	}
	if endpoint.Path != "" && endpoint.Path != "/" || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return errors.New("upstream base URL must be an origin")
	}
	if c.RuntimePort == 0 {
		return errors.New("runtime port is required")
	}
	return nil
}

type Provider struct {
	config Config
	engine Engine
}

func New(config Config, engine Engine) (*Provider, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if engine == nil {
		return nil, errors.New("Docker Engine client is required")
	}
	// Retain the validated policy, not mutable caller-owned slice storage.
	config.AllowedSeccompProfiles = append([]string(nil), config.AllowedSeccompProfiles...)
	config.AllowedAppArmorProfiles = append([]string(nil), config.AllowedAppArmorProfiles...)
	if config.WorkerBootstrap != nil {
		bootstrap := *config.WorkerBootstrap
		config.WorkerBootstrap = &bootstrap
	}
	return &Provider{config: config, engine: engine}, nil
}

func (p *Provider) Create(ctx context.Context, spec base.SlotSpec) (base.Instance, error) {
	if err := spec.Validate(); err != nil {
		return base.Instance{}, err
	}
	if !sandboxImmutableReference(spec.ImageDigest) {
		return base.Instance{}, errors.New("canonical immutable image reference is required")
	}
	if !contains(p.config.AllowedSeccompProfiles, spec.Security.SeccompProfile) {
		return base.Instance{}, fmt.Errorf("seccomp profile %q is not allowed", spec.Security.SeccompProfile)
	}
	if !contains(p.config.AllowedAppArmorProfiles, spec.Security.AppArmorProfile) {
		return base.Instance{}, fmt.Errorf("AppArmor profile %q is not allowed", spec.Security.AppArmorProfile)
	}
	if err := p.engine.Ping(ctx); err != nil {
		return base.Instance{}, fmt.Errorf("ping Docker Engine: %w", err)
	}
	name := containerName(spec.SlotID)
	existing, err := p.existingSlot(ctx, name, spec)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, base.ErrNotFound) {
		return base.Instance{}, err
	}
	networkCreated, err := p.ensureSlotNetwork(ctx, spec.SlotID)
	if err != nil {
		return base.Instance{}, err
	}
	networkName := p.networkName(spec.SlotID)
	hostAgentGateway, err := p.slotNetworkGateway(ctx, networkName, spec.SlotID)
	if err != nil {
		// A concurrent creator may have attached the exact instance after our
		// first lookup. Re-adopt only after the complete read-only gate; never
		// delete or repair a network whose ownership/shape could not be proven.
		if existing, inspectErr := p.existingSlot(ctx, name, spec); inspectErr == nil {
			return existing, nil
		}
		return base.Instance{}, err
	}

	tmpfsBytes := sandboxTmpfsBytes(spec.Resources.TmpfsBytes)
	initProcess := true
	stopTimeout := int(math.Ceil(p.config.StopTimeout.Seconds()))
	environment := []string{
		"EXECUTION_SLOT_ID=" + spec.SlotID,
		"EXECUTION_EPOCH=" + strconv.FormatUint(spec.Epoch, 10),
		"EXECUTION_RUNTIME_GENERATION=" + strconv.FormatUint(spec.RuntimeGeneration, 10),
		"EXECUTION_EGRESS_PROXY_URL=" + spec.Network.EgressProxyEndpoint,
		"HTTP_PROXY=" + spec.Network.EgressProxyEndpoint,
		"HTTPS_PROXY=" + spec.Network.EgressProxyEndpoint,
		"NO_PROXY=127.0.0.1,localhost",
	}
	var exposedPorts map[string]struct{}
	if bootstrap := p.config.WorkerBootstrap; bootstrap != nil {
		port := strconv.FormatUint(uint64(bootstrap.RuntimePort), 10)
		containerPort := port + "/tcp"
		environment = append(environment,
			"EXECUTION_ACCOUNT_HASH="+base.RuntimeAccountID(spec.AccountID),
			"EXECUTION_NODE_ID="+bootstrap.NodeID,
			"EXECUTION_LISTEN_ADDRESS=0.0.0.0:"+port,
			"EXECUTION_TICKET_PUBLIC_KEY="+bootstrap.TicketPublicKey,
			"EXECUTION_UPSTREAM_BASE_URL="+strings.TrimSuffix(bootstrap.UpstreamBaseURL, "/"),
			"EXECUTION_IMAGE_DIGEST="+spec.ImageDigest,
			"EXECUTION_ALLOW_FAKE_ACTIVATION="+strconv.FormatBool(bootstrap.AllowFakeActivation),
			"EXECUTION_IDENTITY_DIRECTORY="+bootstrap.IdentityDirectory,
			"EXECUTION_RUNTIME_TRUST_FILE="+bootstrap.RuntimeTrustFile,
		)
		exposedPorts = map[string]struct{}{containerPort: {}}
	}
	response, err := p.engine.CreateContainer(ctx, name, CreateContainerRequest{
		Image:       spec.ImageDigest,
		Hostname:    name,
		User:        strconv.FormatUint(uint64(spec.Security.RunAsUser), 10) + ":" + strconv.FormatUint(uint64(spec.Security.RunAsUser), 10),
		StopTimeout: &stopTimeout,
		Labels: map[string]string{
			labelManaged:           "true",
			labelSlotID:            spec.SlotID,
			labelAccountHash:       base.RuntimeAccountID(spec.AccountID),
			labelEpoch:             strconv.FormatUint(spec.Epoch, 10),
			labelRuntimeGeneration: strconv.FormatUint(spec.RuntimeGeneration, 10),
			labelImageDigest:       spec.ImageDigest,
		},
		Env:          environment,
		ExposedPorts: exposedPorts,
		HostConfig: HostConfig{
			Runtime:        sandboxRuntime,
			IpcMode:        "private",
			CgroupnsMode:   "private",
			NetworkMode:    networkName,
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt: []string{
				"no-new-privileges=true",
				"seccomp=" + spec.Security.SeccompProfile,
				"apparmor=" + spec.Security.AppArmorProfile,
			},
			PidsLimit: spec.Resources.PIDs,
			Memory:    spec.Resources.MemoryBytes,
			NanoCPUs:  spec.Resources.CPUMilli * 1_000_000,
			Tmpfs: map[string]string{
				"/tmp": "rw,noexec,nosuid,nodev,size=" + strconv.FormatInt(tmpfsBytes, 10),
				"/run": "rw,noexec,nosuid,nodev,size=" + strconv.FormatInt(tmpfsBytes, 10),
			},
			Init:          &initProcess,
			RestartPolicy: RestartPolicy{Name: "unless-stopped"},
			ExtraHosts:    []string{"host-agent.execution.internal:" + hostAgentGateway},
			LogConfig: LogConfig{
				Type: "json-file",
				Config: map[string]string{
					"max-size": "10m",
					"max-file": "3",
				},
			},
		},
	})
	if err != nil {
		if IsConflict(err) {
			existing, inspectErr := p.existingSlot(ctx, name, spec)
			if inspectErr == nil {
				return existing, nil
			}
			return base.Instance{}, errors.Join(fmt.Errorf("create Docker container: %w", err), inspectErr)
		}
		if networkCreated {
			_ = p.engine.RemoveNetwork(ctx, networkName)
		}
		return base.Instance{}, fmt.Errorf("create Docker container: %w", err)
	}
	if strings.TrimSpace(response.ID) == "" {
		existing, inspectErr := p.existingSlot(ctx, name, spec)
		if inspectErr == nil {
			return existing, nil
		}
		if networkCreated && errors.Is(inspectErr, base.ErrNotFound) {
			_ = p.engine.RemoveNetwork(ctx, networkName)
		}
		return base.Instance{}, errors.Join(errors.New("Docker create returned an empty container id"), inspectErr)
	}
	now := p.config.Now().UTC()
	return base.Instance{
		ProviderRef:       name,
		SlotID:            spec.SlotID,
		Epoch:             spec.Epoch,
		RuntimeGeneration: spec.RuntimeGeneration,
		State:             slot.StateStopped,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

func (p *Provider) RuntimeEndpoint(ctx context.Context, providerRef string) (string, error) {
	if p.config.WorkerBootstrap == nil {
		return "", errors.New("worker runtime bootstrap is not configured")
	}
	container, err := p.readSandbox(ctx, providerRef)
	if err != nil {
		return "", err
	}
	if !container.State.Running {
		return "", errors.New("worker runtime is not running")
	}
	network := container.NetworkSettings.Networks[container.HostConfig.NetworkMode]
	ip := net.ParseIP(network.IPAddress)
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return "", fmt.Errorf("worker runtime has no private address on slot network %q", container.HostConfig.NetworkMode)
	}
	return net.JoinHostPort(ip.String(), strconv.FormatUint(uint64(p.config.WorkerBootstrap.RuntimePort), 10)), nil
}

func (p *Provider) existingSlot(ctx context.Context, name string, spec base.SlotSpec) (base.Instance, error) {
	container, err := p.readSandbox(ctx, name)
	if err != nil {
		return base.Instance{}, err
	}
	existing, err := p.statusFromContainer(container, name)
	if err != nil {
		return base.Instance{}, err
	}
	if existing.SlotID != spec.SlotID {
		return base.Instance{}, fmt.Errorf("container name collision for slot %q", spec.SlotID)
	}
	if existing.Epoch != spec.Epoch {
		return base.Instance{}, fmt.Errorf("slot %q already exists at epoch %d", spec.SlotID, existing.Epoch)
	}
	if existing.RuntimeGeneration != spec.RuntimeGeneration {
		return base.Instance{}, fmt.Errorf("slot %q already exists at another runtime generation", spec.SlotID)
	}
	if existing.ImageDigest != spec.ImageDigest {
		return base.Instance{}, fmt.Errorf("slot %q already exists with a different image digest", spec.SlotID)
	}
	if container.Config.Labels[labelAccountHash] != base.RuntimeAccountID(spec.AccountID) {
		return base.Instance{}, errors.New("existing slot belongs to another account")
	}
	env, err := sandboxEnvironment(container)
	if err != nil || env["EXECUTION_EGRESS_PROXY_URL"] != spec.Network.EgressProxyEndpoint ||
		container.Config.User != fmt.Sprintf("%d:%d", spec.Security.RunAsUser, spec.Security.RunAsUser) ||
		container.HostConfig.Memory != spec.Resources.MemoryBytes || container.HostConfig.PidsLimit != spec.Resources.PIDs ||
		container.HostConfig.NanoCPUs != spec.Resources.CPUMilli*1_000_000 ||
		!contains(container.HostConfig.SecurityOpt, "seccomp="+spec.Security.SeccompProfile) ||
		container.AppArmorProfile != spec.Security.AppArmorProfile ||
		!sandboxTmpfsSizeMatches(container.HostConfig.Tmpfs["/tmp"], sandboxTmpfsBytes(spec.Resources.TmpfsBytes)) ||
		!sandboxTmpfsSizeMatches(container.HostConfig.Tmpfs["/run"], sandboxTmpfsBytes(spec.Resources.TmpfsBytes)) {
		return base.Instance{}, errors.New("existing slot sandbox configuration does not match its specification")
	}
	return existing.Instance, nil
}

func (p *Provider) Inspect(ctx context.Context, providerRef string) (base.Status, error) {
	return p.inspect(ctx, providerRef)
}

func (p *Provider) InspectSlot(ctx context.Context, slotID string) (base.Status, error) {
	if strings.TrimSpace(slotID) == "" || len(slotID) > 128 {
		return base.Status{}, base.ErrNotFound
	}
	return p.inspect(ctx, containerName(slotID))
}

func (p *Provider) Start(ctx context.Context, providerRef string) error {
	container, err := p.readSandbox(ctx, providerRef)
	if err != nil {
		return err
	}
	err = p.engine.StartContainer(ctx, container.ID)
	if IsNotModified(err) {
		return nil
	}
	return err
}

func (p *Provider) Drain(ctx context.Context, providerRef string, deadline time.Time) error {
	if deadline.IsZero() || !deadline.After(p.config.Now()) {
		return errors.New("drain deadline must be in the future")
	}
	return p.engine.KillContainer(ctx, providerRef, "USR1")
}

func (p *Provider) Stop(ctx context.Context, providerRef string) error {
	err := p.engine.StopContainer(ctx, providerRef, p.config.StopTimeout)
	if IsNotFound(err) || IsNotModified(err) {
		return nil
	}
	return err
}

func (p *Provider) Destroy(ctx context.Context, providerRef string) error {
	containerErr := p.engine.RemoveContainer(ctx, providerRef, true, true)
	if IsNotFound(containerErr) {
		containerErr = nil
	}
	networkName, nameErr := p.networkNameFromProviderRef(providerRef)
	if nameErr != nil {
		return errors.Join(containerErr, nameErr)
	}
	networkErr := p.engine.RemoveNetwork(ctx, networkName)
	if IsNotFound(networkErr) {
		networkErr = nil
	}
	return errors.Join(containerErr, networkErr)
}

func (p *Provider) ensureSlotNetwork(ctx context.Context, slotID string) (bool, error) {
	name := p.networkName(slotID)
	network, err := p.engine.InspectNetwork(ctx, name)
	if err == nil {
		return false, validateSlotNetwork(network, name, slotID)
	}
	if !IsNotFound(err) {
		return false, fmt.Errorf("inspect slot Docker network: %w", err)
	}
	response, err := p.engine.CreateNetwork(ctx, CreateNetworkRequest{
		Name:           name,
		CheckDuplicate: true,
		Driver:         "bridge",
		Internal:       true,
		Attachable:     false,
		Labels: map[string]string{
			labelManaged: "true",
			labelSlotID:  slotID,
		},
	})
	if err != nil {
		if IsConflict(err) {
			network, inspectErr := p.engine.InspectNetwork(ctx, name)
			if inspectErr != nil {
				return false, errors.Join(fmt.Errorf("create slot Docker network: %w", err), inspectErr)
			}
			return false, validateSlotNetwork(network, name, slotID)
		}
		return false, fmt.Errorf("create slot Docker network: %w", err)
	}
	if strings.TrimSpace(response.ID) == "" {
		network, inspectErr := p.engine.InspectNetwork(ctx, name)
		if inspectErr == nil {
			return true, validateSlotNetwork(network, name, slotID)
		}
		return false, errors.Join(errors.New("Docker network create returned an empty id"), inspectErr)
	}
	return true, nil
}

func (p *Provider) slotNetworkGateway(ctx context.Context, name, slotID string) (string, error) {
	network, err := p.engine.InspectNetwork(ctx, name)
	if err != nil {
		return "", fmt.Errorf("inspect slot Docker network gateway: %w", err)
	}
	if err := validateSlotNetwork(network, name, slotID); err != nil {
		return "", err
	}
	if len(network.Containers) != 0 {
		return "", errors.New("new container dedicated network already has members")
	}
	gateway, _, err := sandboxNetworkGateway(network)
	return gateway.String(), err
}

func (p *Provider) inspect(ctx context.Context, providerRef string) (base.Status, error) {
	container, err := p.readSandbox(ctx, providerRef)
	if err != nil {
		return base.Status{}, err
	}
	return p.statusFromContainer(container, providerRef)
}

func (p *Provider) statusFromContainer(container Container, providerRef string) (base.Status, error) {
	epoch, err := strconv.ParseUint(container.Config.Labels[labelEpoch], 10, 64)
	if err != nil || epoch == 0 {
		return base.Status{}, errors.New("container has an invalid execution epoch label")
	}
	generation, err := strconv.ParseUint(container.Config.Labels[labelRuntimeGeneration], 10, 64)
	if err != nil || generation == 0 {
		return base.Status{}, errors.New("container has an invalid runtime generation label")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, container.Created)
	if err != nil {
		return base.Status{}, fmt.Errorf("parse Docker create time: %w", err)
	}
	state, healthy, reason := dockerState(container.State)
	return base.Status{
		Instance: base.Instance{
			ProviderRef:       providerRef,
			SlotID:            container.Config.Labels[labelSlotID],
			Epoch:             epoch,
			RuntimeGeneration: generation,
			State:             state,
			CreatedAt:         createdAt.UTC(),
			UpdatedAt:         p.config.Now().UTC(),
		},
		Healthy:     healthy,
		Reason:      reason,
		ImageDigest: container.Config.Labels[labelImageDigest],
	}, nil
}

func dockerState(state ContainerState) (slot.State, bool, string) {
	if !state.Running {
		if state.Status == "dead" {
			return slot.StateUnhealthy, false, "container is dead"
		}
		return slot.StateStopped, false, state.Status
	}
	if state.Health == nil {
		return slot.StateUnhealthy, false, "container healthcheck is missing"
	}
	switch state.Health.Status {
	case "healthy":
		return slot.StateReady, true, ""
	case "starting":
		return slot.StateStarting, false, "container healthcheck is starting"
	default:
		return slot.StateUnhealthy, false, "container healthcheck is " + state.Health.Status
	}
}

func containerName(slotID string) string {
	var normalized strings.Builder
	for _, character := range strings.ToLower(slotID) {
		if character <= unicode.MaxASCII && (unicode.IsLetter(character) || unicode.IsDigit(character) || character == '-' || character == '_') {
			normalized.WriteRune(character)
		} else {
			normalized.WriteByte('-')
		}
		if normalized.Len() == 32 {
			break
		}
	}
	baseName := strings.Trim(normalized.String(), "-_")
	if baseName == "" {
		baseName = "slot"
	}
	digest := sha256.Sum256([]byte(slotID))
	return "execution-slot-" + baseName + "-" + hex.EncodeToString(digest[:4])
}

func (p *Provider) networkName(slotID string) string {
	return p.config.NetworkPrefix + strings.TrimPrefix(containerName(slotID), "execution-slot-")
}

func (p *Provider) networkNameFromProviderRef(providerRef string) (string, error) {
	const prefix = "execution-slot-"
	if !strings.HasPrefix(providerRef, prefix) {
		return "", fmt.Errorf("invalid managed Docker provider ref %q", providerRef)
	}
	return p.config.NetworkPrefix + strings.TrimPrefix(providerRef, prefix), nil
}

func contains(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

var _ base.ExecutionProvider = (*Provider)(nil)
