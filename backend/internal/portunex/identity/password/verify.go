package password

import (
	"context"
	"crypto/subtle"

	"golang.org/x/crypto/argon2"
	"golang.org/x/sync/semaphore"
)

type deriveFunc func(password, salt []byte, time, memory uint32, threads uint8, keyLen uint32) []byte

// Verifier is safe for concurrent use. It has no default policy, persistence,
// login route, token generation or password-upgrade side effects.
type Verifier struct {
	limits Limits
	slots  *semaphore.Weighted
	derive deriveFunc
}

func NewVerifier(limits Limits) (*Verifier, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	return &Verifier{limits: limits, slots: semaphore.NewWeighted(int64(limits.MaxConcurrent)), derive: argon2.IDKey}, nil
}

// Verify checks a PHC against an unmodified password. Empty passwords are not
// silently rewritten or prohibited here; any login policy belongs in a separate
// transport/service. Errors never include either input.
//
// x/crypto's KDF cannot be interrupted by context cancellation. Work runs
// synchronously, retains its capacity slot until completion, and cannot report
// success after observing cancellation. Callers must still check context before
// performing subsequent side effects such as creating a session.
func (v *Verifier) Verify(ctx context.Context, password, encoded string) error {
	if ctx == nil {
		return ErrInvalidContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(password) > v.limits.MaxPasswordBytes {
		return ErrPasswordPolicy
	}
	params, err := parse(encoded, v.limits)
	if err != nil {
		return err
	}
	if !v.slots.TryAcquire(1) {
		return ErrBusy
	}
	defer v.slots.Release(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	passwordBytes := []byte(password)
	defer clear(passwordBytes)
	derived := v.derive(passwordBytes, params.salt, params.time, params.memory, params.parallelism, uint32(len(params.hash)))
	defer clear(derived)
	if err := ctx.Err(); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(derived, params.hash) != 1 {
		return ErrPassword
	}
	return nil
}
