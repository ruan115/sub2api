package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemShapeDiagnosticCapturesStructureWithoutPromptText(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(extraUsageDiagnosticDirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, systemShapeDiagnosticArm), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	const secretPrompt = "private-system-prompt-do-not-store"
	prepared := claudePreparedRequest{
		Model: "claude-opus-5",
		Body:  []byte(`{"model":"claude-opus-5","system":[{"type":"text","text":"billing"},{"type":"text","text":"` + secretPrompt + `","cache_control":{"type":false,"ttl":7,"unexpected":"secret-value"}}],"messages":[]}`),
	}
	request := httptest.NewRequest("POST", "/v1/messages", nil)
	app := &app{}
	app.captureSystemShapeDiagnosticOnce(request, gatewayKey{ID: 34, GroupID: "b"}, gatewayAccount{ID: 511}, prepared, nil,
		[]byte(`{"type":"error","error":{"message":"system.1: Input does not match the expected shape."}}`))

	files, err := filepath.Glob(filepath.Join(dir, "system-shape-*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("diagnostic files=%v err=%v", files, err)
	}
	payload, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), secretPrompt) || strings.Contains(string(payload), "secret-value") {
		t.Fatalf("diagnostic leaked request content: %s", payload)
	}
	var report systemShapeDiagnostic
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.System.Blocks) != 2 || report.System.Blocks[1].TextLength != len(secretPrompt) {
		t.Fatalf("system summary=%+v", report.System)
	}
	cache := report.System.Blocks[1].CacheControl
	if cache == nil || cache.Shape != "object" || cache.ValueTypes["type"] != "boolean" || cache.ValueTypes["ttl"] != "number" {
		t.Fatalf("cache summary=%+v", cache)
	}
	if _, err := os.Stat(filepath.Join(dir, systemShapeDiagnosticArm)); !os.IsNotExist(err) {
		t.Fatalf("arm file was not consumed: %v", err)
	}
}

func TestSystemShapeDiagnosticIgnoresOther400s(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(extraUsageDiagnosticDirEnv, dir)
	arm := filepath.Join(dir, systemShapeDiagnosticArm)
	if err := os.WriteFile(arm, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	app := &app{}
	app.captureSystemShapeDiagnosticOnce(httptest.NewRequest("POST", "/v1/messages", nil), gatewayKey{}, gatewayAccount{}, claudePreparedRequest{}, nil,
		[]byte(`{"type":"error","error":{"message":"temperature is deprecated"}}`))
	if _, err := os.Stat(arm); err != nil {
		t.Fatalf("unrelated 400 consumed arm file: %v", err)
	}
}
