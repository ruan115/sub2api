// Package workerproof checks one worker's loaded state against current control
// plane metadata. A receipt is not a ticket, route, cached serving permission,
// or lease renewal. The caller must separately authenticate business requests.
package workerproof

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/dataplane"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"google.golang.org/protobuf/proto"
)

var (
	ErrConfiguration = errors.New("worker proof configuration is invalid")
	ErrUnavailable   = errors.New("worker loaded-state proof is unavailable")
	sessionPattern   = regexp.MustCompile(`^[a-f0-9]{32}$`)
	imagePattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Sessions interface {
	ValidateControlSession(context.Context, string, string) error
}

type Leases interface {
	Validate(context.Context, lease.Claim) error
}

// HealthReader must resolve only the existing provider reference bound to this
// exact authority and use a separately authorized health-only ticket. It must
// honor context and may not provision a runtime or accept an arbitrary URL.
// Production authentication/connection assembly is a later acceptance gate.
type HealthReader interface {
	ReadHealth(context.Context, store.WorkerReadinessBinding, string) (*executionv1.HealthResponse, error)
}

type Config struct {
	Repository store.WorkerReadinessBindingRepository
	Sessions   Sessions
	Leases     Leases
	Health     HealthReader
	MaxAge     time.Duration
	Timeout    time.Duration
	Now        func() time.Time
}

type Verifier struct{ config Config }

// Receipt records a single successful comparison. CheckedAt is frozen before
// the worker call, never advanced by later repository/session I/O. No Ready or
// reusable-validity flag is intentionally provided.
type Receipt struct {
	Authority          store.WorkerReadinessBinding
	Mode               executionv1.ExecutionMode
	ActivationRevision uint64
	CheckedAt          time.Time
}

func New(config Config) (*Verifier, error) {
	if config.Repository == nil || config.Sessions == nil || config.Leases == nil || config.Health == nil {
		return nil, ErrConfiguration
	}
	if config.MaxAge == 0 {
		config.MaxAge = 45 * time.Second
	}
	if config.Timeout == 0 {
		config.Timeout = 2 * time.Second
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.MaxAge <= 0 || config.MaxAge > 45*time.Second || config.Timeout <= 0 || config.Timeout > 10*time.Second {
		return nil, ErrConfiguration
	}
	return &Verifier{config: config}, nil
}

func (v *Verifier) Check(parent context.Context, want dataplane.Binding, mode executionv1.ExecutionMode) (Receipt, error) {
	if v == nil || parent == nil || parent.Err() != nil || v.config.Repository == nil || v.config.Now == nil || !validRequest(want, mode) {
		return Receipt{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(parent, v.config.Timeout)
	defer cancel()
	before, err := v.config.Repository.ReadWorkerReadinessBinding(ctx, want.SlotID, v.config.Now().UTC(), v.config.MaxAge)
	if err != nil || ctx.Err() != nil || !matchesRequest(before, want) || !v.current(before, v.config.Now().UTC()) {
		return Receipt{}, ErrUnavailable
	}
	claim := lease.Claim{SlotID: before.SlotID, NodeID: before.NodeID, ExecutionEpoch: before.ExecutionEpoch, OwnerID: before.LeaseOwnerID}
	if v.config.Sessions.ValidateControlSession(ctx, before.NodeID, before.ControlSessionID) != nil || ctx.Err() != nil ||
		v.config.Leases.Validate(ctx, claim) != nil || ctx.Err() != nil {
		return Receipt{}, ErrUnavailable
	}
	started := v.config.Now().UTC()
	if !v.current(before, started) {
		return Receipt{}, ErrUnavailable
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Receipt{}, ErrUnavailable
	}
	challenge := hex.EncodeToString(nonce[:])
	// Expiry of the authority used to start this request cannot be extended by
	// a later heartbeat or lease renewal while the health RPC is in flight.
	deadline := minTime(before.LeaseExpiresAt, before.ObservedAt.Add(v.config.MaxAge), before.NodeSeenAt.Add(v.config.MaxAge))
	healthCtx, cancelHealth := context.WithDeadline(ctx, deadline)
	defer cancelHealth()
	response, err := v.config.Health.ReadHealth(healthCtx, before, challenge)
	if err != nil || healthCtx.Err() != nil || !matchesHealth(response, before, mode, challenge) || !v.current(before, v.config.Now().UTC()) {
		return Receipt{}, ErrUnavailable
	}
	// Do not retain the response object across later dependency calls.
	revision := response.GetLoadedState().GetActivationRevision()
	after, err := v.config.Repository.ReadWorkerReadinessBinding(healthCtx, want.SlotID, v.config.Now().UTC(), v.config.MaxAge)
	if err != nil || healthCtx.Err() != nil || !sameAuthority(before, after) || !v.current(after, v.config.Now().UTC()) {
		return Receipt{}, ErrUnavailable
	}
	if v.config.Leases.Validate(healthCtx, claim) != nil || healthCtx.Err() != nil ||
		v.config.Sessions.ValidateControlSession(healthCtx, after.NodeID, after.ControlSessionID) != nil || healthCtx.Err() != nil {
		return Receipt{}, ErrUnavailable
	}
	now := v.config.Now().UTC()
	if now.Before(started) || !started.Add(v.config.Timeout).After(now) || !v.current(before, now) || !v.current(after, now) {
		return Receipt{}, ErrUnavailable
	}
	return Receipt{Authority: after, Mode: mode, ActivationRevision: revision, CheckedAt: started}, nil
}

func validRequest(b dataplane.Binding, mode executionv1.ExecutionMode) bool {
	for _, id := range []string{b.AccountID, b.SlotID, b.NodeID} {
		if credential.ValidateTransportID(id) != nil {
			return false
		}
	}
	return b.ExecutionEpoch > 0 && b.RouteGeneration > 0 && validMode(mode)
}

func validMode(mode executionv1.ExecutionMode) bool {
	return mode == executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API || mode == executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE
}

func matchesRequest(b store.WorkerReadinessBinding, want dataplane.Binding) bool {
	return b.AccountID == want.AccountID && b.SlotID == want.SlotID && b.NodeID == want.NodeID && b.ExecutionEpoch == want.ExecutionEpoch && b.RouteGeneration == want.RouteGeneration
}

func (v *Verifier) current(b store.WorkerReadinessBinding, now time.Time) bool {
	for _, id := range []string{b.AccountID, b.SlotID, b.NodeID, b.LeaseOwnerID, b.CredentialVersionID, b.ProxyLeaseID} {
		if credential.ValidateTransportID(id) != nil {
			return false
		}
	}
	if store.ValidateProxyReservationOpaqueID(b.ProxyReservationID) != nil || store.ValidateProxyBindingID(b.ProxyBindingID) != nil {
		return false
	}
	switch b.AuthType {
	case "oauth", "setup_token", "api_key":
	default:
		return false
	}
	return !now.IsZero() && sessionPattern.MatchString(b.ControlSessionID) && imagePattern.MatchString(b.ImageDigest) &&
		b.ExecutionEpoch > 0 && b.RouteGeneration > 0 && b.CredentialVersionNumber > 0 && b.ProxyBindingRevision > 0 &&
		b.ProviderRef != "" && len(b.ProviderRef) <= 255 && !b.ObservedAt.IsZero() && !b.ObservedAt.After(now) && b.ObservedAt.Add(v.config.MaxAge).After(now) &&
		!b.NodeSeenAt.IsZero() && !b.NodeSeenAt.After(now) && b.NodeSeenAt.Add(v.config.MaxAge).After(now) && b.LeaseExpiresAt.After(now)
}

func matchesHealth(response *executionv1.HealthResponse, b store.WorkerReadinessBinding, mode executionv1.ExecutionMode, challenge string) bool {
	if response == nil || len(response.GetModes()) < 1 || len(response.GetModes()) > 2 || proto.Size(response) > 4096 ||
		response.GetChallenge() != challenge || response.GetSlotId() != b.SlotID || response.GetExecutionEpoch() != b.ExecutionEpoch || response.GetImageDigest() != b.ImageDigest {
		return false
	}
	loaded := response.GetLoadedState()
	if loaded == nil || loaded.GetAccountBinding() != provider.RuntimeAccountID(b.AccountID) || loaded.GetNodeId() != b.NodeID ||
		loaded.GetCredentialVersionId() != b.CredentialVersionID || loaded.GetAuthType() != b.AuthType || loaded.GetProxyLeaseId() != b.ProxyLeaseID || loaded.GetActivationRevision() == 0 {
		return false
	}
	seen := map[executionv1.ExecutionMode]bool{}
	ready := false
	for _, health := range response.GetModes() {
		if health == nil || !validMode(health.GetMode()) || seen[health.GetMode()] || len(health.GetReasonCode()) > 64 || len(health.GetReasonMessage()) > 256 {
			return false
		}
		seen[health.GetMode()] = true
		if health.GetHealthy() && (health.GetReasonCode() != "" || health.GetReasonMessage() != "") {
			return false
		}
		if health.GetMode() == mode {
			ready = health.GetHealthy()
		}
	}
	return ready
}

func sameAuthority(a, b store.WorkerReadinessBinding) bool {
	return a.AccountID == b.AccountID && a.SlotID == b.SlotID && a.NodeID == b.NodeID && a.ExecutionEpoch == b.ExecutionEpoch && a.RouteGeneration == b.RouteGeneration &&
		a.ProviderRef == b.ProviderRef && a.LeaseOwnerID == b.LeaseOwnerID && a.ControlSessionID == b.ControlSessionID && a.ImageDigest == b.ImageDigest &&
		a.CredentialVersionID == b.CredentialVersionID && a.CredentialVersionNumber == b.CredentialVersionNumber && a.AuthType == b.AuthType &&
		a.ProxyLeaseID == b.ProxyLeaseID && a.ProxyReservationID == b.ProxyReservationID && a.ProxyBindingID == b.ProxyBindingID && a.ProxyBindingRevision == b.ProxyBindingRevision
}

func minTime(values ...time.Time) time.Time {
	result := values[0]
	for _, value := range values[1:] {
		if value.Before(result) {
			result = value
		}
	}
	return result
}
