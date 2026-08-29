package lease

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestRedisBackendIntegration(t *testing.T) {
	serverURL := os.Getenv("EXECUTION_REDIS_TEST_URL")
	if serverURL == "" {
		t.Skip("EXECUTION_REDIS_TEST_URL is not set")
	}
	options, err := redis.ParseURL(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	options.MaxRetries = 0
	client := redis.NewClient(options)
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("execution:integration:%d:", time.Now().UnixNano())
	backend, err := NewRedisBackend(client, prefix)
	if err != nil {
		t.Fatal(err)
	}
	first := Claim{SlotID: "slot-redis", NodeID: "node-a", ExecutionEpoch: 1, OwnerID: "agent-a"}
	second := Claim{SlotID: "slot-redis", NodeID: "node-b", ExecutionEpoch: 2, OwnerID: "agent-b"}
	if err := backend.Acquire(ctx, first, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := backend.Acquire(ctx, second, 100*time.Millisecond); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("concurrent epoch error = %v", err)
	}
	if err := backend.Renew(ctx, first, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if err := backend.Acquire(ctx, second, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := backend.Validate(ctx, first); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("expired old epoch error = %v", err)
	}
	if err := backend.Validate(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := backend.Revoke(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := backend.Validate(ctx, second); !errors.Is(err, ErrLeaseNotCurrent) {
		t.Fatalf("revoked epoch error = %v", err)
	}
	failureClaim := Claim{SlotID: "slot-redis-failure", NodeID: "node-a", ExecutionEpoch: 1, OwnerID: "agent-a"}
	if err := backend.Acquire(ctx, failureClaim, time.Second); err != nil {
		t.Fatal(err)
	}
	fencer, err := NewFencer(backend)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	if _, err := fencer.Admit(ctx, failureClaim, func() { closed = true }); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := fencer.Revalidate(ctx); !errors.Is(err, ErrBackendUnavailable) {
		t.Fatalf("closed Redis failure error = %v", err)
	}
	if !closed {
		t.Fatal("protected egress remained open after Redis failure")
	}
}
