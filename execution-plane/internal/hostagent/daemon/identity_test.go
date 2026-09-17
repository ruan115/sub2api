package daemon

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"golang.org/x/sys/unix"
)

type identityFixture struct {
	cfg            Config
	authority      *pki.Authority
	root           *x509.Certificate
	caKey, nodeKey *ecdsa.PrivateKey
}

func identityTestMust(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal("synthetic identity fixture failed")
	}
}

func identityTestFiles(t *testing.T) *identityFixture {
	t.Helper()
	authority, caPEM, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	identityTestMust(t, err)
	block, _ := pem.Decode(caPEM)
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	identityTestMust(t, err)
	rootBlock, _ := pem.Decode(authority.CertificatePEM())
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	identityTestMust(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	identityTestMust(t, err)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	identityTestMust(t, err)
	identityTestMust(t, os.Chmod(dir, 0700))
	cfg, err := configFromEnv(daemonTestEnv())
	identityTestMust(t, err)
	cfg.TrustFile = filepath.Join(dir, "ca.pem")
	cfg.NodeCertFile = filepath.Join(dir, "node.pem")
	cfg.NodeKeyFile = filepath.Join(dir, "node.key")
	f := &identityFixture{cfg: cfg, authority: authority, root: root, caKey: parsed.(*ecdsa.PrivateKey), nodeKey: key}
	identityTestMust(t, os.WriteFile(cfg.TrustFile, authority.CertificatePEM(), 0644))
	der, err := x509.MarshalPKCS8PrivateKey(key)
	identityTestMust(t, err)
	identityTestMust(t, os.WriteFile(cfg.NodeKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
	f.writeLeaf(t, nil)
	return f
}

func (f *identityFixture) writeLeaf(t *testing.T, mutate func(*x509.Certificate)) {
	t.Helper()
	u, _ := url.Parse("spiffe://sub2api.execution/node/node-1")
	now := time.Now()
	leaf := &x509.Certificate{SerialNumber: big.NewInt(100), Subject: pkix.Name{CommonName: "node-1"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{u}}
	if mutate != nil {
		mutate(leaf)
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, f.root, f.nodeKey.Public(), f.caKey)
	identityTestMust(t, err)
	identityTestMust(t, os.WriteFile(f.cfg.NodeCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644))
}

func daemonIdentityFixture(t *testing.T) (Config, *Identity) {
	t.Helper()
	f := identityTestFiles(t)
	identity, err := LoadIdentity("node-1", f.cfg)
	identityTestMust(t, err)
	return f.cfg, identity
}

func TestDaemonIdentityLoadsExactNodeAndSeparateControlTLS(t *testing.T) {
	f := identityTestFiles(t)
	identity, err := LoadIdentity("node-1", f.cfg)
	identityTestMust(t, err)
	if identity == nil || len(identity.TrustPEM) == 0 || identity.ExpiresAt != identity.NodeCertificate.Leaf.NotAfter ||
		identity.ControlTLS.InsecureSkipVerify || identity.ControlTLS.MinVersion != tls.VersionTLS13 || identity.ControlTLS.ServerName != f.cfg.ControlServerName ||
		identity.ControlTLS.VerifyConnection != nil || identity.ControlTLS.VerifyPeerCertificate != nil || identity.ControlTLS.RootCAs == nil ||
		len(identity.ControlTLS.Certificates) != 1 || identity.ControlTLS.KeyLogWriter != nil || identity.ControlTLS.ClientSessionCache != nil {
		t.Fatal("control TLS identity policy mismatch")
	}
	if !identity.ExpiresAt.After(time.Now()) {
		t.Fatal("expired loaded identity")
	}
	for _, permissions := range []os.FileMode{0400, 0600} {
		identityTestMust(t, os.Chmod(f.cfg.NodeKeyFile, permissions))
		if _, err = LoadIdentity("node-1", f.cfg); err != nil {
			t.Fatal("owner-only readable key rejected")
		}
	}
}

func TestDaemonIdentityRejectsNodeCertificatePolicy(t *testing.T) {
	for name, mutate := range map[string]func(*x509.Certificate){
		"expired": func(c *x509.Certificate) {
			c.NotBefore = time.Now().Add(-2 * time.Hour)
			c.NotAfter = time.Now().Add(-time.Hour)
		},
		"future": func(c *x509.Certificate) {
			c.NotBefore = time.Now().Add(time.Hour)
			c.NotAfter = time.Now().Add(2 * time.Hour)
		},
		"server-only": func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} },
		"both-usages": func(c *x509.Certificate) { c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageServerAuth) },
		"extra-dns":   func(c *x509.Certificate) { c.DNSNames = []string{"control.example.test"} },
		"extra-ip":    func(c *x509.Certificate) { c.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)} },
		"extra-email": func(c *x509.Certificate) { c.EmailAddresses = []string{"synthetic@example.test"} },
		"extra-uri":   func(c *x509.Certificate) { c.URIs = append(c.URIs, c.URIs[0]) },
		"wrong-node":  func(c *x509.Certificate) { c.URIs[0], _ = url.Parse("spiffe://sub2api.execution/node/node-2") },
		"wrong-role":  func(c *x509.Certificate) { c.URIs[0], _ = url.Parse("spiffe://sub2api.execution/service/node-1") },
		"keyusage":    func(c *x509.Certificate) { c.KeyUsage |= x509.KeyUsageKeyEncipherment },
		"ca-leaf":     func(c *x509.Certificate) { c.IsCA = true; c.KeyUsage |= x509.KeyUsageCertSign },
	} {
		t.Run(name, func(t *testing.T) {
			f := identityTestFiles(t)
			f.writeLeaf(t, mutate)
			if identity, err := LoadIdentity("node-1", f.cfg); err != ErrIdentity || identity != nil {
				t.Fatal("invalid leaf accepted")
			}
		})
	}
	for _, node := range []string{"node-2", "Node-1", "../node-1", "", "node_1"} {
		t.Run("caller-node", func(t *testing.T) {
			f := identityTestFiles(t)
			if _, err := LoadIdentity(node, f.cfg); err != ErrIdentity {
				t.Fatal("invalid binding accepted")
			}
		})
	}
}

