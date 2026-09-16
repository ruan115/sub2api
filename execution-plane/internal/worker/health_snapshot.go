package worker

import (
	"context"
	"regexp"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

var healthChallengePattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

// LoadedState is metadata, not credential material or execution authority.
// ActivationRevision belongs to this worker instance, not the vault sequence.
type LoadedState struct {
	Identity            Identity
	CredentialVersionID string
	AuthType            string
	ProxyLeaseID        string
	ActivationRevision  uint64
}

// HealthSnapshot binds modes and loaded metadata to one atomic observation.
// Neither field may be filled by a second read of an independently changing
// source. Returned snapshots do not alias the activator's mutable state.
type HealthSnapshot struct {
	Modes       []ModeHealth
	LoadedState *LoadedState
}

type HealthSnapshotSource interface {
	HealthSnapshot(context.Context) HealthSnapshot
}

func (a *SecureActivator) HealthSnapshot(context.Context) HealthSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()
	snapshot := HealthSnapshot{Modes: a.modeHealthLocked()}
	if a.readyLocked() {
		snapshot.LoadedState = &LoadedState{
			Identity: a.identity, CredentialVersionID: a.active.VersionID, AuthType: a.active.AuthType,
			ProxyLeaseID: a.active.ProxyLeaseID, ActivationRevision: a.activationRevision,
		}
	}
	return snapshot
}

// Called with a.mu held. This readiness condition is shared with Ready and
// ModeHealth, including the requirement for actual in-memory credential bytes.
func (a *SecureActivator) readyLocked() bool {
	return !a.draining && a.active.VersionID != "" && len(a.active.CredentialJSON) != 0
}

func (a *SecureActivator) modeHealthLocked() []ModeHealth {
	reason := ""
	if a.draining {
		reason = "draining"
	} else if !a.readyLocked() {
		reason = "not_activated"
	}
	return []ModeHealth{
		{Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE, Healthy: false, ReasonCode: "not_implemented"},
		{Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, Healthy: reason == "", ReasonCode: reason},
	}
}

func validLoadedState(state *LoadedState, identity Identity) bool {
	if state == nil || state.Identity != identity || state.ActivationRevision == 0 ||
		!validCredentialVersionID(state.CredentialVersionID) || credential.ValidateTransportID(state.ProxyLeaseID) != nil {
		return false
	}
	return validLoadedAuthType(state.AuthType)
}

func validLoadedAuthType(authType string) bool {
	return authType == AuthTypeOAuth || authType == AuthTypeSetupToken || authType == AuthTypeAPIKey
}

var _ HealthSnapshotSource = (*SecureActivator)(nil)
