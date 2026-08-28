package anthropicfp

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	openCodeIdentitySentence = "You are OpenCode, the best coding agent on the planet."
	openCodeEnvPreamble      = "Here is some useful information about the environment you are running in:"
	openCodePoweredByNeedle  = "You are powered by the model named"
)

var (
	openCodeIdentityLine  = regexp.MustCompile(`You are OpenCode, the best coding agent on the planet\.[^\n]*\n*`)
	openCodeFeedbackBlock = regexp.MustCompile(`If the user asks for help or wants to give feedback[\s\S]*?github\.com/anomalyco/opencode[^\n]*\n*`)
	openCodeDocsParagraph = regexp.MustCompile(`When the user directly asks about OpenCode[\s\S]*?opencode\.ai/docs[^\n]*\n*`)
	openCodeObjectivity   = regexp.MustCompile(`It is best for the user if OpenCode honestly applies[^\n]*\n*`)
	omoIdentityLine       = regexp.MustCompile(`You are ("|\*\*)Sisyphus("|\*\*)[^\n]*OhMyOpenCode[^\n]*\n*`)
	omoAgentIdentityBlock = regexp.MustCompile(`<agent-identity>[\s\S]*?</agent-identity>\n*`)
	omoEnvBlock           = regexp.MustCompile(`<omo-env>[\s\S]*?</omo-env>\n*`)
	poweredByLine         = regexp.MustCompile(`You are powered by the model named [^\n]+\n?`)
	openCodeEnvBlock      = regexp.MustCompile(`\n?Here is some useful information about the environment you are running in:\n<env>[\s\S]*?</env>\n?`)
	assumingOpenCodeLine  = regexp.MustCompile(`(?i)\(Assuming you're using OpenCode\)[^\n]*\n?`)
	meridianPluginLine    = regexp.MustCompile(`(?i)You need to use this mer[i]?dian plugin:[^\n]*\n?`)
	extraBlankLines       = regexp.MustCompile(`\n{3,}`)
)

func looksLikeOpenCode(text string) bool {
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	return strings.Contains(text, openCodeIdentitySentence) ||
		strings.Contains(lower, "anomalyco/opencode") ||
		strings.Contains(lower, "opencode.ai/docs") ||
		strings.Contains(text, "OhMyOpenCode") ||
		strings.Contains(text, openCodePoweredByNeedle) ||
		strings.Contains(lower, "assuming you're using opencode") ||
		strings.Contains(lower, "meridian-plugin-opencode-scrub") ||
		strings.Contains(text, "<omo-env>") ||
		(strings.Contains(text, "<agent-identity>") && strings.Contains(text, "Sisyphus"))
}

// ScrubOpenCodeText deletes OpenCode / OhMyOpenCode fingerprint lines from a
// system-prompt string. It never injects replacement identity or brand prose.
// Payloads without those markers are returned byte-identical. Idempotent.
func ScrubOpenCodeText(text string) string {
	if !looksLikeOpenCode(text) {
		return text
	}
	hadPoweredBy := strings.Contains(text, openCodePoweredByNeedle)
	out := openCodeIdentityLine.ReplaceAllString(text, "")
	out = openCodeFeedbackBlock.ReplaceAllString(out, "")
	out = openCodeDocsParagraph.ReplaceAllString(out, "")
	out = openCodeObjectivity.ReplaceAllString(out, "")
	out = omoAgentIdentityBlock.ReplaceAllString(out, "")
	out = omoIdentityLine.ReplaceAllString(out, "")
	out = omoEnvBlock.ReplaceAllString(out, "")
	out = poweredByLine.ReplaceAllString(out, "")
	out = assumingOpenCodeLine.ReplaceAllString(out, "")
	out = meridianPluginLine.ReplaceAllString(out, "")
	if hadPoweredBy {
		out = openCodeEnvBlock.ReplaceAllString(out, "\n")
	}
	out = stripDuplicateOpenCodeEnv(out)
	out = extraBlankLines.ReplaceAllString(out, "\n\n")
	out = strings.TrimRight(out, " \t")
	return out
}

