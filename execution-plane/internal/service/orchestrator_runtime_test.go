package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/control"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeenrollment/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestRunProductionOrchestratorBuildsEverythingBeforeServing(t *testing.T) {
	for _, mode := range []string{"off", "on", "on 1h", "on 48h", "factory error", "factory nil", "config nil", "invalid dependencies", "cancel after factory", "kms failure", "listener failure"} {
		t.Run(mode, func(t *testing.T) { testProductionOrchestratorEnrollmentLifecycle(t, mode) })
	}
}

type testRuntimeEnrollmentDependencies struct {
	config *control.RuntimeEnrollmentConfig
	closes atomic.Int32
}

func (d *testRuntimeEnrollmentDependencies) ControlConfig() *control.RuntimeEnrollmentConfig {
	return d.config
}
func (d *testRuntimeEnrollmentDependencies) Close() error { d.closes.Add(1); return nil }

type rejectEnrollmentLease struct{}

func (rejectEnrollmentLease) Validate(context.Context, lease.Claim) error {
	return lease.ErrLeaseNotCurrent
}

func testProductionOrchestratorEnrollmentLifecycle(t *testing.T, mode string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var enrollmentCalls atomic.Int32
	enrollmentResource := &testRuntimeEnrollmentDependencies{}
	database, runtimeMock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	runtimeMock.ExpectPing()
	ccmaxDatabase, ccmaxMock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	ccmaxMock.ExpectPing()
	now := time.Now().UTC()
	certificateTTL := 24 * time.Hour
	if mode == "on 1h" {
		certificateTTL = time.Hour
	}
	if mode == "on 48h" {
		certificateTTL = 48 * time.Hour
	}
	authority, _, err := pki.NewEphemeralAuthority(func() time.Time { return now }, certificateTTL)
	if err != nil {
		t.Fatal(err)
	}
	serverCertificate, _, err := authority.IssueServer([]string{"orchestrator.test"})
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverCertificate},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: authority.CertificatePool(),
	}
	kms, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x65}, 32), "kms-runtime", "v1")
	if err != nil {
		t.Fatal(err)
	}
	recipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x55}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	listener := bufconn.Listen(1 << 20)
	t.Cleanup(func() { listener.Close(); recipient.Destroy() })
	started := make(chan string, 4)
	factories := orchestratorRuntimeFactories{
		openDatabase: func(dsn string) (*sql.DB, error) {
			if dsn == validProductionOrchestratorConfig().CCMAXMySQLDSN {
				return ccmaxDatabase, nil
			}
			return database, nil
		},
		verifySchema:          func(context.Context, *sql.DB) error { return nil },
		verifyCCMAXSchema:     func(context.Context, *sql.DB) error { return nil },
		ensureCCMAXCheckpoint: func(context.Context, *sql.DB, string) error { return nil },
		newRuntimeOutbox: func(_ *sql.DB, _ *store.Repository, _ RuntimeOutboxConfig) (orchestratorRunner, error) {
			return orchestratorTestRunner{run: func(ctx context.Context) error {
				started <- "outbox"
				<-ctx.Done()
				return nil
			}}, nil
		},
		newRoutePublisher: func(_ *store.Repository, _ config.OrchestratorRuntimeConfig) (orchestratorRunner, error) {
			return orchestratorTestRunner{run: func(ctx context.Context) error {
				started <- "routes"
				<-ctx.Done()
				return nil
			}}, nil
		},
		newRuntimeEnrollment: func(ctx context.Context, c config.RuntimeEnrollmentConfig, db *sql.DB, repository *store.Repository) (runtimeEnrollmentDependencies, error) {
			enrollmentCalls.Add(1)
			if mode == "off" {
				t.Error("disabled enrollment factory called")
			}
			if !c.Enabled || c.LeaseRedisAddr != "127.0.0.1:6380" || db != database || repository == nil {
				t.Error("wrong enrollment dependencies")
			}
			if mode == "factory nil" {
				return nil, nil
			}
			receipts, err := storage.NewSQL(db)
			if err != nil {
				t.Fatal(err)
			}
			enrollmentResource.config = &control.RuntimeEnrollmentConfig{Bindings: repository, Receipts: receipts, Leases: rejectEnrollmentLease{}, Timeout: c.Timeout}
			if mode == "config nil" {
				enrollmentResource.config = nil
			}
			if mode == "invalid dependencies" {
				enrollmentResource.config.Bindings = nil
			}
			if mode == "cancel after factory" {
				cancel()
			}
			if mode == "factory error" {
				return enrollmentResource, errors.New("private-redis-secret")
			}
			return enrollmentResource, nil
		},
		newKMS: func(credential.TencentKMSConfig) (credential.KMS, error) {
			if mode == "kms failure" {
				return nil, errors.New("private-cloud-secret")
			}
			return kms, nil
		},
		loadPKI: func(c OrchestratorPKIConfig) (*pki.Authority, *tls.Config, error) {
			if c.CertificateTTL != certificateTTL {
				t.Error("authority TTL configuration mismatch")
			}
			return authority, tlsConfig, nil
		},
		loadRecipient: func(context.Context, credential.KMS, string) (*credential.Recipient, error) {
			return recipient, nil
		},
		listen: func(network, address string) (net.Listener, error) {
			if mode == "listener failure" {
				return nil, errors.New("private-listener-secret")
			}
			if network != "tcp" || address != "127.0.0.1:8094" {
				t.Fatalf("listen = %q/%q", network, address)
			}
			return listener, nil
		},
		runRPC: func(ctx context.Context, _ net.Listener, _ *tls.Config, components *OrchestratorComponents) error {
			if components == nil || components.CredentialSink == nil || components.StartCoordinator == nil || components.HealthyStarter == nil {
				t.Error("RPC started without complete components")
			}
			// An unauthenticated call distinguishes enabled configuration from
			// a silently overwritten/default-disabled broker without touching SQL.
			_, enrollmentErr := components.Control.EnrollRuntimeCertificate(ctx, nil)
			expectedCode := codes.Unauthenticated
			if mode == "off" {
				expectedCode = codes.Unimplemented
			}
			if status.Code(enrollmentErr) != expectedCode {
				t.Errorf("enrollment config lost: %v", enrollmentErr)
			}
			started <- "rpc"
			<-ctx.Done()
			return nil
		},
		runHealth: func(ctx context.Context, _ config.Config, _ *slog.Logger) error {
			started <- "health"
			<-ctx.Done()
			return nil
		},
	}
	healthConfig := config.Default(config.RoleOrchestrator)
	runtimeConfig := validProductionOrchestratorConfig()
	runtimeConfig.CertificateTTL = certificateTTL
	if mode != "off" {
		runtimeConfig.RuntimeEnrollment = config.RuntimeEnrollmentConfig{Enabled: true, LeaseRedisAddr: "127.0.0.1:6380", Timeout: time.Second}
	}
	if !strings.HasPrefix(mode, "on") && mode != "off" {
		err := runProductionOrchestrator(ctx, healthConfig, runtimeConfig, slog.New(slog.NewTextHandler(io.Discard, nil)), factories)
		if !errors.Is(err, ErrProductionOrchestrator) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("failed dependency accepted or leaked: %v", err)
		}
		expectedCloses := int32(1)
		if mode == "factory nil" {
			expectedCloses = 0
		}
		if enrollmentCalls.Load() != 1 || enrollmentResource.closes.Load() != expectedCloses {
			t.Fatalf("failed startup leaked enrollment dependencies: calls %d closes %d", enrollmentCalls.Load(), enrollmentResource.closes.Load())
		}
		select {
		case started := <-started:
			t.Fatalf("listener/runtime started on failure: %s", started)
		default:
		}
		if err := runtimeMock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		if err := ccmaxMock.ExpectationsWereMet(); err != nil {
			t.Error(err)
		}
		return
	}
	result := make(chan error, 1)
	go func() {
		result <- runProductionOrchestrator(
			ctx, healthConfig, runtimeConfig, slog.New(slog.NewTextHandler(io.Discard, nil)), factories,
		)
	}()
	seen := map[string]bool{}
	for len(seen) < 4 {
		select {
		case component := <-started:
			seen[component] = true
		case <-time.After(5 * time.Second):
			t.Fatal("production runtime did not start all services")
		}
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("production runtime did not stop")
	}
	if _, _, err := recipient.PublicKey(); err == nil {
		t.Fatal("production runtime did not destroy rotation recipient")
	}
	expectedCalls := int32(1)
	if mode == "off" {
		expectedCalls = 0
	}
	if enrollmentCalls.Load() != expectedCalls || enrollmentResource.closes.Load() != expectedCalls {
		t.Fatal("enrollment opt-in or cleanup lifecycle mismatch")
	}
	if err := runtimeMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := ccmaxMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRunProductionOrchestratorFailsBeforeCloudAndListenersOnSchemaError(t *testing.T) {
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing()
	var cloudCalls atomic.Int32
	factories := defaultOrchestratorRuntimeFactories()
	factories.openDatabase = func(string) (*sql.DB, error) { return database, nil }
	factories.verifySchema = func(context.Context, *sql.DB) error { return errors.New("schema missing") }
	factories.newKMS = func(credential.TencentKMSConfig) (credential.KMS, error) {
		cloudCalls.Add(1)
		return nil, errors.New("must not be called")
	}
	if err := runProductionOrchestrator(
		context.Background(), config.Default(config.RoleOrchestrator), validProductionOrchestratorConfig(), nil, factories,
	); !errors.Is(err, ErrProductionOrchestrator) {
		t.Fatalf("schema failure = %v", err)
	}
	if cloudCalls.Load() != 0 {
		t.Fatalf("schema failure contacted cloud %d times", cloudCalls.Load())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOrchestratorRuntimeEnrollmentControlDefaultsMatchAuthorityTTL(t *testing.T) {
	for _, test := range []struct{ ttl, rotate time.Duration }{
		{48 * time.Hour, 6 * time.Hour}, {24 * time.Hour, 6 * time.Hour}, {time.Hour, 15 * time.Minute}, {time.Nanosecond, time.Nanosecond},
	} {
		c := validProductionOrchestratorConfig()
		c.CertificateTTL = test.ttl
		c.RuntimeEnrollment.Enabled = true
		got := orchestratorControlConfig(c)
		if got.CertificateTTL != test.ttl || got.RotateBefore != test.rotate || got.EnrollmentTTL == 0 {
			t.Fatalf("incorrect control defaults for %s", test.ttl)
		}
		c.RuntimeEnrollment.Enabled = false
		legacy := orchestratorControlConfig(c)
		if legacy.CertificateTTL != 24*time.Hour || legacy.RotateBefore != 6*time.Hour {
			t.Fatal("disabled path changed legacy defaults")
		}
	}
}

func TestRunProductionOrchestratorMissingEnrollmentFactoryFailsBeforeIO(t *testing.T) {
	c := validProductionOrchestratorConfig()
	c.RuntimeEnrollment = config.RuntimeEnrollmentConfig{Enabled: true, LeaseRedisAddr: "127.0.0.1:6380", Timeout: time.Second}
	factories := defaultOrchestratorRuntimeFactories()
	factories.newRuntimeEnrollment = nil
	factories.openDatabase = func(string) (*sql.DB, error) {
		t.Fatal("opened database with missing enrollment factory")
		return nil, nil
	}
	if err := runProductionOrchestrator(context.Background(), config.Default(config.RoleOrchestrator), c, nil, factories); !errors.Is(err, ErrProductionOrchestrator) {
		t.Fatalf("missing factory accepted: %v", err)
	}
}

func TestRunProductionOrchestratorFailsBeforeCloudOnCCMAXBoundaryError(t *testing.T) {
	for _, stage := range []string{"schema", "checkpoint", "outbox"} {
		t.Run(stage, func(t *testing.T) {
			runtimeDatabase, runtimeMock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			runtimeMock.ExpectPing()
			ccmaxDatabase, ccmaxMock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
			if err != nil {
				t.Fatal(err)
			}
			ccmaxMock.ExpectPing()
			var cloudCalls atomic.Int32
			factories := defaultOrchestratorRuntimeFactories()
			factories.openDatabase = func(dsn string) (*sql.DB, error) {
				if dsn == validProductionOrchestratorConfig().CCMAXMySQLDSN {
					return ccmaxDatabase, nil
				}
				return runtimeDatabase, nil
			}
			factories.verifySchema = func(context.Context, *sql.DB) error { return nil }
			factories.verifyCCMAXSchema = func(context.Context, *sql.DB) error {
				if stage == "schema" {
					return errors.New("ccmax schema missing")
				}
				return nil
			}
			factories.ensureCCMAXCheckpoint = func(context.Context, *sql.DB, string) error {
				if stage == "checkpoint" {
					return errors.New("checkpoint needs bootstrap")
				}
				return nil
			}
			factories.newRuntimeOutbox = func(*sql.DB, *store.Repository, RuntimeOutboxConfig) (orchestratorRunner, error) {
				return nil, errors.New("outbox composition failed")
			}
			factories.newKMS = func(credential.TencentKMSConfig) (credential.KMS, error) {
				cloudCalls.Add(1)
				return nil, errors.New("must not be called")
			}
			if err := runProductionOrchestrator(
				context.Background(), config.Default(config.RoleOrchestrator), validProductionOrchestratorConfig(), nil, factories,
			); !errors.Is(err, ErrProductionOrchestrator) {
				t.Fatalf("CCMAX %s failure = %v", stage, err)
			}
			if cloudCalls.Load() != 0 {
				t.Fatalf("CCMAX %s failure contacted cloud %d times", stage, cloudCalls.Load())
			}
			if err := runtimeMock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
			if err := ccmaxMock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func validProductionOrchestratorConfig() config.OrchestratorRuntimeConfig {
	return config.OrchestratorRuntimeConfig{
		Enabled: true, RPCListenAddress: "127.0.0.1:8094",
		MySQLDSN:                  "runtime:secret@tcp(mysql.internal:3306)/worker_runtime?parseTime=true&loc=UTC&tls=true",
		CCMAXMySQLDSN:             "ccmax:secret@tcp(mysql.internal:3306)/ccmax?parseTime=true&loc=UTC&tls=true",
		CoordinatorInstanceID:     "orchestrator-srv74-1",
		RuntimeOutboxConsumerName: "sub2api-execution-runtime-v1",
		RuntimeOutboxLeaseTTL:     30 * time.Second, RuntimeOutboxPollInterval: time.Second,
		RuntimeOutboxBatchSize: 200, RuntimeOutboxMaxRetryFailures: 5,
		WorkerImageDigest: "sha256:" + strings.Repeat("a", 64), WorkerRequiredLabels: map[string]string{},
		WorkerCPURequestMillis: 500, WorkerMemoryRequestBytes: 128 << 20,
		CACertificateFile: "/runtime/ca.crt", CAPrivateKeyFile: "/runtime/ca.key",
		ServerCertificateFile: "/runtime/server.crt", ServerPrivateKeyFile: "/runtime/server.key",
		ServerName: "orchestrator.test", RotationRecipientEnvelopeFile: "/runtime/rotation.json",
		IntakeServiceID: "ccmax", CertificateTTL: 24 * time.Hour,
		IntentTTL: 30 * time.Minute, IntentClaimTTL: 5 * time.Minute,
		ProvisioningPollInterval: time.Second, ProvisioningBatchSize: 100,
		OnboardingStartObservationMaxAge: 45 * time.Second, OnboardingStartCommandTTL: 2 * time.Minute,
		OnboardingStartPollInterval: time.Second, OnboardingStartBatchSize: 200,
		OnboardingStartClaimTTL: 30 * time.Second, OnboardingStartRetryDelay: 2 * time.Second,
		RouteRedisAddr: "127.0.0.1:6379", RoutePublishTTL: 45 * time.Second, RoutePublishInterval: 5 * time.Second,
		KMS: credential.TencentKMSConfig{
			Region: "ap-guangzhou", KeyID: "kms-key", KeyVersion: "v1", CVMRoleName: "orchestrator-role",
		},
	}
}
