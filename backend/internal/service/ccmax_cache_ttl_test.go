package service

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestForceCCMaxCacheTTL5MOnlyChangesProtocolDirectives(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","cache_control":{"type":"ephemeral","ttl":"1h"},"system":[{"type":"text","text":"literal ttl=1h","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"uncached"}],"tools":[{"name":"read_file","cache_control":{"type":"ephemeral"},"input_schema":{"type":"object","properties":{"cache_control":{"const":{"type":"ephemeral","ttl":"1h"}}}}}],"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"read_file","input":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"literal ttl=1h","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"continue","cache_control":{"type":"ephemeral","ttl":"5m"}}]}],"metadata":{"cache_control":{"type":"ephemeral","ttl":"1h"}}}`)
	original := bytes.Clone(body)
	got := ForceCCMaxCacheTTL5M(body)
	for _, path := range []string{"cache_control", "system.0.cache_control", "tools.0.cache_control", "messages.1.content.0.cache_control", "messages.1.content.1.cache_control"} {
		require.Equal(t, "5m", gjson.GetBytes(got, path+".ttl").String(), path)
	}
	for _, path := range []string{"tools.0.input_schema", "messages.0.content.0.input", "metadata", "system.0.text", "messages.1.content.0.content"} {
		require.Equal(t, gjson.GetBytes(original, path).Raw, gjson.GetBytes(got, path).Raw, path)
	}
	require.False(t, gjson.GetBytes(got, "system.1.cache_control").Exists())
	require.Equal(t, original, body)
	require.Equal(t, got, ForceCCMaxCacheTTL5M(got))
	for _, unchanged := range []string{`{"messages":[{"role":"user","content":"hi"}]}`, `{"cache_control":null}`, `{"cache_control":{"type":"unknown","ttl":"1h"}}`} {
		require.Equal(t, unchanged, string(ForceCCMaxCacheTTL5M([]byte(unchanged))))
	}
}

func TestCCMaxCacheTTL5MCompatibilityLanesAndRetries(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		for _, normal := range []bool{false, true} {
			for _, count := range []bool{false, true} {
				t.Run(fmt.Sprintf("oauth=%t/normal=%t/count=%t", oauth, normal, count), func(t *testing.T) {
					body := []byte(`{"model":"claude-sonnet-4-5","thinking":{"type":"enabled","budget_tokens":2048},"max_tokens":4096,"tools":[{"name":"read_file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"continue","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
					input := CCMaxCompatibilityInput{Body: body, Model: "claude-sonnet-4-5", OAuth: oauth, NormalRequestMode: normal, CountTokens: count, APIKey: "test-key", AccessToken: "test-token"}
					off, err := PrepareCCMaxCompatibilityRequest(input)
					require.NoError(t, err)
					require.Contains(t, string(off.LogicalBody), `"ttl":"1h"`)
					input.ForceCacheTTL5M = true
					on, err := PrepareCCMaxCompatibilityRequest(input)
					require.NoError(t, err)
					require.NotContains(t, string(on.Body), `"ttl":"1h"`)
					require.NotContains(t, string(on.LogicalBody), `"ttl":"1h"`)
					require.Contains(t, string(on.Body), `"ttl":"5m"`)
					if !count {
						stream, err := ForceCCMaxCompatibilityStream(on)
						require.NoError(t, err)
						require.NotContains(t, string(stream.Body), `"ttl":"1h"`)
						retry, applied, err := PrepareCCMaxCompatibilityRetry(on, CCMaxCompatibilityRetryBudget)
						require.NoError(t, err)
						require.True(t, applied)
						require.NotContains(t, string(retry.Body), `"ttl":"1h"`)
					}
				})
			}
		}
	}
}
