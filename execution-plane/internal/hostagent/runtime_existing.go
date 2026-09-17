package hostagent

import (
	"context"
	"errors"
	"regexp"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

var ErrExistingRuntimeStart = errors.New("existing worker authenticated startup failed")

var existingRuntimeIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ExistingRuntimeProvider can verify the complete account, resource, network,
// sandbox and image specification without creating or repairing anything.
// RuntimeID pins the physical instance independently of its logical reference.
type ExistingRuntimeProvider interface {
	RuntimeProvider
	ValidateExisting(context.Context, provider.Instance, provider.SlotSpec) error
}

// StartExisting is the authenticated lifecycle START path. It never creates,
// recreates, stops or deletes an instance, including on a partial failure. The
// caller owns the returned Runtime and must Close it when no longer needed.
func (c *Controller) StartExisting(ctx context.Context, spec provider.SlotSpec, expected provider.Instance) (*Runtime, error) {
	if c == nil || ctx == nil || ctx.Err() != nil || c.provider == nil || c.bootstrap == nil || c.readyTimeout <= 0 ||
		spec.Validate() != nil || expected.ProviderRef == "" || !existingRuntimeIDPattern.MatchString(expected.RuntimeID) ||
		expected.SlotID != spec.SlotID || expected.Epoch != spec.Epoch || expected.RuntimeGeneration != spec.RuntimeGeneration {
		return nil, ErrExistingRuntimeStart
	}
	verified, ok := c.provider.(ExistingRuntimeProvider)
	if !ok {
		return nil, ErrExistingRuntimeStart
	}
	tlsConfig, err := runtimeidentity.ClientTLS(runtimeidentity.Binding{
		AccountHash: provider.RuntimeAccountID(spec.AccountID), SlotID: spec.SlotID,
		NodeID: c.nodeID, Epoch: spec.Epoch, Generation: spec.RuntimeGeneration,
	}, c.runtimeTrustPEM, c.nodeCertificate)
	if err != nil || ctx.Err() != nil {
		return nil, ErrExistingRuntimeStart
	}
	bounded, cancel := context.WithTimeout(ctx, c.readyTimeout)
	defer cancel()
	if verified.ValidateExisting(bounded, expected, spec) != nil || bounded.Err() != nil {
		return nil, ErrExistingRuntimeStart
	}
	// Only physical IDs reach mutable/connection operations. A later container
	// replacing the stable slot name cannot be started or adopted by this call.
	physical := expected
	physical.ProviderRef = expected.RuntimeID
	if c.provider.Start(bounded, physical.ProviderRef) != nil || bounded.Err() != nil {
		return nil, ErrExistingRuntimeStart
	}
	if c.bootstrap.Prepare(bounded, spec, physical) != nil || bounded.Err() != nil {
		return nil, ErrExistingRuntimeStart
	}
	if verified.ValidateExisting(bounded, expected, spec) != nil || bounded.Err() != nil {
		return nil, ErrExistingRuntimeStart
	}
	if c.waitReady(bounded, physical.ProviderRef, spec, expected.RuntimeID) != nil || bounded.Err() != nil {
		return nil, ErrExistingRuntimeStart
	}
	runtime, err := c.connectRuntime(bounded, spec, physical, tlsConfig)
	if err != nil {
		return nil, ErrExistingRuntimeStart
	}
	if verified.ValidateExisting(bounded, expected, spec) != nil || bounded.Err() != nil {
		_ = runtime.Close()
		return nil, ErrExistingRuntimeStart
	}
	runtime.Instance = expected
	return runtime, nil
}
