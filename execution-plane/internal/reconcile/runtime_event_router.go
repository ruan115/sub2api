package reconcile

import (
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

var ErrRuntimeEventRouter = errors.New("runtime event router configuration is invalid")

// NewRuntimeEventRouter binds all CCMAX event classes to one ordered handler
// graph. Callers must attach the returned router to exactly one consumer name;
// separate checkpoints would allow onboarding to overtake its proxy grant.
func NewRuntimeEventRouter(
	source AccountRuntimeSource,
	lifecycle store.LifecycleEventApplyRepository,
	triggers onboarding.OnboardingStartTriggerRepository,
	reservations store.ProxyReservationGrantRepository,
	now func() time.Time,
) (*outbox.Router, error) {
	runtimeHandler, err := NewOutboxHandler(source, lifecycle, now)
	if err != nil {
		return nil, ErrRuntimeEventRouter
	}
	onboardingHandler, err := NewOnboardingTriggerOutboxHandler(source, triggers, now)
	if err != nil {
		return nil, ErrRuntimeEventRouter
	}
	reservationHandler, err := NewProxyReservationOutboxHandler(reservations, now)
	if err != nil {
		return nil, ErrRuntimeEventRouter
	}
	router, err := outbox.NewRouter(map[string]outbox.Handler{
		ProxyReservationGrantedEvent: reservationHandler,
		ProxyReservationRevokedEvent: reservationHandler,

		"account.runtime.provision_requested":  onboardingHandler,
		"account.credential.migrate_requested": onboardingHandler,
		"account.credential.rotate_requested":  onboardingHandler,

		"account.runtime.restore_requested": runtimeHandler,
		"account.proxy.change_requested":    runtimeHandler,
		"account.runtime.drain_requested":   runtimeHandler,
		"account.runtime.destroy_requested": runtimeHandler,
	})
	if err != nil {
		return nil, ErrRuntimeEventRouter
	}
	return router, nil
}
