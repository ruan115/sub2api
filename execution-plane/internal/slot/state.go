package slot

import "fmt"

// State is the orchestrator-visible lifecycle state of one account execution
// slot. A slot is an isolation boundary, not a virtual machine or server.
type State string

const (
	StateRequested  State = "requested"
	StatePulling    State = "pulling"
	StateCreating   State = "creating"
	StateStarting   State = "starting"
	StateReady      State = "ready"
	StateBusy       State = "busy"
	StateDraining   State = "draining"
	StateStopped    State = "stopped"
	StateDestroyed  State = "destroyed"
	StateUnhealthy  State = "unhealthy"
	StateRecreating State = "recreating"
)

var transitions = map[State]map[State]struct{}{
	StateRequested: {
		StatePulling:   {},
		StateDestroyed: {},
	},
	StatePulling: {
		StateCreating:  {},
		StateUnhealthy: {},
		StateDestroyed: {},
	},
	StateCreating: {
		StateStarting:  {},
		StateUnhealthy: {},
		StateDestroyed: {},
	},
	StateStarting: {
		StateReady:     {},
		StateDraining:  {},
		StateUnhealthy: {},
	},
	StateReady: {
		StateBusy:      {},
		StateDraining:  {},
		StateStopped:   {},
		StateUnhealthy: {},
	},
	StateBusy: {
		StateReady:     {},
		StateDraining:  {},
		StateUnhealthy: {},
	},
	StateDraining: {
		StateStopped:   {},
		StateDestroyed: {},
		StateUnhealthy: {},
	},
	StateStopped: {
		StateStarting:   {},
		StateRecreating: {},
		StateDestroyed:  {},
	},
	StateUnhealthy: {
		StateRecreating: {},
		StateDraining:   {},
		StateDestroyed:  {},
	},
	StateRecreating: {
		StateStarting:  {},
		StateUnhealthy: {},
		StateDestroyed: {},
	},
	StateDestroyed: {},
}

func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransition accepts a same-state transition so reconcile operations remain
// idempotent after retries.
func CanTransition(from, to State) bool {
	if from == to {
		return from.Valid()
	}
	allowed, ok := transitions[from]
	if !ok {
		return false
	}
	_, ok = allowed[to]
	return ok
}

func ValidateTransition(from, to State) error {
	if !from.Valid() {
		return fmt.Errorf("invalid current slot state %q", from)
	}
	if !to.Valid() {
		return fmt.Errorf("invalid target slot state %q", to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("slot state transition %q -> %q is not allowed", from, to)
	}
	return nil
}
