package daemon

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

func runtimeTestConfig(t *testing.T) (config.Config, Config) {
	t.Helper()
	health := config.Default(config.RoleHostAgent)
	health.NodeID = "node-1"
	env := map[string]string{
		"RUNTIME_ENABLED": "true", "CONTROL_ADDRESS": "127.0.0.1:8493", "CONTROL_SERVER_NAME": "control.test",
		"DOCKER_SOCKET": "/run/docker.sock", "TRUST_FILE": "/run/host/ca.pem", "NODE_CERT_FILE": "/run/host/node.pem", "NODE_KEY_FILE": "/run/host/node.key",
		"TICKET_PUBLIC_KEY": base64.RawStdEncoding.EncodeToString(make([]byte, 32)), "UPSTREAM_BASE_URL": "https://model.example",
	}
	cfg, err := Load(func(key string) string { return env[strings.TrimPrefix(key, "EXECUTION_HOST_AGENT_")] })
	if err != nil {
		t.Fatal(err)
	}
	return health, cfg
}

type runFunc func(context.Context) error

func (f runFunc) Run(ctx context.Context) error { return f(ctx) }

type drainRecorder struct {
	mu             sync.Mutex
	sealed, waited bool
	err            error
}

func (d *drainRecorder) Seal() { d.mu.Lock(); defer d.mu.Unlock(); d.sealed = true }
func (d *drainRecorder) Wait(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.sealed {
		return errors.New("unsealed")
	}
	d.waited = true
	return d.err
}
func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestLifecycleHealthDoesNotClaimReadiness(t *testing.T) {
	for _, path := range []string{"/healthz", "/readyz"} {
		recorder := httptest.NewRecorder()
		Handler().ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
		want := 200
		if path == "/readyz" {
			want = 503
		}
		if recorder.Code != want || recorder.Header().Get("Cache-Control") != "no-store" || !strings.Contains(recorder.Body.String(), `"production_ready":false`) || strings.Contains(recorder.Body.String(), "control_ready") {
			t.Fatal("dishonest health status")
		}
	}
}

func TestRuntimeRejectsConfigurationBeforeAnyIO(t *testing.T) {
	for _, kind := range []string{"off", "public-health", "dns-health", "zero-port", "wrong-role", "bad-node", "capacity", "heartbeat", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			health, cfg := runtimeTestConfig(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "off":
				cfg.Enabled = false
			case "public-health":
				health.ListenAddress = "0.0.0.0:8092"
			case "dns-health":
				health.ListenAddress = "localhost:8092"
			case "zero-port":
				health.ListenAddress = "127.0.0.1:0"
			case "wrong-role":
				health.Role = config.RoleWorker
			case "bad-node":
				health.NodeID = "Node_1"
			case "capacity":
				health.Limits.MaxSlots = 1025
			case "heartbeat":
				health.Timings.NodeHeartbeat = 2 * time.Minute
			case "canceled":
				cancel()
			}
			ioCalls := 0
			err := run(ctx, health, cfg, quietLogger(), factories{
				prepare: func(context.Context, config.Config, Config) (*components, error) { ioCalls++; return nil, ErrRuntime },
				listen:  func(string, string) (net.Listener, error) { ioCalls++; return nil, ErrRuntime },
			})
			if !errors.Is(err, ErrRuntime) || ioCalls != 0 {
				t.Fatal("configuration reached external dependency")
			}
		})
	}
}

