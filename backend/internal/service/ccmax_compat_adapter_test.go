package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCCMaxCompatibilityWireMatchesOriginalForwardDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fixtures := []struct {
		name    string
		body    string
		headers http.Header
	}{
		{
			name: "oauth mimic",
			body: `{"model":"claude-sonnet-4-5","system":"Only answer with facts.","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
			headers: http.Header{
				"Content-Type":   {"application/json"},
				"User-Agent":     {"relay/1.0"},
				"Anthropic-Beta": {"client-only-beta"},
			},
		},
		{
			name: "real Claude Code",
			body: `{"model":"claude-haiku-4-5","max_tokens":64,"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"account\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}"},"system":[{"type":"text","text":"client system","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hello"}]}`,
			headers: http.Header{
				"Content-Type":            {"application/json"},
				"User-Agent":              {"claude-cli/2.1.220 (external, cli)"},
				"X-Stainless-Lang":        {"js"},
				"X-Stainless-Runtime":     {"node"},
				"X-Stainless-Retry-Count": {"0"},
			},
		},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			body := []byte(fixture.body)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
			require.NoError(t, err)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
			c.Request.Header = fixture.headers.Clone()

			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"msg_1","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)),
			}}
			cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
			service := &GatewayService{
				cfg: cfg, responseHeaderFilter: compileResponseHeaderFilter(cfg), httpUpstream: upstream,
				rateLimitService: &RateLimitService{}, deferredService: &DeferredService{},
			}
			account := &Account{
				ID: 301, Name: "compat-contract", Platform: PlatformAnthropic, Type: AccountTypeOAuth,
				Concurrency: 1, Credentials: map[string]any{"access_token": "oauth-token"},
				Status: StatusActive, Schedulable: true,
			}
			_, err = service.Forward(context.Background(), c, account, parsed)
			require.NoError(t, err)

			prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
				Body: body, ClientHeaders: fixture.headers.Clone(), Model: parsed.Model, Stream: parsed.Stream,
				OAuth: true, AccessToken: "oauth-token", AccountID: account.ID,
			})
			require.NoError(t, err)
			require.Equal(t, string(upstream.lastBody), string(prepared.Body))

			originalHeaders := upstream.lastReq.Header.Clone()
			compatHeaders := prepared.Headers.Clone()
			deleteHeaderAllForms(originalHeaders, "x-client-request-id")
			deleteHeaderAllForms(compatHeaders, "x-client-request-id")
			require.Equal(t, originalHeaders, compatHeaders)
		})
	}
}

func TestPrepareCCMaxCompatibilityRequestUsesOriginalOAuthPipeline(t *testing.T) {
	body := []byte(`{"unknown":{"keep":true},"model":"claude-sonnet-4-5","system":"Only answer with facts.","messages":[{"role":"user","content":"hello from compatibility"}],"max_tokens":64}`)
	fingerprint := &Fingerprint{
		ClientID:      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		UserAgent:     "claude-cli/" + "2.1.220" + " (external, cli)",
		StainlessLang: "js", StainlessPackageVersion: "0.94.0", StainlessOS: "Linux",
		StainlessArch: "arm64", StainlessRuntime: "node", StainlessRuntimeVersion: "v24.3.0",
	}
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, ClientHeaders: http.Header{"Content-Type": {"application/json"}},
		Model: "claude-sonnet-4-5", OAuth: true, AccessToken: "oauth-token",
		AccountID: 9, AccountUUID: "account-uuid", ClientIP: "203.0.113.9",
		ClientUserAgent: "client/1.0", APIKeyID: 17, Fingerprint: fingerprint,
	})
	require.NoError(t, err)
	require.True(t, prepared.Mimic)
	require.False(t, prepared.ClaudeCode)
	require.Equal(t, "claude-sonnet-4-5-20250929", prepared.Model)
	require.True(t, gjson.GetBytes(prepared.Body, "unknown.keep").Bool())
	require.Len(t, gjson.GetBytes(prepared.Body, "system").Array(), 3)
	require.Contains(t, gjson.GetBytes(prepared.Body, "system.0.text").String(), "x-anthropic-billing-header")
	require.Contains(t, gjson.GetBytes(prepared.Body, "messages.0.content.0.text").String(), "Only answer with facts.")
	parsedUserID := ParseMetadataUserID(gjson.GetBytes(prepared.Body, "metadata.user_id").String())
	require.Equal(t, "account-uuid", parsedUserID.AccountUUID)
	seed := buildStableSessionSeed(9, sessionContextDiscriminator(&SessionContext{
		ClientIP: "203.0.113.9", UserAgent: "client/1.0", APIKeyID: 17,
	}), extractFirstUserText(prepared.Body))
	require.Equal(t, generateUUIDFromSeed("9::"+generateSessionUUID(seed)), parsedUserID.SessionID)
	require.Equal(t, "Bearer oauth-token", getHeaderRaw(prepared.Headers, "Authorization"))
	require.Equal(t, claudeCodeSystemPrompt, gjson.GetBytes(prepared.Body, "system.1.text").String())
	require.Contains(t, getHeaderRaw(prepared.Headers, "anthropic-beta"), "context-management-2025-06-27")
	require.Less(t, bytes.Index(prepared.Body, []byte(`"unknown"`)), bytes.Index(prepared.Body, []byte(`"model"`)))
}

