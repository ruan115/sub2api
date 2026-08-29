package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

type fakeOnboardingEngine struct {
	calls atomic.Int64
}

func (e *fakeOnboardingEngine) Onboard(_ context.Context, input OnboardingInput) (OnboardingResult, error) {
	e.calls.Add(1)
	credentialJSON, _ := json.Marshal(map[string]string{"access_token": "normalized-" + string(input.Secret)})
	return OnboardingResult{AuthType: input.AuthType, CredentialJSON: credentialJSON, EmailAddress: "owner@example.com"}, nil
}

type recordingCredentialCommitter struct {
	mu      sync.Mutex
	request CredentialCommitRequest
	err     error
	version string
}

type failOnceCredentialCommitter struct {
	mu    sync.Mutex
	calls int
}

func (c *failOnceCredentialCommitter) CommitCredential(_ context.Context, _ CredentialCommitRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls == 1 {
		return "", errors.New("commit acknowledgement lost")
	}
	return "credential-version-recovered", nil
}

func (c *recordingCredentialCommitter) CommitCredential(_ context.Context, request CredentialCommitRequest) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.request = request
	c.request.RotationRecipientPublicKey = append([]byte(nil), request.RotationRecipientPublicKey...)
	if request.Result != nil {
		copied := *request.Result
		copied.CredentialJSON = append([]byte(nil), request.Result.CredentialJSON...)
		c.request.Result = &copied
	}
	if c.err != nil {
		return "", c.err
	}
	if c.version != "" {
		return c.version, nil
	}
	return "credential-version-2", nil
}

func (c *recordingCredentialCommitter) destroy() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.request.Result != nil {
		c.request.Result.Destroy()
	}
}

func TestSecureActivatorDecryptsOnboardsCommitsThenActivates(t *testing.T) {
	t.Parallel()
	identity := Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", NodeID: "srv74", Epoch: 3}
	recipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x33}, 32)))
	defer recipient.Destroy()
	onboarder := &fakeOnboardingEngine{}
	committer := &recordingCredentialCommitter{}
	defer committer.destroy()
	activator, err := NewSecureActivator(SecureActivatorConfig{Identity: identity, Recipient: recipient, Onboarder: onboarder, Committer: committer})
	if err != nil {
		t.Fatal(err)
	}
	activation := sealedTestActivation(t, recipient, identity, "credential-lease-1", "proxy-lease-1", []byte("source-secret"))
	modes, err := activator.Activate(context.Background(), activation)
	if err != nil {
		t.Fatal(err)
	}
	if len(modes) != 1 || onboarder.calls.Load() != 1 {
		t.Fatalf("activation modes/calls = %v/%d", modes, onboarder.calls.Load())
	}
	active, err := activator.ActiveCredential()
	if err != nil {
		t.Fatal(err)
	}
	defer active.Destroy()
	if active.VersionID != "credential-version-2" || active.AuthType != AuthTypeOAuth || !bytes.Contains(active.CredentialJSON, []byte("normalized-source-secret")) {
		t.Fatalf("unexpected active credential: %+v", active)
	}
	committer.mu.Lock()
	commit := committer.request
	committer.mu.Unlock()
	if commit.CredentialLeaseID != "credential-lease-1" || commit.ProxyLeaseID != "proxy-lease-1" ||
		commit.RotationRecipientKeyID == "" || len(commit.RotationRecipientPublicKey) != 32 || commit.Result == nil {
		t.Fatalf("unexpected credential commit: %+v", commit)
	}
	for _, serialized := range []string{active.String(), fmt.Sprintf("%+v", active), string(mustJSON(t, active)), commit.String(), string(mustJSON(t, commit))} {
		for _, secret := range []string{"source-secret", "normalized-source-secret"} {
			if strings.Contains(serialized, secret) {
				t.Fatalf("secure activation serialization leaked %q: %s", secret, serialized)
			}
		}
	}
	if _, err := activator.Activate(context.Background(), activation); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("activation replay error = %v", err)
	}
	activator.Drain()
	if _, err := activator.ActiveCredential(); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("drained active credential error = %v", err)
	}
}

func TestSecureActivatorFailsClosedOnBindingOrCommitFailure(t *testing.T) {
	t.Parallel()
	identity := Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", NodeID: "srv74", Epoch: 3}
	recipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x34}, 32)))
	defer recipient.Destroy()
	onboarder := &fakeOnboardingEngine{}
	committer := &recordingCredentialCommitter{err: errors.New("vault unavailable with normalized-source-secret")}
	defer committer.destroy()
	activator, _ := NewSecureActivator(SecureActivatorConfig{Identity: identity, Recipient: recipient, Onboarder: onboarder, Committer: committer})
	wrongBinding := sealedTestActivation(t, recipient, Identity{AccountID: identity.AccountID, SlotID: identity.SlotID, NodeID: identity.NodeID, Epoch: 4}, "lease-wrong", "proxy", []byte("secret"))
	if _, err := activator.Activate(context.Background(), wrongBinding); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("wrong epoch activation error = %v", err)
	}
	activation := sealedTestActivation(t, recipient, identity, "lease-commit", "proxy", []byte("source-secret"))
	if _, err := activator.Activate(context.Background(), activation); !errors.Is(err, ErrActivationRejected) || strings.Contains(err.Error(), "source-secret") {
		t.Fatalf("commit failure activation error = %v", err)
	}
	if _, err := activator.ActiveCredential(); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("failed commit activated credential: %v", err)
	}
}

