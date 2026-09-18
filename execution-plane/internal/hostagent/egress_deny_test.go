package hostagent

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

// The matrix pairs every denial with an allow control so that a refusal is
// attributable to the named class rather than to a broken fixture.
func TestClassifyEgressTargetDenyMatrix(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name string
		host string
		port uint16
		want egressDenyReason
	}{
		{"imds-ipv4", "169.254.169.254", 80, denyReasonMetadataHost},
		{"imds-ipv4-any-port", "169.254.169.254", 443, denyReasonMetadataHost},
		{"ecs-task-metadata", "169.254.170.2", 80, denyReasonMetadataHost},
		{"alibaba-metadata", "100.100.100.200", 80, denyReasonMetadataHost},
		{"imds-ipv6", "fd00:ec2::254", 80, denyReasonMetadataHost},
		{"metadata-hostname", "metadata.google.internal", 80, denyReasonMetadataHost},
		{"metadata-short-hostname", "metadata", 80, denyReasonMetadataHost},
		{"link-local-neighbour", "169.254.10.7", 8080, denyReasonLinkLocal},
		{"link-local-ipv6", "fe80::1", 8080, denyReasonLinkLocal},
		{"unspecified-ipv4", "0.0.0.0", 443, denyReasonUnspecified},
		{"unspecified-ipv6", "::", 443, denyReasonUnspecified},
		// 224.0.0.0/24 is the local network control block, so it is reported as
		// link-local rather than plain multicast. Both are denials.
		{"link-local-multicast", "224.0.0.251", 443, denyReasonLinkLocal},
		{"routable-multicast", "239.255.0.1", 443, denyReasonMulticast},
		{"broadcast", "255.255.255.255", 443, denyReasonMulticast},
		{"ipv6-literal", "2606:4700:4700::1111", 443, denyReasonIPv6Literal},
		{"ipv6-loopback-literal", "::1", 443, denyReasonIPv6Literal},
		{"dns-tcp", "8.8.8.8", 53, denyReasonResolverPort},
		{"dns-over-tls", "dns.example", 853, denyReasonResolverPort},
		{"mdns", "224.0.0.251", 5353, denyReasonResolverPort},
		{"resolver-port-beats-allowlisted-host", "api.anthropic.com", 53, denyReasonResolverPort},

		{"mapped-ipv6-public", "::ffff:203.0.113.10", 443, denyReasonIPv6Literal},
		{"mapped-ipv6-metadata", "::ffff:169.254.169.254", 80, denyReasonMetadataHost},

		// Allow controls: the classifier defers to the slot allowlist here.
		{"allow-upstream-host", "api.anthropic.com", 443, denyReasonAllowed},
		{"allow-public-ipv4-literal", "203.0.113.10", 443, denyReasonAllowed},
		{"allow-loopback-harness", "127.0.0.1", 8080, denyReasonAllowed},
		{"allow-private-bridge", "172.18.0.2", 8093, denyReasonAllowed},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			if got := classifyEgressTarget(testCase.host, testCase.port); got != testCase.want {
				t.Fatalf("classifyEgressTarget(%q, %d) = %q, want %q", testCase.host, testCase.port, got, testCase.want)
			}
		})
	}
}

