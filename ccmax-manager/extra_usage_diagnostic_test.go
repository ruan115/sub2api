package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sub2claude "github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
)

const extraUsageTestError = `{"type":"error","error":{"message":"Third-party apps now draw from your extra usage, not your plan limits. Add more at claude.ai/settings/usage and keep going."}}`

func TestBuildExtraUsageIdentityDiagnosticFindsCompleteIdentity(t *testing.T) {
	request, account, prepared := completeExtraUsageDiagnosticFixture(t)
	report := buildExtraUsageIdentityDiagnostic(request, gatewayKey{ID: 31, GroupID: "b"}, account, prepared, http.Header{"request-id": []string{"req_test"}}, []byte(extraUsageTestError))

	if !report.Consistency.CompleteFirstPartySet || len(report.Consistency.MissingOrMismatched) != 0 {
		t.Fatalf("identity diagnostics = %+v", report.Consistency)
	}
	if !report.Body.System.BillingBlockPresent || report.Body.System.BillingBlockIndex != 0 || !report.Body.System.ClaudeCodeIdentity {
		t.Fatalf("system diagnostics = %+v", report.Body.System)
	}
	if !report.Body.Metadata.Valid || !report.Body.Metadata.AccountUUIDMatch {
		t.Fatalf("metadata diagnostics = %+v", report.Body.Metadata)
	}
}

func TestBuildExtraUsageIdentityDiagnosticReportsMissingSignals(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	account := gatewayAccount{ID: 9, AuthType: "oauth", CredentialsJSON: `{"access_token":"secret-token","account_uuid":"11111111-1111-4111-8111-111111111111"}`}
	prepared := claudePreparedRequest{
		Body:  []byte(`{"model":"claude-opus-5","system":[{"type":"text","text":"client system"}],"messages":[{"role":"user","content":"private prompt"}],"max_tokens":64}`),
		Model: "claude-opus-5", OAuth: true, ClaudeCode: true,
		Compat: &sub2service.CCMaxCompatibilityPrepared{Headers: http.Header{
			"User-Agent":        []string{"claude-cli/2.1.219 (external, cli)"},
			"anthropic-version": []string{"2023-06-01"},
			"anthropic-beta":    []string{sub2claude.BetaClaudeCode},
			"Authorization":     []string{"Bearer secret-token"},
		}},
	}
	report := buildExtraUsageIdentityDiagnostic(request, gatewayKey{ID: 31, GroupID: "b"}, account, prepared, nil, []byte(extraUsageTestError))
	problems := strings.Join(report.Consistency.MissingOrMismatched, ",")
	for _, expected := range []string{
		"billing_system_block_missing", "required_anthropic_betas_missing",
		"metadata_user_id_invalid_or_missing", "claude_code_identity_system_block_missing",
	} {
		if !strings.Contains(problems, expected) {
			t.Fatalf("missing %q in %q", expected, problems)
		}
	}
}

func TestExtraUsageDiagnosticIsOneShotAndRedacted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(extraUsageDiagnosticDirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, extraUsageDiagnosticArm), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	request, account, prepared := completeExtraUsageDiagnosticFixture(t)
	app := &app{}
	key := gatewayKey{ID: 31, GroupID: "b"}
	app.captureExtraUsageIdentityDiagnosticOnce(request, key, account, prepared, nil, []byte(extraUsageTestError))
	app.captureExtraUsageIdentityDiagnosticOnce(request, key, account, prepared, nil, []byte(extraUsageTestError))

	files, err := filepath.Glob(filepath.Join(dir, "extra-usage-*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("capture files = %v", files)
	}
	payload, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-token", "private prompt", "private_tool", "11111111-1111-4111-8111-111111111111"} {
		if strings.Contains(string(payload), secret) {
			t.Fatalf("diagnostic leaked %q: %s", secret, payload)
		}
	}
	info, err := os.Stat(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	var report extraUsageIdentityDiagnostic
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatal(err)
	}
	if report.Headers.AuthorizationScheme != "Bearer" || report.Headers.APIKeyCredentialPresent {
		t.Fatalf("credential summary = %+v", report.Headers)
	}
}

func completeExtraUsageDiagnosticFixture(t *testing.T) (*http.Request, gatewayAccount, claudePreparedRequest) {
	t.Helper()
	const accountUUID = "11111111-1111-4111-8111-111111111111"
	metadata := sub2service.FormatMetadataUserID(strings.Repeat("a", 64), accountUUID, "22222222-2222-4222-8222-222222222222", sub2claude.CLICurrentVersion)
	body, err := json.Marshal(map[string]any{
		"model":      "claude-opus-5",
		"max_tokens": 64,
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=" + sub2claude.CLICurrentVersion + ".fingerprint; cc_entrypoint=cli;"},
			map[string]any{"type": "text", "text": claudeCodeSystemPrompt},
		},
		"metadata": map[string]any{"user_id": metadata},
		"messages": []any{map[string]any{"role": "user", "content": "private prompt"}},
		"tools":    []any{map[string]any{"name": "private_tool", "description": "private description"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{
		"User-Agent":                  []string{"claude-cli/" + sub2claude.CLICurrentVersion + " (external, cli)"},
		"anthropic-version":           []string{"2023-06-01"},
		"anthropic-beta":              []string{strings.Join(sub2claude.FullClaudeCodeMimicryBetas(), ",")},
		"X-Stainless-Lang":            []string{"js"},
		"X-Stainless-Runtime":         []string{"node"},
		"X-Stainless-Runtime-Version": []string{"v24.3.0"},
		"Authorization":               []string{"Bearer secret-token"},
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(""))
	account := gatewayAccount{ID: 9, AuthType: "oauth", CredentialsJSON: `{"access_token":"secret-token","account_uuid":"` + accountUUID + `"}`}
	prepared := claudePreparedRequest{
		Body: body, Model: "claude-opus-5", OAuth: true, Mimic: true, Stream: false,
		Compat: &sub2service.CCMaxCompatibilityPrepared{Headers: headers},
	}
	return request, account, prepared
}
