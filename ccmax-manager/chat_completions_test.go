package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatCompletionsNonStreamUsesCCMaxPipeline(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan capturedClaudeRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- capturedClaudeRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req-chat-buffered")
		writeChatTestAnthropicStream(w, "Hello from CCMAX")
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "chat-buffered", upstream.URL, 0, nil, map[string]any{
		"access_token": "chat-buffered-token",
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-test","stream":false,"max_tokens":32,
		"messages":[{"role":"system","content":"Be concise."},{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	// This endpoint is never a native Claude Code request, even if the caller
	// supplies a Claude Code-looking User-Agent.
	request.Header.Set("User-Agent", "claude-cli/2.1.220 (external, cli)")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("content-type=%q", got)
	}
	var completion map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &completion); err != nil {
		t.Fatal(err)
	}
	if completion["object"] != "chat.completion" || completion["model"] != "claude-test" {
		t.Fatalf("completion identity=%#v", completion)
	}
	choices, _ := completion["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices=%#v", completion["choices"])
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "Hello from CCMAX" {
		t.Fatalf("message=%#v", message)
	}
	usage, _ := completion["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(4) || usage["completion_tokens"] != float64(2) || usage["total_tokens"] != float64(6) {
		t.Fatalf("usage=%#v", usage)
	}

	forwarded := <-captured
	if forwarded.Path != "/v1/messages" || forwarded.Query != "beta=true" {
		t.Fatalf("upstream target=%s?%s", forwarded.Path, forwarded.Query)
	}
	if forwarded.Body["stream"] != true {
		t.Fatalf("upstream stream=%#v", forwarded.Body["stream"])
	}
	if forwarded.Header.Get("X-App") != "cli" {
		t.Fatalf("mimic headers=%#v", forwarded.Header)
	}
	if system, _ := forwarded.Body["system"].([]any); len(system) == 0 {
		t.Fatalf("OAuth mimicry system blocks missing: %#v", forwarded.Body["system"])
	}

	var stream int
	var input, output, cacheRead int64
	if err := a.db.QueryRow(`SELECT stream, input_tokens, output_tokens, cache_read_tokens FROM usage_logs WHERE account_id = ? AND request_id = ?`, created.ID, "req-chat-buffered").Scan(&stream, &input, &output, &cacheRead); err != nil {
		t.Fatal(err)
	}
	if stream != 0 || input != 3 || output != 2 || cacheRead != 1 {
		t.Fatalf("recorded usage stream=%d tokens=%d/%d cache=%d", stream, input, output, cacheRead)
	}
}

func TestChatCompletionsStreamFlushesAndIncludesUsage(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("request-id", "req-chat-stream")
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_chat_stream\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-test\",\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":1}}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"streamed\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	created := createGatewayTestAccount(t, a, handler, "chat-stream", upstream.URL, 0, nil, map[string]any{
		"access_token": "chat-stream-token",
	})
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	request, _ := http.NewRequest(http.MethodPost, gateway.URL+"/v1/chat/completions", strings.NewReader(`{
		"model":"claude-test","stream":true,"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	responseCh := make(chan *http.Response, 1)
	errorCh := make(chan error, 1)
	go func() {
		response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
		if err != nil {
			errorCh <- err
			return
		}
		responseCh <- response
	}()

	var response *http.Response
	select {
	case response = <-responseCh:
	case err := <-errorCh:
		close(release)
		t.Fatal(err)
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Chat Completions response headers were buffered")
	}
	reader := bufio.NewReader(response.Body)
	firstLineCh := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		firstLineCh <- line
	}()
	select {
	case firstLine := <-firstLineCh:
		if !strings.Contains(firstLine, `"object":"chat.completion.chunk"`) || !strings.Contains(firstLine, `"role":"assistant"`) {
			close(release)
			t.Fatalf("first chunk=%q", firstLine)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("first Chat Completions chunk was buffered")
	}
	close(release)
	remainder, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	streamBody := string(remainder)
	if !strings.Contains(streamBody, `"content":"streamed"`) || !strings.Contains(streamBody, `"prompt_tokens":4`) || !strings.Contains(streamBody, "data: [DONE]") {
		t.Fatalf("stream body=%s", streamBody)
	}

	deadline := time.Now().Add(time.Second)
	for {
		var stream int
		err := a.db.QueryRow(`SELECT stream FROM usage_logs WHERE account_id = ? AND request_id = ?`, created.ID, "req-chat-stream").Scan(&stream)
		if err == nil {
			if stream != 1 {
				t.Fatalf("recorded stream=%d", stream)
			}
			break
		}
		if err != sql.ErrNoRows || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChatCompletionsPreservesUpstreamThinkingSignature(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "non_stream", true: "stream"}[stream], func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_signature\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-test\",\"stop_reason\":null,\"usage\":{\"input_tokens\":3}}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"reason\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"real-upstream-signature\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
				_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
				_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
				_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			}))
			defer upstream.Close()

			a, handler := newGatewayTestApp(t)
			defer a.db.Close()
			key := createGatewayTestKey(t, handler)
			createGatewayTestAccount(t, a, handler, "chat-signature", upstream.URL, 0, nil, map[string]any{
				"access_token": "chat-signature-token",
			})

			body := fmt.Sprintf(`{"model":"claude-test","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, stream)
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+key.Key)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), `"reasoning_signature":"real-upstream-signature"`) {
				t.Fatalf("upstream signature missing from Chat Completions response: %s", response.Body.String())
			}
		})
	}
}

