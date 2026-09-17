package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

func TestRunnerSelectionReadsOnlyItsRoleAndOffFlag(t *testing.T) {
	for _, role := range []config.Role{config.RoleHostAgent, config.RoleOrchestrator, config.RoleWorker} {
		t.Run(string(role), func(t *testing.T) {
			var keys []string
			runner, err := selectRunner(context.Background(), config.Default(role), func(key string) string { keys = append(keys, key); return "" }, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
			if err != nil || runner == nil {
				t.Fatal("disabled role selection failed")
			}
			if role != config.RoleOrchestrator {
				if len(keys) != 0 {
					t.Fatal("worker read unrelated config")
				}
				return
			}
			if len(keys) != 1 || !strings.HasSuffix(keys[0], "RUNTIME_ENABLED") {
				t.Fatal("off role read non-flag config")
			}
		})
	}
}

func TestBinaryRuntimeSelectionDoesNotFallBackOnError(t *testing.T) {
	sentinel := errors.New("runtime selection failed")
	for _, fail := range []bool{false, true} {
		called := false
		selector := func(context.Context, config.Config, func(string) string, *slog.Logger) (func() error, error) {
			if fail {
				return nil, sentinel
			}
			return func() error { called = true; return sentinel }, nil
		}
		runner, err := selectRunner(context.Background(), config.Default(config.RoleHostAgent), func(string) string { t.Fatal("runtime selector fell back"); return "" }, nil, selector)
		if fail {
			if runner != nil || !errors.Is(err, sentinel) {
				t.Fatal("selection error lost")
			}
			continue
		}
		if err != nil || runner == nil || !errors.Is(runner(), sentinel) || !called {
			t.Fatal("binary runner not used")
		}
	}
}
