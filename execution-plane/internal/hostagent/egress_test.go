package hostagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

func TestEgressGatewayConvertsHTTPAndHTTPSProxyCONNECT(t *testing.T) {
	for _, proxyScheme := range []string{"http", "https"} {
		proxyScheme := proxyScheme
		t.Run(proxyScheme, func(t *testing.T) {
			t.Parallel()
			target, stopTarget := startEchoServer(t)
			defer stopTarget()
			proxyURL, tlsConfig, observations, stopProxy := startHTTPConnectProxy(t, proxyScheme == "https", "proxy-user", "proxy-password")
			defer stopProxy()
			proxy, err := ParseUpstreamProxy(proxyURL, tlsConfig)
			if err != nil {
				t.Fatalf("parse upstream proxy: %v", err)
			}
			gatewayAddress, claim, backend, fencer, stopGateway := startTestEgressGateway(t, proxy, target)
			defer stopGateway()

			connection, response := openGatewayTunnel(t, gatewayAddress, target)
			defer connection.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("gateway CONNECT status = %d", response.StatusCode)
			}
			assertTunnelEcho(t, connection, []byte("hello-through-"+proxyScheme))
			select {
			case observation := <-observations:
				if observation.target != target || observation.authorization != basicProxyAuthorization("proxy-user", "proxy-password") {
					t.Fatalf("unexpected proxy observation: %+v", observation)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upstream HTTP proxy did not observe CONNECT")
			}

			if err := backend.Revoke(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			if err := fencer.Revalidate(context.Background()); !errors.Is(err, lease.ErrLeaseNotCurrent) {
				t.Fatalf("revalidate revoked lease error = %v", err)
			}
			_ = connection.SetDeadline(time.Now().Add(time.Second))
			if _, err := connection.Write([]byte("must-not-survive")); err == nil {
				buffer := make([]byte, 1)
				if _, readErr := connection.Read(buffer); readErr == nil {
					t.Fatal("revoked execution lease left tunnel usable")
				}
			}
		})
	}
}

func TestEgressGatewayConvertsAuthenticatedSOCKS5(t *testing.T) {
	t.Parallel()
	target, stopTarget := startEchoServer(t)
	defer stopTarget()
	proxyURL, observations, stopProxy := startSOCKS5Proxy(t, "socks-user", "socks-password")
	defer stopProxy()
	proxy, err := ParseUpstreamProxy(proxyURL, nil)
	if err != nil {
		t.Fatalf("parse SOCKS5 proxy: %v", err)
	}
	gatewayAddress, _, _, _, stopGateway := startTestEgressGateway(t, proxy, target)
	defer stopGateway()

	connection, response := openGatewayTunnel(t, gatewayAddress, target)
	defer connection.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("gateway CONNECT status = %d", response.StatusCode)
	}
	assertTunnelEcho(t, connection, []byte("hello-through-socks5"))
	select {
	case observation := <-observations:
		if observation.target != target || observation.username != "socks-user" || observation.password != "socks-password" {
			t.Fatalf("unexpected SOCKS5 observation: %+v", observation)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SOCKS5 proxy did not observe tunnel")
	}
}

func TestEgressGatewayRejectsUnknownSourceTargetAndUnavailableLease(t *testing.T) {
	t.Parallel()
	target, stopTarget := startEchoServer(t)
	defer stopTarget()
	proxyURL, _, _, stopProxy := startHTTPConnectProxy(t, false, "", "")
	defer stopProxy()
	proxy, err := ParseUpstreamProxy(proxyURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend := lease.NewMemoryBackend(time.Now)
	fencer, _ := lease.NewFencer(backend)
	registry := NewEgressRegistry()
	gateway, _ := NewEgressGateway(EgressGatewayConfig{Registry: registry, Fencer: fencer})
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gateway.Serve(ctx, listener) }()
	defer func() {
		cancel()
		_ = listener.Close()
		<-done
	}()

	connection, response := openGatewayTunnel(t, listener.Addr().String(), target)
	connection.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("unbound source status = %d, want 403", response.StatusCode)
	}
	claim := lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 1, OwnerID: "host-agent-1"}
	if err := registry.Register(EgressBinding{
		SourceIP: netip.MustParseAddr("127.0.0.1"), Claim: claim, ProxyLeaseID: "proxy-lease-1", Proxy: proxy,
		AllowedTargets: []string{"allowed.example:443"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Acquire(context.Background(), claim, time.Minute); err != nil {
		t.Fatal(err)
	}
	connection, response = openGatewayTunnel(t, listener.Addr().String(), target)
	connection.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("denied target status = %d, want 403", response.StatusCode)
	}

	if err := registry.Unregister(netip.MustParseAddr("127.0.0.1"), claim.SlotID, claim.ExecutionEpoch); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(EgressBinding{
		SourceIP: netip.MustParseAddr("127.0.0.1"), Claim: claim, ProxyLeaseID: "proxy-lease-1", Proxy: proxy,
		AllowedTargets: []string{target},
	}); err != nil {
		t.Fatal(err)
	}
	backend.SetAvailable(false)
	connection, response = openGatewayTunnel(t, listener.Addr().String(), target)
	connection.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unavailable lease backend status = %d, want 503", response.StatusCode)
	}
}

