package service

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tidwall/gjson"
)

func TestSyncBillingHeaderVersion(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		userAgent string
		wantSub   string // substring expected in result
		unchanged bool   // expect body to remain the same
	}{
		{
			name:      "replaces cc_version and stale fingerprint",
			body:      `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli; cch=00000;"},{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],"messages":[]}`,
			userAgent: "claude-cli/2.1.22 (external, cli)",
			wantSub:   "cc_version=2.1.22.",
		},
		{
			name:      "no billing header in system",
			body:      `{"system":[{"type":"text","text":"You are Claude Code."}],"messages":[]}`,
			userAgent: "claude-cli/2.1.22",
			unchanged: true,
		},
		{
			name:      "no system field",
			body:      `{"messages":[]}`,
			userAgent: "claude-cli/2.1.22",
			unchanged: true,
		},
		{
			name:      "user-agent without version",
			body:      `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81; cc_entrypoint=cli; cch=00000;"}],"messages":[]}`,
			userAgent: "Mozilla/5.0",
			unchanged: true,
		},
		{
			name:      "empty user-agent",
			body:      `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81; cc_entrypoint=cli; cch=00000;"}],"messages":[]}`,
			userAgent: "",
			unchanged: true,
		},
		{
			name:      "version already matches but fingerprint is missing",
			body:      `{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.22; cc_entrypoint=cli; cch=00000;"}],"messages":[]}`,
			userAgent: "claude-cli/2.1.22",
			wantSub:   "cc_version=2.1.22.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := syncBillingHeaderVersion([]byte(tt.body), tt.userAgent)
			if tt.unchanged {
				assert.Equal(t, tt.body, string(result), "body should remain unchanged")
			} else {
				assert.Contains(t, string(result), tt.wantSub)
				// Ensure old semver is gone
				assert.NotContains(t, string(result), "cc_version=2.1.81")
			}
		})
	}
}

func TestSyncBillingHeaderVersionRecomputesFingerprint(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli;"}],"messages":[{"role":"user","content":"hello from the billing fingerprint test"}]}`)
	version := "2.1.22"

	result := syncBillingHeaderVersion(body, "claude-cli/"+version+" (external, cli)")
	wantFingerprint := computeClaudeCodeFingerprint(body, version)

	assert.Contains(t, string(result), "cc_version="+version+"."+wantFingerprint+";")
	assert.NotContains(t, string(result), "cc_version="+version+".df2;")
}

func TestSyncRequestBillingHeaderVersionUsesFinalWireUserAgent(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli;"}],"messages":[{"role":"user","content":"hello from the final wire request"}]}`)
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	assert.NoError(t, err)
	req.Header.Set("User-Agent", "claude-cli/2.1.22 (external, cli)")

	updated := syncRequestBillingHeaderVersion(req, body)
	want := "cc_version=2.1.22." + computeClaudeCodeFingerprint(body, "2.1.22") + ";"
	assert.Contains(t, string(updated), want)
	assert.Equal(t, int64(len(updated)), req.ContentLength)
	wireBody, readErr := io.ReadAll(req.Body)
	assert.NoError(t, readErr)
	assert.Equal(t, updated, wireBody)
}

func TestSyncBillingHeaderVersionPreservesClientContext(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.81.df2; cc_entrypoint=cli;"},{"type":"text","text":"Keep the client's exact system instructions."}],"messages":[{"role":"user","content":[{"type":"text","text":"keep this message"}]},{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"read_file","input":{"path":"a.txt"}}]}],"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object"}}]}`)

	updated := syncBillingHeaderVersion(body, "claude-cli/2.1.22 (external, cli)")

	for _, path := range []string{"system.1", "messages", "tools"} {
		assert.Equal(t, gjson.GetBytes(body, path).Raw, gjson.GetBytes(updated, path).Raw, path)
	}
}
