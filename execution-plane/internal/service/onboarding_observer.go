package service

import (
	"context"
	"errors"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

var ErrProvisioningCommandObservation = errors.New("provisioning command observation rejected")

// ProvisioningCommandObserver is the durable NodeControl handoff. It records
// only command identity, public process key and bounded result state; the
// polling provisioning controller performs KMS decrypt/dispatch/completion.
type ProvisioningCommandObserver struct {
	repository onboarding.ProvisioningRepository
	now        func() time.Time
}

func NewProvisioningCommandObserver(repository onboarding.ProvisioningRepository, now func() time.Time) (*ProvisioningCommandObserver, error) {
	if repository == nil {
		return nil, ErrProvisioningCommandObservation
	}
	if now == nil {
		now = time.Now
	}
	return &ProvisioningCommandObserver{repository: repository, now: now}, nil
}

func (o *ProvisioningCommandObserver) ObserveCommandResult(ctx context.Context, nodeID string, result *executionv1.CommandResult) error {
	if o == nil || o.repository == nil || o.now == nil || ctx == nil || ctx.Err() != nil || result == nil || result.GetSlot() == nil {
		return ErrProvisioningCommandObservation
	}
	errorCode := result.GetErrorCode()
	if !result.GetSucceeded() && errorCode == "" {
		errorCode = "node_command_failed"
	}
	observation := onboarding.ProvisioningCommandObservation{
		CommandID: result.GetCommandId(), NodeID: nodeID, SlotID: result.GetSlot().GetSlotId(),
		ExecutionEpoch: result.GetSlot().GetExecutionEpoch(), ImageDigest: result.GetSlot().GetImageDigest(),
		Healthy: result.GetSlot().GetHealthy(), Succeeded: result.GetSucceeded(), ErrorCode: errorCode,
		ReceivedAt: o.now().UTC(),
	}
	if key := result.GetCredentialTransportKey(); key != nil {
		observation.KeyID = key.GetKeyId()
		observation.KeyPublicKey = append([]byte(nil), key.GetPublicKey()...)
	}
	defer observation.Destroy()
	stored, err := o.repository.ObserveProvisioningCommand(ctx, observation)
	stored.Destroy()
	if err != nil {
		return ErrProvisioningCommandObservation
	}
	return nil
}
