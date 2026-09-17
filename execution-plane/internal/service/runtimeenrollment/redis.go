package runtimeenrollment

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type redisClient interface {
	Eval(context.Context, string, []string, ...any) *redis.Cmd
	Ping(context.Context) *redis.StatusCmd
	Close() error
}

func newRedisClient(address string) redisClient {
	return redis.NewClient(&redis.Options{
		Addr: address, Protocol: 2, DisableIdentity: true,
		MaxRetries: -1, ContextTimeoutEnabled: true,
		DialTimeout: 2 * time.Second, ReadTimeout: 2 * time.Second,
		WriteTimeout: 2 * time.Second, PoolTimeout: 2 * time.Second,
	})
}