func TestChatCompletionsAliasReturnsOpenAIErrorEnvelope(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)

	request := httptest.NewRequest(http.MethodPost, "/chat/completions", strings.NewReader(`{
		"model":"claude-test","messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Type == "" || envelope.Error.Message == "" {
		t.Fatalf("error envelope=%s", response.Body.String())
	}
}

func TestChatCompletionsRejectsAnthropicSilentModelDowngrade(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "non_stream"
		streamJSON := "false"
		if stream {
			name = "stream"
			streamJSON = "true"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("request-id", "req-openai-downgrade")
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_downgraded\",\"type\":\"message\",\"model\":\"claude-opus-4-8\",\"usage\":{\"input_tokens\":3}}}\n\n")
			}))
			defer upstream.Close()

			a, handler := newGatewayTestApp(t)
			defer a.db.Close()
			putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
				"name": "A 分组", "description": "模型降级控制", "rate_multiplier": 1, "status": "active",
				"reject_anthropic_downgrade_enabled": true,
			}, http.StatusOK, nil)
			key := createGatewayTestKey(t, handler)
			created := createGatewayTestAccount(t, a, handler, "chat-downgrade", upstream.URL, 0, nil, map[string]any{"access_token": "token"})

			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
				"model":"claude-fable-5","stream":`+streamJSON+`,
				"messages":[{"role":"user","content":"hello"}]
			}`))
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), `"type":"api_error"`) || !strings.Contains(response.Body.String(), "silently downgraded model") {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}

			var usageAccountID int64
			var model string
			var billedCost, actualCost float64
			if err := a.db.QueryRow(`SELECT account_id, model, billed_cost, actual_cost FROM usage_logs WHERE request_id = 'req-openai-downgrade'`).Scan(&usageAccountID, &model, &billedCost, &actualCost); err != nil {
				t.Fatal(err)
			}
			if usageAccountID != created.ID || model != "claude-opus-4-8" || billedCost != 0 || actualCost <= 0 {
				t.Fatalf("usage account=%d model=%q billed=%f actual=%f", usageAccountID, model, billedCost, actualCost)
			}
			var errorAccountID sql.NullInt64
			if err := a.db.QueryRow(`SELECT account_id FROM gateway_error_logs WHERE request_id = 'req-openai-downgrade'`).Scan(&errorAccountID); err != nil {
				t.Fatal(err)
			}
			if !errorAccountID.Valid || errorAccountID.Int64 != created.ID {
				t.Fatalf("gateway error account=%v, want %d", errorAccountID, created.ID)
			}
		})
	}
}

func TestChatCompletionsAuthenticatesBeforeParsing(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`not-json`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"type":"authentication_error"`) {
		t.Fatalf("error envelope=%s", response.Body.String())
	}
}

func TestChatCompletionsNonStreamRejectsMissingMessageStart(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "chat-no-start", upstream.URL, 0, nil, map[string]any{
		"access_token": "chat-no-start-token",
	})

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"claude-test","messages":[{"role":"user","content":"hello"}]
	}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"type":"server_error"`) {
		t.Fatalf("error envelope=%s", response.Body.String())
	}
}

func writeChatTestAnthropicStream(w http.ResponseWriter, text string) {
	_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_chat_buffered\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-test\",\"stop_reason\":null,\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":1}}}\n\n")
	start, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	delta, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}})
	_, _ = io.WriteString(w, "event: content_block_start\ndata: "+string(start)+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+string(delta)+"\n\n")
	_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n")
	_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}
