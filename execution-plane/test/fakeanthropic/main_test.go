package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMessagesAndCountTokens(t *testing.T) {
	handler := server{}.handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-fake-1","messages":[{"role":"user","content":"hello"}]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Header().Get("X-Request-Id") == "" {
		t.Fatalf("unexpected response: code=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(response["id"].(string), "msg_fake_") {
		t.Fatalf("unexpected message id: %v", response["id"])
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"messages":[{"content":"hello world"}]}`))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "input_tokens") {
		t.Fatalf("unexpected count response: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestStreamingAndOverload(t *testing.T) {
	handler := server{}.handler()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"stream":true,"messages":[]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "event: message_stop") {
		t.Fatalf("unexpected stream response: code=%d body=%s", recorder.Code, recorder.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"messages":[]}`))
	request.Header.Set("X-Fake-Scenario", "overloaded")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "overloaded_error") {
		t.Fatalf("unexpected overload response: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
