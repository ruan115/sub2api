package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/namespace"
)

func TestDemoSessionExpiresAndLogoutRevokes(t *testing.T) {
	clock := time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC)
	service := newDemoService(t, "identity-expiry", func() time.Time { return clock })

	token, session, err := service.Login(" ADMIN@example.invalid ", "Demo-admin-2026!")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if session.User.ID != "u-admin" {
		t.Fatalf("Login() principal = %#v, want admin fixture", session.User)
	}
	if got, want := session.ExpiresAt, clock.Add(SessionTTL); !got.Equal(want) {
		t.Fatalf("expiry = %s, want %s", got, want)
	}

	clock = clock.Add(SessionTTL)
	if _, err := service.Authenticate(token); !errors.Is(err, ErrSession) {
		t.Fatalf("Authenticate(expired) error = %v, want ErrSession", err)
	}

	clock = clock.Add(time.Second)
	token, _, err = service.Login("member@example.invalid", "Demo-member-2026!")
	if err != nil {
		t.Fatalf("second Login() error = %v", err)
	}
	service.Logout(token)
	if _, err := service.Authenticate(token); !errors.Is(err, ErrSession) {
		t.Fatalf("Authenticate(logged out) error = %v, want ErrSession", err)
	}
}

func TestDemoSessionCapacityCollisionAndEntropyFailure(t *testing.T) {
	service := newDemoService(t, "identity-capacity", fixedNow)
	service.maxSessions = 1
	if _, _, err := service.Login("admin@example.invalid", "Demo-admin-2026!"); err != nil {
		t.Fatalf("first Login() error = %v", err)
	}
	if _, _, err := service.Login("member@example.invalid", "Demo-member-2026!"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Login(capacity) error = %v, want ErrCapacity", err)
	}

	collision := newDemoService(t, "identity-collision", fixedNow)
	collision.maxSessions = 2
	collision.entropy = bytes.NewReader(bytes.Repeat([]byte{0x3a}, 32*5))
	if _, _, err := collision.Login("admin@example.invalid", "Demo-admin-2026!"); err != nil {
		t.Fatalf("first collision Login() error = %v", err)
	}
	if _, _, err := collision.Login("member@example.invalid", "Demo-member-2026!"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Login(repeated token collision) error = %v, want ErrUnavailable", err)
	}

	entropyFailure := newDemoService(t, "identity-entropy", fixedNow)
	entropyFailure.entropy = strings.NewReader("too short")
	if _, _, err := entropyFailure.Login("admin@example.invalid", "Demo-admin-2026!"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Login(entropy failure) error = %v, want ErrUnavailable", err)
	}
}

func TestReplaceAtCapacityAtomicallyRetiresOnlyValidPriorSession(t *testing.T) {
	service := newDemoService(t, "identity-replace", fixedNow)
	service.maxSessions = 1
	oldToken, oldSession, err := service.Login("admin@example.invalid", "Demo-admin-2026!")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	newToken, newSession, err := service.Replace("admin@example.invalid", "Demo-admin-2026!", oldToken)
	if err != nil {
		t.Fatalf("Replace() error = %v", err)
	}
	if newToken == oldToken {
		t.Fatal("Replace() reused the old raw token")
	}
	if newSession.User != oldSession.User {
		t.Fatalf("Replace() principal = %#v, want %#v", newSession.User, oldSession.User)
	}
	if _, err := service.Authenticate(oldToken); !errors.Is(err, ErrSession) {
		t.Fatalf("Authenticate(old token) error = %v, want ErrSession", err)
	}
	if _, err := service.Authenticate(newToken); err != nil {
		t.Fatalf("Authenticate(new token) error = %v", err)
	}
	if len(service.sessions) != 1 {
		t.Fatalf("session count after replacement = %d, want 1", len(service.sessions))
	}
}

func TestReplaceFailuresAndForeignTokensCannotEvictAtCapacity(t *testing.T) {
	service := newDemoService(t, "identity-replace-failures", fixedNow)
	service.maxSessions = 1
	oldToken, _, err := service.Login("admin@example.invalid", "Demo-admin-2026!")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	if _, _, err := service.Replace("admin@example.invalid", "wrong", oldToken); !errors.Is(err, ErrCredentials) {
		t.Fatalf("Replace(wrong password) error = %v, want ErrCredentials", err)
	}
	assertAuthenticated(t, service, oldToken, "wrong password")

	service.entropy = strings.NewReader("too short")
	if _, _, err := service.Replace("admin@example.invalid", "Demo-admin-2026!", oldToken); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Replace(entropy failure) error = %v, want ErrUnavailable", err)
	}
	assertAuthenticated(t, service, oldToken, "entropy failure")
	service.entropy = bytes.NewReader(bytes.Repeat([]byte{0x52}, 32))

	if _, _, err := service.Replace("admin@example.invalid", "Demo-admin-2026!", "not-a-session-token"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Replace(nonexistent token) error = %v, want ErrCapacity", err)
	}
	assertAuthenticated(t, service, oldToken, "nonexistent previous token")

	other := newDemoService(t, "identity-replace-other", fixedNow)
	foreignToken, _, err := other.Login("member@example.invalid", "Demo-member-2026!")
	if err != nil {
		t.Fatalf("other Login() error = %v", err)
	}
	if _, _, err := service.Replace("admin@example.invalid", "Demo-admin-2026!", foreignToken); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Replace(cross-instance token) error = %v, want ErrCapacity", err)
	}
	assertAuthenticated(t, service, oldToken, "cross-instance previous token")
}

