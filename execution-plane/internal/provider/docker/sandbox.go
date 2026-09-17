package docker

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

const sandboxRuntime = "runc"

var sandboxDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var sandboxAccountPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func validSandboxProfileAllowlist(profiles []string) bool {
	for _, profile := range profiles {
		if profile == "" || strings.TrimSpace(profile) != profile || !utf8.ValidString(profile) || strings.EqualFold(profile, "unconfined") {
			return false
		}
		for _, char := range profile {
			if unicode.IsControl(char) {
				return false
			}
		}
	}
	return true
}

// readSandbox is a read-only, point-in-time adoption gate. It does not pull,
// repair, disconnect or delete drifted objects, and is not a host firewall.
func (p *Provider) readSandbox(ctx context.Context, ref string) (Container, error) {
	container, err := p.engine.InspectContainer(ctx, ref)
	if err != nil {
		if IsNotFound(err) {
			return Container{}, base.ErrNotFound
		}
		return Container{}, err
	}
	if err := p.validateSandbox(container); err != nil {
		return Container{}, err
	}
	if err := p.validateSandboxImage(ctx, container); err != nil {
		return Container{}, err
	}
	if _, err := p.validateSandboxNetwork(ctx, container); err != nil {
		return Container{}, err
	}
	if err := ctx.Err(); err != nil {
		return Container{}, err
	}
	return container, nil
}

