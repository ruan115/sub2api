package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAnthropicNonStreamBridgeForcesUpstreamSSEAndPreservesExtendedFields(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstreamBodies := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBodies <- body
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req-native-non-stream-bridge")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_bridge\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-test\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":7,\"cache_read_input_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"reason\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig-123\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool_1\",\"name\":\"read_file\",\"input\":{}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"/tmp/a\\\"}\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":5,\"iterations\":[{\"type\":\"message\",\"input_tokens\":7,\"output_tokens\":5}]}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "native-non-stream-bridge", upstream.URL, 0, nil, map[string]any{"access_token": "token"})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":false,"max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(strings.ToLower(response.Header().Get("Content-Type")), "text/event-stream") {
		t.Fatalf("client content-type=%q, want JSON", response.Header().Get("Content-Type"))
	}
	upstreamBody := <-upstreamBodies
	if !gjson.GetBytes(upstreamBody, "stream").Bool() {
		t.Fatalf("upstream stream was not forced: %s", upstreamBody)
	}
	result := response.Body.Bytes()
	checks := map[string]string{
		"content.0.thinking":   "reason",
		"content.0.signature":  "sig-123",
		"content.1.input.path": "/tmp/a",
		"stop_reason":          "tool_use",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(result, path).String(); got != want {
			t.Fatalf("%s=%q want=%q body=%s", path, got, want, result)
		}
	}
	if got := gjson.GetBytes(result, "usage.iterations.0.output_tokens").Int(); got != 5 {
		t.Fatalf("usage.iterations output=%d body=%s", got, result)
	}
	var recordedStream int
	if err := a.db.QueryRow(`SELECT stream FROM usage_logs WHERE account_id = ? AND request_id = ?`, created.ID, "req-native-non-stream-bridge").Scan(&recordedStream); err != nil {
		t.Fatal(err)
	}
	if recordedStream != 0 {
		t.Fatalf("recorded stream=%d, want client mode 0", recordedStream)
	}
}

func TestAggregateAnthropicSSERejectsMissingTerminalEvent(t *testing.T) {
	body := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_incomplete\"}}\n\n")
	if result, upstreamError, err := aggregateAnthropicSSE(body); err == nil || result != nil || upstreamError != nil {
		t.Fatalf("result=%s upstreamError=%s err=%v", result, upstreamError, err)
	}
}

func TestAggregateAnthropicSSEProducesValidJSON(t *testing.T) {
	body := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_valid\",\"content\":[]}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	result, upstreamError, err := aggregateAnthropicSSE(body)
	if err != nil || upstreamError != nil || !json.Valid(result) {
		t.Fatalf("result=%s upstreamError=%s err=%v", result, upstreamError, err)
	}
}
