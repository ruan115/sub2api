package runtimeenrollment

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/storage"
	"github.com/redis/go-redis/v9"
)

type testRedis struct {
	ping       func(context.Context) *redis.StatusCmd
	eval       func(context.Context, string, []string, ...any) *redis.Cmd
	closes     int
	closeError error
}

func (r *testRedis) Ping(ctx context.Context) *redis.StatusCmd {
	if r.ping != nil {
		return r.ping(ctx)
	}
	return redis.NewStatusResult("PONG", nil)
}
func (r *testRedis) Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
	if r.eval != nil {
		return r.eval(ctx, script, keys, args...)
	}
	return redis.NewCmdResult(int64(0), nil)
}
func (r *testRedis) Close() error { r.closes++; return r.closeError }

func enrollmentDependenciesFixture(t *testing.T) (*sql.DB, *store.Repository, config.RuntimeEnrollmentConfig) {
	t.Helper()
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		database.Close()
	})
	repository, err := store.NewRepository(database)
	if err != nil {
		t.Fatal(err)
	}
	return database, repository, config.RuntimeEnrollmentConfig{Enabled: true, LeaseRedisAddr: "127.0.0.1:6380", Timeout: time.Second}
}

func TestDependenciesPersistentReadOnlyStartupAndLeaseNamespace(t *testing.T) {
	database, repository, c := enrollmentDependenciesFixture(t)
	client := &testRedis{}
	pings, evaluations := 0, 0
	client.ping = func(ctx context.Context) *redis.StatusCmd {
		pings++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > startupTimeout || time.Until(deadline) < 4*time.Second {
			t.Error("startup deadline not bounded")
		}
		return redis.NewStatusResult("PONG", nil)
	}
	claim := lease.Claim{SlotID: "slot-a", NodeID: "node-a", ExecutionEpoch: 3, OwnerID: "owner-a"}
	client.eval = func(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd {
		evaluations++
		hash := sha256.Sum256([]byte(claim.SlotID))
		if len(keys) != 1 || keys[0] != config.RuntimeLeaseKeyPrefix+hex.EncodeToString(hash[:16]) || len(args) != 1 {
			t.Error("incorrect lease namespace or claim")
		}
		if !strings.Contains(script, "'GET'") || strings.Contains(script, "'SET'") || strings.Contains(script, "'DEL'") || strings.Contains(script, "'PEXPIRE'") {
			t.Error("lease validator attempted mutation")
		}
		return redis.NewCmdResult(int64(0), nil)
	}
	d, err := newDependencies(context.Background(), c, database, repository, func(address string) redisClient {
		if address != c.LeaseRedisAddr {
			t.Error("incorrect independent address")
		}
		return client
	})
	if err != nil {
		t.Fatal(err)
	}
	if pings != 1 || evaluations != 0 || client.closes != 0 {
		t.Fatal("startup performed more than PING")
	}
	controlConfig := d.ControlConfig()
	if controlConfig.Bindings != repository || controlConfig.Timeout != c.Timeout {
		t.Fatal("wrong authority binding")
	}
	if _, ok := controlConfig.Receipts.(*storage.SQL); !ok {
		t.Fatal("receipt store is not SQL")
	}
	if _, ok := controlConfig.Leases.(lease.Backend); ok {
		t.Fatal("issuer can write execution leases")
	}
	if err := controlConfig.Leases.Validate(context.Background(), claim); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("empty Redis authorized lease: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			if d.Close() != nil {
				t.Error("close failed")
			}
		})
	}
	wg.Wait()
	if client.closes != 1 || d.ControlConfig() != nil {
		t.Fatal("close was not idempotent")
	}
	if err := controlConfig.Leases.Validate(context.Background(), claim); !errors.Is(err, lease.ErrBackendUnavailable) || evaluations != 1 {
		t.Fatal("closed dependencies admitted a lease")
	}
}

func TestDependenciesRejectBeforeClientCreation(t *testing.T) {
	for _, name := range []string{"nil context", "canceled", "off", "invalid", "nil database", "nil repository", "nil factory"} {
		t.Run(name, func(t *testing.T) {
			database, repository, c := enrollmentDependenciesFixture(t)
			ctx := context.Background()
			factory := func(string) redisClient { t.Fatal("client created on invalid configuration"); return nil }
			switch name {
			case "nil context":
				ctx = nil
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "off":
				c.Enabled = false
			case "invalid":
				c.LeaseRedisAddr = "redis://secret"
			case "nil database":
				database = nil
			case "nil repository":
				repository = nil
			case "nil factory":
				factory = nil
			}
			if d, err := newDependencies(ctx, c, database, repository, factory); d != nil || !errors.Is(err, ErrDependencies) {
				t.Fatalf("unsafe construction: %v", err)
			}
		})
	}
}

func TestDependenciesPingFailureAndCancellationCloseClient(t *testing.T) {
	for _, mode := range []string{"error", "cancel after successful ping", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			database, repository, c := enrollmentDependenciesFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 20*time.Millisecond)
				defer deadlineCancel()
			}
			client := &testRedis{ping: func(ctx context.Context) *redis.StatusCmd {
				if mode == "error" {
					return redis.NewStatusResult("", errors.New("private-backend-secret"))
				}
				if mode == "deadline" {
					<-ctx.Done()
				} else {
					cancel()
				}
				return redis.NewStatusResult("PONG", nil)
			}}
			d, err := newDependencies(ctx, c, database, repository, func(string) redisClient { return client })
			if d != nil || err != ErrDependencies || strings.Contains(err.Error(), "secret") || client.closes != 1 {
				t.Fatalf("failure did not fail closed: %v, closes %d", err, client.closes)
			}
		})
	}
}

func TestDependenciesLeaseTransportFailureAndPostCancellation(t *testing.T) {
	for _, cancelAfterEval := range []bool{false, true} {
		database, repository, c := enrollmentDependenciesFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		client := &testRedis{eval: func(context.Context, string, []string, ...any) *redis.Cmd {
			if cancelAfterEval {
				cancel()
				return redis.NewCmdResult(int64(1), nil)
			}
			return redis.NewCmdResult(nil, errors.New("private-redis-secret"))
		}}
		d, err := newDependencies(ctx, c, database, repository, func(string) redisClient { return client })
		if err != nil {
			t.Fatal(err)
		}
		err = d.ControlConfig().Leases.Validate(ctx, lease.Claim{SlotID: "slot-a", NodeID: "node-a", ExecutionEpoch: 1, OwnerID: "owner-a"})
		if err != lease.ErrBackendUnavailable || strings.Contains(err.Error(), "secret") {
			t.Fatalf("transport/cancellation accepted or leaked: %v", err)
		}
		cancel()
		d.Close()
	}
}
