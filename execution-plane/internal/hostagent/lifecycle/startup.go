package lifecycle

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

var errStartup = errors.New("authenticated existing slot startup failed")

type startup struct {
	controller *hostagent.Controller
	custody    *Custody
}

func (s *startup) Start(ctx context.Context, spec provider.SlotSpec, expected provider.Instance, leaseOwnerID string) error {
	if s == nil || s.controller == nil || ctx == nil || ctx.Err() != nil {
		return errStartup
	}
	runtime, err := s.controller.StartExisting(ctx, spec, expected)
	if err != nil || runtime == nil {
		if runtime != nil {
			_ = runtime.Close()
		}
		return errStartup
	}
	// The binding is checked before anything is handed over. A connection whose
	// instance does not match the command was never the right one to keep.
	if ctx.Err() != nil || runtime.Instance.ProviderRef != expected.ProviderRef ||
		runtime.Instance.RuntimeID != expected.RuntimeID || runtime.Instance.SlotID != spec.SlotID ||
		runtime.Instance.Epoch != spec.Epoch || runtime.Instance.RuntimeGeneration != spec.RuntimeGeneration {
		_ = runtime.Close()
		return errStartup
	}
	if s.custody == nil {
		// Without a custodian, START still owns only a short-lived
		// authenticated connection and keeps no hidden cache. The context is
		// re-checked after the close so that a cancellation arriving during it
		// is still a failure, exactly as before custody existed.
		if closeErr := runtime.Close(); closeErr != nil || ctx.Err() != nil {
			return errStartup
		}
		return nil
	}
	// Ownership moves to the registry only once the execution lease authority
	// has confirmed the claim. Any refusal leaves the connection with us, so we
	// close it rather than leak it.
	adopted, err := s.custody.take(ctx, runtime.Instance, leaseOwnerID, runtime)
	if err != nil {
		_ = runtime.Close()
		return errStartup
	}
	if !adopted {
		// A replayed START of an instance already under custody. The incumbent
		// keeps its authorized lifetime; this duplicate connection is closed.
		if closeErr := runtime.Close(); closeErr != nil {
			return errStartup
		}
	}
	return nil
}
