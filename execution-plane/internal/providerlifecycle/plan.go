package providerlifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

const (
	ExperimentName = "providerlifecycle-p2"
	NodeID         = "node-p2"
	RuntimePort    = 8093
	ImageName      = "isthmus.local/providerlifecycle-worker"
	WorkerPath     = "/worker"
)

var ErrPlan = errors.New("provider lifecycle experiment plan is invalid")

type SlotPlan struct {
	Label     string
	SlotID    string
	AccountID string
}

type Plan struct {
	ImageDigest string
	Resources   provider.ResourceLimits
	Security    provider.SecurityPolicy
	Network     provider.NetworkPolicy
	Slots       [2]SlotPlan
}

func DefaultPlan(imageDigest string) (Plan, error) {
	plan := Plan{
		ImageDigest: strings.TrimSpace(imageDigest),
		Resources: provider.ResourceLimits{
			CPUMilli: 500, MemoryBytes: 512 << 20, PIDs: 128, TmpfsBytes: 128 << 20,
		},
		Security: provider.SecurityPolicy{
			RunAsUser: 1000, ReadOnlyRootFS: true, NoNewPrivileges: true,
			DropAllCapabilities: true, SeccompProfile: "builtin", AppArmorProfile: "docker-default",
		},
		Network: provider.NetworkPolicy{
			DenyDirectInternet: true, EgressProxyEndpoint: "http://host-agent.execution.internal:8094",
		},
		Slots: [2]SlotPlan{
			{Label: "a", SlotID: "slot-p2-a", AccountID: "account-p2-a"},
			{Label: "b", SlotID: "slot-p2-b", AccountID: "account-p2-b"},
		},
	}
	if err := plan.Validate(); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func (p Plan) Validate() error {
	if !strings.HasPrefix(p.ImageDigest, ImageName+"@sha256:") {
		return ErrPlan
	}
	digest := strings.TrimPrefix(p.ImageDigest, ImageName+"@sha256:")
	if len(digest) != 64 {
		return ErrPlan
	}
	if _, err := hex.DecodeString(digest); err != nil || digest != strings.ToLower(digest) {
		return ErrPlan
	}
	if p.Resources.Validate() != nil || p.Security.Validate() != nil || p.Network.Validate() != nil {
		return ErrPlan
	}
	if p.Security.RunAsUser == 0 || p.Slots[0].SlotID == p.Slots[1].SlotID ||
		p.Slots[0].AccountID == p.Slots[1].AccountID {
		return ErrPlan
	}
	seen := map[string]struct{}{}
	for _, slot := range p.Slots {
		if slot.Label != "a" && slot.Label != "b" || slot.SlotID == "" || slot.AccountID == "" {
			return ErrPlan
		}
		if _, exists := seen[slot.SlotID]; exists {
			return ErrPlan
		}
		seen[slot.SlotID] = struct{}{}
		if spec, err := p.Spec(slot.Label, 1, 1); err != nil || spec.Validate() != nil {
			return ErrPlan
		}
	}
	return nil
}

func (p Plan) Spec(label string, epoch, generation uint64) (provider.SlotSpec, error) {
	for _, slot := range p.Slots {
		if slot.Label != label {
			continue
		}
		return provider.SlotSpec{
			SlotID: slot.SlotID, AccountID: slot.AccountID, Epoch: epoch, RuntimeGeneration: generation,
			ImageDigest: p.ImageDigest, Resources: p.Resources, Security: p.Security, Network: p.Network,
		}, nil
	}
	return provider.SlotSpec{}, ErrPlan
}

func AccountHash(accountID string) string {
	return provider.RuntimeAccountID(accountID)
}

func SyntheticImageDigest() string {
	sum := sha256.Sum256([]byte(ExperimentName + "/synthetic-image"))
	return ImageName + "@sha256:" + fmt.Sprintf("%x", sum)
}
