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

// redisReserveITPMScript keeps the estimate for every in-flight request in a
// hash and its crash-recovery deadline in a sorted set. Selection is serialized
// per group, but an account may belong to more than one group, so admission
// still has to be atomic per account.
var redisReserveITPMScript = redis.NewScript(`
local now = redis.call('TIME')
local now_ms = (tonumber(now[1]) * 1000) + math.floor(tonumber(now[2]) / 1000)
local expired = redis.call('ZRANGEBYSCORE', KEYS[1], 0, now_ms)
for _, lease_id in ipairs(expired) do
  redis.call('HDEL', KEYS[2], lease_id)
end
redis.call('ZREMRANGEBYSCORE', KEYS[1], 0, now_ms)

local reserved = 0
local exclusive = 0
for _, value in ipairs(redis.call('HVALS', KEYS[2])) do
  local separator = string.find(value, ':', 1, true)
  if separator then
    reserved = reserved + (tonumber(string.sub(value, 1, separator - 1)) or 0)
    exclusive = math.max(exclusive, tonumber(string.sub(value, separator + 1)) or 0)
  end
end

local estimate = tonumber(ARGV[2])
local current = tonumber(ARGV[3])
local soft_limit = tonumber(ARGV[4])
local hard_limit = tonumber(ARGV[5])
local sticky = tonumber(ARGV[6])
local requested_exclusive = tonumber(ARGV[7])
local inflight = tonumber(ARGV[8])
local small_limit = tonumber(ARGV[9])
local ttl_ms = tonumber(ARGV[10])

if exclusive > 0 then
  return -1
end
if requested_exclusive > 0 then
  if inflight > 0 or reserved > 0 then
    return -1
  end
else
  local projected = current + reserved + estimate
  if hard_limit > 0 and (current + reserved >= hard_limit or projected > hard_limit) then
    return -1
  end
  if soft_limit > 0 and projected > soft_limit and sticky == 0 and estimate > small_limit then
    return -1
  end
end

redis.call('HSET', KEYS[2], ARGV[1], tostring(estimate) .. ':' .. tostring(requested_exclusive))
redis.call('ZADD', KEYS[1], now_ms + ttl_ms, ARGV[1])
redis.call('PEXPIRE', KEYS[1], ttl_ms + 60000)
redis.call('PEXPIRE', KEYS[2], ttl_ms + 60000)
return reserved + estimate
`)

var redisReleaseITPMScript = redis.NewScript(`
redis.call('ZREM', KEYS[1], ARGV[1])
return redis.call('HDEL', KEYS[2], ARGV[1])
`)

var redisSettleITPMScript = redis.NewScript(`
if redis.call('HEXISTS', KEYS[2], ARGV[1]) == 0 then
  return 0
end
local now = redis.call('TIME')
local now_ms = (tonumber(now[1]) * 1000) + math.floor(tonumber(now[2]) / 1000)
redis.call('HSET', KEYS[2], ARGV[1], tostring(ARGV[2]) .. ':0')
redis.call('ZADD', KEYS[1], now_ms + tonumber(ARGV[3]), ARGV[1])
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[3]) + 60000)
redis.call('PEXPIRE', KEYS[2], tonumber(ARGV[3]) + 60000)
return 1
`)

