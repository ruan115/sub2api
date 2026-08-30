package service

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

func Main(role config.Role) {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(role, os.Getenv)
	if err != nil {
		logger.Error("invalid execution service configuration", "role", role, "error", err)
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	run := func() error { return Run(ctx, cfg, logger) }
	if role == config.RoleOrchestrator {
		runtimeConfig, runtimeErr := config.LoadOrchestratorRuntime(os.Getenv)
		if runtimeErr != nil {
			logger.Error("invalid production orchestrator configuration", "role", role, "error", runtimeErr)
			os.Exit(2)
		}
		if runtimeConfig.Enabled {
			run = func() error { return RunProductionOrchestrator(ctx, cfg, runtimeConfig, logger) }
		}
	}
	if err := run(); err != nil {
		logger.Error("execution service stopped", "role", role, "error", err)
		os.Exit(1)
	}
}
