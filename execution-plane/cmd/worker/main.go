package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		address := os.Getenv("EXECUTION_HEALTHCHECK_ADDRESS")
		if address == "" {
			address = "127.0.0.1:8093"
		}
		if err := worker.Healthcheck(address, 2*time.Second); err != nil {
			os.Exit(1)
		}
		return
	}
	config, err := worker.LoadProcessConfig(os.Getenv)
	if err != nil {
		logger.Error("invalid worker runtime configuration", "error", err)
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := worker.RunProcess(ctx, config, logger); err != nil {
		logger.Error("worker runtime stopped", "error", err)
		os.Exit(1)
	}
}
