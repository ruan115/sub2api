package runtimeenrollment

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

// ValidateReceipt checks bounded public storage shape. Signature/trust and
// current validity are additionally checked by Broker immediately before reply.
func ValidateReceipt(r Receipt) error {
	if credential.ValidateTransportID(r.AssignmentID) != nil || r.Binding.Validate() != nil ||
		r.PublicKeySHA256 == [32]byte{} || r.CASHA256 == [32]byte{} || r.NotBefore.IsZero() || !r.NotAfter.After(r.NotBefore) ||
		len(r.CertificatePEM) == 0 || len(r.CertificatePEM) > 16*1024 {
		return ErrRejected
	}
	value := bytes.TrimSpace(r.CertificatePEM)
	if !bytes.HasPrefix(value, []byte("-----BEGIN CERTIFICATE-----\n")) {
		return ErrRejected
	}
	block, rest := pem.Decode(value)
	if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return ErrRejected
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !leaf.NotBefore.Equal(r.NotBefore) || !leaf.NotAfter.Equal(r.NotAfter) {
		return ErrRejected
	}
	uri, _ := r.Binding.URI()
	name, _ := r.Binding.ServerName()
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != uri.String() || len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != name {
		return ErrRejected
	}
	public, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || sha256.Sum256(public) != r.PublicKeySHA256 {
		return ErrRejected
	}
	return nil
}

func SameReceiptIdentity(a, b Receipt) bool {
	return a.AssignmentID == b.AssignmentID && a.Binding == b.Binding && a.PublicKeySHA256 == b.PublicKeySHA256 && a.CASHA256 == b.CASHA256
}

func CloneReceipt(r Receipt) Receipt { r.CertificatePEM = bytes.Clone(r.CertificatePEM); return r }
