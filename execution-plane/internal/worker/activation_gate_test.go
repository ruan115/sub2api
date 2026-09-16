package worker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestActivationWaiterCancellationReturnsBeforeFirstCommit(t *testing.T) {
	firstEntered, releaseFirst := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	a, recipient := newHealthActivator(t, healthCommitFunc(func(_ context.Context, request CredentialCommitRequest) (string, error) {
		if request.CredentialLeaseID != "lease-1" {
			t.Error("cancelled waiter reached credential commit")
			return "", errors.New("unexpected commit")
		}
		close(firstEntered)
		<-releaseFirst
		return "version-1", nil
	}))
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseFirst) }) })
	first := healthActivation(t, a, recipient, 1)
	firstDone := make(chan error, 1)
	go func() { _, err := a.Activate(context.Background(), first); firstDone <- err }()
	select {
	case <-firstEntered:
	case <-time.After(time.Second):
		t.Fatal("first activation did not reach its blocked commit")
	}
	second := healthActivation(t, a, recipient, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiter := &activationWaitContext{Context: ctx, started: make(chan struct{})}
	secondDone := make(chan error, 1)
	go func() { _, err := a.Activate(waiter, second); secondDone <- err }()
	// Done is first consulted by the gate's select. The first commit still owns
	// the gate, so this signal proves the second activation reached the wait.
	select {
	case <-waiter.started:
	case <-time.After(time.Second):
		t.Fatal("second activation did not reach the cancellable operation gate")
	}
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, ErrActivationRejected) {
			t.Fatalf("cancelled waiter error=%v", err)
		}
	case <-time.After(200 * time.Millisecond):
		releaseOnce.Do(func() { close(releaseFirst) })
		<-firstDone
		<-secondDone
		t.Fatal("cancelled activation still waits for another activation's commit")
	}
	select {
	case <-firstDone:
		t.Fatal("waiter cancellation disturbed the first activation")
	default:
	}
	if a.HealthSnapshot(context.Background()).LoadedState != nil || a.onboarder.(*fakeOnboardingEngine).calls.Load() != 1 {
		t.Fatal("cancelled waiter ran onboarding or published state")
	}
	releaseOnce.Do(func() { close(releaseFirst) })
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	loaded := a.HealthSnapshot(context.Background()).LoadedState
	if loaded == nil || loaded.CredentialVersionID != "version-1" || loaded.ProxyLeaseID != "proxy-1" || loaded.ActivationRevision != 1 {
		t.Fatal("cancelled waiter changed the first publication")
	}
}

type activationWaitContext struct {
	context.Context
	once    sync.Once
	started chan struct{}
}

func (c *activationWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.started) })
	return c.Context.Done()
}

func TestActivationGateCancelledAcquisitionDoesNotRetainToken(t *testing.T) {
	var gate activationGate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.LockContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquisition error=%v", err)
	}
	if err := gate.LockContext(nil); !errors.Is(err, ErrActivationRejected) {
		t.Fatalf("nil context error=%v", err)
	}
	// This context cancels during the post-acquisition check, deterministically
	// covering token return even when select has acquired an available gate.
	during, cancelDuring := context.WithCancel(context.Background())
	defer cancelDuring()
	if err := gate.LockContext(&activationCancelAfterAcquisition{Context: during, cancel: cancelDuring}); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-acquisition cancellation error=%v", err)
	}
	live, release := context.WithTimeout(context.Background(), time.Second)
	defer release()
	if err := gate.LockContext(live); err != nil {
		t.Fatalf("cancelled acquisition retained the token: %v", err)
	}
	gate.Unlock()
}

type activationCancelAfterAcquisition struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *activationCancelAfterAcquisition) Err() error {
	c.checks++
	if c.checks > 1 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestActivationGateSerializesConcurrentOperations(t *testing.T) {
	var gate activationGate
	var wg sync.WaitGroup
	value := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 16 {
				gate.Lock()
				value++
				gate.Unlock()
			}
		}()
	}
	wg.Wait()
	if value != 256 {
		t.Fatalf("concurrent gate count=%d", value)
	}
}
