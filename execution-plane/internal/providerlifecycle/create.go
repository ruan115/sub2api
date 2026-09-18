package providerlifecycle

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
)

var ErrCreate = errors.New("provider lifecycle dual create failed")

type NetworkInspector interface {
	InspectNetwork(context.Context, string) (docker.Network, error)
}

func NewProvider(engine docker.Engine, now func() time.Time) (*docker.Provider, error) {
	config := docker.DefaultConfig()
	config.Now = now
	config.WorkerBootstrap = &docker.WorkerBootstrap{
		NodeID: NodeID, TicketPublicKey: base64.RawStdEncoding.EncodeToString(make([]byte, 32)),
		UpstreamBaseURL: "https://api.anthropic.com", RuntimePort: RuntimePort,
		IdentityDirectory: docker.WorkerIdentityDirectory, RuntimeTrustFile: docker.WorkerRuntimeTrustFile,
	}
	return docker.New(config, engine)
}

func CreatePair(ctx context.Context, runtime provider.ExecutionProvider, inspector NetworkInspector, plan Plan) (Inventory, error) {
	if runtime == nil || inspector == nil || ctx == nil || ctx.Err() != nil || plan.Validate() != nil {
		return Inventory{}, ErrCreate
	}
	var inv Inventory
	for i, slot := range plan.Slots {
		spec, err := plan.Spec(slot.Label, 1, 1)
		if err != nil {
			return Inventory{}, err
		}
		instance, err := runtime.Create(ctx, spec)
		if err != nil {
			return Inventory{}, err
		}
		status, err := runtime.Inspect(ctx, instance.ProviderRef)
		if err != nil {
			return Inventory{}, err
		}
		if status.RuntimeID != instance.RuntimeID || status.SlotID != spec.SlotID {
			return Inventory{}, ErrCreate
		}
		network, err := inspector.InspectNetwork(ctx, networkNameFor(instance.ProviderRef))
		if err != nil || !network.Internal || network.ID == "" {
			return Inventory{}, ErrCreate
		}
		inv.Instances[i] = RecordedInstance{
			Label: slot.Label, SlotID: spec.SlotID, ProviderRef: instance.ProviderRef,
			RuntimeID: instance.RuntimeID, NetworkName: network.Name, NetworkID: network.ID,
		}
	}
	if err := inv.Validate(); err != nil {
		return Inventory{}, err
	}
	return inv, nil
}

func networkNameFor(providerRef string) string {
	const prefix = "execution-slot-"
	return docker.DefaultConfig().NetworkPrefix + strings.TrimPrefix(providerRef, prefix)
}
