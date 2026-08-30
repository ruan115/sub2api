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
		case ActualMissing, ActualDestroyed:
			kind = ActionCreate
		case ActualCreated, ActualStopped:
			kind = ActionStart
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
	}
	if kind != ActionNone {
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