func TestConcurrentReplaceConsumesPreviousTokenOnce(t *testing.T) {
	service := newDemoService(t, "identity-replace-concurrent", fixedNow)
	service.maxSessions = 1
	oldToken, _, err := service.Login("admin@example.invalid", "Demo-admin-2026!")
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	type result struct {
		token string
		err   error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			token, _, err := service.Replace("admin@example.invalid", "Demo-admin-2026!", oldToken)
			results <- result{token: token, err: err}
		}()
	}
	group.Wait()
	close(results)

	var replacement string
	var successes, capacityFailures int
	for result := range results {
		if result.err == nil {
			successes++
			replacement = result.token
			continue
		}
		if errors.Is(result.err, ErrCapacity) {
			capacityFailures++
			continue
		}
		t.Fatalf("concurrent Replace() error = %v", result.err)
	}
	if successes != 1 || capacityFailures != 1 {
		t.Fatalf("Replace() outcomes: successes=%d capacity=%d, want one each", successes, capacityFailures)
	}
	if _, err := service.Authenticate(oldToken); !errors.Is(err, ErrSession) {
		t.Fatalf("Authenticate(old token) error = %v, want ErrSession", err)
	}
	assertAuthenticated(t, service, replacement, "concurrent replacement")
}

func TestDemoSessionNamespacesAndJSONRemainOpaque(t *testing.T) {
	first := newDemoService(t, "recovery-a", fixedNow)
	second := newDemoService(t, "recovery-b", fixedNow)
	first.entropy = bytes.NewReader(bytes.Repeat([]byte{0x11}, 32))
	second.entropy = bytes.NewReader(bytes.Repeat([]byte{0x22}, 32))

	firstToken, firstSession, err := first.Login("admin@example.invalid", "Demo-admin-2026!")
	if err != nil {
		t.Fatalf("first Login() error = %v", err)
	}
	secondToken, _, err := second.Login("member@example.invalid", "Demo-member-2026!")
	if err != nil {
		t.Fatalf("second Login() error = %v", err)
	}
	if firstToken == secondToken {
		t.Fatal("separate entropy streams produced equal test tokens")
	}
	if _, err := first.Authenticate(secondToken); !errors.Is(err, ErrSession) {
		t.Fatalf("first Authenticate(second token) error = %v, want ErrSession", err)
	}
	if _, err := second.Authenticate(firstToken); !errors.Is(err, ErrSession) {
		t.Fatalf("second Authenticate(first token) error = %v, want ErrSession", err)
	}

	encoded, err := json.Marshal(firstSession)
	if err != nil {
		t.Fatalf("json.Marshal(Session) error = %v", err)
	}
	if bytes.Contains(encoded, []byte(firstToken)) {
		t.Fatalf("serialized Session leaked raw token: %s", encoded)
	}
}

func TestDemoSessionConcurrentAccessIsSynchronized(t *testing.T) {
	service := newDemoService(t, "identity-concurrency", fixedNow)
	const workers = 32

	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			email, password := "admin@example.invalid", "Demo-admin-2026!"
			if worker%2 == 1 {
				email, password = "member@example.invalid", "Demo-member-2026!"
			}
			token, _, err := service.Login(email, password)
			if err != nil {
				errorsByWorker <- err
				return
			}
			if _, err := service.Authenticate(token); err != nil {
				errorsByWorker <- err
				return
			}
			service.Logout(token)
			if _, err := service.Authenticate(token); !errors.Is(err, ErrSession) {
				errorsByWorker <- err
			}
		}(worker)
	}
	group.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent operation error = %v", err)
		}
	}
}

func newDemoService(t *testing.T, instance string, now func() time.Time) *Service {
	t.Helper()
	space, err := namespace.New(instance)
	if err != nil {
		t.Fatalf("namespace.New(%q) error = %v", instance, err)
	}
	service, err := NewDemo(space, now)
	if err != nil {
		t.Fatalf("NewDemo() error = %v", err)
	}
	return service
}

func assertAuthenticated(t *testing.T, service *Service, token, context string) {
	t.Helper()
	if _, err := service.Authenticate(token); err != nil {
		t.Fatalf("Authenticate() after %s error = %v", context, err)
	}
}

func fixedNow() time.Time {
	return time.Date(2026, time.September, 13, 12, 0, 0, 0, time.UTC)
}
