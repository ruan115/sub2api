package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const maxAnthropicNonStreamBridgeBody = 64 << 20

type anthropicNonStreamResponseWriter struct {
	target   http.ResponseWriter
	header   http.Header
	status   int
	body     bytes.Buffer
	writeErr error
}

func newAnthropicNonStreamResponseWriter(target http.ResponseWriter) *anthropicNonStreamResponseWriter {
	return &anthropicNonStreamResponseWriter{target: target, header: make(http.Header)}
}

func (w *anthropicNonStreamResponseWriter) Header() http.Header { return w.header }

func (w *anthropicNonStreamResponseWriter) Unwrap() http.ResponseWriter { return w.target }

func (w *anthropicNonStreamResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *anthropicNonStreamResponseWriter) Write(payload []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len()+len(payload) > maxAnthropicNonStreamBridgeBody {
		w.writeErr = errors.New("upstream response exceeds non-stream bridge limit")
		return 0, w.writeErr
	}
	return w.body.Write(payload)
}

// Flush intentionally buffers upstream SSE. The client requested a single
// non-streaming Anthropic JSON response.
func (w *anthropicNonStreamResponseWriter) Flush() {}

func (w *anthropicNonStreamResponseWriter) finish() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if w.writeErr != nil {
		copyAnthropicNonStreamHeaders(w.target.Header(), w.header)
		writeAnthropicGatewayError(w.target, http.StatusBadGateway, "upstream_error", "Failed to buffer upstream response")
		return
	}
	body := w.body.Bytes()
	if status < http.StatusOK || status >= http.StatusMultipleChoices || !looksLikeAnthropicSSE(body, w.header) {
		copyAnthropicNonStreamHeaders(w.target.Header(), w.header)
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			w.target.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		w.target.WriteHeader(status)
		_, _ = w.target.Write(body)
		return
	}

	result, upstreamError, err := aggregateAnthropicSSE(body)
	copyAnthropicNonStreamHeaders(w.target.Header(), w.header)
	w.target.Header().Set("Content-Type", "application/json; charset=utf-8")
	if upstreamError != nil {
		writeStatus := gatewaySSEErrorStatus(upstreamError)
		if writeStatus < http.StatusBadRequest {
			writeStatus = http.StatusBadGateway
		}
		w.target.WriteHeader(writeStatus)
		_, _ = w.target.Write(upstreamError)
		return
	}
	if err != nil {
		writeAnthropicGatewayError(w.target, http.StatusBadGateway, "upstream_error", "Upstream stream ended without a complete response")
		return
	}
	w.target.WriteHeader(http.StatusOK)
	_, _ = w.target.Write(result)
}

func copyAnthropicNonStreamHeaders(target, source http.Header) {
	// The buffered header was already filtered by the gateway forwarding path,
	// so no additional quota masking is needed here.
	copyGatewayResponseHeaders(target, source, false)
	for _, key := range []string{"Content-Length", "Transfer-Encoding", "Connection", "Cache-Control", "X-Accel-Buffering"} {
		target.Del(key)
	}
}

func looksLikeAnthropicSSE(body []byte, header http.Header) bool {
	if strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/event-stream") {
		return true
	}
	trimmed := bytes.TrimSpace(body)
	return bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:"))
}

