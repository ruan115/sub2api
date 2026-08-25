package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

const maxChatCompletionsBodySize = 32 << 20

// handleChatCompletions is a protocol adapter over handleClaudeGateway. Account
// selection, proxy binding, compatibility retries, billing, and account health
// therefore remain owned by the same CCMAX gateway path as /v1/messages.
func (a *app) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	secret := bearerOrAPIKey(r)
	if secret == "" {
		writeOpenAIChatError(w, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}
	if _, err := a.authenticateGatewayKey(secret); err != nil {
		writeOpenAIChatError(w, http.StatusUnauthorized, "authentication_error", "Invalid API key")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxChatCompletionsBodySize))
	if err != nil {
		writeOpenAIChatError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body is too large")
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeOpenAIChatError(w, http.StatusBadRequest, "invalid_request_error", "Request body is empty")
		return
	}

	var chatRequest apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &chatRequest); err != nil {
		writeOpenAIChatError(w, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return
	}
	chatRequest.Model = strings.TrimSpace(chatRequest.Model)
	if chatRequest.Model == "" {
		writeOpenAIChatError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	responsesRequest, err := apicompat.ChatCompletionsToResponses(&chatRequest)
	if err != nil {
		writeOpenAIChatError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	anthropicRequest, err := apicompat.ResponsesToAnthropicRequest(responsesRequest)
	if err != nil {
		writeOpenAIChatError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// Sub2API always streams from Anthropic, then either relays converted chunks
	// or buffers them into a non-streaming Chat Completions response.
	anthropicRequest.Stream = true
	anthropicBody, err := json.Marshal(anthropicRequest)
	if err != nil {
		writeOpenAIChatError(w, http.StatusBadRequest, "invalid_request_error", "Failed to convert request body")
		return
	}

	includeUsage := chatRequest.StreamOptions != nil && chatRequest.StreamOptions.IncludeUsage
	adapter := newChatCompletionsResponseWriter(w, chatRequest.Model, chatRequest.Stream, includeUsage)
	ctx := context.WithValue(r.Context(), gatewayProtocolContextKey{}, gatewayProtocolContext{
		openAIChat: true, clientStream: chatRequest.Stream,
	})
	upstreamRequest := r.Clone(ctx)
	upstreamRequest.Body = io.NopCloser(bytes.NewReader(anthropicBody))
	upstreamRequest.ContentLength = int64(len(anthropicBody))
	upstreamRequest.Header = r.Header.Clone()
	upstreamRequest.Header.Set("Content-Type", "application/json")
	upstreamRequest.Header.Set("Content-Length", fmt.Sprintf("%d", len(anthropicBody)))

	a.handleClaudeGateway(adapter, upstreamRequest, false)
	adapter.finish()
}

type chatCompletionsResponseWriter struct {
	target          http.ResponseWriter
	header          http.Header
	status          int
	body            bytes.Buffer
	pending         []byte
	originalModel   string
	clientStream    bool
	committed       bool
	done            bool
	streamError     bool
	writeErr        error
	sawMessageStart bool
	anthropic       *apicompat.AnthropicEventToResponsesState
	chat            *apicompat.ResponsesEventToChatState
}

func newChatCompletionsResponseWriter(target http.ResponseWriter, model string, stream, includeUsage bool) *chatCompletionsResponseWriter {
	anthropicState := apicompat.NewAnthropicEventToResponsesState()
	anthropicState.Model = model
	chatState := apicompat.NewResponsesEventToChatState()
	chatState.Model = model
	chatState.IncludeUsage = includeUsage
	return &chatCompletionsResponseWriter{
		target: target, header: make(http.Header), originalModel: model,
		clientStream: stream,
		anthropic:    anthropicState, chat: chatState,
	}
}

func (w *chatCompletionsResponseWriter) Header() http.Header {
	return w.header
}

func (w *chatCompletionsResponseWriter) Unwrap() http.ResponseWriter {
	return w.target
}

func (w *chatCompletionsResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *chatCompletionsResponseWriter) Write(payload []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if !w.clientStream || w.status < http.StatusOK || w.status >= http.StatusMultipleChoices {
		return w.body.Write(payload)
	}
	w.pending = append(w.pending, payload...)
	w.processPendingBlocks(false)
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(payload), nil
}

func (w *chatCompletionsResponseWriter) Flush() {
	if !w.clientStream || w.writeErr != nil {
		return
	}
	w.processPendingBlocks(false)
	if w.committed {
		if flusher, ok := w.target.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (w *chatCompletionsResponseWriter) finish() {
	if w.writeErr != nil {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status < http.StatusOK || w.status >= http.StatusMultipleChoices {
		w.finishErrorResponse()
		return
	}
	if !w.clientStream {
		w.finishBufferedResponse()
		return
	}

	w.processPendingBlocks(true)
	if w.writeErr != nil {
		return
	}
	if !w.streamError {
		for _, event := range apicompat.FinalizeAnthropicResponsesStream(w.anthropic) {
			w.writeResponsesEvent(event)
		}
		for _, chunk := range apicompat.FinalizeResponsesChatStream(w.chat) {
			w.writeChunk(chunk)
		}
	}
	if !w.committed {
		writeOpenAIChatError(w.target, http.StatusBadGateway, "server_error", "Upstream stream ended without a response")
		return
	}
	if !w.done && w.writeErr == nil {
		_, w.writeErr = io.WriteString(w.target, "data: [DONE]\n\n")
		w.done = true
	}
	if flusher, ok := w.target.(http.Flusher); ok && w.writeErr == nil {
		flusher.Flush()
	}
}

func (w *chatCompletionsResponseWriter) finishErrorResponse() {
	errorType, message := openAIChatErrorDetails(w.body.Bytes(), w.status)
	copyChatResponseHeaders(w.target.Header(), w.header)
	writeOpenAIChatError(w.target, w.status, errorType, message)
}

func (w *chatCompletionsResponseWriter) finishBufferedResponse() {
	var finalResponse *apicompat.ResponsesResponse
	var streamErrorType, streamErrorMessage string
	for _, block := range splitCompleteSSEBlocks(w.body.Bytes(), true) {
		eventType, eventBody := gatewaySSEEvent(block)
		if eventType == "error" {
			streamErrorType, streamErrorMessage = openAIChatErrorDetails(eventBody, http.StatusBadGateway)
			continue
		}
		var event apicompat.AnthropicStreamEvent
		if len(eventBody) == 0 || json.Unmarshal(eventBody, &event) != nil {
			continue
		}
		if event.Type == "" {
			event.Type = eventType
		}
		if event.Type == "message_start" {
			w.sawMessageStart = true
		}
		for _, converted := range apicompat.AnthropicEventToResponsesEvents(&event, w.anthropic) {
			if converted.Response != nil && isResponsesTerminalEvent(converted.Type) {
				finalResponse = converted.Response
			}
		}
	}
	if streamErrorMessage != "" {
		copyChatResponseHeaders(w.target.Header(), w.header)
		writeOpenAIChatError(w.target, http.StatusBadGateway, streamErrorType, streamErrorMessage)
		return
	}
	if !w.sawMessageStart {
		finalResponse = nil
	}
	if finalResponse == nil && w.sawMessageStart {
		for _, converted := range apicompat.FinalizeAnthropicResponsesStream(w.anthropic) {
			if converted.Response != nil && isResponsesTerminalEvent(converted.Type) {
				finalResponse = converted.Response
			}
		}
	}
	if finalResponse == nil {
		// Accept a native non-streaming Anthropic response from compatible
		// upstreams even though Sub2API requests SSE.
		var anthropicResponse apicompat.AnthropicResponse
		if json.Unmarshal(w.body.Bytes(), &anthropicResponse) == nil && anthropicResponse.ID != "" {
			finalResponse = apicompat.AnthropicToResponsesResponse(&anthropicResponse)
		}
	}
	if finalResponse == nil {
		writeOpenAIChatError(w.target, http.StatusBadGateway, "server_error", "Upstream stream ended without a response")
		return
	}
	copyChatResponseHeaders(w.target.Header(), w.header)
	w.target.Header().Set("Content-Type", "application/json; charset=utf-8")
	writeJSON(w.target, http.StatusOK, apicompat.ResponsesToChatCompletions(finalResponse, w.originalModel))
}

func (w *chatCompletionsResponseWriter) processPendingBlocks(final bool) {
	blocks, remainder := takeCompleteSSEBlocks(w.pending)
	for _, block := range blocks {
		w.processStreamBlock(block)
		if w.writeErr != nil {
			return
		}
	}
	w.pending = remainder
	if final && len(bytes.TrimSpace(w.pending)) > 0 {
		w.processStreamBlock(w.pending)
		w.pending = nil
	}
}

func (w *chatCompletionsResponseWriter) processStreamBlock(block []byte) {
	eventType, eventBody := gatewaySSEEvent(block)
	if eventType == "error" {
		w.commitStream()
		errorType, message := openAIChatErrorDetails(eventBody, http.StatusBadGateway)
		payload, _ := json.Marshal(map[string]any{"error": map[string]string{"type": errorType, "message": message}})
		_, w.writeErr = fmt.Fprintf(w.target, "data: %s\n\n", payload)
		w.streamError = true
		return
	}
	if len(eventBody) == 0 {
		return
	}
	var event apicompat.AnthropicStreamEvent
	if err := json.Unmarshal(eventBody, &event); err != nil {
		return
	}
	if event.Type == "" {
		event.Type = eventType
	}
	if event.Type == "" || event.Type == "ping" {
		return
	}
	if event.Type == "message_start" {
		w.sawMessageStart = true
	}
	for _, converted := range apicompat.AnthropicEventToResponsesEvents(&event, w.anthropic) {
		w.writeResponsesEvent(converted)
		if w.writeErr != nil {
			return
		}
	}
}

func (w *chatCompletionsResponseWriter) writeResponsesEvent(event apicompat.ResponsesStreamEvent) {
	for _, chunk := range apicompat.ResponsesEventToChatChunks(&event, w.chat) {
		w.writeChunk(chunk)
		if w.writeErr != nil {
			return
		}
	}
}

func (w *chatCompletionsResponseWriter) writeChunk(chunk apicompat.ChatCompletionsChunk) {
	w.commitStream()
	if w.writeErr != nil {
		return
	}
	line, err := apicompat.ChatChunkToSSE(chunk)
	if err != nil {
		w.writeErr = err
		return
	}
	_, w.writeErr = io.WriteString(w.target, line)
}

func (w *chatCompletionsResponseWriter) commitStream() {
	if w.committed {
		return
	}
	copyChatResponseHeaders(w.target.Header(), w.header)
	w.target.Header().Set("Content-Type", "text/event-stream")
	w.target.Header().Set("Cache-Control", "no-cache")
	w.target.Header().Set("Connection", "keep-alive")
	w.target.Header().Set("X-Accel-Buffering", "no")
	w.target.WriteHeader(http.StatusOK)
	w.committed = true
}

func takeCompleteSSEBlocks(data []byte) ([][]byte, []byte) {
	var blocks [][]byte
	start := 0
	for start < len(data) {
		lf := bytes.Index(data[start:], []byte("\n\n"))
		crlf := bytes.Index(data[start:], []byte("\r\n\r\n"))
		end, delimiterLength := -1, 0
		if lf >= 0 {
			end, delimiterLength = start+lf, 2
		}
		if crlf >= 0 && (end < 0 || start+crlf < end) {
			end, delimiterLength = start+crlf, 4
		}
		if end < 0 {
			break
		}
		blockEnd := end + delimiterLength
		blocks = append(blocks, append([]byte(nil), data[start:blockEnd]...))
		start = blockEnd
	}
	return blocks, append([]byte(nil), data[start:]...)
}

func splitCompleteSSEBlocks(data []byte, includeRemainder bool) [][]byte {
	blocks, remainder := takeCompleteSSEBlocks(data)
	if includeRemainder && len(bytes.TrimSpace(remainder)) > 0 {
		blocks = append(blocks, remainder)
	}
	return blocks
}

func isResponsesTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "response.done":
		return true
	default:
		return false
	}
}

func openAIChatErrorDetails(body []byte, status int) (string, string) {
	errorType := "server_error"
	if status == http.StatusBadRequest {
		errorType = "invalid_request_error"
	} else if status == http.StatusUnauthorized {
		errorType = "authentication_error"
	} else if status == http.StatusForbidden {
		errorType = "permission_error"
	} else if status == http.StatusRequestEntityTooLarge {
		errorType = "invalid_request_error"
	} else if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}
	message := strings.TrimSpace(http.StatusText(status))
	var object map[string]any
	if json.Unmarshal(body, &object) == nil {
		switch value := object["error"].(type) {
		case string:
			if strings.TrimSpace(value) != "" {
				message = strings.TrimSpace(value)
			}
		case map[string]any:
			if valueType, ok := value["type"].(string); ok && strings.TrimSpace(valueType) != "" {
				errorType = strings.TrimSpace(valueType)
			}
			if valueMessage, ok := value["message"].(string); ok && strings.TrimSpace(valueMessage) != "" {
				message = strings.TrimSpace(valueMessage)
			}
		}
	}
	if message == "" {
		message = "Request failed"
	}
	return errorType, message
}

func writeOpenAIChatError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"type": errorType, "message": message},
	})
}

func copyChatResponseHeaders(target, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") || strings.EqualFold(key, "Content-Type") {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

var _ http.Flusher = (*chatCompletionsResponseWriter)(nil)
var _ http.ResponseWriter = (*chatCompletionsResponseWriter)(nil)
