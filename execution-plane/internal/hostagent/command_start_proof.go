package hostagent

import (
	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

// A proof only prevents raw provider/TCP health from reviving a failed or
// never-authenticated START. It is process-local, not a reusable credential,
// fresh mTLS observation, active lease or permission to execute a request.
type startupProof struct {
	instance  provider.Instance
	accountID string
	image     string
}

func (p startupProof) matches(command *executionv1.SlotCommand) bool {
	return p.instance.SlotID == command.GetSlotId() && p.instance.Epoch == command.GetExecutionEpoch() &&
		p.instance.RuntimeGeneration == commandRuntimeGeneration(command) && p.image == command.GetImageDigest() && p.accountID == command.GetAccountId()
}

func (p startupProof) matchesStatus(status provider.Status) bool {
	i := p.instance
	return status.RuntimeID != "" && i.RuntimeID == status.RuntimeID && i.ProviderRef == status.ProviderRef &&
		i.SlotID == status.SlotID && i.Epoch == status.Epoch && i.RuntimeGeneration == status.RuntimeGeneration && p.image == status.ImageDigest
}

// Both helpers require operationMu, already held for commands and revocation.
func (e *SlotCommandExecutor) forgetStartup(slotID string) {
	delete(e.startupProofs, slotID)
	if e.startup == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if previous := e.observations[slotID]; previous != nil {
		observation := cloneObservation(previous)
		observation.Healthy = false
		observation.Reason = "authenticated_start_required"
		e.observations[slotID] = observation
	}
}

func (e *SlotCommandExecutor) hasStartup(command *executionv1.SlotCommand, status provider.Status) bool {
	proof, ok := e.startupProofs[status.SlotID]
	if !ok || !status.Healthy || e.epochRevoked(status.SlotID, status.Epoch) || !proof.matchesStatus(status) || proof.accountID != command.GetAccountId() {
		e.forgetStartup(status.SlotID)
		return false
	}
	return true
}