var redisRenewITPMScript = redis.NewScript(`
if redis.call('HEXISTS', KEYS[2], ARGV[1]) == 0 then
  return 0
end
local now = redis.call('TIME')
local now_ms = (tonumber(now[1]) * 1000) + math.floor(tonumber(now[2]) / 1000)
redis.call('ZADD', KEYS[1], now_ms + tonumber(ARGV[2]), ARGV[1])
redis.call('PEXPIRE', KEYS[1], tonumber(ARGV[2]) + 60000)
redis.call('PEXPIRE', KEYS[2], tonumber(ARGV[2]) + 60000)
return 1
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

func (runtime *redisRuntime) reserveAccountITPM(ctx context.Context, accountID int64, leaseID string, estimated, current, softLimit, hardLimit int64, sticky, exclusive bool, inflight int, smallLimit int64, ttl time.Duration) (bool, int64, error) {
	if runtime == nil || runtime.client == nil || estimated <= 0 {
		return true, 0, nil
	}
	prefix := redisRuntimePrefix + "account-itpm:" + strconv.FormatInt(accountID, 10)
	result, err := redisReserveITPMScript.Run(ctx, runtime.client, []string{prefix + ":expiry", prefix + ":leases"},
		leaseID, estimated, current, softLimit, hardLimit, boolInt(sticky), boolInt(exclusive), inflight, smallLimit, ttl.Milliseconds()).Int64()
	if err != nil {
		return false, 0, fmt.Errorf("reserve account ITPM: %w", err)
	}
	if result < 0 {
		return false, 0, nil
	}
	return true, result, nil
}

func (runtime *redisRuntime) releaseAccountITPM(ctx context.Context, accountID int64, leaseID string) error {
	if runtime == nil || runtime.client == nil || accountID <= 0 || strings.TrimSpace(leaseID) == "" {
		return nil
	}
	prefix := redisRuntimePrefix + "account-itpm:" + strconv.FormatInt(accountID, 10)
	if err := redisReleaseITPMScript.Run(ctx, runtime.client, []string{prefix + ":expiry", prefix + ":leases"}, leaseID).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("release account ITPM: %w", err)
	}
	return nil
}

func (runtime *redisRuntime) settleAccountITPM(ctx context.Context, accountID int64, leaseID string, actual int64, ttl time.Duration) error {
	if runtime == nil || runtime.client == nil || accountID <= 0 || strings.TrimSpace(leaseID) == "" {
		return nil
	}
	prefix := redisRuntimePrefix + "account-itpm:" + strconv.FormatInt(accountID, 10)
	if err := redisSettleITPMScript.Run(ctx, runtime.client, []string{prefix + ":expiry", prefix + ":leases"}, leaseID, actual, ttl.Milliseconds()).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("settle account ITPM: %w", err)
	}
	return nil
}

func (runtime *redisRuntime) renewAccountITPM(ctx context.Context, accountID int64, leaseID string, ttl time.Duration) error {
	if runtime == nil || runtime.client == nil || accountID <= 0 || strings.TrimSpace(leaseID) == "" {
		return nil
	}
	prefix := redisRuntimePrefix + "account-itpm:" + strconv.FormatInt(accountID, 10)
	if err := redisRenewITPMScript.Run(ctx, runtime.client, []string{prefix + ":expiry", prefix + ":leases"}, leaseID, ttl.Milliseconds()).Err(); err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("renew account ITPM: %w", err)
	}
	return nil
}

func (runtime *redisRuntime) accountITPMReservationStatuses(ctx context.Context, accountIDs []int64) (map[int64]accountITPMReservationStatus, error) {
	result := map[int64]accountITPMReservationStatus{}
	if runtime == nil || runtime.client == nil || len(accountIDs) == 0 {
		return result, nil
	}
	type reservationCommands struct {
		expiry *redis.StringSliceCmd
		leases *redis.MapStringStringCmd
	}
	commands := map[int64]reservationCommands{}
	pipe := runtime.client.Pipeline()
	nowMilliseconds := time.Now().UnixMilli()
	for _, accountID := range accountIDs {
		if accountID <= 0 {
			continue
		}
		prefix := redisRuntimePrefix + "account-itpm:" + strconv.FormatInt(accountID, 10)
		commands[accountID] = reservationCommands{
			expiry: pipe.ZRangeByScore(ctx, prefix+":expiry", &redis.ZRangeBy{Min: strconv.FormatInt(nowMilliseconds, 10), Max: "+inf"}),
			leases: pipe.HGetAll(ctx, prefix+":leases"),
		}
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("read account ITPM reservations: %w", err)
	}
	for accountID, command := range commands {
		values := command.leases.Val()
		status := accountITPMReservationStatus{}
		for _, leaseID := range command.expiry.Val() {
			value, ok := values[leaseID]
			if !ok {
				continue
			}
			parts := strings.SplitN(value, ":", 2)
			tokens, _ := strconv.ParseInt(parts[0], 10, 64)
			status.Tokens += tokens
			if len(parts) == 2 && parts[1] == "1" {
				status.Exclusive = true
			}
		}
		if status.Tokens > 0 || status.Exclusive {
			result[accountID] = status
		}
	}
	return result, nil
}
