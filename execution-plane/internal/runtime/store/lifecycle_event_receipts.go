package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

var (
	ErrLifecycleEventConflict = errors.New("lifecycle event conflicts with a durable apply receipt")
	ErrInvalidLifecycleApply  = errors.New("lifecycle event apply is invalid")
)

// LifecycleEventAnchor is the complete immutable identity of a CCMAX outbox
// event. PayloadSHA256 freezes the exact payload bytes without copying event
// payloads (which must remain metadata-only) into worker_runtime.
type LifecycleEventAnchor struct {
	SourceSequence    int64
	EventID           string
	AccountID         int64
	EventType         string
	DesiredGeneration uint64
	PayloadSHA256     [32]byte
	EventCreatedAt    time.Time
}

type LifecycleEventApply struct {
	Anchor    LifecycleEventAnchor
	Slot      Slot
	AppliedAt time.Time
}

type LifecycleEventApplyReceipt struct {
	Anchor    LifecycleEventAnchor
	Slot      Slot
	AppliedAt time.Time
}

// LifecycleEventApplyRepository provides a durable idempotency boundary around
// a lifecycle event and its slot projection. Lookup must be called before a
// mutable CCMAX desired-state source is consulted; Apply repeats the check in
// the write transaction to fence concurrent consumers.
type LifecycleEventApplyRepository interface {
	LookupLifecycleEventApply(ctx context.Context, anchor LifecycleEventAnchor) (LifecycleEventApplyReceipt, bool, error)
	ApplyLifecycleEvent(ctx context.Context, apply LifecycleEventApply) (LifecycleEventApplyReceipt, bool, error)
}

func (r *Repository) LookupLifecycleEventApply(
	ctx context.Context,
	anchor LifecycleEventAnchor,
) (LifecycleEventApplyReceipt, bool, error) {
	if err := validateLifecycleEventAnchor(anchor); err != nil {
		return LifecycleEventApplyReceipt{}, false, err
	}
	receipt, err := getLifecycleEventApplyReceiptByEventID(ctx, r.db, anchor.EventID, false)
	if err == nil {
		if !sameLifecycleEventAnchor(receipt.Anchor, anchor) {
			return LifecycleEventApplyReceipt{}, false, ErrLifecycleEventConflict
		}
		return receipt, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return LifecycleEventApplyReceipt{}, false, fmt.Errorf("read lifecycle event apply receipt by event id: %w", err)
	}
	receipt, err = getLifecycleEventApplyReceiptBySequence(ctx, r.db, anchor.SourceSequence, false)
	if err == nil {
		// Under READ COMMITTED an exact concurrent writer can commit between
		// the event-id lookup and this sequence lookup. Treat that complete
		// immutable-anchor match as a replay; only a different anchor owns the
		// sequence in conflict.
		if !sameLifecycleEventAnchor(receipt.Anchor, anchor) {
			return LifecycleEventApplyReceipt{}, false, ErrLifecycleEventConflict
		}
		return receipt, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return LifecycleEventApplyReceipt{}, false, fmt.Errorf("read lifecycle event apply receipt by sequence: %w", err)
	}
	return LifecycleEventApplyReceipt{}, false, nil
}