func TestDaemonIdentityRejectsCAAndKeyMismatch(t *testing.T) {
	for _, mode := range []string{"wrong-ca", "future-ca", "expired-ca", "wrong-key", "p384-key", "duplicate-cert", "duplicate-ca", "duplicate-key", "key-in-cert", "cert-in-key", "prefix-key"} {
		t.Run(mode, func(t *testing.T) {
			f := identityTestFiles(t)
			switch mode {
			case "wrong-ca":
				other, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
				identityTestMust(t, err)
				identityTestMust(t, os.WriteFile(f.cfg.TrustFile, other.CertificatePEM(), 0644))
			case "future-ca", "expired-ca":
				template := *f.root
				if mode == "future-ca" {
					template.NotBefore = time.Now().Add(time.Hour)
				} else {
					template.NotAfter = time.Now().Add(-time.Hour)
				}
				der, err := x509.CreateCertificate(rand.Reader, &template, &template, f.caKey.Public(), f.caKey)
				identityTestMust(t, err)
				identityTestMust(t, os.WriteFile(f.cfg.TrustFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644))
			case "wrong-key", "p384-key":
				curve := elliptic.P256()
				if mode == "p384-key" {
					curve = elliptic.P384()
				}
				key, err := ecdsa.GenerateKey(curve, rand.Reader)
				identityTestMust(t, err)
				der, err := x509.MarshalPKCS8PrivateKey(key)
				identityTestMust(t, err)
				identityTestMust(t, os.WriteFile(f.cfg.NodeKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600))
			default:
				source, target := f.cfg.NodeCertFile, f.cfg.NodeCertFile
				if mode == "duplicate-ca" {
					source, target = f.cfg.TrustFile, f.cfg.TrustFile
				}
				if mode == "duplicate-key" || mode == "prefix-key" {
					source, target = f.cfg.NodeKeyFile, f.cfg.NodeKeyFile
				}
				if mode == "key-in-cert" {
					source, target = f.cfg.NodeKeyFile, f.cfg.NodeCertFile
				}
				if mode == "cert-in-key" {
					source, target = f.cfg.NodeCertFile, f.cfg.NodeKeyFile
				}
				data, err := os.ReadFile(source)
				identityTestMust(t, err)
				if strings.HasPrefix(mode, "duplicate") {
					data = append(data, data...)
				}
				if mode == "prefix-key" {
					data = append([]byte("untrusted preamble\n"), data...)
				}
				identityTestMust(t, os.WriteFile(target, data, 0600))
			}
			if identity, err := LoadIdentity("node-1", f.cfg); err != ErrIdentity || identity != nil {
				t.Fatal("CA/key boundary accepted")
			}
		})
	}
}

