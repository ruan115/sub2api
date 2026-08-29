package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type MySQLSource struct {
	db *sql.DB
}

func NewMySQLSource(db *sql.DB) (*MySQLSource, error) {
	if db == nil {
		return nil, errors.New("CCMAX MySQL database is required")
	}
	return &MySQLSource{db: db}, nil
}

func (s *MySQLSource) Claim(ctx context.Context, consumerName, owner string, now time.Time, leaseTTL time.Duration) (Event, bool, error) {
	if err := validateClaim(consumerName, owner, now, leaseTTL); err != nil {
		return Event{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return Event{}, false, fmt.Errorf("begin runtime outbox claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO runtime_outbox_consumers (consumer_name) VALUES (?)`, consumerName); err != nil {
		return Event{}, false, fmt.Errorf("create runtime outbox checkpoint: %w", err)
	}
	var lastSequence, claimedSequence, leaseExpiresAt int64
	var lockedBy string
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at
		FROM runtime_outbox_consumers WHERE consumer_name = ? FOR UPDATE`, consumerName).Scan(
		&lastSequence, &claimedSequence, &lockedBy, &leaseExpiresAt,
	); err != nil {
		return Event{}, false, fmt.Errorf("lock runtime outbox checkpoint: %w", err)
	}
	nowMillis := now.UTC().UnixMilli()
	if lockedBy != "" && lockedBy != owner && leaseExpiresAt > nowMillis {
		return Event{}, false, ErrBusy
	}
	sequence := claimedSequence
	if sequence <= lastSequence {
		err = tx.QueryRowContext(ctx, `SELECT sequence FROM runtime_outbox WHERE sequence > ? ORDER BY sequence LIMIT 1`, lastSequence).Scan(&sequence)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.Commit(); err != nil {
				return Event{}, false, err
			}
			return Event{}, false, nil
		}
		if err != nil {
			return Event{}, false, fmt.Errorf("select next runtime outbox event: %w", err)
		}
	}
	var event Event
	if err := tx.QueryRowContext(ctx, `SELECT sequence, event_id, account_id, event_type, desired_generation, payload_json, created_at
		FROM runtime_outbox WHERE sequence = ?`, sequence).Scan(
		&event.Sequence, &event.EventID, &event.AccountID, &event.EventType, &event.DesiredGeneration, &event.PayloadJSON, &event.CreatedAt,
	); err != nil {
		return Event{}, false, fmt.Errorf("read claimed runtime outbox event: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET
		claimed_sequence = ?, locked_by = ?, lease_expires_at = ?, last_error = '', updated_at = UTC_TIMESTAMP(3)
		WHERE consumer_name = ? AND last_sequence = ?`, sequence, owner, now.Add(leaseTTL).UTC().UnixMilli(), consumerName, lastSequence)
	if err != nil {
		return Event{}, false, fmt.Errorf("claim runtime outbox event: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return Event{}, false, ErrBusy
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, fmt.Errorf("commit runtime outbox claim: %w", err)
	}
	return event, true, nil
}

func (s *MySQLSource) Ack(ctx context.Context, consumerName, owner string, sequence int64, now time.Time) error {
	return s.finish(ctx, consumerName, owner, sequence, "", now, true)
}

func (s *MySQLSource) Nack(ctx context.Context, consumerName, owner string, sequence int64, errorCode string, now time.Time) error {
	if !validErrorCode(errorCode) {
		return errors.New("invalid runtime outbox failure code")
	}
	return s.finish(ctx, consumerName, owner, sequence, errorCode, now, false)
}

func (s *MySQLSource) finish(ctx context.Context, consumerName, owner string, sequence int64, errorCode string, now time.Time, acknowledge bool) error {
	if consumerName == "" || owner == "" || sequence <= 0 || now.IsZero() {
		return errors.New("invalid runtime outbox completion")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var lastSequence, claimedSequence, leaseExpiresAt int64
	var lockedBy string
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at
		FROM runtime_outbox_consumers WHERE consumer_name = ? FOR UPDATE`, consumerName).Scan(
		&lastSequence, &claimedSequence, &lockedBy, &leaseExpiresAt,
	); err != nil {
		return err
	}
	if acknowledge && lastSequence >= sequence {
		return tx.Commit()
	}
	if claimedSequence != sequence || lockedBy != owner || (acknowledge && leaseExpiresAt <= now.UTC().UnixMilli()) {
		return ErrNotClaimed
	}
	if acknowledge {
		_, err = tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET
			last_sequence = ?, claimed_sequence = 0, locked_by = '', lease_expires_at = 0,
			last_error = '', updated_at = UTC_TIMESTAMP(3) WHERE consumer_name = ?`, sequence, consumerName)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET
			claimed_sequence = 0, locked_by = '', lease_expires_at = 0,
			last_error = ?, updated_at = UTC_TIMESTAMP(3) WHERE consumer_name = ?`, errorCode, consumerName)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

var _ Source = (*MySQLSource)(nil)
