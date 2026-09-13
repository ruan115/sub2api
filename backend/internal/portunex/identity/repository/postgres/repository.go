// Package postgres provides session storage primitives for an isolated recovery
// schema. It does not define legacy login, logout or Bearer token policy.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lib/pq"
)

var (
	ErrNotFound    = errors.New("portunex sessions: not found")
	ErrConflict    = errors.New("portunex sessions: conflict")
	ErrConstraint  = errors.New("portunex sessions: constraint violation")
	ErrRetryable   = errors.New("portunex sessions: retryable transaction")
	ErrCanceled    = errors.New("portunex sessions: query canceled")
	ErrUnavailable = errors.New("portunex sessions: repository unavailable")
)

// StorageToken is the exact database lookup value, NOT a claim that the legacy
// server stores incoming Bearer credentials without a transformation.
type StorageToken string

// Avoid accidentally including credentials in common formatted diagnostics.
func (StorageToken) String() string   { return "[redacted storage token]" }
func (StorageToken) GoString() string { return "[redacted storage token]" }

// Record is a database record, not an HTTP response or authentication result.
type Record struct {
	ID           int64
	UserID       int64
	StorageToken StorageToken `json:"-"`
	ExpiresAt    time.Time
	IPAddress    sql.NullString
	UserAgent    sql.NullString
	CreatedAt    sql.NullTime
	LastUsedAt   sql.NullTime
	DeletedAt    sql.NullTime
}

// CreateParams leaves ID generation, token transformation and lifetime to the
// caller. CreatedAt initializes last_used_at too, matching the observed INSERT.
type CreateParams struct {
	ID           int64
	UserID       int64
	StorageToken StorageToken `json:"-"`
	ExpiresAt    time.Time
	IPAddress    sql.NullString
	UserAgent    sql.NullString
	CreatedAt    time.Time
}

type Repository struct {
	db *sql.DB
}

func New(db *sql.DB) (*Repository, error) {
	if db == nil {
		return nil, ErrUnavailable
	}
	return &Repository{db: db}, nil
}

const createSQL = `INSERT INTO portunex_identity.auth_sessions
(id, user_id, token, expires_at, ip_address, user_agent, created_at, last_used_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`

const findActiveSQL = `SELECT id, user_id, token, expires_at, ip_address,
user_agent, created_at, last_used_at, deleted_at
FROM portunex_identity.auth_sessions
WHERE token = $1 AND expires_at > NOW() AND deleted_at IS NULL`

const touchSQL = `UPDATE portunex_identity.auth_sessions
SET last_used_at = $1 WHERE id = $2 AND deleted_at IS NULL`

const revokeSQL = `UPDATE portunex_identity.auth_sessions
SET deleted_at = $1 WHERE id = $2 AND deleted_at IS NULL`

func (r *Repository) Create(ctx context.Context, params CreateParams) error {
	return r.execOne(ctx, createSQL, params.ID, params.UserID,
		string(params.StorageToken), params.ExpiresAt, params.IPAddress,
		params.UserAgent, params.CreatedAt)
}

// FindActiveByStorageToken does not inspect the user or update last_used_at.
// Active here means only the two predicates found in the observed session SQL.
func (r *Repository) FindActiveByStorageToken(ctx context.Context, token StorageToken) (*Record, error) {
	if r == nil || r.db == nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var record Record
	err := r.db.QueryRowContext(ctx, findActiveSQL, string(token)).Scan(
		&record.ID, &record.UserID, &record.StorageToken, &record.ExpiresAt,
		&record.IPAddress, &record.UserAgent, &record.CreatedAt,
		&record.LastUsedAt, &record.DeletedAt,
	)
	if err != nil {
		return nil, safeError(ctx, err)
	}
	return &record, nil
}

// Touch is an explicit last-used write, not sliding expiration or authorization.
// An expired but undeleted session can still be touched, as in the observed SQL.
func (r *Repository) Touch(ctx context.Context, id int64, usedAt time.Time) error {
	return r.execOne(ctx, touchSQL, usedAt, id)
}

// Revoke marks one undeleted row. This recovery primitive is not evidence of the
// legacy logout scope. A missing/already revoked row returns ErrNotFound.
func (r *Repository) Revoke(ctx context.Context, id int64, deletedAt time.Time) error {
	return r.execOne(ctx, revokeSQL, deletedAt, id)
}

func (r *Repository) execOne(ctx context.Context, query string, args ...any) error {
	if r == nil || r.db == nil {
		return ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	result, err := r.db.ExecContext(ctx, query, args...)
	if err != nil {
		return safeError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return safeError(ctx, err)
	}
	if count == 0 {
		return ErrNotFound
	}
	if count != 1 {
		return ErrUnavailable
	}
	return nil
}

// Driver and Scan errors can contain credentials, including parameter values.
// Only fixed sentinels and known context errors cross the repository boundary.
func safeError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var pgError *pq.Error
	if errors.As(err, &pgError) {
		switch pgError.Code {
		case "23505":
			return ErrConflict
		case "23502", "23503", "23514", "23P01", "22001", "22003", "22P02":
			return ErrConstraint
		case "40001", "40P01":
			return ErrRetryable
		case "57014":
			return ErrCanceled
		}
	}
	return ErrUnavailable
}
