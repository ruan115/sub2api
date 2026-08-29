package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func TestIssueNodeProducesVerifiableClientIdentity(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, err := NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	nodeKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	publicKeyPEM, err := PublicKeyPEM(nodeKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueNode("srv74", publicKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authority.CertificatePEM()) {
		t.Fatal("append authority certificate")
	}
	if _, err := issued.Certificate.Verify(x509.VerifyOptions{
		Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("verify issued node certificate: %v", err)
	}
	nodeID, err := NodeIDFromCertificate(issued.Certificate)
	if err != nil {
		t.Fatal(err)
	}
	if nodeID != "srv74" {
		t.Fatalf("node identity = %q", nodeID)
	}
	if issued.SerialNumber == "" || issued.SerialNumber != SerialString(issued.Certificate.SerialNumber) {
		t.Fatalf("non-canonical serial %q", issued.SerialNumber)
	}
	if !issued.Certificate.NotAfter.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("certificate expiry = %s", issued.Certificate.NotAfter)
	}
}

func TestIssueServerReturnsUsableTLSCertificate(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, err := NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tlsCertificate, issued, err := authority.IssueServer([]string{"orchestrator.local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tlsCertificate.Certificate) != 1 || tlsCertificate.PrivateKey == nil {
		t.Fatal("server TLS key pair is incomplete")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(authority.CertificatePEM())
	if _, err := issued.Certificate.Verify(x509.VerifyOptions{
		Roots: roots, DNSName: "orchestrator.local", CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Fatalf("verify server certificate: %v", err)
	}
}

func TestLoadAuthorityRejectsMismatchedPrivateKey(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, err := NewEphemeralAuthority(func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	if _, err := LoadAuthority(authority.CertificatePEM(), keyPEM, time.Hour, func() time.Time { return now }); err == nil {
		t.Fatal("expected mismatched CA key to be rejected")
	}
}
