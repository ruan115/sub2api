package daemon

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

var ErrShutdown = errors.New("host-agent lifecycle shutdown did not finish")
var ErrIdentityExpired = errors.New("host-agent node identity expired")

type factories struct {
	prepare func(context.Context, config.Config, Config) (*components, error)
	listen  func(string, string) (net.Listener, error)
}

// Run explicitly opts into lifecycle-only management. All trust configuration
// is loaded before external I/O; callers must not substitute the generic health
// server's always-ready handler for this mode. No production flags are enabled.
func Run(ctx context.Context, health config.Config, cfg Config, logger *slog.Logger) error {
	return run(ctx, health, cfg, logger, factories{prepare: prepare, listen: net.Listen})
}

func run(ctx context.Context, health config.Config, cfg Config, logger *slog.Logger, f factories) error {
	if ctx == nil || ctx.Err() != nil || validateRuntime(health, cfg) != nil || f.prepare == nil || f.listen == nil {
		return ErrRuntime
	}
	if logger == nil {
		logger = slog.Default()
	}
	parts, err := f.prepare(ctx, health, cfg)
	if err != nil {
		return ErrRuntime
	}
	if parts == nil || parts.close == nil {
		return ErrRuntime
	}
	defer parts.close()
	if parts.control == nil || parts.commands == nil || !parts.expiresAt.After(time.Now()) || ctx.Err() != nil {
		return ErrRuntime
	}
	listener, err := f.listen("tcp", health.ListenAddress)
	if err != nil {
		return ErrRuntime
	}
	defer listener.Close()
	if ctx.Err() != nil {
		parts.commands.Seal()
		return nil
	}
	if !parts.expiresAt.After(time.Now()) {
		parts.commands.Seal()
		return ErrIdentityExpired
	}
	active, cancel := context.WithDeadline(ctx, parts.expiresAt)
	defer cancel()
	server := &http.Server{Handler: Handler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second}
	controlDone, httpDone := make(chan error, 1), make(chan error, 1)
	go func() { controlDone <- parts.control.Run(active) }()
	go func() { httpDone <- server.Serve(listener) }()
	logger.Info("host-agent lifecycle-only runtime started", "production_ready", false)
	var reason error
	controlFinished, httpFinished := false, false
	select {
	case <-active.Done():
		if ctx.Err() == nil {
			reason = ErrIdentityExpired
		}
	case <-controlDone:
		controlFinished, reason = true, ErrRuntime
	case <-httpDone:
		httpFinished, reason = true, ErrRuntime
	}
	// Done and a worker result can become ready together (especially while
	// startup logging yields). Result arrival must not misclassify cancellation
	// or identity expiry as a transport failure.
	if ctx.Err() != nil {
		reason = nil
	} else if !parts.expiresAt.After(time.Now()) {
		reason = ErrIdentityExpired
	}
	// Seal before cancellation can race queued command workers. Join tracked
	// operations before releasing their transport dependencies. A timeout is a
	// fatal shutdown failure, not a successful drain or container cleanup.
	parts.commands.Seal()
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer stop()
	clean := true
	if server.Shutdown(shutdown) != nil {
		_ = server.Close()
		clean = false
	}
	if parts.commands.Wait(shutdown) != nil {
		clean = false
	}
	if !controlFinished {
		select {
		case <-controlDone:
		case <-shutdown.Done():
			clean = false
		}
	}
	if !httpFinished {
		select {
		case <-httpDone:
		case <-shutdown.Done():
			clean = false
		}
	}
	if !clean {
		return ErrShutdown
	}
	return reason
}
