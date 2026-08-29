package hostagent

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	proxyHandshakeTimeout  = 10 * time.Second
	maxProxyHandshakeBytes = 64 << 10
)

// UpstreamProxy holds the account's fixed remote proxy only inside the
// host-agent. Its string and JSON forms deliberately omit credentials.
type UpstreamProxy struct {
	scheme    string
	address   string
	username  string
	password  string
	tlsConfig *tls.Config
}

func ParseUpstreamProxy(rawURL string, tlsConfig *tls.Config) (*UpstreamProxy, error) {
	if rawURL == "" || len(rawURL) > 4096 {
		return nil, errors.New("upstream proxy URL is invalid")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, errors.New("upstream proxy URL is invalid")
	}
	scheme := strings.ToLower(parsed.Scheme)
	defaultPort := ""
	switch scheme {
	case "http":
		defaultPort = "80"
	case "https":
		defaultPort = "443"
	case "socks5", "socks5h":
		defaultPort = "1080"
	default:
		return nil, errors.New("upstream proxy scheme must be http, https or socks5")
	}
	port := parsed.Port()
	if port == "" {
		port = defaultPort
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil || portNumber == 0 {
		return nil, errors.New("upstream proxy port is invalid")
	}
	username, password := "", ""
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
		if len(username) > 1024 || len(password) > 2048 || strings.ContainsAny(username+password, "\x00\r\n") {
			return nil, errors.New("upstream proxy credentials are invalid")
		}
	}
	config := tlsConfig
	if config != nil {
		if config.InsecureSkipVerify {
			return nil, errors.New("upstream proxy TLS verification cannot be disabled")
		}
		config = config.Clone()
	}
	canonicalPort := strconv.Itoa(int(portNumber))
	return &UpstreamProxy{
		scheme: scheme, address: net.JoinHostPort(parsed.Hostname(), canonicalPort),
		username: username, password: password, tlsConfig: config,
	}, nil
}

func (p *UpstreamProxy) String() string {
	if p == nil {
		return "UpstreamProxy<nil>"
	}
	auth := ""
	if p.username != "" || p.password != "" {
		auth = "[REDACTED]@"
	}
	return p.scheme + "://" + auth + p.address
}

func (p *UpstreamProxy) GoString() string { return p.String() }

func (p *UpstreamProxy) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Scheme  string `json:"scheme"`
		Address string `json:"address"`
	}{p.scheme, p.address})
}

func (p *UpstreamProxy) dialTunnel(ctx context.Context, target string) (net.Conn, error) {
	if p == nil {
		return nil, errors.New("upstream proxy is unavailable")
	}
	dialContext, cancel := context.WithTimeout(ctx, proxyHandshakeTimeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: proxyHandshakeTimeout, KeepAlive: 30 * time.Second}
	switch p.scheme {
	case "socks5", "socks5h":
		var auth *xproxy.Auth
		if p.username != "" || p.password != "" {
			auth = &xproxy.Auth{User: p.username, Password: p.password}
		}
		proxyDialer, err := xproxy.SOCKS5("tcp", p.address, auth, dialer)
		if err != nil {
			return nil, errors.New("initialize SOCKS5 proxy failed")
		}
		contextDialer, ok := proxyDialer.(xproxy.ContextDialer)
		if !ok {
			return nil, errors.New("SOCKS5 proxy lacks context dialing")
		}
		connection, err := contextDialer.DialContext(dialContext, "tcp", target)
		if err != nil {
			return nil, errors.New("SOCKS5 proxy connection failed")
		}
		return connection, nil
	case "http", "https":
		return p.dialHTTPTunnel(dialContext, dialer, target)
	default:
		return nil, errors.New("upstream proxy scheme is unsupported")
	}
}

func (p *UpstreamProxy) dialHTTPTunnel(ctx context.Context, dialer *net.Dialer, target string) (net.Conn, error) {
	var connection net.Conn
	var err error
	if p.scheme == "https" {
		config := p.tlsConfig
		if config == nil {
			config = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			config = config.Clone()
		}
		if config.MinVersion == 0 {
			config.MinVersion = tls.VersionTLS12
		}
		if config.ServerName == "" {
			config.ServerName = proxyServerName(p.address)
		}
		config.NextProtos = []string{"http/1.1"}
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: config}
		connection, err = tlsDialer.DialContext(ctx, "tcp", p.address)
	} else {
		connection, err = dialer.DialContext(ctx, "tcp", p.address)
	}
	if err != nil {
		return nil, errors.New("HTTP proxy connection failed")
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = connection.Close()
		}
	}()
	_ = connection.SetDeadline(time.Now().Add(proxyHandshakeTimeout))

	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: make(http.Header),
	}
	request.Header.Set("User-Agent", "sub2api-host-agent/egress-v1")
	if p.username != "" || p.password != "" {
		encoded := base64.StdEncoding.EncodeToString([]byte(p.username + ":" + p.password))
		request.Header.Set("Proxy-Authorization", "Basic "+encoded)
	}
	if err := request.Write(connection); err != nil {
		return nil, errors.New("write HTTP proxy CONNECT failed")
	}
	limited := &proxyHandshakeReader{reader: connection, remaining: maxProxyHandshakeBytes, limited: true}
	reader := bufio.NewReaderSize(limited, 32<<10)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, errors.New("read HTTP proxy CONNECT response failed")
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("HTTP proxy CONNECT rejected with status %d", response.StatusCode)
	}
	limited.limited = false
	_ = connection.SetDeadline(time.Time{})
	succeeded = true
	return &bufferedConnection{Conn: connection, reader: reader}, nil
}

func proxyServerName(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ""
	}
	return host
}

type bufferedConnection struct {
	net.Conn
	reader *bufio.Reader
}

type proxyHandshakeReader struct {
	reader    io.Reader
	remaining int64
	limited   bool
}

func (r *proxyHandshakeReader) Read(buffer []byte) (int, error) {
	if !r.limited {
		return r.reader.Read(buffer)
	}
	if r.remaining <= 0 {
		return 0, errors.New("HTTP proxy response headers exceed limit")
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	count, err := r.reader.Read(buffer)
	r.remaining -= int64(count)
	return count, err
}

func (c *bufferedConnection) Read(buffer []byte) (int, error) {
	return c.reader.Read(buffer)
}
