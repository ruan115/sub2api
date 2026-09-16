package fixedtransport_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker/fixedtransport"
)

// All addresses are test-owned loopback sockets. The two dial adapters below
// deliberately map synthetic names; neither DNS nor the public Internet is used.
func TestFixedProxyActualTLSProtectsOriginAndRejectsCertificateFaults(t *testing.T) {
	for _, fault := range []string{"none", "unknown CA", "wrong SAN", "expired", "TLS 1.1"} {
		t.Run(fault, func(t *testing.T) {
			certificate, roots := temporaryCertificate(t, fault)
			var originCalls atomic.Int64
			var version atomic.Uint32
			var correctSNI atomic.Bool
			origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				originCalls.Add(1)
				version.Store(uint32(r.TLS.Version))
				correctSNI.Store(r.TLS.ServerName == "origin.example.test")
				if r.Header.Get("Authorization") != "Bearer synthetic-origin-only" {
					t.Error("origin did not receive the synthetic authentication inside TLS")
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			origin.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}
			if fault == "TLS 1.1" {
				origin.TLS.MinVersion, origin.TLS.MaxVersion = tls.VersionTLS10, tls.VersionTLS11
			}
			origin.StartTLS()
			t.Cleanup(origin.Close)

			proxy := newLoopbackConnectProxy(t, origin.Listener.Addr().String())
			transport, err := fixedtransport.New(proxy.internalURL())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(transport.CloseIdleConnections)
			if transport.TLSClientConfig == nil || transport.TLSClientConfig.InsecureSkipVerify ||
				transport.TLSClientConfig.KeyLogWriter != nil || transport.TLSClientConfig.ClientSessionCache != nil ||
				len(transport.TLSClientConfig.Certificates) != 0 {
				t.Fatal("default TLS weakens verification or shares identity/session material")
			}
			if fault != "unknown CA" {
				transport.TLSClientConfig.RootCAs = roots
			} else {
				transport.TLSClientConfig.RootCAs = x509.NewCertPool()
			}
			var directAttempts atomic.Int64
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				if network != "tcp" || address != proxy.internalAddress() {
					directAttempts.Add(1)
					return nil, errors.New("test forbids all non-proxy dials")
				}
				return (&net.Dialer{}).DialContext(ctx, "tcp", proxy.server.Listener.Addr().String())
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://origin.example.test/v1/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer synthetic-origin-only")
			response, err := (&http.Client{Transport: transport}).Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			if fault == "none" {
				if err != nil || response == nil || response.StatusCode != http.StatusNoContent || originCalls.Load() != 1 ||
					version.Load() != tls.VersionTLS13 || !correctSNI.Load() {
					t.Fatalf("verified CONNECT/TLS path failed: %v", err)
				}
			} else if err == nil || originCalls.Load() != 0 {
				t.Fatal("invalid TLS peer reached the origin HTTP handler")
			}
			if proxy.connects.Load() != 1 || proxy.plaintextAuth.Load() || directAttempts.Load() != 0 {
				t.Fatal("fixed proxy path was bypassed or origin auth escaped outside TLS")
			}
			proxy.mu.Lock()
			target := proxy.target
			proxy.mu.Unlock()
			if target != "origin.example.test:443" {
				t.Fatal("origin hostname was locally resolved or changed before CONNECT")
			}
		})
	}
}

func TestFixedProxyDialFailureNeverAttemptsOrigin(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://ambient.invalid:1234")
	t.Setenv("HTTPS_PROXY", "http://ambient.invalid:1234")
	t.Setenv("ALL_PROXY", "http://ambient.invalid:1234")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	transport, err := fixedtransport.New("http://host-agent.execution.internal:18080")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(transport.CloseIdleConnections)
	var attempts atomic.Int64
	transport.DialContext = func(_ context.Context, _, address string) (net.Conn, error) {
		attempts.Add(1)
		if address != "host-agent.execution.internal:18080" {
			t.Error("proxy failure caused a direct or ambient-proxy dial")
		}
		return nil, errors.New("synthetic proxy outage")
	}
	for _, destination := range []string{"https://origin.example.test", "https://localhost", "https://127.0.0.1"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, destination, nil)
		response, err := (&http.Client{Transport: transport}).Do(request)
		cancel()
		if response != nil {
			_ = response.Body.Close()
		}
		if err == nil {
			t.Fatal("request succeeded during proxy outage")
		}
	}
	if attempts.Load() != 3 {
		t.Fatal("unexpected implicit retry or missing fixed-proxy attempt")
	}
}

func temporaryCertificate(t *testing.T, fault string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "synthetic test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	parsedCA, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"origin.example.test"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if fault == "wrong SAN" {
		leaf.DNSNames = []string{"another.example.test"}
	}
	if fault == "expired" {
		leaf.NotBefore, leaf.NotAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, parsedCA, key.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsedCA)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

type loopbackConnectProxy struct {
	server        *httptest.Server
	connects      atomic.Int64
	plaintextAuth atomic.Bool
	mu            sync.Mutex
	target        string
	connections   []net.Conn
}

func (p *loopbackConnectProxy) internalAddress() string {
	return net.JoinHostPort("host-agent.execution.internal", strconv.Itoa(p.server.Listener.Addr().(*net.TCPAddr).Port))
}

func (p *loopbackConnectProxy) internalURL() string { return "http://" + p.internalAddress() }

func newLoopbackConnectProxy(t *testing.T, origin string) *loopbackConnectProxy {
	t.Helper()
	p := &loopbackConnectProxy{}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			p.plaintextAuth.Store(true)
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		p.connects.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Cookie") != "" {
			p.plaintextAuth.Store(true)
		}
		p.mu.Lock()
		p.target = r.Host
		p.mu.Unlock()
		upstream, err := net.DialTimeout("tcp", origin, time.Second)
		if err != nil {
			http.Error(w, "test origin unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		p.mu.Lock()
		p.connections = append(p.connections, client, upstream)
		p.mu.Unlock()
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		_ = upstream.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			return
		}
		if buffered.Flush() != nil {
			return
		}
		done := make(chan struct{}, 1)
		go func() { _, _ = io.Copy(upstream, buffered); done <- struct{}{} }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		_ = upstream.Close()
		<-done
	}))
	t.Cleanup(func() {
		// httptest.Server cannot close hijacked connections by itself.
		p.mu.Lock()
		for _, connection := range p.connections {
			_ = connection.Close()
		}
		p.mu.Unlock()
		p.server.Close()
	})
	return p
}