func (p *Provider) validateSandbox(container Container) error {
	labels := container.Config.Labels
	slotID := labels[labelSlotID]
	if labels[labelManaged] != "true" || slotID == "" || container.ID == "" ||
		container.Name != "/"+containerName(slotID) || container.Config.Hostname != containerName(slotID) ||
		!sandboxAccountPattern.MatchString(labels[labelAccountHash]) {
		return errors.New("container sandbox identity is invalid")
	}
	epoch, err := strconv.ParseUint(labels[labelEpoch], 10, 64)
	if err != nil || epoch == 0 || strconv.FormatUint(epoch, 10) != labels[labelEpoch] {
		return errors.New("container sandbox epoch is invalid")
	}
	generation, err := strconv.ParseUint(labels[labelRuntimeGeneration], 10, 64)
	if err != nil || generation == 0 || strconv.FormatUint(generation, 10) != labels[labelRuntimeGeneration] {
		return errors.New("container sandbox runtime generation is invalid")
	}
	user := strings.Split(container.Config.User, ":")
	if len(user) != 2 || !canonicalNonRootID(user[0]) || user[0] != user[1] {
		return errors.New("container sandbox requires a canonical non-root user and group")
	}
	host := container.HostConfig
	if !host.ReadonlyRootfs {
		return errors.New("container sandbox requires a read-only root filesystem")
	}
	if host.Privileged || len(host.CapAdd) != 0 || len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" {
		return errors.New("container sandbox must be unprivileged and drop all capabilities")
	}
	if host.Runtime != sandboxRuntime || host.PidMode != "" || host.UTSMode != "" || host.UsernsMode != "" ||
		host.IpcMode != "private" || host.CgroupnsMode != "private" {
		return errors.New("container sandbox runtime or namespace is not isolated")
	}
	if err := p.validateSecurityOptions(host.SecurityOpt, container.AppArmorProfile); err != nil {
		return err
	}
	if host.PidsLimit <= 0 || host.Memory <= 0 || host.NanoCPUs <= 0 {
		return errors.New("container sandbox resource limits are incomplete")
	}
	if len(host.Tmpfs) != 2 || !validSandboxTmpfs(host.Tmpfs["/tmp"]) || !validSandboxTmpfs(host.Tmpfs["/run"]) {
		return errors.New("container sandbox tmpfs mounts are unsafe")
	}
	if len(host.Binds) != 0 || len(host.Mounts) != 0 || len(host.VolumesFrom) != 0 ||
		len(host.Devices) != 0 || len(host.DeviceRequests) != 0 || len(host.DeviceCgroupRules) != 0 {
		return errors.New("container sandbox must not mount host, device or named volumes")
	}
	// Engines can project HostConfig.Tmpfs into Mounts. Only those exact
	// anonymous tmpfs destinations are safe; they are not persistent volumes.
	seenMounts := make(map[string]bool)
	for _, mount := range container.Mounts {
		if mount.Type != "tmpfs" || mount.Source != "" || !mount.RW || mount.Mode != "" || mount.Propagation != "" ||
			(mount.Destination != "/tmp" && mount.Destination != "/run") || seenMounts[mount.Destination] {
			return errors.New("container sandbox has an unexpected actual mount")
		}
		seenMounts[mount.Destination] = true
	}
	if host.Init == nil || !*host.Init {
		return errors.New("container sandbox must use an init process")
	}
	if host.PublishAllPorts || len(host.PortBindings) != 0 || len(host.Links) != 0 {
		return errors.New("container sandbox must not publish or link worker ports")
	}
	for _, bindings := range container.NetworkSettings.Ports {
		if len(bindings) != 0 {
			return errors.New("container sandbox must not publish worker ports")
		}
	}
	env, err := sandboxEnvironment(container)
	if err != nil {
		return err
	}
	if bootstrap := p.config.WorkerBootstrap; bootstrap != nil {
		port := strconv.FormatUint(uint64(bootstrap.RuntimePort), 10)
		expected := map[string]string{
			"EXECUTION_ACCOUNT_HASH": labels[labelAccountHash], "EXECUTION_NODE_ID": bootstrap.NodeID,
			"EXECUTION_IMAGE_DIGEST": labels[labelImageDigest], "EXECUTION_TICKET_PUBLIC_KEY": bootstrap.TicketPublicKey,
			"EXECUTION_LISTEN_ADDRESS": "0.0.0.0:" + port, "EXECUTION_UPSTREAM_BASE_URL": strings.TrimSuffix(bootstrap.UpstreamBaseURL, "/"),
			"EXECUTION_ALLOW_FAKE_ACTIVATION": strconv.FormatBool(bootstrap.AllowFakeActivation),
			"EXECUTION_IDENTITY_DIRECTORY":    bootstrap.IdentityDirectory,
			"EXECUTION_RUNTIME_TRUST_FILE":    bootstrap.RuntimeTrustFile,
			"EXECUTION_BOOTSTRAP_CA_SHA256":   bootstrap.TrustSHA256,
		}
		for name, value := range expected {
			if env[name] != value {
				return errors.New("container worker bootstrap does not match this node and instance")
			}
		}
		if bootstrap.TrustSHA256 != "" && !contains(strings.Split(host.Tmpfs["/run"], ","), "mode=1777") {
			return errors.New("container bootstrap runtime tmpfs mode is invalid")
		}
	}
	return nil
}

func canonicalNonRootID(raw string) bool {
	value, err := strconv.ParseUint(raw, 10, 32)
	return err == nil && value > 0 && strconv.FormatUint(value, 10) == raw
}

func (p *Provider) validateSecurityOptions(options []string, actualAppArmor string) error {
	nnp, seccomp, apparmor := false, false, false
	for _, option := range options {
		switch {
		case option == "no-new-privileges" || option == "no-new-privileges=true":
			if nnp {
				return errors.New("container sandbox has duplicate no-new-privileges")
			}
			nnp = true
		case strings.HasPrefix(option, "seccomp="):
			if seccomp || !contains(p.config.AllowedSeccompProfiles, strings.TrimPrefix(option, "seccomp=")) {
				return errors.New("container sandbox seccomp profile is not allowed")
			}
			seccomp = true
		case strings.HasPrefix(option, "apparmor="):
			profile := strings.TrimPrefix(option, "apparmor=")
			if apparmor || !contains(p.config.AllowedAppArmorProfiles, profile) || profile != actualAppArmor {
				return errors.New("container sandbox AppArmor profile is not allowed")
			}
			apparmor = true
		default:
			return errors.New("container sandbox security option is not allowed")
		}
	}
	if !nnp || !seccomp || !contains(p.config.AllowedAppArmorProfiles, actualAppArmor) {
		return errors.New("container sandbox security profiles are incomplete")
	}
	return nil
}

