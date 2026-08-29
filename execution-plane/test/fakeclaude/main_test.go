package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestTextScenarioIsDeterministicAndDoesNotEchoPrompt(t *testing.T) {
	prompt := `{"type":"user","message":{"content":"a private prompt"}}`
	runOnce := func() string {
		var stdout, stderr bytes.Buffer
		code := run(strings.NewReader(prompt), &stdout, &stderr, nil, func(string) string { return "" })
		if code != 0 {
			t.Fatalf("run failed: code=%d stderr=%s", code, stderr.String())
		}
		return stdout.String()
	}
	first := runOnce()
	second := runOnce()
	if first != second {
		t.Fatal("fake output is not deterministic")
	}
	if strings.Contains(first, "private prompt") {
		t.Fatal("fake output leaked prompt")
	}
	for _, line := range strings.Split(strings.TrimSpace(first), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid stream-json event: %v", err)
		}
	}
}

func TestToolAndBillingScenarios(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(strings.NewReader("request"), &stdout, &stderr, []string{"--session-id", "fixed"}, func(key string) string {
		if key == "FAKE_CLAUDE_SCENARIO" {
			return "tool"
		}
		return ""
	})
	if code != 0 || !strings.Contains(stdout.String(), "toolu_fake_001") || !strings.Contains(stdout.String(), `"session_id":"fixed"`) {
		t.Fatalf("unexpected tool result: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run(strings.NewReader("request"), &stdout, &stderr, nil, func(key string) string {
		if key == "FAKE_CLAUDE_SCENARIO" {
			return "billing400"
		}
		return ""
	})
	if code == 0 || !strings.Contains(stderr.String(), "status_code=400") {
		t.Fatalf("billing scenario not emitted: code=%d stderr=%s", code, stderr.String())
	}
}
