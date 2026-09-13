package service

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/reconcile"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

func TestNewRuntimeOutboxRunnerComposesSingleOrderedConsumer(t *testing.T) {
	ccmaxDatabase, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ccmaxDatabase.Close() })
	runtimeDatabase, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeDatabase.Close() })
	runtimeRepository, err := store.NewRepository(runtimeDatabase)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := NewRuntimeOutboxRunner(ccmaxDatabase, runtimeRepository, RuntimeOutboxConfig{
		ConsumerName: "execution-runtime-v1",
		Owner:        "orchestrator-srv74-1",
		LeaseTTL:     30 * time.Second,
		PollInterval: time.Second,
		BatchSize:    200,
		Defaults: reconcile.CCMAXRuntimeDefaults{
			ImageDigest:      "sha256:" + strings.Repeat("a", 64),
			CPURequestMillis: 500, MemoryRequestBytes: 128 << 20,
		},
	})
	if err != nil || runner == nil {
		t.Fatalf("runtime outbox runner = %v, %v", runner, err)
	}
}

func TestNewRuntimeOutboxRunnerFailsClosed(t *testing.T) {
	database, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repository, _ := store.NewRepository(database)
	valid := RuntimeOutboxConfig{
		ConsumerName: "execution-runtime-v1", Owner: "orchestrator-1", LeaseTTL: time.Minute,
		Defaults: reconcile.CCMAXRuntimeDefaults{
			ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500, MemoryRequestBytes: 128 << 20,
		},
	}
	for _, test := range []struct {
		name      string
		ccmax     *sql.DB
		runtime   *store.Repository
		configure func(*RuntimeOutboxConfig)
	}{
		{name: "missing ccmax", runtime: repository},
		{name: "missing runtime", ccmax: database},
		{name: "missing owner", ccmax: database, runtime: repository, configure: func(config *RuntimeOutboxConfig) { config.Owner = "" }},
		{name: "invalid image", ccmax: database, runtime: repository, configure: func(config *RuntimeOutboxConfig) { config.Defaults.ImageDigest = "latest" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			if test.configure != nil {
				test.configure(&config)
			}
			if _, err := NewRuntimeOutboxRunner(test.ccmax, test.runtime, config); !errors.Is(err, ErrRuntimeOutboxComposition) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
