package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRuntimeEnrollmentConfigurationFormattingRedactsRawAddress(t *testing.T) {
	address := &url.URL{Scheme: "redis", Host: "redis.internal:6379", User: url.UserPassword("username", "private-secret")}
	c := RuntimeEnrollmentConfig{Enabled: true, LeaseRedisAddr: address.String(), Timeout: time.Second}
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{c.String(), fmt.Sprintf("%+v", c), fmt.Sprintf("%#v", c), string(payload)} {
		if strings.Contains(text, "private-secret") || strings.Contains(text, "redis.internal") || strings.Contains(text, "username") {
			t.Fatal("raw Redis address leaked")
		}
	}
}

func TestRuntimeEnrollmentDefaultsOffAndIgnoresDisabledDependencies(t *testing.T) {
	for _, flag := range []string{"", "false"} {
		c, err := LoadRuntimeEnrollment(func(key string) string {
			if key == "EXECUTION_RUNTIME_ENROLLMENT_ENABLED" {
				return flag
			}
			t.Fatalf("disabled enrollment read %s", key)
			return "invalid"
		})
		if err != nil || c.Enabled || c.LeaseRedisAddr != "" || c.Timeout != 2*time.Second {
			t.Fatalf("off configuration: %+v, %v", c, err)
		}
	}
	if _, err := LoadRuntimeEnrollment(nil); err == nil {
		t.Fatal("nil environment accepted")
	}
}

func TestRuntimeEnrollmentExplicitIndependentAddress(t *testing.T) {
	env := validOrchestratorRuntimeEnv()
	env["EXECUTION_RUNTIME_ENROLLMENT_ENABLED"] = "true"
	if _, err := LoadOrchestratorRuntime(func(key string) string { return env[key] }); err == nil {
		t.Fatal("route Redis silently substituted for lease Redis")
	}
	env["EXECUTION_LEASE_REDIS_ADDR"] = "127.0.0.1:6380"
	env["EXECUTION_RUNTIME_ENROLLMENT_TIMEOUT"] = "3s"
	c, err := LoadOrchestratorRuntime(func(key string) string { return env[key] })
	if err != nil || !c.RuntimeEnrollment.Enabled || c.RuntimeEnrollment.LeaseRedisAddr != "127.0.0.1:6380" || c.RuntimeEnrollment.Timeout != 3*time.Second {
		t.Fatalf("explicit configuration: %+v, %v", c.RuntimeEnrollment, err)
	}
	if RuntimeLeaseKeyPrefix != "execution:lease:v1:" {
		t.Fatal("unexpected execution lease namespace")
	}
}

func TestRuntimeEnrollmentRejectsUnsafeConfiguration(t *testing.T) {
	credentialAddress := &url.URL{Scheme: "redis", Host: "localhost:6379", User: url.UserPassword("user", "secret")}
	for index, address := range []string{"", "localhost", ":6379", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:06379", "127.0.0.1:http", "0.0.0.0:6379", "8.8.8.8:6379", "[::]:6379", "[fe80::1%en0]:6379", credentialAddress.String(), credentialAddress.User.String() + "@localhost:6379", "localhost:6379/0", " localhost:6379", "localhost:6379\n", "bad_host:6379", "-bad:6379", "bad..host:6379"} {
		t.Run(fmt.Sprintf("address-%d", index), func(t *testing.T) {
			c := RuntimeEnrollmentConfig{Enabled: true, LeaseRedisAddr: address, Timeout: time.Second}
			if err := c.Validate(); err == nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("unsafe address accepted or leaked: %v", err)
			}
		})
	}
	for _, address := range []string{"127.0.0.1:6379", "10.0.1.2:6379", "[::1]:6379", "[fd00::1]:6379", "redis.internal:6379", "localhost:6379"} {
		if err := (RuntimeEnrollmentConfig{Enabled: true, LeaseRedisAddr: address, Timeout: time.Second}).Validate(); err != nil {
			t.Fatalf("valid address rejected: %s", address)
		}
	}
	for _, duration := range []time.Duration{0, -time.Second, 5*time.Second + 1} {
		if err := (RuntimeEnrollmentConfig{Enabled: true, LeaseRedisAddr: "127.0.0.1:6379", Timeout: duration}).Validate(); err == nil {
			t.Fatal("unbounded timeout accepted")
		}
	}
	for _, env := range []map[string]string{
		{"EXECUTION_RUNTIME_ENROLLMENT_ENABLED": "1"},
		{"EXECUTION_RUNTIME_ENROLLMENT_ENABLED": "true", "EXECUTION_LEASE_REDIS_ADDR": "127.0.0.1:6379", "EXECUTION_RUNTIME_ENROLLMENT_TIMEOUT": "private-secret"},
	} {
		if _, err := LoadRuntimeEnrollment(func(key string) string { return env[key] }); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("invalid environment accepted or leaked: %v", err)
		}
	}
}
