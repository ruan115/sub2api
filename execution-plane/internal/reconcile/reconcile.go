package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

var imageDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type DesiredState string

const (
	DesiredReady   DesiredState = "ready"
	DesiredDrained DesiredState = "drained"
	DesiredAbsent  DesiredState = "absent"
)

type ActualState string

const (
	ActualMissing   ActualState = "missing"
	ActualCreating  ActualState = "creating"
	ActualCreated   ActualState = "created"
	ActualRunning   ActualState = "running"
	ActualDraining  ActualState = "draining"
	ActualDrained   ActualState = "drained"
	ActualStopped   ActualState = "stopped"
	ActualDestroyed ActualState = "destroyed"
	ActualFailed    ActualState = "failed"
)

type ActionKind string

const (
	ActionNone    ActionKind = "none"
	ActionPlace   ActionKind = "place"
	ActionCreate  ActionKind = "create"
	ActionStart   ActionKind = "start"
	ActionInspect ActionKind = "inspect"
	ActionDrain   ActionKind = "drain"
	ActionDestroy ActionKind = "destroy"
	ActionRelease ActionKind = "release"
)

type Slot struct {
	ID                string
	AccountID         string
	DesiredState      DesiredState
	DesiredGeneration uint64
	ImageDigest       string
	// NextExecutionEpoch is the epoch the store will hand to the next
	// reservation, and it advances on every ReserveAssignment. Placement uses
	// it to tell one attempt from the next: a placement job is completed the
	// moment it is dispatched, and a completed job can never be claimed again,
	// so a placement key that did not move would strand the slot without an
	// assignment forever.
	NextExecutionEpoch uint64
}

type Assignment struct {
	ID                string
	SlotID            string
	NodeID            string
	ExecutionEpoch    uint64
	DesiredGeneration uint64
	ActualGeneration  uint64
	ImageDigest       string
	ActualState       ActualState
	Healthy           bool
}

type Input struct {
	Slot       Slot
	Assignment *Assignment
}

type Action struct {
	Kind              ActionKind
	CommandID         string
	SlotID            string
	AccountID         string
	NodeID            string
	ExecutionEpoch    uint64
	ImageDigest       string
	DesiredGeneration uint64
	// RuntimeGeneration is the immutable generation of the target assignment.
	// Cleanup can target an old runtime under a newer desired-state intent.
	RuntimeGeneration uint64
	IdempotencyKey    string
}

type Result struct {
	Action     Action
	Job        store.ProvisioningJob
	Claimed    bool
	Dispatched bool
	Completed  bool
}

type Executor interface {
	Execute(ctx context.Context, action Action) error
}

type Config struct {
	ClaimTTL   time.Duration
	RetryDelay time.Duration
	Now        func() time.Time
}

func DefaultConfig() Config {
	return Config{ClaimTTL: 30 * time.Second, RetryDelay: 5 * time.Second, Now: time.Now}
}

type Controller struct {
	jobs     store.JobRepository
	executor Executor
	config   Config
}

