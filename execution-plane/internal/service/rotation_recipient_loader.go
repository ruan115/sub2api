package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

const maxRotationRecipientEnvelopeFileBytes = 256 << 10

var ErrRotationRecipientLoad = errors.New("rotation recipient could not be loaded")

func RotationRecipientMetadata() credential.ServiceKeyMetadata {
	return credential.ServiceKeyMetadata{
		ServiceID: "orchestrator", Purpose: "rotation-recipient", Version: 1,
	}
}

// LoadRotationRecipient reads only a KMS envelope. Raw private-key files and
// environment variables are deliberately unsupported. The envelope file must
// be an absolute, owner-only regular file and may not be a symlink.
func LoadRotationRecipient(ctx context.Context, kms credential.KMS, envelopePath string) (*credential.Recipient, error) {
	if ctx == nil || ctx.Err() != nil || kms == nil {
		return nil, ErrRotationRecipientLoad
	}
	payload, err := readProtectedRegularFile(envelopePath, maxRotationRecipientEnvelopeFileBytes, true)
	if err != nil {
		return nil, ErrRotationRecipientLoad
	}
	defer eraseLoaderBytes(payload)
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var envelope credential.Envelope
	if err := decoder.Decode(&envelope); err != nil || decoder.Decode(&struct{}{}) != io.EOF || envelope.Validate() != nil {
		envelope.Destroy()
		return nil, ErrRotationRecipientLoad
	}
	defer envelope.Destroy()
	cryptoService, err := credential.NewService(kms)
	if err != nil {
		return nil, ErrRotationRecipientLoad
	}
	privateKey, err := cryptoService.OpenServiceKey(ctx, RotationRecipientMetadata(), envelope)
	if err != nil {
		eraseLoaderBytes(privateKey)
		return nil, ErrRotationRecipientLoad
	}
	recipient, err := credential.NewRecipientFromPrivateKey(privateKey)
	if err != nil {
		return nil, ErrRotationRecipientLoad
	}
	return recipient, nil
}

func eraseLoaderBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
