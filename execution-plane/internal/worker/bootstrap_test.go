package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimebootstrap"
)

func TestProcessBootstrapRequiresPinnedPrivateSiblingPaths(t *testing.T) {
	values := processConfigEnvironment()
	values["EXECUTION_BOOTSTRAP_CA_SHA256"] = strings.Repeat("a", 64)
	values["EXECUTION_IDENTITY_DIRECTORY"] = "/run/execution/identity"
	values["EXECUTION_RUNTIME_TRUST_FILE"] = "/run/execution/runtime-ca.pem"
	getenv := func(name string) string { return values[name] }
	config, err := LoadProcessConfig(getenv)
	if err != nil || config.BootstrapConfig().Validate() != nil {
		t.Fatal("valid bootstrap config rejected", err)
	}
	for _, invalid := range []string{"bad-pin", strings.Repeat("A", 64), " " + strings.Repeat("a", 64)} {
		values["EXECUTION_BOOTSTRAP_CA_SHA256"] = invalid
		if _, err := LoadProcessConfig(getenv); err != runtimebootstrap.ErrBootstrap {
			t.Fatal("invalid pin accepted")
		}
	}
	values["EXECUTION_BOOTSTRAP_CA_SHA256"] = strings.Repeat("a", 64)
	values["EXECUTION_RUNTIME_TRUST_FILE"] = "/tmp/runtime-ca.pem"
	if _, err := LoadProcessConfig(getenv); err != runtimebootstrap.ErrBootstrap {
		t.Fatal("unbound trust path accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := RunProcess(ctx, config, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled bootstrap proceeded", err)
	}
}
