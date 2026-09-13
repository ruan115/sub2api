package service

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/pki"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"google.golang.org/grpc"
)

type componentRotationAuthorizer struct{}

func (componentRotationAuthorizer) CommitAuthorizedRotation(
	context.Context,
	RotationClaim,
	func(context.Context, string) (string, error),
) (string, error) {
	return "", ErrCredentialRotationRejected
}

type componentDurableProvisioningRepository struct {
	*onboarding.MemoryProvisioningRepository
}

type componentStartTriggerRepository struct {
	claimed chan struct{}
	once    sync.Once
}

func (r *componentStartTriggerRepository) ProjectOnboardingStartTrigger(
	context.Context,
	onboarding.StartTriggerProjection,
) (onboarding.OnboardingStartTrigger, bool, error) {
	return onboarding.OnboardingStartTrigger{}, false, onboarding.ErrStartTriggerRejected
}

func (r *componentStartTriggerRepository) LookupOnboardingStartTrigger(
	context.Context,
	string,
) (onboarding.OnboardingStartTrigger, bool, error) {
	return onboarding.OnboardingStartTrigger{}, false, nil
}

func (r *componentStartTriggerRepository) ClaimDueOnboardingStartTriggers(
	context.Context,
	onboarding.StartTriggerClaimQuery,
) ([]onboarding.OnboardingStartTrigger, error) {
	if r.claimed != nil {
		r.once.Do(func() { close(r.claimed) })
	}
	return nil, nil
}

func (r *componentStartTriggerRepository) RetryOnboardingStartTrigger(
	context.Context,
	onboarding.StartTriggerRetry,
) error {
	return onboarding.ErrStartTriggerRejected
}

type componentHealthyStartRepository struct{}

func (componentHealthyStartRepository) StartHealthySlotOnboarding(
	context.Context,
	onboarding.HealthySlotStartSpec,
) (onboarding.Provisioning, bool, error) {
	return onboarding.Provisioning{}, false, onboarding.ErrHealthySlotStartRejected
}

func (r *componentDurableProvisioningRepository) BeginCredentialRotation(
	context.Context, string, string, uint64, string, string, [32]byte, time.Time,
) (string, string, error) {
	return "", "", ErrCredentialRotationRejected
}

func (r *componentDurableProvisioningRepository) CompleteCredentialRotation(context.Context, string, string, time.Time) error {
	return ErrCredentialRotationRejected
}

func (r *componentDurableProvisioningRepository) ValidateCurrentProxyLease(
	context.Context, string, string, uint64, string, time.Time,
) error {
	return ErrCredentialRotationRejected
}

func TestOrchestratorComponentsComposeAndRegisterCredentialBoundary(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, err := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	kms, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x61}, 32), "kms-components", "v1")
	if err != nil {
		t.Fatal(err)
	}
	runtimeRepository := store.NewMemoryRepository()
	rotationRecipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x51}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	components, err := NewOrchestratorComponents(OrchestratorComponentsConfig{
		NodeRepository: runtimeRepository, CredentialRepository: runtimeRepository,
		IntentRepository: onboarding.NewMemoryRepository(), ProvisioningRepository: &componentDurableProvisioningRepository{MemoryProvisioningRepository: onboarding.NewMemoryProvisioningRepository()},
		Authority: authority, KMS: kms, RotationAuthorizer: componentRotationAuthorizer{}, RotationRecipient: rotationRecipient,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x42}, 4096)), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer components.Close()
	if components.Control == nil || components.Intake == nil || components.Provisioning == nil || components.ProvisioningRunner == nil ||
		components.ProvisioningObserver == nil || components.CredentialSink == nil ||
		components.CredentialVault == nil || components.IntentVault == nil {
		t.Fatal("orchestrator composition left a credential-path dependency nil")
	}
	if components.HealthyStarter != nil || components.StartCoordinator != nil {
		t.Fatal("disabled triggered-start composition unexpectedly created background components")
	}
	server := grpc.NewServer()
	if err := components.Register(server); err != nil {
		t.Fatal(err)
	}
	if err := components.Register(server); !errors.Is(err, ErrOrchestratorComponents) {
		t.Fatalf("duplicate register error = %v", err)
	}
	services := server.GetServiceInfo()
	for _, name := range []string{
		"execution.v1.NodeControlService",
		"execution.v1.OnboardingIntakeService",
	} {
		if _, exists := services[name]; !exists {
			t.Fatalf("gRPC service %q was not registered", name)
		}
	}
}

