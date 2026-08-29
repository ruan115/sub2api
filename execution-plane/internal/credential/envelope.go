package credential

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	dataKeySize       = 32
	gcmNonceSize      = 12
	maxPlaintextBytes = 1 << 20
	maxWrappedKeySize = 64 << 10
	maxAADBytes       = 1024
	aadSchema         = "ccmax.credential.v1"
)

var (
	ErrInvalidMetadata = errors.New("credential metadata is invalid")
	ErrInvalidEnvelope = errors.New("credential envelope is invalid")
	ErrKMSUnavailable  = errors.New("credential key service is unavailable")
	ErrEncryption      = errors.New("credential encryption failed")
	ErrDecryption      = errors.New("credential decryption failed")
)

// Metadata is the authoritative identity bound to both the local AES-GCM
// ciphertext and the KMS-wrapped data key. VersionNumber is monotonically
// increasing within one account.
type Metadata struct {
	AccountID     string
	VersionNumber uint64
	AuthType      string
}

type canonicalAADPayload struct {
	Schema            string `json:"schema"`
	AccountID         string `json:"account_id"`
	CredentialVersion uint64 `json:"credential_version"`
	AuthType          string `json:"auth_type"`
}

// Envelope maps directly to credential_versions without storing plaintext or
// a plaintext data-encryption key.
type Envelope struct {
	Ciphertext    []byte
	EncryptedDEK  []byte
	Nonce         []byte
	AADJSON       []byte
	KMSKeyID      string
	KMSKeyVersion string
}

// WrappedDataKey is the opaque KMS result persisted with an envelope.
type WrappedDataKey struct {
	Ciphertext []byte
	KeyID      string
	KeyVersion string
}

// GeneratedDataKey is returned only to the orchestrator encryption path. The
// caller must erase Plaintext immediately after use.
type GeneratedDataKey struct {
	Plaintext []byte
	Wrapped   WrappedDataKey
}

// KMS is deliberately narrow so workers never need a cloud SDK or KMS
// permission. Implementations must bind the same AAD used by AES-GCM.
type KMS interface {
	GenerateDataKey(ctx context.Context, aad []byte) (GeneratedDataKey, error)
	DecryptDataKey(ctx context.Context, wrapped WrappedDataKey, aad []byte) ([]byte, error)
}

type Service struct {
	kms    KMS
	random io.Reader
}

func NewService(kms KMS) (*Service, error) {
	if kms == nil {
		return nil, errors.New("credential KMS is required")
	}
	return &Service{kms: kms, random: rand.Reader}, nil
}

// Seal encrypts one credential version using a newly generated 256-bit DEK.
func (s *Service) Seal(ctx context.Context, metadata Metadata, plaintext []byte) (Envelope, error) {
	aad, err := metadata.canonicalAAD()
	if err != nil {
		return Envelope{}, err
	}
	if len(plaintext) == 0 || len(plaintext) > maxPlaintextBytes {
		return Envelope{}, fmt.Errorf("%w: plaintext size is outside supported bounds", ErrEncryption)
	}

	generated, err := s.kms.GenerateDataKey(ctx, aad)
	defer zeroBytes(generated.Plaintext)
	if err != nil {
		return Envelope{}, ErrKMSUnavailable
	}
	if err := validateGeneratedDataKey(generated); err != nil {
		return Envelope{}, err
	}

	block, err := aes.NewCipher(generated.Plaintext)
	if err != nil {
		return Envelope{}, ErrEncryption
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, ErrEncryption
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return Envelope{}, ErrEncryption
	}

	envelope := Envelope{
		Ciphertext:    gcm.Seal(nil, nonce, plaintext, aad),
		EncryptedDEK:  append([]byte(nil), generated.Wrapped.Ciphertext...),
		Nonce:         nonce,
		AADJSON:       append([]byte(nil), aad...),
		KMSKeyID:      generated.Wrapped.KeyID,
		KMSKeyVersion: generated.Wrapped.KeyVersion,
	}
	if err := envelope.validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// Open authenticates all persisted metadata before returning plaintext. A KMS
// or integrity failure is fail-closed and never includes sensitive values in
// the returned error.
func (s *Service) Open(ctx context.Context, metadata Metadata, envelope Envelope) ([]byte, error) {
	expectedAAD, err := metadata.canonicalAAD()
	if err != nil {
		return nil, err
	}
	if err := envelope.validate(); err != nil {
		return nil, err
	}
	if subtle.ConstantTimeCompare(expectedAAD, envelope.AADJSON) != 1 {
		return nil, fmt.Errorf("%w: authenticated metadata mismatch", ErrInvalidEnvelope)
	}

	wrapped := WrappedDataKey{
		Ciphertext: envelope.EncryptedDEK,
		KeyID:      envelope.KMSKeyID,
		KeyVersion: envelope.KMSKeyVersion,
	}
	dek, err := s.kms.DecryptDataKey(ctx, wrapped, expectedAAD)
	defer zeroBytes(dek)
	if err != nil {
		return nil, ErrKMSUnavailable
	}
	if len(dek) != dataKeySize {
		return nil, ErrKMSUnavailable
	}

	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, ErrDecryption
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrDecryption
	}
	plaintext, err := gcm.Open(nil, envelope.Nonce, envelope.Ciphertext, expectedAAD)
	if err != nil {
		return nil, ErrDecryption
	}
	return plaintext, nil
}

