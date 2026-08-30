package credential

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestOnboardingIntentEnvelopeBindsLifecycleIdentity(t *testing.T) {
	t.Parallel()
	service := newTestService(t)
	metadata := OnboardingIntentMetadata{
		IntentID: "intent-10380", AccountID: "10380", DesiredGeneration: 7,
		Source: "session_key", AuthType: "oauth",
	}
	plaintext := []byte("encoded-onboarding-material")
	envelope, err := service.SealOnboardingIntent(context.Background(), metadata, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(envelope.Ciphertext, plaintext) || bytes.Contains(envelope.EncryptedDEK, plaintext) {
		t.Fatal("onboarding intent envelope contains plaintext")
	}
	opened, err := service.OpenOnboardingIntent(context.Background(), metadata, envelope)
	if err != nil || !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened intent = %q, %v", opened, err)
	}
	for name, mutate := range map[string]func(*OnboardingIntentMetadata){
		"intent":     func(value *OnboardingIntentMetadata) { value.IntentID = "intent-other" },
		"account":    func(value *OnboardingIntentMetadata) { value.AccountID = "10381" },
		"generation": func(value *OnboardingIntentMetadata) { value.DesiredGeneration++ },
		"source":     func(value *OnboardingIntentMetadata) { value.Source = "cookie" },
		"auth type":  func(value *OnboardingIntentMetadata) { value.AuthType = "setup_token" },
	} {
		t.Run(name, func(t *testing.T) {
			tampered := metadata
			mutate(&tampered)
			if _, err := service.OpenOnboardingIntent(context.Background(), tampered, envelope); !errors.Is(err, ErrInvalidEnvelope) {
				t.Fatalf("tampered metadata error = %v", err)
			}
		})
	}
}

func TestEnvelopeCloneAndDestroyOwnBuffers(t *testing.T) {
	t.Parallel()
	original := Envelope{
		Ciphertext: []byte("ciphertext-material"), EncryptedDEK: []byte("wrapped-key"),
		Nonce: bytes.Repeat([]byte{1}, gcmNonceSize), AADJSON: []byte("authenticated-metadata"),
		KMSKeyID: "kms-id", KMSKeyVersion: "v1",
	}
	clone := original.Clone()
	clone.Destroy()
	if clone.Ciphertext != nil || clone.EncryptedDEK != nil || clone.Nonce != nil || clone.AADJSON != nil || clone.KMSKeyID != "" {
		t.Fatalf("destroyed envelope = %+v", clone)
	}
	if string(original.Ciphertext) != "ciphertext-material" || string(original.EncryptedDEK) != "wrapped-key" {
		t.Fatal("destroying clone modified original buffers")
	}
}
