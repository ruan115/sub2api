package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestLoadOrchestratorRuntimeDefaultsDisabled(t *testing.T) {
	config, err := LoadOrchestratorRuntime(func(string) string { return "" })
	if err != nil || config.Enabled || config.RPCListenAddress != defaultOrchestratorRPCAddress {
		t.Fatalf("disabled config = %+v, %v", config, err)
	}
}

func TestLoadOrchestratorRuntimeValidatesProductionBoundaryAndRedactsDSN(t *testing.T) {
	env := validOrchestratorRuntimeEnv()
	config, err := LoadOrchestratorRuntime(func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if !config.Enabled || config.ProvisioningBatchSize != 200 || config.KMS.CVMRoleName != "sub2api-execution-role" {
		t.Fatalf("loaded config = %+v", config)
	}
	for _, serialized := range []string{config.String(), fmt.Sprintf("%+v", config), string(mustRuntimeConfigJSON(t, config))} {
		if strings.Contains(serialized, "mysql-secret") {
			t.Fatalf("runtime config leaked MySQL DSN: %s", serialized)
		}
	}
}

func TestLoadOrchestratorRuntimeRejectsUnsafeProductionValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"invalid enabled", func(env map[string]string) { env["EXECUTION_ORCHESTRATOR_RUNTIME_ENABLED"] = "1" }},
		{"plaintext mysql", func(env map[string]string) {
			env["EXECUTION_MYSQL_DSN"] = "user:mysql-secret@tcp(127.0.0.1:3306)/worker_runtime?parseTime=true&loc=UTC"
		}},
		{"mysql skip verify", func(env map[string]string) {
			env["EXECUTION_MYSQL_DSN"] = "user:mysql-secret@tcp(127.0.0.1:3306)/worker_runtime?parseTime=true&loc=UTC&tls=skip-verify"
		}},
		{"relative key", func(env map[string]string) { env["EXECUTION_CA_KEY_FILE"] = "ca.key" }},
		{"claim over intent", func(env map[string]string) { env["EXECUTION_ONBOARDING_CLAIM_TTL"] = "31m" }},
		{"intent below CCMAX commit window", func(env map[string]string) {
			env["EXECUTION_ONBOARDING_INTENT_TTL"] = "4m59s"
		}},
		{"batch unbounded", func(env map[string]string) { env["EXECUTION_ONBOARDING_BATCH_SIZE"] = "1001" }},
		{"invalid service id", func(env map[string]string) { env["EXECUTION_ONBOARDING_INTAKE_SERVICE_ID"] = "CCMAX/admin" }},
		{"invalid server name", func(env map[string]string) { env["EXECUTION_SERVER_NAME"] = "https://orchestrator.test" }},
		{"missing role", func(env map[string]string) { env["EXECUTION_KMS_CVM_ROLE_NAME"] = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := validOrchestratorRuntimeEnv()
			test.mutate(env)
			if _, err := LoadOrchestratorRuntime(func(key string) string { return env[key] }); err == nil {
				t.Fatal("unsafe orchestrator configuration was accepted")
			}
		})
	}
}

func validOrchestratorRuntimeEnv() map[string]string {
	return map[string]string{
		"EXECUTION_ORCHESTRATOR_RUNTIME_ENABLED":     "true",
		"EXECUTION_MYSQL_DSN":                        "execution:mysql-secret@tcp(127.0.0.1:3306)/worker_runtime?parseTime=true&loc=UTC&tls=true",
		"EXECUTION_CA_CERT_FILE":                     "/etc/sub2api/execution/ca.crt",
		"EXECUTION_CA_KEY_FILE":                      "/etc/sub2api/execution/ca.key",
		"EXECUTION_SERVER_CERT_FILE":                 "/etc/sub2api/execution/server.crt",
		"EXECUTION_SERVER_KEY_FILE":                  "/etc/sub2api/execution/server.key",
		"EXECUTION_SERVER_NAME":                      "orchestrator.test",
		"EXECUTION_ROTATION_RECIPIENT_ENVELOPE_FILE": "/etc/sub2api/execution/rotation-envelope.json",
		"EXECUTION_KMS_REGION":                       "ap-guangzhou",
		"EXECUTION_KMS_KEY_ID":                       "kms-production-key",
		"EXECUTION_KMS_KEY_VERSION":                  "current",
		"EXECUTION_KMS_CVM_ROLE_NAME":                "sub2api-execution-role",
		"EXECUTION_ONBOARDING_BATCH_SIZE":            "200",
	}
}

func mustRuntimeConfigJSON(t *testing.T, config OrchestratorRuntimeConfig) []byte {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
