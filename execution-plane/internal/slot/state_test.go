package slot

import "testing"

func TestLifecycleHappyPath(t *testing.T) {
	states := []State{
		StateRequested,
		StatePulling,
		StateCreating,
		StateStarting,
		StateReady,
		StateBusy,
		StateReady,
		StateDraining,
		StateStopped,
		StateDestroyed,
	}

	for i := 0; i < len(states)-1; i++ {
		if err := ValidateTransition(states[i], states[i+1]); err != nil {
			t.Fatalf("transition %d failed: %v", i, err)
		}
	}
}

func TestDestroyedIsTerminal(t *testing.T) {
	if CanTransition(StateDestroyed, StateStarting) {
		t.Fatal("destroyed slot must be terminal")
	}
	if !CanTransition(StateDestroyed, StateDestroyed) {
		t.Fatal("same-state transition must be idempotent")
	}
}

func TestInvalidState(t *testing.T) {
	if err := ValidateTransition(State("unknown"), StateReady); err == nil {
		t.Fatal("expected invalid state error")
	}
}
