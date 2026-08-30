package credential

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestServiceSealOpenRoundTripAndIndependentVersions(t *testing.T) {
	t.Parallel()
	service := newTestService(t)
	metadata := Metadata{AccountID: "account-10380", VersionNumber: 7, AuthType: "oauth"}
	plaintext := []byte(`{"access_token":"at-secret","refresh_token":"rt-secret"}`)

	first, err := service.Seal(context.Background(), metadata, plaintext)
	if err != nil {
		t.Fatalf("seal first credential: %v", err)
	}
	second, err := service.Seal(context.Background(), metadata, plaintext)
	if err != nil {
		t.Fatalf("seal second credential: %v", err)
	}
	if bytes.Equal(first.Ciphertext, second.Ciphertext) || bytes.Equal(first.EncryptedDEK, second.EncryptedDEK) {
		t.Fatal("each seal must use an independent data key and nonce")
	}
	if bytes.Contains(first.Ciphertext, plaintext) || bytes.Contains(first.EncryptedDEK, plaintext) {
		t.Fatal("envelope contains plaintext credential bytes")
	}

	opened, err := service.Open(context.Background(), metadata, first)
	if err != nil {
		t.Fatalf("open credential: %v", err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("opened plaintext = %q, want %q", opened, plaintext)
	}
}

func TestServiceRejectsMetadataAndEnvelopeTampering(t *testing.T) {
	t.Parallel()
	service := newTestService(t)
	metadata := Metadata{AccountID: "account-10380", VersionNumber: 7, AuthType: "oauth"}
	envelope, err := service.Seal(context.Background(), metadata, []byte("credential-secret-material"))
	if err != nil {
		t.Fatalf("seal credential: %v", err)
	}

	metadataCases := []Metadata{
		{AccountID: "account-10381", VersionNumber: 7, AuthType: "oauth"},
		{AccountID: "account-10380", VersionNumber: 8, AuthType: "oauth"},
		{AccountID: "account-10380", VersionNumber: 7, AuthType: "api_key"},
	}
	for _, tampered := range metadataCases {
		if _, err := service.Open(context.Background(), tampered, envelope); !errors.Is(err, ErrInvalidEnvelope) {
			t.Fatalf("tampered metadata %+v error = %v, want ErrInvalidEnvelope", tampered, err)
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Envelope)
		wantErr error
	}{
		{
			name: "ciphertext",
			mutate: func(value *Envelope) {
				value.Ciphertext[len(value.Ciphertext)-1] ^= 0xff
			},
			wantErr: ErrDecryption,
		},
		{
			name: "nonce",
			mutate: func(value *Envelope) {
				value.Nonce[0] ^= 0xff
			},
			wantErr: ErrDecryption,
		},
		{
			name: "encrypted data key",
			mutate: func(value *Envelope) {
				value.EncryptedDEK[len(value.EncryptedDEK)-1] ^= 0xff
			},
			wantErr: ErrKMSUnavailable,
		},
		{
			name: "persisted AAD",
			mutate: func(value *Envelope) {
				value.AADJSON = bytes.Replace(value.AADJSON, []byte("account-10380"), []byte("account-10381"), 1)
			},
			wantErr: ErrInvalidEnvelope,
		},
		{
			name: "KMS key id",
			mutate: func(value *Envelope) {
				value.KMSKeyID = "kms-other"
			},
			wantErr: ErrKMSUnavailable,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyOfEnvelope := cloneEnvelope(envelope)
			test.mutate(&copyOfEnvelope)
			if _, err := service.Open(context.Background(), metadata, copyOfEnvelope); !errors.Is(err, test.wantErr) {
				t.Fatalf("Open() error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestServiceFailsClosedWhenKMSFails(t *testing.T) {
	t.Parallel()
	masterKey := bytes.Repeat([]byte{0x45}, dataKeySize)
	kms, err := NewFakeKMS(masterKey, "kms-test", "v1")
	if err != nil {
		t.Fatalf("new fake KMS: %v", err)
	}
	service, err := NewService(kms)
	if err != nil {
		t.Fatalf("new credential service: %v", err)
	}
	metadata := Metadata{AccountID: "account-1", VersionNumber: 1, AuthType: "oauth"}

	kms.SetFailures(errors.New("generate failure with secret-looking text"), nil)
	if envelope, err := service.Seal(context.Background(), metadata, []byte("secret")); !errors.Is(err, ErrKMSUnavailable) || len(envelope.Ciphertext) != 0 {
		t.Fatalf("Seal() = (%+v, %v), want empty envelope and ErrKMSUnavailable", envelope, err)
	}
	kms.SetFailures(nil, nil)
	envelope, err := service.Seal(context.Background(), metadata, []byte("secret"))
	if err != nil {
		t.Fatalf("seal credential: %v", err)
	}
	kms.SetFailures(nil, errors.New("decrypt failure with secret-looking text"))
	if plaintext, err := service.Open(context.Background(), metadata, envelope); !errors.Is(err, ErrKMSUnavailable) || plaintext != nil {
		t.Fatalf("Open() = (%q, %v), want nil and ErrKMSUnavailable", plaintext, err)
	}
}

func TestServiceKeyEnvelopeIsDistinctAndConsumesInput(t *testing.T) {
	kms, err := NewFakeKMS(bytes.Repeat([]byte{0x44}, 32), "kms-service-key", "v1")
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(kms)
	metadata := ServiceKeyMetadata{ServiceID: "orchestrator", Purpose: "rotation-recipient", Version: 1}
	privateKey := bytes.Repeat([]byte{0x55}, 32)
	want := append([]byte(nil), privateKey...)
	envelope, err := service.SealServiceKey(context.Background(), metadata, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer envelope.Destroy()
	if !bytes.Equal(privateKey, make([]byte, 32)) {
		t.Fatal("SealServiceKey did not erase caller input")
	}
	opened, err := service.OpenServiceKey(context.Background(), metadata, envelope)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroBytes(opened)
	if !bytes.Equal(opened, want) {
		t.Fatal("service key round trip changed key bytes")
	}
	if _, err := service.Open(context.Background(), Metadata{AccountID: "orchestrator", VersionNumber: 1, AuthType: "oauth"}, envelope); err == nil {
		t.Fatal("service key envelope opened through credential metadata")
	}
	if _, err := service.OpenServiceKey(context.Background(), ServiceKeyMetadata{
		ServiceID: "orchestrator", Purpose: "different-purpose", Version: 1,
	}, envelope); err == nil {
		t.Fatal("service key envelope opened with different purpose")
	}
}

func TestServiceErasesGeneratedPlaintextDataKey(t *testing.T) {
	t.Parallel()
	provider := &recordingKMS{
		plaintext: bytes.Repeat([]byte{0x5a}, dataKeySize),
		wrapped: WrappedDataKey{
			Ciphertext: []byte("wrapped-data-key"),
			KeyID:      "kms-test",
			KeyVersion: "v1",
		},
	}
	service, err := NewService(provider)
	if err != nil {
		t.Fatalf("new credential service: %v", err)
	}
	_, err = service.Seal(context.Background(), Metadata{AccountID: "account-1", VersionNumber: 1, AuthType: "oauth"}, []byte("secret"))
	if err != nil {
		t.Fatalf("seal credential: %v", err)
	}
	if !bytes.Equal(provider.plaintext, make([]byte, dataKeySize)) {
		t.Fatal("plaintext data key was not erased after encryption")
	}
}

func TestServiceErasesDecryptedDataKeyOnOpenFailure(t *testing.T) {
	t.Parallel()
	provider := &recordingKMS{
		plaintext: bytes.Repeat([]byte{0x5a}, dataKeySize),
		wrapped: WrappedDataKey{
			Ciphertext: []byte("wrapped-data-key"),
			KeyID:      "kms-test",
			KeyVersion: "v1",
		},
	}
	service, err := NewService(provider)
	if err != nil {
		t.Fatalf("new credential service: %v", err)
	}
	metadata := Metadata{AccountID: "account-1", VersionNumber: 1, AuthType: "oauth"}
	aad, err := metadata.canonicalAAD()
	if err != nil {
		t.Fatalf("canonical AAD: %v", err)
	}
	envelope := Envelope{
		Ciphertext:    bytes.Repeat([]byte{0x11}, 32),
		EncryptedDEK:  []byte("wrapped-data-key"),
		Nonce:         bytes.Repeat([]byte{0x22}, gcmNonceSize),
		AADJSON:       aad,
		KMSKeyID:      "kms-test",
		KMSKeyVersion: "v1",
	}
	if _, err := service.Open(context.Background(), metadata, envelope); !errors.Is(err, ErrDecryption) {
		t.Fatalf("Open() error = %v, want ErrDecryption", err)
	}
	if !bytes.Equal(provider.decrypted, make([]byte, dataKeySize)) {
		t.Fatal("plaintext data key was not erased after decryption")
	}
}

func TestServiceValidatesMetadataAndBounds(t *testing.T) {
	t.Parallel()
	service := newTestService(t)
	tests := []Metadata{
		{},
		{AccountID: " account-1", VersionNumber: 1, AuthType: "oauth"},
		{AccountID: "account-1", VersionNumber: 0, AuthType: "oauth"},
		{AccountID: "account-1", VersionNumber: 1, AuthType: "OAuth"},
		{AccountID: "account-1", VersionNumber: 1, AuthType: "oauth/token"},
	}
	for _, metadata := range tests {
		if _, err := service.Seal(context.Background(), metadata, []byte("secret")); !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("Seal(%+v) error = %v, want ErrInvalidMetadata", metadata, err)
		}
	}
	valid := Metadata{AccountID: "account-1", VersionNumber: 1, AuthType: "oauth"}
	if _, err := service.Seal(context.Background(), valid, nil); !errors.Is(err, ErrEncryption) {
		t.Fatalf("Seal(empty) error = %v, want ErrEncryption", err)
	}
	if _, err := service.Seal(context.Background(), valid, make([]byte, maxPlaintextBytes+1)); !errors.Is(err, ErrEncryption) {
		t.Fatalf("Seal(oversize) error = %v, want ErrEncryption", err)
	}
}

func TestVersionRecordNormalizesSemanticJSONStorageWithoutWeakeningAAD(t *testing.T) {
	t.Parallel()
	service := newTestService(t)
	metadata := Metadata{AccountID: "account-1", VersionNumber: 1, AuthType: "oauth"}
	envelope, err := service.Seal(context.Background(), metadata, []byte("credential-secret"))
	if err != nil {
		t.Fatal(err)
	}
	record := VersionRecord{
		ID: "11111111-2222-4333-8444-555555555555", AccountID: metadata.AccountID,
		VersionNumber: metadata.VersionNumber, AuthType: metadata.AuthType, Envelope: envelope,
		CreatedAt: time.Unix(2_000_000_000, 0).UTC(),
	}
	record.Envelope.AADJSON = []byte(`{ "auth_type": "oauth", "credential_version": 1, "account_id": "account-1", "schema": "ccmax.credential.v1" }`)
	if err := record.NormalizeAndValidate(); err != nil {
		t.Fatalf("normalize semantically equal MySQL JSON: %v", err)
	}
	expectedAAD, _ := metadata.canonicalAAD()
	if !bytes.Equal(record.Envelope.AADJSON, expectedAAD) {
		t.Fatalf("normalized AAD = %s, want %s", record.Envelope.AADJSON, expectedAAD)
	}
	if _, err := service.Open(context.Background(), metadata, record.Envelope); err != nil {
		t.Fatalf("open normalized envelope: %v", err)
	}

	for name, aad := range map[string]string{
		"changed account": `{"schema":"ccmax.credential.v1","account_id":"account-2","credential_version":1,"auth_type":"oauth"}`,
		"unknown field":   `{"schema":"ccmax.credential.v1","account_id":"account-1","credential_version":1,"auth_type":"oauth","extra":"value"}`,
		"trailing value":  `{"schema":"ccmax.credential.v1","account_id":"account-1","credential_version":1,"auth_type":"oauth"} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			tampered := record
			tampered.Envelope = cloneEnvelope(record.Envelope)
			tampered.Envelope.AADJSON = []byte(aad)
			if err := tampered.NormalizeAndValidate(); !errors.Is(err, ErrInvalidEnvelope) {
				t.Fatalf("NormalizeAndValidate() error = %v, want ErrInvalidEnvelope", err)
			}
		})
	}
}

type recordingKMS struct {
	plaintext []byte
	wrapped   WrappedDataKey
	decrypted []byte
}

func (k *recordingKMS) GenerateDataKey(context.Context, []byte) (GeneratedDataKey, error) {
	return GeneratedDataKey{Plaintext: k.plaintext, Wrapped: k.wrapped}, nil
}

func (k *recordingKMS) DecryptDataKey(context.Context, WrappedDataKey, []byte) ([]byte, error) {
	k.decrypted = bytes.Repeat([]byte{0x6b}, dataKeySize)
	return k.decrypted, nil
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	masterKey := bytes.Repeat([]byte{0x21}, dataKeySize)
	kms, err := NewFakeKMS(masterKey, "kms-test", "v1")
	if err != nil {
		t.Fatalf("new fake KMS: %v", err)
	}
	service, err := NewService(kms)
	if err != nil {
		t.Fatalf("new credential service: %v", err)
	}
	return service
}

func cloneEnvelope(value Envelope) Envelope {
	value.Ciphertext = append([]byte(nil), value.Ciphertext...)
	value.EncryptedDEK = append([]byte(nil), value.EncryptedDEK...)
	value.Nonce = append([]byte(nil), value.Nonce...)
	value.AADJSON = append([]byte(nil), value.AADJSON...)
	return value
}
