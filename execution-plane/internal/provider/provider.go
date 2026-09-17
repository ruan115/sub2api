package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/slot"
)

var ErrNotFound = errors.New("execution instance not found")

// RuntimeAccountID returns the opaque account identity used inside an
// execution slot. The source account id never needs to appear in Docker
// labels, environment variables or process arguments.
func RuntimeAccountID(accountID string) string {
	digest := sha256.Sum256([]byte(accountID))
	return hex.EncodeToString(digest[:16])
}

type ResourceLimits struct {
	CPUMilli    int64
	MemoryBytes int64
	PIDs        int64
	TmpfsBytes  int64
}

func (r ResourceLimits) Validate() error {
	if r.CPUMilli <= 0 {
		return errors.New("cpu limit must be positive")
	}
	if r.MemoryBytes <= 0 {
		return errors.New("memory limit must be positive")
	}
	if r.PIDs <= 0 {
		return errors.New("pids limit must be positive")
	}
	if r.TmpfsBytes <= 0 {
		return errors.New("tmpfs limit must be positive")
	}
	return nil
}

type SecurityPolicy struct {
	RunAsUser           uint32
	ReadOnlyRootFS      bool
	NoNewPrivileges     bool
	DropAllCapabilities bool
	SeccompProfile      string
	AppArmorProfile     string
}

func (p SecurityPolicy) Validate() error {
	if p.RunAsUser == 0 {
		return errors.New("worker must run as a non-root user")
	}
	if !p.ReadOnlyRootFS || !p.NoNewPrivileges || !p.DropAllCapabilities {
		return errors.New("worker sandbox policy is incomplete")
	}
	if p.SeccompProfile == "" {
		return errors.New("seccomp profile is required")
	}
	if p.AppArmorProfile == "" {
		return errors.New("AppArmor profile is required")
	}
	return nil
}

type NetworkPolicy struct {
	DenyDirectInternet  bool
	EgressProxyEndpoint string
}

func (p NetworkPolicy) Validate() error {
	if !p.DenyDirectInternet {
		return errors.New("direct worker internet access must be denied")
	}
	return ValidateEgressProxyURL(p.EgressProxyEndpoint)
}

// ValidateEgressProxyURL is the shared, pure boundary for provider bootstrap
// and worker fixed transport. The proxy contains no account credentials.
func ValidateEgressProxyURL(raw string) error {
	const invalid = "egress proxy endpoint must be a credential-free internal HTTP origin with a canonical port"
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Scheme != "http" || endpoint.Opaque != "" || endpoint.User != nil ||
		endpoint.Hostname() != "host-agent.execution.internal" || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || endpoint.RawFragment != "" || endpoint.RawPath != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return errors.New(invalid)
	}
	port, err := strconv.ParseUint(endpoint.Port(), 10, 16)
	if err != nil || port == 0 || strconv.FormatUint(port, 10) != endpoint.Port() ||
		endpoint.Host != net.JoinHostPort("host-agent.execution.internal", endpoint.Port()) || endpoint.String() != raw {
		return errors.New(invalid)
	}
	return nil
}

type SlotSpec struct {
	SlotID    string
	AccountID string
	Epoch     uint64
	// RuntimeGeneration is the control-plane desired generation, never a
	// route-cache generation or a value inferred from the execution epoch.
	RuntimeGeneration uint64
	ImageDigest       string
	Resources         ResourceLimits
	Security          SecurityPolicy
	Network           NetworkPolicy
	Metadata          map[string]string
}

func (s SlotSpec) Validate() error {
	if s.SlotID == "" {
		return errors.New("slot id is required")
	}
	if s.AccountID == "" {
		return errors.New("account id is required")
	}
	if s.Epoch == 0 {
		return errors.New("execution epoch must be positive")
	}
	if s.RuntimeGeneration == 0 {
		return errors.New("runtime generation must be positive")
	}
	if !immutableImageReference(s.ImageDigest) {
		return errors.New("immutable image digest is required")
	}
	if err := s.Resources.Validate(); err != nil {
		return fmt.Errorf("resources: %w", err)
	}
	if err := s.Security.Validate(); err != nil {
		return fmt.Errorf("security: %w", err)
	}
	if err := s.Network.Validate(); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	return nil
}

func immutableImageReference(reference string) bool {
	index := strings.LastIndex(reference, "sha256:")
	if index < 0 || (index > 0 && reference[index-1] != '@') {
		return false
	}
	digest := reference[index+len("sha256:"):]
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

type Instance struct {
	ProviderRef string
	// RuntimeID pins the physical instance for authenticated existing-runtime
	// operations. ProviderRef retains its logical lifecycle/name semantics.
	RuntimeID         string
	SlotID            string
	Epoch             uint64
	RuntimeGeneration uint64
	State             slot.State
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type Status struct {
	Instance
	Healthy     bool
	Reason      string
	ImageDigest string
}

// ExecutionProvider deliberately hides Docker-specific types from the
// orchestrator. Credentials and OAuth tokens must never be passed in SlotSpec;
// they are leased to an already running worker over the authenticated channel.
type ExecutionProvider interface {
	Create(ctx context.Context, spec SlotSpec) (Instance, error)
	Inspect(ctx context.Context, providerRef string) (Status, error)
	Start(ctx context.Context, providerRef string) error
	Drain(ctx context.Context, providerRef string, deadline time.Time) error
	Stop(ctx context.Context, providerRef string) error
	Destroy(ctx context.Context, providerRef string) error
}

// SlotInspector resolves the provider-owned runtime identity from the stable
// orchestrator slot id. Host-agents use this additive interface to recover
// after a process restart without persisting Docker container ids.
type SlotInspector interface {
	InspectSlot(ctx context.Context, slotID string) (Status, error)
}
