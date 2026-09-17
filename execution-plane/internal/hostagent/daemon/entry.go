package daemon

import (
	"context"
	"log/slog"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

// SelectRunner is wired only by cmd/host-agent. The shared service package
// must not import host-agent implementations: other services and component
// tests use that package independently of the node daemon.
func SelectRunner(ctx context.Context, health config.Config, getenv func(string) string, logger *slog.Logger) (func() error, error) {
	if health.Role != config.RoleHostAgent {
		return nil, ErrConfig
	}
	cfg, err := Load(getenv)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}
	return func() error { return Run(ctx, health, cfg, logger) }, nil
}
