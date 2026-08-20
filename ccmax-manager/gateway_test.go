package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type capturedClaudeRequest struct {
	Path   string
	Query  string
	Header http.Header
	Body   map[string]any
}

func TestOAuthGatewayMimicsClaudeCodeAndFiltersHeaders(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan capturedClaudeRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- capturedClaudeRequest{Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone(), Body: body}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req-mimic")
		_, _ = w.Write([]byte(`{"id":"msg_mimic","usage":{"input_tokens":2,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "mimic", upstream.URL, 0, map[string]any{"account_uuid": "account-uuid"}, map[string]any{"access_token": "oauth-token", "account_uuid": "account-uuid"})

	payload := map[string]any{
		"model": "claude-test", "system": "Only answer with facts.", "max_tokens": 32,
		"messages": []any{map[string]any{"role": "user", "content": "hello from a third party client"}},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Cookie", "sessionKey=must-not-leak")
	request.Header.Set("X-Untrusted-Header", "must-not-leak")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}

	got := <-captured
	if got.Path != "/v1/messages" || got.Query != "beta=true" {
		t.Fatalf("upstream target=%s?%s", got.Path, got.Query)
	}
	if got.Header.Get("Authorization") != "Bearer oauth-token" {
		t.Fatalf("authorization=%q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("X-Untrusted-Header") != "" {
		t.Fatalf("untrusted headers leaked: %#v", got.Header)
	}
	if got.Header.Get("User-Agent") != claudeDefaultHeaders["User-Agent"] || got.Header.Get("X-App") != "cli" {
		t.Fatalf("Claude Code headers missing: %#v", got.Header)
	}
	for _, beta := range claudeFullMimicBetas {
		if !strings.Contains(got.Header.Get("anthropic-beta"), beta) {
			t.Fatalf("anthropic-beta missing %s: %s", beta, got.Header.Get("anthropic-beta"))
		}
	}
	system, _ := got.Body["system"].([]any)
	if len(system) != 3 {
		t.Fatalf("system blocks=%d body=%#v", len(system), got.Body)
	}
	billing, _ := system[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(billing, "x-anthropic-billing-header: cc_version="+claudeCLIVersion+".") {
		t.Fatalf("billing block=%q", billing)
	}
	messages, _ := got.Body["messages"].([]any)
	if len(messages) != 3 || !strings.Contains(messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "Only answer with facts.") {
		t.Fatalf("original system was not preserved in messages: %#v", messages)
	}
	metadata := got.Body["metadata"].(map[string]any)
	var userID map[string]string
	if err := json.Unmarshal([]byte(metadata["user_id"].(string)), &userID); err != nil || userID["account_uuid"] != "account-uuid" || userID["session_id"] == "" {
		t.Fatalf("metadata.user_id=%#v err=%v", metadata["user_id"], err)
	}
}

func TestAccountRequestPassthroughPreservesRawBody(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	captured := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- body
		if r.Header.Get("Authorization") != "Bearer passthrough-token" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"id":"msg_passthrough","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	extra := map[string]any{
		"request_passthrough": true,
		"model_mapping":       map[string]any{"claude-client": "claude-upstream"},
	}
	createGatewayTestAccount(t, a, handler, "passthrough", upstream.URL, 0, extra, map[string]any{"access_token": "passthrough-token"})
	rawBody := []byte("{\n  \"messages\": [{\"role\":\"user\",\"content\":\"hello\"}],\n  \"custom_parameter\": {\"keep\": true},\n  \"system\": \"do not rewrite\",\n  \"max_tokens\": 17,\n  \"model\": \"claude-client\"\n}")
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(rawBody))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}
	if got := <-captured; !bytes.Equal(got, rawBody) {
		t.Fatalf("raw body changed\n got: %s\nwant: %s", got, rawBody)
	}
}

func TestRealClaudeCodeRequestPreservesSystem(t *testing.T) {
	payload := map[string]any{
		"model": "claude-test", "max_tokens": 32,
		"system":   []any{map[string]any{"type": "text", "text": claudeCodeSystemPrompt}, map[string]any{"type": "text", "text": "client-owned-cache", "cache_control": map[string]any{"type": "ephemeral"}}},
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		"metadata": map[string]any{"user_id": `{"device_id":"device","account_uuid":"account","session_id":"11111111-1111-4111-8111-111111111111"}`},
	}
	body, _ := json.Marshal(payload)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	request.Header.Set("User-Agent", "claude-cli/2.1.220 (external, cli)")
	request.Header.Set("X-App", "cli")
	request.Header.Set("anthropic-beta", "claude-code-20250219")
	request.Header.Set("anthropic-version", claudeAPIVersion)
	prepared, err := prepareClaudeRequest(request, body, gatewayAccount{ID: 7, CredentialsJSON: `{"access_token":"token","account_uuid":"account"}`}, "claude-test", false)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.ClaudeCode || prepared.Mimic {
		t.Fatalf("classification ClaudeCode=%v Mimic=%v", prepared.ClaudeCode, prepared.Mimic)
	}
	var transformed map[string]any
	_ = json.Unmarshal(prepared.Body, &transformed)
	system := transformed["system"].([]any)
	if len(system) != 2 || system[1].(map[string]any)["text"] != "client-owned-cache" {
		t.Fatalf("real Claude Code system changed: %#v", system)
	}
}

func TestGatewayStreamsBeforeUpstreamCompletes(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":2}}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	createGatewayTestAccount(t, a, handler, "stream", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	gatewayServer := httptest.NewServer(handler)
	defer gatewayServer.Close()

	payload := `{"model":"claude-test","stream":true,"max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`
	request, _ := http.NewRequest(http.MethodPost, gatewayServer.URL+"/v1/messages", strings.NewReader(payload))
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
		t.Fatal("gateway did not expose streaming response headers before upstream completion")
	}
	reader := bufio.NewReader(response.Body)
	lineCh := make(chan string, 1)
	go func() {
		line, _ := reader.ReadString('\n')
		lineCh <- line
	}()
	select {
	case line := <-lineCh:
		if !strings.Contains(line, "message_start") {
			t.Fatalf("first SSE line=%q", line)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("gateway buffered the first SSE event")
	}
	close(release)
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
}

func TestGatewayFailsOverAndMapsCountTokensModel(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var firstCalls, secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer first.Close()
	mappedModel := make(chan string, 1)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		if r.URL.Path != "/v1/messages/count_tokens" || r.URL.Query().Get("beta") != "true" {
			t.Errorf("count_tokens target=%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mappedModel <- body["model"].(string)
		if !strings.Contains(r.Header.Get("anthropic-beta"), "token-counting-2024-11-01") {
			t.Errorf("count_tokens beta=%s", r.Header.Get("anthropic-beta"))
		}
		_, _ = w.Write([]byte(`{"input_tokens":9}`))
	}))
	defer second.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	extra := map[string]any{"supported_models": []string{"claude-alias"}, "model_mapping": map[string]any{"claude-alias": "claude-upstream"}}
	createGatewayTestAccount(t, a, handler, "first", first.URL, 0, extra, map[string]any{"access_token": "first-token"})
	createGatewayTestAccount(t, a, handler, "second", second.URL, 1, extra, map[string]any{"access_token": "second-token"})
	requestJSON(t, handler, http.MethodPost, "/v1/messages/count_tokens", map[string]any{"model": "claude-alias", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil, key.Key, http.StatusOK, nil)
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("failover calls first=%d second=%d", firstCalls.Load(), secondCalls.Load())
	}
	if got := <-mappedModel; got != "claude-upstream" {
		t.Fatalf("mapped model=%q", got)
	}
	var models struct {
		Data []gatewayModel `json:"data"`
	}
	requestJSON(t, handler, http.MethodGet, "/v1/models", nil, nil, key.Key, http.StatusOK, &models)
	if len(models.Data) != 1 || models.Data[0].ID != "claude-alias" {
		t.Fatalf("models=%+v", models.Data)
	}
}

func TestGatewayModelsAliasFiltersWildcardModels(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	for _, model := range []string{"claude-sonnet-4-5", "claude-opus-4-6"} {
		if _, err := a.db.Exec(`INSERT INTO model_prices (model) VALUES (?)`, model); err != nil {
			t.Fatal(err)
		}
	}
	createGatewayTestAccount(
		t,
		a,
		handler,
		"wildcard-models",
		"https://api.anthropic.com",
		0,
		map[string]any{"supported_models": []string{"claude-sonnet-*"}},
		map[string]any{"access_token": "test-token"},
	)
	var result struct {
		Object string         `json:"object"`
		Data   []gatewayModel `json:"data"`
	}
	requestJSON(t, handler, http.MethodGet, "/models", nil, nil, key.Key, http.StatusOK, &result)
	if result.Object != "list" || len(result.Data) != 1 || result.Data[0].ID != "claude-sonnet-4-5" {
		t.Fatalf("models=%+v", result.Data)
	}
	if result.Data[0].Type != "model" || result.Data[0].Object != "model" || result.Data[0].OwnedBy != "anthropic" || result.Data[0].Created <= 0 {
		t.Fatalf("compatibility fields=%+v", result.Data[0])
	}
	var model gatewayModel
	requestJSON(t, handler, http.MethodGet, "/models/claude-sonnet-4-5", nil, nil, key.Key, http.StatusOK, &model)
	if model.ID != "claude-sonnet-4-5" {
		t.Fatalf("model=%+v", model)
	}
	requestJSON(t, handler, http.MethodGet, "/models/claude-opus-4-6", nil, nil, key.Key, http.StatusNotFound, nil)
}

func TestGatewayNeverDispatchesWithoutAnActiveProxy(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "no-proxy", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{"custom_forward_url": upstream.URL},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 0, "rate_multiplier": 1, "group_ids": []string{"a"},
		"base_rpm": 15, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, nil)

	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusTooManyRequests, nil)
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("unproxied upstream calls=%d, want 0", calls)
	}
}

