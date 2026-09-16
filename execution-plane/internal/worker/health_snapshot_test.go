package worker

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

type healthCommitFunc func(context.Context, CredentialCommitRequest) (string, error)

func (f healthCommitFunc) CommitCredential(ctx context.Context, request CredentialCommitRequest) (string, error) {
	return f(ctx, request)
}

func newHealthActivator(t *testing.T, committer CredentialCommitter) (*SecureActivator, *credential.Recipient) {
	t.Helper()
	identity := Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-health", NodeID: "node-health", Epoch: 3}
	recipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x56}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(recipient.Destroy)
	activator, err := NewSecureActivator(SecureActivatorConfig{Identity: identity, Recipient: recipient, Onboarder: &fakeOnboardingEngine{}, Committer: committer})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(activator.Drain)
	return activator, recipient
}

func healthActivation(t *testing.T, a *SecureActivator, recipient *credential.Recipient, round int) Activation {
	t.Helper()
	return sealedTestActivation(t, recipient, a.identity, fmt.Sprintf("lease-%d", round), fmt.Sprintf("proxy-%d", round), []byte("health-source-secret"))
}

func TestHealthSnapshotPublishesAtomicSecretFreeMetadata(t *testing.T) {
	a, recipient := newHealthActivator(t, healthCommitFunc(func(_ context.Context, request CredentialCommitRequest) (string, error) {
		return strings.Replace(request.CredentialLeaseID, "lease-", "version-", 1), nil
	}))
	if got := a.HealthSnapshot(context.Background()); got.LoadedState != nil || got.Modes[1].Healthy || a.Ready() {
		t.Fatal("unactivated worker returned loaded evidence")
	}
	for round := 1; round <= 3; round++ {
		if _, err := a.Activate(context.Background(), healthActivation(t, a, recipient, round)); err != nil {
			t.Fatal(err)
		}
		got := a.HealthSnapshot(context.Background())
		want := &LoadedState{Identity: a.identity, CredentialVersionID: fmt.Sprintf("version-%d", round), AuthType: AuthTypeOAuth,
			ProxyLeaseID: fmt.Sprintf("proxy-%d", round), ActivationRevision: uint64(round)}
		if !reflect.DeepEqual(got.LoadedState, want) || !got.Modes[1].Healthy || got.Modes[0].Healthy || !a.Ready() {
			t.Fatalf("snapshot=%+v", got)
		}
		encoded, err := json.Marshal(got)
		if err != nil || bytes.Contains(encoded, []byte("health-source-secret")) || bytes.Contains(encoded, []byte("CredentialJSON")) {
			t.Fatal("health snapshot contains credential material")
		}
		got.LoadedState.ProxyLeaseID = "caller-mutated"
		got.Modes[1].Healthy = false
		fresh := a.HealthSnapshot(context.Background())
		if !reflect.DeepEqual(fresh.LoadedState, want) || !fresh.Modes[1].Healthy {
			t.Fatal("snapshot aliases live state")
		}
	}
}

func TestHealthSnapshotNeverMixesConcurrentActivationMetadata(t *testing.T) {
	a, recipient := newHealthActivator(t, healthCommitFunc(func(_ context.Context, request CredentialCommitRequest) (string, error) {
		return strings.Replace(request.CredentialLeaseID, "lease-", "version-", 1), nil
	}))
	stop, done := make(chan struct{}), make(chan struct{})
	var stopOnce sync.Once
	t.Cleanup(func() { stopOnce.Do(func() { close(stop) }); <-done })
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			snapshot := a.HealthSnapshot(context.Background())
			if loaded := snapshot.LoadedState; loaded != nil {
				if loaded.CredentialVersionID != fmt.Sprintf("version-%d", loaded.ActivationRevision) ||
					loaded.ProxyLeaseID != fmt.Sprintf("proxy-%d", loaded.ActivationRevision) || !snapshot.Modes[1].Healthy {
					t.Error("snapshot mixed different activation revisions")
					return
				}
			} else if snapshot.Modes[1].Healthy {
				t.Error("healthy snapshot lost its loaded metadata")
				return
			}
		}
	}()
	for round := 1; round <= 32; round++ {
		if _, err := a.Activate(context.Background(), healthActivation(t, a, recipient, round)); err != nil {
			stopOnce.Do(func() { close(stop) })
			<-done
			t.Fatal(err)
		}
	}
	stopOnce.Do(func() { close(stop) })
	<-done
}

