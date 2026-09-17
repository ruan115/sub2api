package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

func bootstrapCommandFixture(t *testing.T) (map[string]string, worker.ProcessConfig, *pki.Authority) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("worker bootstrap commands must run non-root")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ca, _, err := pki.NewEphemeralAuthority(time.Now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pin := sha256.Sum256(ca.CertificatePEM())
	values := map[string]string{
		"EXECUTION_RUNTIME_GENERATION": "3", "EXECUTION_EPOCH": "7",
		"EXECUTION_IDENTITY_DIRECTORY":    filepath.Join(base, "runtime", "identity"),
		"EXECUTION_RUNTIME_TRUST_FILE":    filepath.Join(base, "runtime", "runtime-ca.pem"),
		"EXECUTION_BOOTSTRAP_CA_SHA256":   hex.EncodeToString(pin[:]),
		"EXECUTION_TICKET_PUBLIC_KEY":     base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte("k"), 32)),
		"EXECUTION_UPSTREAM_BASE_URL":     "https://api.anthropic.com",
		"EXECUTION_EGRESS_PROXY_URL":      "http://host-agent.execution.internal:8094",
		"EXECUTION_ALLOW_FAKE_ACTIVATION": "false", "EXECUTION_LISTEN_ADDRESS": "127.0.0.1:8093",
		"EXECUTION_ACCOUNT_HASH": strings.Repeat("a", 32), "EXECUTION_SLOT_ID": "slot-command", "EXECUTION_NODE_ID": "node-command",
		"EXECUTION_IMAGE_DIGEST": "sha256:" + strings.Repeat("a", 64),
	}
	config, err := worker.LoadProcessConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := config.BootstrapConfig()
	bootstrap.Timeout = 3 * time.Millisecond
	if err := runtimebootstrap.Wait(context.Background(), bootstrap); err != runtimebootstrap.ErrBootstrap {
		t.Fatal("uninstalled command fixture became ready")
	}
	return values, config, ca
}

func TestBootstrapCommandsExportOnlyCSRAndInstallPublicBundle(t *testing.T) {
	values, config, ca := bootstrapCommandFixture(t)
	getenv := func(name string) string { return values[name] }
	var output bytes.Buffer
	if err := runBootstrap([]string{"bootstrap-request"}, getenv, &output); err != nil {
		t.Fatal(err)
	}
	request, err := runtimebootstrap.DecodeRequest(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), []byte("PRIVATE KEY")) || request.Binding != config.BootstrapConfig().Binding {
		t.Fatal("request leaked or changed identity")
	}
	issued, err := ca.IssueRuntime(request.Binding, request.CSRPEM)
	if err != nil {
		t.Fatal(err)
	}
	argument, err := runtimebootstrap.EncodeBundleArgument(runtimebootstrap.PublicBundle{CertificatePEM: issued.CertificatePEM, CAPEM: ca.CertificatePEM()})
	if err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := runBootstrap([]string{"bootstrap-install", argument}, getenv, &output); err != nil || output.String() != "bootstrap-installed\n" {
		t.Fatal("install failed", err)
	}
	if _, err := runtimeidentity.LoadServerTLS(config.IdentityDirectory, request.Binding, ca.CertificatePEM()); err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapCommandsRejectUnsafeInputWithoutOutput(t *testing.T) {
	values, _, _ := bootstrapCommandFixture(t)
	getenv := func(name string) string { return values[name] }
	for _, args := range [][]string{nil, {"sh", "-c", "echo secret"}, {"bootstrap-request", "extra"}, {"bootstrap-install"}, {"bootstrap-install", "bad"}, {"bootstrap-install", strings.Repeat("x", runtimebootstrap.MaxEncodedBundleBytes+1)}} {
		var output bytes.Buffer
		if err := runBootstrap(args, getenv, &output); err != runtimebootstrap.ErrBootstrap || output.Len() != 0 {
			t.Fatal("unsafe input accepted or logged")
		}
	}
	values["EXECUTION_BOOTSTRAP_CA_SHA256"] = "invalid-pin"
	var output bytes.Buffer
	if err := runBootstrap([]string{"bootstrap-request"}, getenv, &output); err != runtimebootstrap.ErrBootstrap || output.Len() != 0 {
		t.Fatal("invalid configuration accepted")
	}
}

type bootstrapShortWriter struct{}

func (bootstrapShortWriter) Write([]byte) (int, error) { return 0, nil }

func TestBootstrapCommandsRejectShortPublicOutputAndRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		if err := runBootstrap([]string{"bootstrap-request"}, func(string) string { return "" }, io.Discard); err != runtimebootstrap.ErrBootstrap {
			t.Fatal("root command accepted")
		}
		return
	}
	values, _, _ := bootstrapCommandFixture(t)
	if err := runBootstrap([]string{"bootstrap-request"}, func(name string) string { return values[name] }, bootstrapShortWriter{}); err != runtimebootstrap.ErrBootstrap {
		t.Fatal("short public output accepted")
	}
}