// An address written as a non-dotted-quad number reaches the classifier as an
// opaque name but is resolved to that address by the upstream proxy, so it must
// be refused before the allowlist is ever consulted.
func TestEgressRejectsNumericallyEncodedAddressHosts(t *testing.T) {
	t.Parallel()
	for _, host := range []string{
		"2852039166",          // decimal 169.254.169.254
		"0251.0376.0251.0376", // octal 169.254.169.254
		"0xa9fea9fe",          // hex 169.254.169.254
		"169.254.169.254.",    // trailing dot is stripped, then dotted-quad
		"1.2.3.4.5",           // five numeric labels, not an IP literal
	} {
		if validTargetHost(strings.TrimSuffix(strings.ToLower(host), ".")) &&
			classifyEgressTarget(strings.TrimSuffix(strings.ToLower(host), "."), 443) == denyReasonAllowed {
			t.Fatalf("host %q survived both name validation and structural classification", host)
		}
		if _, err := parseConnectTarget(host + ":443"); err == nil {
			if reason := classifyEgressTarget(strings.TrimSuffix(strings.ToLower(host), "."), 443); !reason.denied() {
				t.Fatalf("CONNECT target %q:443 was neither rejected nor classified as denied", host)
			}
		}
		if _, err := newTargetPolicy([]string{host + ":443"}); err == nil {
			t.Fatalf("newTargetPolicy accepted numeric address host %q", host)
		}
	}
	// Allow control: a real name whose rightmost label is a letter-initial TLD.
	if _, err := newTargetPolicy([]string{"api.anthropic.com:443", "1password.com:443"}); err != nil {
		t.Fatalf("allow control policy: %v", err)
	}
}

