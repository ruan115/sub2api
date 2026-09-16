// Package fixedtransport constructs the worker's explicit, fail-closed HTTP
// egress path. It does not read ambient proxy variables or accept TLS overrides.
package fixedtransport

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

var ErrProxyURL = errors.New("worker egress proxy must be a credential-free internal HTTP origin with a valid port")

// ValidateProxyURL accepts only the host-agent origin supplied by SlotSpec.
// In particular, an absent proxy is an error, never a request for direct egress.
func ValidateProxyURL(raw string) error {
	_, err := parseProxyURL(raw)
	return err
}

func parseProxyURL(raw string) (*url.URL, error) {
	if err := provider.ValidateEgressProxyURL(raw); err != nil {
		return nil, ErrProxyURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, ErrProxyURL
	}
	return u, nil
}

// New returns a fresh transport with its own connection pool and verifying TLS
// configuration. The fixed Proxy function also applies to loopback targets;
// HTTP_PROXY, HTTPS_PROXY, ALL_PROXY and NO_PROXY are never consulted. HTTP
// transport does not fall back to a direct connection if this proxy fails.
func New(proxyURL string) (*http.Transport, error) {
	proxy, err := parseProxyURL(proxyURL)
	if err != nil {
		return nil, err
	}
	return &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			// Do not expose the shared URL to mutation by a caller inspecting it.
			copy := *proxy
			return &copy, nil
		},
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   16,
	}, nil
}
