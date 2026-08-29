package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/ccmax-manager/internal/executionv1"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

const (
	executionRouteRedisPrefix = "execution:route:v1:"
	defaultExecutionRouteTTL  = 10 * time.Second
	defaultExecutionCacheSize = 2048
	maxExecutionRequestBytes  = 32 << 20
	maxExecutionHeaderBytes   = 32 << 10
)

var (
	errExecutionRouteNotFound = errors.New("execution route not found")
	errExecutionRouteStale    = errors.New("execution route is stale")
	errExecutionRouteConflict = errors.New("execution route conflicts with the active epoch")
	errExecutionClientClosed  = errors.New("execution data-plane client is closed")
	executionSlotIDPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

type executionRoute struct {
	SlotID     string
	NodeID     string
	Endpoint   string
	Epoch      uint64
	Generation uint64
	ExpiresAt  time.Time
}

func (route executionRoute) validate(now time.Time) error {
	if !executionSlotIDPattern.MatchString(route.SlotID) || strings.TrimSpace(route.NodeID) == "" || len(route.NodeID) > 128 || route.Epoch == 0 || route.Generation == 0 {
		return errors.New("execution route identity is incomplete")
	}
	if !route.ExpiresAt.After(now) {
		return errExecutionRouteNotFound
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(route.Endpoint))
	if err != nil || host == "" || port == "" {
		return errors.New("execution route endpoint must be host:port")
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return errors.New("execution route endpoint port is invalid")
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return errors.New("execution route endpoint must be a private IP")
	}
	return nil
}

type executionRouteResolver interface {
	ResolveExecutionRoute(ctx context.Context, slotID string) (executionRoute, error)
}

type redisExecutionRouteResolver struct {
	runtime *redisRuntime
	now     func() time.Time
}

func (resolver redisExecutionRouteResolver) ResolveExecutionRoute(ctx context.Context, slotID string) (executionRoute, error) {
	if resolver.runtime == nil || resolver.runtime.client == nil {
		return executionRoute{}, errors.New("Redis execution route store is unavailable")
	}
	slotID = strings.TrimSpace(slotID)
	if slotID == "" {
		return executionRoute{}, errExecutionRouteNotFound
	}
	key := executionRouteRedisPrefix + slotID
	pipe := resolver.runtime.client.Pipeline()
	valuesCommand := pipe.HGetAll(ctx, key)
	ttlCommand := pipe.PTTL(ctx, key)
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return executionRoute{}, fmt.Errorf("resolve execution route: %w", err)
	}
	values := valuesCommand.Val()
	ttl := ttlCommand.Val()
	if len(values) == 0 || ttl <= 0 {
		return executionRoute{}, errExecutionRouteNotFound
	}
	epoch, err := strconv.ParseUint(values["execution_epoch"], 10, 64)
	if err != nil {
		return executionRoute{}, errors.New("execution route epoch is invalid")
	}
	generation, err := strconv.ParseUint(values["route_generation"], 10, 64)
	if err != nil {
		return executionRoute{}, errors.New("execution route generation is invalid")
	}
	now := time.Now
	if resolver.now != nil {
		now = resolver.now
	}
	route := executionRoute{
		SlotID: slotID, NodeID: values["node_id"], Endpoint: values["endpoint"],
		Epoch: epoch, Generation: generation, ExpiresAt: now().Add(ttl),
	}
	if storedSlotID := strings.TrimSpace(values["slot_id"]); storedSlotID != "" && storedSlotID != slotID {
		return executionRoute{}, errExecutionRouteConflict
	}
	if err := route.validate(now()); err != nil {
		return executionRoute{}, err
	}
	return route, nil
}

type executionRouteCacheEntry struct {
	route      executionRoute
	expiresAt  time.Time
	lastAccess uint64
}

type executionRouteCache struct {
	mu         sync.Mutex
	entries    map[string]executionRouteCacheEntry
	ttl        time.Duration
	capacity   int
	now        func() time.Time
	accessTick uint64
}