func validSandboxTmpfs(raw string) bool {
	seen := make(map[string]bool)
	for _, option := range strings.Split(raw, ",") {
		name, value, hasValue := strings.Cut(option, "=")
		if seen[name] {
			return false
		}
		seen[name] = true
		switch name {
		case "rw", "noexec", "nosuid", "nodev":
			if hasValue {
				return false
			}
		case "size":
			n, err := strconv.ParseUint(value, 10, 63)
			if !hasValue || err != nil || n == 0 || strconv.FormatUint(n, 10) != value {
				return false
			}
		case "mode":
			if !hasValue || value != "1777" {
				return false
			}
		default:
			return false
		}
	}
	return (len(seen) == 5 || (len(seen) == 6 && seen["mode"])) && seen["rw"] && seen["noexec"] && seen["nosuid"] && seen["nodev"] && seen["size"]
}

func sandboxTmpfsBytes(total int64) int64 {
	size := total / 2
	if size < 1<<20 {
		return 1 << 20
	}
	return size
}

func sandboxTmpfsSizeMatches(raw string, expected int64) bool {
	return validSandboxTmpfs(raw) && contains(strings.Split(raw, ","), "size="+strconv.FormatInt(expected, 10))
}

func sandboxEnvironment(container Container) (map[string]string, error) {
	env := make(map[string]string)
	for _, raw := range container.Config.Env {
		name, value, ok := strings.Cut(raw, "=")
		if !ok || name == "" {
			return nil, errors.New("container environment is malformed")
		}
		if _, duplicate := env[name]; duplicate {
			return nil, errors.New("container environment contains duplicate fields")
		}
		env[name] = value
		switch strings.ToUpper(name) {
		case "ANTHROPIC_API_KEY", "API_KEY", "ACCESS_TOKEN", "REFRESH_TOKEN", "SESSION_KEY", "PASSWORD", "COOKIE", "AUTHORIZATION", "PROXY_PASSWORD":
			return nil, errors.New("container environment contains a forbidden secret field")
		}
	}
	if env["EXECUTION_SLOT_ID"] != container.Config.Labels[labelSlotID] || env["EXECUTION_EPOCH"] != container.Config.Labels[labelEpoch] ||
		env["EXECUTION_RUNTIME_GENERATION"] != container.Config.Labels[labelRuntimeGeneration] {
		return nil, errors.New("container environment identity is inconsistent")
	}
	proxy := env["EXECUTION_EGRESS_PROXY_URL"]
	if (base.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: proxy}).Validate() != nil ||
		env["HTTP_PROXY"] != proxy || env["HTTPS_PROXY"] != proxy || env["NO_PROXY"] != "127.0.0.1,localhost" {
		return nil, errors.New("container fixed egress environment is inconsistent")
	}
	return env, nil
}

func (p *Provider) validateSandboxImage(ctx context.Context, container Container) error {
	ref := container.Config.Labels[labelImageDigest]
	if !sandboxImmutableReference(ref) || container.Config.Image != ref || !sandboxDigestPattern.MatchString(container.Image) {
		return errors.New("container actual image reference is inconsistent")
	}
	image, err := p.engine.InspectImage(ctx, ref)
	if err != nil || !sandboxDigestPattern.MatchString(image.ID) || image.ID != container.Image {
		return errors.New("container actual image cannot be verified")
	}
	// Bare sha256 references are local config IDs. Repository digest references
	// are resolved by the Engine to that ID; their manifest hash must not be
	// compared directly with Container.Image (a different kind of digest).
	if strings.HasPrefix(ref, "sha256:") && ref != image.ID {
		return errors.New("container local image id is inconsistent")
	}
	return nil
}

func sandboxImmutableReference(ref string) bool {
	if sandboxDigestPattern.MatchString(ref) {
		return true
	}
	name, digest, ok := strings.Cut(ref, "@")
	return ok && name != "" && strings.TrimSpace(name) == name && !strings.ContainsAny(name, "\x00\r\n\t @") && sandboxDigestPattern.MatchString(digest)
}

