package credential

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	credentialTransportSchema  = "sub2api.worker-credential.v1"
	credentialTransportKeySize = 32
	maxTransportBundleBytes    = 2 << 20
	maxTransportPlaintextBytes = maxPlaintextBytes + 1024
	maxTransportIDBytes        = 128
	rotationPayloadHeader      = 11
)

var rotationPayloadMagic = [6]byte{'S', '2', 'C', 'R', '0', '1'}

var ErrCredentialTransport = errors.New("worker credential transport rejected")

// TransportContext is authenticated by X25519/HKDF/AES-GCM and binds a sealed
// credential payload to one worker process, slot, execution epoch and lease.
type TransportContext struct {
	AccountBinding string `json:"account_binding"`
	SlotID         string `json:"slot_id"`
	ExecutionEpoch uint64 `json:"execution_epoch"`
	LeaseID        string `json:"lease_id"`
	ProxyLeaseID   string `json:"proxy_lease_id"`
	Purpose        string `json:"purpose"`
}

func (c TransportContext) Validate() error {
	for _, value := range []string{c.AccountBinding, c.SlotID, c.LeaseID, c.ProxyLeaseID} {
		if ValidateTransportID(value) != nil {
			return ErrCredentialTransport
		}
	}
	if c.ExecutionEpoch == 0 || c.Purpose != "onboarding" && c.Purpose != "activation" && c.Purpose != "rotation" {
		return ErrCredentialTransport
	}
	return nil
}

// ValidateTransportID applies the canonical validation used by authenticated
// transport bindings and their acknowledgement identifiers.
func ValidateTransportID(value string) error {
	if !validTransportID(value) {
		return ErrCredentialTransport
	}
	return nil
}

type Recipient struct {
	mu         sync.RWMutex
	privateKey []byte
	publicKey  []byte
	keyID      string
	destroyed  bool
}

func NewRecipient(random io.Reader) (*Recipient, error) {
	if random == nil {
		random = rand.Reader
	}
	privateBytes := make([]byte, credentialTransportKeySize)
	if _, err := io.ReadFull(random, privateBytes); err != nil {
		zeroBytes(privateBytes)
		return nil, ErrCredentialTransport
	}
	return NewRecipientFromPrivateKey(privateBytes)
}

// NewRecipientFromPrivateKey consumes and erases privateBytes on every path.
// It allows an orchestrator to restore its rotation recipient from a
// KMS-decrypted, durable key instead of silently generating a new identity on
// each restart. The returned Recipient owns its independent private-key copy.
func NewRecipientFromPrivateKey(privateBytes []byte) (*Recipient, error) {
	if len(privateBytes) != credentialTransportKeySize {
		zeroBytes(privateBytes)
		return nil, ErrCredentialTransport
	}
	ownedPrivate := append([]byte(nil), privateBytes...)
	zeroBytes(privateBytes)
	privateKey, err := ecdh.X25519().NewPrivateKey(ownedPrivate)
	if err != nil {
		zeroBytes(ownedPrivate)
		return nil, ErrCredentialTransport
	}
	publicBytes := privateKey.PublicKey().Bytes()
	return &Recipient{
		privateKey: ownedPrivate,
		publicKey:  append([]byte(nil), publicBytes...),
		keyID:      transportKeyID(publicBytes),
	}, nil
}

func (r *Recipient) PublicKey() (keyID string, publicKey []byte, err error) {
	if r == nil {
		return "", nil, ErrCredentialTransport
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.destroyed || len(r.privateKey) != credentialTransportKeySize || len(r.publicKey) != credentialTransportKeySize {
		return "", nil, ErrCredentialTransport
	}
	return r.keyID, append([]byte(nil), r.publicKey...), nil
}

// ValidateRecipientKey verifies that a public transport key is well formed and
// that its key id is the canonical digest of those exact bytes. This is useful
// when a recipient key is carried inside another authenticated envelope.
func ValidateRecipientKey(keyID string, publicKey []byte) error {
	if keyID == "" || len(keyID) > maxTransportIDBytes || len(publicKey) != credentialTransportKeySize || keyID != transportKeyID(publicKey) {
		return ErrCredentialTransport
	}
	if _, err := ecdh.X25519().NewPublicKey(publicKey); err != nil {
		return ErrCredentialTransport
	}
	return nil
}

func (r *Recipient) String() string {
	if r == nil {
		return "Recipient<nil>"
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return fmt.Sprintf("Recipient{KeyID:%q Destroyed:%t PrivateKey:[REDACTED]}", r.keyID, r.destroyed)
}

func (r *Recipient) GoString() string { return r.String() }

func (r *Recipient) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return json.Marshal(struct {
		KeyID     string `json:"key_id"`
		Destroyed bool   `json:"destroyed"`
	}{r.keyID, r.destroyed})
}

func (r *Recipient) Destroy() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.destroyed {
		return
	}
	r.destroyed = true
	zeroBytes(r.privateKey)
	zeroBytes(r.publicKey)
	r.privateKey = nil
	r.publicKey = nil
	r.keyID = ""
}

