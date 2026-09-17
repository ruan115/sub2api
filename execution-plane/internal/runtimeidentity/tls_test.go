package runtimeidentity

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

type identityTLSFixture struct {
	now      time.Time
	binding  Binding
	identity *Identity
	root     *x509.Certificate
	rootKey  *ecdsa.PrivateKey
	rootPEM  []byte
	leafPEM  []byte
	node     tls.Certificate
}

func tlsTestKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func tlsTestCertificate(t *testing.T, template, parent *x509.Certificate, public any, signer *ecdsa.PrivateKey) (*x509.Certificate, []byte) {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, parent, public, signer)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newIdentityTLSFixture(t *testing.T) identityTLSFixture {
	t.Helper()
	f := identityTLSFixture{now: time.Now().UTC().Truncate(time.Second), binding: testBinding()}
	f.rootKey = tlsTestKey(t, elliptic.P256())
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic runtime test CA"},
		NotBefore: f.now.Add(-time.Hour), NotAfter: f.now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, MaxPathLen: 0}
	f.root, f.rootPEM = tlsTestCertificate(t, root, root, f.rootKey.Public(), f.rootKey)
	f.identity = &Identity{binding: f.binding, key: tlsTestKey(t, elliptic.P256())}
	_, f.leafPEM = f.runtime(t, nil)
	f.node = f.nodePair(t, f.binding.NodeID, nil)
	return f
}

func (f identityTLSFixture) runtime(t *testing.T, change func(*x509.Certificate)) (*x509.Certificate, []byte) {
	t.Helper()
	uri, _ := f.binding.URI()
	name, _ := f.binding.ServerName()
	template := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(30 * time.Minute),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{name}, URIs: []*url.URL{uri}}
	if change != nil {
		change(template)
	}
	return tlsTestCertificate(t, template, f.root, f.identity.key.Public(), f.rootKey)
}

func (f identityTLSFixture) nodePair(t *testing.T, node string, change func(*x509.Certificate)) tls.Certificate {
	t.Helper()
	key := tlsTestKey(t, elliptic.P256())
	uri := &url.URL{Scheme: "spiffe", Host: "sub2api.execution", Path: "/node/" + node}
	template := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: node},
		NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(30 * time.Minute), BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{uri}}
	if change != nil {
		change(template)
	}
	leaf, _ := tlsTestCertificate(t, template, f.root, key.Public(), f.rootKey)
	return tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
}

func TestValidateRuntimeCertificateExactPolicy(t *testing.T) {
	f := newIdentityTLSFixture(t)
	leaf, err := ValidateCertificate(f.binding, f.leafPEM, f.rootPEM, f.now)
	if err != nil || leaf == nil {
		t.Fatal("valid runtime rejected", err)
	}
	for name, change := range map[string]func(*Binding){
		"account":    func(b *Binding) { b.AccountHash = strings.Repeat("b", 32) },
		"slot":       func(b *Binding) { b.SlotID = "other-slot" },
		"node":       func(b *Binding) { b.NodeID = "other-node" },
		"epoch":      func(b *Binding) { b.Epoch++ },
		"generation": func(b *Binding) { b.Generation++ },
		"invalid":    func(b *Binding) { b.Generation = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			b := f.binding
			change(&b)
			if leaf, err := ValidateCertificate(b, f.leafPEM, f.rootPEM, f.now); leaf != nil || !errors.Is(err, ErrIdentity) {
				t.Fatal("wrong binding accepted or nonfixed error")
			}
		})
	}
}

