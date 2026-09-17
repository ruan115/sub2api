package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

func TestHostEntryOffOnlyReadsFlag(t *testing.T) {
	var keys []string
	runner, err := SelectRunner(context.Background(), config.Default(config.RoleHostAgent), func(key string) string { keys = append(keys, key); return "" }, nil)
	if err != nil || runner != nil || len(keys) != 1 || keys[0] != "EXECUTION_HOST_AGENT_RUNTIME_ENABLED" {
		t.Fatal("off host entry loaded runtime")
	}
	runner, err = SelectRunner(context.Background(), config.Default(config.RoleHostAgent), func(string) string { return "TRUE" }, nil)
	if runner != nil || !errors.Is(err, ErrConfig) {
		t.Fatal("invalid flag silently fell back")
	}
}

func TestHostEntryEnabledInvokesStrictRunWithoutStartupFallback(t *testing.T) {
	health := config.Default(config.RoleHostAgent)
	// If accidentally routed to generic Run, this is still rejected before a
	// listener opens, but cannot return the daemon's fixed error sentinel.
	health.ListenAddress = "invalid-address"
	env := map[string]string{"RUNTIME_ENABLED": "true", "CONTROL_ADDRESS": "127.0.0.1:8091", "CONTROL_SERVER_NAME": "control.test",
		"DOCKER_SOCKET": "/run/docker.sock", "TRUST_FILE": "/etc/host/ca.pem", "NODE_CERT_FILE": "/etc/host/node.pem", "NODE_KEY_FILE": "/etc/host/node.key",
		"TICKET_PUBLIC_KEY": base64.RawStdEncoding.EncodeToString(make([]byte, 32)), "UPSTREAM_BASE_URL": "https://model.example"}
	runner, err := SelectRunner(context.Background(), health, func(key string) string { return env[strings.TrimPrefix(key, "EXECUTION_HOST_AGENT_")] }, nil)
	if err != nil || runner == nil || !errors.Is(runner(), ErrRuntime) {
		t.Fatal("enabled entry bypassed strict daemon validation")
	}
}
