package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

type replayFencedRotationAuthorizer struct {
	mu        sync.Mutex
	accountID string
	err       error
	records   map[string]authorizedRotationRecord
	calls     int
	claim     RotationClaim
}

type authorizedRotationRecord struct {
	digest    [32]byte
	versionID string
}

func (a *replayFencedRotationAuthorizer) CommitAuthorizedRotation(ctx context.Context, claim RotationClaim, rotate func(context.Context, string) (string, error)) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claim = claim
	if a.err != nil {
		return "", a.err
	}
	if a.records == nil {
		a.records = make(map[string]authorizedRotationRecord)
	}
	if existing, ok := a.records[claim.CredentialLeaseID]; ok {
		if existing.digest != claim.MaterialSHA256 {
			return "", errors.New("credential lease replay payload mismatch")
		}
		return existing.versionID, nil
	}
	a.calls++
	versionID, err := rotate(ctx, a.accountID)
	if err != nil {
		return "", err
	}
	a.records[claim.CredentialLeaseID] = authorizedRotationRecord{digest: claim.MaterialSHA256, versionID: versionID}
	return versionID, nil
}

type recordingCredentialRotator struct {
	mu          sync.Mutex
	calls       int
	operationID string
	accountID   string
	authType    string
	hint        string
	plaintext   []byte
	err         error
}

type doubleInvokeRotationAuthorizer struct {
	accountID string
	secondErr error
}

type recordingResultProjectionRepository struct {
	mu     sync.Mutex
	calls  int
	commit onboarding.ResultProjectionCommit
	err    error
}

func (r *recordingResultProjectionRepository) ProjectProvisioningResult(_ context.Context, commit onboarding.ResultProjectionCommit) (onboarding.ProvisioningResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.commit = commit
	if r.err != nil {
		return onboarding.ProvisioningResult{}, r.err
	}
	return onboarding.ProvisioningResult{
		WorkflowID: "workflow-10380", IntentID: "intent-10380", AccountID: "account-10380", DesiredGeneration: 1,
		SlotID: commit.SlotID, ExecutionEpoch: commit.ExecutionEpoch,
		CredentialLeaseID: commit.CredentialLeaseID, CredentialVersionID: commit.CredentialVersionID,
		Projection: commit.Projection, CreatedAt: commit.CommittedAt,
	}, nil
}

func (r *recordingResultProjectionRepository) GetProvisioningResult(context.Context, string, string, uint64) (onboarding.ProvisioningOutcome, error) {
	return onboarding.ProvisioningOutcome{}, onboarding.ErrResultProjectionRejected
}

func (a *doubleInvokeRotationAuthorizer) CommitAuthorizedRotation(ctx context.Context, _ RotationClaim, rotate func(context.Context, string) (string, error)) (string, error) {
	versionID, err := rotate(ctx, a.accountID)
	if err != nil {
		return "", err
	}
	_, a.secondErr = rotate(ctx, a.accountID)
	return versionID, nil
}

func (r *recordingCredentialRotator) RotateIdempotent(_ context.Context, operationID, accountID, authType, hint string, plaintext []byte) (credential.VersionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.operationID, r.accountID, r.authType, r.hint = operationID, accountID, authType, hint
	r.plaintext = append([]byte(nil), plaintext...)
	if r.err != nil {
		return credential.VersionRecord{}, r.err
	}
	return credential.VersionRecord{
		ID: "11111111-2222-4333-8444-555555555555", AccountID: accountID, AuthType: authType,
	}, nil
}

func (r *recordingCredentialRotator) destroy() {
	r.mu.Lock()
	defer r.mu.Unlock()
	eraseRotationBytes(r.plaintext)
}