func TestValidateRuntimeRejectsExpandedOrInvalidLeaf(t *testing.T) {
	f := newIdentityTLSFixture(t)
	for name, change := range map[string]func(*x509.Certificate){
		"expired":             func(c *x509.Certificate) { c.NotAfter = f.now },
		"future":              func(c *x509.Certificate) { c.NotBefore = f.now.Add(time.Second) },
		"ca":                  func(c *x509.Certificate) { c.IsCA = true },
		"constraints":         func(c *x509.Certificate) { c.BasicConstraintsValid = false },
		"extra-key-usage":     func(c *x509.Certificate) { c.KeyUsage |= x509.KeyUsageKeyEncipherment },
		"subject":             func(c *x509.Certificate) { c.Subject.CommonName = "not-an-identity" },
		"client-only":         func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth} },
		"any-eku":             func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageAny} },
		"missing-eku":         func(c *x509.Certificate) { c.ExtKeyUsage = nil },
		"extra-eku":           func(c *x509.Certificate) { c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageClientAuth) },
		"unknown-eku":         func(c *x509.Certificate) { c.UnknownExtKeyUsage = []asn1.ObjectIdentifier{{1, 2, 3, 4}} },
		"wrong-dns":           func(c *x509.Certificate) { c.DNSNames = []string{"wrong.execution.invalid"} },
		"missing-dns":         func(c *x509.Certificate) { c.DNSNames = nil },
		"extra-dns":           func(c *x509.Certificate) { c.DNSNames = append(c.DNSNames, "extra.invalid") },
		"missing-uri":         func(c *x509.Certificate) { c.URIs = nil },
		"extra-uri":           func(c *x509.Certificate) { c.URIs = append(c.URIs, c.URIs[0]) },
		"wrong-uri-right-dns": func(c *x509.Certificate) { c.URIs[0].Path += "/wrong" },
		"ip":                  func(c *x509.Certificate) { c.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")} },
		"email":               func(c *x509.Certificate) { c.EmailAddresses = []string{"synthetic@example.invalid"} },
		"hidden-san": func(c *x509.Certificate) {
			value, _ := asn1.Marshal([]asn1.RawValue{{Class: 2, Tag: 2, Bytes: []byte(c.DNSNames[0])}, {Class: 2, Tag: 6, Bytes: []byte(c.URIs[0].String())}, {Class: 2, Tag: 8, Bytes: []byte{42, 3, 4}}})
			c.ExtraExtensions = []pkix.Extension{{Id: sanOID, Value: value}}
		},
		"unknown-critical": func(c *x509.Certificate) {
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, raw := f.runtime(t, change)
			if leaf, err := ValidateCertificate(f.binding, raw, f.rootPEM, f.now); leaf != nil || !errors.Is(err, ErrIdentity) {
				t.Fatal("invalid leaf accepted")
			}
		})
	}
	t.Run("p384", func(t *testing.T) {
		other := f
		other.identity = &Identity{binding: f.binding, key: tlsTestKey(t, elliptic.P384())}
		_, raw := other.runtime(t, nil)
		if _, err := ValidateCertificate(f.binding, raw, f.rootPEM, f.now); !errors.Is(err, ErrIdentity) {
			t.Fatal("P384 accepted")
		}
	})
}

func TestRuntimeCertificatePEMAndTrustBounds(t *testing.T) {
	f := newIdentityTLSFixture(t)
	block, _ := pem.Decode(f.leafPEM)
	block.Headers = map[string]string{"Comment": "synthetic"}
	for name, raw := range map[string][]byte{
		"empty": nil, "junk": []byte("not a certificate"), "leading-junk": append([]byte("junk\n"), f.leafPEM...),
		"tail": append(bytes.Clone(f.leafPEM), []byte("junk")...), "two": append(bytes.Clone(f.leafPEM), f.leafPEM...),
		"headers": pem.EncodeToMemory(block), "oversized": bytes.Repeat([]byte(" "), maxCertificatePEMBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateCertificate(f.binding, raw, f.rootPEM, f.now); !errors.Is(err, ErrIdentity) {
				t.Fatal("invalid PEM accepted")
			}
		})
	}
	other := newIdentityTLSFixture(t)
	for name, root := range map[string][]byte{"wrong": other.rootPEM, "missing": nil, "leaf": f.leafPEM, "multiple": append(bytes.Clone(f.rootPEM), f.rootPEM...)} {
		t.Run("root-"+name, func(t *testing.T) {
			if _, err := ValidateCertificate(f.binding, f.leafPEM, root, f.now); !errors.Is(err, ErrIdentity) {
				t.Fatal("invalid root accepted")
			}
		})
	}
	for name, instant := range map[string]time.Time{"zero": {}, "ca-future": f.root.NotBefore.Add(-time.Second), "ca-expired": f.root.NotAfter} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateCertificate(f.binding, f.leafPEM, f.rootPEM, instant); !errors.Is(err, ErrIdentity) {
				t.Fatal("invalid time accepted")
			}
		})
	}
	// Keep the leaf valid at the selected time to isolate CA expiry checks.
	_, longLeaf := f.runtime(t, func(c *x509.Certificate) {
		c.NotBefore = f.root.NotBefore.Add(-time.Hour)
		c.NotAfter = f.root.NotAfter.Add(time.Hour)
	})
	for _, instant := range []time.Time{f.root.NotBefore.Add(-time.Second), f.root.NotAfter} {
		if _, err := ValidateCertificate(f.binding, longLeaf, f.rootPEM, instant); !errors.Is(err, ErrIdentity) {
			t.Fatal("CA validity not checked independently")
		}
	}
}

