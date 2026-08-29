// Command fakeclaude emulates the small subset of Claude Code stream-json used
// by the worker adapter. It never contacts a network service or logs prompts.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type streamEvent map[string]any

func main() {
	os.Exit(run(os.Stdin, os.Stdout, os.Stderr, os.Args[1:], os.Getenv))
}

func run(stdin io.Reader, stdout, stderr io.Writer, args []string, getenv func(string) string) int {
	input, err := io.ReadAll(io.LimitReader(stdin, 4<<20))
	if err != nil {
		fmt.Fprintf(stderr, "fakeclaude: read input: %v\n", err)
		return 1
	}

	scenario := strings.TrimSpace(getenv("FAKE_CLAUDE_SCENARIO"))
	if scenario == "" {
		scenario = "text"
	}
	if scenario == "billing400" {
		fmt.Fprintln(stderr, "status_code=400, Third-party apps now draw from your extra usage, not your plan limits.")
		return 1
	}
	if scenario == "error" {
		fmt.Fprintln(stderr, "fakeclaude: deterministic failure")
		return 1
	}

	digest := sha256.Sum256(input)
	fingerprint := hex.EncodeToString(digest[:8])
	sessionID := argumentValue(args, "--resume")
	if sessionID == "" {
		sessionID = argumentValue(args, "--session-id")
	}
	if sessionID == "" {
		sessionID = "fake-session-" + fingerprint
	}

	writer := bufio.NewWriter(stdout)
	defer writer.Flush()
	if err := emit(writer, streamEvent{
		"type":       "system",
		"subtype":    "init",
		"session_id": sessionID,
	}); err != nil {
		fmt.Fprintf(stderr, "fakeclaude: write init event: %v\n", err)
		return 1
	}

	if scenario == "tool" && !strings.Contains(string(input), "tool_result") {
		if err := emit(writer, assistantEvent(sessionID, []map[string]any{{
			"type": "tool_use",
			"id":   "toolu_fake_001",
			"name": "fake_lookup",
			"input": map[string]any{
				"fingerprint": fingerprint,
			},
		}})); err != nil {
			fmt.Fprintf(stderr, "fakeclaude: write tool event: %v\n", err)
			return 1
		}
		return emitResult(writer, stderr, sessionID, "tool_use", "")
	}

	text := "fake-claude-response:" + fingerprint
	if strings.Contains(string(input), "tool_result") {
		text = "fake-claude-tool-result-accepted:" + fingerprint
	}
	if err := emit(writer, assistantEvent(sessionID, []map[string]any{{
		"type": "text",
		"text": text,
	}})); err != nil {
		fmt.Fprintf(stderr, "fakeclaude: write assistant event: %v\n", err)
		return 1
	}
	return emitResult(writer, stderr, sessionID, "success", text)
}

func assistantEvent(sessionID string, content []map[string]any) streamEvent {
	return streamEvent{
		"type":       "assistant",
		"session_id": sessionID,
		"message": map[string]any{
			"role":    "assistant",
			"content": content,
		},
	}
}

func emitResult(writer io.Writer, stderr io.Writer, sessionID, subtype, result string) int {
	if err := emit(writer, streamEvent{
		"type":       "result",
		"subtype":    subtype,
		"is_error":   false,
		"session_id": sessionID,
		"result":     result,
	}); err != nil {
		fmt.Fprintf(stderr, "fakeclaude: write result event: %v\n", err)
		return 1
	}
	return 0
}

func emit(writer io.Writer, event streamEvent) error {
	return json.NewEncoder(writer).Encode(event)
}

func argumentValue(args []string, name string) string {
	for index, value := range args {
		if value == name && index+1 < len(args) {
			return args[index+1]
		}
		if strings.HasPrefix(value, name+"=") {
			return strings.TrimPrefix(value, name+"=")
		}
	}
	return ""
}