func (m Metadata) canonicalAAD() ([]byte, error) {
	if err := validateMetadataString(m.AccountID, 128, false); err != nil {
		return nil, fmt.Errorf("%w: account id", ErrInvalidMetadata)
	}
	if m.VersionNumber == 0 {
		return nil, fmt.Errorf("%w: version number", ErrInvalidMetadata)
	}
	if err := validateMetadataString(m.AuthType, 32, true); err != nil {
		return nil, fmt.Errorf("%w: auth type", ErrInvalidMetadata)
	}

	canonical := canonicalAADPayload{
		Schema:            aadSchema,
		AccountID:         m.AccountID,
		CredentialVersion: m.VersionNumber,
		AuthType:          m.AuthType,
	}
	aad, err := json.Marshal(canonical)
	if err != nil || len(aad) > maxAADBytes {
		return nil, fmt.Errorf("%w: canonical AAD", ErrInvalidMetadata)
	}
	return aad, nil
}

func validateMetadataString(value string, maxBytes int, restricted bool) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return ErrInvalidMetadata
	}
	for index, char := range value {
		if unicode.IsControl(char) {
			return ErrInvalidMetadata
		}
		if restricted {
			allowed := (char >= 'a' && char <= 'z') ||
				index > 0 && ((char >= '0' && char <= '9') || char == '_' || char == '-')
			if !allowed {
				return ErrInvalidMetadata
			}
		}
	}
	return nil
}

func (e Envelope) validate() error {
	if len(e.Ciphertext) < 16 || len(e.Ciphertext) > maxPlaintextBytes+16 ||
		len(e.EncryptedDEK) == 0 || len(e.EncryptedDEK) > maxWrappedKeySize ||
		len(e.Nonce) != gcmNonceSize || len(e.AADJSON) == 0 || len(e.AADJSON) > maxAADBytes {
		return ErrInvalidEnvelope
	}
	if err := validateKeyMetadata(e.KMSKeyID, 255); err != nil {
		return ErrInvalidEnvelope
	}
	if err := validateKeyMetadata(e.KMSKeyVersion, 128); err != nil {
		return ErrInvalidEnvelope
	}
	return nil
}

func validateGeneratedDataKey(generated GeneratedDataKey) error {
	if len(generated.Plaintext) != dataKeySize || len(generated.Wrapped.Ciphertext) == 0 || len(generated.Wrapped.Ciphertext) > maxWrappedKeySize {
		return ErrKMSUnavailable
	}
	if err := validateKeyMetadata(generated.Wrapped.KeyID, 255); err != nil {
		return ErrKMSUnavailable
	}
	if err := validateKeyMetadata(generated.Wrapped.KeyVersion, 128); err != nil {
		return ErrKMSUnavailable
	}
	return nil
}

func validateKeyMetadata(value string, maxBytes int) error {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value || strings.ContainsRune(value, '\x00') {
		return ErrInvalidEnvelope
	}
	return nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