func (r *Repository) ApplyLifecycleEvent(
	ctx context.Context,
	apply LifecycleEventApply,
) (LifecycleEventApplyReceipt, bool, error) {
	labels, err := validateLifecycleEventApply(apply)
	if err != nil {
		return LifecycleEventApplyReceipt{}, false, err
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return LifecycleEventApplyReceipt{}, false, fmt.Errorf("begin lifecycle event apply: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	receipt, err := getLifecycleEventApplyReceiptByEventID(ctx, tx, apply.Anchor.EventID, true)
	if err == nil {
		if !sameLifecycleEventAnchor(receipt.Anchor, apply.Anchor) {
			return LifecycleEventApplyReceipt{}, false, ErrLifecycleEventConflict
		}
		if err := tx.Commit(); err != nil {
			return LifecycleEventApplyReceipt{}, false, fmt.Errorf("commit lifecycle event replay: %w", err)
		}
		return receipt, false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return LifecycleEventApplyReceipt{}, false, fmt.Errorf("lock lifecycle event apply receipt by event id: %w", err)
	}
	receipt, err = getLifecycleEventApplyReceiptBySequence(ctx, tx, apply.Anchor.SourceSequence, true)
	if err == nil {
		// A concurrent exact apply may have committed after the event-id
		// locking read missed. The sequence locking read observes that commit
		// under READ COMMITTED, so compare the full anchor before deciding
		// whether this is replay or conflict.
		if !sameLifecycleEventAnchor(receipt.Anchor, apply.Anchor) {
			return LifecycleEventApplyReceipt{}, false, ErrLifecycleEventConflict
		}
		if err := tx.Commit(); err != nil {
			return LifecycleEventApplyReceipt{}, false, fmt.Errorf("commit lifecycle event replay: %w", err)
		}
		return receipt, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return LifecycleEventApplyReceipt{}, false, fmt.Errorf("lock lifecycle event apply receipt by sequence: %w", err)
	}

	if err := putLifecycleDesiredSlot(ctx, tx, apply.Slot, labels); err != nil {
		return LifecycleEventApplyReceipt{}, false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO lifecycle_event_apply_receipts (
  source_sequence, event_id, event_account_id, event_type, desired_generation,
  event_payload_sha256, event_created_at, slot_id, slot_account_id, provider,
  desired_state, required_labels_json, image_digest, cpu_request_millis,
  memory_request_bytes, applied_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		apply.Anchor.SourceSequence, apply.Anchor.EventID, apply.Anchor.AccountID, apply.Anchor.EventType,
		apply.Anchor.DesiredGeneration, apply.Anchor.PayloadSHA256[:], normalizeLifecycleTime(apply.Anchor.EventCreatedAt),
		apply.Slot.ID, apply.Slot.AccountID, apply.Slot.Provider, apply.Slot.DesiredState, labels,
		apply.Slot.ImageDigest, apply.Slot.CPURequestMillis, apply.Slot.MemoryRequestBytes,
		normalizeLifecycleTime(apply.AppliedAt),
	)
	if err != nil {
		_ = tx.Rollback()
		if replay, found, lookupErr := r.LookupLifecycleEventApply(ctx, apply.Anchor); lookupErr != nil {
			if errors.Is(lookupErr, ErrLifecycleEventConflict) {
				return LifecycleEventApplyReceipt{}, false, lookupErr
			}
		} else if found {
			return replay, false, nil
		}
		return LifecycleEventApplyReceipt{}, false, fmt.Errorf("insert lifecycle event apply receipt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		if replay, found, lookupErr := r.LookupLifecycleEventApply(ctx, apply.Anchor); lookupErr != nil {
			if errors.Is(lookupErr, ErrLifecycleEventConflict) {
				return LifecycleEventApplyReceipt{}, false, lookupErr
			}
		} else if found {
			return replay, false, nil
		}
		return LifecycleEventApplyReceipt{}, false, fmt.Errorf("commit lifecycle event apply: %w", err)
	}
	apply.Slot.RequiredLabels = cloneLabels(apply.Slot.RequiredLabels)
	return LifecycleEventApplyReceipt{Anchor: apply.Anchor, Slot: apply.Slot, AppliedAt: normalizeLifecycleTime(apply.AppliedAt)}, true, nil
}

func putLifecycleDesiredSlot(ctx context.Context, tx *sql.Tx, candidate Slot, labels []byte) error {
	existing, err := getSlot(ctx, tx, candidate.ID, true)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `
INSERT INTO slots (
  slot_id, account_id, provider, desired_state, desired_generation, next_execution_epoch,
  required_labels_json, image_digest, cpu_request_millis, memory_request_bytes, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?)`,
			candidate.ID, candidate.AccountID, candidate.Provider, candidate.DesiredState, candidate.DesiredGeneration,
			labels, candidate.ImageDigest, candidate.CPURequestMillis, candidate.MemoryRequestBytes,
			normalizeLifecycleTime(candidate.CreatedAt), normalizeLifecycleTime(candidate.UpdatedAt),
		)
		if err != nil {
			return fmt.Errorf("insert lifecycle desired slot: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("lock lifecycle desired slot: %w", err)
	}
	if candidate.AccountID != existing.AccountID || candidate.Provider != existing.Provider ||
		candidate.DesiredGeneration < existing.DesiredGeneration {
		return ErrStaleGeneration
	}
	if candidate.DesiredGeneration == existing.DesiredGeneration {
		existingLabels, marshalErr := json.Marshal(existing.RequiredLabels)
		if marshalErr != nil || candidate.DesiredState != existing.DesiredState || candidate.ImageDigest != existing.ImageDigest ||
			candidate.CPURequestMillis != existing.CPURequestMillis || candidate.MemoryRequestBytes != existing.MemoryRequestBytes ||
			!bytes.Equal(labels, existingLabels) {
			return ErrStaleGeneration
		}
		return nil
	}
	_, err = tx.ExecContext(ctx, `
UPDATE slots SET desired_state = ?, desired_generation = ?, required_labels_json = ?,
  image_digest = ?, cpu_request_millis = ?, memory_request_bytes = ?, updated_at = ?
WHERE slot_id = ?`, candidate.DesiredState, candidate.DesiredGeneration, labels,
		candidate.ImageDigest, candidate.CPURequestMillis, candidate.MemoryRequestBytes,
		normalizeLifecycleTime(candidate.UpdatedAt), candidate.ID)
	if err != nil {
		return fmt.Errorf("update lifecycle desired slot: %w", err)
	}
	return nil
}

func validateLifecycleEventApply(apply LifecycleEventApply) ([]byte, error) {
	if err := validateLifecycleEventAnchor(apply.Anchor); err != nil {
		return nil, err
	}
	labels, err := validateDesiredSlot(apply.Slot)
	if err != nil || apply.AppliedAt.IsZero() || apply.AppliedAt.Before(apply.Anchor.EventCreatedAt) ||
		apply.Slot.AccountID != strconv.FormatInt(apply.Anchor.AccountID, 10) ||
		apply.Slot.DesiredGeneration != apply.Anchor.DesiredGeneration ||
		apply.Slot.DesiredState != lifecycleDesiredState(apply.Anchor.EventType) {
		return nil, ErrInvalidLifecycleApply
	}
	return labels, nil
}

func validateLifecycleEventAnchor(anchor LifecycleEventAnchor) error {
	zeroDigest := [32]byte{}
	if anchor.SourceSequence <= 0 || anchor.EventID == "" || len(anchor.EventID) > 128 ||
		anchor.AccountID <= 0 || anchor.DesiredGeneration == 0 || anchor.PayloadSHA256 == zeroDigest ||
		anchor.EventCreatedAt.IsZero() || lifecycleDesiredState(anchor.EventType) == "" {
		return ErrInvalidLifecycleApply
	}
	return nil
}

func lifecycleDesiredState(eventType string) string {
	switch eventType {
	case "account.runtime.restore_requested", "account.proxy.change_requested":
		return "ready"
	case "account.runtime.drain_requested":
		return "drained"
	case "account.runtime.destroy_requested":
		return "absent"
	default:
		return ""
	}
}

func sameLifecycleEventAnchor(left, right LifecycleEventAnchor) bool {
	return left.SourceSequence == right.SourceSequence && left.EventID == right.EventID &&
		left.AccountID == right.AccountID && left.EventType == right.EventType &&
		left.DesiredGeneration == right.DesiredGeneration && left.PayloadSHA256 == right.PayloadSHA256 &&
		normalizeLifecycleTime(left.EventCreatedAt).Equal(normalizeLifecycleTime(right.EventCreatedAt))
}

func normalizeLifecycleTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func getLifecycleEventApplyReceiptByEventID(
	ctx context.Context,
	queryer slotQueryer,
	eventID string,
	forUpdate bool,
) (LifecycleEventApplyReceipt, error) {
	return getLifecycleEventApplyReceipt(ctx, queryer, "event_id", eventID, forUpdate)
}

func getLifecycleEventApplyReceiptBySequence(
	ctx context.Context,
	queryer slotQueryer,
	sequence int64,
	forUpdate bool,
) (LifecycleEventApplyReceipt, error) {
	return getLifecycleEventApplyReceipt(ctx, queryer, "source_sequence", sequence, forUpdate)
}

func getLifecycleEventApplyReceipt(
	ctx context.Context,
	queryer slotQueryer,
	lookupColumn string,
	lookupValue any,
	forUpdate bool,
) (LifecycleEventApplyReceipt, error) {
	if lookupColumn != "event_id" && lookupColumn != "source_sequence" {
		return LifecycleEventApplyReceipt{}, ErrInvalidLifecycleApply
	}
	query := `
SELECT source_sequence, event_id, event_account_id, event_type, desired_generation,
       event_payload_sha256, event_created_at, slot_id, slot_account_id, provider,
       desired_state, required_labels_json, image_digest, cpu_request_millis,
       memory_request_bytes, applied_at
FROM lifecycle_event_apply_receipts WHERE ` + lookupColumn + ` = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var receipt LifecycleEventApplyReceipt
	var digest, labels []byte
	err := queryer.QueryRowContext(ctx, query, lookupValue).Scan(
		&receipt.Anchor.SourceSequence, &receipt.Anchor.EventID, &receipt.Anchor.AccountID,
		&receipt.Anchor.EventType, &receipt.Anchor.DesiredGeneration, &digest,
		&receipt.Anchor.EventCreatedAt, &receipt.Slot.ID, &receipt.Slot.AccountID,
		&receipt.Slot.Provider, &receipt.Slot.DesiredState, &labels, &receipt.Slot.ImageDigest,
		&receipt.Slot.CPURequestMillis, &receipt.Slot.MemoryRequestBytes, &receipt.AppliedAt,
	)
	if err != nil {
		return LifecycleEventApplyReceipt{}, err
	}
	if len(digest) != len(receipt.Anchor.PayloadSHA256) {
		return LifecycleEventApplyReceipt{}, ErrLifecycleEventConflict
	}
	copy(receipt.Anchor.PayloadSHA256[:], digest)
	if err := json.Unmarshal(labels, &receipt.Slot.RequiredLabels); err != nil {
		return LifecycleEventApplyReceipt{}, fmt.Errorf("decode lifecycle receipt labels: %w", err)
	}
	receipt.Anchor.EventCreatedAt = normalizeLifecycleTime(receipt.Anchor.EventCreatedAt)
	receipt.AppliedAt = normalizeLifecycleTime(receipt.AppliedAt)
	receipt.Slot.DesiredGeneration = receipt.Anchor.DesiredGeneration
	receipt.Slot.CreatedAt = receipt.AppliedAt
	receipt.Slot.UpdatedAt = receipt.AppliedAt
	if _, err := validateLifecycleEventApply(LifecycleEventApply{
		Anchor: receipt.Anchor, Slot: receipt.Slot, AppliedAt: receipt.AppliedAt,
	}); err != nil {
		return LifecycleEventApplyReceipt{}, ErrLifecycleEventConflict
	}
	return receipt, nil
}

var _ LifecycleEventApplyRepository = (*Repository)(nil)