func TestDaemonIdentityFilesAreBoundedPrivateAndUnaliased(t *testing.T) {
	for _, mode := range []string{"key-worldread", "key-groupread", "key-executable", "public-writable", "cert-writable", "symlink-key", "symlink-ca", "hardlink-key", "hardlink-ca", "directory", "fifo", "oversize-key", "oversize-ca", "empty-key", "parent-symlink", "parent-writable", "same-path", "mixed-path"} {
		t.Run(mode, func(t *testing.T) {
			f := identityTestFiles(t)
			dir := filepath.Dir(f.cfg.NodeKeyFile)
			switch mode {
			case "key-worldread":
				identityTestMust(t, os.Chmod(f.cfg.NodeKeyFile, 0604))
			case "key-groupread":
				identityTestMust(t, os.Chmod(f.cfg.NodeKeyFile, 0640))
			case "key-executable":
				identityTestMust(t, os.Chmod(f.cfg.NodeKeyFile, 0700))
			case "public-writable":
				identityTestMust(t, os.Chmod(f.cfg.TrustFile, 0666))
			case "cert-writable":
				identityTestMust(t, os.Chmod(f.cfg.NodeCertFile, 0664))
			case "symlink-key", "symlink-ca":
				path := f.cfg.NodeKeyFile
				if mode == "symlink-ca" {
					path = f.cfg.TrustFile
				}
				identityTestMust(t, os.Rename(path, path+".saved"))
				identityTestMust(t, os.Symlink(path+".saved", path))
			case "hardlink-key", "hardlink-ca":
				path := f.cfg.NodeKeyFile
				if mode == "hardlink-ca" {
					path = f.cfg.TrustFile
				}
				identityTestMust(t, os.Link(path, path+".alias"))
			case "directory", "fifo":
				identityTestMust(t, os.Remove(f.cfg.NodeKeyFile))
				if mode == "directory" {
					identityTestMust(t, os.Mkdir(f.cfg.NodeKeyFile, 0700))
				} else {
					identityTestMust(t, unix.Mkfifo(f.cfg.NodeKeyFile, 0600))
				}
			case "oversize-key":
				identityTestMust(t, os.WriteFile(f.cfg.NodeKeyFile, []byte(strings.Repeat("A", (8<<10)+1)), 0600))
			case "oversize-ca":
				identityTestMust(t, os.WriteFile(f.cfg.TrustFile, []byte(strings.Repeat("A", (16<<10)+1)), 0644))
			case "empty-key":
				identityTestMust(t, os.WriteFile(f.cfg.NodeKeyFile, nil, 0600))
			case "parent-symlink":
				alias := filepath.Join(dir, "alias")
				identityTestMust(t, os.Symlink(dir, alias))
				f.cfg.NodeKeyFile = filepath.Join(alias, "node.key")
			case "parent-writable":
				identityTestMust(t, os.Chmod(dir, 0777))
				t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
			case "same-path":
				f.cfg.NodeKeyFile = f.cfg.NodeCertFile
			case "mixed-path":
				f.cfg.TrustFile, f.cfg.NodeCertFile = f.cfg.NodeCertFile, f.cfg.TrustFile
			}
			if identity, err := LoadIdentity("node-1", f.cfg); err != ErrIdentity || identity != nil {
				t.Fatal("unsafe identity input accepted")
			}
		})
	}
}

