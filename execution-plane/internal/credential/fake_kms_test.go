package credential

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestFakeKMSRoundTripAndContextBinding(t *testing.T) {
	t.Parallel()
	provider, err := NewFakeKMS(bytes.Repeat([]byte{0x33}, dataKeySize), "kms-fake", "version-1")
	if err != nil {
		t.Fatalf("new fake KMS: %v", err)
	}
	aad := []byte(`{"account_id":"account-1"}`)
	generated, err := provider.GenerateDataKey(context.Background(), aad)
	if err != nil {
		t.Fatalf("generate data key: %v", err)
	}
	defer zeroBytes(generated.Plaintext)

	decrypted, err := provider.DecryptDataKey(context.Background(), generated.Wrapped, aad)
	if err != nil {
		t.Fatalf("decrypt data key: %v", err)
	}
	defer zeroBytes(decrypted)
	if !bytes.Equal(decrypted, generated.Plaintext) {
		t.Fatal("decrypted data key does not match generated key")
	}

	if value, err := provider.DecryptDataKey(context.Background(), generated.Wrapped, []byte("different-aad")); !errors.Is(err, ErrKMSUnavailable) || value != nil {
		t.Fatalf("DecryptDataKey(different AAD) = (%x, %v), want nil and ErrKMSUnavailable", value, err)
	}
	tampered := generated.Wrapped
	tampered.Ciphertext = append([]byte(nil), tampered.Ciphertext...)
	tampered.Ciphertext[len(tampered.Ciphertext)-1] ^= 0xff
	if _, err := provider.DecryptDataKey(context.Background(), tampered, aad); !errors.Is(err, ErrKMSUnavailable) {
		t.Fatalf("DecryptDataKey(tampered) error = %v, want ErrKMSUnavailable", err)
	}
}

func TestFakeKMSRejectsInvalidConfigurationAndCancellation(t *testing.T) {
	t.Parallel()
	if _, err := NewFakeKMS([]byte("short"), "kms-fake", "v1"); err == nil {
		t.Fatal("NewFakeKMS(short key) succeeded")
	}
	provider, err := NewFakeKMS(bytes.Repeat([]byte{0x33}, dataKeySize), "kms-fake", "v1")
	if err != nil {
		t.Fatalf("new fake KMS: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.GenerateDataKey(ctx, []byte("aad")); !errors.Is(err, ErrKMSUnavailable) {
		t.Fatalf("GenerateDataKey(cancelled) error = %v, want ErrKMSUnavailable", err)
	}
}
