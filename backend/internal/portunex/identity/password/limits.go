// Package password provides a bounded, local Argon2id PHC verification capability.
// It does not establish or implement the legacy Portunex authentication contract.
package password

import "errors"

var (
	ErrLimits         = errors.New("invalid password verification limits")
	ErrInvalidContext = errors.New("password verification context is required")
	ErrInvalidHash    = errors.New("invalid or unsupported password hash")
	ErrHashPolicy     = errors.New("password hash exceeds verification policy")
	ErrPasswordPolicy = errors.New("password exceeds verification policy")
	ErrPassword       = errors.New("password does not match")
	ErrBusy           = errors.New("password verification capacity reached")
)

// Limits is an explicit local resource policy, not a recovered set of legacy
// defaults. Every field is required. The caller must choose limits appropriate
// for its host; MaxConcurrent * MaxMemoryKiB bounds active KDF memory, excluding
// runtime overhead and allocations awaiting garbage collection.
type Limits struct {
	MaxPHCBytes      int
	MaxPasswordBytes int
	MinSaltBytes     int
	MaxSaltBytes     int
	MinHashBytes     int
	MaxHashBytes     int
	MaxMemoryKiB     uint32
	MaxTime          uint32
	MaxParallelism   uint8
	MaxWorkKiB       uint64 // Maximum declared memory KiB multiplied by passes.
	MaxConcurrent    int
}

func (l Limits) validate() error {
	if l.MaxPHCBytes <= 0 || l.MaxPasswordBytes <= 0 ||
		l.MinSaltBytes < 8 || l.MaxSaltBytes < l.MinSaltBytes ||
		l.MinHashBytes < 4 || l.MaxHashBytes < l.MinHashBytes ||
		l.MaxSaltBytes > l.MaxPHCBytes || l.MaxHashBytes > l.MaxPHCBytes ||
		uint64(l.MaxHashBytes) > uint64(^uint32(0)) ||
		l.MaxMemoryKiB < 8 || l.MaxTime == 0 || l.MaxParallelism == 0 ||
		l.MaxWorkKiB < 8 || l.MaxConcurrent <= 0 {
		return ErrLimits
	}
	return nil
}