func newExecutionRouteCache(ttl time.Duration, capacity int, now func() time.Time) (*executionRouteCache, error) {
	if ttl <= 0 {
		return nil, errors.New("execution route cache TTL must be positive")
	}
	if capacity <= 0 {
		return nil, errors.New("execution route cache capacity must be positive")
	}
	if now == nil {
		now = time.Now
	}
	return &executionRouteCache{entries: map[string]executionRouteCacheEntry{}, ttl: ttl, capacity: capacity, now: now}, nil
}

func (cache *executionRouteCache) Get(slotID string, generation, epoch uint64) (executionRoute, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[slotID]
	if !ok {
		return executionRoute{}, false
	}
	if !entry.expiresAt.After(cache.now()) || entry.route.Generation != generation || entry.route.Epoch != epoch {
		delete(cache.entries, slotID)
		return executionRoute{}, false
	}
	cache.accessTick++
	entry.lastAccess = cache.accessTick
	cache.entries[slotID] = entry
	return entry.route, true
}

func (cache *executionRouteCache) Put(route executionRoute) error {
	now := cache.now()
	if err := route.validate(now); err != nil {
		return err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if current, ok := cache.entries[route.SlotID]; ok && current.expiresAt.After(now) {
		if route.Generation < current.route.Generation || route.Epoch < current.route.Epoch {
			return errExecutionRouteStale
		}
		if route.Generation == current.route.Generation && route.Epoch == current.route.Epoch &&
			(route.NodeID != current.route.NodeID || route.Endpoint != current.route.Endpoint) {
			return errExecutionRouteConflict
		}
	}
	if _, exists := cache.entries[route.SlotID]; !exists && len(cache.entries) >= cache.capacity {
		cache.evictOne(now)
	}
	expiresAt := now.Add(cache.ttl)
	if route.ExpiresAt.Before(expiresAt) {
		expiresAt = route.ExpiresAt
	}
	cache.accessTick++
	cache.entries[route.SlotID] = executionRouteCacheEntry{route: route, expiresAt: expiresAt, lastAccess: cache.accessTick}
	return nil
}

func (cache *executionRouteCache) Invalidate(slotID string, generation, epoch uint64) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[slotID]
	if ok && entry.route.Generation == generation && entry.route.Epoch == epoch {
		delete(cache.entries, slotID)
	}
}

func (cache *executionRouteCache) evictOne(now time.Time) {
	var candidate string
	var oldest uint64
	for slotID, entry := range cache.entries {
		if !entry.expiresAt.After(now) {
			delete(cache.entries, slotID)
			return
		}
		if candidate == "" || entry.lastAccess < oldest {
			candidate, oldest = slotID, entry.lastAccess
		}
	}
	if candidate != "" {
		delete(cache.entries, candidate)
	}
}

type executionTLSFiles struct {
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	ServerName     string
}

func loadExecutionTransportCredentials(files executionTLSFiles) (credentials.TransportCredentials, error) {
	if strings.TrimSpace(files.CAFile) == "" || strings.TrimSpace(files.ClientCertFile) == "" ||
		strings.TrimSpace(files.ClientKeyFile) == "" || strings.TrimSpace(files.ServerName) == "" {
		return nil, errors.New("execution mTLS CA, client certificate, client key and server name are required")
	}
	caPEM, err := os.ReadFile(files.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read execution CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("execution CA file contains no certificates")
	}
	source := &executionClientCertificateSource{certFile: files.ClientCertFile, keyFile: files.ClientKeyFile}
	if _, err := source.GetClientCertificate(nil); err != nil {
		return nil, err
	}
	return credentials.NewTLS(&tls.Config{
		MinVersion:           tls.VersionTLS13,
		ServerName:           strings.TrimSpace(files.ServerName),
		RootCAs:              roots,
		GetClientCertificate: source.GetClientCertificate,
	}), nil
}

type executionClientCertificateSource struct {
	mu       sync.Mutex
	certFile string
	keyFile  string
}

