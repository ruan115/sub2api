package runtimebootstrap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func fixture(t *testing.T) (Config, *pki.Authority) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("bootstrap intentionally requires a non-root process")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(authority.CertificatePEM())
	return Config{IdentityDirectory: filepath.Join(base, "runtime", "identity"), TrustFile: filepath.Join(base, "runtime", "runtime-ca.pem"),
		TrustSHA256: hex.EncodeToString(pin[:]), Timeout: time.Second,
		Binding: runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot-a", NodeID: "node-a", Epoch: 7, Generation: 3}}, authority
}

func initializeOnly(t *testing.T, config Config) Request {
	t.Helper()
	config.Timeout = 3 * time.Millisecond
	if err := Wait(context.Background(), config); err != ErrBootstrap {
		t.Fatal("uninstalled identity became ready")
	}
	request, err := RequestIdentity(config)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func signed(t *testing.T, config Config, authority *pki.Authority, request Request) PublicBundle {
	t.Helper()
	issued, err := authority.IssueRuntime(config.Binding, request.CSRPEM)
	if err != nil {
		t.Fatal(err)
	}
	return PublicBundle{CertificatePEM: issued.CertificatePEM, CAPEM: authority.CertificatePEM()}
}

func TestWaitAndPublicInstallPreserveIndependentLocalKey(t *testing.T) {
	c, authority := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- Wait(ctx, c) }()
	var request Request
	deadline := time.Now().Add(time.Second)
	for {
		var err error
		request, err = RequestIdentity(c)
		if err == nil {
			break
		}
		if err != ErrNotReady || time.Now().After(deadline) {
			t.Fatal("request never became ready", err)
		}
		time.Sleep(time.Millisecond)
	}
	identity, err := runtimeidentity.Open(c.IdentityDirectory, c.Binding, false)
	if err != nil {
		t.Fatal(err)
	}
	before := identity.Public()
	if identity.HasCertificate() {
		t.Fatal("startup manufactured a certificate")
	}
	bundle := signed(t, c, authority, request)
	for {
		err = Install(c, bundle)
		if err != ErrNotReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("install lock did not release")
		}
		time.Sleep(time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := Install(c, bundle); err != nil {
		t.Fatal("same-certificate retry failed", err)
	}
	after, err := runtimeidentity.Open(c.IdentityDirectory, c.Binding, false)
	if err != nil || after.Public() != before || !after.HasCertificate() {
		t.Fatal("installation changed the local identity")
	}
	if err := Wait(context.Background(), c); err != nil {
		t.Fatal("installed restart failed", err)
	}
	cancelled, cancelInstalled := context.WithCancel(context.Background())
	cancelInstalled()
	if err := Wait(cancelled, c); err != ErrBootstrap {
		t.Fatal("cancelled installed restart became ready")
	}
	other, _ := fixture(t)
	other.Binding = c.Binding
	initializeOnly(t, other)
	otherIdentity, err := runtimeidentity.Open(other.IdentityDirectory, other.Binding, false)
	if err != nil || otherIdentity.Public().PublicKeySHA256 == before.PublicKeySHA256 {
		t.Fatal("instances shared a private key")
	}
	data, err := json.Marshal(request)
	if err != nil || bytes.Contains(data, []byte("PRIVATE KEY")) {
		t.Fatal("public request exposed a key")
	}
}

func TestInstallRejectsWrongPinKeyAndExistingTrustWithoutOverwrite(t *testing.T) {
	for _, name := range []string{"pin", "key", "existing-trust", "generation", "replacement-certificate"} {
		t.Run(name, func(t *testing.T) {
			c, ca := fixture(t)
			request := initializeOnly(t, c)
			bundle := signed(t, c, ca, request)
			before, _ := os.ReadFile(filepath.Join(c.IdentityDirectory, "instance-identity.json"))
			switch name {
			case "pin":
				c.TrustSHA256 = strings.Repeat("0", 64)
			case "key":
				other, _ := fixture(t)
				other.Binding = c.Binding
				bundle = signed(t, c, ca, initializeOnly(t, other))
			case "existing-trust":
				if err := os.WriteFile(c.TrustFile, []byte("wrong existing CA"), 0600); err != nil {
					t.Fatal(err)
				}
			case "generation":
				c.Binding.Generation++
			case "replacement-certificate":
				if err := Install(c, bundle); err != nil {
					t.Fatal(err)
				}
				before, _ = os.ReadFile(filepath.Join(c.IdentityDirectory, "instance-identity.json"))
				bundle = signed(t, c, ca, request)
			}
			if err := Install(c, bundle); err == nil {
				t.Fatal("unsafe public bundle accepted")
			}
			after, _ := os.ReadFile(filepath.Join(c.IdentityDirectory, "instance-identity.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("rejection changed private state")
			}
		})
	}
}

func TestBootstrapRejectsUnsafeDirectoryAndCancelledStart(t *testing.T) {
	for _, name := range []string{"symlink", "shared", "residual", "state-corrupt"} {
		t.Run(name, func(t *testing.T) {
			c, _ := fixture(t)
			initializeOnly(t, c)
			parent := filepath.Dir(c.IdentityDirectory)
			switch name {
			case "symlink":
				link := filepath.Join(filepath.Dir(parent), "linked")
				if err := os.Symlink(parent, link); err != nil {
					t.Fatal(err)
				}
				c.IdentityDirectory, c.TrustFile = filepath.Join(link, "identity"), filepath.Join(link, "runtime-ca.pem")
			case "shared":
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
			case "residual":
				if err := os.WriteFile(filepath.Join(parent, ".bootstrap-publish-abandoned"), []byte("public"), 0600); err != nil {
					t.Fatal(err)
				}
			case "state-corrupt":
				if err := os.WriteFile(filepath.Join(c.IdentityDirectory, "instance-identity.json"), []byte("broken"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := Wait(context.Background(), c); err != ErrBootstrap {
				t.Fatal("unsafe directory accepted", err)
			}
			if _, err := RequestIdentity(c); err != ErrBootstrap {
				t.Fatal("unsafe directory exported CSR", err)
			}
		})
	}
	c, _ := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, c); err != ErrBootstrap {
		t.Fatal("cancelled bootstrap succeeded")
	}
	if _, err := os.Stat(filepath.Dir(c.IdentityDirectory)); !os.IsNotExist(err) {
		t.Fatal("cancelled startup changed state")
	}
}

func TestBootstrapWireRejectsExtraFieldsAndPrivateMaterial(t *testing.T) {
	c, ca := fixture(t)
	request := initializeOnly(t, c)
	public, _ := json.Marshal(request)
	if decoded, err := DecodeRequest(public); err != nil || decoded.Binding != request.Binding {
		t.Fatal("public request rejected")
	}
	for _, invalid := range [][]byte{append(public, public...), []byte(`{"binding":{},"csr_pem":"","private_key":"secret"}`), []byte(strings.Repeat("x", MaxPublicBytes+1))} {
		if _, err := DecodeRequest(invalid); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	encoded, err := EncodeBundleArgument(signed(t, c, ca, request))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeBundleArgument(encoded); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{encoded + "=", "not base64", strings.Repeat("a", MaxEncodedBundleBytes+1)} {
		if _, err := DecodeBundleArgument(invalid); err == nil {
			t.Fatal("invalid bundle accepted")
		}
	}
	if _, err := EncodeBundleArgument(PublicBundle{CertificatePEM: []byte("-----BEGIN PRIVATE KEY-----\nsecret"), CAPEM: ca.CertificatePEM()}); err == nil {
		t.Fatal("private key entered public transport")
	}
}
