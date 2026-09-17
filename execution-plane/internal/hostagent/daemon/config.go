// Package daemon contains the explicitly enabled, lifecycle-only host entry.
package daemon

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

var ErrConfig = errors.New("host runtime configuration rejected")

const envPrefix = "EXECUTION_HOST_AGENT_"

type Config struct {
	Enabled                                         bool
	ControlAddress, ControlServerName, DockerSocket string
	TrustFile, NodeCertFile, NodeKeyFile            string
	TicketPublicKey, UpstreamBaseURL                string
	RuntimePort                                     uint16
	Resources                                       provider.ResourceLimits
	Security                                        provider.SecurityPolicy
	Network                                         provider.NetworkPolicy
	ReadyTimeout, StartupTimeout, ShutdownTimeout   time.Duration
}

func (Config) String() string               { return "host runtime configuration [redacted]" }
func (c Config) GoString() string           { return c.String() }
func (c Config) Format(s fmt.State, _ rune) { _, _ = fmt.Fprint(s, c.String()) }
func (Config) MarshalJSON() ([]byte, error) { return []byte(`{"configuration":"redacted"}`), nil }

// Load reads only the enable flag when disabled. Neither a default control
// address nor credentials can silently turn this entry into a running agent.
func Load(getenv func(string) string) (Config, error) {
	if getenv == nil {
		return Config{}, ErrConfig
	}
	switch getenv(envPrefix + "RUNTIME_ENABLED") {
	case "", "false":
		return Config{}, nil
	case "true":
	default:
		return Config{}, ErrConfig
	}
	c := Config{Enabled: true,
		ControlAddress: getenv(envPrefix + "CONTROL_ADDRESS"), ControlServerName: getenv(envPrefix + "CONTROL_SERVER_NAME"),
		DockerSocket: getenv(envPrefix + "DOCKER_SOCKET"), TrustFile: getenv(envPrefix + "TRUST_FILE"),
		NodeCertFile: getenv(envPrefix + "NODE_CERT_FILE"), NodeKeyFile: getenv(envPrefix + "NODE_KEY_FILE"),
		TicketPublicKey: getenv(envPrefix + "TICKET_PUBLIC_KEY"), UpstreamBaseURL: getenv(envPrefix + "UPSTREAM_BASE_URL"),
		RuntimePort:  8093,
		Resources:    provider.ResourceLimits{CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 64 << 20},
		Security:     provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "builtin", AppArmorProfile: "docker-default"},
		Network:      provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:8094"},
		ReadyTimeout: 30 * time.Second, StartupTimeout: 5 * time.Second, ShutdownTimeout: 10 * time.Second,
	}
	for _, item := range []struct {
		name  string
		value *int64
	}{
		{"CPU_MILLI", &c.Resources.CPUMilli}, {"MEMORY_BYTES", &c.Resources.MemoryBytes},
		{"PIDS", &c.Resources.PIDs}, {"TMPFS_BYTES", &c.Resources.TmpfsBytes},
	} {
		if raw := getenv(envPrefix + item.name); raw != "" {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n <= 0 || strconv.FormatInt(n, 10) != raw {
				return Config{}, ErrConfig
			}
			*item.value = n
		}
	}
	if raw := getenv(envPrefix + "RUNTIME_PORT"); raw != "" {
		n, err := strconv.ParseUint(raw, 10, 16)
		if err != nil || n == 0 || strconv.FormatUint(n, 10) != raw {
			return Config{}, ErrConfig
		}
		c.RuntimePort = uint16(n)
	}
	for _, item := range []struct {
		name  string
		value *time.Duration
	}{
		{"READY_TIMEOUT", &c.ReadyTimeout}, {"STARTUP_TIMEOUT", &c.StartupTimeout}, {"SHUTDOWN_TIMEOUT", &c.ShutdownTimeout},
	} {
		if raw := getenv(envPrefix + item.name); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil || d <= 0 || strings.TrimSpace(raw) != raw {
				return Config{}, ErrConfig
			}
			*item.value = d
		}
	}
	if raw := getenv(envPrefix + "EGRESS_PROXY_URL"); raw != "" {
		c.Network.EgressProxyEndpoint = raw
	}
	if c.Validate() != nil {
		return Config{}, ErrConfig
	}
	return c, nil
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validDNS(name string) bool {
	if name == "" || len(name) > 253 || net.ParseIP(name) != nil {
		return false
	}
	for _, part := range strings.Split(name, ".") {
		if !dnsLabel.MatchString(part) {
			return false
		}
	}
	return true
}

