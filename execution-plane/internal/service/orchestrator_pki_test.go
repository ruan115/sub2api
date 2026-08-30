package service

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
)

func TestLoadOrchestratorPKIVerifiesChainAndFileProtection(t *testing.T) {
	now := time.Now().UTC()
	authority, caKeyPEM, err := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, issued, err := authority.IssueServer([]string{"orchestrator.test"})
	if err != nil {
		t.Fatal(err)
	}
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverTLS.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	paths := OrchestratorPKIConfig{
		CACertificateFile: filepath.Join(directory, "ca.crt"), CAPrivateKeyFile: filepath.Join(directory, "ca.key"),
		ServerCertificateFile: filepath.Join(directory, "server.crt"), ServerPrivateKeyFile: filepath.Join(directory, "server.key"),
		ServerName: "orchestrator.test", CertificateTTL: 24 * time.Hour, Now: func() time.Time { return now },
	}
	writePKITestFile(t, paths.CACertificateFile, authority.CertificatePEM(), 0o644)
	writePKITestFile(t, paths.CAPrivateKeyFile, caKeyPEM, 0o600)
	writePKITestFile(t, paths.ServerCertificateFile, issued.CertificatePEM, 0o644)
	writePKITestFile(t, paths.ServerPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}), 0o600)

	loadedAuthority, tlsConfig, err := LoadOrchestratorPKI(paths)
	if err != nil {
		t.Fatal(err)
	}
	if loadedAuthority == nil || tlsConfig.MinVersion != tls.VersionTLS13 ||
		tlsConfig.ClientAuth != tls.VerifyClientCertIfGiven || len(tlsConfig.Certificates) != 1 {
		t.Fatalf("loaded PKI is incomplete: authority=%v tls=%+v", loadedAuthority, tlsConfig)
	}

	if err := os.Chmod(paths.ServerPrivateKeyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrchestratorPKI(paths); err != ErrOrchestratorPKI {
		t.Fatalf("wide server key permissions error = %v", err)
	}
	if err := os.Chmod(paths.ServerPrivateKeyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	wrongName := paths
	wrongName.ServerName = "other.test"
	if _, _, err := LoadOrchestratorPKI(wrongName); err != ErrOrchestratorPKI {
		t.Fatalf("wrong server name error = %v", err)
	}
	symlink := filepath.Join(directory, "ca-link.key")
	if err := os.Symlink(paths.CAPrivateKeyFile, symlink); err != nil {
		t.Fatal(err)
	}
	paths.CAPrivateKeyFile = symlink
	if _, _, err := LoadOrchestratorPKI(paths); err != ErrOrchestratorPKI {
		t.Fatalf("CA key symlink error = %v", err)
	}
}

func TestLoadOrchestratorPKIRejectsUntrustedServerCertificate(t *testing.T) {
	now := time.Now().UTC()
	authority, caKeyPEM, _ := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	otherAuthority, _, _ := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	serverTLS, issued, _ := otherAuthority.IssueServer([]string{"orchestrator.test"})
	serverKeyDER, _ := x509.MarshalPKCS8PrivateKey(serverTLS.PrivateKey)
	directory := t.TempDir()
	config := OrchestratorPKIConfig{
		CACertificateFile: filepath.Join(directory, "ca.crt"), CAPrivateKeyFile: filepath.Join(directory, "ca.key"),
		ServerCertificateFile: filepath.Join(directory, "server.crt"), ServerPrivateKeyFile: filepath.Join(directory, "server.key"),
		ServerName: "orchestrator.test", CertificateTTL: 24 * time.Hour, Now: func() time.Time { return now },
	}
	writePKITestFile(t, config.CACertificateFile, authority.CertificatePEM(), 0o644)
	writePKITestFile(t, config.CAPrivateKeyFile, caKeyPEM, 0o600)
	writePKITestFile(t, config.ServerCertificateFile, issued.CertificatePEM, 0o644)
	writePKITestFile(t, config.ServerPrivateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}), 0o600)
	if _, _, err := LoadOrchestratorPKI(config); err != ErrOrchestratorPKI {
		t.Fatalf("untrusted server certificate error = %v", err)
	}
}

func writePKITestFile(t *testing.T, path string, payload []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, payload, mode); err != nil {
		t.Fatal(err)
	}
}
