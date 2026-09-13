package outbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type MySQLSource struct {
	db               *sql.DB
	createCheckpoint bool
}

func NewMySQLSource(db *sql.DB) (*MySQLSource, error) {
	if db == nil {
		return nil, errors.New("CCMAX MySQL database is required")
	}
	return &MySQLSource{db: db, createCheckpoint: true}, nil
}

// NewStrictMySQLSource requires the production checkpoint to be created by an
// explicit startup/bootstrap preflight. It never silently starts at sequence
// zero against a non-empty historical outbox.
func NewStrictMySQLSource(db *sql.DB) (*MySQLSource, error) {
	if db == nil {
		return nil, errors.New("CCMAX MySQL database is required")
	}
	return &MySQLSource{db: db}, nil
}

func (s *MySQLSource) Claim(ctx context.Context, consumerName, owner string, now time.Time, leaseTTL time.Duration) (ClaimedEvent, bool, error) {
	if err := validateClaim(consumerName, owner, now, leaseTTL); err != nil {
		return ClaimedEvent{}, false, err
	}
	now = now.UTC().Truncate(time.Millisecond)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ClaimedEvent{}, false, fmt.Errorf("begin runtime outbox claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if s.createCheckpoint {
		if _, err := tx.ExecContext(ctx, `INSERT IGNORE INTO runtime_outbox_consumers (consumer_name) VALUES (?)`, consumerName); err != nil {
			return ClaimedEvent{}, false, fmt.Errorf("create runtime outbox checkpoint: %w", err)
		}
	}
	var lastSequence, claimedSequence, leaseExpiresAt, failureSequence, nextAttemptAt int64
	var claimVersion, blockedClaimVersion, failureCount uint64
	var lockedBy, failureState, failureClass, failureCode string
	if err := tx.QueryRowContext(ctx, `SELECT
		last_sequence, claimed_sequence, locked_by, lease_expires_at, claim_version,
		failure_state, failure_sequence, failure_class, failure_code, failure_count, next_attempt_at, blocked_claim_version
		FROM runtime_outbox_consumers WHERE consumer_name = ? FOR UPDATE`, consumerName).Scan(
		&lastSequence, &claimedSequence, &lockedBy, &leaseExpiresAt, &claimVersion,
		&failureState, &failureSequence, &failureClass, &failureCode, &failureCount, &nextAttemptAt, &blockedClaimVersion,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) && !s.createCheckpoint {
			return ClaimedEvent{}, false, ErrCheckpointMissing
		}
		return ClaimedEvent{}, false, fmt.Errorf("lock runtime outbox checkpoint: %w", err)
	}
	if failureState == "blocked" {
		blocked := &BlockedError{
			ConsumerName: consumerName, Sequence: failureSequence, Class: FailureClass(failureClass),
			Code: failureCode, BlockedClaimVersion: blockedClaimVersion,
		}
		if lastSequence < 0 || failureSequence <= lastSequence || failureCount == 0 ||
			!validFailureClass(blocked.Class) || blocked.Class == FailureRetryable ||
			!failureCodePattern.MatchString(blocked.Code) || blockedClaimVersion == 0 ||
			claimedSequence != 0 || lockedBy != "" || leaseExpiresAt != 0 || nextAttemptAt != 0 {
			return ClaimedEvent{}, false, errors.New("runtime outbox blocked checkpoint is invalid")
		}
		return ClaimedEvent{}, false, blocked
	}
	if failureState != "ready" && failureState != "retry_wait" {
		return ClaimedEvent{}, false, errors.New("runtime outbox checkpoint failure state is invalid")
	}
	if failureState == "retry_wait" && (failureSequence <= lastSequence || failureClass != string(FailureRetryable) ||
		!failureCodePattern.MatchString(failureCode) || failureCount == 0 || nextAttemptAt <= 0 || blockedClaimVersion != 0 ||
		claimedSequence != 0 || lockedBy != "" || leaseExpiresAt != 0) {
		return ClaimedEvent{}, false, errors.New("runtime outbox retry checkpoint is invalid")
	}
	if failureState == "ready" && failureCount > 0 && (failureSequence <= lastSequence ||
		failureClass != string(FailureRetryable) || !failureCodePattern.MatchString(failureCode) || nextAttemptAt != 0 ||
		blockedClaimVersion != 0 || claimedSequence != failureSequence) {
		return ClaimedEvent{}, false, errors.New("runtime outbox active retry checkpoint is invalid")
	}
	if failureState == "ready" && failureCount == 0 && (failureSequence != 0 || failureClass != "" || failureCode != "" ||
		nextAttemptAt != 0 || blockedClaimVersion != 0) {
		return ClaimedEvent{}, false, errors.New("runtime outbox ready checkpoint is invalid")
	}
	nowMillis := now.UnixMilli()
	if failureState == "retry_wait" && nextAttemptAt > nowMillis {
		if err := tx.Commit(); err != nil {
			return ClaimedEvent{}, false, fmt.Errorf("commit runtime outbox retry wait: %w", err)
		}
		return ClaimedEvent{}, false, nil
	}
	if lockedBy != "" && lockedBy != owner && leaseExpiresAt > nowMillis {
		return ClaimedEvent{}, false, ErrBusy
	}
	var sequence int64
	err = tx.QueryRowContext(ctx, `SELECT sequence FROM runtime_outbox WHERE sequence > ? ORDER BY sequence LIMIT 1`, lastSequence).Scan(&sequence)
	if errors.Is(err, sql.ErrNoRows) {
		if claimedSequence != 0 {
			return ClaimedEvent{}, false, errors.New("runtime outbox claimed sequence has no event")
		}
		if err := tx.Commit(); err != nil {
			return ClaimedEvent{}, false, err
		}
		return ClaimedEvent{}, false, nil
	}
	if err != nil {
		return ClaimedEvent{}, false, fmt.Errorf("select next runtime outbox event: %w", err)
	}
	if claimedSequence != 0 && claimedSequence != sequence {
		return ClaimedEvent{}, false, errors.New("runtime outbox claimed sequence skips the ordered checkpoint")
	}
	if failureCount > 0 && failureSequence != sequence {
		return ClaimedEvent{}, false, errors.New("runtime outbox failure sequence skips the ordered checkpoint")
	}
	var event Event
	if err := tx.QueryRowContext(ctx, `SELECT sequence, event_id, account_id, event_type, desired_generation, payload_json, created_at
		FROM runtime_outbox WHERE sequence = ?`, sequence).Scan(
		&event.Sequence, &event.EventID, &event.AccountID, &event.EventType, &event.DesiredGeneration, &event.PayloadJSON, &event.CreatedAt,
	); err != nil {
		return ClaimedEvent{}, false, fmt.Errorf("read claimed runtime outbox event: %w", err)
	}
	leaseExpires := now.Add(leaseTTL).UTC().Truncate(time.Millisecond)
	result, err := tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET
		claimed_sequence = ?, locked_by = ?, lease_expires_at = ?, claim_version = claim_version + 1,
		failure_state = 'ready', next_attempt_at = 0, last_error = '', updated_at = UTC_TIMESTAMP(3)
		WHERE consumer_name = ? AND last_sequence = ? AND claim_version = ? AND failure_state <> 'blocked'`,
		sequence, owner, leaseExpires.UnixMilli(), consumerName, lastSequence, claimVersion)
	if err != nil {
		return ClaimedEvent{}, false, fmt.Errorf("claim runtime outbox event: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ClaimedEvent{}, false, ErrBusy
	}
	if err := tx.Commit(); err != nil {
		return ClaimedEvent{}, false, fmt.Errorf("commit runtime outbox claim: %w", err)
	}
	return ClaimedEvent{
		Event: event, ConsumerName: consumerName, Owner: owner,
		ClaimVersion: claimVersion + 1, LeaseExpiresAt: leaseExpires,
	}, true, nil
}

func (s *MySQLSource) Ack(ctx context.Context, claim ClaimedEvent, now time.Time) error {
	if claim.Validate() != nil || now.IsZero() {
		return errors.New("invalid runtime outbox completion")
	}
	now = now.UTC().Truncate(time.Millisecond)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var lastSequence, claimedSequence, leaseExpiresAt int64
	var claimVersion uint64
	var lockedBy string
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at, claim_version
		FROM runtime_outbox_consumers WHERE consumer_name = ? FOR UPDATE`, claim.ConsumerName).Scan(
		&lastSequence, &claimedSequence, &lockedBy, &leaseExpiresAt, &claimVersion,
	); err != nil {
		return err
	}
	if lastSequence >= claim.Event.Sequence {
		return tx.Commit()
	}
	if claimedSequence != claim.Event.Sequence || lockedBy != claim.Owner || claimVersion != claim.ClaimVersion ||
		leaseExpiresAt != claim.LeaseExpiresAt.UnixMilli() || leaseExpiresAt <= now.UnixMilli() {
		return ErrNotClaimed
	}
	result, err := tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET
		last_sequence = ?, claimed_sequence = 0, locked_by = '', lease_expires_at = 0,
		failure_state = 'ready', failure_sequence = 0, failure_class = '', failure_code = '',
		failure_count = 0, first_failed_at = 0, last_failed_at = 0, next_attempt_at = 0,
		blocked_claim_version = 0, last_error = '', updated_at = UTC_TIMESTAMP(3)
		WHERE consumer_name = ? AND claimed_sequence = ? AND locked_by = ? AND claim_version = ?`,
		claim.Event.Sequence, claim.ConsumerName, claim.Event.Sequence, claim.Owner, claim.ClaimVersion)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrNotClaimed
	}
	return tx.Commit()
}

func (s *MySQLSource) Fail(ctx context.Context, claim ClaimedEvent, failure Failure) error {
	if claim.ValidateToken() != nil || failure.Validate() != nil || !claim.LeaseExpiresAt.After(failure.FailedAt) {
		return errors.New("invalid runtime outbox failure")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var claimedSequence, leaseExpiresAt int64
	var claimVersion, failureCount uint64
	var lockedBy string
	if err := tx.QueryRowContext(ctx, `SELECT claimed_sequence, locked_by, lease_expires_at, claim_version, failure_count
		FROM runtime_outbox_consumers WHERE consumer_name = ? FOR UPDATE`, claim.ConsumerName).Scan(
		&claimedSequence, &lockedBy, &leaseExpiresAt, &claimVersion, &failureCount,
	); err != nil {
		return err
	}
	if claimedSequence != claim.Event.Sequence || lockedBy != claim.Owner || claimVersion != claim.ClaimVersion ||
		leaseExpiresAt != claim.LeaseExpiresAt.UnixMilli() || leaseExpiresAt <= failure.FailedAt.UnixMilli() {
		return ErrNotClaimed
	}
	failureState := "blocked"
	nextAttemptAt := int64(0)
	blockedClaimVersion := claim.ClaimVersion
	retryExhausted := failure.Class == FailureRetryable && failureCount+1 >= failure.RetryLimit
	if failure.Class == FailureRetryable {
		failureState = "retry_wait"
		nextAttemptAt = failure.RetryAfter.UnixMilli()
		blockedClaimVersion = 0
	}
	result, err := tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET
		claimed_sequence = 0, locked_by = '', lease_expires_at = 0,
		failure_state = ?, failure_sequence = ?, failure_class = ?, failure_code = ?,
		failure_count = failure_count + 1,
		first_failed_at = CASE WHEN first_failed_at = 0 THEN ? ELSE first_failed_at END,
		last_failed_at = ?, next_attempt_at = ?, blocked_claim_version = ?,
		last_error = ?, updated_at = UTC_TIMESTAMP(3)
		WHERE consumer_name = ? AND claimed_sequence = ? AND locked_by = ? AND claim_version = ?`,
		failureState, claim.Event.Sequence, string(failure.Class), failure.Code,
		failure.FailedAt.UnixMilli(), failure.FailedAt.UnixMilli(), nextAttemptAt, blockedClaimVersion,
		failure.Code, claim.ConsumerName, claim.Event.Sequence, claim.Owner, claim.ClaimVersion,
	)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrNotClaimed
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if retryExhausted {
		return &RetryBudgetError{
			ConsumerName: claim.ConsumerName, Sequence: claim.Event.Sequence,
			Code: failure.Code, FailureCount: failureCount + 1,
		}
	}
	return nil
}

// retryBlocked is deliberately unexported and never called by Runner. The
// production CCMAX admin resolver performs exact retry plus audit in its own
// atomic transaction instead of exposing a general force-ack here; this helper
// remains only for repository-model regression tests.
func (s *MySQLSource) retryBlocked(
	ctx context.Context,
	consumerName string,
	expectedSequence int64,
	expectedClaimVersion uint64,
) error {
	if consumerName == "" || expectedSequence <= 0 || expectedClaimVersion == 0 {
		return errors.New("invalid runtime outbox retry authorization")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET
		failure_state = 'ready', failure_sequence = 0, failure_class = '', failure_code = '',
		failure_count = 0, first_failed_at = 0, last_failed_at = 0,
		next_attempt_at = 0, blocked_claim_version = 0,
		last_error = '', updated_at = UTC_TIMESTAMP(3)
		WHERE consumer_name = ? AND failure_state = 'blocked' AND failure_sequence = ?
		  AND blocked_claim_version = ? AND claimed_sequence = 0 AND locked_by = ''`,
		consumerName, expectedSequence, expectedClaimVersion,
	)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ErrNotClaimed
	}
	return nil
}

var _ Source = (*MySQLSource)(nil)