func TestPrepareCCMaxCompatibilityRequestAlignsBillingWithFinalMimicUserAgent(t *testing.T) {
	fingerprint := &Fingerprint{
		ClientID:      strings.Repeat("a", 64),
		UserAgent:     "claude-cli/2.1.247 (external, cli)",
		StainlessLang: "js", StainlessPackageVersion: "0.94.0", StainlessOS: "Linux",
		StainlessArch: "arm64", StainlessRuntime: "node", StainlessRuntimeVersion: "v24.3.0",
	}
	for _, test := range []struct {
		name       string
		configured string
		want       string
	}{
		{name: "built-in mimic version", want: claude.CLICurrentVersion},
		{name: "group configured version", configured: "2.1.238", want: "2.1.238"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
				Body:          []byte(`{"model":"claude-opus-4-8","system":[{"type":"text","text":"client system"}],"max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`),
				ClientHeaders: http.Header{"Content-Type": {"application/json"}},
				Model:         "claude-opus-4-8", Stream: true, OAuth: true, AccessToken: "oauth-token",
				AccountID: 485, AccountUUID: "11111111-1111-4111-8111-111111111111", Fingerprint: fingerprint,
				ClaudeCLIVersion: test.configured,
			})
			require.NoError(t, err)
			require.True(t, prepared.Mimic)

			finalVersion := ExtractCLIVersion(getHeaderRaw(prepared.Headers, "User-Agent"))
			require.Equal(t, test.want, finalVersion)
			billing := gjson.GetBytes(prepared.Body, "system.0.text").String()
			require.Contains(t, billing, "cc_version="+finalVersion+".")
			require.NotContains(t, billing, "cc_version=2.1.247")
		})
	}
}

