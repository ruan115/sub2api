package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSanitizeOpenCodeText_RewritesCanonicalSentence(t *testing.T) {
	in := "You are OpenCode, the best coding agent on the planet."
	got := sanitizeSystemText(in)
	require.Equal(t, strings.TrimSpace(claudeCodeSystemPrompt), got)
}

func TestSanitizeOpenCodeText_LeavesPoweredByWhenNotScrubbing(t *testing.T) {
	in := "You are powered by the model named claude-opus-4-6. The exact model ID is anthropic/claude-opus-4-6"
	require.Equal(t, in, sanitizeSystemText(in))
}