func TestRuntimeTLSConfigurationsAreStrictAndOwnTheirCredentials(t *testing.T) {
	f := newIdentityTLSFixture(t)
	server, err := f.identity.ServerTLS(f.leafPEM, f.rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ClientTLS(f.binding, f.rootPEM, f.node)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := f.binding.ServerName()
	if client.ServerName != name || server.ClientAuth != tls.RequireAndVerifyClientCert || !server.SessionTicketsDisabled {
		t.Fatal("missing identity binding")
	}
	for _, config := range []*tls.Config{server, client} {
		if config.MinVersion != tls.VersionTLS13 || config.InsecureSkipVerify || config.KeyLogWriter != nil || config.ClientSessionCache != nil || config.VerifyPeerCertificate != nil || config.VerifyConnection == nil {
			t.Fatal("TLS policy weakened")
		}
	}
	if err := client.VerifyConnection(tls.ConnectionState{}); !errors.Is(err, ErrIdentity) {
		t.Fatal("callback accepts missing default verification")
	}
	if err := server.VerifyConnection(tls.ConnectionState{}); !errors.Is(err, ErrIdentity) {
		t.Fatal("callback accepts missing default verification")
	}
	before := bytes.Clone(client.Certificates[0].Certificate[0])
	f.node.Certificate[0][0] ^= 1
	f.node.PrivateKey.(*ecdsa.PrivateKey).D.SetInt64(1)
	if !bytes.Equal(before, client.Certificates[0].Certificate[0]) || client.Certificates[0].PrivateKey.(*ecdsa.PrivateKey).D.Int64() == 1 {
		t.Fatal("node credentials were not copied")
	}
	f.identity.key.D.SetInt64(1)
	if server.Certificates[0].PrivateKey.(*ecdsa.PrivateKey).D.Int64() == 1 {
		t.Fatal("runtime key was not copied")
	}
}

func TestRuntimeTLSRejectsWrongLocalKeysAndNodeCertificates(t *testing.T) {
	f := newIdentityTLSFixture(t)
	wrong := &Identity{binding: f.binding, key: tlsTestKey(t, elliptic.P256())}
	if _, err := wrong.ServerTLS(f.leafPEM, f.rootPEM); !errors.Is(err, ErrIdentity) {
		t.Fatal("wrong runtime key accepted")
	}
	if _, err := (*Identity)(nil).ServerTLS(f.leafPEM, f.rootPEM); !errors.Is(err, ErrIdentity) {
		t.Fatal("nil identity accepted")
	}
	for name, pair := range map[string]tls.Certificate{
		"wrong-node":   f.nodePair(t, "node-wrong", nil),
		"server-eku":   f.nodePair(t, f.binding.NodeID, func(c *x509.Certificate) { c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth} }),
		"multiple-eku": f.nodePair(t, f.binding.NodeID, func(c *x509.Certificate) { c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageServerAuth) }),
		"extra-san":    f.nodePair(t, f.binding.NodeID, func(c *x509.Certificate) { c.DNSNames = []string{"extra.invalid"} }),
		"expired":      f.nodePair(t, f.binding.NodeID, func(c *x509.Certificate) { c.NotAfter = f.now.Add(-time.Second) }),
		"future":       f.nodePair(t, f.binding.NodeID, func(c *x509.Certificate) { c.NotBefore = f.now.Add(time.Hour) }),
		"missing":      {},
		"oversized":    {Certificate: [][]byte{bytes.Repeat([]byte("a"), maxCertificatePEMBytes+1)}, PrivateKey: f.node.PrivateKey},
		"wrong-key":    {Certificate: f.node.Certificate, PrivateKey: tlsTestKey(t, elliptic.P256())},
		"chain":        {Certificate: [][]byte{f.node.Certificate[0], f.root.Raw}, PrivateKey: f.node.PrivateKey},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ClientTLS(f.binding, f.rootPEM, pair); !errors.Is(err, ErrIdentity) {
				t.Fatal("invalid node certificate accepted")
			}
		})
	}
}

// Loopback TCP preserves real TLS 1.3 alert behavior. net.Pipe's unbuffered
// writes can deadlock a rejected client flight against a server alert, masking
// rejection behind the test deadline instead of exercising the verifier.
func runtimeHandshake(serverConfig, clientConfig *tls.Config) (error, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err, err
	}
	defer listener.Close()
	right, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		return err, err
	}
	left, err := listener.Accept()
	if err != nil {
		right.Close()
		return err, err
	}
	defer left.Close()
	defer right.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() { serverResult <- tls.Server(left, serverConfig).HandshakeContext(ctx) }()
	clientErr := tls.Client(right, clientConfig).HandshakeContext(ctx)
	if clientErr != nil {
		_ = left.Close()
		_ = right.Close()
	}
	serverErr := <-serverResult
	// Timeouts are not evidence that identity verification actually rejected a
	// peer. Every caller checks failures from this helper; panic makes a stalled
	// test unambiguously fail rather than counting it as a successful negative.
	if errors.Is(serverErr, context.DeadlineExceeded) || errors.Is(clientErr, context.DeadlineExceeded) {
		panic("synthetic TLS handshake reached its test deadline")
	}
	return serverErr, clientErr
}