func (source *executionClientCertificateSource) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	source.mu.Lock()
	defer source.mu.Unlock()
	certificate, err := tls.LoadX509KeyPair(source.certFile, source.keyFile)
	if err != nil {
		return nil, fmt.Errorf("load execution client certificate: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return nil, errors.New("execution client certificate is empty")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("execution client leaf certificate is invalid")
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("execution client certificate is not currently valid")
	}
	certificate.Leaf = leaf
	return &certificate, nil
}

type executionDataPlaneClientConfig struct {
	Resolver             executionRouteResolver
	TransportCredentials credentials.TransportCredentials
	RouteTTL             time.Duration
	RouteCacheCapacity   int
	Now                  func() time.Time
	DialOptions          []grpc.DialOption
}

type executionDataPlaneClient struct {
	resolver executionRouteResolver
	routes   *executionRouteCache
	options  []grpc.DialOption

	mu          sync.Mutex
	connections map[string]*grpc.ClientConn
	closed      bool
}

func newExecutionDataPlaneClient(config executionDataPlaneClientConfig) (*executionDataPlaneClient, error) {
	if config.Resolver == nil {
		return nil, errors.New("execution route resolver is required")
	}
	if config.TransportCredentials == nil {
		return nil, errors.New("execution mTLS transport credentials are required")
	}
	if config.RouteTTL <= 0 {
		config.RouteTTL = defaultExecutionRouteTTL
	}
	if config.RouteCacheCapacity <= 0 {
		config.RouteCacheCapacity = defaultExecutionCacheSize
	}
	routes, err := newExecutionRouteCache(config.RouteTTL, config.RouteCacheCapacity, config.Now)
	if err != nil {
		return nil, err
	}
	options := []grpc.DialOption{
		grpc.WithTransportCredentials(config.TransportCredentials),
		grpc.WithNoProxy(),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallSendMsgSize(maxExecutionRequestBytes),
			grpc.MaxCallRecvMsgSize(maxExecutionRequestBytes),
		),
	}
	options = append(options, config.DialOptions...)
	return &executionDataPlaneClient{
		resolver: config.Resolver, routes: routes, options: options,
		connections: map[string]*grpc.ClientConn{},
	}, nil
}

func newExecutionDataPlaneClientFromEnv(runtime *redisRuntime) (*executionDataPlaneClient, error) {
	if !envEnabled("CCMAX_EXECUTION_DATAPLANE_ENABLED") {
		return nil, nil
	}
	if runtime == nil || runtime.client == nil {
		return nil, errors.New("CCMAX_EXECUTION_DATAPLANE_ENABLED requires CCMAX_REDIS_ADDR")
	}
	credentials, err := loadExecutionTransportCredentials(executionTLSFiles{
		CAFile: os.Getenv("CCMAX_EXECUTION_CA_FILE"), ClientCertFile: os.Getenv("CCMAX_EXECUTION_CLIENT_CERT_FILE"),
		ClientKeyFile: os.Getenv("CCMAX_EXECUTION_CLIENT_KEY_FILE"), ServerName: os.Getenv("CCMAX_EXECUTION_SERVER_NAME"),
	})
	if err != nil {
		return nil, err
	}
	routeTTL := defaultExecutionRouteTTL
	if raw := strings.TrimSpace(os.Getenv("CCMAX_EXECUTION_ROUTE_TTL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 || parsed > time.Minute {
			return nil, errors.New("CCMAX_EXECUTION_ROUTE_TTL must be between 1ns and 1m")
		}
		routeTTL = parsed
	}
	return newExecutionDataPlaneClient(executionDataPlaneClientConfig{
		Resolver: redisExecutionRouteResolver{runtime: runtime}, TransportCredentials: credentials,
		RouteTTL: routeTTL, RouteCacheCapacity: envInt("CCMAX_EXECUTION_ROUTE_CACHE_SIZE", defaultExecutionCacheSize),
	})
}

func envEnabled(key string) bool {
	value := strings.TrimSpace(os.Getenv(key))
	return value == "1" || strings.EqualFold(value, "true")
}