func (p *Provider) validateSandboxNetwork(ctx context.Context, container Container) (string, error) {
	slotID := container.Config.Labels[labelSlotID]
	expected := p.networkName(slotID)
	if container.HostConfig.NetworkMode != expected || len(container.NetworkSettings.Networks) != 1 {
		return "", errors.New("container must use exactly its dedicated slot network")
	}
	endpoint, exists := container.NetworkSettings.Networks[expected]
	if !exists {
		return "", errors.New("container is attached to an unexpected network")
	}
	network, err := p.engine.InspectNetwork(ctx, expected)
	if err != nil {
		return "", errors.New("container dedicated network cannot be inspected")
	}
	if err := validateSlotNetwork(network, expected, slotID); err != nil {
		return "", err
	}
	if len(network.Containers) > 1 {
		return "", errors.New("container dedicated network has other members")
	}
	for id := range network.Containers {
		if id != container.ID {
			return "", errors.New("container dedicated network belongs to another instance")
		}
	}
	gateway, subnet, err := sandboxNetworkGateway(network)
	if err != nil {
		return "", err
	}
	if len(container.HostConfig.ExtraHosts) != 1 || container.HostConfig.ExtraHosts[0] != "host-agent.execution.internal:"+gateway.String() {
		return "", errors.New("container host-agent gateway mapping is inconsistent")
	}
	if endpoint.NetworkID != "" && endpoint.NetworkID != network.ID {
		return "", errors.New("container dedicated network id is inconsistent")
	}
	if endpoint.GlobalIPv6Address != "" {
		return "", errors.New("container IPv6 attachment is not supported")
	}
	if !container.State.Running && endpoint.IPAddress == "" {
		if endpoint.Gateway != "" || len(network.Containers) != 0 {
			return "", errors.New("stopped container network attachment is inconsistent")
		}
		return gateway.String(), nil
	}
	ip, err := netip.ParseAddr(endpoint.IPAddress)
	member, memberExists := network.Containers[container.ID]
	memberAddress, memberErr := netip.ParsePrefix(member.IPv4Address)
	if err != nil || !ip.Is4() || !ip.IsPrivate() || !subnet.Contains(ip) || ip == gateway || endpoint.NetworkID != network.ID ||
		endpoint.Gateway != gateway.String() || !memberExists || member.Name != containerName(slotID) || memberErr != nil ||
		memberAddress.Addr() != ip || memberAddress.Bits() != subnet.Bits() || member.IPv6Address != "" {
		return "", errors.New("container actual network endpoint cannot be verified")
	}
	return gateway.String(), nil
}

func sandboxNetworkGateway(network Network) (netip.Addr, netip.Prefix, error) {
	if len(network.IPAM.Config) != 1 {
		return netip.Addr{}, netip.Prefix{}, errors.New("slot network must have one IPv4 subnet")
	}
	config := network.IPAM.Config[0]
	gateway, err := netip.ParseAddr(config.Gateway)
	subnet, subnetErr := netip.ParsePrefix(config.Subnet)
	if err != nil || subnetErr != nil || !gateway.Is4() || !gateway.IsPrivate() || !subnet.Addr().Is4() ||
		!subnet.Contains(gateway) || subnet != subnet.Masked() {
		return netip.Addr{}, netip.Prefix{}, errors.New("slot network has no valid private IPv4 gateway")
	}
	return gateway, subnet, nil
}

func validateSlotNetwork(network Network, name, slotID string) error {
	if !network.Internal {
		return fmt.Errorf("Docker network %q must have Internal=true", name)
	}
	if network.ID == "" || network.Name != name || network.Driver != "bridge" || network.Attachable || network.EnableIPv6 ||
		network.Labels[labelManaged] != "true" || network.Labels[labelSlotID] != slotID {
		return errors.New("Docker slot network identity or isolation is invalid")
	}
	return nil
}
