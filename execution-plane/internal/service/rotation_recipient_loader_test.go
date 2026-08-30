package service

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

func TestLoadRotationRecipientRequiresProtectedKMSFile(t *testing.T) {
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x71}, 32), "kms-loader", "v1")
	cryptoService, _ := credential.NewService(kms)
	privateKey := bytes.Repeat([]byte{0x72}, 32)
	envelope, err := cryptoService.SealServiceKey(context.Background(), RotationRecipientMetadata(), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	defer envelope.Destroy()
	payload, _ := json.Marshal(envelope)
	path := filepath.Join(t.TempDir(), "rotation-recipient-envelope.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	recipient, err := LoadRotationRecipient(context.Background(), kms, path)
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, err := recipient.PublicKey()
	if err != nil || credential.ValidateRecipientKey(keyID, publicKey) != nil {
		t.Fatalf("loaded recipient is invalid: %v", err)
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRotationRecipient(context.Background(), kms, path); err != ErrRotationRecipientLoad {
		t.Fatalf("world-readable envelope error = %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(t.TempDir(), "rotation-recipient-link.json")
	if err := os.Symlink(path, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRotationRecipient(context.Background(), kms, symlink); err != ErrRotationRecipientLoad {
		t.Fatalf("symlink envelope error = %v", err)
	}
}

func TestLoadRotationRecipientRejectsTamperAndWrongPurpose(t *testing.T) {
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x73}, 32), "kms-loader", "v1")
	cryptoService, _ := credential.NewService(kms)
	privateKey := bytes.Repeat([]byte{0x74}, 32)
	envelope, _ := cryptoService.SealServiceKey(context.Background(), credential.ServiceKeyMetadata{
		ServiceID: "orchestrator", Purpose: "different-purpose", Version: 1,
	}, privateKey)
	defer envelope.Destroy()
	payload, _ := json.Marshal(envelope)
	path := filepath.Join(t.TempDir(), "wrong-purpose.json")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRotationRecipient(context.Background(), kms, path); err != ErrRotationRecipientLoad {
		t.Fatalf("wrong-purpose envelope error = %v", err)
	}

	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	document["unexpected"] = "field"
	tampered, _ := json.Marshal(document)
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRotationRecipient(context.Background(), kms, path); err != ErrRotationRecipientLoad {
		t.Fatalf("unknown-field envelope error = %v", err)
	}
}
