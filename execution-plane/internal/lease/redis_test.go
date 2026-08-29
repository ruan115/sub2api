package lease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisBackendMapsAtomicScriptOutcomes(t *testing.T) {
	client := &queuedRedisCommander{results: []redisResult{{value: int64(1)}, {value: int64(0)}, {value: int64(0)}, {value: int64(1)}}}
	backend, err := NewRedisBackend(client, "execution:test:lease:")
	if err != nil {
		t.Fatal(err)
	}
	claim := testClaim(7, "node-a")
	if err := backend.Acquire(context.Background(), claim, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := backend.Acquire(context.Background(), claim, 45*time.Second); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("held Redis lease error = %v", err)
	}
	if err := backend.Renew(context.Background(), claim, 45*time.Second); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("stale Redis renewal error = %v", err)
	}
	if err := backend.Revoke(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if len(client.calls) != 4 || client.calls[0].script != acquireScript || client.calls[2].script != renewScript || client.calls[3].script != revokeScript {
		t.Fatalf("Redis scripts = %+v", client.calls)
	}
	if strings.Contains(client.calls[0].keys[0], claim.SlotID) || client.calls[0].args[1] != int64(45_000) {
		t.Fatalf("Redis key/TTL = %q / %#v", client.calls[0].keys[0], client.calls[0].args)
	}
}

func TestRedisBackendTreatsClientErrorAsBackendUnavailable(t *testing.T) {
	client := &queuedRedisCommander{results: []redisResult{{err: errors.New("connection refused")}}}
	backend, err := NewRedisBackend(client, "execution:test:lease:")
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Validate(context.Background(), testClaim(1, "node-a")); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("Redis client failure error = %v", err)
	}
}

type redisResult struct {
	value any
	err   error
}

type redisCall struct {
	script string
	keys   []string
	args   []any
}

type queuedRedisCommander struct {
	results []redisResult
	calls   []redisCall
}

func (c *queuedRedisCommander) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	c.calls = append(c.calls, redisCall{script: script, keys: append([]string(nil), keys...), args: append([]any(nil), args...)})
	command := redis.NewCmd(ctx)
	result := c.results[0]
	c.results = c.results[1:]
	if result.err != nil {
		command.SetErr(result.err)
	} else {
		command.SetVal(result.value)
	}
	return command
}
