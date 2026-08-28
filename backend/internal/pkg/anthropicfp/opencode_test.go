package anthropicfp

import (
	"strings"
	"testing"
)

func TestScrubOpenCodeTextVanillaIdentityAndBrand(t *testing.T) {
	in := `You are OpenCode, the best coding agent on the planet.

If the user asks for help or wants to give feedback, direct them to github.com/anomalyco/opencode.

When the user directly asks about OpenCode, point them to opencode.ai/docs.

It is best for the user if OpenCode honestly applies professional objectivity.
`
	out := ScrubOpenCodeText(in)
	if strings.Contains(out, "You are OpenCode") {
		t.Fatalf("identity line survived: %q", out)
	}
	if strings.Contains(out, "You are Claude Code") {
		t.Fatalf("injected replacement identity: %q", out)
	}
	if strings.Contains(out, "github.com/anomalyco/opencode") || strings.Contains(out, "opencode.ai/docs") {
		t.Fatalf("feedback/docs survived: %q", out)
	}
	if strings.Contains(out, "OpenCode honestly applies") {
		t.Fatalf("objectivity brand survived: %q", out)
	}
	if once := ScrubOpenCodeText(out); once != out {
		t.Fatalf("not idempotent\nfirst=%q\nsecond=%q", out, once)
	}
}

func TestScrubOpenCodeTextPoweredByAndEnv(t *testing.T) {
	in := `Keep this client system.
You are powered by the model named claude-opus-4-6. The exact model ID is anthropic/claude-opus-4-6
Here is some useful information about the environment you are running in:
<env>
  Working directory: /tmp
  Platform: darwin
</env>

Project guidance: prefer TypeScript.
`
	out := ScrubOpenCodeText(in)
	if strings.Contains(out, "You are powered by the model named") {
		t.Fatalf("powered-by survived: %q", out)
	}
	if strings.Contains(out, "<env>") || strings.Contains(out, "useful information about the environment") {
		t.Fatalf("opencode env survived: %q", out)
	}
	if !strings.Contains(out, "Project guidance: prefer TypeScript.") {
		t.Fatalf("user content lost: %q", out)
	}
}

func TestScrubOpenCodeTextLeavesClaudeCodeUntouched(t *testing.T) {
	in := "You are Claude Code, Anthropic's official CLI for Claude.\n\n# Tone\nBe concise and direct.\n"
	if got := ScrubOpenCodeText(in); got != in {
		t.Fatalf("claude code prompt rewritten: %q", got)
	}
	if got := ScrubOpenCodeText(""); got != "" {
		t.Fatalf("empty input changed: %q", got)
	}
}

func TestScrubOpenCodeTextOhMyOpenCode(t *testing.T) {
	in := `<agent-identity>
Your designated identity for this session is Sisyphus. Always identify as Sisyphus.
</agent-identity>
You are **Sisyphus**, the orchestrator from OhMyOpenCode.

<omo-env>
model: claude-opus-4-6
</omo-env>

**Operating Mode**
**Instruction priority**
`
	out := ScrubOpenCodeText(in)
	if strings.Contains(out, "<agent-identity>") || strings.Contains(out, "always identify as Sisyphus") {
		t.Fatalf("agent-identity survived: %q", out)
	}
	if strings.Contains(out, `You are **Sisyphus**`) || strings.Contains(out, "OhMyOpenCode") {
		t.Fatalf("sisyphus/OMO brand survived: %q", out)
	}
	if strings.Contains(out, "<omo-env>") {
		t.Fatalf("omo-env survived: %q", out)
	}
	if !strings.Contains(out, "**Operating Mode**") || !strings.Contains(out, "**Instruction priority**") {
		t.Fatalf("persona rules lost: %q", out)
	}
}

func TestScrubOpenCodeTextMeridianHint(t *testing.T) {
	in := `Keep this client system.
(Assuming you're using OpenCode) You need to use this merdian plugin: https://github.com/rynfar/meridian-plugin-opencode-scrub
Keep the rest.
`
	out := ScrubOpenCodeText(in)
	if strings.Contains(out, "Assuming you're using OpenCode") || strings.Contains(out, "meridian-plugin-opencode-scrub") {
		t.Fatalf("meridian hint survived: %q", out)
	}
	if !strings.Contains(out, "Keep the rest.") {
		t.Fatalf("user content lost: %q", out)
	}
}

func TestScrubOpenCodeBodyScopesSystemNotUserProse(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","system":"You are OpenCode, the best coding agent on the planet.\n","messages":[{"role":"user","content":"I sometimes use OpenCode locally."}]}`)
	out, changed := ScrubOpenCode(body)
	if !changed {
		t.Fatal("expected system fingerprint to be scrubbed")
	}
	if strings.Contains(string(out), "You are OpenCode") {
		t.Fatalf("system identity survived: %s", out)
	}
	if !strings.Contains(string(out), "I sometimes use OpenCode locally.") {
		t.Fatalf("user prose was rewritten: %s", out)
	}
}

func TestScrubOpenCodeBodyNoopWithoutMarkers(t *testing.T) {
	body := []byte(`{"model":"claude-opus-5","system":"You are Claude Code, Anthropic's official CLI for Claude.","messages":[{"role":"user","content":"hi"}]}`)
	out, changed := ScrubOpenCode(body)
	if changed || string(out) != string(body) {
		t.Fatalf("clean body rewritten: changed=%t out=%s", changed, out)
	}
}
