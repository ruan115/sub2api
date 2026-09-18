package providerlifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

var ErrInventory = errors.New("provider lifecycle inventory rejected")

type RecordedInstance struct {
	Label       string
	SlotID      string
	ProviderRef string
	RuntimeID   string
	NetworkName string
	NetworkID   string
}

type Inventory struct {
	Instances [2]RecordedInstance
}

func (inv Inventory) Validate() error {
	if inv.Instances[0].RuntimeID == "" || inv.Instances[1].RuntimeID == "" {
		return ErrInventory
	}
	if inv.Instances[0].RuntimeID == inv.Instances[1].RuntimeID {
		return ErrInventory
	}
	if inv.Instances[0].NetworkID == "" || inv.Instances[1].NetworkID == "" ||
		inv.Instances[0].NetworkID == inv.Instances[1].NetworkID {
		return ErrInventory
	}
	if inv.Instances[0].NetworkName == "" || inv.Instances[0].NetworkName == inv.Instances[1].NetworkName {
		return ErrInventory
	}
	seen := map[string]struct{}{}
	for _, item := range inv.Instances {
		if item.Label == "" || item.SlotID == "" || item.ProviderRef == "" ||
			len(item.RuntimeID) != 64 || strings.TrimSpace(item.RuntimeID) != item.RuntimeID {
			return ErrInventory
		}
		if _, exists := seen[item.RuntimeID]; exists {
			return ErrInventory
		}
		seen[item.RuntimeID] = struct{}{}
	}
	return nil
}

func (inv Inventory) ContainsRuntime(id string) bool {
	for _, item := range inv.Instances {
		if item.RuntimeID == id || item.ProviderRef == id || item.NetworkID == id || item.NetworkName == id {
			return true
		}
	}
	return false
}

// Cleanup destroys only previously recorded logical provider refs. Unknown
// identifiers are refused before any Docker call.
func Cleanup(ctx context.Context, runtime provider.ExecutionProvider, inv Inventory) error {
	if runtime == nil || ctx == nil || ctx.Err() != nil || inv.Validate() != nil {
		return ErrInventory
	}
	var joined error
	for _, item := range inv.Instances {
		if err := runtime.Destroy(ctx, item.ProviderRef); err != nil {
			joined = errors.Join(joined, fmt.Errorf("destroy %s: %w", item.SlotID, err))
		}
	}
	return joined
}

func RefuseUntracked(id string, inv Inventory) error {
	if !inv.ContainsRuntime(id) {
		return fmt.Errorf("%w: %s", ErrInventory, id)
	}
	return nil
}
