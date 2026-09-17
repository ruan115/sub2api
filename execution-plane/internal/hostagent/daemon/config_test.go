package daemon

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func daemonTestEnv() map[string]string {
	return map[string]string{
		"RUNTIME_ENABLED": "true", "CONTROL_ADDRESS": "127.0.0.1:8443", "CONTROL_SERVER_NAME": "control.example.test",
		"DOCKER_SOCKET": "/var/run/docker.sock", "TRUST_FILE": "/var/lib/execution/ca.pem", "NODE_CERT_FILE": "/var/lib/execution/node.pem", "NODE_KEY_FILE": "/var/lib/execution/node.key",
		"TICKET_PUBLIC_KEY": base64.RawStdEncoding.EncodeToString([]byte(strings.Repeat("a", 32))), "UPSTREAM_BASE_URL": "https://api.anthropic.com",
	}
}

func configFromEnv(values map[string]string) (Config, error) {
	return Load(func(key string) string { return values[strings.TrimPrefix(key, envPrefix)] })
}

func TestDaemonConfigOffReadsOnlyFlag(t *testing.T) {
	for _, flag := range []string{"", "false"} {
		calls := 0
		cfg, err := Load(func(key string) string {
			calls++
			if key != envPrefix+"RUNTIME_ENABLED" {
				t.Fatal("disabled config read more environment")
			}
			return flag
		})
		if err != nil || cfg.Enabled || cfg != (Config{}) || calls != 1 || cfg.Validate() != nil {
			t.Fatal("disabled contract")
		}
	}
	if _, err := Load(nil); err != ErrConfig {
		t.Fatal("nil environment reader accepted")
	}
	for _, flag := range []string{"1", "TRUE", " true", "yes", "FALSE"} {
		if _, err := Load(func(string) string { return flag }); err != ErrConfig {
			t.Fatal("ambiguous flag accepted")
		}
	}
}

func TestDaemonConfigDefaultsAndExplicitBounds(t *testing.T) {
	values := daemonTestEnv()
	cfg, err := configFromEnv(values)
	if err != nil || cfg.Validate() != nil || cfg.RuntimePort != 8093 || cfg.Resources.CPUMilli != 500 || cfg.Resources.MemoryBytes != 512<<20 || cfg.Resources.PIDs != 128 || cfg.Resources.TmpfsBytes != 64<<20 ||
		cfg.Security.RunAsUser != 1000 || cfg.Security.SeccompProfile != "builtin" || cfg.Security.AppArmorProfile != "docker-default" || cfg.Network.EgressProxyEndpoint != "http://host-agent.execution.internal:8094" ||
		cfg.ReadyTimeout != 30*time.Second || cfg.StartupTimeout != 5*time.Second || cfg.ShutdownTimeout != 10*time.Second {
		t.Fatal("defaults mismatch")
	}
	for name, value := range map[string]string{"CPU_MILLI": "8000", "MEMORY_BYTES": "8589934592", "PIDS": "4096", "TMPFS_BYTES": "1073741824", "RUNTIME_PORT": "65535", "READY_TIMEOUT": "1m", "STARTUP_TIMEOUT": "30s", "SHUTDOWN_TIMEOUT": "30s"} {
		values[name] = value
	}
	if _, err = configFromEnv(values); err != nil {
		t.Fatal("upper bounds rejected")
	}
	for _, address := range []string{"10.0.0.5:443", "192.168.1.2:1", "172.16.0.1:8093", "[::1]:443", "[fd00::1]:443"} {
		values["CONTROL_ADDRESS"] = address
		if _, err = configFromEnv(values); err != nil {
			t.Fatal("private literal rejected")
		}
	}
}