func TestDaemonIdentityDetectsReadToUseReplacement(t *testing.T) {
	for _, mode := range []string{"inode", "content", "mode", "hardlink", "parent-symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := identityTestFiles(t)
			input, err := readIdentityInput(f.cfg.NodeKeyFile, true)
			identityTestMust(t, err)
			defer input.close()
			switch mode {
			case "inode":
				identityTestMust(t, os.Rename(input.path, input.path+".old"))
				identityTestMust(t, os.WriteFile(input.path, input.data, 0600))
			case "content":
				data := append([]byte(nil), input.data...)
				data[len(data)/2] ^= 1
				identityTestMust(t, os.WriteFile(input.path, data, 0600))
				identityTestMust(t, os.Chtimes(input.path, input.info.ModTime(), input.info.ModTime()))
			case "mode":
				identityTestMust(t, os.Chmod(input.path, 0644))
			case "hardlink":
				identityTestMust(t, os.Link(input.path, input.path+".alias"))
			case "parent-symlink":
				dir := filepath.Dir(input.path)
				identityTestMust(t, os.Rename(dir, dir+".old"))
				identityTestMust(t, os.Symlink(dir+".old", dir))
				t.Cleanup(func() { _ = os.Remove(dir); _ = os.Rename(dir+".old", dir) })
			}
			if input.verify() != ErrIdentity {
				t.Fatal("changed identity snapshot accepted")
			}
		})
	}
}

func TestDaemonIdentityControlTLSUsesDefaultHostnameVerification(t *testing.T) {
	f := identityTestFiles(t)
	identity, err := LoadIdentity("node-1", f.cfg)
	identityTestMust(t, err)
	for _, name := range []string{f.cfg.ControlServerName, "wrong.example.test"} {
		t.Run(name, func(t *testing.T) {
			pair, _, err := f.authority.IssueServer([]string{name})
			identityTestMust(t, err)
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			deadline := time.Now().Add(2 * time.Second)
			_ = a.SetDeadline(deadline)
			_ = b.SetDeadline(deadline)
			server := tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: f.authority.CertificatePool()})
			client := tls.Client(b, identity.ControlTLS.Clone())
			done := make(chan error, 1)
			go func() { done <- server.HandshakeContext(context.Background()) }()
			err = client.HandshakeContext(context.Background())
			_ = b.Close()
			serverErr := <-done
			if name == f.cfg.ControlServerName {
				if err != nil || serverErr != nil {
					t.Fatal("valid control mTLS rejected")
				}
			} else if err == nil {
				t.Fatal("wrong control hostname accepted")
			}
		})
	}
}

func TestDaemonIdentityExpiryAndRedaction(t *testing.T) {
	f := identityTestFiles(t)
	template := *f.root
	template.NotAfter = time.Now().Add(10 * time.Minute).Truncate(time.Second)
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, f.caKey.Public(), f.caKey)
	identityTestMust(t, err)
	identityTestMust(t, os.WriteFile(f.cfg.TrustFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644))
	identity, err := LoadIdentity("node-1", f.cfg)
	identityTestMust(t, err)
	if !identity.ExpiresAt.Equal(template.NotAfter) {
		t.Fatal("CA expiry not respected")
	}
	raw, err := json.Marshal(identity)
	identityTestMust(t, err)
	for _, value := range []string{fmt.Sprint(identity), fmt.Sprintf("%+v", identity), fmt.Sprintf("%#v", identity), string(raw)} {
		if !strings.Contains(value, "redacted") || strings.Contains(value, "CERTIFICATE") || strings.Contains(value, "PRIVATE KEY") || strings.Contains(value, "node-1") {
			t.Fatal("identity material disclosed")
		}
	}
	if _, err = LoadIdentity("node-1", Config{}); err != ErrIdentity {
		t.Fatal("disabled identity loader accepted")
	}
}