func TestEgressRegistryEpochFencingAndProxyRedaction(t *testing.T) {
	t.Parallel()
	proxy, err := ParseUpstreamProxy("http://sensitive-user:sensitive-password@127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	binding := EgressBinding{
		SourceIP:     netip.MustParseAddr("172.18.0.2"),
		Claim:        lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 1, OwnerID: "host-agent-1"},
		ProxyLeaseID: "proxy-lease-1", Proxy: proxy, AllowedTargets: []string{"api.anthropic.com:443"},
	}
	registry := NewEgressRegistry()
	if err := registry.Register(binding); err != nil {
		t.Fatal(err)
	}
	conflict := binding
	conflict.Claim.SlotID = "slot-2"
	if err := registry.Register(conflict); !errors.Is(err, ErrEgressBindingConflict) {
		t.Fatalf("source reuse error = %v, want conflict", err)
	}
	changedProxy := binding
	changedProxy.Proxy, err = ParseUpstreamProxy("http://other-user:other-password@127.0.0.1:8081", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(changedProxy); !errors.Is(err, ErrEgressBindingConflict) {
		t.Fatalf("same epoch proxy replacement error = %v, want conflict", err)
	}
	newEpoch := binding
	newEpoch.Claim.ExecutionEpoch = 2
	newEpoch.ProxyLeaseID = "proxy-lease-2"
	if err := registry.Register(newEpoch); err != nil {
		t.Fatalf("higher epoch registration: %v", err)
	}
	if err := registry.Register(binding); !errors.Is(err, ErrEgressBindingConflict) {
		t.Fatalf("stale epoch registration error = %v, want conflict", err)
	}

	serialized, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{proxy.String(), fmt.Sprintf("%+v", proxy), fmt.Sprintf("%+v", binding), string(serialized)} {
		if strings.Contains(output, "sensitive-user") || strings.Contains(output, "sensitive-password") {
			t.Fatalf("proxy serialization leaked credentials: %s", output)
		}
	}
}