func NewController(jobs store.JobRepository, executor Executor, config Config) (*Controller, error) {
	if jobs == nil || executor == nil || config.ClaimTTL <= 0 || config.RetryDelay <= 0 {
		return nil, errors.New("job repository, executor and positive reconcile timings are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Controller{jobs: jobs, executor: executor, config: config}, nil
}

func (c *Controller) Reconcile(ctx context.Context, input Input) (Result, error) {
	action, err := Plan(input)
	if err != nil {
		return Result{}, err
	}
	if action.Kind == ActionNone {
		return Result{Action: action}, nil
	}
	now := c.config.Now().UTC()
	jobID := deterministicJobID(action.IdempotencyKey)
	job, claimed, err := c.jobs.ClaimProvisioningJob(ctx, store.ProvisioningJob{
		ID: jobID, SlotID: action.SlotID, IdempotencyKey: action.IdempotencyKey,
		DesiredGeneration: action.DesiredGeneration, Step: string(action.Kind),
	}, now, c.config.ClaimTTL)
	if err != nil {
		return Result{}, fmt.Errorf("claim reconcile job: %w", err)
	}
	result := Result{Action: action, Job: job, Claimed: claimed}
	if !claimed {
		return result, nil
	}
	action.CommandID = job.ID
	result.Action = action
	if err := c.executor.Execute(ctx, action); err != nil {
		failErr := c.jobs.FailProvisioningJob(ctx, job.ID, "action_dispatch_failed", now, now.Add(c.config.RetryDelay))
		if failErr != nil {
			return result, fmt.Errorf("execute reconcile action: %v; persist retry: %w", err, failErr)
		}
		return result, fmt.Errorf("execute reconcile action: %w", err)
	}
	if action.Kind == ActionPlace || action.Kind == ActionRelease {
		if err := c.jobs.CompleteProvisioningJob(ctx, job.ID, now); err != nil {
			return result, fmt.Errorf("complete local reconcile action: %w", err)
		}
		result.Completed = true
		result.Job.Status = "completed"
		return result, nil
	}
	if err := c.jobs.MarkProvisioningJobDispatched(ctx, job.ID, now); err != nil {
		return result, fmt.Errorf("mark reconcile action dispatched: %w", err)
	}
	result.Dispatched = true
	result.Job.Status = "dispatched"
	return result, nil
}

func Plan(input Input) (Action, error) {
	if input.Slot.ID == "" || input.Slot.AccountID == "" || input.Slot.DesiredGeneration == 0 || !imageDigestPattern.MatchString(input.Slot.ImageDigest) {
		return Action{}, errors.New("reconcile slot is invalid")
	}
	if input.Slot.DesiredState != DesiredReady && input.Slot.DesiredState != DesiredDrained && input.Slot.DesiredState != DesiredAbsent {
		return Action{}, errors.New("reconcile desired state is invalid")
	}
	if input.Assignment == nil {
		if input.Slot.DesiredState == DesiredReady {
			// Fail closed rather than emit a placement key that silently
			// collides with the previous attempt's completed job.
			if input.Slot.NextExecutionEpoch == 0 {
				return Action{}, errors.New("reconcile slot has no next execution epoch to place into")
			}
			return newAction(ActionPlace, input), nil
		}
		return newAction(ActionNone, input), nil
	}
	assignment := input.Assignment
	if assignment.ID == "" || assignment.SlotID != input.Slot.ID || assignment.NodeID == "" || assignment.ExecutionEpoch == 0 || assignment.ActualGeneration == 0 {
		return Action{}, errors.New("reconcile assignment is invalid")
	}

	// Desired generation is an immutable assignment input, independent from
	// ActualGeneration (which only counts observed state changes). A legacy
	// assignment has generation zero and must take the same replacement path as
	// any other stale generation, even when its image is unchanged.
	if input.Slot.DesiredState == DesiredReady &&
		(assignment.DesiredGeneration != input.Slot.DesiredGeneration || assignment.ImageDigest != input.Slot.ImageDigest) {
		switch assignment.ActualState {
		case ActualMissing, ActualDestroyed:
			return newAction(ActionRelease, input), nil
		case ActualDrained, ActualStopped, ActualFailed:
			return newAction(ActionDestroy, input), nil
		case ActualDraining:
			return newAction(ActionInspect, input), nil
		default:
			return newAction(ActionDrain, input), nil
		}
	}

	var kind ActionKind
	switch input.Slot.DesiredState {
	case DesiredReady:
		switch assignment.ActualState {
		case ActualMissing:
			// The assignment is placed but no container was ever created for
			// this epoch, so it is still free to enrol.
			kind = ActionCreate
		case ActualDestroyed:
			// A container did exist for this epoch. Recreating under the same
			// assignment would present a new instance key to an enrollment
			// receipt that is unique on (slot, epoch) and pinned to the old
			// key, so the epoch has to be released and replaced.
			kind = ActionRelease
		case ActualCreated:
			kind = ActionStart
		case ActualStopped:
			// Identity lives on tmpfs, so a stopped container has lost its key
			// and cannot resume at this epoch. Destroy leads to release and a
			// fresh placement rather than a start. This is deliberately
			// conservative: a created-but-never-started container observed by
			// INSPECT also reports "stopped", and is replaced rather than
			// started. That costs one placement and still converges.
			kind = ActionDestroy
		case ActualRunning:
			if assignment.Healthy {
				kind = ActionNone
			} else {
				kind = ActionDrain
			}
		case ActualCreating, ActualDraining, ActualFailed:
			kind = ActionInspect
		case ActualDrained:
			kind = ActionDestroy
		default:
			return Action{}, errors.New("reconcile actual state is invalid")
		}
	case DesiredDrained:
		switch assignment.ActualState {
		case ActualMissing, ActualDrained, ActualStopped, ActualDestroyed:
			kind = ActionNone
		case ActualDraining, ActualFailed:
			kind = ActionInspect
		case ActualCreating, ActualCreated, ActualRunning:
			kind = ActionDrain
		default:
			return Action{}, errors.New("reconcile actual state is invalid")
		}
	case DesiredAbsent:
		switch assignment.ActualState {
		case ActualMissing, ActualDestroyed:
			kind = ActionRelease
		case ActualCreating, ActualCreated, ActualRunning:
			kind = ActionDrain
		case ActualDraining:
			kind = ActionInspect
		case ActualDrained, ActualStopped, ActualFailed:
			kind = ActionDestroy
		default:
			return Action{}, errors.New("reconcile actual state is invalid")
		}
	}
	return newAction(kind, input), nil
}

func newAction(kind ActionKind, input Input) Action {
	action := Action{
		Kind: kind, SlotID: input.Slot.ID, AccountID: input.Slot.AccountID,
		ImageDigest: input.Slot.ImageDigest, DesiredGeneration: input.Slot.DesiredGeneration,
	}
	if input.Assignment != nil {
		action.NodeID = input.Assignment.NodeID
		action.ExecutionEpoch = input.Assignment.ExecutionEpoch
		action.RuntimeGeneration = input.Assignment.DesiredGeneration
		if kind == ActionDrain || kind == ActionDestroy || kind == ActionInspect {
			// The image is part of the target assignment too. An upgraded desired
			// image must not poison cleanup observations for the old instance.
			action.ImageDigest = input.Assignment.ImageDigest
		}
	}
	switch {
	case kind == ActionNone:
	case kind == ActionPlace:
		// Placement has no assignment yet, so epoch and actual generation are
		// both zero and cannot separate one attempt from the next. The epoch
		// the store is about to issue can, and it is also what the reservation
		// is keyed on.
		action.IdempotencyKey = fmt.Sprintf("slot/%s/generation/%d/next-epoch/%d/%s", action.SlotID, action.DesiredGeneration, input.Slot.NextExecutionEpoch, action.Kind)
	default:
		actualGeneration := uint64(0)
		if input.Assignment != nil {
			actualGeneration = input.Assignment.ActualGeneration
		}
		action.IdempotencyKey = fmt.Sprintf("slot/%s/generation/%d/epoch/%d/actual/%d/%s", action.SlotID, action.DesiredGeneration, action.ExecutionEpoch, actualGeneration, action.Kind)
	}
	return action
}

func deterministicJobID(idempotencyKey string) string {
	digest := sha256.Sum256([]byte(idempotencyKey))
	return hex.EncodeToString(digest[:16])
}
