package providerlifecycle_test

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/providerlifecycle"
)

func TestExperimentSlotsStayExclusive(t *testing.T) {
	plan, err := providerlifecycle.DefaultPlan(providerlifecycle.SyntheticImageDigest())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Slots[0].SlotID == plan.Slots[1].SlotID || plan.Slots[0].AccountID == plan.Slots[1].AccountID {
		t.Fatal("slots were not exclusive")
	}
	if plan.ImageDigest == "" || plan.Security.RunAsUser == 0 {
		t.Fatal("plan omitted image or non-root user")
	}
}
