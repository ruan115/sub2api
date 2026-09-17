package runtimeidentity_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func installDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(dir, 0700) != nil {
		t.Fatal("private directory unavailable")
	}
	return dir
}

func TestCertificateInstallationPreservesLocalIdentity(t *testing.T) {
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot-a", NodeID: "node-a", Epoch: 7, Generation: 11}
	dir := installDir(t)
	identity, err := runtimeidentity.Open(dir, binding, true)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := identity.CSR()
	if err != nil {
		t.Fatal(err)
	}
	issued, err := authority.IssueRuntime(binding, csr)
	if err != nil {
		t.Fatal(err)
	}
	trust := authority.CertificatePEM()
	if _, err := runtimeidentity.LoadServerTLS(dir, binding, trust); err != runtimeidentity.ErrIdentity {
		t.Fatal("uninstalled identity has TLS")
	}
	statePath := filepath.Join(dir, "instance-identity.json")
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	// A same-binding certificate for a different locally generated key must
	// not overwrite or take over the first instance's identity.
	other, err := runtimeidentity.Open(installDir(t), binding, true)
	if err != nil {
		t.Fatal(err)
	}
	otherCSR, _ := other.CSR()
	otherIssued, err := authority.IssueRuntime(binding, otherCSR)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeidentity.InstallCertificate(dir, binding, otherIssued.CertificatePEM, trust); err != runtimeidentity.ErrIdentity {
		t.Fatal("foreign key installed")
	}
	after, _ := os.ReadFile(statePath)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected install changed identity")
	}
	if err := runtimeidentity.InstallCertificate(dir, binding, issued.CertificatePEM, trust); err != nil {
		t.Fatal(err)
	}
	installed, _ := os.ReadFile(statePath)
	info, _ := os.Stat(statePath)
	if info.Mode().Perm() != 0600 || bytes.Equal(installed, before) {
		t.Fatal("installation not committed privately")
	}
	if err := runtimeidentity.InstallCertificate(dir, binding, issued.CertificatePEM, trust); err != nil {
		t.Fatal("same certificate not idempotent")
	}
	if _, err := runtimeidentity.LoadServerTLS(dir, binding, trust); err != nil {
		t.Fatal("installed identity unavailable")
	}
	reopened, err := runtimeidentity.Open(dir, binding, false)
	if err != nil || reopened.Public() != identity.Public() {
		t.Fatal("installation changed local key or machine identity")
	}
	newCert, err := authority.IssueRuntime(binding, csr)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeidentity.InstallCertificate(dir, binding, newCert.CertificatePEM, trust); err != runtimeidentity.ErrIdentity {
		t.Fatal("unimplemented rotation silently allowed")
	}
	wrong := binding
	wrong.Generation++
	if err := runtimeidentity.InstallCertificate(dir, wrong, issued.CertificatePEM, trust); err != runtimeidentity.ErrIdentity {
		t.Fatal("wrong generation installed")
	}
	if _, err := runtimeidentity.LoadServerTLS(dir, wrong, trust); err != runtimeidentity.ErrIdentity {
		t.Fatal("wrong generation loaded")
	}
	final, _ := os.ReadFile(statePath)
	if !bytes.Equal(installed, final) {
		t.Fatal("rejected update changed installed certificate")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatal("installation left temporary secret")
	}
}

func TestConcurrentCertificateInstallAndResidualFailClosed(t *testing.T) {
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	binding := runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot-a", NodeID: "node-a", Epoch: 7, Generation: 11}
	dir := installDir(t)
	identity, err := runtimeidentity.Open(dir, binding, true)
	if err != nil {
		t.Fatal(err)
	}
	csr, _ := identity.CSR()
	issued, err := authority.IssueRuntime(binding, csr)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for n := 0; n < 8; n++ {
		group.Add(1)
		go func() {
			defer group.Done()
			// Nonblocking flock may explicitly reject contention.
			if err := runtimeidentity.InstallCertificate(dir, binding, issued.CertificatePEM, authority.CertificatePEM()); err != nil && err != runtimeidentity.ErrIdentity {
				t.Error("unexpected install error")
			}
		}()
	}
	group.Wait()
	if _, err := runtimeidentity.LoadServerTLS(dir, binding, authority.CertificatePEM()); err != nil {
		t.Fatal("concurrent installation lost certificate")
	}
	path := filepath.Join(dir, "instance-identity.json")
	before, _ := os.ReadFile(path)
	if err := os.WriteFile(filepath.Join(dir, ".identity-install-residual"), []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runtimeidentity.InstallCertificate(dir, binding, issued.CertificatePEM, authority.CertificatePEM()); err != runtimeidentity.ErrIdentity {
		t.Fatal("crashed install residual ignored")
	}
	if _, err := runtimeidentity.LoadServerTLS(dir, binding, authority.CertificatePEM()); err != runtimeidentity.ErrIdentity {
		t.Fatal("residual identity served")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("residual recovery silently changed state")
	}
}

func TestReadTrustFileRejectsUntrustedFilesystem(t *testing.T) {
	for _, name := range []string{"valid", "missing", "writable", "symlink", "hardlink", "empty", "oversize", "shared-parent"} {
		t.Run(name, func(t *testing.T) {
			dir := installDir(t)
			path := filepath.Join(dir, "ca.pem")
			if name != "missing" {
				if err := os.WriteFile(path, []byte("public trust input"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "writable":
				os.Chmod(path, 0666)
			case "symlink":
				os.Rename(path, filepath.Join(dir, "original"))
				os.Symlink(filepath.Join(dir, "original"), path)
			case "hardlink":
				os.Link(path, filepath.Join(dir, "linked"))
			case "empty":
				os.WriteFile(path, nil, 0600)
			case "oversize":
				os.WriteFile(path, bytes.Repeat([]byte("a"), 16385), 0600)
			case "shared-parent":
				os.Chmod(dir, 0777)
			}
			data, err := runtimeidentity.ReadTrustFile(path)
			if name == "valid" {
				if err != nil || string(data) != "public trust input" {
					t.Fatal("valid trust file rejected")
				}
			} else if err != runtimeidentity.ErrIdentity {
				t.Fatal("unsafe trust path accepted")
			}
		})
	}
}
