package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func runtimeCertificateBinding() runtimeidentity.Binding {
	return runtimeidentity.Binding{
		AccountHash: strings.Repeat("a", 32), SlotID: "slot-a", NodeID: "node-a", Epoch: 7, Generation: 3,
	}
}

func runtimeCertificateKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func runtimeCertificateCSR(t *testing.T, binding runtimeidentity.Binding, key *ecdsa.PrivateKey, change func(*x509.CertificateRequest)) []byte {
	t.Helper()
	identity, err := binding.URI()
	if err != nil {
		t.Fatal(err)
	}
	request := &x509.CertificateRequest{URIs: []*url.URL{identity}}
	if change != nil {
		change(request)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, request, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func TestIssueRuntimeExactServerIdentityAndUsesInstancePublicKey(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, err := NewEphemeralAuthority(func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeCertificateBinding()
	key := runtimeCertificateKey(t)
	request := runtimeCertificateCSR(t, binding, key, nil)
	before := append([]byte(nil), request...)
	issued, err := authority.IssueRuntime(binding, request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, request) {
		t.Fatal("issuance mutated the caller's CSR")
	}
	leaf := issued.Certificate
	identity, _ := binding.URI()
	if len(leaf.URIs) != 1 || leaf.URIs[0].String() != identity.String() || len(leaf.Subject.Names) != 0 ||
		len(leaf.DNSNames) != 0 || len(leaf.IPAddresses) != 0 || len(leaf.EmailAddresses) != 0 {
		t.Fatal("issued certificate contains an unexpected identity")
	}
	if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageServerAuth ||
		len(leaf.UnknownExtKeyUsage) != 0 || leaf.KeyUsage != x509.KeyUsageDigitalSignature ||
		leaf.IsCA || !leaf.BasicConstraintsValid {
		t.Fatal("runtime certificate has an unexpected purpose")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if !publicKeysEqual(leaf.PublicKey, key.Public()) || issued.PublicKeySHA256 != sha256.Sum256(publicDER) ||
		issued.CertificateSHA256 != sha256.Sum256(leaf.Raw) || issued.SerialNumber != SerialString(leaf.SerialNumber) {
		t.Fatal("certificate does not preserve the instance public key and fingerprint contract")
	}
	if !leaf.NotAfter.Equal(now.Add(time.Hour)) {
		t.Fatal("certificate TTL was not enforced")
	}
	options := x509.VerifyOptions{Roots: authority.CertificatePool(), CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if _, err := leaf.Verify(options); err != nil {
		t.Fatalf("verify runtime server certificate: %v", err)
	}
	options.KeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	if _, err := leaf.Verify(options); err == nil {
		t.Fatal("runtime server certificate was accepted for client authentication")
	}
	if _, err := NodeIDFromCertificate(leaf); err == nil {
		t.Fatal("runtime certificate was accepted as a host-agent node")
	}
	if _, err := ServiceIDFromCertificate(leaf); err == nil {
		t.Fatal("runtime certificate was accepted as an internal service client")
	}
}

func TestIssueRuntimeSameAuthorityDistinctInstanceKeys(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, err := NewEphemeralAuthority(func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first := runtimeCertificateBinding()
	second := first
	second.SlotID = "slot-b"
	var certificates []IssuedCertificate
	for _, binding := range []runtimeidentity.Binding{first, second} {
		issued, err := authority.IssueRuntime(binding, runtimeCertificateCSR(t, binding, runtimeCertificateKey(t), nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := issued.Certificate.CheckSignatureFrom(authority.certificate); err != nil {
			t.Fatal("instance certificate was not signed by the common authority")
		}
		certificates = append(certificates, issued)
	}
	if certificates[0].PublicKeySHA256 == certificates[1].PublicKeySHA256 ||
		certificates[0].CertificateSHA256 == certificates[1].CertificateSHA256 ||
		certificates[0].SerialNumber == certificates[1].SerialNumber ||
		certificates[0].Certificate.URIs[0].String() == certificates[1].Certificate.URIs[0].String() {
		t.Fatal("different instances share key or certificate identity")
	}
}

func TestIssueRuntimeRejectsWrongBinding(t *testing.T) {
	authority, _, err := NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeCertificateBinding()
	request := runtimeCertificateCSR(t, binding, runtimeCertificateKey(t), nil)
	for name, mutate := range map[string]func(*runtimeidentity.Binding){
		"account":    func(b *runtimeidentity.Binding) { b.AccountHash = strings.Repeat("b", 32) },
		"slot":       func(b *runtimeidentity.Binding) { b.SlotID = "slot-b" },
		"node":       func(b *runtimeidentity.Binding) { b.NodeID = "node-b" },
		"epoch":      func(b *runtimeidentity.Binding) { b.Epoch++ },
		"generation": func(b *runtimeidentity.Binding) { b.Generation++ },
		"invalid":    func(b *runtimeidentity.Binding) { b.Generation = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			wrong := binding
			mutate(&wrong)
			if _, err := authority.IssueRuntime(wrong, request); !errors.Is(err, ErrRuntimeCertificate) {
				t.Fatal("mismatched or invalid binding was not rejected with the fixed error")
			}
		})
	}
}

func TestIssueRuntimeRejectsInvalidOrExpandedCSR(t *testing.T) {
	authority, _, err := NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeCertificateBinding()
	key := runtimeCertificateKey(t)
	valid := runtimeCertificateCSR(t, binding, key, nil)
	signatureBlock, _ := pem.Decode(valid)
	signatureBlock.Bytes[len(signatureBlock.Bytes)-1] ^= 1
	invalidSignature := pem.EncodeToMemory(signatureBlock)
	otherKey, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty":             nil,
		"not-pem":           []byte("synthetic-invalid-csr"),
		"oversized":         bytes.Repeat([]byte("a"), 64*1024),
		"invalid-signature": invalidSignature,
		"two-requests":      append(append([]byte(nil), valid...), valid...),
		"trailing-content":  append(append([]byte(nil), valid...), []byte("unexpected")...),
		"p384":              runtimeCertificateCSR(t, binding, otherKey, nil),
	}
	for name, change := range map[string]func(*x509.CertificateRequest){
		"subject":       func(c *x509.CertificateRequest) { c.Subject = pkix.Name{CommonName: "synthetic-not-authoritative"} },
		"dns":           func(c *x509.CertificateRequest) { c.DNSNames = []string{"synthetic.invalid"} },
		"ip":            func(c *x509.CertificateRequest) { c.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")} },
		"email":         func(c *x509.CertificateRequest) { c.EmailAddresses = []string{"synthetic@example.invalid"} },
		"missing-uri":   func(c *x509.CertificateRequest) { c.URIs = nil },
		"duplicate-uri": func(c *x509.CertificateRequest) { c.URIs = append(c.URIs, c.URIs[0]) },
		"unknown-extension": func(c *x509.CertificateRequest) {
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Value: []byte{5, 0}}}
		},
	} {
		cases[name] = runtimeCertificateCSR(t, binding, key, change)
	}
	for name, request := range cases {
		t.Run(name, func(t *testing.T) {
			issued, err := authority.IssueRuntime(binding, request)
			if !errors.Is(err, ErrRuntimeCertificate) || issued.Certificate != nil || len(issued.CertificatePEM) != 0 {
				t.Fatal("invalid request produced a certificate or an unexpected error")
			}
		})
	}
}

func TestIssueRuntimeRespectsCAValidityAndFreezesClock(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, err := NewEphemeralAuthority(func() time.Time { return now }, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeCertificateBinding()
	request := runtimeCertificateCSR(t, binding, runtimeCertificateKey(t), nil)
	for name, instant := range map[string]time.Time{
		"not-yet-valid": authority.certificate.NotBefore.Add(-time.Nanosecond),
		"expired":       authority.certificate.NotAfter,
		"after-expiry":  authority.certificate.NotAfter.Add(time.Second),
	} {
		t.Run(name, func(t *testing.T) {
			copy := *authority
			copy.now = func() time.Time { return instant }
			if _, err := copy.IssueRuntime(binding, request); !errors.Is(err, ErrRuntimeCertificate) {
				t.Fatal("CA outside validity window was allowed to sign")
			}
		})
	}
	nearExpiry := *authority
	nearExpiry.now = func() time.Time { return authority.certificate.NotAfter.Add(-time.Minute) }
	issued, err := nearExpiry.IssueRuntime(binding, request)
	if err != nil || !issued.Certificate.NotAfter.Equal(authority.certificate.NotAfter) {
		t.Fatal("runtime certificate outlived its CA or valid shortened issuance failed")
	}
	calls := 0
	frozen := *authority
	frozen.now = func() time.Time {
		calls++
		if calls > 1 {
			return authority.certificate.NotBefore.Add(-time.Hour)
		}
		return now
	}
	issued, err = frozen.IssueRuntime(binding, request)
	if err != nil || calls != 1 || !issued.Certificate.NotAfter.Equal(now.Add(time.Hour)) {
		t.Fatal("issuance did not use the same clock observation as CA validation")
	}
	var missing *Authority
	if _, err := missing.IssueRuntime(binding, request); !errors.Is(err, ErrRuntimeCertificate) {
		t.Fatal("missing authority was not rejected")
	}
}
