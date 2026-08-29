package worker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

const maxRememberedActivationLeases = 4096

var ErrActivationRejected = errors.New("worker credential activation rejected")

type CredentialBundleRecipient interface {
	PublicKey() (keyID string, publicKey []byte, err error)
	Open(ctx context.Context, encoded []byte, expected credential.TransportContext) ([]byte, error)
}

type OnboardingEngine interface {
	Onboard(ctx context.Context, input OnboardingInput) (OnboardingResult, error)
}

type CredentialCommitRequest struct {
	AccountBinding             string
	SlotID                     string
	ExecutionEpoch             uint64
	CredentialLeaseID          string
	ProxyLeaseID               string
	RotationRecipientKeyID     string
	RotationRecipientPublicKey []byte
	Result                     *OnboardingResult
}

func (r CredentialCommitRequest) String() string {
	authType := ""
	if r.Result != nil {
		authType = r.Result.AuthType
	}
	return fmt.Sprintf("CredentialCommitRequest{AccountBinding:%q SlotID:%q ExecutionEpoch:%d CredentialLeaseID:%q ProxyLeaseID:%q RotationRecipientKeyID:%q AuthType:%q Credential:[REDACTED] RotationRecipientPublicKey:[PUBLIC]}",
		r.AccountBinding, r.SlotID, r.ExecutionEpoch, r.CredentialLeaseID, r.ProxyLeaseID, r.RotationRecipientKeyID, authType)
}

func (r CredentialCommitRequest) GoString() string { return r.String() }

func (r CredentialCommitRequest) MarshalJSON() ([]byte, error) {
	authType := ""
	if r.Result != nil {
		authType = r.Result.AuthType
	}
	return json.Marshal(struct {
		AccountBinding    string `json:"account_binding"`
		SlotID            string `json:"slot_id"`
		ExecutionEpoch    uint64 `json:"execution_epoch"`
		CredentialLeaseID string `json:"credential_lease_id"`
		ProxyLeaseID      string `json:"proxy_lease_id"`
		RotationKeyID     string `json:"rotation_recipient_key_id"`
		AuthType          string `json:"auth_type"`
	}{r.AccountBinding, r.SlotID, r.ExecutionEpoch, r.CredentialLeaseID, r.ProxyLeaseID, r.RotationRecipientKeyID, authType})
}

// CredentialCommitter must send the normalized credential to the orchestrator
// over the encrypted rotation channel and return only after KMS/vault commit.
// It must not persist plaintext in the worker or host-agent.
type CredentialCommitter interface {
	CommitCredential(ctx context.Context, request CredentialCommitRequest) (versionID string, err error)
}

type SecureActivatorConfig struct {
	Identity  Identity
	Recipient CredentialBundleRecipient
	Onboarder OnboardingEngine
	Committer CredentialCommitter
}

type SecureActivator struct {
	identity  Identity
	recipient CredentialBundleRecipient
	onboarder OnboardingEngine
	committer CredentialCommitter

	operationMu sync.Mutex
	mu          sync.RWMutex
	draining    bool
	active      ActiveCredential
	pending     pendingActivation
	seenLeases  map[string]struct{}
}

type pendingActivation struct {
	CredentialLeaseID          string
	ProxyLeaseID               string
	ActivationBundleSHA256     [32]byte
	RotationRecipientKeyID     string
	RotationRecipientPublicKey []byte
	Result                     OnboardingResult
}

func (p *pendingActivation) Destroy() {
	if p == nil {
		return
	}
	p.Result.Destroy()
	zero(p.RotationRecipientPublicKey)
	p.RotationRecipientPublicKey = nil
	p.RotationRecipientKeyID = ""
	p.CredentialLeaseID = ""
	p.ProxyLeaseID = ""
	p.ActivationBundleSHA256 = [32]byte{}
}

type ActiveCredential struct {
	VersionID      string
	AuthType       string
	CredentialJSON []byte
}

func (c ActiveCredential) String() string {
	return fmt.Sprintf("ActiveCredential{VersionID:%q AuthType:%q CredentialJSON:[REDACTED]}", c.VersionID, c.AuthType)
}

func (c ActiveCredential) GoString() string { return c.String() }

func (c ActiveCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		VersionID string `json:"version_id"`
		AuthType  string `json:"auth_type"`
	}{c.VersionID, c.AuthType})
}

func (c *ActiveCredential) Destroy() {
	if c == nil {
		return
	}
	zero(c.CredentialJSON)
	c.CredentialJSON = nil
}

func NewSecureActivator(config SecureActivatorConfig) (*SecureActivator, error) {
	if config.Identity.Validate() != nil || config.Recipient == nil || config.Onboarder == nil {
		return nil, errors.New("secure worker activator configuration is invalid")
	}
	return &SecureActivator{
		identity: config.Identity, recipient: config.Recipient, onboarder: config.Onboarder, committer: config.Committer,
		seenLeases: make(map[string]struct{}),
	}, nil
}

func (a *SecureActivator) CredentialTransportKey() (string, []byte, error) {
	return a.recipient.PublicKey()
}

func (a *SecureActivator) Activate(ctx context.Context, activation Activation) ([]executionv1.ExecutionMode, error) {
	if a.committer == nil {
		return nil, ErrActivationRejected
	}
	return a.ActivateWithCommitter(ctx, activation, a.committer)
}