func TestRuntimeMutualTLSRealHandshake(t *testing.T) {
	f := newIdentityTLSFixture(t)
	server, err := f.identity.ServerTLS(f.leafPEM, f.rootPEM)
	if err != nil {
		t.Fatal(err)
	}
	client, err := ClientTLS(f.binding, f.rootPEM, f.node)
	if err != nil {
		t.Fatal(err)
	}
	if se, ce := runtimeHandshake(server, client); se != nil || ce != nil {
		t.Fatalf("valid mutual TLS failed: server=%v client=%v", se, ce)
	}
	for name, mutate := range map[string]func(*Binding){"slot": func(b *Binding) { b.SlotID = "other-slot" }, "generation": func(b *Binding) { b.Generation++ }, "epoch": func(b *Binding) { b.Epoch++ }, "account": func(b *Binding) { b.AccountHash = strings.Repeat("b", 32) }} {
		t.Run("wrong-"+name, func(t *testing.T) {
			b := f.binding
			mutate(&b)
			c, err := ClientTLS(b, f.rootPEM, f.node)
			if err != nil {
				t.Fatal(err)
			}
			if _, ce := runtimeHandshake(server, c); ce == nil {
				t.Fatal("wrong runtime binding accepted")
			}
		})
	}
	t.Run("wrong-node", func(t *testing.T) {
		c := client.Clone()
		c.Certificates = []tls.Certificate{f.nodePair(t, "wrong-node", nil)}
		if se, _ := runtimeHandshake(server, c); se == nil {
			t.Fatal("wrong client node accepted")
		}
	})
	t.Run("no-client-cert", func(t *testing.T) {
		c := client.Clone()
		c.Certificates = nil
		if se, _ := runtimeHandshake(server, c); se == nil {
			t.Fatal("anonymous client accepted")
		}
	})
	for name, change := range map[string]func(*x509.Certificate){
		"extra-client-san": func(c *x509.Certificate) { c.DNSNames = []string{"extra.invalid"} },
		"mixed-client-eku": func(c *x509.Certificate) { c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageServerAuth) },
		"expired-client":   func(c *x509.Certificate) { c.NotAfter = f.now.Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			c := client.Clone()
			// Supply the malformed, signed peer directly: the real server must
			// reject it even when the caller bypasses ClientTLS's prevalidation.
			c.Certificates = []tls.Certificate{f.nodePair(t, f.binding.NodeID, change)}
			if se, _ := runtimeHandshake(server, c); se == nil {
				t.Fatal("malformed node identity accepted by handshake")
			}
		})
	}
	t.Run("wrong-root", func(t *testing.T) {
		other := newIdentityTLSFixture(t)
		_, pool, err := trustRoot(other.rootPEM, f.now)
		if err != nil {
			t.Fatal(err)
		}
		c := client.Clone()
		c.RootCAs = pool
		if _, ce := runtimeHandshake(server, c); ce == nil {
			t.Fatal("untrusted runtime accepted")
		}
	})
	t.Run("tls12", func(t *testing.T) {
		c := client.Clone()
		c.MinVersion = tls.VersionTLS12
		c.MaxVersion = tls.VersionTLS12
		if se, ce := runtimeHandshake(server, c); se == nil && ce == nil {
			t.Fatal("TLS 1.2 accepted")
		}
	})
	for name, change := range map[string]func(*x509.Certificate){
		"wrong-uri-right-dns": func(c *x509.Certificate) { c.URIs[0].Path += "/wrong" },
		"extra-san":           func(c *x509.Certificate) { c.DNSNames = append(c.DNSNames, "extra.invalid") },
		"mixed-eku":           func(c *x509.Certificate) { c.ExtKeyUsage = append(c.ExtKeyUsage, x509.ExtKeyUsageClientAuth) },
		"expired":             func(c *x509.Certificate) { c.NotAfter = f.now.Add(-time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			leaf, _ := f.runtime(t, change)
			s := server.Clone()
			s.Certificates = []tls.Certificate{{Certificate: [][]byte{leaf.Raw}, PrivateKey: f.identity.key}}
			if _, ce := runtimeHandshake(s, client); ce == nil {
				t.Fatal("malformed runtime identity accepted by handshake")
			}
		})
	}
}
