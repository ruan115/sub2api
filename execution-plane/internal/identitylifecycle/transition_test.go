package identitylifecycle

import (
	"errors"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider/docker"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtimeidentity"
)

const (
	accountA = "0123456789abcdef0123456789abcdef"
	accountB = "fedcba9876543210fedcba9876543210"
)

func binding(account string, epoch, generation uint64) runtimeidentity.Binding {
	return runtimeidentity.Binding{
		AccountHash: account, SlotID: "slot-p2-a", NodeID: "node-p2",
		Epoch: epoch, Generation: generation,
	}
}

// The contract must describe the identity paths the provider actually uses. If
// the provider moves them, this fails rather than leaving the rule pointing at
// a path that no longer exists. The assertion lives in the test so the rule set
// itself does not have to depend on the docker provider.
func TestNonPersistentPathsMatchProvider(t *testing.T) {
	t.Parallel()
	want := []string{docker.WorkerIdentityDirectory, docker.WorkerRuntimeTrustFile}
	if len(NonPersistentPaths) != len(want) {
		t.Fatalf("NonPersistentPaths = %v, want %v", NonPersistentPaths, want)
	}
	for index, path := range want {
		if NonPersistentPaths[index] != path {
			t.Fatalf("NonPersistentPaths[%d] = %q, want %q", index, NonPersistentPaths[index], path)
		}
	}
}

func TestTransitionContract(t *testing.T) {
	t.Parallel()
	base := binding(accountA, 3, 7)
	for _, testCase := range []struct {
		name       string
		transition Transition
		wantReason string // empty means the transition must be allowed
	}{
		{name: "adopt-same-live-instance", transition: Transition{EventAdopt, base, base}},
		{name: "restart-advances-epoch-and-generation", transition: Transition{EventRestart, base, binding(accountA, 4, 8)}},
		{name: "upgrade-advances-epoch-and-generation", transition: Transition{EventUpgrade, base, binding(accountA, 4, 8)}},
		{name: "account-change-advances-both", transition: Transition{EventAccountChange, base, binding(accountB, 4, 8)}},
		{name: "destroy-is-terminal-step", transition: Transition{EventDestroy, base, runtimeidentity.Binding{}}},

		// The receipt store is unique on (slot, epoch), so advancing only the
		// generation could never obtain a certificate.
		{"restart-holding-epoch", Transition{EventRestart, base, binding(accountA, 3, 8)}, "must advance the epoch past 3"},
		{"upgrade-holding-epoch", Transition{EventUpgrade, base, binding(accountA, 3, 8)}, "must advance the epoch past 3"},
		{"account-change-holding-epoch", Transition{EventAccountChange, base, binding(accountB, 3, 8)}, "must advance the epoch past 3"},
		{"restart-rewinding-epoch", Transition{EventRestart, base, binding(accountA, 2, 8)}, "must advance the epoch past 3"},

		// The tmpfs identity is gone, so holding the generation would let the
		// previous instance's unexpired certificate verify as this one.
		{"restart-holding-generation", Transition{EventRestart, base, binding(accountA, 4, 7)}, "must advance the generation past 7"},
		{"restart-rewinding-generation", Transition{EventRestart, base, binding(accountA, 4, 6)}, "must advance the generation past 7"},
		{"account-change-holding-generation", Transition{EventAccountChange, base, binding(accountB, 4, 7)}, "must advance the generation past 7"},

		// Adoption claims the key survived; any binding drift contradicts that.
		{"adopt-with-new-generation", Transition{EventAdopt, base, binding(accountA, 3, 8)}, "adoption must keep the binding identical"},
		{"adopt-with-new-epoch", Transition{EventAdopt, base, binding(accountA, 4, 7)}, "adoption must keep the binding identical"},
		{"adopt-with-new-account", Transition{EventAdopt, base, binding(accountB, 3, 7)}, "adoption must keep the binding identical"},

		// A silent account swap must not present itself as a restart.
		{"restart-swapping-account", Transition{EventRestart, base, binding(accountB, 4, 8)}, "must not change account"},
		{"upgrade-swapping-account", Transition{EventUpgrade, base, binding(accountB, 4, 8)}, "must not change account"},
		{"account-change-without-new-account", Transition{EventAccountChange, base, binding(accountA, 4, 8)}, "account-change kept account"},

		{"slot-change", Transition{EventRestart, base, runtimeidentity.Binding{
			AccountHash: accountA, SlotID: "slot-p2-b", NodeID: "node-p2", Epoch: 4, Generation: 8,
		}}, "slot changed"},
		// lease.Claim is keyed on node as well, so a node move is a different
		// history rather than a restart of this one.
		{"node-change", Transition{EventRestart, base, runtimeidentity.Binding{
			AccountHash: accountA, SlotID: "slot-p2-a", NodeID: "node-other", Epoch: 4, Generation: 8,
		}}, "node changed"},
		{"adopt-node-change", Transition{EventAdopt, base, runtimeidentity.Binding{
			AccountHash: accountA, SlotID: "slot-p2-a", NodeID: "node-other", Epoch: 3, Generation: 7,
		}}, "node changed"},

		{"destroy-naming-successor", Transition{EventDestroy, base, binding(accountA, 4, 8)}, "destroy must not name a successor"},
		{"unknown-event", Transition{Event("resume"), base, binding(accountA, 4, 8)}, "unknown event"},
		{"empty-event", Transition{Event(""), base, binding(accountA, 4, 8)}, "unknown event"},
		{"invalid-previous", Transition{EventRestart, binding("not-a-hash", 3, 7), binding(accountA, 4, 8)}, "previous binding invalid"},
		{"invalid-next-zero-generation", Transition{EventRestart, base, binding(accountA, 4, 0)}, "next binding invalid"},
		{"invalid-next-zero-epoch", Transition{EventRestart, base, binding(accountA, 0, 8)}, "next binding invalid"},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			err := testCase.transition.Validate()
			if testCase.wantReason == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want allow", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() allowed a rejected transition")
			}
			if !errors.Is(err, ErrTransition) {
				t.Fatalf("Validate() error = %v, want ErrTransition", err)
			}
			// Assert the reason, not merely that something failed: otherwise a
			// rule could be deleted and the case would still pass on a
			// different rule's error.
			if !strings.Contains(err.Error(), testCase.wantReason) {
				t.Fatalf("Validate() error = %q, want it to mention %q", err, testCase.wantReason)
			}
		})
	}
}

