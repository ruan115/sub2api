package service

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	_ "github.com/go-sql-driver/mysql"
)

const orchestratorDatabaseStartupTimeout = 10 * time.Second

var ErrProductionOrchestrator = errors.New("production orchestrator runtime failed")

type orchestratorRuntimeFactories struct {
	openDatabase  func(string) (*sql.DB, error)
	verifySchema  func(context.Context, *sql.DB) error
	newKMS        func(credential.TencentKMSConfig) (credential.KMS, error)
	loadPKI       func(OrchestratorPKIConfig) (*pki.Authority, *tls.Config, error)
	loadRecipient func(context.Context, credential.KMS, string) (*credential.Recipient, error)
	listen        func(string, string) (net.Listener, error)
	runRPC        func(context.Context, net.Listener, *tls.Config, *OrchestratorComponents) error
	runHealth     func(context.Context, config.Config, *slog.Logger) error
}

func defaultOrchestratorRuntimeFactories() orchestratorRuntimeFactories {
	return orchestratorRuntimeFactories{
		openDatabase: func(dsn string) (*sql.DB, error) { return sql.Open("mysql", dsn) },
		verifySchema: store.VerifyRuntimeSchema,
		newKMS: func(runtimeConfig credential.TencentKMSConfig) (credential.KMS, error) {
			return credential.NewTencentKMSFromCVMRole(runtimeConfig)
		},
		loadPKI:       LoadOrchestratorPKI,
		loadRecipient: LoadRotationRecipient,
		listen:        net.Listen,
		runRPC:        RunOrchestratorRPC,
		runHealth:     Run,
	}
}

// RunProductionOrchestrator constructs the complete credential trust boundary
// before either health readiness or credential RPC starts listening. It never
// applies schema migrations and never falls back to ephemeral keys or local
// cloud credentials.
func RunProductionOrchestrator(
	ctx context.Context,
	healthConfig config.Config,
	runtimeConfig config.OrchestratorRuntimeConfig,
	logger *slog.Logger,
) error {
	return runProductionOrchestrator(ctx, healthConfig, runtimeConfig, logger, defaultOrchestratorRuntimeFactories())
}

func runProductionOrchestrator(
	ctx context.Context,
	healthConfig config.Config,
	runtimeConfig config.OrchestratorRuntimeConfig,
	logger *slog.Logger,
	factories orchestratorRuntimeFactories,
) error {
	if ctx == nil || ctx.Err() != nil || healthConfig.Validate() != nil || healthConfig.Role != config.RoleOrchestrator ||
		!runtimeConfig.Enabled || runtimeConfig.Validate() != nil || validateOrchestratorRuntimeFactories(factories) != nil {
		return productionOrchestratorStageError("configuration")
	}
	if logger == nil {
		logger = slog.Default()
	}
	database, err := factories.openDatabase(runtimeConfig.MySQLDSN)
	if err != nil || database == nil {
		return productionOrchestratorStageError("database open")
	}
	defer database.Close()
	database.SetMaxOpenConns(25)
	database.SetMaxIdleConns(10)
	database.SetConnMaxLifetime(5 * time.Minute)
	databaseContext, cancelDatabase := context.WithTimeout(ctx, orchestratorDatabaseStartupTimeout)
	if err := database.PingContext(databaseContext); err != nil {
		cancelDatabase()
		return productionOrchestratorStageError("database ping")
	}
	if err := factories.verifySchema(databaseContext, database); err != nil {
		cancelDatabase()
		return productionOrchestratorStageError("database schema")
	}
	cancelDatabase()
	repository, err := store.NewRepository(database)
	if err != nil {
		return productionOrchestratorStageError("database repository")
	}
	kms, err := factories.newKMS(runtimeConfig.KMS)
	if err != nil || kms == nil {
		return productionOrchestratorStageError("kms")
	}
	authority, tlsConfig, err := factories.loadPKI(OrchestratorPKIConfig{
		CACertificateFile: runtimeConfig.CACertificateFile, CAPrivateKeyFile: runtimeConfig.CAPrivateKeyFile,
		ServerCertificateFile: runtimeConfig.ServerCertificateFile, ServerPrivateKeyFile: runtimeConfig.ServerPrivateKeyFile,
		ServerName: runtimeConfig.ServerName, CertificateTTL: runtimeConfig.CertificateTTL,
	})
	if err != nil || authority == nil || validateOrchestratorTLS(tlsConfig) != nil {
		return productionOrchestratorStageError("pki")
	}
	recipientContext, cancelRecipient := context.WithTimeout(ctx, orchestratorDatabaseStartupTimeout)
	recipient, err := factories.loadRecipient(recipientContext, kms, runtimeConfig.RotationRecipientEnvelopeFile)
	cancelRecipient()
	if err != nil || recipient == nil {
		return productionOrchestratorStageError("rotation recipient")
	}
	recipientOwned := true
	defer func() {
		if recipientOwned {
			recipient.Destroy()
		}
	}()
	components, err := NewOrchestratorComponents(OrchestratorComponentsConfig{
		NodeRepository: repository, CredentialRepository: repository,
		IntentRepository: repository, ProvisioningRepository: repository,
		Authority: authority, KMS: kms, RotationRecipient: recipient,
		IntentTTL: runtimeConfig.IntentTTL, IntentClaimTTL: runtimeConfig.IntentClaimTTL,
		IntakeServiceID: runtimeConfig.IntakeServiceID,
		RunnerConfig: ProvisioningRunnerConfig{
			PollInterval: runtimeConfig.ProvisioningPollInterval, BatchSize: runtimeConfig.ProvisioningBatchSize,
		},
	})
	if err != nil {
		return productionOrchestratorStageError("component composition")
	}
	recipientOwned = false
	defer components.Close()
	listener, err := factories.listen("tcp", runtimeConfig.RPCListenAddress)
	if err != nil || listener == nil {
		return productionOrchestratorStageError("rpc listener")
	}
	defer listener.Close()
	logger.Info("production orchestrator dependencies ready",
		"rpc_address", runtimeConfig.RPCListenAddress,
		"health_address", healthConfig.ListenAddress,
		"intake_service_id", runtimeConfig.IntakeServiceID,
	)

	runtimeContext, cancelRuntime := context.WithCancel(ctx)
	defer cancelRuntime()
	type runtimeResult struct {
		component string
		err       error
	}
	results := make(chan runtimeResult, 2)
	go func() {
		results <- runtimeResult{component: "rpc", err: factories.runRPC(runtimeContext, listener, tlsConfig, components)}
	}()
	go func() {
		results <- runtimeResult{component: "health", err: factories.runHealth(runtimeContext, healthConfig, logger)}
	}()
	first := <-results
	cancelRuntime()
	second := <-results
	if ctx.Err() != nil && first.err == nil && second.err == nil {
		return nil
	}
	logger.Error("production orchestrator component stopped", "component", first.component)
	return productionOrchestratorStageError(first.component)
}

func productionOrchestratorStageError(stage string) error {
	return fmt.Errorf("%w: %s", ErrProductionOrchestrator, stage)
}

func validateOrchestratorRuntimeFactories(factories orchestratorRuntimeFactories) error {
	if factories.openDatabase == nil || factories.verifySchema == nil || factories.newKMS == nil || factories.loadPKI == nil ||
		factories.loadRecipient == nil || factories.listen == nil || factories.runRPC == nil || factories.runHealth == nil {
		return ErrProductionOrchestrator
	}
	return nil
}
