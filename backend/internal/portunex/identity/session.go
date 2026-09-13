// Package identity implements synthetic-only demo identities and opaque sessions.
// It is not a production password verifier or a recovered Portunex auth contract.
package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/namespace"
)

const Admin = "admin"
const Member = "user"
const SessionTTL = 30 * time.Minute

var (
	ErrCredentials = errors.New("invalid synthetic credentials")
	ErrSession     = errors.New("session missing or expired")
	ErrCapacity    = errors.New("demo session capacity reached")
	ErrUnavailable = errors.New("demo authentication unavailable")
)

type Principal struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

type Session struct {
	User      Principal `json:"user"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type account struct {
	principal      Principal
	passwordDigest [32]byte
}

type Service struct {
	mu          sync.Mutex
	space       namespace.Space
	accounts    map[string]account
	sessions    map[string]Session
	now         func() time.Time
	entropy     io.Reader
	maxSessions int
}

// NewDemo owns a separate store; it never imports identities, passwords or
// sessions from Sub2API, CCMAX, a database, an environment variable or the network.
func NewDemo(space namespace.Space, now func() time.Time) (*Service, error) {
	if _, err := space.Key("sessions", "validation"); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	s := &Service{space: space, accounts: make(map[string]account), sessions: make(map[string]Session), now: now, entropy: rand.Reader, maxSessions: 128}
	for _, fixture := range []struct {
		user     Principal
		password string
	}{
		{Principal{"u-admin", "admin@example.invalid", "演示管理员", Admin}, "Demo-admin-2026!"},
		{Principal{"u-member", "member@example.invalid", "演示成员", Member}, "Demo-member-2026!"},
	} {
		// SHA-256 is only a constant-time comparison helper for public, synthetic
		// fixtures. This code must never store real user passwords.
		s.accounts[fixture.user.Email] = account{fixture.user, sha256.Sum256([]byte(fixture.password))}
	}
	return s, nil
}

func (s *Service) Login(email, password string) (string, Session, error) {
	return s.login(email, password, "")
}

// Replace verifies credentials first and replaces only a token already present
// in this instance. Capacity or entropy errors do not revoke the old session.
func (s *Service) Replace(email, password, previousToken string) (string, Session, error) {
	return s.login(email, password, previousToken)
}

func (s *Service) login(email, password, previousToken string) (string, Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, found := s.accounts[strings.ToLower(strings.TrimSpace(email))]
	digest := sha256.Sum256([]byte(password))
	valid := subtle.ConstantTimeCompare(digest[:], user.passwordDigest[:])
	if !found || valid != 1 {
		return "", Session{}, ErrCredentials
	}
	now := s.now()
	for key, session := range s.sessions {
		if !now.Before(session.ExpiresAt) {
			delete(s.sessions, key)
		}
	}
	previousKey, previousError := s.tokenKey(previousToken)
	_, canReplace := s.sessions[previousKey]
	canReplace = canReplace && previousError == nil
	if len(s.sessions) >= s.maxSessions && !canReplace {
		return "", Session{}, ErrCapacity
	}
	for range 4 {
		var random [32]byte
		if _, err := io.ReadFull(s.entropy, random[:]); err != nil {
			return "", Session{}, ErrUnavailable
		}
		token := hex.EncodeToString(random[:])
		key, err := s.tokenKey(token)
		if err != nil {
			return "", Session{}, ErrUnavailable
		}
		if _, exists := s.sessions[key]; exists {
			continue
		}
		session := Session{User: user.principal, ExpiresAt: now.Add(SessionTTL).UTC()}
		if canReplace {
			delete(s.sessions, previousKey)
		}
		s.sessions[key] = session
		return token, session, nil
	}
	return "", Session{}, ErrUnavailable
}

func (s *Service) Authenticate(token string) (Session, error) {
	key, err := s.tokenKey(token)
	if err != nil {
		return Session{}, ErrSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[key]
	if !ok {
		return Session{}, ErrSession
	}
	if !s.now().Before(session.ExpiresAt) {
		delete(s.sessions, key)
		return Session{}, ErrSession
	}
	return session, nil
}

func (s *Service) Logout(token string) {
	key, err := s.tokenKey(token)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
}

func (s *Service) tokenKey(token string) (string, error) {
	if len(token) != 64 {
		return "", ErrSession
	}
	if _, err := hex.DecodeString(token); err != nil {
		return "", ErrSession
	}
	digest := sha256.Sum256([]byte(token))
	return s.space.Key("sessions", hex.EncodeToString(digest[:]))
}
