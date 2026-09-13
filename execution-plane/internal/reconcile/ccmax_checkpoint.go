package reconcile

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

var (
	ErrCCMAXRuntimeCheckpointInvalid           = errors.New("CCMAX runtime outbox checkpoint is invalid")
	ErrCCMAXRuntimeCheckpointBootstrapRequired = errors.New("CCMAX runtime outbox checkpoint requires audited bootstrap")
	ErrCCMAXRuntimeCheckpointBlocked           = errors.New("CCMAX runtime outbox checkpoint is blocked")
	ccmaxCheckpointFailureCode                 = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
)

type ccmaxCheckpointFailure struct {
	state               string
	sequence            int64
	class               string
	code                string
	count               uint64
	nextAttemptAt       int64
	blockedClaimVersion uint64
}

// EnsureCCMAXRuntimeConsumerCheckpoint creates sequence zero only while the
// outbox is empty. A non-empty historical outbox requires an explicit audited
// watermark/snapshot migration; production must never silently replay an
// unknown legacy payload history or skip it without operator authorization.
func EnsureCCMAXRuntimeConsumerCheckpoint(ctx context.Context, db *sql.DB, consumerName string) error {
	if ctx == nil || ctx.Err() != nil || db == nil || credential.ValidateTransportID(consumerName) != nil {
		return ErrCCMAXRuntimeCheckpointInvalid
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin CCMAX runtime checkpoint preflight: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var lastSequence, claimedSequence, leaseExpiresAt int64
	var lockedBy string
	var failure ccmaxCheckpointFailure
	err = tx.QueryRowContext(ctx, `
SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at,
       failure_state, failure_sequence, failure_class, failure_code, failure_count,
       next_attempt_at, blocked_claim_version
FROM runtime_outbox_consumers
WHERE consumer_name = ?
FOR UPDATE`, consumerName).Scan(
		&lastSequence, &claimedSequence, &lockedBy, &leaseExpiresAt,
		&failure.state, &failure.sequence, &failure.class, &failure.code, &failure.count,
		&failure.nextAttemptAt, &failure.blockedClaimVersion,
	)
	found := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lock CCMAX runtime checkpoint: %w", err)
	}

	var newestSequence int64
	err = tx.QueryRowContext(ctx, `
SELECT sequence
FROM runtime_outbox
ORDER BY sequence DESC
LIMIT 1
FOR UPDATE`).Scan(&newestSequence)
	hasNewest := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lock newest CCMAX runtime outbox sequence: %w", err)
	}

	if !found {
		if hasNewest {
			return ErrCCMAXRuntimeCheckpointBootstrapRequired
		}
		result, err := tx.ExecContext(ctx, `
INSERT IGNORE INTO runtime_outbox_consumers (consumer_name)
VALUES (?)`, consumerName)
		if err != nil {
			return fmt.Errorf("create empty CCMAX runtime checkpoint: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected < 0 || affected > 1 {
			return ErrCCMAXRuntimeCheckpointInvalid
		}
		if affected == 0 {
			if err := tx.QueryRowContext(ctx, `
SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at,
			       failure_state, failure_sequence, failure_class, failure_code, failure_count,
			       next_attempt_at, blocked_claim_version
			FROM runtime_outbox_consumers
			WHERE consumer_name = ?
			FOR UPDATE`, consumerName).Scan(
				&lastSequence, &claimedSequence, &lockedBy, &leaseExpiresAt,
				&failure.state, &failure.sequence, &failure.class, &failure.code, &failure.count,
				&failure.nextAttemptAt, &failure.blockedClaimVersion,
			); err != nil ||
				!validCCMAXRuntimeCheckpoint(lastSequence, claimedSequence, lockedBy, leaseExpiresAt, 0, false, 0, false, lastSequence == 0) ||
				!validCCMAXRuntimeCheckpointFailure(failure, lastSequence, claimedSequence, lockedBy, leaseExpiresAt, 0, false) {
				return ErrCCMAXRuntimeCheckpointInvalid
			}
			if failure.state == "blocked" {
				return ErrCCMAXRuntimeCheckpointBlocked
			}
		}
	} else {
		lastExists := lastSequence == 0
		if lastSequence > 0 {
			var exactSequence int64
			err = tx.QueryRowContext(ctx, `
SELECT sequence
FROM runtime_outbox
WHERE sequence = ?
FOR UPDATE`, lastSequence).Scan(&exactSequence)
			lastExists = err == nil && exactSequence == lastSequence
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("lock CCMAX runtime checkpoint sequence: %w", err)
			}
		}
		var nextSequence int64
		err = tx.QueryRowContext(ctx, `
SELECT sequence
FROM runtime_outbox
WHERE sequence > ?
ORDER BY sequence
LIMIT 1
FOR UPDATE`, lastSequence).Scan(&nextSequence)
		hasNext := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock next CCMAX runtime outbox sequence: %w", err)
		}
		if !validCCMAXRuntimeCheckpoint(
			lastSequence, claimedSequence, lockedBy, leaseExpiresAt,
			nextSequence, hasNext, newestSequence, hasNewest, lastExists,
		) {
			return ErrCCMAXRuntimeCheckpointInvalid
		}
		if !validCCMAXRuntimeCheckpointFailure(
			failure, lastSequence, claimedSequence, lockedBy, leaseExpiresAt, nextSequence, hasNext,
		) {
			return ErrCCMAXRuntimeCheckpointInvalid
		}
		if failure.state == "blocked" {
			return ErrCCMAXRuntimeCheckpointBlocked
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit CCMAX runtime checkpoint preflight: %w", err)
	}
	return nil
}