func cleanPath(path string) bool {
	if !utf8.ValidString(path) {
		return false
	}
	for _, char := range path {
		if char < 32 || char == 127 {
			return false
		}
	}
	return path != "/" && len(path) <= 4096 && filepath.IsAbs(path) && filepath.Clean(path) == path &&
		!strings.ContainsAny(path, "\x00\r\n") && strings.TrimSpace(path) == path
}

func validControlAddress(raw string) bool {
	if len(raw) > 128 {
		return false
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || ip.Zone() != "" || ip.Is4In6() || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return false
	}
	p, err := strconv.ParseUint(port, 10, 16)
	return err == nil && p > 0 && strconv.FormatUint(p, 10) == port && net.JoinHostPort(ip.String(), port) == raw
}

func validHTTPSOrigin(raw string) bool {
	if len(raw) > 2048 {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" ||
		(u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || u.String() != raw {
		return false
	}
	host := u.Hostname()
	if !validDNS(host) {
		ip, err := netip.ParseAddr(host)
		if err != nil || ip.Zone() != "" || ip.Is4In6() || ip.String() != host {
			return false
		}
	}
	expected := host
	if strings.Contains(host, ":") {
		expected = "[" + host + "]"
	}
	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 || strconv.FormatUint(port, 10) != u.Port() {
			return false
		}
		expected = net.JoinHostPort(host, u.Port())
	}
	return u.Host == expected
}

func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !validControlAddress(c.ControlAddress) || !validDNS(c.ControlServerName) ||
		!cleanPath(c.DockerSocket) || !cleanPath(c.TrustFile) || !cleanPath(c.NodeCertFile) || !cleanPath(c.NodeKeyFile) ||
		c.TrustFile == c.NodeCertFile || c.TrustFile == c.NodeKeyFile || c.NodeCertFile == c.NodeKeyFile ||
		c.DockerSocket == c.TrustFile || c.DockerSocket == c.NodeCertFile || c.DockerSocket == c.NodeKeyFile ||
		!validHTTPSOrigin(c.UpstreamBaseURL) || c.RuntimePort == 0 {
		return ErrConfig
	}
	if len(c.TicketPublicKey) != 43 {
		return ErrConfig
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(c.TicketPublicKey)
	if err != nil || len(key) != 32 || base64.RawStdEncoding.EncodeToString(key) != c.TicketPublicKey {
		return ErrConfig
	}
	if c.Resources.Validate() != nil || c.Resources.CPUMilli > 8000 || c.Resources.MemoryBytes > 8<<30 ||
		c.Resources.PIDs > 4096 || c.Resources.TmpfsBytes > 1<<30 || c.Resources.TmpfsBytes > c.Resources.MemoryBytes {
		return ErrConfig
	}
	if c.Security != (provider.SecurityPolicy{RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true, DropAllCapabilities: true, SeccompProfile: "builtin", AppArmorProfile: "docker-default"}) ||
		c.Network.Validate() != nil || c.ReadyTimeout <= 0 || c.ReadyTimeout > time.Minute ||
		c.StartupTimeout <= 0 || c.StartupTimeout > 30*time.Second || c.ShutdownTimeout <= 0 || c.ShutdownTimeout > 30*time.Second {
		return ErrConfig
	}
	return nil
}
