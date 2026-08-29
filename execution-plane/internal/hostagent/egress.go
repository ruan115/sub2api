package hostagent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
)

const (
	defaultEgressRevalidateInterval = 5 * time.Second
	defaultEgressTunnelDuration     = 20 * time.Minute
	maxEgressHeaderBytes            = 16 << 10
)

var (
	ErrEgressBindingConflict = errors.New("egress source is already bound to another slot or epoch")
	ErrEgressBindingNotFound = errors.New("egress source binding not found")
)

type EgressBinding struct {
	SourceIP       netip.Addr
	Claim          lease.Claim
	ProxyLeaseID   string
	Proxy          *UpstreamProxy
	AllowedTargets []string
}

func (b EgressBinding) String() string {
	return fmt.Sprintf("EgressBinding{SourceIP:%s SlotID:%q NodeID:%q Epoch:%d OwnerID:%q ProxyLeaseID:%q Proxy:%s AllowedTargets:%v}",
		b.SourceIP, b.Claim.SlotID, b.Claim.NodeID, b.Claim.ExecutionEpoch, b.Claim.OwnerID,
		b.ProxyLeaseID, b.Proxy, b.AllowedTargets)
}

func (b EgressBinding) GoString() string { return b.String() }

func (b EgressBinding) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		SourceIP       string   `json:"source_ip"`
		SlotID         string   `json:"slot_id"`
		NodeID         string   `json:"node_id"`
		ExecutionEpoch uint64   `json:"execution_epoch"`
		OwnerID        string   `json:"owner_id"`
		ProxyLeaseID   string   `json:"proxy_lease_id"`
		Proxy          string   `json:"proxy"`
		AllowedTargets []string `json:"allowed_targets"`
	}{
		b.SourceIP.String(), b.Claim.SlotID, b.Claim.NodeID, b.Claim.ExecutionEpoch,
		b.Claim.OwnerID, b.ProxyLeaseID, b.Proxy.String(), append([]string(nil), b.AllowedTargets...),
	})
}

type registeredEgressBinding struct {
	binding EgressBinding
	policy  targetPolicy
}

type EgressRegistry struct {
	mu       sync.RWMutex
	bySource map[netip.Addr]registeredEgressBinding
	bySlot   map[string]netip.Addr
}

func NewEgressRegistry() *EgressRegistry {
	return &EgressRegistry{
		bySource: make(map[netip.Addr]registeredEgressBinding),
		bySlot:   make(map[string]netip.Addr),
	}
}

func (r *EgressRegistry) Register(binding EgressBinding) error {
	stored, err := validateEgressBinding(binding)
	if err != nil {
		return err
	}
	source := binding.SourceIP.Unmap()
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, exists := r.bySource[source]; exists {
		if current.binding.Claim.SlotID != binding.Claim.SlotID || binding.Claim.ExecutionEpoch < current.binding.Claim.ExecutionEpoch {
			return ErrEgressBindingConflict
		}
		if binding.Claim.ExecutionEpoch == current.binding.Claim.ExecutionEpoch {
			if current.binding.ProxyLeaseID != binding.ProxyLeaseID || current.binding.Claim != binding.Claim ||
				current.binding.Proxy != binding.Proxy || !slices.Equal(current.binding.AllowedTargets, binding.AllowedTargets) {
				return ErrEgressBindingConflict
			}
			return nil
		}
	}
	if previousSource, exists := r.bySlot[binding.Claim.SlotID]; exists && previousSource != source {
		previous := r.bySource[previousSource]
		if binding.Claim.ExecutionEpoch <= previous.binding.Claim.ExecutionEpoch {
			return ErrEgressBindingConflict
		}
		delete(r.bySource, previousSource)
	}
	r.bySource[source] = stored
	r.bySlot[binding.Claim.SlotID] = source
	return nil
}

func (r *EgressRegistry) Unregister(sourceIP netip.Addr, slotID string, epoch uint64) error {
	source := sourceIP.Unmap()
	r.mu.Lock()
	defer r.mu.Unlock()
	current, exists := r.bySource[source]
	if !exists || current.binding.Claim.SlotID != slotID || current.binding.Claim.ExecutionEpoch != epoch {
		return ErrEgressBindingNotFound
	}
	delete(r.bySource, source)
	if r.bySlot[slotID] == source {
		delete(r.bySlot, slotID)
	}
	return nil
}

func (r *EgressRegistry) resolve(remoteAddress string) (registeredEgressBinding, error) {
	host, _, err := net.SplitHostPort(remoteAddress)
	if err != nil {
		return registeredEgressBinding{}, ErrEgressBindingNotFound
	}
	source, err := netip.ParseAddr(host)
	if err != nil {
		return registeredEgressBinding{}, ErrEgressBindingNotFound
	}
	source = source.Unmap()
	r.mu.RLock()
	defer r.mu.RUnlock()
	binding, exists := r.bySource[source]
	if !exists {
		return registeredEgressBinding{}, ErrEgressBindingNotFound
	}
	return binding, nil
}