type transportAAD struct {
	Schema          string           `json:"schema"`
	RecipientKeyID  string           `json:"recipient_key_id"`
	EphemeralPublic []byte           `json:"ephemeral_public"`
	Context         TransportContext `json:"context"`
}

type transportBundle struct {
	Schema          string           `json:"schema"`
	RecipientKeyID  string           `json:"recipient_key_id"`
	EphemeralPublic []byte           `json:"ephemeral_public"`
	Nonce           []byte           `json:"nonce"`
	Context         TransportContext `json:"context"`
	Ciphertext      []byte           `json:"ciphertext"`
}

func SealForRecipient(ctx context.Context, random io.Reader, recipientKeyID string, recipientPublicKey []byte, binding TransportContext, plaintext []byte) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, ErrCredentialTransport
	}
	if random == nil {
		random = rand.Reader
	}
	if binding.Validate() != nil || len(plaintext) == 0 || len(plaintext) > maxTransportPlaintextBytes ||
		ValidateRecipientKey(recipientKeyID, recipientPublicKey) != nil {
		return nil, ErrCredentialTransport
	}
	recipient, err := ecdh.X25519().NewPublicKey(recipientPublicKey)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	ephemeralBytes := make([]byte, credentialTransportKeySize)
	if _, err := io.ReadFull(random, ephemeralBytes); err != nil {
		zeroBytes(ephemeralBytes)
		return nil, ErrCredentialTransport
	}
	ephemeral, err := ecdh.X25519().NewPrivateKey(ephemeralBytes)
	zeroBytes(ephemeralBytes)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	ephemeralPublic := ephemeral.PublicKey().Bytes()
	shared, err := ephemeral.ECDH(recipient)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	defer zeroBytes(shared)
	aad, err := canonicalTransportAAD(recipientKeyID, ephemeralPublic, binding)
	if err != nil {
		return nil, err
	}
	key, err := deriveTransportKey(shared, aad)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		zeroBytes(nonce)
		return nil, ErrCredentialTransport
	}
	bundle := transportBundle{
		Schema: credentialTransportSchema, RecipientKeyID: recipientKeyID,
		EphemeralPublic: append([]byte(nil), ephemeralPublic...), Nonce: nonce, Context: binding,
		Ciphertext: gcm.Seal(nil, nonce, plaintext, aad),
	}
	encoded, err := json.Marshal(bundle)
	if err != nil || len(encoded) > maxTransportBundleBytes {
		zeroBytes(encoded)
		return nil, ErrCredentialTransport
	}
	return encoded, nil
}

