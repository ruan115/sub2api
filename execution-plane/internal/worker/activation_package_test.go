package worker

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

func TestActivationPackageRoundTripRedactsAndErases(t *testing.T) {
	t.Parallel()
	recipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x73}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	pkg := ActivationPackage{
		Input:                  OnboardingInput{Source: OnboardingSessionKey, AuthType: AuthTypeOAuth, Secret: []byte("package-secret")},
		RotationRecipientKeyID: keyID, RotationRecipientPublicKey: publicKey,
	}
	payload, err := EncodeActivationPackage(pkg)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeActivationPackage(payload)
	if err != nil {
		t.Fatal(err)
	}
	if string(decoded.Input.Secret) != "package-secret" || decoded.RotationRecipientKeyID != keyID || !bytes.Equal(decoded.RotationRecipientPublicKey, publicKey) {
		t.Fatalf("decoded activation package mismatch: %+v", decoded)
	}
	for _, serialized := range []string{decoded.String(), fmt.Sprintf("%+v", decoded), string(mustActivationJSON(t, decoded))} {
		if strings.Contains(serialized, "package-secret") {
			t.Fatalf("activation package serialization leaked secret: %s", serialized)
		}
	}
	secretAlias := decoded.Input.Secret
	decoded.Destroy()
	for _, value := range secretAlias {
		if value != 0 {
			t.Fatal("activation package destroy did not erase onboarding secret")
		}
	}
}

func TestActivationPackageRejectsRecipientSubstitutionAndTampering(t *testing.T) {
	t.Parallel()
	recipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x74}, 32)))
	defer recipient.Destroy()
	other, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x75}, 32)))
	defer other.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	_, otherPublicKey, _ := other.PublicKey()
	base := ActivationPackage{
		Input:                  OnboardingInput{Source: OnboardingAPIKey, AuthType: AuthTypeAPIKey, Secret: []byte("sk-test")},
		RotationRecipientKeyID: keyID, RotationRecipientPublicKey: publicKey,
	}
	if _, err := EncodeActivationPackage(ActivationPackage{
		Input: base.Input, RotationRecipientKeyID: keyID, RotationRecipientPublicKey: otherPublicKey,
	}); err == nil {
		t.Fatal("activation package accepted substituted recipient public key")
	}
	payload, err := EncodeActivationPackage(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range [][]byte{
		payload[:40],
		append(append([]byte(nil), payload...), 0),
		func() []byte { changed := append([]byte(nil), payload...); changed[12] ^= 1; return changed }(),
		func() []byte { changed := append([]byte(nil), payload...); changed[len(changed)-1] = 0; return changed }(),
	} {
		decoded, err := DecodeActivationPackage(candidate)
		if err == nil {
			decoded.Destroy()
			t.Fatalf("tampered activation package decoded: %x", candidate)
		}
	}
}

func mustActivationJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
