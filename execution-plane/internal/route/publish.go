package route

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	RedisPrefix          = "execution:route:v1:"
	DataplaneEndpointKey = "dataplane_endpoint"
	defaultTTL           = 45 * time.Second
	maxTTL               = time.Minute
)

var (
	ErrRouteInvalid  = errors.New("execution route is invalid")
	ErrRouteStale    = errors.New("execution route is stale")
	ErrRouteConflict = errors.New("execution route conflicts with the published epoch")
	ErrRouteMissing  = errors.New("execution route is not published")
)

type Route struct {
	SlotID     string
	NodeID     string
	Endpoint   string
	Epoch      uint64
	Generation uint64
}

type HashCommander interface {
	HGetAll(ctx context.Context, key string) *redis.MapStringStringCmd
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
}

type Publisher struct {
	client HashCommander
	ttl    time.Duration
}

func NewPublisher(client HashCommander, ttl time.Duration) (*Publisher, error) {
	if client == nil {
		return nil, errors.New("execution route Redis client is required")
	}
	if ttl <= 0 {
		ttl = defaultTTL
	}
	if ttl > maxTTL {
		return nil, errors.New("execution route TTL must be at most 1m")
	}
	return &Publisher{client: client, ttl: ttl}, nil
}

func (p *Publisher) Publish(ctx context.Context, route Route) error {
	if p == nil || p.client == nil || ctx == nil || ctx.Err() != nil {
		return ErrRouteInvalid
	}
	if err := validateRoute(route); err != nil {
		return err
	}
	milliseconds := strconv.FormatInt(p.ttl.Milliseconds(), 10)
	result, err := p.client.Eval(ctx, publishScript, []string{RedisPrefix + route.SlotID},
		route.SlotID, route.NodeID, route.Endpoint,
		strconv.FormatUint(route.Epoch, 10), strconv.FormatUint(route.Generation, 10), milliseconds,
	).Result()
	if err != nil {
		return fmt.Errorf("publish execution route: %w", err)
	}
	return classifyRouteScriptResult(result)
}

func (p *Publisher) Unpublish(ctx context.Context, slotID string, epoch, generation uint64) error {
	if p == nil || p.client == nil || ctx == nil || ctx.Err() != nil || strings.TrimSpace(slotID) == "" || epoch == 0 || generation == 0 {
		return ErrRouteInvalid
	}
	result, err := p.client.Eval(ctx, unpublishScript, []string{RedisPrefix + slotID},
		strconv.FormatUint(epoch, 10), strconv.FormatUint(generation, 10),
	).Result()
	if err != nil {
		return fmt.Errorf("unpublish execution route: %w", err)
	}
	return classifyRouteScriptResult(result)
}

func (p *Publisher) Get(ctx context.Context, slotID string) (Route, error) {
	if p == nil || p.client == nil || ctx == nil || ctx.Err() != nil || strings.TrimSpace(slotID) == "" {
		return Route{}, ErrRouteInvalid
	}
	values, err := p.client.HGetAll(ctx, RedisPrefix+slotID).Result()
	if err != nil {
		return Route{}, fmt.Errorf("read execution route: %w", err)
	}
	if len(values) == 0 {
		return Route{}, ErrRouteMissing
	}
	epoch, err := strconv.ParseUint(values["execution_epoch"], 10, 64)
	if err != nil {
		return Route{}, ErrRouteInvalid
	}
	generation, err := strconv.ParseUint(values["route_generation"], 10, 64)
	if err != nil {
		return Route{}, ErrRouteInvalid
	}
	route := Route{
		SlotID: values["slot_id"], NodeID: values["node_id"], Endpoint: values["endpoint"],
		Epoch: epoch, Generation: generation,
	}
	if route.SlotID != slotID {
		return Route{}, ErrRouteConflict
	}
	if err := validateRoute(route); err != nil {
		return Route{}, err
	}
	return route, nil
}

func EndpointFromLabels(labels map[string]string) (string, error) {
	if labels == nil {
		return "", ErrRouteInvalid
	}
	endpoint := strings.TrimSpace(labels[DataplaneEndpointKey])
	route := Route{
		SlotID: "slot-1", NodeID: "node-1", Endpoint: endpoint, Epoch: 1, Generation: 1,
	}
	if err := validateRoute(route); err != nil {
		return "", err
	}
	return endpoint, nil
}

func validateRoute(route Route) error {
	if !validRouteIdentity(route.SlotID) || !validRouteIdentity(route.NodeID) || route.Epoch == 0 || route.Generation == 0 {
		return ErrRouteInvalid
	}
	host, port, err := net.SplitHostPort(strings.TrimSpace(route.Endpoint))
	if err != nil || host == "" || port == "" {
		return ErrRouteInvalid
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return ErrRouteInvalid
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return ErrRouteInvalid
	}
	return nil
}

func validRouteIdentity(value string) bool {
	return len(value) > 0 && len(value) <= 128 && value == strings.TrimSpace(value)
}

func classifyRouteScriptResult(result any) error {
	code, ok := result.(int64)
	if !ok {
		return ErrRouteInvalid
	}
	switch code {
	case 1:
		return nil
	case 0:
		return ErrRouteStale
	case -1:
		return ErrRouteConflict
	default:
		return ErrRouteInvalid
	}
}

const publishScript = `
local current = redis.call('HGETALL', KEYS[1])
if #current > 0 then
  local epoch = 0
  local generation = 0
  local node = ''
  local endpoint = ''
  for i = 1, #current, 2 do
    if current[i] == 'execution_epoch' then epoch = tonumber(current[i + 1]) or 0 end
    if current[i] == 'route_generation' then generation = tonumber(current[i + 1]) or 0 end
    if current[i] == 'node_id' then node = current[i + 1] end
    if current[i] == 'endpoint' then endpoint = current[i + 1] end
  end
  local nextEpoch = tonumber(ARGV[4])
  local nextGeneration = tonumber(ARGV[5])
  if nextGeneration < generation or nextEpoch < epoch then
    return 0
  end
  if nextGeneration == generation and nextEpoch == epoch and (node ~= ARGV[2] or endpoint ~= ARGV[3]) then
    return -1
  end
end
redis.call('HSET', KEYS[1], 'slot_id', ARGV[1], 'node_id', ARGV[2], 'endpoint', ARGV[3], 'execution_epoch', ARGV[4], 'route_generation', ARGV[5])
redis.call('PEXPIRE', KEYS[1], ARGV[6])
return 1
`

const unpublishScript = `
local epoch = redis.call('HGET', KEYS[1], 'execution_epoch')
local generation = redis.call('HGET', KEYS[1], 'route_generation')
if not epoch then
  return 1
end
if epoch ~= ARGV[1] or generation ~= ARGV[2] then
  return 0
end
redis.call('DEL', KEYS[1])
return 1
`
