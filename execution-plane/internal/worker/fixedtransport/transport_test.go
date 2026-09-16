package fixedtransport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

const testProxyURL = "http://host-agent.execution.internal:8094"

func TestProxyURLStrictOrigin(t *testing.T) {
	for _, raw := range []string{testProxyURL, testProxyURL + "/", "http://host-agent.execution.internal:1", "http://host-agent.execution.internal:65535"} {
		if err := ValidateProxyURL(raw); err != nil {
			t.Errorf("valid origin %q rejected: %v", raw, err)
		}
	}
	for _, raw := range []string{
		"", "http://localhost:8094", "http://127.0.0.1:8094", "http://[::1]:8094",
		"http://host-agent.execution.internal", "http://host-agent.execution.internal:",
		"http://host-agent.execution.internal:0", "http://host-agent.execution.internal:65536",
		"http://host-agent.execution.internal:08094", "http://host-agent.execution.internal:+8094",
		"http://host-agent.execution.internal:port", "http://HOST-agent.execution.internal:8094",
		"http://host-agent.execution.internal.:8094", "http://host-agent.execution.internal.attacker.test:8094",
		"https://host-agent.execution.internal:8094", "socks5://host-agent.execution.internal:8094",
		"http:host-agent.execution.internal:8094", "//host-agent.execution.internal:8094",
		(&url.URL{Scheme: "http", Host: "host-agent.execution.internal:8094", User: url.UserPassword("synthetic-user", "synthetic-password")}).String(),
		"http://@host-agent.execution.internal:8094",
		testProxyURL + "/path", testProxyURL + "/%2f", testProxyURL + "?q=value", testProxyURL + "?",
		testProxyURL + "#fragment", testProxyURL + "#", testProxyURL + " ", " " + testProxyURL,
		testProxyURL + "\n", "http://host-agent.execution.internal%2e:8094",
	} {
		t.Run(raw, func(t *testing.T) {
			transport, err := New(raw)
			if transport != nil || !errors.Is(err, ErrProxyURL) || err.Error() != ErrProxyURL.Error() {
				t.Fatalf("invalid origin must return only fixed error, got transport=%v err=%v", transport != nil, err)
			}
			if !errors.Is(ValidateProxyURL(raw), ErrProxyURL) {
				t.Fatal("validation and construction disagree")
			}
		})
	}
}

func TestFixedProxyIgnoresEnvironmentAndLocalhost(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(key, "http://untrusted.invalid:9999")
	}
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	transport, err := New(testProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	for _, target := range []string{"https://origin.example.test", "http://localhost:8080", "https://127.0.0.1:8443", "https://[::1]:8443"} {
		request, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		proxy, err := transport.Proxy(request)
		if err != nil || proxy == nil || proxy.String() != testProxyURL {
			t.Fatalf("target %s bypassed fixed proxy: %v / %v", target, proxy, err)
		}
		proxy.Host = "changed.invalid:9999"
		proxy.User = url.UserPassword("changed", "secret")
		unchanged, _ := transport.Proxy(request)
		if unchanged.String() != testProxyURL {
			t.Fatal("returned proxy URL mutation changed the fixed route")
		}
	}
}

func TestProxyFailureNeverDialsOrigin(t *testing.T) {
	for _, target := range []string{"https://origin.example.test:8443/path", "http://localhost:8080/path", "https://127.0.0.1:8443/path"} {
		t.Run(target, func(t *testing.T) {
			transport, err := New(testProxyURL)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.CloseIdleConnections()
			var mu sync.Mutex
			var dialed []string
			failed := errors.New("synthetic proxy unavailable")
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, network+" "+address)
				mu.Unlock()
				return nil, failed
			}
			request, _ := http.NewRequest(http.MethodGet, target, nil)
			response, err := transport.RoundTrip(request)
			if response != nil || !errors.Is(err, failed) {
				t.Fatalf("proxy failure = %v, response present = %v", err, response != nil)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(dialed) != 1 || dialed[0] != "tcp host-agent.execution.internal:8094" {
				t.Fatalf("unexpected dial attempts: %v", dialed)
			}
		})
	}
}

func TestTransportHasPrivateVerifyingTLSDefaults(t *testing.T) {
	first, err := New(testProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.CloseIdleConnections()
	second, err := New(testProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.CloseIdleConnections()
	c := first.TLSClientConfig
	if first == second || c == nil || c == second.TLSClientConfig || c.MinVersion != tls.VersionTLS12 || c.InsecureSkipVerify ||
		c.RootCAs != nil || c.KeyLogWriter != nil || len(c.Certificates) != 0 || c.GetClientCertificate != nil ||
		c.GetCertificate != nil || c.GetConfigForClient != nil || c.VerifyPeerCertificate != nil || c.VerifyConnection != nil ||
		c.ClientSessionCache != nil || c.ServerName != "" || len(c.CipherSuites) != 0 || len(c.CurvePreferences) != 0 ||
		first.DialTLSContext != nil || first.DialTLS != nil || first.GetProxyConnectHeader != nil || len(first.ProxyConnectHeader) != 0 {
		t.Fatal("transport has shared, custom identity, or non-verifying TLS configuration")
	}
}