func (client *executionDataPlaneClient) Close() error {
	if client == nil {
		return nil
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	client.closed = true
	var result error
	for endpoint, connection := range client.connections {
		if err := connection.Close(); err != nil {
			result = errors.Join(result, err)
		}
		delete(client.connections, endpoint)
	}
	return result
}

type executionCall struct {
	RequestID       string
	AccountID       string
	SlotID          string
	Mode            executionv1.ExecutionMode
	SessionKey      string
	Body            []byte
	Headers         map[string]string
	ExecutionEpoch  uint64
	RouteGeneration uint64
}

func (call executionCall) validate() error {
	if strings.TrimSpace(call.RequestID) == "" || len(call.RequestID) > 128 || strings.TrimSpace(call.AccountID) == "" || len(call.AccountID) > 64 || !executionSlotIDPattern.MatchString(call.SlotID) {
		return errors.New("execution request, account and slot ids are required")
	}
	if call.ExecutionEpoch == 0 || call.RouteGeneration == 0 {
		return errors.New("execution epoch and route generation are required")
	}
	if call.Mode != executionv1.ExecutionMode_EXECUTION_MODE_CLI_NATIVE && call.Mode != executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API {
		return errors.New("execution mode is invalid")
	}
	if len(call.Body) == 0 || len(call.Body) > maxExecutionRequestBytes {
		return errors.New("execution request body size is invalid")
	}
	return nil
}

func (client *executionDataPlaneClient) CountTokens(ctx context.Context, call executionCall) (*executionv1.CountTokensResponse, error) {
	if err := call.validate(); err != nil {
		return nil, err
	}
	route, service, err := client.service(ctx, call)
	if err != nil {
		return nil, err
	}
	response, err := service.CountTokens(ctx, &executionv1.CountTokensRequest{
		RequestId: call.RequestID, AccountId: call.AccountID, Mode: call.Mode,
		AnthropicRequestJson: append([]byte(nil), call.Body...), SlotId: call.SlotID,
		ExecutionEpoch: call.ExecutionEpoch, RouteGeneration: call.RouteGeneration,
	})
	if err != nil {
		client.invalidateOnRouteFailure(route, err)
		return nil, err
	}
	return response, nil
}

func (client *executionDataPlaneClient) Execute(ctx context.Context, call executionCall) (*executionResponseStream, error) {
	if err := call.validate(); err != nil {
		return nil, err
	}
	headers, err := sanitizeExecutionHeaders(call.Headers)
	if err != nil {
		return nil, err
	}
	route, service, err := client.service(ctx, call)
	if err != nil {
		return nil, err
	}
	stream, err := service.Execute(ctx)
	if err != nil {
		client.invalidateOnRouteFailure(route, err)
		return nil, err
	}
	if err := stream.Send(&executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_Begin{Begin: &executionv1.BeginExecution{
		RequestId: call.RequestID, AccountId: call.AccountID, Mode: call.Mode, SessionKey: call.SessionKey,
		AnthropicRequestJson: append([]byte(nil), call.Body...), RequestHeaders: headers,
		SlotId: call.SlotID, ExecutionEpoch: call.ExecutionEpoch, RouteGeneration: call.RouteGeneration,
	}}}); err != nil {
		client.invalidateOnRouteFailure(route, err)
		return nil, err
	}
	return &executionResponseStream{stream: stream, client: client, route: route}, nil
}

func (client *executionDataPlaneClient) service(ctx context.Context, call executionCall) (executionRoute, executionv1.ExecutionDataPlaneServiceClient, error) {
	route, err := client.resolveRoute(ctx, call.SlotID, call.RouteGeneration, call.ExecutionEpoch)
	if err != nil {
		return executionRoute{}, nil, err
	}
	connection, err := client.connection(route.Endpoint)
	if err != nil {
		return executionRoute{}, nil, err
	}
	return route, executionv1.NewExecutionDataPlaneServiceClient(connection), nil
}

