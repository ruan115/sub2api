package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestValidateFilteredRequestFormat(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "temperature only", body: `{"temperature":0.7}`, wantErr: ""},
		{name: "top p only", body: `{"top_p":0.9}`, wantErr: ""},
		{name: "sampling conflict", body: `{"temperature":0.7,"top_p":0.9}`, wantErr: "cannot both be specified"},
		{name: "system string", body: `{"system":"answer briefly"}`, wantErr: ""},
		{name: "system text blocks", body: `{"system":[{"type":"text","text":"answer briefly","cache_control":{"type":"ephemeral"}}]}`, wantErr: ""},
		{name: "system block wrong type", body: `{"system":[{"type":"text","text":"ok"},{"role":"system","content":"bad"}]}`, wantErr: "system.1"},
		{name: "system object", body: `{"system":{"type":"text","text":"bad"}}`, wantErr: "system:"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateFilteredRequestFormat([]byte(test.body))
			if test.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error=%v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestValidatePreparedFilteredRequestFormatRejectsInvalidCompatibilitySystem(t *testing.T) {
	prepared := claudePreparedRequest{Body: []byte(`{
		"model":"claude-opus-5",
		"system":[{"type":"text","text":"billing"},"client system"],
		"messages":[{"role":"user","content":"hello"}]
	}`)}
	if err := validatePreparedFilteredRequestFormat(true, prepared); err == nil || !strings.Contains(err.Error(), "system.1") {
		t.Fatalf("prepared validation error=%v, want system.1", err)
	}
	if err := validatePreparedFilteredRequestFormat(false, prepared); err != nil {
		t.Fatalf("disabled prepared validation error=%v", err)
	}
}

func TestGatewayRequestFormatFilterRejectsBeforeAccountDispatch(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_ok","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "请求格式过滤", "rate_multiplier": 1, "status": "active",
		"normal_request_mode": true, "request_format_filter_enabled": true,
	}, http.StatusOK, nil)
	key := createGatewayTestKey(t, handler)
	account := createGatewayTestAccount(t, a, handler, "format-filter", upstream.URL, 0, nil, map[string]any{"access_token": "oauth-token"})

	requests := []struct {
		body    string
		message string
	}{
		{
			body:    `{"model":"claude-fable-5","max_tokens":32,"temperature":0.7,"top_p":0.9,"messages":[{"role":"user","content":"hello"}]}`,
			message: "cannot both be specified",
		},
		{
			body:    `{"model":"claude-fable-5","max_tokens":32,"system":[{"type":"text","text":"ok"},{"role":"system","content":"bad"}],"messages":[{"role":"user","content":"hello"}]}`,
			message: "system.1",
		},
	}
	for _, test := range requests {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(test.body))
		request.Header.Set("Authorization", "Bearer "+key.Key)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), test.message) {
			t.Fatalf("status=%d body=%s, want 400 containing %q", response.Code, response.Body.String(), test.message)
		}
		if got := jsonStringAt(response.Body.Bytes(), "error.type"); got != "invalid_request_error" {
			t.Fatalf("error.type=%q body=%s", got, response.Body.String())
		}
	}

	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("blocked requests reached upstream %d times", calls)
	}
	var rpmEvents, inflight int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_events WHERE account_id = ?`, account.ID).Scan(&rpmEvents); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_inflight WHERE account_id = ?`, account.ID).Scan(&inflight); err != nil {
		t.Fatal(err)
	}
	if rpmEvents != 0 || inflight != 0 {
		t.Fatalf("blocked request consumed account capacity: rpm_events=%d inflight=%d", rpmEvents, inflight)
	}
	var logCount, attributedAccounts int
	if err := a.db.QueryRow(`SELECT COUNT(*), COUNT(account_id) FROM gateway_error_logs WHERE category = ? AND status_code = ?`, requestFormatBlockedCategory, http.StatusBadRequest).Scan(&logCount, &attributedAccounts); err != nil {
		t.Fatal(err)
	}
	if logCount != len(requests) || attributedAccounts != 0 {
		t.Fatalf("filter logs=%d attributed_accounts=%d", logCount, attributedAccounts)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "请求格式过滤关闭", "rate_multiplier": 1, "status": "active",
		"normal_request_mode": true, "request_format_filter_enabled": false,
	}, http.StatusOK, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(requests[0].body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || upstreamCalls.Load() != 1 {
		t.Fatalf("disabled filter status=%d calls=%d body=%s", response.Code, upstreamCalls.Load(), response.Body.String())
	}
}

func jsonStringAt(body []byte, path string) string {
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	current := any(payload)
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = object[part]
	}
	value, _ := current.(string)
	return value
}