type EgressGatewayConfig struct {
	Registry           *EgressRegistry
	Fencer             *lease.Fencer
	RevalidateInterval time.Duration
	MaxTunnelDuration  time.Duration
}

type EgressGateway struct {
	registry           *EgressRegistry
	fencer             *lease.Fencer
	revalidateInterval time.Duration
	maxTunnelDuration  time.Duration

	mu      sync.Mutex
	server  *http.Server
	tunnels map[*protectedTunnel]struct{}
}

func NewEgressGateway(config EgressGatewayConfig) (*EgressGateway, error) {
	if config.Registry == nil || config.Fencer == nil {
		return nil, errors.New("egress registry and execution lease fencer are required")
	}
	if config.RevalidateInterval == 0 {
		config.RevalidateInterval = defaultEgressRevalidateInterval
	}
	if config.MaxTunnelDuration == 0 {
		config.MaxTunnelDuration = defaultEgressTunnelDuration
	}
	if config.RevalidateInterval <= 0 || config.RevalidateInterval > time.Minute ||
		config.MaxTunnelDuration <= 0 || config.MaxTunnelDuration > 24*time.Hour {
		return nil, errors.New("egress gateway timing is invalid")
	}
	return &EgressGateway{
		registry: config.Registry, fencer: config.Fencer,
		revalidateInterval: config.RevalidateInterval, maxTunnelDuration: config.MaxTunnelDuration,
		tunnels: make(map[*protectedTunnel]struct{}),
	}, nil
}

func (g *EgressGateway) Serve(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("egress listener is required")
	}
	server := &http.Server{
		Handler: g, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: maxEgressHeaderBytes, ErrorLog: log.New(io.Discard, "", 0),
	}
	g.mu.Lock()
	if g.server != nil {
		g.mu.Unlock()
		return errors.New("egress gateway is already serving")
	}
	g.server = server
	g.mu.Unlock()

	shutdownDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdownContext)
			g.closeAllTunnels()
		case <-shutdownDone:
		}
	}()
	go g.revalidate(ctx, shutdownDone)
	err := server.Serve(listener)
	close(shutdownDone)
	g.closeAllTunnels()
	if errors.Is(err, http.ErrServerClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

func (g *EgressGateway) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodConnect {
		response.Header().Set("Connection", "close")
		http.Error(response, "CONNECT required", http.StatusMethodNotAllowed)
		return
	}
	binding, err := g.registry.resolve(request.RemoteAddr)
	if err != nil {
		response.Header().Set("Connection", "close")
		http.Error(response, "egress binding unavailable", http.StatusForbidden)
		return
	}
	target, err := parseConnectTarget(request.Host)
	if err != nil || !binding.policy.allows(target) {
		response.Header().Set("Connection", "close")
		http.Error(response, "egress target denied", http.StatusForbidden)
		return
	}
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		http.Error(response, "CONNECT unavailable", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	tunnel := &protectedTunnel{client: client}
	g.trackTunnel(tunnel)
	defer func() {
		g.untrackTunnel(tunnel)
		tunnel.Close()
	}()
	release, err := g.fencer.Admit(request.Context(), binding.binding.Claim, tunnel.Close)
	if err != nil {
		writeHijackedStatus(buffered, http.StatusServiceUnavailable)
		return
	}
	defer release()

	upstream, err := binding.binding.Proxy.dialTunnel(request.Context(), target.address)
	if err != nil {
		writeHijackedStatus(buffered, http.StatusBadGateway)
		return
	}
	if !tunnel.SetUpstream(upstream) {
		return
	}
	deadline := time.Now().Add(g.maxTunnelDuration)
	_ = client.SetDeadline(deadline)
	_ = upstream.SetDeadline(deadline)
	if err := writeHijackedStatus(buffered, http.StatusOK); err != nil {
		return
	}
	relayTunnel(client, buffered.Reader, upstream)
}

func (g *EgressGateway) revalidate(ctx context.Context, shutdown <-chan struct{}) {
	ticker := time.NewTicker(g.revalidateInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-shutdown:
			return
		case <-ticker.C:
			checkContext, cancel := context.WithTimeout(ctx, g.revalidateInterval)
			_ = g.fencer.Revalidate(checkContext)
			cancel()
		}
	}
}

func (g *EgressGateway) trackTunnel(tunnel *protectedTunnel) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tunnels[tunnel] = struct{}{}
}

func (g *EgressGateway) untrackTunnel(tunnel *protectedTunnel) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.tunnels, tunnel)
}