func (client *executionDataPlaneClient) resolveRoute(ctx context.Context, slotID string, generation, epoch uint64) (executionRoute, error) {
	if route, ok := client.routes.Get(slotID, generation, epoch); ok {
		return route, nil
	}
	route, err := client.resolver.ResolveExecutionRoute(ctx, slotID)
	if err != nil {
		return executionRoute{}, err
	}
	if route.SlotID != slotID || route.Generation != generation || route.Epoch != epoch {
		return executionRoute{}, errExecutionRouteStale
	}
	if err := client.routes.Put(route); err != nil {
		return executionRoute{}, err
	}
	return route, nil
}

func (client *executionDataPlaneClient) connection(endpoint string) (*grpc.ClientConn, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return nil, errExecutionClientClosed
	}
	if connection := client.connections[endpoint]; connection != nil {
		return connection, nil
	}
	connection, err := grpc.NewClient(endpoint, client.options...)
	if err != nil {
		return nil, fmt.Errorf("create execution data-plane connection: %w", err)
	}
	client.connections[endpoint] = connection
	return connection, nil
}

func (client *executionDataPlaneClient) invalidateOnRouteFailure(route executionRoute, err error) {
	if executionRouteFailure(err) {
		client.routes.Invalidate(route.SlotID, route.Generation, route.Epoch)
	}
}

func executionRouteFailure(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.NotFound, codes.FailedPrecondition, codes.Aborted:
		return true
	default:
		return false
	}
}

type executionResponseStream struct {
	stream executionv1.ExecutionDataPlaneService_ExecuteClient
	client *executionDataPlaneClient
	route  executionRoute
	sendMu sync.Mutex
}

func (stream *executionResponseStream) Context() context.Context {
	return stream.stream.Context()
}

func (stream *executionResponseStream) Recv() (*executionv1.ExecuteResponse, error) {
	response, err := stream.stream.Recv()
	if err != nil && !errors.Is(err, io.EOF) {
		stream.client.invalidateOnRouteFailure(stream.route, err)
	}
	return response, err
}

func (stream *executionResponseStream) SendToolResult(toolUseID string, content []byte, isError bool) error {
	if strings.TrimSpace(toolUseID) == "" || len(content) == 0 || len(content) > maxExecutionRequestBytes {
		return errors.New("tool result is invalid")
	}
	stream.sendMu.Lock()
	defer stream.sendMu.Unlock()
	return stream.stream.Send(&executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_ToolResult{ToolResult: &executionv1.ToolResult{
		ToolUseId: toolUseID, ContentJson: append([]byte(nil), content...), IsError: isError,
	}}})
}

func (stream *executionResponseStream) Cancel(reason string) error {
	stream.sendMu.Lock()
	defer stream.sendMu.Unlock()
	if err := stream.stream.Send(&executionv1.ExecuteRequest{Event: &executionv1.ExecuteRequest_Cancel{Cancel: &executionv1.CancelExecution{Reason: safeCancelReason(reason)}}}); err != nil {
		return err
	}
	return stream.stream.CloseSend()
}

func (stream *executionResponseStream) CloseSend() error {
	stream.sendMu.Lock()
	defer stream.sendMu.Unlock()
	return stream.stream.CloseSend()
}

func safeCancelReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "client_cancelled"
	}
	if len(reason) > 64 || runtimeSecretString(reason) {
		return "client_cancelled"
	}
	return reason
}

func sanitizeExecutionHeaders(input map[string]string) (map[string]string, error) {
	allowed := map[string]bool{
		"accept": true, "anthropic-beta": true, "anthropic-version": true,
		"content-type": true, "user-agent": true, "x-request-id": true,
	}
	result := make(map[string]string, len(input))
	total := 0
	for rawName, value := range input {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if !allowed[name] {
			return nil, fmt.Errorf("execution header %q is not allowed", name)
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("execution header value is invalid")
		}
		if _, duplicated := result[name]; duplicated {
			return nil, fmt.Errorf("execution header %q is duplicated", name)
		}
		total += len(name) + len(value)
		if total > maxExecutionHeaderBytes {
			return nil, errors.New("execution headers are too large")
		}
		result[name] = value
	}
	return result, nil
}
