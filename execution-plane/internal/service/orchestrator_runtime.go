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
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/reconcile"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	_ "github.com/go-sql-driver/mysql"
)

const orchestratorDatabaseStartupTimeout = 10 * time.Second

var ErrProductionOrchestrator = errors.New("production orchestrator runtime failed")

type orchestratorRuntimeFactories struct {
	openDatabase          func(string) (*sql.DB, error)
	verifySchema          func(context.Context, *sql.DB) error
	verifyCCMAXSchema     func(context.Context, *sql.DB) error
	ensureCCMAXCheckpoint func(context.Context, *sql.DB, string) error
	newRuntimeOutbox      func(*sql.DB, *store.Repository, RuntimeOutboxConfig) (orchestratorRunner, error)
	newRoutePublisher     func(*store.Repository, config.OrchestratorRuntimeConfig) (orchestratorRunner, error)
	newKMS                func(credential.TencentKMSConfig) (credential.KMS, error)
	loadPKI               func(OrchestratorPKIConfig) (*pki.Authority, *tls.Config, error)
	loadRecipient         func(context.Context, credential.KMS, string) (*credential.Recipient, error)
	listen                func(string, string) (net.Listener, error)
	runRPC                func(context.Context, net.Listener, *tls.Config, *OrchestratorComponents) error
	runHealth             func(context.Context, config.Config, *slog.Logger) error
}

