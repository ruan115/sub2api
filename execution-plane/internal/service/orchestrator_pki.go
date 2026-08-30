package service

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
)

const maxOrchestratorPEMFileBytes = 1 << 20

var ErrOrchestratorPKI = errors.New("orchestrator PKI could not be loaded")

type OrchestratorPKIConfig struct {
	CACertificateFile     string
	CAPrivateKeyFile      string
	ServerCertificateFile string
	ServerPrivateKeyFile  string
	ServerName            string
	CertificateTTL        time.Duration
	Now                   func() time.Time
}

// LoadOrchestratorPKI rejects symlinks and mutable key/certificate files,
// verifies the server certificate against the same CA used for node/service
// clients, and returns the only TLS policy accepted by RunOrchestratorRPC.
func LoadOrchestratorPKI(config OrchestratorPKIConfig) (*pki.Authority, *tls.Config, error) {
	if config.CertificateTTL <= 0 || config.ServerName == "" {
		return nil, nil, ErrOrchestratorPKI
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	caCertificatePEM, err := readProtectedRegularFile(config.CACertificateFile, maxOrchestratorPEMFileBytes, false)
	if err != nil {
		return nil, nil, ErrOrchestratorPKI
	}
	defer eraseLoaderBytes(caCertificatePEM)
	caPrivateKeyPEM, err := readProtectedRegularFile(config.CAPrivateKeyFile, maxOrchestratorPEMFileBytes, true)
	if err != nil {
		return nil, nil, ErrOrchestratorPKI
	}
	defer eraseLoaderBytes(caPrivateKeyPEM)
	serverCertificatePEM, err := readProtectedRegularFile(config.ServerCertificateFile, maxOrchestratorPEMFileBytes, false)
	if err != nil {
		return nil, nil, ErrOrchestratorPKI
	}
	defer eraseLoaderBytes(serverCertificatePEM)
	serverPrivateKeyPEM, err := readProtectedRegularFile(config.ServerPrivateKeyFile, maxOrchestratorPEMFileBytes, true)
	if err != nil {
		return nil, nil, ErrOrchestratorPKI
	}
	defer eraseLoaderBytes(serverPrivateKeyPEM)

	authority, err := pki.LoadAuthority(caCertificatePEM, caPrivateKeyPEM, config.CertificateTTL, config.Now)
	if err != nil {
		return nil, nil, ErrOrchestratorPKI
	}
	serverCertificate, err := tls.X509KeyPair(serverCertificatePEM, serverPrivateKeyPEM)
	if err != nil || len(serverCertificate.Certificate) == 0 {
		return nil, nil, ErrOrchestratorPKI
	}
	leaf, err := x509.ParseCertificate(serverCertificate.Certificate[0])
	if err != nil || len(leaf.DNSNames) == 0 && len(leaf.IPAddresses) == 0 {
		return nil, nil, ErrOrchestratorPKI
	}
	intermediates := x509.NewCertPool()
	for _, raw := range serverCertificate.Certificate[1:] {
		certificate, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, nil, ErrOrchestratorPKI
		}
		intermediates.AddCert(certificate)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: authority.CertificatePool(), Intermediates: intermediates,
		CurrentTime: config.Now().UTC(), DNSName: config.ServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, nil, ErrOrchestratorPKI
	}
	serverCertificate.Leaf = leaf
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: authority.CertificatePool(),
	}
	if validateOrchestratorTLS(tlsConfig) != nil {
		return nil, nil, ErrOrchestratorPKI
	}
	return authority, tlsConfig, nil
}
