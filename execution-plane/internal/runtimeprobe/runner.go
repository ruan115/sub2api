// Package runtimeprobe schedules bounded, read-only provider inspections. It
// never grants/renews leases, provisions runtimes, or issues worker tickets.
package runtimeprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"sync"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var (
	ErrConfiguration = errors.New("runtime probe configuration is invalid")
	ErrScan          = errors.New("runtime probe scan failed")
	ErrBusy          = errors.New("runtime probe step is already running")
	sessionPattern   = regexp.MustCompile(`^[a-f0-9]{32}$`)
	imagePattern     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

type Sessions interface {
	ValidateControlSession(context.Context, string, string) error
}

type Leases interface {
	Validate(context.Context, lease.Claim) error
}

type Dispatcher interface {
	DispatchToSession(context.Context, string, string, *executionv1.NodeControlServiceControlResponse) error
}

type Config struct {
	Repository             store.ProbeBindingRepository
	Sessions               Sessions
	Leases                 Leases
	Dispatcher             Dispatcher
	BatchSize              int
	PollInterval           time.Duration
	RefreshInterval        time.Duration
	MaxNodeAge             time.Duration
	CheckTimeout           time.Duration
	CommandTTL             time.Duration
	MaxConsecutiveFailures int
	Now                    func() time.Time
}

type Result struct {
	Candidates, Dispatched, Skipped, Denied int
}

type Runner struct {
	config Config
	stepMu sync.Mutex
	after  string
}

func New(config Config) (*Runner, error) {
	if config.Repository == nil || config.Sessions == nil || config.Leases == nil || config.Dispatcher == nil {
		return nil, ErrConfiguration
	}
	if config.BatchSize == 0 {
		config.BatchSize = 100
	}
	if config.PollInterval == 0 {
		config.PollInterval = time.Second
	}
	if config.RefreshInterval == 0 {
		config.RefreshInterval = 15 * time.Second
	}
	if config.MaxNodeAge == 0 {
		config.MaxNodeAge = 45 * time.Second
	}
	if config.CheckTimeout == 0 {
		config.CheckTimeout = 2 * time.Second
	}
	if config.CommandTTL == 0 {
		config.CommandTTL = 10 * time.Second
	}
	if config.MaxConsecutiveFailures == 0 {
		config.MaxConsecutiveFailures = 5
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.BatchSize < 1 || config.BatchSize > 100 || config.PollInterval <= 0 || config.PollInterval > 10*time.Second ||
		config.RefreshInterval <= 0 || config.RefreshInterval >= config.MaxNodeAge ||
		config.MaxNodeAge <= 0 || config.MaxNodeAge > 45*time.Second ||
		config.CheckTimeout <= 0 || config.CheckTimeout > 10*time.Second ||
		config.CommandTTL <= 0 || config.CommandTTL > 10*time.Second ||
		config.MaxConsecutiveFailures < 1 || config.MaxConsecutiveFailures > 100 {
		return nil, ErrConfiguration
	}
	return &Runner{config: config}, nil
}

// Step scans at most one page. It has no growing per-slot cache. The control
// server owns in-flight deduplication; only a returned command result changes
// the observation clock. Denied nodes do not prevent the cursor advancing.
func (r *Runner) Step(ctx context.Context) (Result, error) {
	if r == nil || ctx == nil || r.config.Repository == nil || r.config.Sessions == nil || r.config.Leases == nil || r.config.Dispatcher == nil || r.config.Now == nil {
		return Result{}, ErrConfiguration
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if !r.stepMu.TryLock() {
		return Result{}, ErrBusy
	}
	defer r.stepMu.Unlock()
	queryCtx, cancel := context.WithTimeout(ctx, r.config.CheckTimeout)
	page, err := r.config.Repository.ListProbeBindings(queryCtx, r.after, r.config.Now().UTC(), r.config.MaxNodeAge, r.config.BatchSize)
	queryErr := queryCtx.Err()
	cancel()
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}
	if err != nil || queryErr != nil || !validPage(page, r.after, r.config.BatchSize) {
		return Result{}, ErrScan
	}
	// Preserve progress even if a later individual probe is denied or canceled.
	r.after = page.NextAfterSlotID
	result := Result{Candidates: len(page.Bindings)}
	for _, binding := range page.Bindings {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		switch r.probe(ctx, binding) {
		case dispatched:
			result.Dispatched++
		case skipped:
			result.Skipped++
		default:
			result.Denied++
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}

type outcome uint8

const (
	denied outcome = iota
	skipped
	dispatched
)

func (r *Runner) probe(parent context.Context, binding store.ProbeBinding) outcome {
	now := r.config.Now().UTC()
	if !r.current(binding, now) {
		return denied
	}
	if !r.due(binding, now) {
		return skipped
	}
	ctx, cancel := context.WithTimeout(parent, r.config.CheckTimeout)
	defer cancel()
	claim := lease.Claim{SlotID: binding.SlotID, NodeID: binding.NodeID, ExecutionEpoch: binding.ExecutionEpoch, OwnerID: binding.LeaseOwnerID}
	if r.config.Sessions.ValidateControlSession(ctx, binding.NodeID, binding.ControlSessionID) != nil || ctx.Err() != nil ||
		r.config.Leases.Validate(ctx, claim) != nil || ctx.Err() != nil {
		return denied
	}
	fresh, err := r.config.Repository.ReadProbeBinding(ctx, binding.SlotID, r.config.Now().UTC(), r.config.MaxNodeAge)
	if err != nil || ctx.Err() != nil || !sameBinding(binding, fresh) || !r.current(fresh, r.config.Now().UTC()) {
		return denied
	}
	if !r.due(fresh, r.config.Now().UTC()) {
		return skipped
	}
	// Recheck the independent authority after repository I/O. No acquire/renew
	// fallback exists if Redis is unavailable, revoked, or already expired.
	if r.config.Leases.Validate(ctx, claim) != nil || ctx.Err() != nil {
		return denied
	}
	now = r.config.Now().UTC()
	if !r.current(fresh, now) {
		return denied
	}
	deadline := now.Add(r.config.CommandTTL)
	if fresh.LeaseExpiresAt.Before(deadline) {
		deadline = fresh.LeaseExpiresAt
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return denied
	}
	command := &executionv1.SlotCommand{
		CommandId: "probe-" + hex.EncodeToString(nonce[:]), Action: executionv1.SlotCommandAction_SLOT_COMMAND_ACTION_INSPECT,
		AccountId: fresh.AccountID, SlotId: fresh.SlotID, ExecutionEpoch: fresh.ExecutionEpoch,
		ImageDigest: fresh.ImageDigest, Deadline: timestamppb.New(deadline),
		Metadata: map[string]string{"desired_generation": strconv.FormatUint(fresh.RouteGeneration, 10)},
	}
	response := &executionv1.NodeControlServiceControlResponse{Event: &executionv1.NodeControlServiceControlResponse_SlotCommand{SlotCommand: command}}
	if r.config.Dispatcher.DispatchToSession(ctx, fresh.NodeID, fresh.ControlSessionID, response) != nil || ctx.Err() != nil {
		return denied
	}
	return dispatched
}

func (r *Runner) current(b store.ProbeBinding, now time.Time) bool {
	for _, id := range []string{b.AccountID, b.SlotID, b.NodeID, b.LeaseOwnerID} {
		if credential.ValidateTransportID(id) != nil {
			return false
		}
	}
	return !now.IsZero() && sessionPattern.MatchString(b.ControlSessionID) && imagePattern.MatchString(b.ImageDigest) &&
		b.ExecutionEpoch > 0 && b.RouteGeneration > 0 && b.ProviderRef != "" && len(b.ProviderRef) <= 255 &&
		!b.NodeSeenAt.IsZero() && !b.NodeSeenAt.After(now) && b.NodeSeenAt.Add(r.config.MaxNodeAge).After(now) &&
		b.LeaseExpiresAt.After(now) && (b.LastObservedAt == nil || (!b.LastObservedAt.IsZero() && !b.LastObservedAt.After(now))) &&
		(b.ObservedControlSessionID == "" || sessionPattern.MatchString(b.ObservedControlSessionID))
}

func (r *Runner) due(b store.ProbeBinding, now time.Time) bool {
	return b.ObservedControlSessionID != b.ControlSessionID || b.LastObservedAt == nil || !b.LastObservedAt.Add(r.config.RefreshInterval).After(now)
}

func sameBinding(a, b store.ProbeBinding) bool {
	return a.AccountID == b.AccountID && a.SlotID == b.SlotID && a.NodeID == b.NodeID && a.ProviderRef == b.ProviderRef &&
		a.LeaseOwnerID == b.LeaseOwnerID && a.ControlSessionID == b.ControlSessionID && a.ImageDigest == b.ImageDigest &&
		a.ExecutionEpoch == b.ExecutionEpoch && a.RouteGeneration == b.RouteGeneration
}

func validPage(page store.ProbeBindingPage, after string, limit int) bool {
	if len(page.Bindings) > limit || (page.NextAfterSlotID != "" &&
		(credential.ValidateTransportID(page.NextAfterSlotID) != nil || page.NextAfterSlotID <= after)) {
		return false
	}
	previous := after
	for _, b := range page.Bindings {
		if credential.ValidateTransportID(b.SlotID) != nil || b.SlotID <= previous ||
			(page.NextAfterSlotID != "" && b.SlotID > page.NextAfterSlotID) {
			return false
		}
		previous = b.SlotID
	}
	return true
}

// Run is opt-in; production assembly is deliberately outside this slice.
// Persistent scan failures terminate; individual disconnected/busy nodes are
// expected denials. Context-aware dependencies are required for bounded exit.
func (r *Runner) Run(ctx context.Context) error {
	if r == nil || ctx == nil || r.config.Repository == nil || r.config.Now == nil {
		return ErrConfiguration
	}
	failures := 0
	for {
		_, err := r.Step(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			failures++
		} else {
			failures = 0
		}
		if failures >= r.config.MaxConsecutiveFailures {
			return ErrScan
		}
		timer := time.NewTimer(r.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
