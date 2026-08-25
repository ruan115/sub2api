package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const redisRuntimePrefix = "ccmax:runtime:v1:"

var redisSlidingRPMScript = redis.NewScript(`
local now = redis.call('TIME')
local now_ms = (tonumber(now[1]) * 1000) + math.floor(tonumber(now[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, now_ms - 60000)
local current = redis.call('ZCARD', KEYS[1])
if current >= tonumber(ARGV[1]) then
  redis.call('PEXPIRE', KEYS[1], 61000)
  return 0
end
redis.call('ZADD', KEYS[1], now_ms, ARGV[2])
redis.call('PEXPIRE', KEYS[1], 61000)
return 1
`)

var redisUnlockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

type redisRuntime struct {
	client *redis.Client
}

func newRedisRuntime() (*redisRuntime, error) {
	address := strings.TrimSpace(os.Getenv("CCMAX_REDIS_ADDR"))
	if address == "" {
		return nil, nil
	}
	databaseNumber := 0
	if text := strings.TrimSpace(os.Getenv("CCMAX_REDIS_DB")); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 0 {
			return nil, fmt.Errorf("invalid CCMAX_REDIS_DB")
		}
		databaseNumber = parsed
	}
	client := redis.NewClient(&redis.Options{
		Addr:         address,
		Password:     os.Getenv("CCMAX_REDIS_PASSWORD"),
		DB:           databaseNumber,
		PoolSize:     envInt("CCMAX_REDIS_POOL_SIZE", 100),
		MinIdleConns: envInt("CCMAX_REDIS_MIN_IDLE_CONNS", 10),
		DialTimeout:  3 * time.Second,
		ReadTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
		MaxRetries:   2,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("ping Redis runtime store: %w", err)
	}
	return &redisRuntime{client: client}, nil
}

func (runtime *redisRuntime) Close() error {
	if runtime == nil || runtime.client == nil {
		return nil
	}
	return runtime.client.Close()
}

func (runtime *redisRuntime) Ping(ctx context.Context) error {
	if runtime == nil || runtime.client == nil {
		return nil
	}
	return runtime.client.Ping(ctx).Err()
}

func (runtime *redisRuntime) allowUserRPM(ctx context.Context, userID int64, limit int) (bool, error) {
	if runtime == nil || runtime.client == nil || limit <= 0 {
		return true, nil
	}
	member, err := secureHex(16)
	if err != nil {
		return false, err
	}
	key := redisRuntimePrefix + "user-rpm:" + strconv.FormatInt(userID, 10)
	allowed, err := redisSlidingRPMScript.Run(ctx, runtime.client, []string{key}, limit, member).Int()
	if err != nil {
		return false, err
	}
	return allowed == 1, nil
}

func (runtime *redisRuntime) acquireDispatchLock(ctx context.Context, groupID string) (func(), error) {
	if runtime == nil || runtime.client == nil {
		return func() {}, nil
	}
	owner, err := secureHex(16)
	if err != nil {
		return nil, err
	}
	key := redisRuntimePrefix + "dispatch-lock:" + strings.ToLower(strings.TrimSpace(groupID))
	deadline := time.Now().Add(3 * time.Second)
	for {
		acquired, err := runtime.client.SetNX(ctx, key, owner, 30*time.Second).Result()
		if err != nil {
			return nil, fmt.Errorf("acquire Redis dispatch lock: %w", err)
		}
		if acquired {
			return func() {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = redisUnlockScript.Run(releaseCtx, runtime.client, []string{key}, owner).Err()
			}, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("timed out waiting for Redis dispatch lock")
		}
		timer := time.NewTimer(15 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
