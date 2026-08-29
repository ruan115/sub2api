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
	labelManaged     = "com.sub2api.execution.managed"
	labelSlotID      = "com.sub2api.execution.slot_id"
	labelAccountHash = "com.sub2api.execution.account_hash"
	labelEpoch       = "com.sub2api.execution.epoch"
	labelImageDigest = "com.sub2api.execution.image_digest"
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
}

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
	return &Provider{config: config, engine: engine}, nil
}

func (p *Provider) Create(ctx context.Context, spec base.SlotSpec) (base.Instance, error) {
	if err := spec.Validate(); err != nil {
		return base.Instance{}, err
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
		if networkCreated {
			_ = p.engine.RemoveNetwork(ctx, networkName)
		}
		return base.Instance{}, err
	}

	tmpfsBytes := int64(math.Max(float64(spec.Resources.TmpfsBytes/2), float64(1<<20)))
	initProcess := true
	stopTimeout := int(math.Ceil(p.config.StopTimeout.Seconds()))
	environment := []string{
		"EXECUTION_SLOT_ID=" + spec.SlotID,
		"EXECUTION_EPOCH=" + strconv.FormatUint(spec.Epoch, 10),
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
		)
		exposedPorts = map[string]struct{}{containerPort: {}}
	}
	response, err := p.engine.CreateContainer(ctx, name, CreateContainerRequest{
		Image:       spec.ImageDigest,
		Hostname:    name,
		User:        strconv.FormatUint(uint64(spec.Security.RunAsUser), 10) + ":" + strconv.FormatUint(uint64(spec.Security.RunAsUser), 10),
		StopTimeout: &stopTimeout,
		Labels: map[string]string{
			labelManaged:     "true",
			labelSlotID:      spec.SlotID,
			labelAccountHash: base.RuntimeAccountID(spec.AccountID),
			labelEpoch:       strconv.FormatUint(spec.Epoch, 10),
			labelImageDigest: spec.ImageDigest,
		},
		Env:          environment,
		ExposedPorts: exposedPorts,
		HostConfig: HostConfig{
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
		ProviderRef: name,
		SlotID:      spec.SlotID,
		Epoch:       spec.Epoch,
		State:       slot.StateStopped,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

func (p *Provider) RuntimeEndpoint(ctx context.Context, providerRef string) (string, error) {
	if p.config.WorkerBootstrap == nil {
		return "", errors.New("worker runtime bootstrap is not configured")
	}
	container, err := p.engine.InspectContainer(ctx, providerRef)
	if err != nil {
		return "", err
	}
	if container.Config.Labels[labelManaged] != "true" || container.Config.Labels[labelSlotID] == "" {
		return "", errors.New("container is not a managed execution slot")
	}
	if err := p.validateSandbox(container); err != nil {
		return "", err
	}
	expectedNetwork := p.networkName(container.Config.Labels[labelSlotID])
	if container.HostConfig.NetworkMode != expectedNetwork {
		return "", fmt.Errorf("worker runtime is attached to unexpected Docker network %q", container.HostConfig.NetworkMode)
	}
	if _, err := p.slotNetworkGateway(ctx, expectedNetwork, container.Config.Labels[labelSlotID]); err != nil {
		return "", err
	}
	network := container.NetworkSettings.Networks[container.HostConfig.NetworkMode]
	ip := net.ParseIP(network.IPAddress)
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return "", fmt.Errorf("worker runtime has no private address on slot network %q", container.HostConfig.NetworkMode)
	}
	return net.JoinHostPort(ip.String(), strconv.FormatUint(uint64(p.config.WorkerBootstrap.RuntimePort), 10)), nil
}

func (p *Provider) existingSlot(ctx context.Context, name string, spec base.SlotSpec) (base.Instance, error) {
	existing, err := p.inspect(ctx, name)
	if err != nil {
		return base.Instance{}, err
	}
	if existing.SlotID != spec.SlotID {
		return base.Instance{}, fmt.Errorf("container name collision for slot %q", spec.SlotID)
	}
	if existing.Epoch != spec.Epoch {
		return base.Instance{}, fmt.Errorf("slot %q already exists at epoch %d", spec.SlotID, existing.Epoch)
	}
	if existing.ImageDigest != spec.ImageDigest {
		return base.Instance{}, fmt.Errorf("slot %q already exists with a different image digest", spec.SlotID)
	}
	container, err := p.engine.InspectContainer(ctx, name)
	if err != nil {
		return base.Instance{}, err
	}
	expectedNetwork := p.networkName(spec.SlotID)
	if container.HostConfig.NetworkMode != expectedNetwork {
		return base.Instance{}, fmt.Errorf("slot %q is attached to unexpected Docker network %q", spec.SlotID, container.HostConfig.NetworkMode)
	}
	if _, err := p.ensureSlotNetwork(ctx, spec.SlotID); err != nil {
		return base.Instance{}, err
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
	err := p.engine.StartContainer(ctx, providerRef)
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

func validateSlotNetwork(network Network, name, slotID string) error {
	if !network.Internal {
		return fmt.Errorf("Docker network %q must have Internal=true", name)
	}
	if network.Labels[labelManaged] != "true" || network.Labels[labelSlotID] != slotID {
		return fmt.Errorf("Docker network %q is not owned by slot %q", name, slotID)
	}
	return nil
}

func (p *Provider) slotNetworkGateway(ctx context.Context, name, slotID string) (string, error) {
	network, err := p.engine.InspectNetwork(ctx, name)
	if err != nil {
		return "", fmt.Errorf("inspect slot Docker network gateway: %w", err)
	}
	if err := validateSlotNetwork(network, name, slotID); err != nil {
		return "", err
	}
	for _, config := range network.IPAM.Config {
		gateway := net.ParseIP(config.Gateway)
		if gateway != nil && (gateway.IsPrivate() || gateway.IsLoopback()) {
			return gateway.String(), nil
		}
	}
	return "", fmt.Errorf("Docker network %q has no private gateway", name)
}

func (p *Provider) inspect(ctx context.Context, providerRef string) (base.Status, error) {
	container, err := p.engine.InspectContainer(ctx, providerRef)
	if err != nil {
		if IsNotFound(err) {
			return base.Status{}, base.ErrNotFound
		}
		return base.Status{}, err
	}
	if container.Config.Labels[labelManaged] != "true" || container.Config.Labels[labelSlotID] == "" {
		return base.Status{}, errors.New("container is not a managed execution slot")
	}
	if err := p.validateSandbox(container); err != nil {
		return base.Status{}, err
	}
	epoch, err := strconv.ParseUint(container.Config.Labels[labelEpoch], 10, 64)
	if err != nil || epoch == 0 {
		return base.Status{}, errors.New("container has an invalid execution epoch label")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, container.Created)
	if err != nil {
		return base.Status{}, fmt.Errorf("parse Docker create time: %w", err)
	}
	state, healthy, reason := dockerState(container.State)
	return base.Status{
		Instance: base.Instance{
			ProviderRef: providerRef,
			SlotID:      container.Config.Labels[labelSlotID],
			Epoch:       epoch,
			State:       state,
			CreatedAt:   createdAt.UTC(),
			UpdatedAt:   p.config.Now().UTC(),
		},
		Healthy:     healthy,
		Reason:      reason,
		ImageDigest: container.Config.Labels[labelImageDigest],
	}, nil
}

func (p *Provider) validateSandbox(container Container) error {
	user := strings.SplitN(container.Config.User, ":", 2)[0]
	if user == "" || user == "0" || strings.EqualFold(user, "root") {
		return errors.New("container sandbox requires a non-root user")
	}
	if !container.HostConfig.ReadonlyRootfs {
		return errors.New("container sandbox requires a read-only root filesystem")
	}
	if !contains(container.HostConfig.CapDrop, "ALL") {
		return errors.New("container sandbox must drop all capabilities")
	}
	if !hasSecurityOption(container.HostConfig.SecurityOpt, "no-new-privileges") {
		return errors.New("container sandbox security profiles are incomplete")
	}
	if !hasAllowedSecurityProfile(container.HostConfig.SecurityOpt, "seccomp=", p.config.AllowedSeccompProfiles) {
		return errors.New("container sandbox seccomp profile is not allowed")
	}
	if !hasAllowedSecurityProfile(container.HostConfig.SecurityOpt, "apparmor=", p.config.AllowedAppArmorProfiles) &&
		!contains(p.config.AllowedAppArmorProfiles, container.AppArmorProfile) {
		return errors.New("container sandbox AppArmor profile is not allowed")
	}
	if container.HostConfig.PidsLimit <= 0 || container.HostConfig.Memory <= 0 || container.HostConfig.NanoCPUs <= 0 {
		return errors.New("container sandbox resource limits are incomplete")
	}
	if container.HostConfig.Tmpfs["/tmp"] == "" || container.HostConfig.Tmpfs["/run"] == "" {
		return errors.New("container sandbox tmpfs mounts are incomplete")
	}
	if len(container.HostConfig.Binds) != 0 || len(container.Mounts) != 0 {
		return errors.New("container sandbox must not mount host or named volumes")
	}
	if container.HostConfig.Init == nil || !*container.HostConfig.Init {
		return errors.New("container sandbox must use an init process")
	}
	if len(container.HostConfig.PortBindings) != 0 {
		return errors.New("container sandbox must not publish worker ports")
	}
	for _, value := range container.Config.Env {
		name, _, _ := strings.Cut(value, "=")
		switch strings.ToUpper(name) {
		case "ANTHROPIC_API_KEY", "API_KEY", "ACCESS_TOKEN", "REFRESH_TOKEN", "SESSION_KEY", "PASSWORD", "COOKIE", "AUTHORIZATION", "PROXY_PASSWORD":
			return fmt.Errorf("container environment contains forbidden secret field %q", name)
		}
	}
	return nil
}

func hasSecurityOption(options []string, expected string) bool {
	for _, option := range options {
		if option == expected || strings.HasPrefix(option, expected) {
			return true
		}
	}
	return false
}

func hasAllowedSecurityProfile(options []string, prefix string, allowed []string) bool {
	for _, option := range options {
		if strings.HasPrefix(option, prefix) && contains(allowed, strings.TrimPrefix(option, prefix)) {
			return true
		}
	}
	return false
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
