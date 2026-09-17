package runtimeidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"time"
)

const maxCertificatePEMBytes = 16 * 1024

// ValidateCertificate accepts one runtime leaf under one explicitly configured
// CA. Neither the leaf nor a submitted chain can add a trust anchor.
func ValidateCertificate(binding Binding, certificatePEM, trustPEM []byte, now time.Time) (*x509.Certificate, error) {
	if binding.Validate() != nil || now.IsZero() {
		return nil, ErrIdentity
	}
	_, roots, err := trustRoot(trustPEM, now)
	if err != nil {
		return nil, ErrIdentity
	}
	leaf, err := singleCertificate(certificatePEM)
	if err != nil || verifyRuntime(binding, leaf, roots, now) != nil {
		return nil, ErrIdentity
	}
	return leaf, nil
}

// ServerTLS keeps the instance's local private key local. A valid certificate
// for a different key, instance, generation or client node is never accepted.
// TLS authenticates the transport only; execution tickets remain mandatory.
func (i *Identity) ServerTLS(certificatePEM, trustPEM []byte) (*tls.Config, error) {
	if i == nil || i.binding.Validate() != nil {
		return nil, ErrIdentity
	}
	now := time.Now()
	_, roots, err := trustRoot(trustPEM, now)
	if err != nil {
		return nil, ErrIdentity
	}
	leaf, err := singleCertificate(certificatePEM)
	if err != nil || verifyRuntime(i.binding, leaf, roots, now) != nil {
		return nil, ErrIdentity
	}
	pair, err := localKeyPair(leaf, i.key)
	if err != nil {
		return nil, ErrIdentity
	}
	nodeID := i.binding.NodeID
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		Certificates:           []tls.Certificate{pair},
		ClientAuth:             tls.RequireAndVerifyClientCert,
		ClientCAs:              roots.Clone(),
		SessionTicketsDisabled: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if state.Version < tls.VersionTLS13 || len(state.PeerCertificates) != 1 || len(state.VerifiedChains) == 0 ||
				verifyNode(nodeID, state.PeerCertificates[0], roots, time.Now()) != nil {
				return ErrIdentity
			}
			return nil
		},
	}, nil
}

// ClientTLS retains Go's default chain, time, purpose and hostname verifier.
// The derived DNS name is a verifier input, not a network dial destination.
func ClientTLS(binding Binding, trustPEM []byte, nodeCertificate tls.Certificate) (*tls.Config, error) {
	if binding.Validate() != nil || len(nodeCertificate.Certificate) != 1 ||
		len(nodeCertificate.Certificate[0]) == 0 || len(nodeCertificate.Certificate[0]) > maxCertificatePEMBytes {
		return nil, ErrIdentity
	}
	now := time.Now()
	_, roots, err := trustRoot(trustPEM, now)
	if err != nil {
		return nil, ErrIdentity
	}
	leaf, err := x509.ParseCertificate(bytes.Clone(nodeCertificate.Certificate[0]))
	if err != nil || verifyNode(binding.NodeID, leaf, roots, now) != nil {
		return nil, ErrIdentity
	}
	key, ok := nodeCertificate.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, ErrIdentity
	}
	pair, err := localKeyPair(leaf, key)
	if err != nil {
		return nil, ErrIdentity
	}
	name, _ := binding.ServerName()
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    roots.Clone(), ServerName: name,
		Certificates: []tls.Certificate{pair},
		VerifyConnection: func(state tls.ConnectionState) error {
			if state.Version < tls.VersionTLS13 || len(state.PeerCertificates) != 1 || len(state.VerifiedChains) == 0 ||
				verifyRuntime(binding, state.PeerCertificates[0], roots, time.Now()) != nil {
				return ErrIdentity
			}
			return nil
		},
	}, nil
}

func singleCertificate(value []byte) (*x509.Certificate, error) {
	if len(value) == 0 || len(value) > maxCertificatePEMBytes {
		return nil, ErrIdentity
	}
	value = bytes.TrimSpace(value)
	if !bytes.HasPrefix(value, []byte("-----BEGIN CERTIFICATE-----\n")) {
		return nil, ErrIdentity
	}
	block, rest := pem.Decode(value)
	if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrIdentity
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, ErrIdentity
	}
	return leaf, nil
}