func TestOrchestratorComponentsBuildsDurableAuthorizerFromProductionRepository(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, _ := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x63}, 32), "kms-components", "v1")
	runtimeRepository := store.NewMemoryRepository()
	provisioning := &componentDurableProvisioningRepository{MemoryProvisioningRepository: onboarding.NewMemoryProvisioningRepository()}
	rotationRecipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x54}, 32)))
	components, err := NewOrchestratorComponents(OrchestratorComponentsConfig{
		NodeRepository: runtimeRepository, CredentialRepository: runtimeRepository,
		IntentRepository: onboarding.NewMemoryRepository(), ProvisioningRepository: provisioning,
		Authority: authority, KMS: kms, RotationRecipient: rotationRecipient,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x44}, 4096)), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer components.Close()
	if _, ok := components.CredentialSink.authorizer.(*DurableRotationAuthorizer); !ok {
		t.Fatalf("auto rotation authorizer type = %T", components.CredentialSink.authorizer)
	}
}

func TestOrchestratorComponentsOptionallyBuildsTriggeredStarter(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	config := validComponentConfig(t, now)
	config.StartTriggerRepository = &componentStartTriggerRepository{}
	config.HealthyStartRepository = componentHealthyStartRepository{}
	config.HealthyStarterConfig = HealthySlotOnboardingStarterConfig{
		ObservationMaxAge: time.Minute,
		CommandTTL:        5 * time.Minute,
	}
	config.StartCoordinatorConfig = OnboardingStartCoordinatorConfig{Owner: "orchestrator-a"}
	components, err := NewOrchestratorComponents(config)
	if err != nil {
		t.Fatal(err)
	}
	defer components.Close()
	if components.HealthyStarter == nil || components.StartCoordinator == nil {
		t.Fatal("enabled triggered-start composition left a dependency nil")
	}
}

func TestOrchestratorComponentsRejectsPartiallyEnabledTriggeredStarter(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	for _, configure := range []func(*OrchestratorComponentsConfig){
		func(config *OrchestratorComponentsConfig) {
			config.StartTriggerRepository = &componentStartTriggerRepository{}
		},
		func(config *OrchestratorComponentsConfig) {
			config.HealthyStartRepository = componentHealthyStartRepository{}
		},
	} {
		config := validComponentConfig(t, now)
		configure(&config)
		if _, err := NewOrchestratorComponents(config); !errors.Is(err, ErrOrchestratorComponents) {
			t.Fatalf("partial triggered-start composition error = %v", err)
		}
		if _, _, err := config.RotationRecipient.PublicKey(); err == nil {
			t.Fatal("failed partial composition retained a live rotation recipient")
		}
	}
}

func TestOrchestratorComponentsFailClosedAndDestroyRecipient(t *testing.T) {
	if _, err := NewOrchestratorComponents(OrchestratorComponentsConfig{}); !errors.Is(err, ErrOrchestratorComponents) {
		t.Fatalf("empty composition error = %v", err)
	}
	if err := (*OrchestratorComponents)(nil).Register(grpc.NewServer()); !errors.Is(err, ErrOrchestratorComponents) {
		t.Fatalf("nil register error = %v", err)
	}

	now := time.Unix(2_000_000_000, 0).UTC()
	authority, _, _ := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	kms, _ := credential.NewFakeKMS(bytes.Repeat([]byte{0x62}, 32), "kms-components", "v1")
	runtimeRepository := store.NewMemoryRepository()
	rotationRecipient, _ := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x52}, 32)))
	components, err := NewOrchestratorComponents(OrchestratorComponentsConfig{
		NodeRepository: runtimeRepository, CredentialRepository: runtimeRepository,
		IntentRepository: onboarding.NewMemoryRepository(), ProvisioningRepository: &componentDurableProvisioningRepository{MemoryProvisioningRepository: onboarding.NewMemoryProvisioningRepository()},
		Authority: authority, KMS: kms, RotationAuthorizer: componentRotationAuthorizer{}, RotationRecipient: rotationRecipient,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x43}, 4096)), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	components.Close()
	components.Close()
	if _, _, err := components.rotationRecipient.PublicKey(); err == nil {
		t.Fatal("closed orchestrator still exposes its rotation public key")
	}
	if err := components.Register(grpc.NewServer()); !errors.Is(err, ErrOrchestratorComponents) {
		t.Fatalf("register after close error = %v", err)
	}
}

func validComponentConfig(t *testing.T, now time.Time) OrchestratorComponentsConfig {
	t.Helper()
	authority, _, err := pki.NewEphemeralAuthority(func() time.Time { return now }, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	kms, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x64}, 32), "kms-components", "v1")
	if err != nil {
		t.Fatal(err)
	}
	runtimeRepository := store.NewMemoryRepository()
	rotationRecipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x55}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	return OrchestratorComponentsConfig{
		NodeRepository: runtimeRepository, CredentialRepository: runtimeRepository,
		IntentRepository: onboarding.NewMemoryRepository(), ProvisioningRepository: &componentDurableProvisioningRepository{MemoryProvisioningRepository: onboarding.NewMemoryProvisioningRepository()},
		Authority: authority, KMS: kms, RotationAuthorizer: componentRotationAuthorizer{}, RotationRecipient: rotationRecipient,
		Random: bytes.NewReader(bytes.Repeat([]byte{0x46}, 4096)), Now: func() time.Time { return now },
	}
}
