package reconcile

import (
	"context"
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
	source AccountRuntimeSource
	slots  store.SlotRepository
	now    func() time.Time
}

func NewOutboxHandler(source AccountRuntimeSource, slots store.SlotRepository, now func() time.Time) (*OutboxHandler, error) {
	if source == nil || slots == nil {
		return nil, errors.New("account runtime source and slot repository are required")
	}
	if now == nil {
		now = time.Now
	}
	return &OutboxHandler{source: source, slots: slots, now: now}, nil
}

func (h *OutboxHandler) ApplyRuntimeEvent(ctx context.Context, event outbox.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	desiredState, err := desiredStateForEvent(event.EventType)
	if err != nil {
		return err
	}
	desired, err := h.source.LoadAccountRuntimeDesired(ctx, event.AccountID, event.DesiredGeneration)
	if err != nil {
		return fmt.Errorf("load CCMAX account runtime desired state: %w", err)
	}
	if desired.AccountID != event.AccountID || desired.DesiredGeneration != event.DesiredGeneration {
		return errors.New("CCMAX account runtime desired generation does not match outbox event")
	}
	now := h.now().UTC()
	_, err = h.slots.PutDesiredSlot(ctx, store.Slot{
		ID: desired.SlotID, AccountID: strconv.FormatInt(desired.AccountID, 10), Provider: desired.Provider,
		DesiredState: desiredState, DesiredGeneration: desired.DesiredGeneration,
		RequiredLabels: desired.RequiredLabels, ImageDigest: desired.ImageDigest,
		CPURequestMillis: desired.CPURequestMillis, MemoryRequestBytes: desired.MemoryRequestBytes,
		CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		return fmt.Errorf("persist slot desired state from CCMAX outbox: %w", err)
	}
	return nil
}

func desiredStateForEvent(eventType string) (string, error) {
	switch eventType {
	case "account.runtime.provision_requested", "account.runtime.restore_requested",
		"account.credential.migrate_requested", "account.credential.rotate_requested",
		"account.proxy.change_requested":
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
