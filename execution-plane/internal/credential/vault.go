package credential

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	defaultCredentialLeaseTTL = 30 * time.Second
	maxCredentialLeaseTTL     = 2 * time.Minute
	maxRotationAttempts       = 8
	credentialTokenBytes      = 32
)

var (
	ErrCredentialVaultNotFound     = errors.New("credential vault is not initialized")
	ErrCredentialVersionConflict   = errors.New("credential version changed concurrently")
	ErrCredentialOperationNotFound = errors.New("credential rotation operation is not committed")
	ErrCredentialOperationConflict = errors.New("credential rotation operation conflicts with its committed version")
	ErrCredentialLeaseRejected     = errors.New("credential lease is invalid, expired or already consumed")
)

type VersionRecord struct {
	ID            string
	AccountID     string
	VersionNumber uint64
	AuthType      string
	Envelope      Envelope
	Hint          string
	CreatedAt     time.Time
}

func (v VersionRecord) Validate() error {
	return (&v).NormalizeAndValidate()
}

// NormalizeAndValidate accepts JSON storage normalization (for example MySQL
// key reordering/whitespace) only when every authenticated field is exactly
// equal, then restores the one canonical byte representation used by AES-GCM
// and KMS EncryptionContext.
func (v *VersionRecord) NormalizeAndValidate() error {
	if v == nil {
		return errors.New("credential version is nil")
	}
	if !validOpaqueID(v.ID) || v.CreatedAt.IsZero() {
		return errors.New("credential version identity is invalid")
	}
	metadata := Metadata{AccountID: v.AccountID, VersionNumber: v.VersionNumber, AuthType: v.AuthType}
	expectedAAD, err := metadata.canonicalAAD()
	if err != nil {
		return err
	}
	if err := v.Envelope.validate(); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(v.Envelope.AADJSON))
	decoder.DisallowUnknownFields()
	var storedAAD canonicalAADPayload
	if err := decoder.Decode(&storedAAD); err != nil {
		return ErrInvalidEnvelope
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrInvalidEnvelope
	}
	if storedAAD.Schema != aadSchema || storedAAD.AccountID != v.AccountID ||
		storedAAD.CredentialVersion != v.VersionNumber || storedAAD.AuthType != v.AuthType {
		return ErrInvalidEnvelope
	}
	v.Envelope.AADJSON = expectedAAD
	if err := validateCredentialHint(v.Hint); err != nil {
		return err
	}
	return nil
}

type LeaseRecord struct {
	ID             string
	TokenSHA256    [32]byte
	AccountID      string
	VersionID      string
	SlotID         string
	ExecutionEpoch uint64
	ExpiresAt      time.Time
	ConsumedAt     *time.Time
	RevokedAt      *time.Time
	CreatedAt      time.Time
}

func (l LeaseRecord) ValidateForIssue() error {
	if !validOpaqueID(l.ID) || l.TokenSHA256 == ([32]byte{}) || l.VersionID != "" || l.ConsumedAt != nil || l.RevokedAt != nil ||
		l.CreatedAt.IsZero() || !l.ExpiresAt.After(l.CreatedAt) || l.ExpiresAt.Sub(l.CreatedAt) > maxCredentialLeaseTTL {
		return errors.New("credential lease candidate is invalid")
	}
	return validateLeaseBinding(l.AccountID, l.SlotID, l.ExecutionEpoch)
}

type LeaseClaim struct {
	TokenSHA256     [32]byte
	AccountID       string
	SlotID          string
	ExecutionEpoch  uint64
	SecurityEventID string
	ConsumedAt      time.Time
}

func (c LeaseClaim) Validate() error {
	if c.TokenSHA256 == ([32]byte{}) || !validOpaqueID(c.SecurityEventID) || c.ConsumedAt.IsZero() {
		return errors.New("credential lease claim is invalid")
	}
	return validateLeaseBinding(c.AccountID, c.SlotID, c.ExecutionEpoch)
}

type SecurityEvent struct {
	ID             string
	EventType      string
	ReasonCode     string
	AccountID      string
	SlotID         string
	ExecutionEpoch uint64
	LeaseID        string
	CreatedAt      time.Time
}

type VaultRepository interface {
	NextCredentialVersionNumber(ctx context.Context, accountID string) (uint64, error)
	CommitCredentialVersion(ctx context.Context, version VersionRecord) error
	GetActiveCredentialVersion(ctx context.Context, accountID string) (VersionRecord, error)
	IssueCredentialLease(ctx context.Context, candidate LeaseRecord) (LeaseRecord, error)
	ConsumeCredentialLease(ctx context.Context, claim LeaseClaim) (VersionRecord, error)
}