func stripDuplicateOpenCodeEnv(text string) string {
	first := strings.Index(text, openCodeEnvPreamble)
	if first < 0 {
		return text
	}
	rest := text[first+len(openCodeEnvPreamble):]
	secondRel := strings.Index(rest, openCodeEnvPreamble)
	if secondRel < 0 {
		return text
	}
	second := first + len(openCodeEnvPreamble) + secondRel
	endRel := strings.Index(text[second:], "</env>")
	if endRel < 0 {
		if lineEnd := strings.Index(text[second:], "\n"); lineEnd >= 0 {
			return text[:second] + text[second+lineEnd+1:]
		}
		return text[:second]
	}
	end := second + endRel + len("</env>")
	if end < len(text) && text[end] == '\n' {
		end++
	}
	return text[:second] + text[end:]
}

func scrubOpenCodeSystemReminderScopedText(text string) string {
	if !strings.Contains(text, "<system-reminder>") {
		return text
	}
	locs := systemReminderRegex.FindAllStringIndex(text, -1)
	if len(locs) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	changed := false
	for _, loc := range locs {
		_, _ = b.WriteString(text[prev:loc[0]])
		block := text[loc[0]:loc[1]]
		scrubbed := ScrubOpenCodeText(block)
		if scrubbed != block {
			changed = true
		}
		_, _ = b.WriteString(scrubbed)
		prev = loc[1]
	}
	if !changed {
		return text
	}
	_, _ = b.WriteString(text[prev:])
	return b.String()
}

// ScrubOpenCode scans an Anthropic /v1/messages body and strips OpenCode
// fingerprints from system text. User messages are only rewritten inside
// <system-reminder> blocks so ordinary chat that mentions OpenCode is left
// alone. If nothing matches, the original slice is returned.
func ScrubOpenCode(body []byte) ([]byte, bool) {
	if len(body) == 0 {
		return body, false
	}
	out := body
	changed := false

	sys := gjson.GetBytes(out, "system")
	if sys.Exists() {
		switch {
		case sys.Type == gjson.String:
			scrubbed := ScrubOpenCodeText(sys.String())
			if scrubbed != sys.String() {
				if next, err := sjson.SetBytes(out, "system", scrubbed); err == nil {
					out = next
					changed = true
				}
			}
		case sys.IsArray():
			idx := 0
			sys.ForEach(func(_, item gjson.Result) bool {
				if item.Get("type").String() == "text" {
					t := item.Get("text")
					if t.Exists() && t.Type == gjson.String {
						scrubbed := ScrubOpenCodeText(t.String())
						if scrubbed != t.String() {
							path := fmt.Sprintf("system.%d.text", idx)
							if next, err := sjson.SetBytes(out, path, scrubbed); err == nil {
								out = next
								changed = true
							}
						}
					}
				}
				idx++
				return true
			})
		}
	}

	messages := gjson.GetBytes(out, "messages")
	if messages.IsArray() {
		msgIdx := -1
		messages.ForEach(func(_, msg gjson.Result) bool {
			msgIdx++
			content := msg.Get("content")
			if !content.Exists() {
				return true
			}
			switch {
			case content.Type == gjson.String:
				scrubbed := scrubOpenCodeSystemReminderScopedText(content.String())
				if scrubbed != content.String() {
					path := fmt.Sprintf("messages.%d.content", msgIdx)
					if next, err := sjson.SetBytes(out, path, scrubbed); err == nil {
						out = next
						changed = true
					}
				}
			case content.IsArray():
				contentIdx := -1
				content.ForEach(func(_, block gjson.Result) bool {
					contentIdx++
					if block.Get("type").String() != "text" {
						return true
					}
					t := block.Get("text")
					if !t.Exists() || t.Type != gjson.String {
						return true
					}
					scrubbed := scrubOpenCodeSystemReminderScopedText(t.String())
					if scrubbed != t.String() {
						path := fmt.Sprintf("messages.%d.content.%d.text", msgIdx, contentIdx)
						if next, err := sjson.SetBytes(out, path, scrubbed); err == nil {
							out = next
							changed = true
						}
					}
					return true
				})
			}
			return true
		})
	}

	if !changed {
		return body, false
	}
	return out, true
}
