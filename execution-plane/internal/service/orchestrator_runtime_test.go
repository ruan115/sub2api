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
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"google.golang.org/grpc/test/bufconn"
)

func TestRunProductionOrchestratorBuildsEverythingBeforeServing(t *testing.T) {
	database, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectPing()
	now := time.Now().UTC()
	authority, _, err := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
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
	started := make(chan string, 2)
	factories := orchestratorRuntimeFactories{
		openDatabase: func(string) (*sql.DB, error) { return database, nil },
		verifySchema: func(context.Context, *sql.DB) error { return nil },
		newKMS:       func(credential.TencentKMSConfig) (credential.KMS, error) { return kms, nil },
		loadPKI: func(OrchestratorPKIConfig) (*pki.Authority, *tls.Config, error) {
			return authority, tlsConfig, nil
		},
		loadRecipient: func(context.Context, credential.KMS, string) (*credential.Recipient, error) {
			return recipient, nil
		},
		listen: func(network, address string) (net.Listener, error) {
			if network != "tcp" || address != "127.0.0.1:8094" {
				t.Fatalf("listen = %q/%q", network, address)
			}
			return listener, nil
		},
		runRPC: func(ctx context.Context, _ net.Listener, _ *tls.Config, components *OrchestratorComponents) error {
			if components == nil || components.CredentialSink == nil {
				t.Error("RPC started without complete components")
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
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- runProductionOrchestrator(
			ctx, healthConfig, runtimeConfig, slog.New(slog.NewTextHandler(io.Discard, nil)), factories,
		)
	}()
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case component := <-started:
			seen[component] = true
		case <-time.After(5 * time.Second):
			t.Fatal("production runtime did not start both services")
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
	if err := mock.ExpectationsWereMet(); err != nil {
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

func validProductionOrchestratorConfig() config.OrchestratorRuntimeConfig {
	return config.OrchestratorRuntimeConfig{
		Enabled: true, RPCListenAddress: "127.0.0.1:8094",
		MySQLDSN:          "runtime:secret@tcp(mysql.internal:3306)/worker_runtime?parseTime=true&loc=UTC&tls=true",
		CACertificateFile: "/runtime/ca.crt", CAPrivateKeyFile: "/runtime/ca.key",
		ServerCertificateFile: "/runtime/server.crt", ServerPrivateKeyFile: "/runtime/server.key",
		ServerName: "orchestrator.test", RotationRecipientEnvelopeFile: "/runtime/rotation.json",
		IntakeServiceID: "ccmax", CertificateTTL: 24 * time.Hour,
		IntentTTL: 30 * time.Minute, IntentClaimTTL: 5 * time.Minute,
		ProvisioningPollInterval: time.Second, ProvisioningBatchSize: 100,
		KMS: credential.TencentKMSConfig{
			Region: "ap-guangzhou", KeyID: "kms-key", KeyVersion: "v1", CVMRoleName: "orchestrator-role",
		},
	}
}