// Resuming at an unchanged binding is unreachable through enrollment, so this
// contract could never permit it: stopping the container wipes the tmpfs
// identity, the instance presents a NEW public key, and the receipt for
// (slot, epoch) is pinned to the old one via SameReceiptIdentity.
//
// reconcile.go used to route ActualStopped + DesiredReady to ActionStart,
// which is exactly this transition and therefore always failed. That routing
// now walks the slot out through destroy, release and place so the store
// issues a fresh epoch; see TestPlanReplacesStoppedRuntimeRatherThanResumingIts
// Epoch in internal/reconcile.
func TestResumeAtTheSameBindingStaysRejected(t *testing.T) {
	t.Parallel()
	asReconcileDispatchesItToday := Transition{
		Event:    EventRestart,
		Previous: binding(accountA, 3, 7),
		Next:     binding(accountA, 3, 7),
	}
	err := asReconcileDispatchesItToday.Validate()
	if err == nil {
		t.Fatal("resume at an unchanged binding was allowed")
	}
	if !strings.Contains(err.Error(), "must advance the epoch past 3") {
		t.Fatalf("error = %q, want the epoch rule", err)
	}
}

// Generation is a per-slot counter that never resets, so no binding tuple is
// reachable twice — including when an account returns to a slot it once held.
// The bindings below are collected from histories the validator ACCEPTED, so
// gutting ValidateHistory makes this fail rather than pass.
func TestAcceptedHistoriesNeverRepeatABinding(t *testing.T) {
	t.Parallel()
	accepted := [][]Transition{
		{
			{EventRestart, binding(accountA, 1, 1), binding(accountA, 2, 2)},
			{EventAccountChange, binding(accountA, 2, 2), binding(accountB, 3, 3)},
			{EventRestart, binding(accountB, 3, 3), binding(accountB, 4, 4)},
			{EventAccountChange, binding(accountB, 4, 4), binding(accountA, 5, 5)},
		},
		{
			{EventAdopt, binding(accountA, 5, 5), binding(accountA, 5, 5)},
			{EventUpgrade, binding(accountA, 5, 5), binding(accountA, 6, 6)},
		},
	}
	seen := make(map[runtimeidentity.Binding]string)
	for _, history := range accepted {
		if err := ValidateHistory(Floor{}, history); err != nil {
			t.Fatalf("ValidateHistory() = %v, want allow", err)
		}
		for _, transition := range history {
			// Adoption legitimately repeats its own binding within one step.
			if transition.Event == EventAdopt {
				continue
			}
			if where, exists := seen[transition.Next]; exists {
				t.Fatalf("binding %+v reused, first seen at %s", transition.Next, where)
			}
			seen[transition.Next] = string(transition.Event)
		}
	}

	// Returning to accountA must not rewind to the generation it left behind.
	rewind := []Transition{
		{EventRestart, binding(accountA, 1, 1), binding(accountA, 2, 2)},
		{EventAccountChange, binding(accountA, 2, 2), binding(accountB, 3, 3)},
		{EventAccountChange, binding(accountB, 3, 3), binding(accountA, 4, 2)},
	}
	if err := ValidateHistory(Floor{}, rewind); err == nil {
		t.Fatal("ValidateHistory() allowed a generation reset on account return")
	}
}

