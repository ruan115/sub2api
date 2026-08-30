package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

var ErrCredentialRotationRejected = errors.New("orchestrator credential rotation rejected")

// RotationClaim is the durable replay/fencing identity for one worker return
// path. MaterialSHA256 is computed from the authenticated canonical rotation
// frame, so retrying after a lost acknowledgement remains idempotent even
// though transport encryption uses a fresh ephemeral key and nonce.
type RotationClaim struct {
	AccountBinding    string
	SlotID            string
	ExecutionEpoch    uint64
	CredentialLeaseID string
	ProxyLeaseID      string
	MaterialSHA256    [32]byte
}

// RotationCommitAuthorizer is the transaction boundary owned by the
// orchestrator. Implementations must validate the current slot/account/epoch,
// execution lease and proxy lease, and execute rotate at most once for the
// credential lease + material digest. A successful replay returns the recorded
// version id without invoking rotate again.
type RotationCommitAuthorizer interface {
	CommitAuthorizedRotation(ctx context.Context, claim RotationClaim, rotate func(ctx context.Context, accountID string) (versionID string, err error)) (versionID string, err error)
}

type CredentialRotator interface {
	RotateIdempotent(ctx context.Context, operationID, accountID, authType, hint string, plaintext []byte) (credential.VersionRecord, error)
}

type CredentialRotationSinkConfig struct {
	Recipient        *credential.Recipient
	Authorizer       RotationCommitAuthorizer
	Vault            CredentialRotator
	ResultRepository onboarding.ResultProjectionRepository
	Now              func() time.Time
}

// CredentialRotationSink is the orchestrator-only endpoint of the encrypted
// worker return path. Neither workers nor host-agents receive KMS permissions
// or the source account id.
type CredentialRotationSink struct {
	recipient  *credential.Recipient
	authorizer RotationCommitAuthorizer
	vault      CredentialRotator
	results    onboarding.ResultProjectionRepository
	now        func() time.Time
}

func NewCredentialRotationSink(config CredentialRotationSinkConfig) (*CredentialRotationSink, error) {
	if config.Recipient == nil || config.Authorizer == nil || config.Vault == nil {
		return nil, errors.New("credential rotation recipient, authorizer and vault are required")
	}
	if _, _, err := config.Recipient.PublicKey(); err != nil {
		return nil, errors.New("credential rotation recipient is unavailable")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &CredentialRotationSink{
		recipient: config.Recipient, authorizer: config.Authorizer, vault: config.Vault,
		results: config.ResultRepository, now: config.Now,
	}, nil
}

func (s *CredentialRotationSink) CommitSealedCredential(ctx context.Context, request worker.SealedCredentialCommitRequest) (string, error) {
	if s == nil || s.recipient == nil || s.authorizer == nil || s.vault == nil || ctx == nil || ctx.Err() != nil ||
		len(request.SealedCredentialBundle) == 0 {
		return "", ErrCredentialRotationRejected
	}
	binding := credential.TransportContext{
		AccountBinding: request.AccountBinding, SlotID: request.SlotID, ExecutionEpoch: request.ExecutionEpoch,
		LeaseID: request.CredentialLeaseID, ProxyLeaseID: request.ProxyLeaseID, Purpose: "rotation",
	}
	if binding.Validate() != nil {
		return "", ErrCredentialRotationRejected
	}
	plaintext, err := s.recipient.Open(ctx, request.SealedCredentialBundle, binding)
	if err != nil {
		return "", ErrCredentialRotationRejected
	}
	defer eraseRotationBytes(plaintext)
	materialDigest := sha256.Sum256(plaintext)
	material, err := credential.DecodeRotationMaterial(plaintext)
	if err != nil {
		return "", ErrCredentialRotationRejected
	}
	defer material.Destroy()
	claim := RotationClaim{
		AccountBinding: request.AccountBinding, SlotID: request.SlotID, ExecutionEpoch: request.ExecutionEpoch,
		CredentialLeaseID: request.CredentialLeaseID, ProxyLeaseID: request.ProxyLeaseID,
		MaterialSHA256: materialDigest,
	}
	var rotateMu sync.Mutex
	rotateCalled := false
	versionID, err := s.authorizer.CommitAuthorizedRotation(ctx, claim, func(callbackContext context.Context, accountID string) (string, error) {
		rotateMu.Lock()
		if rotateCalled {
			rotateMu.Unlock()
			return "", ErrCredentialRotationRejected
		}
		rotateCalled = true
		rotateMu.Unlock()
		if callbackContext == nil || callbackContext.Err() != nil || provider.RuntimeAccountID(accountID) != request.AccountBinding {
			return "", ErrCredentialRotationRejected
		}
		record, err := s.vault.RotateIdempotent(
			callbackContext, claim.CredentialLeaseID, accountID, material.AuthType, material.AuthType+":***", material.Plaintext,
		)
		if err != nil || credential.ValidateTransportID(record.ID) != nil || record.AccountID != accountID || record.AuthType != material.AuthType {
			return "", ErrCredentialRotationRejected
		}
		return record.ID, nil
	})
	if err != nil || credential.ValidateTransportID(versionID) != nil {
		return "", ErrCredentialRotationRejected
	}
	if s.results != nil {
		projection, err := worker.ProjectCredential(material.AuthType, material.Plaintext)
		if err != nil {
			return "", ErrCredentialRotationRejected
		}
		_, err = s.results.ProjectProvisioningResult(ctx, onboarding.ResultProjectionCommit{
			AccountBinding: request.AccountBinding, SlotID: request.SlotID, ExecutionEpoch: request.ExecutionEpoch,
			CredentialLeaseID: request.CredentialLeaseID, ProxyLeaseID: request.ProxyLeaseID,
			CredentialVersionID: versionID,
			Projection: onboarding.ResultProjection{
				AuthType: projection.AuthType, ExpiresAt: projection.ExpiresAt, EmailAddress: projection.EmailAddress,
				OrganizationID: projection.OrganizationID, UpstreamAccountID: projection.UpstreamAccountID,
				Scope: projection.Scope, SubscriptionType: projection.SubscriptionType, RateLimitTier: projection.RateLimitTier,
			},
			CommittedAt: s.now().UTC(),
		})
		if err != nil {
			return "", ErrCredentialRotationRejected
		}
	}
	return versionID, nil
}

func (c RotationClaim) String() string {
	return fmt.Sprintf("RotationClaim{AccountBinding:%q SlotID:%q ExecutionEpoch:%d CredentialLeaseID:%q ProxyLeaseID:%q MaterialSHA256:[DIGEST]}",
		c.AccountBinding, c.SlotID, c.ExecutionEpoch, c.CredentialLeaseID, c.ProxyLeaseID)
}

func (c RotationClaim) GoString() string { return c.String() }

func (c RotationClaim) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AccountBinding    string `json:"account_binding"`
		SlotID            string `json:"slot_id"`
		ExecutionEpoch    uint64 `json:"execution_epoch"`
		CredentialLeaseID string `json:"credential_lease_id"`
		ProxyLeaseID      string `json:"proxy_lease_id"`
	}{c.AccountBinding, c.SlotID, c.ExecutionEpoch, c.CredentialLeaseID, c.ProxyLeaseID})
}

func eraseRotationBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

var _ worker.SealedCredentialSink = (*CredentialRotationSink)(nil)
var _ CredentialRotator = (*credential.Vault)(nil)