func TestPrepareCCMaxCompatibilityRequestAppliesGroupFieldPassthrough(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"service_tier":"auto","inference_geo":"us","speed":"fast"}`)
	tests := []struct {
		name          string
		normalMode    bool
		serviceTier   bool
		inferenceGeo  bool
		speed         bool
		wantService   bool
		wantInference bool
		wantSpeed     bool
	}{
		{name: "default strips all fields"},
		{name: "switches are independent", serviceTier: true, wantService: true},
		{
			name: "distilled mode preserves enabled fields", normalMode: true,
			serviceTier: true, inferenceGeo: true, speed: true,
			wantService: true, wantInference: true, wantSpeed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
				Body: body, Model: "claude-sonnet-4-5", APIKey: "upstream-key",
				NormalRequestMode: test.normalMode, ServiceTierPassthrough: test.serviceTier,
				InferenceGeoPassthrough: test.inferenceGeo, SpeedPassthrough: test.speed,
			})
			require.NoError(t, err)
			require.Equal(t, test.wantService, gjson.GetBytes(prepared.Body, "service_tier").Exists())
			require.Equal(t, test.wantInference, gjson.GetBytes(prepared.Body, "inference_geo").Exists())
			require.Equal(t, test.wantSpeed, gjson.GetBytes(prepared.Body, "speed").Exists())
		})
	}
}

func TestPrepareCCMaxCompatibilityRequestControlsClientAnthropicBeta(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	headers := http.Header{
		"User-Agent":     {"relay/1.0"},
		"Anthropic-Beta": {"client-custom-beta-2099-01-01"},
	}
	base := CCMaxCompatibilityInput{
		Body: body, ClientHeaders: headers, Model: "claude-sonnet-4-5",
		OAuth: true, AccessToken: "oauth-token",
	}
	withoutClientBeta, err := PrepareCCMaxCompatibilityRequest(base)
	require.NoError(t, err)
	require.NotContains(t, getHeaderRaw(withoutClientBeta.Headers, "anthropic-beta"), "client-custom-beta-2099-01-01")
	require.Contains(t, getHeaderRaw(withoutClientBeta.Headers, "anthropic-beta"), "claude-code-20250219")

	base.AnthropicBetaPassthrough = true
	withClientBeta, err := PrepareCCMaxCompatibilityRequest(base)
	require.NoError(t, err)
	require.Contains(t, getHeaderRaw(withClientBeta.Headers, "anthropic-beta"), "client-custom-beta-2099-01-01")
	require.Contains(t, getHeaderRaw(withClientBeta.Headers, "anthropic-beta"), "claude-code-20250219")
}

func TestPrepareCCMaxCompatibilityRequestStrictClaudeCodeClassification(t *testing.T) {
	body := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"arbitrary"}}`)
	headers := http.Header{"User-Agent": {"claude-cli/2.1.220 (external, cli)"}}
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, ClientHeaders: headers, Model: "claude-test", OAuth: true,
		AccessToken: "token", AccountID: 1,
		Fingerprint: &Fingerprint{ClientID: strings.Repeat("a", 64), UserAgent: headers.Get("User-Agent")},
	})
	require.NoError(t, err)
	require.True(t, prepared.Mimic)
	require.False(t, prepared.ClaudeCode)

	validBody := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"{\"device_id\":\"device\",\"account_uuid\":\"account\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}"}}`)
	prepared, err = PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: validBody, ClientHeaders: headers, Model: "claude-test", OAuth: true,
		AccessToken: "token", AccountID: 1, AccountUUID: "account",
		Fingerprint: &Fingerprint{ClientID: strings.Repeat("a", 64), UserAgent: headers.Get("User-Agent")},
	})
	require.NoError(t, err)
	require.False(t, prepared.Mimic)
	require.True(t, prepared.ClaudeCode)
}

func TestPrepareCCMaxCompatibilityRequestCanForceChatMimicry(t *testing.T) {
	body := []byte(`{"model":"claude-test","stream":true,"max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-test", Stream: true, OAuth: true,
		AccessToken: "token", ClientHeaders: http.Header{"User-Agent": {"claude-cli/2.1.220 (external, cli)"}},
		ForceNonClaudeCode: true,
	})
	require.NoError(t, err)
	require.True(t, prepared.Mimic)
	require.False(t, prepared.ClaudeCode)
}

