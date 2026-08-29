package worker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

var ErrCredentialCommitRejected = errors.New("encrypted credential commit rejected")

// SealedCredentialCommitRequest is the only credential form permitted across
// the worker -> host-agent -> orchestrator return path. Its binding fields are
// authenticated again by the transport envelope.
type SealedCredentialCommitRequest struct {
	AccountBinding         string
	SlotID                 string
	ExecutionEpoch         uint64
	CredentialLeaseID      string
	ProxyLeaseID           string
	SealedCredentialBundle []byte
}

func (r SealedCredentialCommitRequest) String() string {
	return fmt.Sprintf("SealedCredentialCommitRequest{AccountBinding:%q SlotID:%q ExecutionEpoch:%d CredentialLeaseID:%q ProxyLeaseID:%q SealedCredentialBundle:[REDACTED]}",
		r.AccountBinding, r.SlotID, r.ExecutionEpoch, r.CredentialLeaseID, r.ProxyLeaseID)
}

func (r SealedCredentialCommitRequest) GoString() string { return r.String() }

func (r SealedCredentialCommitRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AccountBinding    string `json:"account_binding"`
		SlotID            string `json:"slot_id"`
		ExecutionEpoch    uint64 `json:"execution_epoch"`
		CredentialLeaseID string `json:"credential_lease_id"`
		ProxyLeaseID      string `json:"proxy_lease_id"`
	}{r.AccountBinding, r.SlotID, r.ExecutionEpoch, r.CredentialLeaseID, r.ProxyLeaseID})
}

// SealedCredentialSink synchronously forwards a ciphertext bundle to the
// authenticated orchestrator channel and returns only after its vault commit.
// Implementations must copy the request if they retain it after return.
type SealedCredentialSink interface {
	CommitSealedCredential(ctx context.Context, request SealedCredentialCommitRequest) (versionID string, err error)
}

type EncryptedCredentialCommitter struct {
	random io.Reader
	sink   SealedCredentialSink
}

func NewEncryptedCredentialCommitter(random io.Reader, sink SealedCredentialSink) (*EncryptedCredentialCommitter, error) {
	if sink == nil {
		return nil, errors.New("sealed credential sink is required")
	}
	if random == nil {
		random = rand.Reader
	}
	return &EncryptedCredentialCommitter{random: random, sink: sink}, nil
}

func (c *EncryptedCredentialCommitter) CommitCredential(ctx context.Context, request CredentialCommitRequest) (string, error) {
	if c == nil || c.sink == nil || ctx == nil || ctx.Err() != nil || request.Result == nil ||
		credential.ValidateRecipientKey(request.RotationRecipientKeyID, request.RotationRecipientPublicKey) != nil {
		return "", ErrCredentialCommitRejected
	}
	binding := credential.TransportContext{
		AccountBinding: request.AccountBinding, SlotID: request.SlotID, ExecutionEpoch: request.ExecutionEpoch,
		LeaseID: request.CredentialLeaseID, ProxyLeaseID: request.ProxyLeaseID, Purpose: "rotation",
	}
	if binding.Validate() != nil {
		return "", ErrCredentialCommitRejected
	}
	material, err := credential.EncodeRotationMaterial(credential.RotationMaterial{
		AuthType: request.Result.AuthType, Plaintext: request.Result.CredentialJSON,
	})
	if err != nil {
		return "", ErrCredentialCommitRejected
	}
	defer zero(material)
	sealed, err := credential.SealForRecipient(ctx, c.random, request.RotationRecipientKeyID, request.RotationRecipientPublicKey, binding, material)
	if err != nil {
		return "", ErrCredentialCommitRejected
	}
	defer zero(sealed)
	versionID, err := c.sink.CommitSealedCredential(ctx, SealedCredentialCommitRequest{
		AccountBinding: request.AccountBinding, SlotID: request.SlotID, ExecutionEpoch: request.ExecutionEpoch,
		CredentialLeaseID: request.CredentialLeaseID, ProxyLeaseID: request.ProxyLeaseID,
		SealedCredentialBundle: sealed,
	})
	if err != nil || !validCredentialVersionID(versionID) {
		return "", ErrCredentialCommitRejected
	}
	return versionID, nil
}

func validCredentialVersionID(value string) bool {
	return credential.ValidateTransportID(value) == nil
}

var _ CredentialCommitter = (*EncryptedCredentialCommitter)(nil)