func TestHealthSnapshotFailedOrCancelledCommitPreservesPreviousState(t *testing.T) {
	for _, cause := range []string{"commit error", "invalid ack", "cancel after ack"} {
		t.Run(cause, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fail := false
			a, recipient := newHealthActivator(t, healthCommitFunc(func(_ context.Context, request CredentialCommitRequest) (string, error) {
				if fail {
					switch cause {
					case "commit error":
						return "", errors.New("vault-private-detail")
					case "invalid ack":
						return "bad\nversion", nil
					case "cancel after ack":
						cancel()
					}
				}
				return strings.Replace(request.CredentialLeaseID, "lease-", "version-", 1), nil
			}))
			if _, err := a.Activate(context.Background(), healthActivation(t, a, recipient, 1)); err != nil {
				t.Fatal(err)
			}
			before := a.HealthSnapshot(context.Background())
			fail = true
			next := healthActivation(t, a, recipient, 2)
			if _, err := a.Activate(ctx, next); !errors.Is(err, ErrActivationRejected) {
				t.Fatal(err)
			}
			if after := a.HealthSnapshot(context.Background()); !reflect.DeepEqual(before, after) {
				t.Fatal("failed/cancelled commit published metadata")
			}
			fail = false
			if _, err := a.Activate(context.Background(), next); err != nil {
				t.Fatal("safe pending retry failed", err)
			}
			if got := a.HealthSnapshot(context.Background()).LoadedState; got.ActivationRevision != 2 || got.CredentialVersionID != "version-2" || got.ProxyLeaseID != "proxy-2" {
				t.Fatal("retry counted a rejected publication")
			}
			if a.onboarder.(*fakeOnboardingEngine).calls.Load() != 2 {
				t.Fatal("safe retry repeated onboarding")
			}
		})
	}
}