func TestPrepareCCMaxCompatibilityRequestNormalModeMatchesDistilledRequestSurface(t *testing.T) {
	body := []byte(`{"unknown":{"drop":true},"model":"claude-fable-5","system":[{"type":"text","text":"Keep this system prompt.","cache_control":{"type":"ephemeral","ttl":"5m"}}],"stream":true,"max_tokens":64,"temperature":999,"top_p":999,"top_k":-1,"thinking":{"type":"enabled","budget_tokens":1024},"stop_sequences":["END"],"tools":[{"name":"read.file","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"read.file"},"messages":[{"role":"user","content":"hi"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", Stream: true, OAuth: true,
		AccessToken: "token", NormalRequestMode: true,
	})
	require.NoError(t, err)
	require.True(t, prepared.Mimic)
	require.True(t, prepared.Distilled)
	require.Len(t, gjson.GetBytes(prepared.Body, "system").Array(), 2)
	require.Contains(t, gjson.GetBytes(prepared.Body, "system.0.text").String(), "x-anthropic-billing-header")
	require.Equal(t, "Keep this system prompt.", gjson.GetBytes(prepared.Body, "system.1.text").String())
	require.Equal(t, "5m", gjson.GetBytes(prepared.Body, "system.1.cache_control.ttl").String())
	require.NotContains(t, string(prepared.Body), claudeCodeSystemPrompt)
	require.NotContains(t, string(prepared.Body), "authorized security testing")
	for _, path := range []string{"unknown", "temperature", "top_p", "top_k"} {
		require.False(t, gjson.GetBytes(prepared.Body, path).Exists(), path)
	}
	require.Equal(t, "adaptive", gjson.GetBytes(prepared.Body, "thinking.type").String())
	require.Equal(t, "END", gjson.GetBytes(prepared.Body, "stop_sequences.0").String())
	require.Equal(t, "read.file", gjson.GetBytes(prepared.Body, "tools.0.name").String())
	require.Equal(t, "read.file", gjson.GetBytes(prepared.Body, "tool_choice.name").String())
	require.Equal(t, "5m", gjson.GetBytes(prepared.Body, "tools.0.cache_control.ttl").String())
	require.Equal(t, "claude-fable-5", prepared.Model)
	require.Equal(t, "claude-fable-5", gjson.GetBytes(prepared.Body, "model").String())
	require.Contains(t, getHeaderRaw(prepared.Headers, "anthropic-beta"), "claude-code-20250219")
}

func TestPrepareCCMaxCompatibilityRequestNormalModeDropsSamplingForOpus(t *testing.T) {
	body := []byte(`{"model":"claude-opus-4-8","max_tokens":64,"temperature":0.7,"top_p":0.8,"top_k":20,"messages":[{"role":"user","content":"hi"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-opus-4-8", OAuth: true,
		AccessToken: "token", NormalRequestMode: true,
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, gjson.GetBytes(prepared.Body, "temperature").Int())
	for _, path := range []string{"top_p", "top_k"} {
		require.False(t, gjson.GetBytes(prepared.Body, path).Exists(), path)
	}
	require.Equal(t, "claude-opus-4-8", prepared.Model)
}

func TestPrepareCCMaxCompatibilityRequestNormalModePreservesSignedThinkingHistory(t *testing.T) {
	signature := strings.Repeat("signed-history-", 154)
	body, err := json.Marshal(map[string]any{
		"model":      "claude-fable-5",
		"max_tokens": 23333,
		"stream":     false,
		"thinking": map[string]any{
			"type":    "adaptive",
			"display": "summarized",
		},
		"output_config": map[string]any{"effort": "xhigh"},
		"metadata": map[string]any{
			"user_id": `{"device_id":"device","account_uuid":"account","session_id":"11111111-1111-4111-8111-111111111111"}`,
		},
		"system": []any{
			map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.220; cc_entrypoint=cli; cch=00000;"},
			map[string]any{"type": "text", "text": claudeCodeSystemPrompt, "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "text", "text": "Keep the client system exactly.", "cache_control": map[string]any{"type": "ephemeral"}},
		},
		"tools": []any{
			map[string]any{"name": "read.file", "description": "Read a file", "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Use the tool."}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "", "signature": signature},
				map[string]any{"type": "tool_use", "id": "toolu_1", "name": "read.file", "input": map[string]any{"path": "/tmp/a"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
			}},
		},
	})
	require.NoError(t, err)

	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, ClientHeaders: http.Header{
			"User-Agent":     {"Go-http-client/1.1"},
			"Anthropic-Beta": {"claude-code-20250219,interleaved-thinking-2025-05-14,effort-2025-11-24"},
		},
		Model: "claude-fable-5", OAuth: true, AccessToken: "token",
		NormalRequestMode: true, AnthropicBetaPassthrough: true,
	})
	require.NoError(t, err)
	require.True(t, prepared.ClaudeCode)
	require.False(t, prepared.Mimic)
	require.Equal(t, "adaptive", gjson.GetBytes(prepared.Body, "thinking.type").String())
	require.Equal(t, "summarized", gjson.GetBytes(prepared.Body, "thinking.display").String())
	require.Equal(t, signature, gjson.GetBytes(prepared.Body, "messages.1.content.0.signature").String())
	require.Equal(t, "thinking", gjson.GetBytes(prepared.Body, "messages.1.content.0.type").String())
	require.Equal(t, "read.file", gjson.GetBytes(prepared.Body, "messages.1.content.1.name").String())
	require.Equal(t, "read.file", gjson.GetBytes(prepared.Body, "tools.0.name").String())
	require.Equal(t, "xhigh", gjson.GetBytes(prepared.Body, "output_config.effort").String())
	require.False(t, gjson.GetBytes(prepared.Body, "context_management").Exists())
	require.Len(t, gjson.GetBytes(prepared.Body, "system").Array(), 3)
}

func TestPrepareCCMaxCompatibilityRequestNormalModePreservesSignedThinkingHistoryWithoutTopLevelThinking(t *testing.T) {
	signature := strings.Repeat("signed-history-", 154)
	body, err := json.Marshal(map[string]any{
		"model":      "claude-fable-5",
		"max_tokens": 23333,
		"stream":     false,
		"messages": []any{
			map[string]any{"role": "user", "content": "Use the previous context."},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "", "signature": signature},
				map[string]any{"type": "text", "text": "Previous response."},
			}},
			map[string]any{"role": "user", "content": "Continue."},
		},
	})
	require.NoError(t, err)

	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", OAuth: true,
		AccessToken: "token", NormalRequestMode: true,
	})
	require.NoError(t, err)
	require.Equal(t, "adaptive", gjson.GetBytes(prepared.Body, "thinking.type").String())
	require.Equal(t, signature, gjson.GetBytes(prepared.Body, "messages.1.content.0.signature").String())
	require.Equal(t, "thinking", gjson.GetBytes(prepared.Body, "messages.1.content.0.type").String())
}