func TestDaemonConfigRejectsUnsafeValues(t *testing.T) {
	credentials := &url.URL{Scheme: "https", Host: "example.test", User: url.UserPassword("synthetic-user", "synthetic-password")}
	cases := map[string][]string{
		"CONTROL_ADDRESS":     {"", "control.example.test:443", "8.8.8.8:443", "169.254.1.2:443", "0.0.0.0:1", "[::]:1", "[fe80::1%en0]:1", "[::ffff:127.0.0.1]:1", "127.0.0.1:0", "127.0.0.1:080", "127.0.0.1:65536", " 127.0.0.1:443"},
		"CONTROL_SERVER_NAME": {"", "127.0.0.1", "*.example.test", "control.example.test.", "UPPER.example", "-bad.example", "bad_name", "control:443"},
		"DOCKER_SOCKET":       {"", "unix:///var/run/docker.sock", "tcp://127.0.0.1:2375", "relative.sock", "/var//run/docker.sock", "/var/run/../docker.sock", "/", "/tmp/a\x00b", "/tmp/a\tb"},
		"TRUST_FILE":          {"", "relative", "/tmp/../trust.pem", "/var/lib/execution/node.pem", "/var/lib/execution/node.key"},
		"NODE_KEY_FILE":       {"/var/lib/execution/node.pem", "/var/run/docker.sock"},
		"TICKET_PUBLIC_KEY":   {"", "AA", strings.Repeat("a", 43) + "=", strings.Repeat("a", 42) + "_", strings.Repeat("A", 42) + "B", strings.Repeat("A", 1<<15)},
		"UPSTREAM_BASE_URL":   {"", "http://example.test", credentials.String(), "https://example.test/path", "https://example.test?", "https://example.test?q=1", "https://example.test#x", "https://example.test:", "https://example.test:0", "https://example.test:0443", "https://EXAMPLE.test", "https://example.test/%2f"},
		"EGRESS_PROXY_URL":    {"http://localhost:8094", "https://host-agent.execution.internal:8094", "http://host-agent.execution.internal:0"},
		"RUNTIME_PORT":        {"0", "65536", "08093", "-1"},
		"CPU_MILLI":           {"0", "8001", "-1", "01", "+1", "1.5", "9223372036854775808"},
		"MEMORY_BYTES":        {"0", "8589934593", "16"}, "PIDS": {"0", "4097"}, "TMPFS_BYTES": {"0", "1073741825", "536870913"},
		"READY_TIMEOUT": {"0s", "-1s", "61s", "bad", " 1s"}, "STARTUP_TIMEOUT": {"31s"}, "SHUTDOWN_TIMEOUT": {"31s"},
	}
	for name, values := range cases {
		for index, value := range values {
			t.Run(fmt.Sprintf("%s/%d", name, index), func(t *testing.T) {
				env := daemonTestEnv()
				env[name] = value
				if cfg, err := configFromEnv(env); err != ErrConfig || cfg != (Config{}) {
					t.Fatal("unsafe config accepted or leaked partial config")
				}
			})
		}
	}
}

func TestDaemonConfigRejectsDirectPolicyWeakening(t *testing.T) {
	valid, err := configFromEnv(daemonTestEnv())
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Security.RunAsUser = 0 }, func(c *Config) { c.Security.RunAsUser = 1001 }, func(c *Config) { c.Security.ReadOnlyRootFS = false },
		func(c *Config) { c.Security.NoNewPrivileges = false }, func(c *Config) { c.Security.DropAllCapabilities = false }, func(c *Config) { c.Security.SeccompProfile = "unconfined" },
		func(c *Config) { c.Security.SeccompProfile = "default" }, func(c *Config) { c.Security.AppArmorProfile = "default" }, func(c *Config) { c.Network.DenyDirectInternet = false },
		func(c *Config) { c.RuntimePort = 0 }, func(c *Config) { c.StartupTimeout = 0 }, func(c *Config) { c.ShutdownTimeout = 0 }, func(c *Config) { c.Resources.CPUMilli = -1 },
	} {
		cfg := valid
		mutate(&cfg)
		if cfg.Validate() != ErrConfig {
			t.Fatal("unsafe direct config accepted")
		}
	}
}

func TestDaemonConfigFormattingIsRedacted(t *testing.T) {
	cfg, err := configFromEnv(daemonTestEnv())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprintf("%+v", &cfg), string(raw)} {
		if !strings.Contains(value, "redacted") || strings.Contains(value, cfg.TrustFile) || strings.Contains(value, cfg.TicketPublicKey) || strings.Contains(value, cfg.ControlAddress) {
			t.Fatal("configuration disclosed")
		}
	}
}
