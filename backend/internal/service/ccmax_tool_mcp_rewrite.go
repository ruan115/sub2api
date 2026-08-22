package service

// This file implements the CCMAX "MCP tool names" lane. Instead of the
// Parrot-style opaque aliases produced by buildToolNameRewriteFromBody, tool
// names are rewritten into the mcp__<server>__<tool> shape that Claude Code
// uses for MCP servers, keeping the original tool name readable for the model.
//
// The resulting mapping reuses ToolNameRewrite so applyToolNameRewriteToBody
// and restoreToolNamesInBytes stay untouched.

import (
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	// mcpToolNameMaxLength is the Anthropic constraint for tool names:
	// ^[a-zA-Z0-9_-]{1,64}$. Some upstream builds accept 128, 64 is the
	// documented contract and the safe target.
	mcpToolNameMaxLength = 64
	mcpToolNamePrefix    = "mcp__"
	mcpToolServerLength  = 6
	mcpToolHashLength    = 6
	mcpToolFallbackName  = "tool"
)

// mcpToolNameBudget is how many characters remain for the sanitized tool name
// after "mcp__" + server segment + "__".
const mcpToolNameBudget = mcpToolNameMaxLength - len(mcpToolNamePrefix) - mcpToolServerLength - 2

// buildCCMaxToolRewrite picks the tool naming lane for the CCMAX adapter.
func buildCCMaxToolRewrite(body []byte, input CCMaxCompatibilityInput) *ToolNameRewrite {
	if input.MCPToolNames {
		return buildMCPToolNameRewrite(body, input.AccountID)
	}
	return buildToolNameRewriteFromBody(body)
}

// buildMCPToolNameRewrite maps every mimicable tool name to
// mcp__<server>__<tool>. The server segment is derived from the account id so
// the mapping is stable per account (prompt cache friendly) while different
// accounts do not share one recognizable marker.
//
// Returns nil when there is nothing to rewrite.
func buildMCPToolNameRewrite(body []byte, accountID int64) *ToolNameRewrite {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return nil
	}
	server := mcpToolServerSegment(accountID)
	rw := &ToolNameRewrite{
		Forward: make(map[string]string),
		Reverse: make(map[string]string),
	}
	used := make(map[string]bool)
	for _, tool := range tools.Array() {
		if !shouldMimicToolName(tool.Get("type").String()) {
			continue
		}
		name := tool.Get("name").String()
		if name == "" || isMCPToolName(name) {
			continue
		}
		if _, exists := rw.Forward[name]; exists {
			continue
		}
		fake := allocateMCPToolName(server, name, used)
		if fake == name {
			continue
		}
		used[fake] = true
		rw.Forward[name] = fake
		rw.Reverse[fake] = name
	}
	if len(rw.Forward) == 0 {
		return nil
	}

	rw.ReverseOrdered = make([][2]string, 0, len(rw.Reverse))
	for fake, real := range rw.Reverse {
		rw.ReverseOrdered = append(rw.ReverseOrdered, [2]string{fake, real})
	}
	sort.SliceStable(rw.ReverseOrdered, func(i, j int) bool {
		return len(rw.ReverseOrdered[i][0]) > len(rw.ReverseOrdered[j][0])
	})
	return rw
}

// allocateMCPToolName builds a unique mcp__ name for one tool. Sanitizing and
// truncating can make two different tools collide (read.file and read_file), so
// colliding names get a numeric suffix instead of silently sharing an alias the
// response side could not tell apart.
func allocateMCPToolName(server, name string, used map[string]bool) string {
	segment := mcpToolNameSegment(name)
	candidate := mcpToolNamePrefix + server + "__" + segment
	if !used[candidate] {
		return candidate
	}
	for index := 2; index < 100; index++ {
		suffix := "_" + strconv.Itoa(index)
		trimmed := segment
		if len(trimmed)+len(suffix) > mcpToolNameBudget {
			trimmed = strings.TrimRight(trimmed[:mcpToolNameBudget-len(suffix)], "_-")
		}
		candidate = mcpToolNamePrefix + server + "__" + trimmed + suffix
		if !used[candidate] {
			return candidate
		}
	}
	return mcpToolNamePrefix + server + "__" + mcpToolHash(name, mcpToolNameBudget)
}

// mcpToolNameSegment turns a client tool name into the trailing segment: only
// [a-zA-Z0-9_-] survives, so names carrying dots, spaces or non-Latin
// characters stop being rejected by the upstream name pattern. Names that no
// longer fit the budget keep a readable head plus a hash of the original.
func mcpToolNameSegment(name string) string {
	segment := sanitizeMCPToolSegment(name)
	if segment == "" {
		// Nothing survived sanitizing (a fully non-Latin name). Keep the tools
		// distinguishable instead of collapsing them all onto one fallback.
		return mcpToolFallbackName + "_" + mcpToolHash(name, mcpToolHashLength)
	}
	if len(segment) <= mcpToolNameBudget {
		return segment
	}
	head := mcpToolNameBudget - mcpToolHashLength - 1
	return strings.TrimRight(segment[:head], "_-") + "_" + mcpToolHash(name, mcpToolHashLength)
}

func sanitizeMCPToolSegment(name string) string {
	var builder strings.Builder
	builder.Grow(len(name))
	previousSeparator := false
	for index := 0; index < len(name); index++ {
		char := name[index]
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-':
			builder.WriteByte(char)
			previousSeparator = false
		default:
			// Underscores are legal but still collapse, so "a._b" cannot grow
			// into "a__b"; a single CJK character would otherwise expand into
			// three underscores.
			if !previousSeparator {
				builder.WriteByte('_')
				previousSeparator = true
			}
		}
	}
	return strings.Trim(builder.String(), "_")
}

// isMCPToolName reports whether the client already sent a valid MCP-shaped
// name. Those are left alone so an unnecessary rewrite cannot invalidate the
// upstream prompt cache.
func isMCPToolName(name string) bool {
	if !strings.HasPrefix(name, mcpToolNamePrefix) || len(name) > mcpToolNameMaxLength {
		return false
	}
	if !strings.Contains(strings.TrimPrefix(name, mcpToolNamePrefix), "__") {
		return false
	}
	for index := 0; index < len(name); index++ {
		char := name[index]
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9', char == '-', char == '_':
		default:
			return false
		}
	}
	return true
}

func mcpToolServerSegment(accountID int64) string {
	return mcpToolHash("ccmax-mcp-server:"+strconv.FormatInt(accountID, 10), mcpToolServerLength)
}

func mcpToolHash(value string, length int) string {
	digest := fnv.New64a()
	_, _ = digest.Write([]byte(value))
	sum := strconv.FormatUint(digest.Sum64(), 16)
	for len(sum) < length {
		sum = "0" + sum
	}
	return sum[:length]
}
