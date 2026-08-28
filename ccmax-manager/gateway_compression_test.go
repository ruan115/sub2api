package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func TestClientForProxyUsesLongConfigurableTimeoutsAndConnectionCache(t *testing.T) {
	t.Setenv("CCMAX_UPSTREAM_RESPONSE_HEADER_TIMEOUT", "17m")
	t.Setenv("CCMAX_UPSTREAM_REQUEST_TIMEOUT", "0")

	client, err := clientForProxy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if client.Timeout != 0 {
		t.Fatalf("client timeout = %s, want disabled", client.Timeout)
	}
	wrapper, ok := client.Transport.(decompressingRoundTripper)
	if !ok {
		t.Fatalf("transport = %T, want decompressingRoundTripper", client.Transport)
	}
	transport, ok := wrapper.base.(*http.Transport)
	if !ok {
		t.Fatalf("base transport = %T, want *http.Transport", wrapper.base)
	}
	if transport.ResponseHeaderTimeout != 17*time.Minute {
		t.Fatalf("response header timeout = %s, want 17m", transport.ResponseHeaderTimeout)
	}
	if transport.MaxIdleConns != 1024 || transport.MaxIdleConnsPerHost != 128 {
		t.Fatalf("idle connection limits = %d/%d, want 1024/128", transport.MaxIdleConns, transport.MaxIdleConnsPerHost)
	}

	cached, err := clientForProxy(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cached != client {
		t.Fatal("clientForProxy did not reuse the configured transport")
	}
}

func TestClientForAccountProxyUsesIsolatedNodeTLSClients(t *testing.T) {
	firstAccount := gatewayAccount{ID: 91001, TLSProfile: defaultAccountTLSProfile}
	secondAccount := gatewayAccount{ID: 91002, TLSProfile: defaultAccountTLSProfile}

	first, err := clientForAccountProxy(nil, firstAccount)
	if err != nil {
		t.Fatal(err)
	}
	again, err := clientForAccountProxy(nil, firstAccount)
	if err != nil {
		t.Fatal(err)
	}
	second, err := clientForAccountProxy(nil, secondAccount)
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatal("same account did not reuse its upstream transport")
	}
	if first == second {
		t.Fatal("different accounts shared an upstream transport")
	}

	decompressing, ok := first.Transport.(decompressingRoundTripper)
	if !ok {
		t.Fatalf("transport type = %T", first.Transport)
	}
	schemes, ok := decompressing.base.(*accountSchemeRoundTripper)
	if !ok {
		t.Fatalf("base transport type = %T", decompressing.base)
	}
	if schemes.secure == nil || schemes.secure.DialTLSContext == nil {
		t.Fatal("account HTTPS transport does not use a custom TLS dialer")
	}
	if schemes.secure.ForceAttemptHTTP2 {
		t.Fatal("Node.js 24 profile must keep the captured HTTP/1.1 ALPN")
	}
}

func TestClientForProxyDecompressesExplicitAcceptEncoding(t *testing.T) {
	payload := []byte(`{"id":"msg_compressed","usage":{"input_tokens":17,"output_tokens":4}}`)
	for _, encoding := range []string{"gzip", "br", "deflate", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			compressed := compressGatewayTestPayload(t, encoding, payload)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := r.Header.Get("Accept-Encoding"); got != encoding {
					t.Errorf("Accept-Encoding = %q, want %q", got, encoding)
				}
				w.Header().Set("Content-Encoding", encoding)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(compressed)
			}))
			defer server.Close()

			client, err := clientForProxy(nil)
			if err != nil {
				t.Fatal(err)
			}
			request, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept-Encoding", encoding)
			response, err := client.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(body, payload) {
				t.Fatalf("decoded body = %q, want %q", body, payload)
			}
			if got := response.Header.Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding was not removed: %q", got)
			}
			usage := parseAnthropicUsage(body, false)
			if usage.Input != 17 || usage.Output != 4 {
				t.Fatalf("usage = %+v", usage)
			}
		})
	}
}

func TestCompressedGatewayResponseRecordsUsage(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	payload := []byte(`{"id":"msg_compressed_gateway","type":"message","role":"assistant","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":23,"output_tokens":6}}`)
	compressed := compressGatewayTestPayload(t, "gzip", payload)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("request-id", "req-compressed-gateway")
		_, _ = w.Write(compressed)
	}))
	defer upstream.Close()

	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	account := createGatewayTestAccount(t, a, handler, "compressed", upstream.URL, 0, nil, map[string]any{"access_token": "oauth-token"})
	hint := sourceSKHint("sk-ant-sid02-compressed-test-source-ABCDEF")
	if _, err := a.db.Exec(`UPDATE accounts SET source_sk_hint = ? WHERE id = ?`, hint, account.ID); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(`{"model":"claude-test","max_tokens":16,"stream":false,"messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Authorization", "Bearer "+key.Key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gateway status=%d body=%s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("gateway leaked decoded Content-Encoding %q", got)
	}
	if !bytes.Equal(response.Body.Bytes(), payload) {
		t.Fatalf("gateway body=%q, want %q", response.Body.Bytes(), payload)
	}

	usage, err := a.getUsageByRequestID("req-compressed-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 23 || usage.OutputTokens != 6 || usage.AccountSKHint != hint {
		t.Fatalf("recorded usage=%+v", usage)
	}
}

func compressGatewayTestPayload(t *testing.T, encoding string, payload []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var writer io.WriteCloser
	switch encoding {
	case "gzip":
		writer = gzip.NewWriter(&buffer)
	case "br":
		writer = brotli.NewWriter(&buffer)
	case "deflate":
		writer, _ = flate.NewWriter(&buffer, flate.DefaultCompression)
	case "zstd":
		var err error
		writer, err = zstd.NewWriter(&buffer)
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported test encoding %q", encoding)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
