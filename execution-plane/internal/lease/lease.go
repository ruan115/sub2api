package lease

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrBackendUnavailable = errors.New("execution lease backend unavailable")
	ErrLeaseNotCurrent    = errors.New("execution lease is not current")
	ErrLeaseHeld          = errors.New("execution lease is held by another owner")
)

type Claim struct {
	SlotID         string
	NodeID         string
	ExecutionEpoch uint64
	OwnerID        string
}

func (c Claim) Validate() error {
	if c.SlotID == "" || len(c.SlotID) > 128 || c.NodeID == "" || len(c.NodeID) > 128 ||
		c.ExecutionEpoch == 0 || c.OwnerID == "" || len(c.OwnerID) > 128 {
		return errors.New("execution lease claim is invalid")
	}
	return nil
}

type Backend interface {
	Acquire(ctx context.Context, claim Claim, ttl time.Duration) error
	Renew(ctx context.Context, claim Claim, ttl time.Duration) error
	Validate(ctx context.Context, claim Claim) error
	Revoke(ctx context.Context, claim Claim) error
}

type MemoryBackend struct {
	mu        sync.Mutex
	now       func() time.Time
	available bool
	leases    map[string]memoryLease
}

type memoryLease struct {
	claim     Claim
	expiresAt time.Time
}

func NewMemoryBackend(now func() time.Time) *MemoryBackend {
	if now == nil {
		now = time.Now
	}
	return &MemoryBackend{now: now, available: true, leases: make(map[string]memoryLease)}
}

func (b *MemoryBackend) SetAvailable(available bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.available = available
}

func (b *MemoryBackend) Acquire(_ context.Context, claim Claim, ttl time.Duration) error {
	if err := validateOperation(claim, ttl); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.available {
		return ErrBackendUnavailable
	}
	b.expire(claim.SlotID)
	if current, exists := b.leases[claim.SlotID]; exists && current.claim != claim {
		return ErrLeaseHeld
	}
	b.leases[claim.SlotID] = memoryLease{claim: claim, expiresAt: b.now().UTC().Add(ttl)}
	return nil
}

func (b *MemoryBackend) Renew(_ context.Context, claim Claim, ttl time.Duration) error {
	if err := validateOperation(claim, ttl); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.available {
		return ErrBackendUnavailable
	}
	b.expire(claim.SlotID)
	current, exists := b.leases[claim.SlotID]
	if !exists || current.claim != claim {
		return ErrLeaseNotCurrent
	}
	current.expiresAt = b.now().UTC().Add(ttl)
	b.leases[claim.SlotID] = current
	return nil
}

func (b *MemoryBackend) Validate(_ context.Context, claim Claim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.available {
		return ErrBackendUnavailable
	}
	b.expire(claim.SlotID)
	current, exists := b.leases[claim.SlotID]
	if !exists || current.claim != claim {
		return ErrLeaseNotCurrent
	}
	return nil
}

func (b *MemoryBackend) Revoke(_ context.Context, claim Claim) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.available {
		return ErrBackendUnavailable
	}
	b.expire(claim.SlotID)
	current, exists := b.leases[claim.SlotID]
	if !exists {
		return nil
	}
	if current.claim != claim {
		return ErrLeaseNotCurrent
	}
	delete(b.leases, claim.SlotID)
	return nil
}

func (b *MemoryBackend) expire(slotID string) {
	if current, exists := b.leases[slotID]; exists && !current.expiresAt.After(b.now().UTC()) {
		delete(b.leases, slotID)
	}
}

func validateOperation(claim Claim, ttl time.Duration) error {
	if err := claim.Validate(); err != nil {
		return err
	}
	if ttl <= 0 {
		return errors.New("execution lease TTL must be positive")
	}
	return nil
}

type Timing struct {
	RenewEvery    time.Duration
	OfflineAfter  time.Duration
	FailoverAfter time.Duration
}

func DefaultTiming() Timing {
	return Timing{RenewEvery: 15 * time.Second, OfflineAfter: 45 * time.Second, FailoverAfter: 90 * time.Second}
}

func (t Timing) Validate() error {
	if t.RenewEvery <= 0 || t.OfflineAfter < 3*t.RenewEvery || t.FailoverAfter < t.OfflineAfter+t.RenewEvery {
		return errors.New("execution lease timing is invalid")
	}
	return nil
}

type Availability struct {
	CanRouteNew bool
	CanFailover bool
	Age         time.Duration
}

func EvaluateAvailability(lastRenewedAt, now time.Time, timing Timing) (Availability, error) {
	if lastRenewedAt.IsZero() || now.IsZero() || now.Before(lastRenewedAt) {
		return Availability{}, errors.New("execution lease renewal times are invalid")
	}
	if err := timing.Validate(); err != nil {
		return Availability{}, err
	}
	age := now.Sub(lastRenewedAt)
	return Availability{CanRouteNew: age < timing.OfflineAfter, CanFailover: age >= timing.FailoverAfter, Age: age}, nil
}

type Renewer struct {
	backend Backend
	claim   Claim
	timing  Timing
	onLost  func(error)
}

func NewRenewer(backend Backend, claim Claim, timing Timing, onLost func(error)) (*Renewer, error) {
	if backend == nil || claim.Validate() != nil || timing.Validate() != nil {
		return nil, errors.New("execution lease renewer configuration is invalid")
	}
	if onLost == nil {
		onLost = func(error) {}
	}
	return &Renewer{backend: backend, claim: claim, timing: timing, onLost: onLost}, nil
}

func (r *Renewer) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.timing.RenewEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := r.backend.Renew(ctx, r.claim, r.timing.OfflineAfter); err != nil {
				r.onLost(err)
				return fmt.Errorf("renew execution lease: %w", err)
			}
		}
	}
}

type Fencer struct {
	backend Backend

	mu          sync.Mutex
	nextID      uint64
	connections map[uint64]fencedConnection
}

type fencedConnection struct {
	claim Claim
	close func()
}

func NewFencer(backend Backend) (*Fencer, error) {
	if backend == nil {
		return nil, errors.New("execution lease backend is required")
	}
	return &Fencer{backend: backend, connections: make(map[uint64]fencedConnection)}, nil
}

// Admit validates the current epoch before opening protected egress. The
// returned release function must be called when the connection closes.
func (f *Fencer) Admit(ctx context.Context, claim Claim, closeConnection func()) (func(), error) {
	if closeConnection == nil {
		return nil, errors.New("protected connection close callback is required")
	}
	if err := f.backend.Validate(ctx, claim); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	f.connections[id] = fencedConnection{claim: claim, close: closeConnection}
	f.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.connections, id)
			f.mu.Unlock()
		})
	}, nil
}

// Revalidate closes every connection whose claim is no longer current. A
// backend failure deliberately fences all protected egress (fail closed).
func (f *Fencer) Revalidate(ctx context.Context) error {
	f.mu.Lock()
	snapshot := make(map[uint64]fencedConnection, len(f.connections))
	for id, connection := range f.connections {
		snapshot[id] = connection
	}
	f.mu.Unlock()
	var firstErr error
	for id, connection := range snapshot {
		if err := f.backend.Validate(ctx, connection.claim); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			f.mu.Lock()
			current, exists := f.connections[id]
			if exists {
				delete(f.connections, id)
			}
			f.mu.Unlock()
			if exists {
				current.close()
			}
		}
	}
	return firstErr
}

var _ Backend = (*MemoryBackend)(nil)
