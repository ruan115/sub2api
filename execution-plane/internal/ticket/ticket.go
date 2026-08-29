package ticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const Version = 1

var (
	ErrMalformed = errors.New("malformed execution ticket")
	ErrSignature = errors.New("invalid execution ticket signature")
	ErrExpired   = errors.New("execution ticket expired")
)

type Claims struct {
	Version   int      `json:"v"`
	AccountID string   `json:"account_id"`
	SlotID    string   `json:"slot_id"`
	NodeID    string   `json:"node_id"`
	Epoch     uint64   `json:"epoch"`
	Scopes    []string `json:"scopes"`
	IssuedAt  int64    `json:"iat"`
	ExpiresAt int64    `json:"exp"`
	Nonce     string   `json:"nonce"`
}

func NewClaims(accountID, slotID, nodeID string, epoch uint64, scopes []string, now time.Time, ttl time.Duration) (Claims, error) {
	if ttl <= 0 {
		return Claims{}, errors.New("ticket ttl must be positive")
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return Claims{}, fmt.Errorf("generate nonce: %w", err)
	}
	claims := Claims{
		Version:   Version,
		AccountID: accountID,
		SlotID:    slotID,
		NodeID:    nodeID,
		Epoch:     epoch,
		Scopes:    append([]string(nil), scopes...),
		IssuedAt:  now.UTC().Unix(),
		ExpiresAt: now.UTC().Add(ttl).Unix(),
		Nonce:     hex.EncodeToString(nonce),
	}
	if err := claims.Validate(now); err != nil {
		return Claims{}, err
	}
	return claims, nil
}

func (c Claims) Validate(now time.Time) error {
	if c.Version != Version {
		return fmt.Errorf("unsupported ticket version %d", c.Version)
	}
	if c.AccountID == "" || c.SlotID == "" || c.NodeID == "" || c.Epoch == 0 || c.Nonce == "" {
		return errors.New("ticket identity claims are incomplete")
	}
	if len(c.Scopes) == 0 {
		return errors.New("ticket requires at least one scope")
	}
	if c.IssuedAt <= 0 || c.ExpiresAt <= c.IssuedAt {
		return errors.New("ticket time window is invalid")
	}
	if !now.IsZero() && now.UTC().Unix() >= c.ExpiresAt {
		return ErrExpired
	}
	return nil
}

func (c Claims) HasScope(scope string) bool {
	for _, candidate := range c.Scopes {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(scope)) == 1 {
			return true
		}
	}
	return false
}

type Issuer struct {
	privateKey ed25519.PrivateKey
}

func NewIssuer(privateKey ed25519.PrivateKey) (*Issuer, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("ticket issuer requires an Ed25519 private key")
	}
	return &Issuer{privateKey: append(ed25519.PrivateKey(nil), privateKey...)}, nil
}

func (i *Issuer) Sign(claims Claims) (string, error) {
	if err := claims.Validate(time.Time{}); err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal ticket claims: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(i.privateKey, []byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

type Verifier struct {
	publicKey ed25519.PublicKey
}

func NewVerifier(publicKey ed25519.PublicKey) (*Verifier, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("ticket verifier requires an Ed25519 public key")
	}
	return &Verifier{publicKey: append(ed25519.PublicKey(nil), publicKey...)}, nil
}

func (v *Verifier) Verify(token string, now time.Time) (Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Claims{}, ErrMalformed
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if !ed25519.Verify(v.publicKey, []byte(parts[0]), signature) {
		return Claims{}, ErrSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var claims Claims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return Claims{}, ErrMalformed
	}
	if err := claims.Validate(now); err != nil {
		return Claims{}, err
	}
	return claims, nil
}