func TestUpstreamProxyAndTargetValidation(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"", "ftp://proxy.example:21", "http://proxy.example/path", "http://proxy.example:0",
		"http://user:password%0Ainjected@proxy.example:80",
	} {
		if _, err := ParseUpstreamProxy(raw, nil); err == nil {
			t.Fatalf("ParseUpstreamProxy(%q) succeeded", raw)
		}
	}
	if _, err := ParseUpstreamProxy("https://proxy.example:443", &tls.Config{InsecureSkipVerify: true}); err == nil { //nolint:gosec -- invalid configuration under test
		t.Fatal("HTTPS proxy accepted disabled TLS verification")
	}
	canonicalProxy, err := ParseUpstreamProxy("http://proxy.example:080", nil)
	if err != nil || canonicalProxy.address != "proxy.example:80" {
		t.Fatalf("proxy address normalization = %v, %q", err, canonicalProxy.address)
	}
	policy, err := newTargetPolicy([]string{"api.anthropic.com:443", "*.claude.ai:443"})
	if err != nil {
		t.Fatal(err)
	}
	for target, allowed := range map[string]bool{
		"api.anthropic.com:443": true,
		"chat.claude.ai:443":    true,
		"claude.ai:443":         false,
		"chat.claude.ai:80":     false,
		"127.0.0.1:443":         false,
	} {
		parsed, err := parseConnectTarget(target)
		if err != nil {
			t.Fatalf("parse target %q: %v", target, err)
		}
		if policy.allows(parsed) != allowed {
			t.Fatalf("policy allows(%q) = %v, want %v", target, policy.allows(parsed), allowed)
		}
	}
}

func startTestEgressGateway(t *testing.T, proxy *UpstreamProxy, target string) (string, lease.Claim, *lease.MemoryBackend, *lease.Fencer, func()) {
	t.Helper()
	claim := lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 1, OwnerID: "host-agent-1"}
	backend := lease.NewMemoryBackend(time.Now)
	if err := backend.Acquire(context.Background(), claim, time.Minute); err != nil {
		t.Fatal(err)
	}
	fencer, err := lease.NewFencer(backend)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewEgressRegistry()
	if err := registry.Register(EgressBinding{
		SourceIP: netip.MustParseAddr("127.0.0.1"), Claim: claim, ProxyLeaseID: "proxy-lease-1",
		Proxy: proxy, AllowedTargets: []string{target},
	}); err != nil {
		t.Fatal(err)
	}
	gateway, err := NewEgressGateway(EgressGatewayConfig{
		Registry: registry, Fencer: fencer, RevalidateInterval: 100 * time.Millisecond, MaxTunnelDuration: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gateway.Serve(ctx, listener) }()
	stop := func() {
		cancel()
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("egress gateway stopped: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("egress gateway did not stop")
		}
	}
	return listener.Addr().String(), claim, backend, fencer, stop
}

func openGatewayTunnel(t *testing.T, gatewayAddress, target string) (net.Conn, *http.Response) {
	t.Helper()
	connection, err := net.DialTimeout("tcp", gatewayAddress, 3*time.Second)
	if err != nil {
		t.Fatalf("dial egress gateway: %v", err)
	}
	request := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"
	if _, err := io.WriteString(connection, request); err != nil {
		connection.Close()
		t.Fatalf("write gateway CONNECT: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
	response, err := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodConnect})
	if err != nil {
		connection.Close()
		t.Fatalf("read gateway CONNECT response: %v", err)
	}
	_ = connection.SetDeadline(time.Time{})
	return connection, response
}

func assertTunnelEcho(t *testing.T, connection net.Conn, payload []byte) {
	t.Helper()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := connection.Write(payload); err != nil {
		t.Fatalf("write tunnel payload: %v", err)
	}
	response := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, response); err != nil {
		t.Fatalf("read tunnel payload: %v", err)
	}
	_ = connection.SetDeadline(time.Time{})
	if !bytes.Equal(response, payload) {
		t.Fatalf("echo payload = %q, want %q", response, payload)
	}
}

func startEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connections sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() {
				defer connections.Done()
				defer connection.Close()
				buffer := make([]byte, 32<<10)
				for {
					count, err := connection.Read(buffer)
					if count > 0 {
						if _, writeErr := connection.Write(buffer[:count]); writeErr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	stop := func() {
		_ = listener.Close()
		<-done
	}
	return listener.Addr().String(), stop
}

type httpProxyObservation struct {
	target        string
	authorization string
}

func startHTTPConnectProxy(t *testing.T, useTLS bool, username, password string) (string, *tls.Config, <-chan httpProxyObservation, func()) {
	t.Helper()
	observations := make(chan httpProxyObservation, 16)
	handler := http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			http.Error(response, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		observations <- httpProxyObservation{target: request.Host, authorization: request.Header.Get("Proxy-Authorization")}
		if request.Header.Get("Proxy-Authorization") != basicProxyAuthorization(username, password) {
			http.Error(response, "proxy auth required", http.StatusProxyAuthRequired)
			return
		}
		upstream, err := net.DialTimeout("tcp", request.Host, 3*time.Second)
		if err != nil {
			http.Error(response, "proxy dial failed", http.StatusBadGateway)
			return
		}
		hijacker := response.(http.Hijacker)
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		_, _ = io.WriteString(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		go func() {
			defer client.Close()
			defer upstream.Close()
			relayTestConnections(client, buffered.Reader, upstream)
		}()
	})
	server := httptest.NewUnstartedServer(handler)
	if useTLS {
		server.StartTLS()
	} else {
		server.Start()
	}
	parsed, _ := url.Parse(server.URL)
	parsed.User = url.UserPassword(username, password)
	var tlsConfig *tls.Config
	if useTLS {
		roots := x509.NewCertPool()
		roots.AddCert(server.Certificate())
		tlsConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return parsed.String(), tlsConfig, observations, server.Close
}

func basicProxyAuthorization(username, password string) string {
	if username == "" && password == "" {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+password))
}

type socksObservation struct {
	target   string
	username string
	password string
}

func startSOCKS5Proxy(t *testing.T, expectedUsername, expectedPassword string) (string, <-chan socksObservation, func()) {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	observations := make(chan socksObservation, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go handleSOCKS5Connection(connection, expectedUsername, expectedPassword, observations)
		}
	}()
	parsed := &url.URL{Scheme: "socks5", Host: listener.Addr().String(), User: url.UserPassword(expectedUsername, expectedPassword)}
	stop := func() {
		_ = listener.Close()
		<-done
	}
	return parsed.String(), observations, stop
}

func handleSOCKS5Connection(connection net.Conn, expectedUsername, expectedPassword string, observations chan<- socksObservation) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil || header[0] != 5 {
		return
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, methods); err != nil {
		return
	}
	if _, err := connection.Write([]byte{5, 2}); err != nil {
		return
	}
	if _, err := io.ReadFull(reader, header); err != nil || header[0] != 1 {
		return
	}
	username := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, username); err != nil {
		return
	}
	passwordLength, err := reader.ReadByte()
	if err != nil {
		return
	}
	password := make([]byte, int(passwordLength))
	if _, err := io.ReadFull(reader, password); err != nil {
		return
	}
	if string(username) != expectedUsername || string(password) != expectedPassword {
		_, _ = connection.Write([]byte{1, 1})
		return
	}
	if _, err := connection.Write([]byte{1, 0}); err != nil {
		return
	}
	requestHeader := make([]byte, 4)
	if _, err := io.ReadFull(reader, requestHeader); err != nil || requestHeader[0] != 5 || requestHeader[1] != 1 {
		return
	}
	host := ""
	switch requestHeader[3] {
	case 1:
		address := make([]byte, 4)
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = net.IP(address).String()
	case 3:
		length, err := reader.ReadByte()
		if err != nil {
			return
		}
		address := make([]byte, int(length))
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = string(address)
	case 4:
		address := make([]byte, 16)
		if _, err := io.ReadFull(reader, address); err != nil {
			return
		}
		host = net.IP(address).String()
	default:
		return
	}
	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(reader, portBytes); err != nil {
		return
	}
	target := net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(portBytes)))
	observations <- socksObservation{target: target, username: string(username), password: string(password)}
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		_, _ = connection.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer upstream.Close()
	if _, err := connection.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 0}); err != nil {
		return
	}
	relayTestConnections(connection, reader, upstream)
}

func relayTestConnections(client net.Conn, clientReader io.Reader, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, clientReader)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}