// A rejected registration must not disturb the binding it failed to replace.
func TestEgressRegistryRejectedRegisterKeepsExistingTunnels(t *testing.T) {
	t.Parallel()
	proxy, err := ParseUpstreamProxy("http://127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	registry := NewEgressRegistry()
	var observed []EgressRevocation
	var mu sync.Mutex
	registry.WatchRevocations(func(event EgressRevocation) {
		mu.Lock()
		defer mu.Unlock()
		observed = append(observed, event)
	})
	binding := EgressBinding{
		SourceIP:     netip.MustParseAddr("172.18.0.2"),
		Claim:        lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 2, OwnerID: "host-agent-1"},
		ProxyLeaseID: "proxy-lease-2", Proxy: proxy, AllowedTargets: []string{"api.anthropic.com:443"},
	}
	if err := registry.Register(binding); err != nil {
		t.Fatal(err)
	}
	stale := binding
	stale.Claim.ExecutionEpoch = 1
	stale.ProxyLeaseID = "proxy-lease-1"
	if err := registry.Register(stale); !errors.Is(err, ErrEgressBindingConflict) {
		t.Fatalf("stale epoch register error = %v, want conflict", err)
	}
	otherSlot := binding
	otherSlot.Claim.SlotID = "slot-2"
	if err := registry.Register(otherSlot); !errors.Is(err, ErrEgressBindingConflict) {
		t.Fatalf("source reuse register error = %v, want conflict", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 0 {
		t.Fatalf("failed registrations emitted revocations: %+v", observed)
	}
}

// Serve must release its registry subscription so a finished gateway is not
// pinned by the registry for the process lifetime.
func TestEgressGatewayUnsubscribesOnServeReturn(t *testing.T) {
	t.Parallel()
	proxyURL, _, _, stopProxy := startHTTPConnectProxy(t, false, "", "")
	defer stopProxy()
	proxy, err := ParseUpstreamProxy(proxyURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	target, stopTarget := startEchoServer(t)
	defer stopTarget()
	_, registry, binding, stop := startRevocableEgressGateway(t, proxy, target)
	registry.mu.RLock()
	subscribed := len(registry.listeners)
	registry.mu.RUnlock()
	if subscribed != 1 {
		t.Fatalf("listener count while serving = %d, want 1", subscribed)
	}
	stop()
	registry.mu.RLock()
	remaining := len(registry.listeners)
	registry.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("listener count after Serve returned = %d, want 0", remaining)
	}
	if err := registry.Unregister(binding.SourceIP, binding.Claim.SlotID, binding.Claim.ExecutionEpoch); err != nil {
		t.Fatalf("unregister after shutdown: %v", err)
	}

	// A gateway that can never serve must not stay subscribed either.
	unservable, err := NewEgressGateway(EgressGatewayConfig{Registry: registry, Fencer: mustFencer(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := unservable.Serve(context.Background(), nil); err == nil {
		t.Fatal("Serve accepted a nil listener")
	}
	registry.mu.RLock()
	afterUnservable := len(registry.listeners)
	registry.mu.RUnlock()
	if afterUnservable != 0 {
		t.Fatalf("listener count after failed Serve = %d, want 0", afterUnservable)
	}
}

func mustFencer(t *testing.T) *lease.Fencer {
	t.Helper()
	fencer, err := lease.NewFencer(lease.NewMemoryBackend(time.Now))
	if err != nil {
		t.Fatal(err)
	}
	return fencer
}

// A structurally denied allowlist entry must fail the whole binding: the slot
// is left without egress rather than with a partially applied rule set.
func TestEgressRegistryRefusesStructurallyDeniedAllowlistRule(t *testing.T) {
	t.Parallel()
	proxy, err := ParseUpstreamProxy("http://127.0.0.1:8080", nil)
	if err != nil {
		t.Fatal(err)
	}
	claim := lease.Claim{SlotID: "slot-1", NodeID: "srv74", ExecutionEpoch: 1, OwnerID: "host-agent-1"}
	source := netip.MustParseAddr("172.18.0.2")
	for _, rule := range []string{
		"169.254.169.254:80",
		"metadata.google.internal:80",
		"fe80::1:443",
		"8.8.8.8:53",
		"[2606:4700:4700::1111]:443",
		"*.metadata.goog:443",
	} {
		registry := NewEgressRegistry()
		err := registry.Register(EgressBinding{
			SourceIP: source, Claim: claim, ProxyLeaseID: "proxy-lease-1", Proxy: proxy,
			AllowedTargets: []string{"api.anthropic.com:443", rule},
		})
		if err == nil {
			t.Fatalf("Register accepted denied allowlist rule %q", rule)
		}
		// Fail closed: the surviving legitimate rule must not be installed either.
		if _, resolveErr := registry.resolve(net.JoinHostPort(source.String(), "40000")); !errors.Is(resolveErr, ErrEgressBindingNotFound) {
			t.Fatalf("rule %q left a partial binding: %v", rule, resolveErr)
		}
	}
	registry := NewEgressRegistry()
	if err := registry.Register(EgressBinding{
		SourceIP: source, Claim: claim, ProxyLeaseID: "proxy-lease-1", Proxy: proxy,
		AllowedTargets: []string{"api.anthropic.com:443", "*.claude.ai:443"},
	}); err != nil {
		t.Fatalf("allow control registration: %v", err)
	}
}

// Every refusal carries its class, so an isolation claim never rests on a
// connection that merely hung.
func TestEgressGatewayReportsDenyReasonOverAllowlistedBinding(t *testing.T) {
	t.Parallel()
	target, stopTarget := startEchoServer(t)
	defer stopTarget()
	proxyURL, _, _, stopProxy := startHTTPConnectProxy(t, false, "", "")
	defer stopProxy()
	proxy, err := ParseUpstreamProxy(proxyURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	gatewayAddress, _, _, _, stop := startTestEgressGateway(t, proxy, target)
	defer stop()

	for _, testCase := range []struct {
		target string
		reason egressDenyReason
	}{
		{"169.254.169.254:80", denyReasonMetadataHost},
		{"metadata.google.internal:80", denyReasonMetadataHost},
		{"169.254.10.7:8080", denyReasonLinkLocal},
		{"0.0.0.0:443", denyReasonUnspecified},
		{"239.255.0.1:443", denyReasonMulticast},
		{"224.0.0.251:443", denyReasonLinkLocal},
		{"[2606:4700:4700::1111]:443", denyReasonIPv6Literal},
		{"8.8.8.8:53", denyReasonResolverPort},
		{"api.anthropic.com:443", denyReasonOutsideAllowed},
	} {
		connection, response := openGatewayTunnel(t, gatewayAddress, testCase.target)
		connection.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("CONNECT %s status = %d, want 403", testCase.target, response.StatusCode)
		}
		if got := response.Header.Get(EgressDenyReasonHeader); got != string(testCase.reason) {
			t.Fatalf("CONNECT %s deny reason = %q, want %q", testCase.target, got, testCase.reason)
		}
	}

	connection, response := openGatewayTunnel(t, gatewayAddress, target)
	defer connection.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("allow control status = %d, want 200", response.StatusCode)
	}
	assertTunnelEcho(t, connection, []byte("allow-control"))
}

// Revoking the proxy binding must reclaim egress that is already relaying.
// Refusing only the next CONNECT would leave the previous generation online.
func TestEgressGatewayClosesInFlightTunnelOnBindingRevocation(t *testing.T) {
	t.Parallel()
	for _, revoke := range []struct {
		name string
		call func(*testing.T, *EgressRegistry, EgressBinding)
	}{
		{"unregister", func(t *testing.T, registry *EgressRegistry, binding EgressBinding) {
			if err := registry.Unregister(binding.SourceIP, binding.Claim.SlotID, binding.Claim.ExecutionEpoch); err != nil {
				t.Fatalf("unregister binding: %v", err)
			}
		}},
		{"superseded-by-newer-epoch", func(t *testing.T, registry *EgressRegistry, binding EgressBinding) {
			next := binding
			next.Claim.ExecutionEpoch = binding.Claim.ExecutionEpoch + 1
			next.ProxyLeaseID = "proxy-lease-2"
			if err := registry.Register(next); err != nil {
				t.Fatalf("register newer epoch: %v", err)
			}
		}},
	} {
		revoke := revoke
		t.Run(revoke.name, func(t *testing.T) {
			t.Parallel()
			target, stopTarget := startEchoServer(t)
			defer stopTarget()
			proxyURL, _, _, stopProxy := startHTTPConnectProxy(t, false, "", "")
			defer stopProxy()
			proxy, err := ParseUpstreamProxy(proxyURL, nil)
			if err != nil {
				t.Fatal(err)
			}
			gatewayAddress, registry, binding, stop := startRevocableEgressGateway(t, proxy, target)
			defer stop()

			connection, response := openGatewayTunnel(t, gatewayAddress, target)
			defer connection.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT status = %d, want 200", response.StatusCode)
			}
			assertTunnelEcho(t, connection, []byte("before-revocation"))

			revoke.call(t, registry, binding)
			assertTunnelClosed(t, connection)
		})
	}
}

func assertTunnelClosed(t *testing.T, connection net.Conn) {
	t.Helper()
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	defer func() { _ = connection.SetDeadline(time.Time{}) }()
	// A write may still land in the socket buffer after the peer closed, so the
	// read is what proves the tunnel is gone rather than merely idle.
	_, _ = connection.Write([]byte("after-revocation"))
	buffer := make([]byte, 1)
	if _, err := connection.Read(buffer); err == nil {
		t.Fatal("revoked binding left the in-flight tunnel usable")
	} else if errors.Is(err, context.DeadlineExceeded) || isTimeout(err) {
		t.Fatalf("tunnel timed out instead of being closed: %v", err)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func startRevocableEgressGateway(t *testing.T, proxy *UpstreamProxy, target string) (string, *EgressRegistry, EgressBinding, func()) {
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
	binding := EgressBinding{
		SourceIP: netip.MustParseAddr("127.0.0.1"), Claim: claim, ProxyLeaseID: "proxy-lease-1",
		Proxy: proxy, AllowedTargets: []string{target},
	}
	if err := registry.Register(binding); err != nil {
		t.Fatal(err)
	}
	gateway, err := NewEgressGateway(EgressGatewayConfig{
		// A long revalidate interval keeps the execution lease fencer out of the
		// way: the teardown under test must come from the binding revocation.
		Registry: registry, Fencer: fencer, RevalidateInterval: time.Minute, MaxTunnelDuration: time.Minute,
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
	return listener.Addr().String(), registry, binding, stop
}