func (g *EgressGateway) closeAllTunnels() {
	g.mu.Lock()
	tunnels := make([]*protectedTunnel, 0, len(g.tunnels))
	for tunnel := range g.tunnels {
		tunnels = append(tunnels, tunnel)
	}
	g.tunnels = make(map[*protectedTunnel]struct{})
	g.mu.Unlock()
	for _, tunnel := range tunnels {
		tunnel.Close()
	}
}

type protectedTunnel struct {
	mu       sync.Mutex
	client   net.Conn
	upstream net.Conn
	closed   bool
}

func (t *protectedTunnel) SetUpstream(connection net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = connection.Close()
		return false
	}
	t.upstream = connection
	return true
}

func (t *protectedTunnel) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	client, upstream := t.client, t.upstream
	t.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
	if upstream != nil {
		_ = upstream.Close()
	}
}

func relayTunnel(client net.Conn, clientReader *bufio.Reader, upstream net.Conn) {
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

func closeWrite(connection net.Conn) {
	if writer, ok := connection.(interface{ CloseWrite() error }); ok {
		_ = writer.CloseWrite()
	}
}

func writeHijackedStatus(buffered *bufio.ReadWriter, statusCode int) error {
	if buffered == nil {
		return errors.New("hijacked connection buffer is unavailable")
	}
	connection := "close"
	if statusCode == http.StatusOK {
		connection = "keep-alive"
	}
	_, err := fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\nConnection: %s\r\n\r\n", statusCode, http.StatusText(statusCode), connection)
	if err != nil {
		return err
	}
	return buffered.Flush()
}

type connectTarget struct {
	host    string
	port    uint16
	address string
}

func parseConnectTarget(raw string) (connectTarget, error) {
	if len(raw) == 0 || len(raw) > 512 || strings.ContainsAny(raw, "\x00\r\n/@") {
		return connectTarget{}, errors.New("CONNECT target is invalid")
	}
	host, portText, err := net.SplitHostPort(raw)
	if err != nil {
		return connectTarget{}, errors.New("CONNECT target must include a port")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !validTargetHost(host) {
		return connectTarget{}, errors.New("CONNECT target host is invalid")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return connectTarget{}, errors.New("CONNECT target port is invalid")
	}
	canonicalPort := strconv.Itoa(int(port))
	return connectTarget{host: host, port: uint16(port), address: net.JoinHostPort(host, canonicalPort)}, nil
}

type targetPolicy struct {
	exact    map[string]struct{}
	wildcard []connectTarget
}

func newTargetPolicy(values []string) (targetPolicy, error) {
	if len(values) == 0 || len(values) > 128 {
		return targetPolicy{}, errors.New("at least one allowed egress target is required")
	}
	policy := targetPolicy{exact: make(map[string]struct{})}
	for _, value := range values {
		host, portText, err := net.SplitHostPort(value)
		if err != nil {
			return targetPolicy{}, errors.New("allowed egress target must include a port")
		}
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		wildcard := strings.HasPrefix(host, "*.")
		plainHost := strings.TrimPrefix(host, "*.")
		if !validTargetHost(plainHost) {
			return targetPolicy{}, errors.New("allowed egress target host is invalid")
		}
		port, err := strconv.ParseUint(portText, 10, 16)
		if err != nil || port == 0 {
			return targetPolicy{}, errors.New("allowed egress target port is invalid")
		}
		if wildcard {
			if net.ParseIP(plainHost) != nil {
				return targetPolicy{}, errors.New("IP egress targets cannot use wildcards")
			}
			policy.wildcard = append(policy.wildcard, connectTarget{host: plainHost, port: uint16(port)})
			continue
		}
		policy.exact[net.JoinHostPort(plainHost, strconv.Itoa(int(port)))] = struct{}{}
	}
	return policy, nil
}

func (p targetPolicy) allows(target connectTarget) bool {
	if _, exists := p.exact[target.address]; exists {
		return true
	}
	for _, allowed := range p.wildcard {
		if target.port == allowed.port && strings.HasSuffix(target.host, "."+allowed.host) && target.host != allowed.host {
			return true
		}
	}
	return false
}

func validTargetHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-') {
				return false
			}
		}
	}
	return true
}

func validateEgressBinding(binding EgressBinding) (registeredEgressBinding, error) {
	source := binding.SourceIP.Unmap()
	if !source.IsValid() || (!source.IsPrivate() && !source.IsLoopback()) || binding.Claim.Validate() != nil ||
		binding.Proxy == nil || binding.ProxyLeaseID == "" || len(binding.ProxyLeaseID) > 128 {
		return registeredEgressBinding{}, errors.New("egress binding is invalid")
	}
	policy, err := newTargetPolicy(binding.AllowedTargets)
	if err != nil {
		return registeredEgressBinding{}, err
	}
	binding.SourceIP = source
	binding.AllowedTargets = append([]string(nil), binding.AllowedTargets...)
	return registeredEgressBinding{binding: binding, policy: policy}, nil
}