// Destruction is terminal for the binding, but a caller can always start a new
// history for the same slot. The floor is what stops that fresh history from
// landing back on a tuple whose certificate has not expired yet.
func TestFloorBlocksRebirthOntoADestroyedBinding(t *testing.T) {
	t.Parallel()
	destroyed := Floor{Epoch: 9, Generation: 12}
	reborn := []Transition{
		{EventRestart, binding(accountA, 1, 1), binding(accountA, 2, 2)},
	}
	err := ValidateHistory(destroyed, reborn)
	if err == nil {
		t.Fatal("ValidateHistory() allowed a rebirth below the floor")
	}
	if !strings.Contains(err.Error(), "below the floor") {
		t.Fatalf("error = %q, want it to mention the floor", err)
	}
	// Equal to the floor is the boundary and must be allowed; the transition's
	// own rules then force it upward.
	atFloor := []Transition{
		{EventRestart, binding(accountA, 9, 12), binding(accountA, 10, 13)},
	}
	if err := ValidateHistory(destroyed, atFloor); err != nil {
		t.Fatalf("ValidateHistory() at the floor = %v, want allow", err)
	}
	// A half-rewind — epoch above the floor but generation below it — is the
	// case a single combined comparison would miss.
	halfRewind := []Transition{
		{EventRestart, binding(accountA, 10, 3), binding(accountA, 11, 4)},
	}
	if err := ValidateHistory(destroyed, halfRewind); err == nil {
		t.Fatal("ValidateHistory() allowed a generation below the floor")
	}
}

func TestValidateHistoryChainingAndTerminality(t *testing.T) {
	t.Parallel()
	if err := ValidateHistory(Floor{}, nil); err == nil {
		t.Fatal("ValidateHistory(nil) was allowed")
	}
	gap := []Transition{
		{EventRestart, binding(accountA, 1, 1), binding(accountA, 2, 2)},
		// Previous does not equal the prior step's Next: a generation was
		// issued outside the recorded history.
		{EventRestart, binding(accountA, 3, 3), binding(accountA, 4, 4)},
	}
	if err := ValidateHistory(Floor{}, gap); err == nil {
		t.Fatal("ValidateHistory() allowed a discontinuous history")
	}
	resurrect := []Transition{
		{EventDestroy, binding(accountA, 1, 1), runtimeidentity.Binding{}},
		{EventRestart, binding(accountA, 1, 1), binding(accountA, 2, 2)},
	}
	if err := ValidateHistory(Floor{}, resurrect); err == nil {
		t.Fatal("ValidateHistory() allowed a step after destroy")
	}
	ok := []Transition{
		{EventAdopt, binding(accountA, 1, 1), binding(accountA, 1, 1)},
		{EventRestart, binding(accountA, 1, 1), binding(accountA, 2, 2)},
		{EventDestroy, binding(accountA, 2, 2), runtimeidentity.Binding{}},
	}
	if err := ValidateHistory(Floor{}, ok); err != nil {
		t.Fatalf("ValidateHistory() = %v, want allow", err)
	}
}

