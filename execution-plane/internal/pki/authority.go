package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const nodeURIScheme = "spiffe"
const nodeURIHost = "sub2api.execution"

var serviceIdentityPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

type Authority struct {
	certificate    *x509.Certificate
	certificatePEM []byte
	signer         crypto.Signer
	certificateTTL time.Duration
	now            func() time.Time
}

type IssuedCertificate struct {
	CertificatePEM    []byte
	Certificate       *x509.Certificate
	SerialNumber      string
	CertificateSHA256 [32]byte
	PublicKeySHA256   [32]byte
}

func LoadAuthority(certificatePEM, privateKeyPEM []byte, certificateTTL time.Duration, now func() time.Time) (*Authority, error) {
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("CA certificate PEM is invalid")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("CA certificate cannot sign certificates")
	}
	signer, err := parseSigner(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(certificate.PublicKey, signer.Public()) {
		return nil, errors.New("CA private key does not match certificate")
	}
	if certificateTTL <= 0 {
		return nil, errors.New("node certificate TTL must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &Authority{
		certificate: certificate, certificatePEM: append([]byte(nil), certificatePEM...),
		signer: signer, certificateTTL: certificateTTL, now: now,
	}, nil
}

// NewEphemeralAuthority is intended for local tests and development only. A
// production orchestrator must load a protected CA key instead of generating
// one at process startup.
func NewEphemeralAuthority(now func() time.Time, certificateTTL time.Duration) (*Authority, []byte, error) {
	if now == nil {
		now = time.Now
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	current := now().UTC()
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "sub2api execution local CA"},
		NotBefore:    current.Add(-time.Minute), NotAfter: current.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		return nil, nil, err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	authority, err := LoadAuthority(certificatePEM, keyPEM, certificateTTL, now)
	return authority, keyPEM, err
}

func (a *Authority) CertificatePEM() []byte {
	return append([]byte(nil), a.certificatePEM...)
}

func (a *Authority) CertificatePool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.certificate)
	return pool
}

func (a *Authority) CertificateTTL() time.Duration {
	return a.certificateTTL
}