func validCCMAXRuntimeCheckpointFailure(
	failure ccmaxCheckpointFailure,
	lastSequence int64,
	claimedSequence int64,
	lockedBy string,
	leaseExpiresAt int64,
	nextSequence int64,
	hasNext bool,
) bool {
	validFailureSequence := hasNext && failure.sequence == nextSequence && failure.sequence > lastSequence
	validRetryIdentity := validFailureSequence && failure.class == "retryable" &&
		ccmaxCheckpointFailureCode.MatchString(failure.code) && failure.count > 0 && failure.blockedClaimVersion == 0
	switch failure.state {
	case "ready":
		if failure.count == 0 {
			return failure.sequence == 0 && failure.class == "" && failure.code == "" && failure.nextAttemptAt == 0 &&
				failure.blockedClaimVersion == 0
		}
		return validRetryIdentity && failure.nextAttemptAt == 0 && claimedSequence == failure.sequence &&
			credential.ValidateTransportID(lockedBy) == nil && leaseExpiresAt > 0
	case "retry_wait":
		return validRetryIdentity && failure.nextAttemptAt > 0 && claimedSequence == 0 && lockedBy == "" && leaseExpiresAt == 0
	case "blocked":
		return validFailureSequence && validCCMAXBlockedFailureClass(failure.class) &&
			ccmaxCheckpointFailureCode.MatchString(failure.code) && failure.count > 0 && failure.nextAttemptAt == 0 &&
			failure.blockedClaimVersion > 0 && claimedSequence == 0 && lockedBy == "" && leaseExpiresAt == 0
	default:
		return false
	}
}

func validCCMAXBlockedFailureClass(class string) bool {
	switch class {
	case "terminal", "authority", "integrity", "security", "indeterminate":
		return true
	default:
		return false
	}
}

func validCCMAXRuntimeCheckpoint(
	lastSequence int64,
	claimedSequence int64,
	lockedBy string,
	leaseExpiresAt int64,
	nextSequence int64,
	hasNext bool,
	newestSequence int64,
	hasNewest bool,
	lastExists bool,
) bool {
	if lastSequence < 0 || claimedSequence < 0 || leaseExpiresAt < 0 {
		return false
	}
	if !hasNewest {
		return lastSequence == 0 && !hasNext && claimedSequence == 0 && lockedBy == "" && leaseExpiresAt == 0
	}
	if newestSequence <= 0 || lastSequence > newestSequence || lastSequence > 0 && !lastExists ||
		hasNext && (nextSequence <= lastSequence || nextSequence > newestSequence) ||
		lastSequence < newestSequence && !hasNext || lastSequence == newestSequence && hasNext {
		return false
	}
	if !hasNext && claimedSequence != 0 {
		return false
	}
	if claimedSequence != 0 && claimedSequence != nextSequence {
		return false
	}
	if claimedSequence == 0 {
		return lockedBy == "" && leaseExpiresAt == 0
	}
	return credential.ValidateTransportID(lockedBy) == nil && leaseExpiresAt > 0
}
