package lifecycle

import (
	"context"
	"errors"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/hostagent"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

var errStartup = errors.New("authenticated existing slot startup failed")

type startup struct{ controller *hostagent.Controller }

func (s *startup) Start(ctx context.Context, spec provider.SlotSpec, expected provider.Instance) error {
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
	// START owns only a short-lived authenticated connection. A future runtime
	// registry must acquire its own authorized lifetime; no hidden cache here.
	closeErr := runtime.Close()
	if closeErr != nil || ctx.Err() != nil || runtime.Instance.ProviderRef != expected.ProviderRef ||
		runtime.Instance.RuntimeID != expected.RuntimeID || runtime.Instance.SlotID != spec.SlotID ||
		runtime.Instance.Epoch != spec.Epoch || runtime.Instance.RuntimeGeneration != spec.RuntimeGeneration {
		return errStartup
	}
	return nil
}
