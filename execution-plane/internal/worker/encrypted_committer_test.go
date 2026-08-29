package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

type decryptingCredentialSink struct {
	mu        sync.Mutex
	recipient *credential.Recipient
	request   SealedCredentialCommitRequest
	material  credential.RotationMaterial
	err       error
}

func (s *decryptingCredentialSink) CommitSealedCredential(ctx context.Context, request SealedCredentialCommitRequest) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	binding := credential.TransportContext{
		AccountBinding: request.AccountBinding, SlotID: request.SlotID, ExecutionEpoch: request.ExecutionEpoch,
		LeaseID: request.CredentialLeaseID, ProxyLeaseID: request.ProxyLeaseID, Purpose: "rotation",
	}
	plaintext, err := s.recipient.Open(ctx, request.SealedCredentialBundle, binding)
	if err != nil {
		return "", err
	}
	defer zero(plaintext)
	material, err := credential.DecodeRotationMaterial(plaintext)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.material.Destroy()
	s.material = material
	s.request = request
	s.request.SealedCredentialBundle = append([]byte(nil), request.SealedCredentialBundle...)
	return "version-encrypted-1", nil
}

func (s *decryptingCredentialSink) destroy() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.material.Destroy()
	zero(s.request.SealedCredentialBundle)
}

func TestEncryptedCredentialCommitterSealsReturnPath(t *testing.T) {
	t.Parallel()
	recipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x67}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	sink := &decryptingCredentialSink{recipient: recipient}
	defer sink.destroy()
	committer, err := NewEncryptedCredentialCommitter(bytes.NewReader(bytes.Repeat([]byte{0x68}, 256)), sink)
	if err != nil {
		t.Fatal(err)
	}
	secret := "normalized-return-path-secret"
	versionID, err := committer.CommitCredential(context.Background(), CredentialCommitRequest{
		AccountBinding: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", ExecutionEpoch: 7,
		CredentialLeaseID: "credential-lease-7", ProxyLeaseID: "proxy-lease-7",
		RotationRecipientKeyID: keyID, RotationRecipientPublicKey: publicKey,
		Result: &OnboardingResult{AuthType: AuthTypeOAuth, CredentialJSON: []byte(`{"access_token":"` + secret + `"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if versionID != "version-encrypted-1" {
		t.Fatalf("version id = %q", versionID)
	}
	sink.mu.Lock()
	request := sink.request
	material := credential.RotationMaterial{AuthType: sink.material.AuthType, Plaintext: append([]byte(nil), sink.material.Plaintext...)}
	sink.mu.Unlock()
	defer material.Destroy()
	if bytes.Contains(request.SealedCredentialBundle, []byte(secret)) {
		t.Fatal("worker return path contains plaintext credential")
	}
	if material.AuthType != AuthTypeOAuth || !bytes.Contains(material.Plaintext, []byte(secret)) {
		t.Fatalf("unexpected decrypted rotation material: %+v", material)
	}
	for _, serialized := range []string{request.String(), fmt.Sprintf("%+v", request), string(mustEncryptedCommitJSON(t, request))} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("sealed commit serialization leaked secret: %s", serialized)
		}
	}
}

func TestEncryptedCredentialCommitterFailsClosed(t *testing.T) {
	t.Parallel()
	recipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x69}, 32)))
	defer recipient.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	base := CredentialCommitRequest{
		AccountBinding: "account-binding", SlotID: "slot", ExecutionEpoch: 1,
		CredentialLeaseID: "lease", ProxyLeaseID: "proxy", RotationRecipientKeyID: keyID,
		RotationRecipientPublicKey: publicKey,
		Result:                     &OnboardingResult{AuthType: AuthTypeAPIKey, CredentialJSON: []byte(`{"api_key":"must-not-leak"}`)},
	}
	for name, mutate := range map[string]func(*CredentialCommitRequest){
		"missing result":  func(request *CredentialCommitRequest) { request.Result = nil },
		"wrong recipient": func(request *CredentialCommitRequest) { request.RotationRecipientPublicKey[0] ^= 1 },
		"missing lease":   func(request *CredentialCommitRequest) { request.CredentialLeaseID = "" },
		"bad auth":        func(request *CredentialCommitRequest) { request.Result.AuthType = "unknown" },
	} {
		t.Run(name, func(t *testing.T) {
			request := base
			request.RotationRecipientPublicKey = append([]byte(nil), base.RotationRecipientPublicKey...)
			if base.Result != nil {
				result := *base.Result
				result.CredentialJSON = append([]byte(nil), base.Result.CredentialJSON...)
				request.Result = &result
			}
			mutate(&request)
			sink := &decryptingCredentialSink{recipient: recipient}
			committer, _ := NewEncryptedCredentialCommitter(bytes.NewReader(bytes.Repeat([]byte{0x70}, 256)), sink)
			if _, err := committer.CommitCredential(context.Background(), request); !errors.Is(err, ErrCredentialCommitRejected) || strings.Contains(err.Error(), "must-not-leak") {
				t.Fatalf("invalid commit error = %v", err)
			}
			sink.destroy()
		})
	}

	sink := &decryptingCredentialSink{recipient: recipient, err: errors.New("sink exposed must-not-leak")}
	committer, _ := NewEncryptedCredentialCommitter(bytes.NewReader(bytes.Repeat([]byte{0x71}, 256)), sink)
	if _, err := committer.CommitCredential(context.Background(), base); !errors.Is(err, ErrCredentialCommitRejected) || strings.Contains(err.Error(), "must-not-leak") {
		t.Fatalf("sink failure error = %v", err)
	}
}

func mustEncryptedCommitJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