// ActivateWithCommitter binds one synchronous commit transport to one secure
// activation stream. Production uses this method so the worker cannot become
// ready until that exact stream returns the orchestrator Vault version id.
func (a *SecureActivator) ActivateWithCommitter(ctx context.Context, activation Activation, committer CredentialCommitter) ([]executionv1.ExecutionMode, error) {
	if committer == nil {
		return nil, ErrActivationRejected
	}
	if activation.CredentialLeaseID == "" || activation.ProxyLeaseID == "" || len(activation.EncryptedCredentialBundle) == 0 {
		return nil, ErrActivationRejected
	}
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	a.mu.RLock()
	_, replay := a.seenLeases[activation.CredentialLeaseID]
	draining := a.draining
	a.mu.RUnlock()
	if replay || draining {
		return nil, ErrActivationRejected
	}
	if len(a.seenLeases) >= maxRememberedActivationLeases {
		return nil, ErrActivationRejected
	}
	bundleDigest := sha256.Sum256(activation.EncryptedCredentialBundle)
	if a.pending.CredentialLeaseID != "" {
		if a.pending.CredentialLeaseID == activation.CredentialLeaseID {
			if a.pending.ProxyLeaseID != activation.ProxyLeaseID || a.pending.ActivationBundleSHA256 != bundleDigest {
				return nil, ErrActivationRejected
			}
		} else {
			a.pending.Destroy()
			a.pending = pendingActivation{}
		}
	}
	if a.pending.CredentialLeaseID == "" {
		binding := credential.TransportContext{
			AccountBinding: a.identity.AccountID, SlotID: a.identity.SlotID, ExecutionEpoch: a.identity.Epoch,
			LeaseID: activation.CredentialLeaseID, ProxyLeaseID: activation.ProxyLeaseID, Purpose: "onboarding",
		}
		plaintext, err := a.recipient.Open(ctx, activation.EncryptedCredentialBundle, binding)
		if err != nil {
			return nil, ErrActivationRejected
		}
		defer zero(plaintext)
		pkg, err := DecodeActivationPackage(plaintext)
		if err != nil {
			return nil, ErrActivationRejected
		}
		defer pkg.Destroy()
		result, err := a.onboarder.Onboard(ctx, pkg.Input)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrActivationRejected
		}
		a.pending = pendingActivation{
			CredentialLeaseID: activation.CredentialLeaseID, ProxyLeaseID: activation.ProxyLeaseID,
			ActivationBundleSHA256: bundleDigest, RotationRecipientKeyID: pkg.RotationRecipientKeyID,
			RotationRecipientPublicKey: append([]byte(nil), pkg.RotationRecipientPublicKey...), Result: result,
		}
	}
	versionID, err := committer.CommitCredential(ctx, CredentialCommitRequest{
		AccountBinding: a.identity.AccountID, SlotID: a.identity.SlotID, ExecutionEpoch: a.identity.Epoch,
		CredentialLeaseID: activation.CredentialLeaseID, ProxyLeaseID: activation.ProxyLeaseID,
		RotationRecipientKeyID: a.pending.RotationRecipientKeyID, RotationRecipientPublicKey: a.pending.RotationRecipientPublicKey,
		Result: &a.pending.Result,
	})
	if err != nil || !validCredentialVersionID(versionID) {
		return nil, ErrActivationRejected
	}
	newCredential := ActiveCredential{
		VersionID: versionID, AuthType: a.pending.Result.AuthType,
		CredentialJSON: append([]byte(nil), a.pending.Result.CredentialJSON...),
	}
	completedPending := a.pending
	a.pending = pendingActivation{}
	defer completedPending.Destroy()
	a.mu.Lock()
	previous := a.active
	a.active = newCredential
	a.seenLeases[activation.CredentialLeaseID] = struct{}{}
	a.mu.Unlock()
	previous.Destroy()
	return []executionv1.ExecutionMode{executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API}, nil
}

func (a *SecureActivator) ActiveCredential() (ActiveCredential, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.active.VersionID == "" || len(a.active.CredentialJSON) == 0 || a.draining {
		return ActiveCredential{}, ErrActivationRejected
	}
	return ActiveCredential{
		VersionID: a.active.VersionID, AuthType: a.active.AuthType,
		CredentialJSON: append([]byte(nil), a.active.CredentialJSON...),
	}, nil
}

func (a *SecureActivator) Ready() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return !a.draining && a.active.VersionID != "" && len(a.active.CredentialJSON) != 0
}

func (a *SecureActivator) Drain() {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	a.mu.Lock()
	a.draining = true
	active := a.active
	a.active = ActiveCredential{}
	a.mu.Unlock()
	active.Destroy()
	a.pending.Destroy()
	a.pending = pendingActivation{}
}

func (a *SecureActivator) ModeHealth(context.Context) []ModeHealth {
	a.mu.RLock()
	defer a.mu.RUnlock()
	reason := ""
	if a.draining {
		reason = "draining"
	} else if a.active.VersionID == "" {
		reason = "not_activated"
	}
	return []ModeHealth{
		{Mode: executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE, Healthy: false, ReasonCode: "not_implemented"},
		{Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, Healthy: reason == "", ReasonCode: reason},
	}
}

var _ Activator = (*SecureActivator)(nil)
var _ PerActivationCommitterActivator = (*SecureActivator)(nil)
var _ ModeHealthSource = (*SecureActivator)(nil)
