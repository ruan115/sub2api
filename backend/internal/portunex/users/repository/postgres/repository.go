// Package postgres provides internal reads from the isolated recovery schema.
// It does not implement the legacy users/me response or authorization policy.
package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"
	"github.com/shopspring/decimal"
)

var (
	ErrNotFound    = errors.New("portunex users: not found")
	ErrConflict    = errors.New("portunex users: conflict")
	ErrConstraint  = errors.New("portunex users: constraint violation")
	ErrRetryable   = errors.New("portunex users: retryable transaction")
	ErrCanceled    = errors.New("portunex users: query canceled")
	ErrUnavailable = errors.New("portunex users: repository unavailable")
)

// Record preserves database NULLs and exact NUMERIC values. It is not an HTTP DTO.
// PasswordPHC must only be passed to the password verifier, never to a response.
type Record struct {
	ID                      int64
	Email                   sql.NullString
	PasswordPHC             sql.NullString `json:"-"`
	Points                  decimal.NullDecimal
	Role                    sql.NullString
	CreatedAt               sql.NullTime
	UpdatedAt               sql.NullTime
	DeletedAt               sql.NullTime
	CanPurchaseSubscription bool
	DailyRechargeLimit      decimal.Decimal
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

const selectColumns = `SELECT id, email, password_phc, points, role, created_at,
updated_at, deleted_at, can_purchase_subscription, daily_recharge_limit
FROM portunex_identity.users`

const findByIDSQL = selectColumns + ` WHERE id = $1 AND deleted_at IS NULL`

// Qualifying both the citext type and its operator avoids dependence on the
// connection's search_path for case-insensitive email equality.
const findByEmailSQL = selectColumns + ` WHERE email OPERATOR(public.=) $1::public.citext AND deleted_at IS NULL`

func (r *Repository) FindByID(ctx context.Context, id int64) (*Record, error) {
	return r.find(ctx, findByIDSQL, id)
}

// FindByEmail delegates case comparison to PostgreSQL citext. It deliberately
// does not trim, lowercase or otherwise invent legacy input normalization.
func (r *Repository) FindByEmail(ctx context.Context, email string) (*Record, error) {
	return r.find(ctx, findByEmailSQL, email)
}

func (r *Repository) find(ctx context.Context, query string, value any) (*Record, error) {
	if r == nil || r.db == nil {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var record Record
	err := r.db.QueryRowContext(ctx, query, value).Scan(
		&record.ID, &record.Email, &record.PasswordPHC, &record.Points,
		&record.Role, &record.CreatedAt, &record.UpdatedAt, &record.DeletedAt,
		&record.CanPurchaseSubscription, &record.DailyRechargeLimit,
	)
	if err != nil {
		return nil, safeError(ctx, err)
	}
	return &record, nil
}

// safeError intentionally does not wrap driver errors: pq errors and Scan
// errors can include PHCs, emails, parameters and connection details.
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
