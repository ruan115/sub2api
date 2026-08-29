package lease

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const acquireScript = `
local current = redis.call('GET', KEYS[1])
if not current then
  redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
  return 1
end
if current == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0`

const renewScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0`

const validateScript = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return 1
end
return 0`

const revokeScript = `
local current = redis.call('GET', KEYS[1])
if not current then
  return 1
end
if current ~= ARGV[1] then
  return 0
end
redis.call('DEL', KEYS[1])
return 1`

type RedisCommander interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *redis.Cmd
}

type RedisBackend struct {
	client    RedisCommander
	keyPrefix string
}

func NewRedisBackend(client RedisCommander, keyPrefix string) (*RedisBackend, error) {
	if client == nil || strings.TrimSpace(keyPrefix) == "" || len(keyPrefix) > 128 {
		return nil, errors.New("Redis execution lease client and key prefix are required")
	}
	return &RedisBackend{client: client, keyPrefix: keyPrefix}, nil
}

func (b *RedisBackend) Acquire(ctx context.Context, claim Claim, ttl time.Duration) error {
	key, value, milliseconds, err := b.operationInput(claim, ttl)
	if err != nil {
		return err
	}
	accepted, err := b.evalBoolean(ctx, acquireScript, []string{key}, value, milliseconds)
	if err != nil {
		return err
	}
	if !accepted {
		return ErrLeaseHeld
	}
	return nil
}

func (b *RedisBackend) Renew(ctx context.Context, claim Claim, ttl time.Duration) error {
	key, value, milliseconds, err := b.operationInput(claim, ttl)
	if err != nil {
		return err
	}
	renewed, err := b.evalBoolean(ctx, renewScript, []string{key}, value, milliseconds)
	if err != nil {
		return err
	}
	if !renewed {
		return ErrLeaseNotCurrent
	}
	return nil
}

func (b *RedisBackend) Validate(ctx context.Context, claim Claim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	valid, err := b.evalBoolean(ctx, validateScript, []string{b.key(claim.SlotID)}, encodeClaim(claim))
	if err != nil {
		return err
	}
	if !valid {
		return ErrLeaseNotCurrent
	}
	return nil
}

func (b *RedisBackend) Revoke(ctx context.Context, claim Claim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	revoked, err := b.evalBoolean(ctx, revokeScript, []string{b.key(claim.SlotID)}, encodeClaim(claim))
	if err != nil {
		return err
	}
	if !revoked {
		return ErrLeaseNotCurrent
	}
	return nil
}

func (b *RedisBackend) operationInput(claim Claim, ttl time.Duration) (string, string, int64, error) {
	if err := validateOperation(claim, ttl); err != nil {
		return "", "", 0, err
	}
	milliseconds := ttl.Milliseconds()
	if milliseconds <= 0 {
		return "", "", 0, errors.New("Redis execution lease TTL must be at least one millisecond")
	}
	return b.key(claim.SlotID), encodeClaim(claim), milliseconds, nil
}

func (b *RedisBackend) evalBoolean(ctx context.Context, script string, keys []string, args ...any) (bool, error) {
	value, err := b.client.Eval(ctx, script, keys, args...).Int64()
	if err != nil {
		return false, errors.Join(ErrBackendUnavailable, err)
	}
	return value == 1, nil
}

func (b *RedisBackend) key(slotID string) string {
	digest := sha256.Sum256([]byte(slotID))
	return b.keyPrefix + hex.EncodeToString(digest[:16])
}

func encodeClaim(claim Claim) string {
	encoded, err := json.Marshal(struct {
		NodeID         string `json:"node_id"`
		ExecutionEpoch uint64 `json:"execution_epoch"`
		OwnerID        string `json:"owner_id"`
	}{NodeID: claim.NodeID, ExecutionEpoch: claim.ExecutionEpoch, OwnerID: claim.OwnerID})
	if err != nil {
		panic(fmt.Sprintf("encode validated execution lease claim: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

var _ Backend = (*RedisBackend)(nil)
