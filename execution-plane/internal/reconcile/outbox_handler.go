package reconcile

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/outbox"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
)

type AccountRuntimeDesired struct {
	AccountID          int64
	SlotID             string
	Provider           string
	DesiredGeneration  uint64
	RequiredLabels     map[string]string
	ImageDigest        string
	CPURequestMillis   uint64
	MemoryRequestBytes uint64
}

type AccountRuntimeSource interface {
	LoadAccountRuntimeDesired(ctx context.Context, accountID int64, desiredGeneration uint64) (AccountRuntimeDesired, error)
}

type OutboxHandler struct {
	source     AccountRuntimeSource
	projection store.LifecycleEventApplyRepository
	now        func() time.Time
}

func NewOutboxHandler(source AccountRuntimeSource, projection store.LifecycleEventApplyRepository, now func() time.Time) (*OutboxHandler, error) {
	if source == nil || projection == nil {
		return nil, errors.New("account runtime source and lifecycle projection repository are required")
	}
	if now == nil {
		now = time.Now
	}
	return &OutboxHandler{source: source, projection: projection, now: now}, nil
}

func (h *OutboxHandler) ApplyRuntimeEvent(ctx context.Context, event outbox.Event) error {
	if err := event.Validate(); err != nil {
		return outbox.BlockingHandlerError(outbox.FailureIntegrity, "lifecycle_event_invalid", err)
	}
	desiredState, err := desiredStateForEvent(event.EventType)
	if err != nil {
		return outbox.BlockingHandlerError(outbox.FailureIntegrity, "lifecycle_event_type_invalid", err)
	}
	anchor := store.LifecycleEventAnchor{
		SourceSequence: event.Sequence, EventID: event.EventID, AccountID: event.AccountID,
		EventType: event.EventType, DesiredGeneration: event.DesiredGeneration,
		PayloadSHA256: sha256.Sum256(event.PayloadJSON), EventCreatedAt: event.CreatedAt,
	}
	if _, found, err := h.projection.LookupLifecycleEventApply(ctx, anchor); err != nil {
		wrapped := fmt.Errorf("lookup lifecycle event apply receipt: %w", err)
		if errors.Is(err, store.ErrLifecycleEventConflict) || errors.Is(err, store.ErrInvalidLifecycleApply) {
			return outbox.BlockingHandlerError(outbox.FailureIntegrity, "lifecycle_replay_conflict", wrapped)
		}
		return retryRuntimeEvent(h.now, "lifecycle_receipt_unavailable", wrapped)
	} else if found {
		return nil
	}
	desired, err := h.source.LoadAccountRuntimeDesired(ctx, event.AccountID, event.DesiredGeneration)
	if err != nil {
		wrapped := fmt.Errorf("load CCMAX account runtime desired state: %w", err)
		if errors.Is(err, ErrCCMAXRuntimeDesired) {
			return outbox.BlockingHandlerError(outbox.FailureIntegrity, "ccmax_runtime_identity_invalid", wrapped)
		}
		return retryRuntimeEvent(h.now, "ccmax_runtime_source_unavailable", wrapped)
	}
	if desired.AccountID != event.AccountID || desired.DesiredGeneration != event.DesiredGeneration {
		return outbox.BlockingHandlerError(
			outbox.FailureIntegrity,
			"ccmax_runtime_identity_mismatch",
			errors.New("CCMAX account runtime desired generation does not match outbox event"),
		)
	}
	now := h.now().UTC().Truncate(time.Microsecond)
	eventCreatedAt := event.CreatedAt.UTC().Truncate(time.Microsecond)
	if now.Before(eventCreatedAt) {
		now = eventCreatedAt
	}
	_, _, err = h.projection.ApplyLifecycleEvent(ctx, store.LifecycleEventApply{
		Anchor: anchor,
		Slot: store.Slot{
			ID: desired.SlotID, AccountID: strconv.FormatInt(desired.AccountID, 10), Provider: desired.Provider,
			DesiredState: desiredState, DesiredGeneration: desired.DesiredGeneration,
			RequiredLabels: desired.RequiredLabels, ImageDigest: desired.ImageDigest,
			CPURequestMillis: desired.CPURequestMillis, MemoryRequestBytes: desired.MemoryRequestBytes,
			CreatedAt: now, UpdatedAt: now,
		},
		AppliedAt: now,
	})
	if err != nil {
		wrapped := fmt.Errorf("persist slot desired state from CCMAX outbox: %w", err)
		if errors.Is(err, store.ErrLifecycleEventConflict) || errors.Is(err, store.ErrInvalidLifecycleApply) ||
			errors.Is(err, store.ErrStaleGeneration) {
			return outbox.BlockingHandlerError(outbox.FailureIntegrity, "lifecycle_projection_conflict", wrapped)
		}
		return retryRuntimeEvent(h.now, "lifecycle_projection_unavailable", wrapped)
	}
	return nil
}

func desiredStateForEvent(eventType string) (string, error) {
	switch eventType {
	case "account.runtime.restore_requested", "account.proxy.change_requested":
		return "ready", nil
	case "account.runtime.drain_requested":
		return "drained", nil
	case "account.runtime.destroy_requested":
		return "absent", nil
	default:
		return "", errors.New("unsupported CCMAX runtime outbox event type")
	}
}

var _ outbox.Handler = (*OutboxHandler)(nil)
