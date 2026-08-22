package service

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestBuildMCPToolNameRewriteKeepsOriginalNameReadable(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read_file","input_schema":{}},{"name":"web_search","type":"web_search_20250305"}]}`)
	rw := buildMCPToolNameRewrite(body, 42)
	require.NotNil(t, rw)

	fake := rw.Forward["read_file"]
	require.True(t, strings.HasPrefix(fake, mcpToolNamePrefix), fake)
	require.True(t, strings.HasSuffix(fake, "__read_file"), fake)
	require.LessOrEqual(t, len(fake), mcpToolNameMaxLength)
	// server tool 名字属于 Anthropic 协议语义，改了上游会直接拒绝。
	require.NotContains(t, rw.Forward, "web_search")
}

func TestBuildMCPToolNameRewriteIsStablePerAccount(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read_file","input_schema":{}},{"name":"list_dir","input_schema":{}}]}`)
	first := buildMCPToolNameRewrite(body, 7)
	second := buildMCPToolNameRewrite(body, 7)
	other := buildMCPToolNameRewrite(body, 8)
	require.NotNil(t, first)
	require.Equal(t, first.Forward, second.Forward, "同账号同工具集必须稳定，否则 prompt cache 每次都会 miss")
	require.NotEqual(t, first.Forward["read_file"], other.Forward["read_file"], "不同账号不能共用同一个 server 段")
}

func TestBuildMCPToolNameRewriteSanitizesIllegalCharacters(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read.file","input_schema":{}},{"name":"Search Files","input_schema":{}},{"name":"读取文件","input_schema":{}}]}`)
	rw := buildMCPToolNameRewrite(body, 1)
	require.NotNil(t, rw)
	require.Len(t, rw.Forward, 3)
	for real, fake := range rw.Forward {
		require.Regexp(t, `^[a-zA-Z0-9_-]{1,64}$`, fake, "real=%s", real)
	}
	require.True(t, strings.HasSuffix(rw.Forward["read.file"], "__read_file"))
	require.True(t, strings.HasSuffix(rw.Forward["Search Files"], "__Search_Files"))
}

func TestBuildMCPToolNameRewriteResolvesCollisions(t *testing.T) {
	// read.file 与 read_file 清洗后会撞名；两者必须拿到不同假名，
	// 否则响应侧无法判断该还原成哪一个。
	body := []byte(`{"tools":[{"name":"read_file","input_schema":{}},{"name":"read.file","input_schema":{}},{"name":"read file","input_schema":{}}]}`)
	rw := buildMCPToolNameRewrite(body, 3)
	require.NotNil(t, rw)
	require.Len(t, rw.Forward, 3)
	require.Len(t, rw.Reverse, 3, "假名必须互不相同")
}

func TestBuildMCPToolNameRewriteTruncatesLongNames(t *testing.T) {
	long := "mcp_github_server_" + strings.Repeat("very_long_tool_name_", 5)
	body := []byte(`{"tools":[{"name":"` + long + `","input_schema":{}}]}`)
	rw := buildMCPToolNameRewrite(body, 9)
	require.NotNil(t, rw)
	fake := rw.Forward[long]
	require.Len(t, fake, mcpToolNameMaxLength)
	require.Regexp(t, `^[a-zA-Z0-9_-]{1,64}$`, fake)
}

func TestBuildMCPToolNameRewriteSkipsValidMCPNames(t *testing.T) {
	// 客户端已经发了合法的 MCP 名字，再包一层只会白白打乱上游缓存。
	body := []byte(`{"tools":[{"name":"mcp__github__list_prs","input_schema":{}},{"name":"read_file","input_schema":{}}]}`)
	rw := buildMCPToolNameRewrite(body, 5)
	require.NotNil(t, rw)
	require.NotContains(t, rw.Forward, "mcp__github__list_prs")
	require.Contains(t, rw.Forward, "read_file")
}

func TestBuildMCPToolNameRewriteWithoutToolsReturnsNil(t *testing.T) {
	require.Nil(t, buildMCPToolNameRewrite([]byte(`{"messages":[]}`), 1))
	require.Nil(t, buildMCPToolNameRewrite([]byte(`{"tools":[]}`), 1))
	require.Nil(t, buildMCPToolNameRewrite([]byte(`{"tools":[{"name":"web_search","type":"web_search_20250305"}]}`), 1))
}

func TestMCPToolNameRewriteRoundTripThroughResponse(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read.file","input_schema":{}}],"tool_choice":{"type":"tool","name":"read.file"},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"tu_1","name":"read.file","input":{}}]}]}`)
	rw := buildMCPToolNameRewrite(body, 11)
	require.NotNil(t, rw)
	fake := rw.Forward["read.file"]

	out := applyToolNameRewriteToBody(body, rw)
	require.Equal(t, fake, gjson.GetBytes(out, "tools.0.name").String())
	require.Equal(t, fake, gjson.GetBytes(out, "tool_choice.name").String())
	require.Equal(t, fake, gjson.GetBytes(out, "messages.0.content.0.name").String())

	upstream := []byte(`{"content":[{"type":"tool_use","id":"tu_2","name":"` + fake + `","input":{}}]}`)
	restored := restoreToolNamesInBytes(upstream, rw)
	require.Equal(t, "read.file", gjson.GetBytes(restored, "content.0.name").String())
}

func TestBuildCCMaxToolRewriteSelectsLane(t *testing.T) {
	body := []byte(`{"tools":[{"name":"read_file","input_schema":{}}]}`)

	// 默认车道：单个工具既不足动态混淆阈值也没有静态前缀，因此不改写。
	require.Nil(t, buildCCMaxToolRewrite(body, CCMaxCompatibilityInput{AccountID: 4}))

	mcp := buildCCMaxToolRewrite(body, CCMaxCompatibilityInput{AccountID: 4, MCPToolNames: true})
	require.NotNil(t, mcp)
	require.True(t, strings.HasPrefix(mcp.Forward["read_file"], mcpToolNamePrefix))
}

func TestPrepareCCMaxCompatibilityRequestAppliesMCPToolNames(t *testing.T) {
	body := []byte(`{"model":"claude-sonnet-4-5","max_tokens":16,"tools":[{"name":"read.file","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`)
	input := CCMaxCompatibilityInput{
		Body:          body,
		ClientHeaders: http.Header{"User-Agent": []string{"curl/8.4.0"}},
		OAuth:         true,
		AccessToken:   "oauth-token",
		AccountID:     42,
		MCPToolNames:  true,
	}
	prepared, err := PrepareCCMaxCompatibilityRequest(input)
	require.NoError(t, err)
	require.True(t, prepared.Mimic)
	require.NotNil(t, prepared.ToolRewrite)

	sent := gjson.GetBytes(prepared.Body, "tools.0.name").String()
	require.True(t, strings.HasPrefix(sent, mcpToolNamePrefix), sent)
	require.True(t, strings.HasSuffix(sent, "__read_file"), sent)

	upstream := []byte(`{"content":[{"type":"tool_use","name":"` + sent + `","input":{}}]}`)
	restored := RestoreCCMaxCompatibilityResponse(upstream, prepared)
	require.Equal(t, "read.file", gjson.GetBytes(restored, "content.0.name").String())
}
