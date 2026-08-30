package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

// RotationAuthorizationRepository reserves the authenticated material digest
// before Vault is invoked and records the exact idempotent Vault version after
// it commits. Begin returns a non-empty versionID only for a completed replay.
type RotationAuthorizationRepository interface {
	BeginCredentialRotation(
		ctx context.Context,
		accountBinding, slotID string,
		executionEpoch uint64,
		credentialLeaseID, proxyLeaseID string,
		materialSHA256 [32]byte,
		authorizedAt time.Time,
	) (accountID, versionID string, err error)
	CompleteCredentialRotation(ctx context.Context, credentialLeaseID, versionID string, committedAt time.Time) error
}

// ProxyLeaseAuthority is deliberately independent from the onboarding
// workflow repository. A workflow's proxy_lease_id proves immutable binding,
// but only this authority can prove that the external fixed-proxy reservation
// is still current at authorization time.
type ProxyLeaseAuthority interface {
	ValidateCurrentProxyLease(
		ctx context.Context,
		accountBinding, slotID string,
		executionEpoch uint64,
		proxyLeaseID string,
		checkedAt time.Time,
	) error
}

type DurableRotationAuthorizer struct {
	repository RotationAuthorizationRepository
	proxies    ProxyLeaseAuthority
	now        func() time.Time
}

func NewDurableRotationAuthorizer(
	repository RotationAuthorizationRepository,
	proxies ProxyLeaseAuthority,
	now func() time.Time,
) (*DurableRotationAuthorizer, error) {
	if repository == nil || proxies == nil {
		return nil, errors.New("rotation repository and proxy lease authority are required")
	}
	if now == nil {
		now = time.Now
	}
	return &DurableRotationAuthorizer{repository: repository, proxies: proxies, now: now}, nil
}

func (a *DurableRotationAuthorizer) CommitAuthorizedRotation(
	ctx context.Context,
	claim RotationClaim,
	rotate func(context.Context, string) (string, error),
) (string, error) {
	if a == nil || a.repository == nil || a.proxies == nil || ctx == nil || ctx.Err() != nil || rotate == nil ||
		validateRotationClaim(claim) != nil {
		return "", ErrCredentialRotationRejected
	}
	authorizedAt := a.now().UTC()
	if authorizedAt.IsZero() {
		return "", ErrCredentialRotationRejected
	}
	accountID, versionID, err := a.repository.BeginCredentialRotation(
		ctx, claim.AccountBinding, claim.SlotID, claim.ExecutionEpoch,
		claim.CredentialLeaseID, claim.ProxyLeaseID, claim.MaterialSHA256, authorizedAt,
	)
	if err != nil || credential.ValidateTransportID(accountID) != nil {
		return "", ErrCredentialRotationRejected
	}
	if versionID != "" {
		if credential.ValidateTransportID(versionID) != nil {
			return "", ErrCredentialRotationRejected
		}
		return versionID, nil
	}
	if err := a.proxies.ValidateCurrentProxyLease(
		ctx, claim.AccountBinding, claim.SlotID, claim.ExecutionEpoch, claim.ProxyLeaseID, authorizedAt,
	); err != nil {
		return "", ErrCredentialRotationRejected
	}
	versionID, err = rotate(ctx, accountID)
	if err != nil || credential.ValidateTransportID(versionID) != nil {
		return "", ErrCredentialRotationRejected
	}
	if err := a.repository.CompleteCredentialRotation(ctx, claim.CredentialLeaseID, versionID, a.now().UTC()); err != nil {
		return "", ErrCredentialRotationRejected
	}
	return versionID, nil
}

func validateRotationClaim(claim RotationClaim) error {
	for _, value := range []string{
		claim.AccountBinding, claim.SlotID, claim.CredentialLeaseID, claim.ProxyLeaseID,
	} {
		if credential.ValidateTransportID(value) != nil {
			return ErrCredentialRotationRejected
		}
	}
	if claim.ExecutionEpoch == 0 || claim.MaterialSHA256 == ([32]byte{}) {
		return ErrCredentialRotationRejected
	}
	return nil
}

var _ RotationCommitAuthorizer = (*DurableRotationAuthorizer)(nil)
