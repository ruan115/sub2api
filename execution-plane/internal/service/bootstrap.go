package service

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

// RuntimeSelector is supplied by the binary's composition root. A nil runner
// selects the default health-only service; an error must never fall back.
type RuntimeSelector func(context.Context, config.Config, func(string) string, *slog.Logger) (func() error, error)

func Main(role config.Role) {
	MainWithRuntime(role, nil)
}

func MainWithRuntime(role config.Role, selector RuntimeSelector) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(role, os.Getenv)
	if err != nil {
		logger.Error("invalid execution service configuration", "role", role, "error", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	run, err := selectRunner(ctx, cfg, os.Getenv, logger, selector)
	if err != nil {
		logger.Error("invalid execution runtime configuration", "role", role, "error", err)
		os.Exit(2)
	}
	if err := run(); err != nil {
		logger.Error("execution service stopped", "role", role, "error", err)
		os.Exit(1)
	}
}

// Selection is pure configuration loading: off does not load identity or dial
// dependencies, and each role only reads its own runtime configuration.
func selectRunner(ctx context.Context, cfg config.Config, getenv func(string) string, logger *slog.Logger, selector RuntimeSelector) (func() error, error) {
	if selector != nil {
		run, err := selector(ctx, cfg, getenv, logger)
		if err != nil || run != nil {
			return run, err
		}
	}
	run := func() error { return Run(ctx, cfg, logger) }
	if cfg.Role == config.RoleOrchestrator {
		runtimeConfig, runtimeErr := config.LoadOrchestratorRuntime(getenv)
		if runtimeErr != nil {
			return nil, runtimeErr
		}
		if runtimeConfig.Enabled {
			run = func() error { return RunProductionOrchestrator(ctx, cfg, runtimeConfig, logger) }
		}
	}
	return run, nil
}
