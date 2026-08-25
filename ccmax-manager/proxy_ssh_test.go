package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startTestSSHProxy runs an in-process SSH server that only accepts password
// auth and services direct-tcpip channels, which is exactly what the tunnel
// needs from a real SSH proxy.
func startTestSSHProxy(t *testing.T, username, password string) string {
	t.Helper()
	return startTestSSHProxyWithPasswordCallback(t, func(conn ssh.ConnMetadata, secret []byte) (*ssh.Permissions, error) {
		if conn.User() != username || string(secret) != password {
			return nil, fmt.Errorf("invalid credentials")
		}
		return nil, nil
	})
}

func startTestSSHProxyWithPasswordCallback(t *testing.T, callback func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error)) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PasswordCallback: callback}
	config.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			go serveTestSSHConn(raw, config)
		}
	}()
	return listener.Addr().String()
}

func serveTestSSHConn(raw net.Conn, config *ssh.ServerConfig) {
	defer raw.Close()
	_, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for newChannel := range channels {
		if newChannel.ChannelType() != "direct-tcpip" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported")
			continue
		}
		var payload struct {
			Host  string
			Port  uint32
			Orig  string
			OPort uint32
		}
		if err := ssh.Unmarshal(newChannel.ExtraData(), &payload); err != nil {
			_ = newChannel.Reject(ssh.ConnectionFailed, "bad payload")
			continue
		}
		upstream, err := net.Dial("tcp", net.JoinHostPort(payload.Host, strconv.Itoa(int(payload.Port))))
		if err != nil {
			_ = newChannel.Reject(ssh.ConnectionFailed, err.Error())
			continue
		}
		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			upstream.Close()
			continue
		}
		go ssh.DiscardRequests(channelRequests)
		go func() {
			defer channel.Close()
			defer upstream.Close()
			go io.Copy(upstream, channel)
			io.Copy(channel, upstream)
		}()
	}
}

func TestSSHProxyTunnelsHTTPRequests(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ip": "203.0.113.7"})
	}))
	defer origin.Close()
	address := startTestSSHProxy(t, "root", "s3cret")

	proxyURL, err := url.Parse("ssh://root:s3cret@" + address)
	if err != nil {
		t.Fatal(err)
	}
	client, err := newClientForProxy(proxyURL, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("request through SSH tunnel failed: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.StatusCode, body)
	}
	if !bytesContains(body, "203.0.113.7") {
		t.Fatalf("unexpected body: %s", body)
	}

	// The tunnel is reused, so a second request must not need a new handshake.
	tunnel, err := sshTunnelFor(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	tunnel.mu.Lock()
	first := tunnel.client
	tunnel.mu.Unlock()
	second, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	tunnel.mu.Lock()
	reused := tunnel.client == first
	tunnel.mu.Unlock()
	if !reused {
		t.Fatal("SSH tunnel reconnected instead of reusing the session")
	}
	sshTunnels.Delete(proxyURL.String())
	tunnel.discard(first)
}

func TestSSHProxyRejectsWrongPassword(t *testing.T) {
	address := startTestSSHProxy(t, "root", "s3cret")
	proxyURL, err := url.Parse("ssh://root:wrong@" + address)
	if err != nil {
		t.Fatal(err)
	}
	tunnel, err := sshTunnelFor(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	defer sshTunnels.Delete(proxyURL.String())
	if _, err := tunnel.DialContext(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("expected authentication failure")
	}
}

func TestSSHDifferentEndpointsHandshakeConcurrently(t *testing.T) {
	ready := make(chan struct{})
	var callbacks atomic.Int32
	callback := func(_ ssh.ConnMetadata, _ []byte) (*ssh.Permissions, error) {
		if callbacks.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
			return nil, nil
		case <-time.After(2 * time.Second):
			return nil, fmt.Errorf("other SSH endpoint did not handshake concurrently")
		}
	}
	addresses := []string{
		startTestSSHProxyWithPasswordCallback(t, callback),
		startTestSSHProxyWithPasswordCallback(t, callback),
	}
	results := make(chan error, len(addresses))
	for _, address := range addresses {
		proxyURL, err := url.Parse("ssh://root:secret@" + address)
		if err != nil {
			t.Fatal(err)
		}
		tunnel, err := sshTunnelFor(proxyURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			sshTunnels.Delete(proxyURL.String())
			tunnel.mu.Lock()
			client := tunnel.client
			tunnel.mu.Unlock()
			tunnel.discard(client)
		})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := tunnel.ensureClient(ctx)
			results <- err
		}()
	}
	for range addresses {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
}

func TestProxyImportAcceptsSSHPoolProtocol(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	putJSON(t, handler, http.MethodPut, "/api/proxy-pools/1", map[string]any{
		"name": "ssh-pool", "source_type": "manual", "default_protocol": "ssh", "status": "active",
	}, http.StatusOK, nil)
	var result struct {
		Created int `json:"created"`
		Invalid int `json:"invalid"`
	}
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": 1, "text": "192.0.2.10:22:root:test-password\nssh://root:pw@198.51.100.9:2222",
	}, http.StatusCreated, &result)
	if result.Created != 2 || result.Invalid != 0 {
		t.Fatalf("import result = %+v, want 2 created", result)
	}
	var protocol, host, username, password string
	var port int
	if err := a.db.QueryRow(`SELECT protocol, host, port, username, password FROM proxies WHERE host = '192.0.2.10'`).Scan(&protocol, &host, &port, &username, &password); err != nil {
		t.Fatal(err)
	}
	if protocol != "ssh" || port != 22 || username != "root" || password != "test-password" {
		t.Fatalf("stored proxy = %s://%s:%s@%s:%d", protocol, username, password, host, port)
	}
	url, err := a.proxyURL(1)
	if err != nil {
		t.Fatal(err)
	}
	if !isSSHProxyURL(url) {
		t.Fatalf("proxy URL scheme = %q, want ssh", url.Scheme)
	}
}

func bytesContains(haystack []byte, needle string) bool {
	return len(needle) == 0 || indexOfBytes(haystack, needle) >= 0
}

func indexOfBytes(haystack []byte, needle string) int {
	limit := len(haystack) - len(needle)
	for index := 0; index <= limit; index++ {
		if string(haystack[index:index+len(needle)]) == needle {
			return index
		}
	}
	return -1
}
