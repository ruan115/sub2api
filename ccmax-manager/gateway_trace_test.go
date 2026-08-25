package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayRequestIDsCorrelateNewAPIAccountAndUpstream(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "anthropic_456")
		_, _ = fmt.Fprint(w, `{"id":"msg_trace","type":"message","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	account := createGatewayTestAccount(t, a, handler, "trace-account", upstream.URL, 0, nil, map[string]any{"access_token": "oauth-token"})

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(newAPIRequestIDHeader, "newapi_123")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}
	traceID := response.Header().Get(gatewayTraceHeader)
	if traceID == "" {
		t.Fatal("CCMAX response trace header is empty")
	}

	var requestID, clientID, storedTraceID, upstreamID, accountName string
	err := a.db.QueryRow(`SELECT request_id, client_request_id, trace_id, upstream_request_id, account_name FROM usage_logs WHERE client_request_id = ?`, "newapi_123").Scan(&requestID, &clientID, &storedTraceID, &upstreamID, &accountName)
	if err != nil {
		t.Fatal(err)
	}
	if requestID != "anthropic_456" || clientID != "newapi_123" || storedTraceID != traceID || upstreamID != "anthropic_456" || accountName != account.Name {
		t.Fatalf("usage correlation = request:%q client:%q trace:%q upstream:%q account:%q", requestID, clientID, storedTraceID, upstreamID, accountName)
	}
}

func TestGatewayCapacityErrorRecordsTraceAndDiagnostics(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(newAPIRequestIDHeader, "newapi_capacity_123")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}

	var requestID, clientID, traceID, category, diagnosticsJSON string
	err := a.db.QueryRow(`SELECT request_id, client_request_id, trace_id, category, dispatch_diagnostics FROM gateway_error_logs WHERE client_request_id = ?`, "newapi_capacity_123").Scan(&requestID, &clientID, &traceID, &category, &diagnosticsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if requestID != traceID || clientID != "newapi_capacity_123" || traceID != response.Header().Get(gatewayTraceHeader) || category != "gateway_capacity" {
		t.Fatalf("error correlation = request:%q client:%q trace:%q category:%q", requestID, clientID, traceID, category)
	}
	var diagnostics gatewayCapacityDiagnostics
	if err := json.Unmarshal([]byte(diagnosticsJSON), &diagnostics); err != nil {
		t.Fatalf("decode diagnostics %q: %v", diagnosticsJSON, err)
	}
	if diagnostics.Candidates != 0 {
		t.Fatalf("capacity candidates=%d, want 0", diagnostics.Candidates)
	}
}

func TestGatewayCorrelationIDsPreferNewAPIRequest(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header.Set(newAPIRequestIDHeader, "newapi_123")
	trace := newGatewayRequestTrace(request)
	ctx := context.WithValue(request.Context(), gatewayTraceContextKey{}, trace)

	requestID, clientID, traceID, upstreamID := gatewayCorrelationIDs(ctx, "anthropic_456")
	if requestID != "anthropic_456" || clientID != "newapi_123" {
		t.Fatalf("request IDs = %q / %q, want upstream idempotency ID and NewAPI client ID", requestID, clientID)
	}
	if traceID == "" || upstreamID != "anthropic_456" {
		t.Fatalf("trace/upstream IDs = %q / %q", traceID, upstreamID)
	}
}

func TestGatewayCapacityDiagnosticsSurviveWrappedQueueError(t *testing.T) {
	want := gatewayCapacityDiagnostics{Candidates: 32, ConcurrencyBlocked: 28, RPMBlocked: 4}
	capacityErr := &gatewayCapacityError{groupID: "b", diagnostics: want}
	wrapped := fmt.Errorf("capacity queue timed out: %w", capacityErr)

	got, ok := gatewayCapacityDiagnosticsFromError(wrapped)
	if !ok {
		t.Fatal("wrapped capacity error did not expose diagnostics")
	}
	if got != want {
		t.Fatalf("diagnostics = %+v, want %+v", got, want)
	}
}

func TestBuildUsageWhereSearchesEveryRequestID(t *testing.T) {
	where, args := buildUsageWhere(usageFilters{Search: "newapi_123"})
	for _, column := range []string{
		"u.request_id", "u.client_request_id", "u.trace_id", "u.upstream_request_id",
		"u.account_name", "u.account_sk_hint", "usage_proxy.exit_ip", "k.key_prefix",
	} {
		if !strings.Contains(where, column) {
			t.Fatalf("usage search condition %q does not include %s", where, column)
		}
	}
	if len(args) != 12 {
		t.Fatalf("usage search arguments = %d, want 12", len(args))
	}
	for index, arg := range args {
		want := "newapi_123"
		if index >= 4 {
			want = "%newapi_123%"
		}
		if arg != want {
			t.Fatalf("usage search argument %d = %q, want %q", index, arg, want)
		}
	}
}
