// Command fakeanthropic is a deterministic Anthropic-compatible test server.
// It is for local and CI tests only; production code must forward real IDs and
// usage returned by the selected upstream account.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type server struct{}

func main() {
	address := flag.String("listen", "127.0.0.1:19080", "listen address")
	flag.Parse()
	httpServer := &http.Server{
		Addr:              *address,
		Handler:           server{}.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("fake Anthropic server listening on %s", *address)
	log.Fatal(httpServer.ListenAndServe())
}

func (server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"data":     []map[string]any{{"id": "claude-fake-1", "type": "model", "display_name": "Claude Fake 1"}},
			"has_more": false,
		})
	})
	mux.HandleFunc("POST /v1/messages/count_tokens", countTokens)
	mux.HandleFunc("POST /v1/messages", messages)
	return mux
}

func messages(w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 4<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request")
		return
	}
	if request.Header.Get("X-Fake-Scenario") == "overloaded" {
		writeError(w, http.StatusServiceUnavailable, "overloaded_error", "fake upstream overloaded")
		return
	}

	var input struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON")
		return
	}
	if input.Model == "" {
		input.Model = "claude-fake-1"
	}
	digest := sha256.Sum256(body)
	id := "msg_fake_" + hex.EncodeToString(digest[:8])
	text := "fake-anthropic-response:" + hex.EncodeToString(digest[8:16])
	inputTokens := tokenEstimate(body)

	if input.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Request-Id", id)
		flusher, _ := w.(http.Flusher)
		writeSSE(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "model": input.Model,
			"content": []any{}, "usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0},
		}})
		writeSSE(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		writeSSE(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
		writeSSE(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		writeSSE(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 8}})
		writeSSE(w, "message_stop", map[string]any{"type": "message_stop"})
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	w.Header().Set("X-Request-Id", id)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": input.Model,
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 8},
	})
}

func countTokens(w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 4<<20))
	if err != nil || !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": tokenEstimate(body)})
}

func tokenEstimate(body []byte) int {
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return 1
	}
	return len(fields)
}

func writeSSE(w io.Writer, event string, value any) {
	payload, _ := json.Marshal(value)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
}

func writeError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
