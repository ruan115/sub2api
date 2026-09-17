package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

func TestCommandHasOnlyPublicOutputAndNoImplicitImport(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.Chmod(dir, 0700)
	binding := runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot", NodeID: "node", Epoch: 1, Generation: 1}
	data, _ := json.Marshal(binding)
	for _, mode := range []string{"init", "show", "request"} {
		var out bytes.Buffer
		if err := run([]string{mode, dir}, bytes.NewReader(data), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "private_key") {
			t.Fatal("private output")
		}
		var result struct {
			Public runtimeidentity.Public `json:"public"`
			CSR    string                 `json:"csr_pem"`
		}
		if json.Unmarshal(out.Bytes(), &result) != nil || result.Public.Binding != binding {
			t.Fatal("bad public response")
		}
		if mode == "request" {
			if _, err := runtimeidentity.ValidateCSR(binding, []byte(result.CSR)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, body := range []string{`{"private_key":"do-not-echo"}`, `{"epoch":2,` + string(data[1:]), string(data) + " {}", strings.Repeat("x", 4097), "invalid"} {
		var out bytes.Buffer
		if err := run([]string{"init", dir}, strings.NewReader(body), &out); err != errCommand || out.Len() != 0 {
			t.Fatal("unsafe command accepted or echoed")
		}
	}
	for _, args := range [][]string{{}, {"import", dir}, {"rotate", dir}, {"request", dir, "secret"}} {
		var out bytes.Buffer
		if run(args, bytes.NewReader(data), &out) != errCommand || out.Len() != 0 {
			t.Fatal("unknown command accepted")
		}
	}
}

func TestCommandInstallsOnlyPublicLeafAgainstExplicitTrust(t *testing.T) {
	dir, _ := filepath.EvalSymlinks(t.TempDir())
	os.Chmod(dir, 0700)
	trustDir, _ := filepath.EvalSymlinks(t.TempDir())
	os.Chmod(trustDir, 0700)
	binding := runtimeidentity.Binding{AccountHash: strings.Repeat("a", 32), SlotID: "slot", NodeID: "node", Epoch: 7, Generation: 11}
	identity, err := runtimeidentity.Open(dir, binding, true)
	if err != nil {
		t.Fatal(err)
	}
	authority, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
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
	trustPath := filepath.Join(trustDir, "ca.pem")
	if err := os.WriteFile(trustPath, authority.CertificatePEM(), 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(binding)
	var fields map[string]any
	json.Unmarshal(data, &fields)
	fields["certificate_pem"] = string(issued.CertificatePEM)
	body, _ := json.Marshal(fields)
	var out bytes.Buffer
	if err := run([]string{"install", dir, trustPath}, bytes.NewReader(body), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "certificate_pem") {
		t.Fatal("unexpected install output")
	}
	if _, err := runtimeidentity.LoadServerTLS(dir, binding, authority.CertificatePEM()); err != nil {
		t.Fatal("installation did not load")
	}
	for _, args := range [][]string{{"install", dir}, {"install", dir, trustPath, "extra"}, {"init", dir, trustPath}, {"install", dir, filepath.Join(trustDir, "missing")}} {
		out.Reset()
		if err := run(args, bytes.NewReader(body), &out); err != errCommand || out.Len() != 0 {
			t.Fatal("invalid installation accepted")
		}
	}
	for _, invalid := range []string{`{"certificate_pem":"duplicate",` + string(body[1:]), string(body) + " {}", strings.Repeat("x", 16385), string(data)} {
		out.Reset()
		if err := run([]string{"install", dir, trustPath}, strings.NewReader(invalid), &out); err != errCommand || out.Len() != 0 {
			t.Fatal("invalid certificate envelope accepted")
		}
	}
}
