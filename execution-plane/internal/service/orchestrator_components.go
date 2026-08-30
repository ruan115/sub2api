package service

import (
	"crypto/rand"
	"errors"
	"io"
	"sync"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/control"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"google.golang.org/grpc"
)

const (
	defaultOnboardingIntentTTL = 30 * time.Minute
	defaultOnboardingClaimTTL  = 5 * time.Minute
	defaultIntakeServiceID     = "ccmax"
)

var ErrOrchestratorComponents = errors.New("orchestrator components are invalid")

// OrchestratorComponentsConfig is the composition boundary for the
// orchestrator-only credential path. Production may use one MySQL Repository
// for all four repository interfaces, while tests can keep the stores
// independent. No worker or host-agent process receives KMS or CA authority.
type OrchestratorComponentsConfig struct {
	NodeRepository         store.NodeRepository
	CredentialRepository   credential.IdempotentVaultRepository
	IntentRepository       onboarding.Repository
	ProvisioningRepository onboarding.ActiveProvisioningRepository
	Authority              *pki.Authority
	KMS                    credential.KMS
	RotationAuthorizer     RotationCommitAuthorizer
	RotationRecipient      *credential.Recipient

	ControlConfig         control.Config
	ControllerConfig      SecureOnboardingControllerConfig
	RunnerConfig          ProvisioningRunnerConfig
	CredentialVaultConfig credential.VaultConfig
	IntentTTL             time.Duration
	IntentClaimTTL        time.Duration
	IntakeServiceID       string
	Random                io.Reader
	Now                   func() time.Time
}

// OrchestratorComponents owns every object that must share the same credential
// trust boundary. Register installs both NodeControl and CCMAX onboarding
// intake on one gRPC registrar; callers remain responsible for a TLS 1.3
// listener whose client policy supplies verified chains to method-level auth.
type OrchestratorComponents struct {
	Control              *control.Server
	Intake               *OnboardingIntakeServer
	Provisioning         *SecureOnboardingController
	ProvisioningRunner   *ProvisioningRunner
	ProvisioningObserver *ProvisioningCommandObserver
	CredentialSink       *CredentialRotationSink
	CredentialVault      *credential.Vault
	IntentVault          *onboarding.Vault

	rotationRecipient *credential.Recipient
	lifecycleMu       sync.Mutex
	registered        bool
	closed            bool
}