// The client embeds a proxy-detection watermark in the "Today's date is ..."
// sentence (curly apostrophe U+2019 + slash date). By default the anti-
// fingerprint normalization erases it (ASCII apostrophe + hyphen), which costs
// one token. PreserveClientDateline forwards it verbatim so a token-parity
// compatibility group keeps the client's exact count, accepting the leak.
func TestPrepareCCMaxCompatibilityRequestOpenCodeScrubBySwitch(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","system":[{"type":"text","text":"You are OpenCode, the best coding agent on the planet.\nYou are powered by the model named claude-opus-4-6. The exact model ID is anthropic/claude-opus-4-6\n(Assuming you're using OpenCode) You need to use this merdian plugin: https://github.com/rynfar/meridian-plugin-opencode-scrub\nKeep client instructions."}],"max_tokens":64,"messages":[{"role":"user","content":"I sometimes use OpenCode locally."}]}`)

	erased, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", OAuth: true, AccessToken: "token",
		NormalRequestMode: true, OpenCodeScrub: true,
	})
	require.NoError(t, err)
	system := gjson.GetBytes(erased.Body, "system").Raw
	require.NotContains(t, system, "You are OpenCode")
	require.NotContains(t, system, "You are Claude Code")
	require.NotContains(t, system, "You are powered by the model named")
	require.NotContains(t, system, "Assuming you're using OpenCode")
	require.NotContains(t, system, "meridian-plugin-opencode-scrub")
	require.Contains(t, system, "Keep client instructions.")
	require.Contains(t, gjson.GetBytes(erased.Body, "messages.0.content").String(), "I sometimes use OpenCode locally.")

	passthrough, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", OAuth: true, AccessToken: "token",
		NormalRequestMode: true,
	})
	require.NoError(t, err)
	passed := gjson.GetBytes(passthrough.Body, "system").Raw
	require.Contains(t, passed, "You are powered by the model named")
}

func TestPrepareCCMaxCompatibilityRequestDatelineNormalizationSwitch(t *testing.T) {
	raw := "Today\u2019s date is 2026/04/17."
	canonical := "Today's date is 2026-04-17."
	makeBody := func() []byte {
		body, err := json.Marshal(map[string]any{
			"model":      "claude-fable-5",
			"max_tokens": 64,
			"messages": []any{
				map[string]any{"role": "user", "content": []any{
					map[string]any{"type": "text", "text": "<system-reminder>\n" + raw + "\n</system-reminder>"},
				}},
			},
		})
		require.NoError(t, err)
		return body
	}

	fp := &Fingerprint{ClientID: strings.Repeat("a", 64), UserAgent: "claude-cli/2.1.250 (external, cli)"}

	// Default: normalization on → watermark erased.
	normalized, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: makeBody(), Model: "claude-fable-5", OAuth: true, AccessToken: "token",
		NormalRequestMode: true, Fingerprint: fp,
	})
	require.NoError(t, err)
	normText := gjson.GetBytes(normalized.Body, "messages.0.content.0.text").String()
	require.Contains(t, normText, canonical)
	require.NotContains(t, normText, raw)

	// PreserveClientDateline: normalization off → watermark forwarded verbatim.
	preserved, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: makeBody(), Model: "claude-fable-5", OAuth: true, AccessToken: "token",
		NormalRequestMode: true, Fingerprint: fp, PreserveClientDateline: true,
	})
	require.NoError(t, err)
	presText := gjson.GetBytes(preserved.Body, "messages.0.content.0.text").String()
	require.Contains(t, presText, raw)
	require.NotContains(t, presText, canonical)
}

func TestPrepareCCMaxCompatibilityRequestNormalModePreservesExplicitCacheTTL(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","system":[{"type":"text","text":"one hour","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"default","cache_control":{"type":"ephemeral"}}],"max_tokens":64,"tools":[{"name":"read_file","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", OAuth: true,
		AccessToken: "token", NormalRequestMode: true,
	})
	require.NoError(t, err)
	require.Equal(t, "1h", gjson.GetBytes(prepared.Body, "system.1.cache_control.ttl").String())
	require.Equal(t, "5m", gjson.GetBytes(prepared.Body, "system.2.cache_control.ttl").String())
	require.Equal(t, "1h", gjson.GetBytes(prepared.Body, "tools.0.cache_control.ttl").String())
}