func trustRoot(value []byte, now time.Time) (*x509.Certificate, *x509.CertPool, error) {
	root, err := singleCertificate(value)
	if err != nil || now.IsZero() || root.Version != 3 || !root.IsCA || !root.BasicConstraintsValid ||
		root.KeyUsage&x509.KeyUsageCertSign == 0 || len(root.UnhandledCriticalExtensions) != 0 ||
		now.Before(root.NotBefore) || !now.Before(root.NotAfter) {
		return nil, nil, ErrIdentity
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	return root, roots, nil
}

func verifyRuntime(binding Binding, leaf *x509.Certificate, roots *x509.CertPool, now time.Time) error {
	u, err := binding.URI()
	if err != nil || leafPolicy(leaf, x509.ExtKeyUsageServerAuth, now) != nil || !bytes.Equal(leaf.RawSubject, []byte{0x30, 0}) {
		return ErrIdentity
	}
	name, _ := binding.ServerName()
	if len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != name || len(leaf.URIs) != 1 || leaf.URIs[0].String() != u.String() ||
		!exactSAN(leaf, []asn1.RawValue{{Class: 2, Tag: 2, Bytes: []byte(name)}, {Class: 2, Tag: 6, Bytes: []byte(u.String())}}) {
		return ErrIdentity
	}
	return verifyChain(leaf, roots, now, x509.ExtKeyUsageServerAuth, name)
}

func verifyNode(nodeID string, leaf *x509.Certificate, roots *x509.CertPool, now time.Time) error {
	u := "spiffe://sub2api.execution/node/" + nodeID
	if !namePattern.MatchString(nodeID) || leafPolicy(leaf, x509.ExtKeyUsageClientAuth, now) != nil ||
		len(leaf.DNSNames) != 0 || len(leaf.URIs) != 1 || leaf.URIs[0].String() != u ||
		!exactSAN(leaf, []asn1.RawValue{{Class: 2, Tag: 6, Bytes: []byte(u)}}) {
		return ErrIdentity
	}
	return verifyChain(leaf, roots, now, x509.ExtKeyUsageClientAuth, "")
}

func leafPolicy(leaf *x509.Certificate, usage x509.ExtKeyUsage, now time.Time) error {
	if leaf == nil || now.IsZero() || leaf.Version != 3 || leaf.IsCA || !leaf.BasicConstraintsValid ||
		leaf.KeyUsage != x509.KeyUsageDigitalSignature || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != usage ||
		len(leaf.UnknownExtKeyUsage) != 0 || len(leaf.UnhandledCriticalExtensions) != 0 ||
		len(leaf.IPAddresses) != 0 || len(leaf.EmailAddresses) != 0 ||
		now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return ErrIdentity
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return ErrIdentity
	}
	return nil
}

// Inspect raw SAN as well: x509 ignores some GeneralName kinds when projecting
// URI/DNS fields. Such hidden additional identities must not be accepted.
func exactSAN(leaf *x509.Certificate, names []asn1.RawValue) bool {
	want, err := asn1.Marshal(names)
	if err != nil {
		return false
	}
	count := 0
	for _, extension := range leaf.Extensions {
		if extension.Id.Equal(sanOID) {
			count++
			if !bytes.Equal(extension.Value, want) {
				return false
			}
		}
	}
	return count == 1
}

func verifyChain(leaf *x509.Certificate, roots *x509.CertPool, now time.Time, usage x509.ExtKeyUsage, name string) error {
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, DNSName: name, KeyUsages: []x509.ExtKeyUsage{usage}})
	if err != nil || len(chains) != 1 || len(chains[0]) != 2 {
		return ErrIdentity
	}
	// Explicitly check anchor expiry too. A configured CA is trusted for chain
	// construction, not exempted from this module's bounded validity policy.
	root := chains[0][1]
	if now.Before(root.NotBefore) || !now.Before(root.NotAfter) {
		return ErrIdentity
	}
	return nil
}

func localKeyPair(leaf *x509.Certificate, key *ecdsa.PrivateKey) (tls.Certificate, error) {
	if key == nil || key.Curve != elliptic.P256() || key.D == nil || key.D.Sign() <= 0 || key.D.Cmp(elliptic.P256().Params().N) >= 0 {
		return tls.Certificate{}, ErrIdentity
	}
	public, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	x, y := elliptic.P256().ScalarBaseMult(key.D.Bytes())
	if !ok || public.X.Cmp(x) != 0 || public.Y.Cmp(y) != 0 || key.X == nil || key.Y == nil || key.X.Cmp(x) != 0 || key.Y.Cmp(y) != 0 {
		return tls.Certificate{}, ErrIdentity
	}
	copyKey := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).Set(x), Y: new(big.Int).Set(y)}, D: new(big.Int).Set(key.D)}
	return tls.Certificate{Certificate: [][]byte{bytes.Clone(leaf.Raw)}, PrivateKey: copyKey, Leaf: leaf}, nil
}