func (a *Authority) IssueNode(nodeID string, publicKeyPEM []byte) (IssuedCertificate, error) {
	if strings.TrimSpace(nodeID) == "" {
		return IssuedCertificate{}, errors.New("node id is required")
	}
	publicKey, publicKeyDER, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return IssuedCertificate{}, err
	}
	identity := &url.URL{Scheme: nodeURIScheme, Host: nodeURIHost, Path: "/node/" + url.PathEscape(nodeID)}
	return a.issue(pkix.Name{CommonName: nodeID}, publicKey, publicKeyDER, nil, []*url.URL{identity}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

// IssueServiceClient issues a short-lived internal client identity such as
// spiffe://sub2api.execution/service/ccmax. It is distinct from node identity
// and cannot authenticate to NodeControl as a host-agent.
func (a *Authority) IssueServiceClient(serviceID string, publicKeyPEM []byte) (IssuedCertificate, error) {
	if ValidateServiceID(serviceID) != nil {
		return IssuedCertificate{}, errors.New("service id is invalid")
	}
	publicKey, publicKeyDER, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return IssuedCertificate{}, err
	}
	identity := &url.URL{Scheme: nodeURIScheme, Host: nodeURIHost, Path: "/service/" + serviceID}
	return a.issue(pkix.Name{CommonName: serviceID}, publicKey, publicKeyDER, nil, []*url.URL{identity}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
}

func ValidateServiceID(serviceID string) error {
	if !serviceIdentityPattern.MatchString(serviceID) {
		return errors.New("service id is invalid")
	}
	return nil
}

func (a *Authority) IssueServer(serverNames []string) (tls.Certificate, IssuedCertificate, error) {
	if len(serverNames) == 0 {
		return tls.Certificate{}, IssuedCertificate{}, errors.New("server name is required")
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, IssuedCertificate{}, err
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(privateKey.Public())
	if err != nil {
		return tls.Certificate{}, IssuedCertificate{}, err
	}
	issued, err := a.issue(pkix.Name{CommonName: serverNames[0]}, privateKey.Public(), publicKeyDER, serverNames, nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	if err != nil {
		return tls.Certificate{}, IssuedCertificate{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return tls.Certificate{}, IssuedCertificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	tlsCertificate, err := tls.X509KeyPair(issued.CertificatePEM, keyPEM)
	return tlsCertificate, issued, err
}

func (a *Authority) issue(subject pkix.Name, publicKey any, publicKeyDER []byte, dnsNames []string, uris []*url.URL, usages []x509.ExtKeyUsage) (IssuedCertificate, error) {
	current := a.now().UTC()
	expiresAt := current.Add(a.certificateTTL)
	if expiresAt.After(a.certificate.NotAfter) {
		expiresAt = a.certificate.NotAfter
	}
	if !expiresAt.After(current) {
		return IssuedCertificate{}, errors.New("CA certificate has expired")
	}
	serial, err := randomSerial()
	if err != nil {
		return IssuedCertificate{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: subject,
		NotBefore: current.Add(-time.Minute), NotAfter: expiresAt,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
		BasicConstraintsValid: true, DNSNames: append([]string(nil), dnsNames...), URIs: uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.certificate, publicKey, a.signer)
	if err != nil {
		return IssuedCertificate{}, fmt.Errorf("sign execution certificate: %w", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return IssuedCertificate{}, err
	}
	return IssuedCertificate{
		CertificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Certificate:    certificate, SerialNumber: SerialString(serial),
		CertificateSHA256: sha256.Sum256(der), PublicKeySHA256: sha256.Sum256(publicKeyDER),
	}, nil
}

func NodeIDFromCertificate(certificate *x509.Certificate) (string, error) {
	if certificate == nil {
		return "", errors.New("client certificate is required")
	}
	for _, identity := range certificate.URIs {
		if identity.Scheme != nodeURIScheme || identity.Host != nodeURIHost {
			continue
		}
		const prefix = "/node/"
		if !strings.HasPrefix(identity.EscapedPath(), prefix) {
			continue
		}
		nodeID, err := url.PathUnescape(strings.TrimPrefix(identity.EscapedPath(), prefix))
		if err == nil && nodeID != "" {
			return nodeID, nil
		}
	}
	return "", errors.New("client certificate does not contain a node identity")
}

func ServiceIDFromCertificate(certificate *x509.Certificate) (string, error) {
	if certificate == nil {
		return "", errors.New("client certificate is required")
	}
	for _, identity := range certificate.URIs {
		if identity.Scheme != nodeURIScheme || identity.Host != nodeURIHost {
			continue
		}
		const prefix = "/service/"
		if !strings.HasPrefix(identity.EscapedPath(), prefix) {
			continue
		}
		serviceID, err := url.PathUnescape(strings.TrimPrefix(identity.EscapedPath(), prefix))
		if err == nil && serviceIdentityPattern.MatchString(serviceID) {
			return serviceID, nil
		}
	}
	return "", errors.New("client certificate does not contain a service identity")
}

func PublicKeyPEM(publicKey any) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

func parsePublicKey(value []byte) (any, []byte, error) {
	block, rest := pem.Decode(value)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, nil, errors.New("node public key PEM is invalid")
	}
	publicKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, nil, errors.New("node public key PEM is invalid")
	}
	switch publicKey.(type) {
	case *ecdsa.PublicKey, *rsa.PublicKey, ed25519.PublicKey:
	default:
		return nil, nil, errors.New("node public key algorithm is not supported")
	}
	return publicKey, block.Bytes, nil
}

func parseSigner(value []byte) (crypto.Signer, error) {
	block, rest := pem.Decode(value)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("CA private key PEM is invalid")
	}
	var key any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "EC PRIVATE KEY":
		key, err = x509.ParseECPrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, errors.New("CA private key PEM is invalid")
	}
	if err != nil {
		return nil, errors.New("CA private key PEM is invalid")
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, errors.New("CA private key cannot sign certificates")
	}
	return signer, nil
}

func publicKeysEqual(left, right any) bool {
	leftDER, leftErr := x509.MarshalPKIXPublicKey(left)
	rightDER, rightErr := x509.MarshalPKIXPublicKey(right)
	return leftErr == nil && rightErr == nil && string(leftDER) == string(rightDER)
}

func randomSerial() (*big.Int, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return nil, err
	}
	bytes[0] &= 0x7f
	serial := new(big.Int).SetBytes(bytes)
	if serial.Sign() == 0 {
		return nil, errors.New("generated empty certificate serial")
	}
	return serial, nil
}

func SerialString(serial *big.Int) string {
	if serial == nil {
		return ""
	}
	return hex.EncodeToString(serial.Bytes())
}