func defaultOrchestratorRuntimeFactories() orchestratorRuntimeFactories {
	return orchestratorRuntimeFactories{
		openDatabase:          func(dsn string) (*sql.DB, error) { return sql.Open("mysql", dsn) },
		verifySchema:          store.VerifyRuntimeSchema,
		verifyCCMAXSchema:     reconcile.VerifyCCMAXRuntimeSchema,
		ensureCCMAXCheckpoint: reconcile.EnsureCCMAXRuntimeConsumerCheckpoint,
		newRuntimeOutbox: func(database *sql.DB, repository *store.Repository, runtimeConfig RuntimeOutboxConfig) (orchestratorRunner, error) {
			return NewRuntimeOutboxRunner(database, repository, runtimeConfig)
		},
		newRoutePublisher: func(repository *store.Repository, runtimeConfig config.OrchestratorRuntimeConfig) (orchestratorRunner, error) {
			return NewRoutePublisherRunner(repository, runtimeConfig, func(error) {
				slog.Error("execution route publication failed")
			})
		},
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
	configureOrchestratorDatabase(database)
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
	ccmaxDatabase, err := factories.openDatabase(runtimeConfig.CCMAXMySQLDSN)
	if err != nil || ccmaxDatabase == nil {
		return productionOrchestratorStageError("ccmax database open")
	}
	defer ccmaxDatabase.Close()
	configureOrchestratorDatabase(ccmaxDatabase)
	ccmaxContext, cancelCCMAX := context.WithTimeout(ctx, orchestratorDatabaseStartupTimeout)
	if err := ccmaxDatabase.PingContext(ccmaxContext); err != nil {
		cancelCCMAX()
		return productionOrchestratorStageError("ccmax database ping")
	}
	if err := factories.verifyCCMAXSchema(ccmaxContext, ccmaxDatabase); err != nil {
		cancelCCMAX()
		return productionOrchestratorStageError("ccmax database schema")
	}
	if err := factories.ensureCCMAXCheckpoint(ccmaxContext, ccmaxDatabase, runtimeConfig.RuntimeOutboxConsumerName); err != nil {
		cancelCCMAX()
		return productionOrchestratorStageError("ccmax runtime checkpoint")
	}
	cancelCCMAX()
	repository, err := store.NewRepository(database)
	if err != nil {
		return productionOrchestratorStageError("database repository")
	}
	runtimeOutbox, err := factories.newRuntimeOutbox(ccmaxDatabase, repository, RuntimeOutboxConfig{
		ConsumerName:     runtimeConfig.RuntimeOutboxConsumerName,
		Owner:            runtimeConfig.CoordinatorInstanceID,
		LeaseTTL:         runtimeConfig.RuntimeOutboxLeaseTTL,
		PollInterval:     runtimeConfig.RuntimeOutboxPollInterval,
		BatchSize:        runtimeConfig.RuntimeOutboxBatchSize,
		MaxRetryFailures: runtimeConfig.RuntimeOutboxMaxRetryFailures,
		Defaults: reconcile.CCMAXRuntimeDefaults{
			RequiredLabels:     runtimeConfig.WorkerRequiredLabels,
			ImageDigest:        runtimeConfig.WorkerImageDigest,
			CPURequestMillis:   runtimeConfig.WorkerCPURequestMillis,
			MemoryRequestBytes: runtimeConfig.WorkerMemoryRequestBytes,
		},
		OnError: func(error) { logger.Error("ordered CCMAX runtime outbox iteration failed") },
		OnBlocked: func(blocked outbox.BlockedError) {
			logger.Error("ordered CCMAX runtime outbox checkpoint blocked",
				"consumer_name", blocked.ConsumerName,
				"sequence", blocked.Sequence,
				"failure_class", blocked.Class,
				"failure_code", blocked.Code,
				"blocked_claim_version", blocked.BlockedClaimVersion,
			)
		},
		OnRetryBudgetExceeded: func(exhausted outbox.RetryBudgetError) {
			logger.Error("ordered CCMAX runtime outbox retry budget exhausted",
				"consumer_name", exhausted.ConsumerName,
				"sequence", exhausted.Sequence,
				"failure_code", exhausted.Code,
				"failure_count", exhausted.FailureCount,
			)
		},
	})
	if err != nil || runtimeOutbox == nil {
		return productionOrchestratorStageError("runtime outbox composition")
	}
	routePublisher, err := factories.newRoutePublisher(repository, runtimeConfig)
	if err != nil || routePublisher == nil {
		return productionOrchestratorStageError("route publication")
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
		StartTriggerRepository: repository, HealthyStartRepository: repository,
		Authority: authority, KMS: kms, RotationRecipient: recipient,
		IntentTTL: runtimeConfig.IntentTTL, IntentClaimTTL: runtimeConfig.IntentClaimTTL,
		IntakeServiceID: runtimeConfig.IntakeServiceID,
		RunnerConfig: ProvisioningRunnerConfig{
			PollInterval: runtimeConfig.ProvisioningPollInterval, BatchSize: runtimeConfig.ProvisioningBatchSize,
			OnError: func(workflowID string, _ error) {
				logger.Error("onboarding provisioning iteration failed", "workflow_id", workflowID)
			},
		},
		HealthyStarterConfig: HealthySlotOnboardingStarterConfig{
			ObservationMaxAge: runtimeConfig.OnboardingStartObservationMaxAge,
			CommandTTL:        runtimeConfig.OnboardingStartCommandTTL,
		},
		StartCoordinatorConfig: OnboardingStartCoordinatorConfig{
			Owner:        runtimeConfig.CoordinatorInstanceID,
			PollInterval: runtimeConfig.OnboardingStartPollInterval,
			BatchSize:    runtimeConfig.OnboardingStartBatchSize,
			ClaimTTL:     runtimeConfig.OnboardingStartClaimTTL,
			RetryDelay:   runtimeConfig.OnboardingStartRetryDelay,
			OnError: func(eventID string, _ error) {
				logger.Error("onboarding start trigger iteration failed", "event_id", eventID)
			},
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
		"runtime_outbox_consumer", runtimeConfig.RuntimeOutboxConsumerName,
		"coordinator_instance", runtimeConfig.CoordinatorInstanceID,
	)

	runtimeContext, cancelRuntime := context.WithCancel(ctx)
	defer cancelRuntime()
	type runtimeResult struct {
		component string
		err       error
	}
	results := make(chan runtimeResult, 4)
	go func() {
		results <- runtimeResult{component: "rpc", err: factories.runRPC(runtimeContext, listener, tlsConfig, components)}
	}()
	go func() {
		results <- runtimeResult{component: "health", err: factories.runHealth(runtimeContext, healthConfig, logger)}
	}()
	go func() {
		results <- runtimeResult{component: "runtime outbox", err: runtimeOutbox.Run(runtimeContext)}
	}()
	go func() {
		results <- runtimeResult{component: "route publication", err: routePublisher.Run(runtimeContext)}
	}()
	first := <-results
	cancelRuntime()
	second := <-results
	third := <-results
	fourth := <-results
	if ctx.Err() != nil && first.err == nil && second.err == nil && third.err == nil && fourth.err == nil {
		return nil
	}
	logger.Error("production orchestrator component stopped", "component", first.component)
	return productionOrchestratorStageError(first.component)
}

func productionOrchestratorStageError(stage string) error {
	return fmt.Errorf("%w: %s", ErrProductionOrchestrator, stage)
}

func validateOrchestratorRuntimeFactories(factories orchestratorRuntimeFactories) error {
	if factories.openDatabase == nil || factories.verifySchema == nil || factories.verifyCCMAXSchema == nil ||
		factories.ensureCCMAXCheckpoint == nil || factories.newRuntimeOutbox == nil || factories.newRoutePublisher == nil ||
		factories.newKMS == nil || factories.loadPKI == nil ||
		factories.loadRecipient == nil || factories.listen == nil || factories.runRPC == nil || factories.runHealth == nil {
		return ErrProductionOrchestrator
	}
	return nil
}

func configureOrchestratorDatabase(database *sql.DB) {
	database.SetMaxOpenConns(25)
	database.SetMaxIdleConns(10)
	database.SetConnMaxLifetime(5 * time.Minute)
}