func TestDrainHidesLoadedStateBeforeBlockedCommitFinishes(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	a, recipient := newHealthActivator(t, healthCommitFunc(func(_ context.Context, request CredentialCommitRequest) (string, error) {
		if request.CredentialLeaseID == "lease-2" {
			close(entered)
			<-release // deliberately ignores cancellation: Drain must still hide active state.
		}
		return strings.Replace(request.CredentialLeaseID, "lease-", "version-", 1), nil
	}))
	// Release the intentionally blocked dependency before fixture Drain cleanup,
	// including when an assertion fails before the normal release below.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	if _, err := a.Activate(context.Background(), healthActivation(t, a, recipient, 1)); err != nil {
		t.Fatal(err)
	}
	activation := healthActivation(t, a, recipient, 2)
	activated := make(chan error, 1)
	go func() { _, err := a.Activate(context.Background(), activation); activated <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("commit did not block")
	}
	if got := a.HealthSnapshot(context.Background()).LoadedState; got == nil || got.CredentialVersionID != "version-1" || got.ProxyLeaseID != "proxy-1" || got.ActivationRevision != 1 {
		t.Fatal("unacknowledged commit changed the active snapshot")
	}
	drained := make(chan struct{})
	go func() { a.Drain(); close(drained) }()
	deadline := time.After(time.Second)
	for a.HealthSnapshot(context.Background()).LoadedState != nil {
		select {
		case <-deadline:
			releaseOnce.Do(func() { close(release) })
			t.Fatal("Drain did not immediately hide loaded state")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if a.Ready() || a.ModeHealth(context.Background())[1].Healthy {
		t.Fatal("draining worker still ready")
	}
	select {
	case <-drained:
		t.Fatal("Drain claimed blocked dependency cleanup had finished")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-activated; !errors.Is(err, ErrActivationRejected) {
		t.Fatal("in-flight commit revived draining worker", err)
	}
	<-drained
	if a.HealthSnapshot(context.Background()).LoadedState != nil || a.pending.CredentialLeaseID != "" || a.activationRevision != 1 {
		t.Fatal("Drain left loaded state, pending material or a new revision")
	}
}

func TestHealthRequiresActualCredentialBytesAndRevisionNeverWraps(t *testing.T) {
	a, recipient := newHealthActivator(t, healthCommitFunc(func(context.Context, CredentialCommitRequest) (string, error) { return "version", nil }))
	a.mu.Lock()
	a.active = ActiveCredential{VersionID: "version-without-bytes", AuthType: AuthTypeOAuth, ProxyLeaseID: "proxy"}
	a.mu.Unlock()
	if a.Ready() || a.ModeHealth(context.Background())[1].Healthy || a.HealthSnapshot(context.Background()).LoadedState != nil {
		t.Fatal("version ID without credential bytes reported healthy")
	}
	a.mu.Lock()
	a.activationRevision = math.MaxUint64
	a.mu.Unlock()
	if _, err := a.Activate(context.Background(), healthActivation(t, a, recipient, 1)); !errors.Is(err, ErrActivationRejected) {
		t.Fatal("revision overflow was accepted", err)
	}
	if a.onboarder.(*fakeOnboardingEngine).calls.Load() != 0 || a.activationRevision != math.MaxUint64 {
		t.Fatal("overflow performed onboarding or wrapped the revision")
	}
}

type healthFirstErrContext struct {
	context.Context
	once    sync.Once
	checked chan struct{}
}

func (c *healthFirstErrContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func TestActivationCancelledWhileWaitingForOperationLockCannotPublish(t *testing.T) {
	a, recipient := newHealthActivator(t, healthCommitFunc(func(context.Context, CredentialCommitRequest) (string, error) {
		t.Error("cancelled activation attempted commit")
		return "version-1", nil
	}))
	activation := healthActivation(t, a, recipient, 1)
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &healthFirstErrContext{Context: parent, checked: make(chan struct{})}
	a.operationMu.Lock()
	done := make(chan error, 1)
	go func() { _, err := a.Activate(ctx, activation); done <- err }()
	select {
	case <-ctx.checked:
	case <-time.After(time.Second):
		a.operationMu.Unlock()
		t.Fatal("activation did not pass its initial context check")
	}
	cancel()
	a.operationMu.Unlock()
	if err := <-done; !errors.Is(err, ErrActivationRejected) {
		t.Fatal("cancelled lock waiter published", err)
	}
	if a.onboarder.(*fakeOnboardingEngine).calls.Load() != 0 || a.HealthSnapshot(context.Background()).LoadedState != nil || a.activationRevision != 0 {
		t.Fatal("cancelled lock waiter changed activation state")
	}
}

type healthOnboardFunc func(context.Context, OnboardingInput) (OnboardingResult, error)

func (f healthOnboardFunc) Onboard(ctx context.Context, input OnboardingInput) (OnboardingResult, error) {
	return f(ctx, input)
}

func TestInvalidOnboardingResultCannotBecomeLoadedEvidence(t *testing.T) {
	for _, cause := range []string{"empty credential", "unknown auth", "error with material"} {
		t.Run(cause, func(t *testing.T) {
			a, recipient := newHealthActivator(t, healthCommitFunc(func(context.Context, CredentialCommitRequest) (string, error) {
				t.Error("invalid onboarding result reached commit")
				return "version-1", nil
			}))
			material := []byte("onboard-result-secret")
			a.onboarder = healthOnboardFunc(func(context.Context, OnboardingInput) (OnboardingResult, error) {
				result := OnboardingResult{AuthType: AuthTypeOAuth, CredentialJSON: material}
				switch cause {
				case "empty credential":
					result.CredentialJSON = nil
				case "unknown auth":
					result.AuthType = "unknown"
				case "error with material":
					return result, errors.New("untrusted-source-detail")
				}
				return result, nil
			})
			if _, err := a.Activate(context.Background(), healthActivation(t, a, recipient, 1)); !errors.Is(err, ErrActivationRejected) {
				t.Fatal(err)
			}
			if a.Ready() || a.HealthSnapshot(context.Background()).LoadedState != nil || a.activationRevision != 0 {
				t.Fatal("invalid onboarding result became loaded evidence")
			}
			if cause != "empty credential" && !bytes.Equal(material, make([]byte, len(material))) {
				t.Fatal("rejected onboarding material was not zeroed")
			}
		})
	}
}

type oneReadHealthSource struct {
	snapshot HealthSnapshot
	calls    int
	read     func(context.Context) HealthSnapshot
}

func (s *oneReadHealthSource) HealthSnapshot(ctx context.Context) HealthSnapshot {
	s.calls++
	if s.read != nil {
		return s.read(ctx)
	}
	return s.snapshot
}
func (*oneReadHealthSource) ModeHealth(context.Context) []ModeHealth {
	panic("Health must not make an independent ModeHealth read")
}

func healthRuntimeFixture(t *testing.T, identity Identity, source ModeHealthSource) (*RuntimeServer, func() string) {
	t.Helper()
	now := time.Now().UTC()
	pub, key, err := ed25519.GenerateKey(strings.NewReader(strings.Repeat("h", ed25519.SeedSize)))
	if err != nil {
		t.Fatal(err)
	}
	issuer, _ := ticket.NewIssuer(key)
	verifier, _ := ticket.NewVerifier(pub)
	guard, _ := NewGuard(verifier, identity, func() time.Time { return now })
	server, err := NewRuntimeServer(RuntimeServerConfig{Identity: identity, Guard: guard, Activator: &recordingActivator{}, Executor: deterministicExecutor{}, HealthSource: source, ImageDigest: "sha256:" + strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	return server, func() string {
		claims, err := ticket.NewClaims(identity.AccountID, identity.SlotID, identity.NodeID, identity.Epoch, []string{"health"}, now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := issuer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
}

func TestHealthRPCUsesOneSnapshotAndRejectsMalformedLoadedState(t *testing.T) {
	identity := Identity{AccountID: "account-health", SlotID: "slot-health", NodeID: "node-health", Epoch: 1}
	for _, name := range []string{"valid", "account", "slot", "node", "epoch", "version", "proxy", "auth", "revision"} {
		t.Run(name, func(t *testing.T) {
			loaded := &LoadedState{Identity: identity, CredentialVersionID: "version-1", AuthType: AuthTypeOAuth, ProxyLeaseID: "proxy-1", ActivationRevision: 1}
			switch name {
			case "account":
				loaded.Identity.AccountID = "other"
			case "slot":
				loaded.Identity.SlotID = "other"
			case "node":
				loaded.Identity.NodeID = "other"
			case "epoch":
				loaded.Identity.Epoch++
			case "version":
				loaded.CredentialVersionID = "bad\nversion"
			case "proxy":
				loaded.ProxyLeaseID = ""
			case "auth":
				loaded.AuthType = "private-credential-detail"
			case "revision":
				loaded.ActivationRevision = 0
			}
			source := &oneReadHealthSource{snapshot: HealthSnapshot{Modes: []ModeHealth{{Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, Healthy: true}}, LoadedState: loaded}}
			server, issue := healthRuntimeFixture(t, identity, source)
			challenge := strings.Repeat("a", 32)
			got, err := server.Health(context.Background(), &executionv1.HealthRequest{ExecutionTicket: issue(), Challenge: challenge})
			if source.calls != 1 {
				t.Fatal("snapshot was not read exactly once")
			}
			if name != "valid" {
				if got != nil || status.Code(err) != codes.Internal || strings.Contains(err.Error(), "private-credential-detail") {
					t.Fatal("invalid snapshot returned evidence or leaked detail", got, err)
				}
				return
			}
			if err != nil || got.GetChallenge() != challenge || got.GetLoadedState().GetCredentialVersionId() != loaded.CredentialVersionID || got.GetLoadedState().GetActivationRevision() != 1 {
				t.Fatal(got, err)
			}
		})
	}
}

func TestHealthRPCLegacyFakeAndChallengeCompatibility(t *testing.T) {
	identity := Identity{AccountID: "account-health", SlotID: "slot-health", NodeID: "node-health", Epoch: 1}
	for _, source := range []ModeHealthSource{staticHealth{}, &processState{activated: true}} {
		server, issue := healthRuntimeFixture(t, identity, source)
		for _, challenge := range []string{"", strings.Repeat("b", 32)} {
			response, err := server.Health(context.Background(), &executionv1.HealthRequest{ExecutionTicket: issue(), Challenge: challenge})
			if err != nil || response.GetLoadedState() != nil || response.GetChallenge() != challenge || len(response.GetModes()) != 2 || !response.GetModes()[1].GetHealthy() {
				t.Fatal("legacy/fake compatibility invented proof or changed modes", response, err)
			}
		}
		for _, challenge := range []string{"short", strings.Repeat("A", 32), strings.Repeat("g", 32), strings.Repeat("b", 32) + "\n"} {
			raw := issue()
			if _, err := server.Health(context.Background(), &executionv1.HealthRequest{ExecutionTicket: raw, Challenge: challenge}); status.Code(err) != codes.InvalidArgument {
				t.Fatal("malformed challenge accepted", err)
			}
			if _, err := server.Health(context.Background(), &executionv1.HealthRequest{ExecutionTicket: raw}); err != nil {
				t.Fatal("malformed challenge consumed a valid ticket", err)
			}
		}
	}
}

func TestHealthRPCLoadedSnapshotContainsNoCredentialBytes(t *testing.T) {
	a, recipient := newHealthActivator(t, healthCommitFunc(func(context.Context, CredentialCommitRequest) (string, error) { return "version-1", nil }))
	server, issue := healthRuntimeFixture(t, a.identity, a)
	if _, err := a.Activate(context.Background(), healthActivation(t, a, recipient, 1)); err != nil {
		t.Fatal(err)
	}
	response, err := server.Health(context.Background(), &executionv1.HealthRequest{ExecutionTicket: issue(), Challenge: strings.Repeat("c", 32)})
	if err != nil || response.GetLoadedState() == nil {
		t.Fatal(response, err)
	}
	encoded, err := protojson.Marshal(response)
	if err != nil || bytes.Contains(encoded, []byte("health-source-secret")) || bytes.Contains(encoded, []byte("access_token")) {
		t.Fatal("health response leaked credential bytes")
	}
	a.Drain()
	response, err = server.Health(context.Background(), &executionv1.HealthRequest{ExecutionTicket: issue(), Challenge: strings.Repeat("d", 32)})
	if err != nil || response.GetLoadedState() != nil || response.GetModes()[1].GetHealthy() {
		t.Fatal("drained RPC returned loaded state", response, err)
	}
}

type healthModeFunc func(context.Context) []ModeHealth

func (f healthModeFunc) ModeHealth(ctx context.Context) []ModeHealth { return f(ctx) }

func TestHealthRPCRejectsCancellationBeforeAndDuringSnapshotRead(t *testing.T) {
	identity := Identity{AccountID: "account-health", SlotID: "slot-health", NodeID: "node-health", Epoch: 1}
	server, issue := healthRuntimeFixture(t, identity, staticHealth{})
	raw := issue()
	if response, err := server.Health(nil, &executionv1.HealthRequest{ExecutionTicket: raw}); response != nil || status.Code(err) != codes.InvalidArgument {
		t.Fatal("nil context accepted", response, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if response, err := server.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: raw}); response != nil || status.Code(err) != codes.Canceled {
		t.Fatal("cancelled inbound context accepted", response, err)
	}
	if _, err := server.Health(context.Background(), &executionv1.HealthRequest{ExecutionTicket: raw}); err != nil {
		t.Fatal("cancelled inbound request consumed ticket", err)
	}
	for _, atomicSource := range []bool{false, true} {
		t.Run(fmt.Sprintf("atomic=%t", atomicSource), func(t *testing.T) {
			entered := make(chan struct{})
			modes := []ModeHealth{{Mode: executionv1.ExecutionMode_EXECUTION_MODE_OAUTH_API, Healthy: true}}
			read := healthModeFunc(func(ctx context.Context) []ModeHealth {
				close(entered)
				<-ctx.Done()
				return modes // a source returning success cannot override cancellation.
			})
			var source ModeHealthSource = read
			if atomicSource {
				source = &oneReadHealthSource{read: func(ctx context.Context) HealthSnapshot {
					return HealthSnapshot{Modes: read(ctx), LoadedState: &LoadedState{Identity: identity, CredentialVersionID: "version-1", AuthType: AuthTypeOAuth, ProxyLeaseID: "proxy-1", ActivationRevision: 1}}
				}}
			}
			server, issue := healthRuntimeFixture(t, identity, source)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			raw := issue()
			go func() {
				response, err := server.Health(ctx, &executionv1.HealthRequest{ExecutionTicket: raw, Challenge: strings.Repeat("a", 32)})
				if response != nil {
					done <- errors.New("cancelled read returned health metadata")
					return
				}
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("health source was not called")
			}
			cancel()
			select {
			case err := <-done:
				if status.Code(err) != codes.Canceled {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled health read did not finish")
			}
		})
	}
}