func TestKeySurvivalAndEnrollmentExpectations(t *testing.T) {
	t.Parallel()
	for event, wantPreserved := range map[Event]bool{
		EventAdopt: true, EventRestart: false, EventUpgrade: false,
		EventAccountChange: false, EventDestroy: false,
	} {
		if PreservesInstanceKey(event) != wantPreserved {
			t.Fatalf("PreservesInstanceKey(%q) = %v, want %v", event, !wantPreserved, wantPreserved)
		}
	}
	for event, wantEnrollment := range map[Event]bool{
		EventRestart: true, EventUpgrade: true, EventAccountChange: true,
		EventAdopt: false, EventDestroy: false,
	} {
		if RequiresEnrollment(event) != wantEnrollment {
			t.Fatalf("RequiresEnrollment(%q) = %v, want %v", event, !wantEnrollment, wantEnrollment)
		}
	}
	// Anything that loses the key must re-enroll, and nothing may claim both.
	for _, event := range []Event{EventAdopt, EventRestart, EventUpgrade, EventAccountChange, EventDestroy} {
		if PreservesInstanceKey(event) && RequiresEnrollment(event) {
			t.Fatalf("event %q claims to keep its key and to need enrollment", event)
		}
	}
}

func TestValidatePersistentPathsAllowsOnlyTheHomeSubtree(t *testing.T) {
	t.Parallel()
	for _, proposed := range []string{
		docker.WorkerIdentityDirectory,
		docker.WorkerIdentityDirectory + "/instance-identity.json",
		docker.WorkerRuntimeTrustFile,
		"/run/execution",
		"/run",
		// /var/run is a symlink to /run on the Debian base: a lexical denylist
		// would approve this while it captures identity material.
		"/var/run",
		"/var/run/execution/identity",
		"/",
		"/tmp",
		"/homework", // shares a prefix with /home at a non-segment boundary
		"/home",     // the root itself is not a persistable path
		"relative/home",
		"/home/agent/../../run/execution/identity",
		"/home/agent/",
		"//home/agent",
		"/home/./agent",
		"/home/agent\n/run/execution/identity",
		"/home/agent\x00",
		"",
		"/home/" + strings.Repeat("a", 5000),
	} {
		if err := ValidatePersistentPaths([]string{proposed}); err == nil {
			t.Fatalf("ValidatePersistentPaths(%q) was allowed", proposed)
		}
	}
	if err := ValidatePersistentPaths([]string{"/home/agent", "/home/agent/.claude"}); err != nil {
		t.Fatalf("ValidatePersistentPaths() = %v, want allow", err)
	}
	if err := ValidatePersistentPaths(nil); err != nil {
		t.Fatalf("ValidatePersistentPaths(nil) = %v, want allow", err)
	}
	// One bad entry rejects the whole set.
	if err := ValidatePersistentPaths([]string{"/home/agent", "/var/run"}); err == nil {
		t.Fatal("ValidatePersistentPaths() allowed a set containing a denied path")
	}
}

func TestPersistenceErrorsAreIdentifiable(t *testing.T) {
	t.Parallel()
	err := ValidatePersistentPaths([]string{"/var/run"})
	if !errors.Is(err, ErrPersistence) || !errors.Is(err, ErrTransition) {
		t.Fatalf("error = %v, want both ErrPersistence and ErrTransition", err)
	}
	if !strings.Contains(err.Error(), PersistentHomeRoot) {
		t.Fatalf("error = %q, want it to name %q", err, PersistentHomeRoot)
	}
	// The identity-overlap rule must be reachable rather than shadowed by the
	// allowlist, so that naming identity material reports why it is refused and
	// the rule keeps working if the allowlist root is ever widened.
	identity := ValidatePersistentPaths([]string{docker.WorkerIdentityDirectory + "/instance-identity.json"})
	if identity == nil || !strings.Contains(identity.Error(), "overlaps identity material") {
		t.Fatalf("error = %v, want the identity-overlap reason", identity)
	}
}
