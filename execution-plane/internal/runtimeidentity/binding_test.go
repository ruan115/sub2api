package runtimeidentity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
)

func TestBindingIsCanonicalAndBounded(t *testing.T) {
	b := testBinding()
	u, err := b.URI()
	if err != nil || u.String() != "spiffe://sub2api.execution/runtime/node-a/slot-a/"+strings.Repeat("a", 32)+"/7/3" {
		t.Fatal("noncanonical URI")
	}
	for _, value := range []Binding{{}, {strings.Repeat("a", 64), "slot", "node", 1, 1}, {strings.Repeat("A", 32), "slot", "node", 1, 1}, {b.AccountHash, "../slot", "node", 1, 1}, {b.AccountHash, "slot", "node/x", 1, 1}, {b.AccountHash, "slot", "node", 0, 1}, {b.AccountHash, "slot", "node", 1, 0}} {
		if value.Validate() == nil {
			t.Fatal("unsafe binding accepted")
		}
	}
}

func TestCSRRejectsIdentityAndExtensionAmbiguity(t *testing.T) {
	b := testBinding()
	u, _ := b.URI()
	for _, mode := range []string{"valid", "subject", "dns", "email", "multiple-uri", "wrong-uri", "unknown-extension", "hidden-san", "critical", "p384", "tampered", "extra-pem", "prefix-junk", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			template := &x509.CertificateRequest{URIs: []*url.URL{u}}
			curve := elliptic.P256()
			switch mode {
			case "subject":
				template.Subject = pkix.Name{CommonName: "untrusted"}
			case "dns":
				template.DNSNames = []string{"untrusted.example"}
			case "email":
				template.EmailAddresses = []string{"untrusted@example.test"}
			case "multiple-uri":
				template.URIs = append(template.URIs, u)
			case "wrong-uri":
				other := *u
				other.Path += "/extra"
				template.URIs = []*url.URL{&other}
			case "unknown-extension":
				template.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Value: []byte{5, 0}}}
			case "hidden-san", "critical":
				names := []asn1.RawValue{{Class: 2, Tag: 6, Bytes: []byte(u.String())}}
				if mode == "hidden-san" {
					names = append(names, asn1.RawValue{Class: 2, Tag: 8, Bytes: []byte{42, 3, 4}})
				}
				value, _ := asn1.Marshal(names)
				template.ExtraExtensions = []pkix.Extension{{Id: sanOID, Value: value, Critical: mode == "critical"}}
			case "p384":
				curve = elliptic.P384()
			}
			key, _ := ecdsa.GenerateKey(curve, rand.Reader)
			der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "tampered" {
				der[len(der)-1] ^= 1
			}
			value := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
			if mode == "extra-pem" {
				value = append(value, value...)
			}
			if mode == "prefix-junk" {
				value = append([]byte("unexpected\n"), value...)
			}
			if mode == "oversize" {
				value = append(value, []byte(strings.Repeat(" ", 16384))...)
			}
			_, err = ValidateCSR(b, value)
			if (err == nil) != (mode == "valid") {
				t.Fatal("CSR contract violated")
			}
		})
	}
}