func TestConcurrentTokenRefreshUsesSingleUpstreamCall(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fresh-token","refresh_token":"fresh-refresh","expires_in":3600,"token_type":"bearer"}`))
	}))
	defer server.Close()
	previousEndpoint := claudeTokenEndpoint
	claudeTokenEndpoint = server.URL
	defer func() { claudeTokenEndpoint = previousEndpoint }()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "refresh", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "old-token", "refresh_token": "old-refresh", "expires_at": time.Now().Add(-time.Minute).Unix()},
		"extra":       map[string]any{}, "status": "active", "schedulable": true, "concurrency": 10, "priority": 0, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)
	base := gatewayAccount{ID: created.ID, AuthType: "oauth", CredentialsJSON: `{"access_token":"old-token","refresh_token":"old-refresh","expires_at":1}`}
	var wg sync.WaitGroup
	errorsCh := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.ensureGatewayAccountToken(context.Background(), base)
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d, want 1", calls.Load())
	}
}

func TestSerialUserMessageQueueAndToolResultBypass(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, _ := newGatewayTestApp(t)
	defer a.db.Close()
	account := gatewayAccount{ID: 99, BaseRPM: 15, UserMsgQueueMode: "serial"}
	userBody := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	firstRelease, err := a.acquireUserMessageQueue(context.Background(), account, userBody, false)
	if err != nil {
		t.Fatal(err)
	}
	secondReady := make(chan func(), 1)
	go func() {
		release, _ := a.acquireUserMessageQueue(context.Background(), account, userBody, false)
		secondReady <- release
	}()
	select {
	case <-secondReady:
		t.Fatal("serial queue admitted a second user message before release")
	case <-time.After(50 * time.Millisecond):
	}
	toolBody := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"ok"}]}]}`)
	toolRelease, err := a.acquireUserMessageQueue(context.Background(), account, toolBody, false)
	if err != nil {
		t.Fatal(err)
	}
	toolRelease()
	firstRelease()
	select {
	case secondRelease := <-secondReady:
		secondRelease()
	case <-time.After(time.Second):
		t.Fatal("serial queue did not admit the next user message after release")
	}
}

func newGatewayTestApp(t *testing.T) (*app, http.Handler) {
	t.Helper()
	a, err := newApp(filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	return a, a.routes()
}

func createGatewayTestKey(t *testing.T, handler http.Handler) apiKeyRecord {
	t.Helper()
	var user panelUser
	putJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "gateway-user", "name": "Gateway User", "password": "gateway-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 100,
	}, http.StatusCreated, &user)
	var key apiKeyRecord
	putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{"user_id": user.ID, "name": "gateway-key", "group_id": "a", "status": "active", "quota": 0}, http.StatusCreated, &key)
	return key
}

func createGatewayTestAccount(t *testing.T, a *app, handler http.Handler, name, upstream string, priority int, extra, credentials map[string]any) account {
	t.Helper()
	if extra == nil {
		extra = map[string]any{}
	}
	extra["custom_forward_url"] = upstream
	proxyID := createTestForwardProxy(t, a)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": name, "platform": "anthropic", "auth_type": "oauth", "credentials": credentials, "extra": extra,
		"status": "active", "schedulable": true, "concurrency": 10, "priority": priority, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID,
		"base_rpm": 100, "rpm_strategy": "tiered", "rpm_sticky_buffer": 0, "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	return created
}

func createTestForwardProxy(t *testing.T, a *app) int64 {
	t.Helper()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		outgoing := r.Clone(r.Context())
		outgoing.RequestURI = ""
		outgoing.Header.Del("Proxy-Connection")
		response, err := transport.RoundTrip(outgoing)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		buffer := make([]byte, 32*1024)
		for {
			read, readErr := response.Body.Read(buffer)
			if read > 0 {
				_, _ = w.Write(buffer[:read])
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
			if readErr != nil {
				break
			}
		}
	}))
	t.Cleanup(func() {
		server.Close()
		transport.CloseIdleConnections()
	})
	host, portText, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO proxies (pool_id, name, protocol, host, port, status) VALUES (1, ?, 'http', ?, ?, 'active')`, "test-proxy-"+portText, host, port)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
