package credential

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
	"sync"
)

// FakeKMS is an in-process development/test provider. It preserves the same
// envelope and AAD semantics as the cloud adapter but must never be enabled in
// production.
type FakeKMS struct {
	masterKey  [dataKeySize]byte
	keyID      string
	keyVersion string
	random     io.Reader

	mu          sync.RWMutex
	generateErr error
	decryptErr  error
}

func NewFakeKMS(masterKey []byte, keyID, keyVersion string) (*FakeKMS, error) {
	if len(masterKey) != dataKeySize {
		return nil, errors.New("fake KMS master key must be 32 bytes")
	}
	if validateKeyMetadata(keyID, 255) != nil || validateKeyMetadata(keyVersion, 128) != nil {
		return nil, errors.New("fake KMS key metadata is invalid")
	}
	provider := &FakeKMS{keyID: keyID, keyVersion: keyVersion, random: rand.Reader}
	copy(provider.masterKey[:], masterKey)
	return provider, nil
}

// SetFailures supports explicit fail-closed tests without exposing or storing
// data keys. Passing nil restores normal operation.
func (k *FakeKMS) SetFailures(generateErr, decryptErr error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.generateErr = generateErr
	k.decryptErr = decryptErr
}

func (k *FakeKMS) GenerateDataKey(ctx context.Context, aad []byte) (GeneratedDataKey, error) {
	if err := ctx.Err(); err != nil {
		return GeneratedDataKey{}, ErrKMSUnavailable
	}
	k.mu.RLock()
	injectedErr := k.generateErr
	k.mu.RUnlock()
	if injectedErr != nil {
		return GeneratedDataKey{}, ErrKMSUnavailable
	}

	plaintext := make([]byte, dataKeySize)
	if _, err := io.ReadFull(k.random, plaintext); err != nil {
		return GeneratedDataKey{}, ErrKMSUnavailable
	}
	wrapped, err := k.wrap(plaintext, aad)
	if err != nil {
		zeroBytes(plaintext)
		return GeneratedDataKey{}, ErrKMSUnavailable
	}
	return GeneratedDataKey{
		Plaintext: plaintext,
		Wrapped: WrappedDataKey{
			Ciphertext: wrapped,
			KeyID:      k.keyID,
			KeyVersion: k.keyVersion,
		},
	}, nil
}

func (k *FakeKMS) DecryptDataKey(ctx context.Context, wrapped WrappedDataKey, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, ErrKMSUnavailable
	}
	k.mu.RLock()
	injectedErr := k.decryptErr
	k.mu.RUnlock()
	if injectedErr != nil || wrapped.KeyID != k.keyID || wrapped.KeyVersion != k.keyVersion || len(wrapped.Ciphertext) <= gcmNonceSize {
		return nil, ErrKMSUnavailable
	}

	block, err := aes.NewCipher(k.masterKey[:])
	if err != nil {
		return nil, ErrKMSUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrKMSUnavailable
	}
	plaintext, err := gcm.Open(nil, wrapped.Ciphertext[:gcmNonceSize], wrapped.Ciphertext[gcmNonceSize:], fakeWrappingAAD(k.keyID, k.keyVersion, aad))
	if err != nil || len(plaintext) != dataKeySize {
		zeroBytes(plaintext)
		return nil, ErrKMSUnavailable
	}
	return plaintext, nil
}

func (k *FakeKMS) wrap(plaintext, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(k.masterKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcmNonceSize)
	if _, err := io.ReadFull(k.random, nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, fakeWrappingAAD(k.keyID, k.keyVersion, aad)), nil
}

func fakeWrappingAAD(keyID, keyVersion string, aad []byte) []byte {
	prefix := []byte("ccmax.fake-kms.v1\x00" + keyID + "\x00" + keyVersion + "\x00")
	return append(prefix, aad...)
}
