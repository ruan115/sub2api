package service

import (
	"context"
	"errors"
	"testing"
	"time"
)

type recordingRotationAuthorizationRepository struct {
	accountID        string
	versionID        string
	beginErr         error
	completeErr      error
	beginCalls       int
	completeCalls    int
	completedLease   string
	completedVersion string
}

func (r *recordingRotationAuthorizationRepository) BeginCredentialRotation(
	context.Context,
	string,
	string,
	uint64,
	string,
	string,
	[32]byte,
	time.Time,
) (string, string, error) {
	r.beginCalls++
	return r.accountID, r.versionID, r.beginErr
}

func (r *recordingRotationAuthorizationRepository) CompleteCredentialRotation(
	_ context.Context,
	credentialLeaseID, versionID string,
	_ time.Time,
) error {
	r.completeCalls++
	r.completedLease = credentialLeaseID
	r.completedVersion = versionID
	return r.completeErr
}

type recordingProxyLeaseAuthority struct {
	calls int
	err   error
}

func (a *recordingProxyLeaseAuthority) ValidateCurrentProxyLease(
	context.Context,
	string,
	string,
	uint64,
	string,
	time.Time,
) error {
	a.calls++
	return a.err
}

func TestDurableRotationAuthorizerReservesValidatesAndCompletes(t *testing.T) {
	repository := &recordingRotationAuthorizationRepository{accountID: "account-10380"}
	proxies := &recordingProxyLeaseAuthority{}
	now := time.Unix(2_000_000_000, 0).UTC()
	authorizer, err := NewDurableRotationAuthorizer(repository, proxies, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claim := validDurableRotationClaim()
	rotateCalls := 0
	versionID, err := authorizer.CommitAuthorizedRotation(context.Background(), claim, func(_ context.Context, accountID string) (string, error) {
		rotateCalls++
		if accountID != repository.accountID {
			t.Fatalf("rotation account = %q", accountID)
		}
		return "11111111-2222-4333-8444-555555555555", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if versionID != repository.completedVersion || repository.completedLease != claim.CredentialLeaseID ||
		repository.beginCalls != 1 || repository.completeCalls != 1 || proxies.calls != 1 || rotateCalls != 1 {
		t.Fatalf("unexpected durable authorization calls: repo=%d/%d proxy=%d rotate=%d lease=%q version=%q",
			repository.beginCalls, repository.completeCalls, proxies.calls, rotateCalls,
			repository.completedLease, repository.completedVersion)
	}
}

func TestDurableRotationAuthorizerCommittedReplaySkipsProxyAndVault(t *testing.T) {
	repository := &recordingRotationAuthorizationRepository{
		accountID: "account-10380", versionID: "11111111-2222-4333-8444-555555555555",
	}
	proxies := &recordingProxyLeaseAuthority{err: errors.New("proxy reservation already expired")}
	authorizer, _ := NewDurableRotationAuthorizer(repository, proxies, time.Now)
	rotateCalls := 0
	versionID, err := authorizer.CommitAuthorizedRotation(context.Background(), validDurableRotationClaim(), func(context.Context, string) (string, error) {
		rotateCalls++
		return "", errors.New("must not be called")
	})
	if err != nil || versionID != repository.versionID {
		t.Fatalf("committed replay = %q, %v", versionID, err)
	}
	if proxies.calls != 0 || rotateCalls != 0 || repository.completeCalls != 0 {
		t.Fatalf("replay performed mutable work proxy=%d rotate=%d complete=%d", proxies.calls, rotateCalls, repository.completeCalls)
	}
}

func TestDurableRotationAuthorizerFailsClosedBeforeVault(t *testing.T) {
	claim := validDurableRotationClaim()
	for name, mutate := range map[string]func(*recordingRotationAuthorizationRepository, *recordingProxyLeaseAuthority, *RotationClaim){
		"repository": func(repository *recordingRotationAuthorizationRepository, _ *recordingProxyLeaseAuthority, _ *RotationClaim) {
			repository.beginErr = errors.New("database unavailable")
		},
		"proxy": func(_ *recordingRotationAuthorizationRepository, proxies *recordingProxyLeaseAuthority, _ *RotationClaim) {
			proxies.err = errors.New("proxy lease not current")
		},
		"zero digest": func(_ *recordingRotationAuthorizationRepository, _ *recordingProxyLeaseAuthority, claim *RotationClaim) {
			claim.MaterialSHA256 = [32]byte{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			repository := &recordingRotationAuthorizationRepository{accountID: "account-10380"}
			proxies := &recordingProxyLeaseAuthority{}
			candidate := claim
			mutate(repository, proxies, &candidate)
			authorizer, _ := NewDurableRotationAuthorizer(repository, proxies, time.Now)
			rotateCalls := 0
			if _, err := authorizer.CommitAuthorizedRotation(context.Background(), candidate, func(context.Context, string) (string, error) {
				rotateCalls++
				return "11111111-2222-4333-8444-555555555555", nil
			}); !errors.Is(err, ErrCredentialRotationRejected) {
				t.Fatalf("authorization error = %v", err)
			}
			if rotateCalls != 0 {
				t.Fatalf("rejected authorization invoked Vault %d times", rotateCalls)
			}
		})
	}
}

func validDurableRotationClaim() RotationClaim {
	return RotationClaim{
		AccountBinding: "runtime-account-binding", SlotID: "slot-10380", ExecutionEpoch: 19,
		CredentialLeaseID: "lease-10380", ProxyLeaseID: "proxy-10380", MaterialSHA256: [32]byte{1},
	}
}