func TestSecureActivatorRejectsNonCanonicalVersionID(t *testing.T) {
	t.Parallel()
	identity := Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", NodeID: "srv74", Epoch: 3}
	recipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x38}, 32)))
	defer recipient.Destroy()
	committer := &recordingCredentialCommitter{version: "version-\u0085invalid"}
	defer committer.destroy()
	activator, _ := NewSecureActivator(SecureActivatorConfig{
		Identity: identity, Recipient: recipient, Onboarder: &fakeOnboardingEngine{}, Committer: committer,
	})
	defer activator.Drain()
	activation := sealedTestActivation(t, recipient, identity, "lease-invalid-version", "proxy", []byte("source-secret"))
	if _, err := activator.Activate(context.Background(), activation); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("invalid version id activation error = %v", err)
	}
	if _, err := activator.ActiveCredential(); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("invalid version id activated credential: %v", err)
	}
}

func TestSecureActivatorConcurrentReplayAllowsOneCommit(t *testing.T) {
	identity := Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", NodeID: "srv74", Epoch: 3}
	recipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x35}, 32)))
	defer recipient.Destroy()
	onboarder := &fakeOnboardingEngine{}
	committer := &recordingCredentialCommitter{}
	defer committer.destroy()
	activator, _ := NewSecureActivator(SecureActivatorConfig{Identity: identity, Recipient: recipient, Onboarder: onboarder, Committer: committer})
	activation := sealedTestActivation(t, recipient, identity, "lease-race", "proxy", []byte("source-secret"))
	var successes atomic.Int64
	var wait sync.WaitGroup
	for index := 0; index < 32; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := activator.Activate(context.Background(), activation); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrActivationRejected) {
				t.Errorf("unexpected concurrent activation error: %v", err)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 || onboarder.calls.Load() != 1 {
		t.Fatalf("activation successes/onboarding calls = %d/%d", successes.Load(), onboarder.calls.Load())
	}
}

func TestSecureActivatorRetriesPendingCommitWithoutRepeatingOnboarding(t *testing.T) {
	t.Parallel()
	identity := Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", NodeID: "srv74", Epoch: 3}
	recipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x36}, 32)))
	defer recipient.Destroy()
	onboarder := &fakeOnboardingEngine{}
	committer := &failOnceCredentialCommitter{}
	activator, _ := NewSecureActivator(SecureActivatorConfig{Identity: identity, Recipient: recipient, Onboarder: onboarder, Committer: committer})
	defer activator.Drain()
	activation := sealedTestActivation(t, recipient, identity, "lease-retry", "proxy-retry", []byte("retry-source-secret"))
	if _, err := activator.Activate(context.Background(), activation); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("first commit attempt error = %v", err)
	}
	if onboarder.calls.Load() != 1 {
		t.Fatalf("first onboarding calls = %d", onboarder.calls.Load())
	}
	if _, err := activator.Activate(context.Background(), activation); err != nil {
		t.Fatal(err)
	}
	if onboarder.calls.Load() != 1 || committer.calls != 2 {
		t.Fatalf("recovery onboarding/commit calls = %d/%d", onboarder.calls.Load(), committer.calls)
	}
	active, err := activator.ActiveCredential()
	if err != nil {
		t.Fatal(err)
	}
	defer active.Destroy()
	if active.VersionID != "credential-version-recovered" || !bytes.Contains(active.CredentialJSON, []byte("retry-source-secret")) {
		t.Fatalf("recovered active credential = %+v", active)
	}
}

func TestSecureActivatorRejectsSamePendingLeaseWithDifferentBundle(t *testing.T) {
	t.Parallel()
	identity := Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", NodeID: "srv74", Epoch: 3}
	recipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x37}, 32)))
	defer recipient.Destroy()
	onboarder := &fakeOnboardingEngine{}
	committer := &recordingCredentialCommitter{err: errors.New("commit unavailable")}
	defer committer.destroy()
	activator, _ := NewSecureActivator(SecureActivatorConfig{Identity: identity, Recipient: recipient, Onboarder: onboarder, Committer: committer})
	defer activator.Drain()
	first := sealedTestActivation(t, recipient, identity, "lease-pending", "proxy", []byte("first-source-secret"))
	if _, err := activator.Activate(context.Background(), first); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("first pending activation error = %v", err)
	}
	swapped := sealedTestActivation(t, recipient, identity, "lease-pending", "proxy", []byte("swapped-source-secret"))
	if _, err := activator.Activate(context.Background(), swapped); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("pending lease bundle swap error = %v", err)
	}
	if onboarder.calls.Load() != 1 {
		t.Fatalf("bundle swap repeated onboarding: %d", onboarder.calls.Load())
	}
}

func sealedTestActivation(t *testing.T, recipient *credential.Recipient, identity Identity, leaseID, proxyLeaseID string, secret []byte) Activation {
	t.Helper()
	input := OnboardingInput{Source: OnboardingSessionKey, AuthType: AuthTypeOAuth, Secret: secret}
	rotationRecipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x55}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer rotationRecipient.Destroy()
	rotationKeyID, rotationPublicKey, err := rotationRecipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := EncodeActivationPackage(ActivationPackage{
		Input: input, RotationRecipientKeyID: rotationKeyID, RotationRecipientPublicKey: rotationPublicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer zero(payload)
	keyID, publicKey, err := recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := credential.SealForRecipient(context.Background(), bytes.NewReader(bytes.Repeat([]byte{0x44}, 128)), keyID, publicKey, credential.TransportContext{
		AccountBinding: identity.AccountID, SlotID: identity.SlotID, ExecutionEpoch: identity.Epoch,
		LeaseID: leaseID, ProxyLeaseID: proxyLeaseID, Purpose: "onboarding",
	}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return Activation{CredentialLeaseID: leaseID, ProxyLeaseID: proxyLeaseID, EncryptedCredentialBundle: sealed}
}
