// Package runtimeidentity owns instance-local identity material. It neither
// authenticates enrollment callers nor authorizes execution or network access.
package runtimeidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/url"
	"regexp"
	"strconv"
)

var ErrIdentity = errors.New("instance identity rejected")

var accountPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
var sanOID = asn1.ObjectIdentifier{2, 5, 29, 17}

// AccountHash is provider.RuntimeAccountID, not a raw account name or a second
// hashing scheme. Generation is the externally assigned runtime generation.
type Binding struct {
	AccountHash string `json:"account_hash"`
	SlotID      string `json:"slot_id"`
	NodeID      string `json:"node_id"`
	Epoch       uint64 `json:"epoch"`
	Generation  uint64 `json:"generation"`
}

func (b Binding) Validate() error {
	if !accountPattern.MatchString(b.AccountHash) || !namePattern.MatchString(b.SlotID) ||
		!namePattern.MatchString(b.NodeID) || b.Epoch == 0 || b.Generation == 0 {
		return ErrIdentity
	}
	return nil
}

func (b Binding) URI() (*url.URL, error) {
	if b.Validate() != nil {
		return nil, ErrIdentity
	}
	return &url.URL{Scheme: "spiffe", Host: "sub2api.execution", Path: "/runtime/" + b.NodeID + "/" + b.SlotID + "/" + b.AccountHash + "/" + strconv.FormatUint(b.Epoch, 10) + "/" + strconv.FormatUint(b.Generation, 10)}, nil
}

// ServerName lets Go's default TLS verifier check a DNS SAN without depending
// on a container address. It complements, never replaces, exact URI identity.
func (b Binding) ServerName() (string, error) {
	u, err := b.URI()
	if err != nil {
		return "", ErrIdentity
	}
	digest := sha256.Sum256([]byte(u.String()))
	return "rt-" + hex.EncodeToString(digest[:16]) + ".execution.invalid", nil
}

// ValidateCSR does not authorize the binding. The caller must first establish
// its authority independently. No identity claims are taken from the CSR.
func ValidateCSR(b Binding, value []byte) (*x509.CertificateRequest, error) {
	u, err := b.URI()
	if err != nil || len(value) == 0 || len(value) > 16*1024 {
		return nil, ErrIdentity
	}
	value = bytes.TrimSpace(value)
	if !bytes.HasPrefix(value, []byte("-----BEGIN CERTIFICATE REQUEST-----\n")) {
		return nil, ErrIdentity
	}
	block, rest := pem.Decode(value)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrIdentity
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || csr.Version != 0 || csr.CheckSignature() != nil {
		return nil, ErrIdentity
	}
	key, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() || len(csr.URIs) != 1 || csr.URIs[0].String() != u.String() ||
		len(csr.DNSNames) != 0 || len(csr.IPAddresses) != 0 || len(csr.EmailAddresses) != 0 ||
		!bytes.Equal(csr.RawSubject, []byte{0x30, 0}) || len(csr.Extensions) != 1 ||
		!csr.Extensions[0].Id.Equal(sanOID) || csr.Extensions[0].Critical {
		return nil, ErrIdentity
	}
	// Decode the raw SAN too: x509 intentionally ignores unsupported GeneralName
	// types, which must not be smuggled into an otherwise valid request.
	want, err := asn1.Marshal([]asn1.RawValue{{Class: 2, Tag: 6, Bytes: []byte(u.String())}})
	if err != nil || !bytes.Equal(csr.Extensions[0].Value, want) {
		return nil, ErrIdentity
	}
	return csr, nil
}