func TestPrepareCCMaxCompatibilityRequestNormalModeOrdersGeneratedToolTTLBeforeSystem1h(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","system":[{"type":"text","text":"one hour","cache_control":{"type":"ephemeral","ttl":"1h"}}],"max_tokens":64,"tools":[{"name":"read_file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", OAuth: true,
		AccessToken: "token", NormalRequestMode: true,
	})
	require.NoError(t, err)
	require.Equal(t, "1h", gjson.GetBytes(prepared.Body, "tools.0.cache_control.ttl").String())
	require.Equal(t, "1h", gjson.GetBytes(prepared.Body, "system.1.cache_control.ttl").String())
}

func TestNormalizeEphemeralCacheControlTTLOrderUsesAnthropicProcessingOrder(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read_file","cache_control":{"type":"ephemeral","ttl":"5m"}}],"system":[{"type":"text","text":"system","cache_control":{"type":"ephemeral","ttl":"5m"}}],"messages":[{"role":"user","content":[{"type":"text","text":"first","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"last","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`)
	normalized := normalizeEphemeralCacheControlTTLOrder(body)
	require.Equal(t, "1h", gjson.GetBytes(normalized, "tools.0.cache_control.ttl").String())
	require.Equal(t, "1h", gjson.GetBytes(normalized, "system.0.cache_control.ttl").String())
	require.Equal(t, "1h", gjson.GetBytes(normalized, "messages.0.content.0.cache_control.ttl").String())
	require.Equal(t, "5m", gjson.GetBytes(normalized, "messages.0.content.1.cache_control.ttl").String())
}

func TestPrepareCCMaxCompatibilityCountTokensOrdersToolTTLBeforeMessage1h(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","thinking":{"type":"adaptive","display":"summarized"},"tools":[{"name":"read_file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", OAuth: true, CountTokens: true,
		AccessToken: "token", NormalRequestMode: true,
	})
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(prepared.Body, "thinking").Exists())
	require.Equal(t, "1h", gjson.GetBytes(prepared.Body, "tools.0.cache_control.ttl").String())
	require.Equal(t, "1h", gjson.GetBytes(prepared.Body, "messages.0.content.0.cache_control.ttl").String())
}

func TestPrepareCCMaxCompatibilityRequestNormalModeOmitsIdentityByDefault(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.test; cc_entrypoint=cli;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Keep this system prompt."}],"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", OAuth: true,
		AccessToken: "token", NormalRequestMode: true,
	})
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(prepared.Body, "system").Array(), 2)
	require.Equal(t, "Keep this system prompt.", gjson.GetBytes(prepared.Body, "system.1.text").String())
	require.Equal(t, 1, strings.Count(string(prepared.Body), "x-anthropic-billing-header:"))
	require.NotContains(t, string(prepared.Body), claudeCodeSystemPrompt)
}

func TestPrepareCCMaxCompatibilityRequestNormalModeCanEnableIdentity(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.test; cc_entrypoint=cli;"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."},{"type":"text","text":"Client system."}],"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", OAuth: true,
		AccessToken: "token", NormalRequestMode: true,
		ClaudeCodeIdentityEnabled: true,
	})
	require.NoError(t, err)
	require.Len(t, gjson.GetBytes(prepared.Body, "system").Array(), 3)
	require.Equal(t, 1, strings.Count(string(prepared.Body), "x-anthropic-billing-header:"))
	require.Equal(t, 1, strings.Count(string(prepared.Body), claudeCodeSystemPrompt))
	require.Equal(t, "Client system.", gjson.GetBytes(prepared.Body, "system.2.text").String())
}

