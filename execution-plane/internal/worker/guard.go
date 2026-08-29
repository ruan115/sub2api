package worker

import (
	"crypto/subtle"
	"errors"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
)

var (
	ErrIdentityMismatch = errors.New("execution ticket identity does not match worker slot")
	ErrScopeDenied      = errors.New("execution ticket scope denied")
	ErrReplay           = errors.New("execution ticket has already been used")
)

type Identity struct {
	AccountID string
	SlotID    string
	NodeID    string
	Epoch     uint64
}

func (i Identity) Validate() error {
	if i.AccountID == "" || i.SlotID == "" || i.NodeID == "" || i.Epoch == 0 {
		return errors.New("worker identity is incomplete")
	}
	return nil
}

// Guard verifies orchestrator-issued execution tickets and consumes each nonce
// once. It owns only an Ed25519 public key, so a compromised worker cannot mint
// tickets for itself or another slot.
type Guard struct {
	verifier *ticket.Verifier
	identity Identity
	now      func() time.Time

	mu        sync.Mutex
	usedNonce map[string]int64
}

func NewGuard(verifier *ticket.Verifier, identity Identity, now func() time.Time) (*Guard, error) {
	if verifier == nil {
		return nil, errors.New("ticket verifier is required")
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Guard{
		verifier:  verifier,
		identity:  identity,
		now:       now,
		usedNonce: make(map[string]int64),
	}, nil
}

func (g *Guard) Authorize(rawTicket, requiredScope string) (ticket.Claims, error) {
	now := g.now().UTC()
	claims, err := g.verifier.Verify(rawTicket, now)
	if err != nil {
		return ticket.Claims{}, err
	}
	if !sameString(claims.AccountID, g.identity.AccountID) ||
		!sameString(claims.SlotID, g.identity.SlotID) ||
		!sameString(claims.NodeID, g.identity.NodeID) ||
		claims.Epoch != g.identity.Epoch {
		return ticket.Claims{}, ErrIdentityMismatch
	}
	if requiredScope == "" || !claims.HasScope(requiredScope) {
		return ticket.Claims{}, ErrScopeDenied
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	for nonce, expiresAt := range g.usedNonce {
		if expiresAt <= now.Unix() {
			delete(g.usedNonce, nonce)
		}
	}
	if _, exists := g.usedNonce[claims.Nonce]; exists {
		return ticket.Claims{}, ErrReplay
	}
	g.usedNonce[claims.Nonce] = claims.ExpiresAt
	return claims, nil
}

func sameString(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
