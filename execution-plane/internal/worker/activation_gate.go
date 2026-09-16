package worker

import (
	"context"
	"sync"
)

// activationGate serializes activation operations without retaining a cancelled
// waiter behind a blocked dependency. Its zero value is ready for use. Like a
// mutex, it must not be copied after first use.
type activationGate struct {
	once sync.Once
	held chan struct{}
}

func (g *activationGate) init() {
	g.once.Do(func() { g.held = make(chan struct{}, 1) })
}

func (g *activationGate) LockContext(ctx context.Context) error {
	if ctx == nil {
		return ErrActivationRejected
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	g.init()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case g.held <- struct{}{}:
	}
	// If cancellation and acquisition were both ready, select may have chosen
	// acquisition. Return its token before reporting cancellation in that case.
	if err := ctx.Err(); err != nil {
		g.Unlock()
		return err
	}
	return nil
}

// Lock is used by Drain, whose pending-secret cleanup deliberately waits for an
// in-flight operation even though health becomes unavailable before that wait.
func (g *activationGate) Lock() {
	_ = g.LockContext(context.Background())
}

func (g *activationGate) Unlock() {
	g.init()
	select {
	case <-g.held:
	default:
		panic("worker: unlock of unlocked activation gate")
	}
}
