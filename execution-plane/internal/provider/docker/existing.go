package docker

import (
	"context"
	"errors"
	"regexp"

	base "github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

var exactExistingID = regexp.MustCompile(`^[a-f0-9]{64}$`)
var errExistingInstance = errors.New("existing Docker instance could not be validated")

// ValidateExisting reads one exact container and its image/network metadata.
// It is not an adoption, creation, repair or lifecycle operation. State and
// timestamps are intentionally not requirements: START can target a stopped
// instance. RuntimeID is the physical identity; ProviderRef retains its prior
// logical name semantics for lifecycle operations such as Destroy.
func (p *Provider) ValidateExisting(ctx context.Context, expected base.Instance, spec base.SlotSpec) error {
	if p == nil || p.engine == nil || ctx == nil || ctx.Err() != nil || !exactExistingID.MatchString(expected.RuntimeID) ||
		spec.Validate() != nil || !sandboxImmutableReference(spec.ImageDigest) ||
		(expected.ProviderRef != containerName(spec.SlotID) && expected.ProviderRef != expected.RuntimeID) ||
		expected.SlotID != spec.SlotID || expected.Epoch != spec.Epoch || expected.RuntimeGeneration != spec.RuntimeGeneration ||
		!contains(p.config.AllowedSeccompProfiles, spec.Security.SeccompProfile) || !contains(p.config.AllowedAppArmorProfiles, spec.Security.AppArmorProfile) {
		return errExistingInstance
	}
	actual, err := p.existingSlot(ctx, expected.RuntimeID, spec)
	// existingSlot echoes its lookup argument as ProviderRef. Only RuntimeID
	// comes from the actual Container.ID within that same verified snapshot.
	if err != nil || ctx.Err() != nil || actual.ProviderRef != expected.RuntimeID || actual.RuntimeID != expected.RuntimeID || actual.SlotID != expected.SlotID ||
		actual.Epoch != expected.Epoch || actual.RuntimeGeneration != expected.RuntimeGeneration {
		return errExistingInstance
	}
	return nil
}