// IdempotentVaultRepository atomically binds one durable operation identity to
// the credential version it creates. The caller that chooses operationID is
// responsible for separately fencing the authenticated material digest; the
// repository deliberately persists no plaintext-derived digest.
type IdempotentVaultRepository interface {
	VaultRepository
	GetCredentialVersionByOperation(ctx context.Context, operationID string) (VersionRecord, error)
	CommitCredentialVersionForOperation(ctx context.Context, operationID string, version VersionRecord) error
}

type VaultConfig struct {
	LeaseTTL time.Duration
	Now      func() time.Time
	Random   io.Reader
}

type Vault struct {
	crypto   *Service
	repo     VaultRepository
	leaseTTL time.Duration
	now      func() time.Time
	random   io.Reader
	locks    sync.Map
}

type LeaseGrant struct {
	Token          string
	LeaseID        string
	AccountID      string
	VersionID      string
	SlotID         string
	ExecutionEpoch uint64
	ExpiresAt      time.Time
}

func (g LeaseGrant) String() string {
	return fmt.Sprintf("LeaseGrant{LeaseID:%q AccountID:%q VersionID:%q SlotID:%q ExecutionEpoch:%d ExpiresAt:%s Token:[REDACTED]}",
		g.LeaseID, g.AccountID, g.VersionID, g.SlotID, g.ExecutionEpoch, g.ExpiresAt.UTC().Format(time.RFC3339Nano))
}

func (g LeaseGrant) GoString() string { return g.String() }

func (g LeaseGrant) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		LeaseID        string    `json:"lease_id"`
		AccountID      string    `json:"account_id"`
		VersionID      string    `json:"version_id"`
		SlotID         string    `json:"slot_id"`
		ExecutionEpoch uint64    `json:"execution_epoch"`
		ExpiresAt      time.Time `json:"expires_at"`
	}{g.LeaseID, g.AccountID, g.VersionID, g.SlotID, g.ExecutionEpoch, g.ExpiresAt})
}

type LeasedCredential struct {
	AccountID     string
	VersionID     string
	VersionNumber uint64
	AuthType      string
	Hint          string
	Plaintext     []byte
}

func (c LeasedCredential) String() string {
	return fmt.Sprintf("LeasedCredential{AccountID:%q VersionID:%q VersionNumber:%d AuthType:%q Hint:%q Plaintext:[REDACTED]}",
		c.AccountID, c.VersionID, c.VersionNumber, c.AuthType, c.Hint)
}

func (c LeasedCredential) GoString() string { return c.String() }

func (c LeasedCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AccountID     string `json:"account_id"`
		VersionID     string `json:"version_id"`
		VersionNumber uint64 `json:"version_number"`
		AuthType      string `json:"auth_type"`
		Hint          string `json:"hint"`
	}{c.AccountID, c.VersionID, c.VersionNumber, c.AuthType, c.Hint})
}

func (c *LeasedCredential) Destroy() {
	if c == nil {
		return
	}
	zeroBytes(c.Plaintext)
	c.Plaintext = nil
}