func TestNormalizeCCMaxDistilledResponseAddsIterationsAndPreservesSignature(t *testing.T) {
	body := []byte(`{"type":"message","content":[{"type":"thinking","thinking":"","signature":"signed-thinking"},{"type":"text","text":"ok"}],"usage":{"input_tokens":23,"output_tokens":33,"cache_read_input_tokens":4,"cache_creation_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":5},"output_tokens_details":{"thinking_tokens":21}}}`)
	normalized := NormalizeCCMaxDistilledResponse(body)
	require.Equal(t, "signed-thinking", gjson.GetBytes(normalized, "content.0.signature").String())
	require.EqualValues(t, 23, gjson.GetBytes(normalized, "usage.iterations.0.input_tokens").Int())
	require.EqualValues(t, 33, gjson.GetBytes(normalized, "usage.iterations.0.output_tokens").Int())
	require.EqualValues(t, 4, gjson.GetBytes(normalized, "usage.iterations.0.cache_read_input_tokens").Int())
	require.EqualValues(t, 7, gjson.GetBytes(normalized, "usage.iterations.0.cache_creation_input_tokens").Int())
	require.EqualValues(t, 2, gjson.GetBytes(normalized, "usage.iterations.0.cache_creation.ephemeral_5m_input_tokens").Int())
	require.EqualValues(t, 5, gjson.GetBytes(normalized, "usage.iterations.0.cache_creation.ephemeral_1h_input_tokens").Int())
	require.Equal(t, "message", gjson.GetBytes(normalized, "usage.iterations.0.type").String())
	require.EqualValues(t, 21, gjson.GetBytes(normalized, "usage.output_tokens_details.thinking_tokens").Int())

	again := NormalizeCCMaxDistilledResponse(normalized)
	require.JSONEq(t, string(normalized), string(again))
}

func TestPrepareCCMaxCompatibilityCountTokensUsesOriginalSanitizer(t *testing.T) {
	body := []byte(`{"model":"claude-test","messages":[{"role":"user","content":"hello"}],"metadata":{"trace":"keep"},"max_tokens":12,"stream":true,"temperature":0.2}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, ClientHeaders: http.Header{}, Model: "claude-test", CountTokens: true,
		OAuth: true, AccessToken: "token", AccountID: 1,
		Fingerprint: &Fingerprint{ClientID: strings.Repeat("a", 64), UserAgent: "claude-cli/2.1.220 (external, cli)"},
	})
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(prepared.Body, "max_tokens").Exists())
	require.False(t, gjson.GetBytes(prepared.Body, "stream").Exists())
	require.False(t, gjson.GetBytes(prepared.Body, "temperature").Exists())
	require.Equal(t, "keep", gjson.GetBytes(prepared.Body, "metadata.trace").String())
	require.Contains(t, getHeaderRaw(prepared.Headers, "anthropic-beta"), "token-counting-2024-11-01")
}

func TestPrepareCCMaxCompatibilityCountTokensDoesNotUseMessagesBillingFallback(t *testing.T) {
	body := []byte(`{"model":"claude-test","system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220.abc; cc_entrypoint=cli;"}],"messages":[{"role":"user","content":"hello"}],"metadata":{"user_id":"arbitrary"}}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, ClientHeaders: http.Header{"User-Agent": {"relay/1.0"}},
		Model: "claude-test", CountTokens: true, OAuth: true, AccessToken: "token",
		AccountID: 1, Fingerprint: &Fingerprint{ClientID: strings.Repeat("a", 64), UserAgent: "claude-cli/2.1.220 (external, cli)"},
	})
	require.NoError(t, err)
	require.True(t, prepared.Mimic)
	require.False(t, prepared.ClaudeCode)
}