func (r *Recipient) Open(ctx context.Context, encoded []byte, expected TransportContext) ([]byte, error) {
	if ctx == nil || ctx.Err() != nil || r == nil || expected.Validate() != nil || len(encoded) == 0 || len(encoded) > maxTransportBundleBytes {
		return nil, ErrCredentialTransport
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.destroyed {
		return nil, ErrCredentialTransport
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var bundle transportBundle
	if err := decoder.Decode(&bundle); err != nil || decoder.Decode(&struct{}{}) != io.EOF ||
		bundle.Schema != credentialTransportSchema || bundle.Context != expected || bundle.RecipientKeyID != r.keyID ||
		len(bundle.EphemeralPublic) != credentialTransportKeySize || len(bundle.Nonce) != gcmNonceSize ||
		len(bundle.Ciphertext) < 16 || len(bundle.Ciphertext) > maxTransportPlaintextBytes+16 {
		return nil, ErrCredentialTransport
	}
	privateKey, err := ecdh.X25519().NewPrivateKey(r.privateKey)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	ephemeral, err := ecdh.X25519().NewPublicKey(bundle.EphemeralPublic)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	shared, err := privateKey.ECDH(ephemeral)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	defer zeroBytes(shared)
	aad, err := canonicalTransportAAD(bundle.RecipientKeyID, bundle.EphemeralPublic, bundle.Context)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	key, err := deriveTransportKey(shared, aad)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	defer zeroBytes(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	plaintext, err := gcm.Open(nil, bundle.Nonce, bundle.Ciphertext, aad)
	if err != nil {
		return nil, ErrCredentialTransport
	}
	return plaintext, nil
}

func canonicalTransportAAD(recipientKeyID string, ephemeralPublic []byte, binding TransportContext) ([]byte, error) {
	if binding.Validate() != nil || recipientKeyID == "" || len(ephemeralPublic) != credentialTransportKeySize {
		return nil, ErrCredentialTransport
	}
	aad, err := json.Marshal(transportAAD{
		Schema: credentialTransportSchema, RecipientKeyID: recipientKeyID,
		EphemeralPublic: append([]byte(nil), ephemeralPublic...), Context: binding,
	})
	if err != nil || len(aad) > maxAADBytes*2 {
		return nil, ErrCredentialTransport
	}
	return aad, nil
}

func deriveTransportKey(shared, aad []byte) ([]byte, error) {
	salt := sha256.Sum256(aad)
	return hkdf.Key(sha256.New, shared, salt[:], credentialTransportSchema, credentialTransportKeySize)
}

func transportKeyID(publicKey []byte) string {
	digest := sha256.Sum256(publicKey)
	return "wck_" + hex.EncodeToString(digest[:16])
}

func validTransportID(value string) bool {
	if value == "" || len(value) > maxTransportIDBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

type RotationMaterial struct {
	AuthType  string
	Plaintext []byte
}

func (m RotationMaterial) String() string {
	return fmt.Sprintf("RotationMaterial{AuthType:%q Plaintext:[REDACTED]}", m.AuthType)
}

func (m RotationMaterial) GoString() string { return m.String() }

func (m RotationMaterial) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AuthType string `json:"auth_type"`
	}{m.AuthType})
}

func (m *RotationMaterial) Destroy() {
	if m == nil {
		return
	}
	zeroBytes(m.Plaintext)
	m.Plaintext = nil
}

func EncodeRotationMaterial(material RotationMaterial) ([]byte, error) {
	authCode := rotationAuthCode(material.AuthType)
	if authCode == 0 || len(material.Plaintext) == 0 || len(material.Plaintext) > maxPlaintextBytes {
		return nil, ErrCredentialTransport
	}
	payload := make([]byte, rotationPayloadHeader+len(material.Plaintext))
	copy(payload, rotationPayloadMagic[:])
	payload[6] = authCode
	binary.BigEndian.PutUint32(payload[7:11], uint32(len(material.Plaintext)))
	copy(payload[rotationPayloadHeader:], material.Plaintext)
	return payload, nil
}

func DecodeRotationMaterial(payload []byte) (RotationMaterial, error) {
	if len(payload) <= rotationPayloadHeader || len(payload) > maxPlaintextBytes+rotationPayloadHeader ||
		!bytes.Equal(payload[:6], rotationPayloadMagic[:]) {
		return RotationMaterial{}, ErrCredentialTransport
	}
	authType := rotationAuthType(payload[6])
	length := int(binary.BigEndian.Uint32(payload[7:11]))
	if authType == "" || length <= 0 || rotationPayloadHeader+length != len(payload) {
		return RotationMaterial{}, ErrCredentialTransport
	}
	return RotationMaterial{AuthType: authType, Plaintext: append([]byte(nil), payload[rotationPayloadHeader:]...)}, nil
}

func rotationAuthCode(authType string) byte {
	switch authType {
	case "oauth":
		return 1
	case "setup_token":
		return 2
	case "api_key":
		return 3
	default:
		return 0
	}
}

func rotationAuthType(code byte) string {
	switch code {
	case 1:
		return "oauth"
	case 2:
		return "setup_token"
	case 3:
		return "api_key"
	default:
		return ""
	}
}

func (c TransportContext) String() string {
	return fmt.Sprintf("TransportContext{AccountBinding:%q SlotID:%q ExecutionEpoch:%d LeaseID:%q ProxyLeaseID:%q Purpose:%q}",
		c.AccountBinding, c.SlotID, c.ExecutionEpoch, c.LeaseID, c.ProxyLeaseID, c.Purpose)
}
