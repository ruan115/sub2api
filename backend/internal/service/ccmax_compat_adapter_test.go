package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
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

func TestPrepareCCMaxCompatibilityRequestNormalModeKeepsMimicryWithNeutralExpansion(t *testing.T) {
	body := []byte(`{"model":"claude-fable-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	prepared, err := PrepareCCMaxCompatibilityRequest(CCMaxCompatibilityInput{
		Body: body, Model: "claude-fable-5", Stream: true, OAuth: true,
		AccessToken: "token", NormalRequestMode: true,
	})
	require.NoError(t, err)
	require.True(t, prepared.Mimic)
	require.Len(t, gjson.GetBytes(prepared.Body, "system").Array(), 3)
	require.Equal(t, ccmaxNormalRequestExpansionPrompt, gjson.GetBytes(prepared.Body, "system.2.text").String())
	require.NotContains(t, string(prepared.Body), "authorized security testing")
	require.Contains(t, getHeaderRaw(prepared.Headers, "anthropic-beta"), "claude-code-20250219")
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