func TestPrepareCCMaxCompatibilityAPIKeyUsesAccountModelMapping(t *testing.T) {
	body := []byte(`{"unknown":{"keep":true},"model":"claude-client","messages":[{"role":"user","content":"hello"}],"max_tokens":8}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, ClientHeaders: http.Header{}, Model: "claude-client",
		APIKey: "upstream-key", MappedModel: "claude-upstream",
	})
	require.NoError(t, err)
	require.Equal(t, "claude-upstream", prepared.Model)
	require.Equal(t, "claude-upstream", gjson.GetBytes(prepared.Body, "model").String())
	require.True(t, gjson.GetBytes(prepared.Body, "unknown.keep").Bool())
	require.Equal(t, "upstream-key", getHeaderRaw(prepared.Headers, "x-api-key"))
}

func TestResolveCCMaxCompatibilityFingerprintUsesOriginalCacheLifetime(t *testing.T) {
	headers := http.Header{"User-Agent": {"claude-cli/2.1.220 (external, cli)"}}
	stale := &Fingerprint{ClientID: strings.Repeat("a", 64), UserAgent: headers.Get("User-Agent"), UpdatedAt: time.Now().Add(-8 * 24 * time.Hour).Unix()}
	recreated, changed := ResolveCCMaxCompatibilityFingerprint(headers, stale)
	require.True(t, changed)
	require.NotEqual(t, stale.ClientID, recreated.ClientID)
	require.WithinDuration(t, time.Now(), time.Unix(recreated.UpdatedAt, 0), 2*time.Second)

	old := &Fingerprint{ClientID: strings.Repeat("b", 64), UserAgent: headers.Get("User-Agent"), UpdatedAt: time.Now().Add(-25 * time.Hour).Unix()}
	renewed, changed := ResolveCCMaxCompatibilityFingerprint(headers, old)
	require.True(t, changed)
	require.Equal(t, old.ClientID, renewed.ClientID)
	require.Greater(t, renewed.UpdatedAt, old.UpdatedAt)
}

func TestPrepareCCMaxCompatibilityRetryUsesOriginalRectifiers(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","budget_tokens":2048},"max_tokens":4096,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning","signature":"valid"},{"type":"text","text":"answer"}]},{"role":"user","content":"continue"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-sonnet-4-5", APIKey: "key",
	})
	require.NoError(t, err)

	thinkingRetry, applied, err := PrepareCCMaxCompatibilityRetry(prepared, CCMaxCompatibilityRetryThinking)
	require.NoError(t, err)
	require.True(t, applied)
	require.False(t, gjson.GetBytes(thinkingRetry.Body, "thinking").Exists())
	require.Equal(t, "text", gjson.GetBytes(thinkingRetry.Body, "messages.0.content.0.type").String())

	budgetRetry, applied, err := PrepareCCMaxCompatibilityRetry(prepared, CCMaxCompatibilityRetryBudget)
	require.NoError(t, err)
	require.True(t, applied)
	require.EqualValues(t, BudgetRectifyBudgetTokens, gjson.GetBytes(budgetRetry.Body, "thinking.budget_tokens").Int())
	require.EqualValues(t, BudgetRectifyMaxTokens, gjson.GetBytes(budgetRetry.Body, "max_tokens").Int())

	require.True(t, IsCCMaxCompatibilitySignatureError([]byte(`{"error":{"message":"Invalid signature in thinking block"}}`), prepared.Model))
	require.True(t, IsCCMaxCompatibilityToolSignatureError([]byte(`{"error":{"message":"Invalid signature in tool_use"}}`)))
	require.True(t, IsCCMaxCompatibilityBudgetError([]byte(`{"error":{"message":"thinking.budget_tokens input should be >= 1024"}}`)))
}

func TestCCMaxCompatibilityDelegatesAccountRetryPolicy(t *testing.T) {
	require.True(t, ShouldRetryCCMaxCompatibilityStatus(AccountTypeOAuth, nil, http.StatusForbidden))
	require.False(t, ShouldRetryCCMaxCompatibilityStatus(AccountTypeOAuth, nil, http.StatusTooManyRequests))
	require.False(t, ShouldRetryCCMaxCompatibilityStatus(AccountTypeAPIKey, map[string]any{}, http.StatusServiceUnavailable))
	require.True(t, ShouldRetryCCMaxCompatibilityStatus(AccountTypeAPIKey, map[string]any{
		"custom_error_codes_enabled": true,
		"custom_error_codes":         []any{float64(http.StatusTooManyRequests)},
	}, http.StatusServiceUnavailable))

	pool := ResolveCCMaxCompatibilityAccountPolicy(AccountTypeAPIKey, map[string]any{
		"pool_mode": true, "pool_mode_retry_count": float64(4),
	}, http.StatusTooManyRequests)
	require.True(t, pool.PoolMode)
	require.True(t, pool.PoolRetryable)
	require.Equal(t, 4, pool.PoolRetryCount)
	require.True(t, pool.SkipDefaultErrorHandling)
}

func TestResolveCCMaxCompatibilityCooldownKeeps529ModelScoped(t *testing.T) {
	_, ok := ResolveCCMaxCompatibilityCooldown(529, make(http.Header))
	require.False(t, ok)

	resetAt, ok := ResolveCCMaxCompatibilityCooldown(http.StatusTooManyRequests, make(http.Header))
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(5*time.Second), resetAt, time.Second)
}

func TestResolveCCMaxCompatibilityCooldownWindowReportsSelectedWindow(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset.Unix(), 10))

	resetAt, window, ok := ResolveCCMaxCompatibilityCooldownWindow(http.StatusTooManyRequests, headers)
	require.True(t, ok)
	require.Equal(t, "5h", window)
	require.Equal(t, reset, resetAt)

	_, window, ok = ResolveCCMaxCompatibilityCooldownWindow(http.StatusTooManyRequests, make(http.Header))
	require.True(t, ok)
	require.Empty(t, window)
}