func NewOrchestratorComponents(config OrchestratorComponentsConfig) (*OrchestratorComponents, error) {
	if config.NodeRepository == nil || config.CredentialRepository == nil || config.IntentRepository == nil ||
		config.ProvisioningRepository == nil || config.Authority == nil || config.KMS == nil || config.RotationRecipient == nil {
		return nil, ErrOrchestratorComponents
	}
	if _, _, err := config.RotationRecipient.PublicKey(); err != nil {
		return nil, ErrOrchestratorComponents
	}
	// A validated recipient transfers ownership immediately. All later failure
	// paths destroy it so a partially assembled trust boundary cannot leak a
	// live private key back to an ambiguous owner.
	rotationRecipient := config.RotationRecipient
	fail := func() (*OrchestratorComponents, error) {
		rotationRecipient.Destroy()
		return nil, ErrOrchestratorComponents
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	rotationAuthorizer := config.RotationAuthorizer
	proxies, proxiesOK := config.ProvisioningRepository.(ProxyLeaseAuthority)
	if !proxiesOK {
		return fail()
	}
	if rotationAuthorizer == nil {
		repository, repositoryOK := config.ProvisioningRepository.(RotationAuthorizationRepository)
		if !repositoryOK {
			return fail()
		}
		builtAuthorizer, authorizerErr := NewDurableRotationAuthorizer(repository, proxies, config.Now)
		if authorizerErr != nil {
			return fail()
		}
		rotationAuthorizer = builtAuthorizer
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.IntentTTL == 0 {
		config.IntentTTL = defaultOnboardingIntentTTL
	}
	if config.IntentClaimTTL == 0 {
		config.IntentClaimTTL = defaultOnboardingClaimTTL
	}
	if config.IntakeServiceID == "" {
		config.IntakeServiceID = defaultIntakeServiceID
	}
	if pki.ValidateServiceID(config.IntakeServiceID) != nil {
		return fail()
	}

	cryptoService, err := credential.NewService(config.KMS)
	if err != nil {
		return fail()
	}
	config.CredentialVaultConfig.Now = config.Now
	if config.CredentialVaultConfig.Random == nil {
		config.CredentialVaultConfig.Random = config.Random
	}
	credentialVault, err := credential.NewVault(cryptoService, config.CredentialRepository, config.CredentialVaultConfig)
	if err != nil {
		return fail()
	}
	intentVault, err := onboarding.NewVault(onboarding.VaultConfig{
		Crypto: cryptoService, Repository: config.IntentRepository, Random: config.Random, Now: config.Now,
		IntentTTL: config.IntentTTL, ClaimTTL: config.IntentClaimTTL,
	})
	if err != nil {
		return fail()
	}

	builder, err := NewSecureOnboardingCommandBuilder(rotationRecipient, config.Random, config.Now)
	if err != nil {
		return fail()
	}
	workflow, err := NewSecureOnboardingWorkflow(intentVault, builder, proxies, config.Now)
	if err != nil {
		return fail()
	}
	observer, err := NewProvisioningCommandObserver(config.ProvisioningRepository, config.Now)
	if err != nil {
		return fail()
	}
	resultRepository, ok := config.ProvisioningRepository.(onboarding.ResultProjectionRepository)
	if !ok {
		return fail()
	}
	sink, err := NewCredentialRotationSink(CredentialRotationSinkConfig{
		Recipient: rotationRecipient, Authorizer: rotationAuthorizer, Vault: credentialVault,
		ResultRepository: resultRepository, Now: config.Now,
	})
	if err != nil {
		return fail()
	}
	if config.ControlConfig.EnrollmentTTL == 0 {
		config.ControlConfig = control.DefaultConfig()
	}
	config.ControlConfig.Now = config.Now
	config.ControlConfig.CommandObserver = observer
	config.ControlConfig.CredentialSink = sink
	controlServer, err := control.NewServer(config.NodeRepository, config.Authority, config.ControlConfig)
	if err != nil {
		return fail()
	}
	intake, err := NewOnboardingIntakeServer(intentVault, resultRepository, MTLSServiceAuthorizer{ExpectedServiceID: config.IntakeServiceID})
	if err != nil {
		return fail()
	}
	if config.ControllerConfig.RetryDelay == 0 {
		config.ControllerConfig = DefaultSecureOnboardingControllerConfig()
	}
	config.ControllerConfig.Now = config.Now
	controller, err := NewSecureOnboardingController(config.ProvisioningRepository, workflow, controlServer, config.ControllerConfig)
	if err != nil {
		return fail()
	}
	config.RunnerConfig.Now = config.Now
	runner, err := NewProvisioningRunner(config.ProvisioningRepository, controller, config.RunnerConfig)
	if err != nil {
		return fail()
	}

	return &OrchestratorComponents{
		Control: controlServer, Intake: intake, Provisioning: controller, ProvisioningRunner: runner, ProvisioningObserver: observer,
		CredentialSink: sink, CredentialVault: credentialVault, IntentVault: intentVault,
		rotationRecipient: rotationRecipient,
	}, nil
}

func (c *OrchestratorComponents) Register(registrar grpc.ServiceRegistrar) error {
	if c == nil || registrar == nil {
		return ErrOrchestratorComponents
	}
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	if c.closed || c.registered || c.Control == nil || c.Intake == nil || c.Provisioning == nil || c.ProvisioningRunner == nil ||
		c.ProvisioningObserver == nil || c.CredentialSink == nil || c.CredentialVault == nil || c.IntentVault == nil ||
		c.rotationRecipient == nil {
		return ErrOrchestratorComponents
	}
	if _, _, err := c.rotationRecipient.PublicKey(); err != nil {
		return ErrOrchestratorComponents
	}
	executionv1.RegisterNodeControlServiceServer(registrar, c.Control)
	executionv1.RegisterOnboardingIntakeServiceServer(registrar, c.Intake)
	c.registered = true
	return nil
}

func (c *OrchestratorComponents) Close() {
	if c == nil {
		return
	}
	c.lifecycleMu.Lock()
	if c.closed {
		c.lifecycleMu.Unlock()
		return
	}
	c.closed = true
	recipient := c.rotationRecipient
	c.lifecycleMu.Unlock()
	if recipient != nil {
		recipient.Destroy()
	}
}