func aggregateAnthropicSSE(body []byte) ([]byte, []byte, error) {
	var message map[string]any
	blocks := map[int]map[string]any{}
	partialInputs := map[int]*strings.Builder{}
	sawStart := false
	sawStop := false

	for _, block := range splitCompleteSSEBlocks(body, true) {
		eventType, eventBody := gatewaySSEEvent(block)
		if len(eventBody) == 0 {
			continue
		}
		var event map[string]any
		decoder := json.NewDecoder(bytes.NewReader(eventBody))
		decoder.UseNumber()
		if err := decoder.Decode(&event); err != nil {
			continue
		}
		if eventType == "" {
			eventType, _ = event["type"].(string)
		}
		switch eventType {
		case "error":
			return nil, append([]byte(nil), eventBody...), nil
		case "message_start":
			if value, ok := event["message"].(map[string]any); ok {
				message = cloneJSONMap(value)
				sawStart = true
			}
		case "content_block_start":
			index, ok := jsonIndex(event["index"])
			value, blockOK := event["content_block"].(map[string]any)
			if ok && blockOK {
				blocks[index] = cloneJSONMap(value)
			}
		case "content_block_delta":
			index, ok := jsonIndex(event["index"])
			delta, deltaOK := event["delta"].(map[string]any)
			if ok && deltaOK {
				applyAnthropicContentDelta(blocks, partialInputs, index, delta)
			}
		case "content_block_stop":
			if index, ok := jsonIndex(event["index"]); ok {
				if err := finishAnthropicToolInput(blocks[index], partialInputs[index]); err != nil {
					return nil, nil, err
				}
			}
		case "message_delta":
			if message == nil {
				message = map[string]any{}
			}
			if delta, ok := event["delta"].(map[string]any); ok {
				mergeJSONMap(message, delta)
			}
			if usage, ok := event["usage"].(map[string]any); ok {
				current, _ := message["usage"].(map[string]any)
				if current == nil {
					current = map[string]any{}
					message["usage"] = current
				}
				mergeJSONMap(current, usage)
			}
		case "message_stop":
			sawStop = true
		}
	}
	if !sawStart || !sawStop || message == nil {
		return nil, nil, errors.New("incomplete Anthropic SSE response")
	}

	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	content := make([]any, 0, len(indexes))
	for _, index := range indexes {
		if err := finishAnthropicToolInput(blocks[index], partialInputs[index]); err != nil {
			return nil, nil, err
		}
		content = append(content, blocks[index])
	}
	message["content"] = content
	if _, ok := message["type"]; !ok {
		message["type"] = "message"
	}
	if _, ok := message["role"]; !ok {
		message["role"] = "assistant"
	}
	encoded, err := json.Marshal(message)
	return encoded, nil, err
}

func applyAnthropicContentDelta(blocks map[int]map[string]any, partialInputs map[int]*strings.Builder, index int, delta map[string]any) {
	block := blocks[index]
	if block == nil {
		block = map[string]any{}
		blocks[index] = block
	}
	deltaType, _ := delta["type"].(string)
	switch deltaType {
	case "text_delta":
		appendJSONText(block, "text", delta["text"])
	case "thinking_delta":
		appendJSONText(block, "thinking", delta["thinking"])
	case "signature_delta":
		appendJSONText(block, "signature", delta["signature"])
	case "input_json_delta":
		builder := partialInputs[index]
		if builder == nil {
			builder = &strings.Builder{}
			partialInputs[index] = builder
		}
		if value, ok := delta["partial_json"].(string); ok {
			builder.WriteString(value)
		}
	case "citations_delta":
		if citation, ok := delta["citation"]; ok {
			citations, _ := block["citations"].([]any)
			block["citations"] = append(citations, citation)
		}
	default:
		for key, value := range delta {
			if key == "type" {
				continue
			}
			if text, ok := value.(string); ok {
				appendJSONText(block, key, text)
			} else {
				block[key] = value
			}
		}
	}
}

func appendJSONText(target map[string]any, key string, value any) {
	text, ok := value.(string)
	if !ok {
		return
	}
	current, _ := target[key].(string)
	target[key] = current + text
}

func finishAnthropicToolInput(block map[string]any, builder *strings.Builder) error {
	if block == nil || builder == nil {
		return nil
	}
	raw := strings.TrimSpace(builder.String())
	if raw == "" {
		return nil
	}
	var input any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return fmt.Errorf("invalid streamed tool input: %w", err)
	}
	block["input"] = input
	return nil
}

func jsonIndex(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := strconv.Atoi(typed.String())
		return parsed, err == nil
	case float64:
		return int(typed), true
	case int:
		return typed, true
	default:
		return 0, false
	}
}

func cloneJSONMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		if nested, ok := value.(map[string]any); ok {
			result[key] = cloneJSONMap(nested)
		} else {
			result[key] = value
		}
	}
	return result
}

func mergeJSONMap(target, source map[string]any) {
	for key, value := range source {
		if nested, ok := value.(map[string]any); ok {
			current, _ := target[key].(map[string]any)
			if current == nil {
				current = map[string]any{}
				target[key] = current
			}
			mergeJSONMap(current, nested)
			continue
		}
		target[key] = value
	}
}

var _ http.Flusher = (*anthropicNonStreamResponseWriter)(nil)
var _ io.Writer = (*anthropicNonStreamResponseWriter)(nil)