func TestRuntimeSupervisesControlExpiryAndShutdown(t *testing.T) {
	for _, kind := range []string{"cancel", "control-failure", "certificate-expiry", "listen-failure", "drain-failure"} {
		t.Run(kind, func(t *testing.T) {
			health, cfg := runtimeTestConfig(t)
			cfg.ShutdownTimeout = time.Second
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			drain := &drainRecorder{}
			if kind == "drain-failure" {
				drain.err = errors.New("secret-dependency-error")
			}
			started := make(chan struct{})
			closed := 0
			expires := time.Now().Add(time.Minute)
			if kind == "certificate-expiry" {
				expires = time.Now().Add(200 * time.Millisecond)
			}
			parts := &components{commands: drain, expiresAt: expires, close: func() { closed++ }, control: runFunc(func(ctx context.Context) error {
				close(started)
				if kind == "control-failure" {
					return errors.New("secret-remote-error")
				}
				<-ctx.Done()
				return nil
			})}
			listened := make(chan string, 1)
			result := make(chan error, 1)
			go func() {
				result <- run(ctx, health, cfg, quietLogger(), factories{
					prepare: func(context.Context, config.Config, Config) (*components, error) { return parts, nil },
					listen: func(string, string) (net.Listener, error) {
						if kind == "listen-failure" {
							return nil, errors.New("secret-listen-error")
						}
						listener, err := net.Listen("tcp", "127.0.0.1:0")
						if err == nil {
							listened <- listener.Addr().String()
						}
						return listener, err
					},
				})
			}()
			if kind == "cancel" || kind == "drain-failure" {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("control did not start")
				}
				address := <-listened
				client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
				defer client.CloseIdleConnections()
				response, err := client.Get("http://" + address + "/readyz")
				if err != nil {
					t.Fatal(err)
				}
				_ = response.Body.Close()
				if response.StatusCode != 503 {
					t.Fatal("runtime claims business ready")
				}
				cancel()
			}
			var err error
			select {
			case err = <-result:
			case <-time.After(4 * time.Second):
				t.Fatal("runtime did not finish")
			}
			want := error(nil)
			switch kind {
			case "control-failure", "listen-failure":
				want = ErrRuntime
			case "certificate-expiry":
				want = ErrIdentityExpired
			case "drain-failure":
				want = ErrShutdown
			}
			if !errors.Is(err, want) || closed != 1 {
				t.Fatalf("result=%v close=%d", err, closed)
			}
			if kind != "listen-failure" && (!drain.sealed || !drain.waited) {
				t.Fatal("executor not sealed/joined")
			}
		})
	}
}

type startupLogHandler struct{ onLog func() }

func (h startupLogHandler) Enabled(context.Context, slog.Level) bool  { return true }
func (h startupLogHandler) Handle(context.Context, slog.Record) error { h.onLog(); return nil }
func (h startupLogHandler) WithAttrs([]slog.Attr) slog.Handler        { return h }
func (h startupLogHandler) WithGroup(string) slog.Handler             { return h }

func TestRuntimeStartupCancellationRaceIsNotTransportFailure(t *testing.T) {
	for i := 0; i < 20; i++ {
		health, cfg := runtimeTestConfig(t)
		ctx, cancel := context.WithCancel(context.Background())
		returned := make(chan struct{})
		drain := &drainRecorder{}
		parts := &components{commands: drain, expiresAt: time.Now().Add(time.Minute), close: func() {}, control: runFunc(func(ctx context.Context) error { <-ctx.Done(); close(returned); return nil })}
		logger := slog.New(startupLogHandler{onLog: func() {
			cancel()
			select {
			case <-returned:
			case <-time.After(time.Second):
				t.Error("control did not exit during startup log")
			}
		}})
		err := run(ctx, health, cfg, logger, factories{prepare: func(context.Context, config.Config, Config) (*components, error) { return parts, nil }, listen: func(string, string) (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }})
		cancel()
		if err != nil {
			t.Fatalf("canceled startup misclassified: %v", err)
		}
	}
}

func TestRuntimeDoesNotStartAfterCancellationDuringListen(t *testing.T) {
	health, cfg := runtimeTestConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parts := &components{commands: &drainRecorder{}, expiresAt: time.Now().Add(time.Minute), close: func() {}, control: runFunc(func(context.Context) error { t.Error("control started after canceled listen"); return nil })}
	err := run(ctx, health, cfg, quietLogger(), factories{prepare: func(context.Context, config.Config, Config) (*components, error) { return parts, nil }, listen: func(string, string) (net.Listener, error) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		cancel()
		return listener, err
	}})
	if err != nil {
		t.Fatal("listen-time cancellation not graceful")
	}
}
