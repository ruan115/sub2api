package service

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

type healthResponse struct {
	Status string      `json:"status"`
	Role   config.Role `json:"role"`
	NodeID string      `json:"node_id"`
}

func Handler(cfg config.Config) http.Handler {
	mux := http.NewServeMux()
	writeHealth := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status: "ok",
			Role:   cfg.Role,
			NodeID: cfg.NodeID,
		})
	}
	mux.HandleFunc("GET /healthz", writeHealth)
	mux.HandleFunc("GET /readyz", writeHealth)
	return mux
}

func Run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.Default()
	}

	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           Handler(cfg),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	result := make(chan error, 1)
	go func() {
		logger.Info("execution service listening",
			"role", cfg.Role,
			"node_id", cfg.NodeID,
			"address", cfg.ListenAddress,
		)
		result <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		err := <-result
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
