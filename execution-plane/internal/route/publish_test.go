package route

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestPublisherRejectsPublicEndpointAndStaleEpoch(t *testing.T) {
	store := newMemoryRouteStore()
	publisher, err := NewPublisher(store, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPublisher(store, 2*time.Minute); err == nil {
		t.Fatal("expected TTL upper bound")
	}
	if err := publisher.Publish(context.Background(), Route{
		SlotID: "slot-1", NodeID: "srv74", Endpoint: "8.8.8.8:8091", Epoch: 1, Generation: 1,
	}); err != ErrRouteInvalid {
		t.Fatalf("public endpoint error = %v", err)
	}
	route := Route{SlotID: "slot-1", NodeID: "srv74", Endpoint: "10.8.0.12:8091", Epoch: 2, Generation: 4}
	if err := publisher.Publish(context.Background(), route); err != nil {
		t.Fatal(err)
	}
	got, err := publisher.Get(context.Background(), "slot-1")
	if err != nil || got != route {
		t.Fatalf("published route = %+v err=%v", got, err)
	}
	if err := publisher.Publish(context.Background(), Route{
		SlotID: "slot-1", NodeID: "srv74", Endpoint: "10.8.0.12:8091", Epoch: 1, Generation: 4,
	}); err != ErrRouteStale {
		t.Fatalf("stale epoch error = %v", err)
	}
	if err := publisher.Publish(context.Background(), Route{
		SlotID: "slot-1", NodeID: "srv99", Endpoint: "10.8.0.12:8091", Epoch: 2, Generation: 4,
	}); err != ErrRouteConflict {
		t.Fatalf("same-epoch node conflict error = %v", err)
	}
	if err := publisher.Unpublish(context.Background(), "slot-1", 1, 4); err != ErrRouteStale {
		t.Fatalf("unpublish stale error = %v", err)
	}
	if err := publisher.Unpublish(context.Background(), "slot-1", 2, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Get(context.Background(), "slot-1"); err != ErrRouteMissing {
		t.Fatalf("unpublished get = %v", err)
	}
}

func TestEndpointFromLabelsRequiresPrivateAddress(t *testing.T) {
	endpoint, err := EndpointFromLabels(map[string]string{DataplaneEndpointKey: "127.0.0.1:8091"})
	if err != nil || endpoint != "127.0.0.1:8091" {
		t.Fatalf("loopback endpoint = %q err=%v", endpoint, err)
	}
	if _, err := EndpointFromLabels(map[string]string{DataplaneEndpointKey: "example.com:8091"}); err != ErrRouteInvalid {
		t.Fatalf("hostname endpoint error = %v", err)
	}
}

type memoryRouteStore struct {
	mu     sync.RWMutex
	hashes map[string]map[string]string
}

func newMemoryRouteStore() *memoryRouteStore {
	return &memoryRouteStore{hashes: map[string]map[string]string{}}
}

func (s *memoryRouteStore) HGetAll(_ context.Context, key string) *redis.MapStringStringCmd {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cmd := redis.NewMapStringStringCmd(context.Background(), "hgetall", key)
	if values := s.hashes[key]; len(values) > 0 {
		cmd.SetVal(cloneStringMap(values))
	} else {
		cmd.SetVal(map[string]string{})
	}
	return cmd
}

func (s *memoryRouteStore) Eval(_ context.Context, script string, keys []string, args ...any) *redis.Cmd {
	cmd := redis.NewCmd(context.Background(), "eval")
	if len(keys) != 1 {
		cmd.SetErr(ErrRouteInvalid)
		return cmd
	}
	key := keys[0]
	if script == publishScript {
		cmd.SetVal(s.publish(key, stringifyArgs(args)))
		return cmd
	}
	if script == unpublishScript {
		cmd.SetVal(s.unpublish(key, stringifyArgs(args)))
		return cmd
	}
	cmd.SetErr(ErrRouteInvalid)
	return cmd
}

func (s *memoryRouteStore) publish(key string, args []string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(args) != 6 {
		return -2
	}
	current := s.hashes[key]
	if current != nil {
		epoch, _ := strconv.ParseUint(current["execution_epoch"], 10, 64)
		generation, _ := strconv.ParseUint(current["route_generation"], 10, 64)
		nextEpoch, _ := strconv.ParseUint(args[3], 10, 64)
		nextGeneration, _ := strconv.ParseUint(args[4], 10, 64)
		if nextGeneration < generation || nextEpoch < epoch {
			return 0
		}
		if nextGeneration == generation && nextEpoch == epoch &&
			(current["node_id"] != args[1] || current["endpoint"] != args[2]) {
			return -1
		}
	}
	s.hashes[key] = map[string]string{
		"slot_id": args[0], "node_id": args[1], "endpoint": args[2],
		"execution_epoch": args[3], "route_generation": args[4],
	}
	return 1
}

func (s *memoryRouteStore) unpublish(key string, args []string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.hashes[key]
	if current == nil {
		return 1
	}
	if current["execution_epoch"] != args[0] || current["route_generation"] != args[1] {
		return 0
	}
	delete(s.hashes, key)
	return 1
}

func stringifyArgs(args []any) []string {
	values := make([]string, 0, len(args))
	for _, arg := range args {
		values = append(values, arg.(string))
	}
	return values
}

func cloneStringMap(input map[string]string) map[string]string {
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}
