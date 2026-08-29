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
	if err := Run(ctx, cfg, logger); err != nil {
		logger.Error("execution service stopped", "role", role, "error", err)
		os.Exit(1)
	}
}