func TestCredentialRotationSinkAuthenticatesDecryptsAndFencesReplay(t *testing.T) {
	t.Parallel()
	accountID := "account-10380"
	recipient, request := sealedRotationRequest(t, accountID, "lease-1", "rotation-vault-secret")
	defer recipient.Destroy()
	authorizer := &replayFencedRotationAuthorizer{accountID: accountID}
	rotator := &recordingCredentialRotator{}
	defer rotator.destroy()
	sink, err := NewCredentialRotationSink(CredentialRotationSinkConfig{Recipient: recipient, Authorizer: authorizer, Vault: rotator})
	if err != nil {
		t.Fatal(err)
	}
	versionID, err := sink.CommitSealedCredential(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	_, reenvelopedRetry := sealRotationRequestWithRecipient(t, recipient, accountID, "lease-1", "rotation-vault-secret", 0x7e)
	if bytes.Equal(request.SealedCredentialBundle, reenvelopedRetry.SealedCredentialBundle) {
		t.Fatal("retry fixture did not use fresh transport encryption")
	}
	replayedVersionID, err := sink.CommitSealedCredential(context.Background(), reenvelopedRetry)
	if err != nil {
		t.Fatal(err)
	}
	if versionID != "11111111-2222-4333-8444-555555555555" || replayedVersionID != versionID {
		t.Fatalf("version ids = %q / %q", versionID, replayedVersionID)
	}
	rotator.mu.Lock()
	calls, operationID, rotatedAccount, authType, hint := rotator.calls, rotator.operationID, rotator.accountID, rotator.authType, rotator.hint
	plaintext := append([]byte(nil), rotator.plaintext...)
	rotator.mu.Unlock()
	defer eraseRotationBytes(plaintext)
	if calls != 1 || authorizer.calls != 1 || operationID != request.CredentialLeaseID || rotatedAccount != accountID || authType != worker.AuthTypeOAuth || hint != "oauth:***" ||
		!bytes.Contains(plaintext, []byte("rotation-vault-secret")) {
		t.Fatalf("rotation result calls=%d/%d account=%q auth=%q hint=%q plaintext=%q", calls, authorizer.calls, rotatedAccount, authType, hint, plaintext)
	}
	if bytes.Contains(request.SealedCredentialBundle, []byte("rotation-vault-secret")) {
		t.Fatal("orchestrator request contains plaintext credential")
	}
	for _, serialized := range []string{authorizer.claim.String(), fmt.Sprintf("%+v", authorizer.claim), string(mustRotationJSON(t, authorizer.claim))} {
		if strings.Contains(serialized, "rotation-vault-secret") {
			t.Fatalf("rotation claim serialization leaked secret: %s", serialized)
		}
	}
}

func TestCredentialRotationSinkFailsClosed(t *testing.T) {
	t.Parallel()
	accountID := "account-10380"
	for name, testCase := range map[string]struct {
		mutateRequest func(*worker.SealedCredentialCommitRequest)
		authorizerErr error
		rotatorErr    error
	}{
		"tampered ciphertext": {mutateRequest: func(request *worker.SealedCredentialCommitRequest) {
			request.CredentialLeaseID = "lease-tampered"
			request.SealedCredentialBundle[len(request.SealedCredentialBundle)-8] ^= 1
		}},
		"wrong account binding": {mutateRequest: func(request *worker.SealedCredentialCommitRequest) {
			request.AccountBinding = provider.RuntimeAccountID("other-account")
		}},
		"missing proxy lease": {mutateRequest: func(request *worker.SealedCredentialCommitRequest) { request.ProxyLeaseID = "" }},
		"authorizer rejects":  {authorizerErr: errors.New("lease database exposed rotation-vault-secret")},
		"vault rejects":       {rotatorErr: errors.New("kms exposed rotation-vault-secret")},
	} {
		t.Run(name, func(t *testing.T) {
			recipient, request := sealedRotationRequest(t, accountID, "lease-1", "rotation-vault-secret")
			defer recipient.Destroy()
			request.SealedCredentialBundle = append([]byte(nil), request.SealedCredentialBundle...)
			if testCase.mutateRequest != nil {
				testCase.mutateRequest(&request)
			}
			authorizer := &replayFencedRotationAuthorizer{accountID: accountID, err: testCase.authorizerErr}
			rotator := &recordingCredentialRotator{err: testCase.rotatorErr}
			defer rotator.destroy()
			sink, err := NewCredentialRotationSink(CredentialRotationSinkConfig{Recipient: recipient, Authorizer: authorizer, Vault: rotator})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sink.CommitSealedCredential(context.Background(), request); !errors.Is(err, ErrCredentialRotationRejected) || strings.Contains(err.Error(), "rotation-vault-secret") {
				t.Fatalf("failed rotation error = %v", err)
			}
		})
	}
}

func TestCredentialRotationSinkRejectsCredentialLeaseCiphertextSwap(t *testing.T) {
	t.Parallel()
	accountID := "account-10380"
	recipient, first := sealedRotationRequest(t, accountID, "lease-replay", "first-secret")
	defer recipient.Destroy()
	_, second := sealRotationRequestWithRecipient(t, recipient, accountID, "lease-replay", "second-secret", 0x7b)
	authorizer := &replayFencedRotationAuthorizer{accountID: accountID}
	rotator := &recordingCredentialRotator{}
	defer rotator.destroy()
	sink, _ := NewCredentialRotationSink(CredentialRotationSinkConfig{Recipient: recipient, Authorizer: authorizer, Vault: rotator})
	if _, err := sink.CommitSealedCredential(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := sink.CommitSealedCredential(context.Background(), second); !errors.Is(err, ErrCredentialRotationRejected) {
		t.Fatalf("ciphertext swap error = %v", err)
	}
	if rotator.calls != 1 {
		t.Fatalf("rotation calls after ciphertext swap = %d", rotator.calls)
	}
}

func TestCredentialRotationSinkInvokesVaultAtMostOncePerAuthorization(t *testing.T) {
	t.Parallel()
	accountID := "account-10380"
	recipient, request := sealedRotationRequest(t, accountID, "lease-double-callback", "rotation-vault-secret")
	defer recipient.Destroy()
	authorizer := &doubleInvokeRotationAuthorizer{accountID: accountID}
	rotator := &recordingCredentialRotator{}
	defer rotator.destroy()
	sink, _ := NewCredentialRotationSink(CredentialRotationSinkConfig{Recipient: recipient, Authorizer: authorizer, Vault: rotator})
	if _, err := sink.CommitSealedCredential(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(authorizer.secondErr, ErrCredentialRotationRejected) || rotator.calls != 1 {
		t.Fatalf("second rotate callback error/calls = %v/%d", authorizer.secondErr, rotator.calls)
	}
}

func TestCredentialRotationSinkProjectsSafeWorkerResultAfterVaultCommit(t *testing.T) {
	t.Parallel()
	accountID := "account-10380"
	recipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x6a}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	request := sealRotationPayloadWithRecipient(t, recipient, accountID, "lease-projection", []byte(`{
		"access_token":"oauth-projection-secret",
		"refresh_token":"refresh-projection-secret",
		"email_address":"Owner@Example.COM",
		"subscription_type":"max",
		"rate_limit_tier":"tier-1"
	}`), 0x6b)
	authorizer := &replayFencedRotationAuthorizer{accountID: accountID}
	rotator := &recordingCredentialRotator{}
	defer rotator.destroy()
	results := &recordingResultProjectionRepository{}
	now := time.Unix(2_000_000_000, 0).UTC()
	sink, err := NewCredentialRotationSink(CredentialRotationSinkConfig{
		Recipient: recipient, Authorizer: authorizer, Vault: rotator, ResultRepository: results,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sink.CommitSealedCredential(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	results.mu.Lock()
	calls, commit := results.calls, results.commit
	results.mu.Unlock()
	if calls != 1 || commit.Projection.EmailAddress != "owner@example.com" || commit.Projection.SubscriptionType != "max" ||
		commit.CredentialLeaseID != request.CredentialLeaseID || !commit.CommittedAt.Equal(now) {
		t.Fatalf("result projection calls=%d commit=%+v", calls, commit)
	}
	for _, serialized := range []string{commit.Projection.String(), fmt.Sprintf("%+v", commit.Projection), string(mustRotationJSON(t, commit.Projection))} {
		if strings.Contains(serialized, "projection-secret") {
			t.Fatalf("result projection leaked credential: %s", serialized)
		}
	}
}

func TestCredentialRotationSinkProjectionFailureDoesNotRotateAgain(t *testing.T) {
	t.Parallel()
	accountID := "account-10380"
	recipient, request := sealedRotationRequest(t, accountID, "lease-projection-failure", "rotation-vault-secret")
	defer recipient.Destroy()
	authorizer := &replayFencedRotationAuthorizer{accountID: accountID}
	rotator := &recordingCredentialRotator{}
	defer rotator.destroy()
	results := &recordingResultProjectionRepository{err: errors.New("projection database unavailable")}
	sink, err := NewCredentialRotationSink(CredentialRotationSinkConfig{
		Recipient: recipient, Authorizer: authorizer, Vault: rotator, ResultRepository: results,
	})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := sink.CommitSealedCredential(context.Background(), request); !errors.Is(err, ErrCredentialRotationRejected) {
			t.Fatalf("projection failure attempt %d error = %v", attempt, err)
		}
	}
	if rotator.calls != 1 || authorizer.calls != 1 || results.calls != 2 {
		t.Fatalf("projection retry calls vault=%d authorizer=%d projection=%d", rotator.calls, authorizer.calls, results.calls)
	}
}

func sealedRotationRequest(t *testing.T, accountID, leaseID, secret string) (*credential.Recipient, worker.SealedCredentialCommitRequest) {
	t.Helper()
	recipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x79}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	_, request := sealRotationRequestWithRecipient(t, recipient, accountID, leaseID, secret, 0x7a)
	return recipient, request
}

func sealRotationRequestWithRecipient(t *testing.T, recipient *credential.Recipient, accountID, leaseID, secret string, randomByte byte) (*credential.Recipient, worker.SealedCredentialCommitRequest) {
	t.Helper()
	request := sealRotationPayloadWithRecipient(t, recipient, accountID, leaseID, []byte(`{"access_token":"`+secret+`"}`), randomByte)
	return recipient, request
}

func sealRotationPayloadWithRecipient(t *testing.T, recipient *credential.Recipient, accountID, leaseID string, payload []byte, randomByte byte) worker.SealedCredentialCommitRequest {
	t.Helper()
	binding := credential.TransportContext{
		AccountBinding: provider.RuntimeAccountID(accountID), SlotID: "slot-10380", ExecutionEpoch: 9,
		LeaseID: leaseID, ProxyLeaseID: "proxy-lease-9", Purpose: "rotation",
	}
	material, err := credential.EncodeRotationMaterial(credential.RotationMaterial{
		AuthType: worker.AuthTypeOAuth, Plaintext: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eraseRotationBytes(material)
	keyID, publicKey, _ := recipient.PublicKey()
	sealed, err := credential.SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{randomByte}, 256)), keyID, publicKey, binding, material)
	if err != nil {
		t.Fatal(err)
	}
	return worker.SealedCredentialCommitRequest{
		AccountBinding: binding.AccountBinding, SlotID: binding.SlotID, ExecutionEpoch: binding.ExecutionEpoch,
		CredentialLeaseID: binding.LeaseID, ProxyLeaseID: binding.ProxyLeaseID, SealedCredentialBundle: sealed,
	}
}

func mustRotationJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