func NewVault(cryptoService *Service, repository VaultRepository, config VaultConfig) (*Vault, error) {
	if cryptoService == nil || repository == nil {
		return nil, errors.New("credential crypto service and repository are required")
	}
	if config.LeaseTTL == 0 {
		config.LeaseTTL = defaultCredentialLeaseTTL
	}
	if config.LeaseTTL <= 0 || config.LeaseTTL > maxCredentialLeaseTTL {
		return nil, errors.New("credential lease TTL must be between 1ns and 2m")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &Vault{crypto: cryptoService, repo: repository, leaseTTL: config.LeaseTTL, now: config.Now, random: config.Random}, nil
}

// Rotate encrypts and commits a new active version. Same-process operations are
// serialized per account; the repository's version fence covers other replicas.
func (v *Vault) Rotate(ctx context.Context, accountID, authType, hint string, plaintext []byte) (VersionRecord, error) {
	metadata, err := validateRotationInput(accountID, authType, hint, plaintext)
	if err != nil {
		return VersionRecord{}, err
	}

	accountLockValue, _ := v.locks.LoadOrStore(accountID, &sync.Mutex{})
	accountLock := accountLockValue.(*sync.Mutex)
	accountLock.Lock()
	defer accountLock.Unlock()
	return v.rotateLocked(ctx, metadata, hint, plaintext, v.repo.CommitCredentialVersion)
}

// RotateIdempotent encrypts at most one credential version for operationID.
// A retry after a lost acknowledgement or process crash returns the original
// committed version. operationID must be a durable, authenticated identity
// whose material digest is fenced by the higher-level rotation authorizer.
func (v *Vault) RotateIdempotent(ctx context.Context, operationID, accountID, authType, hint string, plaintext []byte) (VersionRecord, error) {
	if ValidateTransportID(operationID) != nil {
		return VersionRecord{}, ErrCredentialOperationConflict
	}
	metadata, err := validateRotationInput(accountID, authType, hint, plaintext)
	if err != nil {
		return VersionRecord{}, err
	}
	repository, ok := v.repo.(IdempotentVaultRepository)
	if !ok {
		return VersionRecord{}, errors.New("credential vault persistence is unavailable")
	}

	accountLockValue, _ := v.locks.LoadOrStore(accountID, &sync.Mutex{})
	accountLock := accountLockValue.(*sync.Mutex)
	accountLock.Lock()
	defer accountLock.Unlock()

	existing, err := repository.GetCredentialVersionByOperation(ctx, operationID)
	if err == nil {
		return validateCommittedOperation(existing, accountID, authType, hint)
	}
	if !errors.Is(err, ErrCredentialOperationNotFound) {
		return VersionRecord{}, sanitizeRepositoryError(err)
	}

	return v.rotateLocked(ctx, metadata, hint, plaintext, func(commitContext context.Context, candidate VersionRecord) error {
		return repository.CommitCredentialVersionForOperation(commitContext, operationID, candidate)
	}, func(commitErr error) (VersionRecord, bool, error) {
		committed, lookupErr := repository.GetCredentialVersionByOperation(ctx, operationID)
		if lookupErr == nil {
			validated, validationErr := validateCommittedOperation(committed, accountID, authType, hint)
			return validated, true, validationErr
		}
		if !errors.Is(lookupErr, ErrCredentialOperationNotFound) {
			return VersionRecord{}, true, sanitizeRepositoryError(lookupErr)
		}
		return VersionRecord{}, false, commitErr
	})
}

type credentialVersionCommit func(context.Context, VersionRecord) error

type credentialVersionRecovery func(error) (VersionRecord, bool, error)

func (v *Vault) rotateLocked(
	ctx context.Context,
	metadata Metadata,
	hint string,
	plaintext []byte,
	commit credentialVersionCommit,
	recoveries ...credentialVersionRecovery,
) (VersionRecord, error) {

	for attempt := 0; attempt < maxRotationAttempts; attempt++ {
		versionNumber, err := v.repo.NextCredentialVersionNumber(ctx, metadata.AccountID)
		if err != nil {
			return VersionRecord{}, sanitizeRepositoryError(err)
		}
		metadata.VersionNumber = versionNumber
		envelope, err := v.crypto.Seal(ctx, metadata, plaintext)
		if err != nil {
			return VersionRecord{}, err
		}
		versionID, err := randomUUID(v.random)
		if err != nil {
			return VersionRecord{}, ErrEncryption
		}
		record := VersionRecord{
			ID: versionID, AccountID: metadata.AccountID, VersionNumber: versionNumber,
			AuthType: metadata.AuthType, Envelope: envelope, Hint: hint, CreatedAt: v.now().UTC(),
		}
		if err := commit(ctx, record); err != nil {
			for _, recovery := range recoveries {
				if recovered, handled, recoveryErr := recovery(err); handled {
					return recovered, recoveryErr
				}
			}
			if errors.Is(err, ErrCredentialVersionConflict) {
				continue
			}
			return VersionRecord{}, sanitizeRepositoryError(err)
		}
		return record, nil
	}
	return VersionRecord{}, ErrCredentialVersionConflict
}

func validateRotationInput(accountID, authType, hint string, plaintext []byte) (Metadata, error) {
	metadata := Metadata{AccountID: accountID, VersionNumber: 1, AuthType: authType}
	if _, err := metadata.canonicalAAD(); err != nil {
		return Metadata{}, err
	}
	if err := validateCredentialHint(hint); err != nil {
		return Metadata{}, err
	}
	if len(plaintext) == 0 || len(plaintext) > maxPlaintextBytes {
		return Metadata{}, ErrEncryption
	}
	return metadata, nil
}

func validateCommittedOperation(record VersionRecord, accountID, authType, hint string) (VersionRecord, error) {
	if err := record.Validate(); err != nil || record.AccountID != accountID || record.AuthType != authType || record.Hint != hint {
		return VersionRecord{}, ErrCredentialOperationConflict
	}
	return record, nil
}

func (v *Vault) IssueLease(ctx context.Context, accountID, slotID string, executionEpoch uint64) (LeaseGrant, error) {
	if err := validateLeaseBinding(accountID, slotID, executionEpoch); err != nil {
		return LeaseGrant{}, err
	}
	leaseID, err := randomUUID(v.random)
	if err != nil {
		return LeaseGrant{}, errors.New("credential lease token generation failed")
	}
	tokenBytes := make([]byte, credentialTokenBytes)
	if _, err := io.ReadFull(v.random, tokenBytes); err != nil {
		return LeaseGrant{}, errors.New("credential lease token generation failed")
	}
	token := "clt_" + base64.RawURLEncoding.EncodeToString(tokenBytes)
	zeroBytes(tokenBytes)
	now := v.now().UTC()
	record, err := v.repo.IssueCredentialLease(ctx, LeaseRecord{
		ID: leaseID, TokenSHA256: sha256.Sum256([]byte(token)), AccountID: accountID,
		SlotID: slotID, ExecutionEpoch: executionEpoch, ExpiresAt: now.Add(v.leaseTTL), CreatedAt: now,
	})
	if err != nil {
		return LeaseGrant{}, sanitizeRepositoryError(err)
	}
	return LeaseGrant{
		Token: token, LeaseID: record.ID, AccountID: record.AccountID, VersionID: record.VersionID,
		SlotID: record.SlotID, ExecutionEpoch: record.ExecutionEpoch, ExpiresAt: record.ExpiresAt,
	}, nil
}

func (v *Vault) RedeemLease(ctx context.Context, token, accountID, slotID string, executionEpoch uint64) (LeasedCredential, error) {
	if token == "" || len(token) > 128 || !strings.HasPrefix(token, "clt_") {
		return LeasedCredential{}, ErrCredentialLeaseRejected
	}
	if err := validateLeaseBinding(accountID, slotID, executionEpoch); err != nil {
		return LeasedCredential{}, ErrCredentialLeaseRejected
	}
	eventID, err := randomUUID(v.random)
	if err != nil {
		return LeasedCredential{}, ErrCredentialLeaseRejected
	}
	version, err := v.repo.ConsumeCredentialLease(ctx, LeaseClaim{
		TokenSHA256: sha256.Sum256([]byte(token)), AccountID: accountID, SlotID: slotID,
		ExecutionEpoch: executionEpoch, SecurityEventID: eventID, ConsumedAt: v.now().UTC(),
	})
	if err != nil {
		if errors.Is(err, ErrCredentialLeaseRejected) {
			return LeasedCredential{}, ErrCredentialLeaseRejected
		}
		return LeasedCredential{}, sanitizeRepositoryError(err)
	}
	plaintext, err := v.crypto.Open(ctx, Metadata{
		AccountID: version.AccountID, VersionNumber: version.VersionNumber, AuthType: version.AuthType,
	}, version.Envelope)
	if err != nil {
		return LeasedCredential{}, err
	}
	return LeasedCredential{
		AccountID: version.AccountID, VersionID: version.ID, VersionNumber: version.VersionNumber,
		AuthType: version.AuthType, Hint: version.Hint, Plaintext: plaintext,
	}, nil
}

func validateLeaseBinding(accountID, slotID string, executionEpoch uint64) error {
	if validateMetadataString(accountID, 128, false) != nil || validateMetadataString(slotID, 128, false) != nil || executionEpoch == 0 {
		return errors.New("credential lease binding is invalid")
	}
	return nil
}

func validateCredentialHint(hint string) error {
	if len(hint) > 64 || !utf8.ValidString(hint) || strings.TrimSpace(hint) != hint {
		return errors.New("credential hint is invalid")
	}
	for _, char := range hint {
		if unicode.IsControl(char) {
			return errors.New("credential hint is invalid")
		}
	}
	if hint != "" && !strings.Contains(hint, "***") {
		return errors.New("credential hint must be masked")
	}
	return nil
}

func sanitizeRepositoryError(err error) error {
	switch {
	case errors.Is(err, ErrCredentialVaultNotFound):
		return ErrCredentialVaultNotFound
	case errors.Is(err, ErrCredentialVersionConflict):
		return ErrCredentialVersionConflict
	case errors.Is(err, ErrCredentialOperationNotFound):
		return ErrCredentialOperationNotFound
	case errors.Is(err, ErrCredentialOperationConflict):
		return ErrCredentialOperationConflict
	case errors.Is(err, ErrCredentialLeaseRejected):
		return ErrCredentialLeaseRejected
	default:
		return errors.New("credential vault persistence is unavailable")
	}
}

func validOpaqueID(value string) bool {
	return len(value) == 36 && strings.Count(value, "-") == 4 && !strings.ContainsAny(value, " \t\r\n\x00")
}

func randomUUID(random io.Reader) (string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(random, raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}
