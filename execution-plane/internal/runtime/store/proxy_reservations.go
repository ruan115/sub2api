package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/go-sql-driver/mysql"
)

var (
	ErrProxyReservationConflict = errors.New("proxy reservation grant conflicts with its trusted binding")
	ErrProxyReservationNotFound = errors.New("proxy reservation grant is not current")
	proxyReservationOpaqueID    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// ProxyReservationGrant is the secret-free durable projection of a fixed
// proxy reservation granted by CCMAX. ProxyBindingID is the canonical decimal
// identifier of the CCMAX proxy row: endpoints and proxy credentials must
// never enter this record.
type ProxyReservationGrant struct {
	ReservationID     string
	AccountID         string
	DesiredGeneration uint64
	ProxyBindingID    string
	BindingRevision   uint64
	GrantEventID      string
	RevokeEventID     string
	RevokedAt         *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type ProxyReservationRevocation struct {
	ReservationID     string
	AccountID         string
	DesiredGeneration uint64
	ProxyBindingID    string
	BindingRevision   uint64
	RevokeEventID     string
	RevokedAt         time.Time
}

type ProxyReservationGrantRepository interface {
	GrantProxyReservation(ctx context.Context, candidate ProxyReservationGrant) (ProxyReservationGrant, error)
	RevokeProxyReservation(ctx context.Context, candidate ProxyReservationRevocation) (ProxyReservationGrant, error)
	GetProxyReservation(ctx context.Context, reservationID string) (ProxyReservationGrant, error)
	ValidateCurrentProxyReservation(
		ctx context.Context,
		accountID string,
		desiredGeneration uint64,
		reservationID string,
		bindingRevision uint64,
		checkedAt time.Time,
	) error
}

func (r *Repository) GrantProxyReservation(ctx context.Context, candidate ProxyReservationGrant) (ProxyReservationGrant, error) {
	if validateProxyReservationGrant(candidate) != nil {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ProxyReservationGrant{}, fmt.Errorf("begin proxy reservation grant: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
INSERT INTO proxy_reservation_grants (
  reservation_id, account_id, desired_generation, proxy_binding_id, binding_revision,
  grant_event_id, revoke_event_id, revoked_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)
ON DUPLICATE KEY UPDATE reservation_id = reservation_id`,
		candidate.ReservationID, candidate.AccountID, candidate.DesiredGeneration, candidate.ProxyBindingID,
		candidate.BindingRevision, candidate.GrantEventID, candidate.CreatedAt.UTC(), candidate.UpdatedAt.UTC(),
	)
	if err != nil {
		return ProxyReservationGrant{}, fmt.Errorf("insert idempotent proxy reservation grant: %w", err)
	}
	stored, err := getProxyReservation(ctx, tx, candidate.ReservationID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	if err != nil {
		return ProxyReservationGrant{}, fmt.Errorf("read proxy reservation grant: %w", err)
	}
	if !sameProxyReservationGrantIdentity(stored, candidate) {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	if err := tx.Commit(); err != nil {
		return ProxyReservationGrant{}, fmt.Errorf("commit proxy reservation grant: %w", err)
	}
	return stored, nil
}

func (r *Repository) RevokeProxyReservation(ctx context.Context, candidate ProxyReservationRevocation) (ProxyReservationGrant, error) {
	if validateProxyReservationRevocation(candidate) != nil {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return ProxyReservationGrant{}, fmt.Errorf("begin proxy reservation revocation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stored, err := getProxyReservation(ctx, tx, candidate.ReservationID, true)
	if errors.Is(err, sql.ErrNoRows) {
		return ProxyReservationGrant{}, ErrProxyReservationNotFound
	}
	if err != nil {
		return ProxyReservationGrant{}, fmt.Errorf("lock proxy reservation revocation: %w", err)
	}
	if !sameProxyReservationRevocationBinding(stored, candidate) || candidate.RevokedAt.Before(stored.CreatedAt) {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	if stored.RevokedAt != nil {
		if stored.RevokeEventID != candidate.RevokeEventID || !stored.RevokedAt.Equal(candidate.RevokedAt) {
			return ProxyReservationGrant{}, ErrProxyReservationConflict
		}
		if err := tx.Commit(); err != nil {
			return ProxyReservationGrant{}, fmt.Errorf("commit proxy reservation revocation replay: %w", err)
		}
		return stored, nil
	}
	result, err := tx.ExecContext(ctx, `
UPDATE proxy_reservation_grants
SET revoke_event_id = ?, revoked_at = ?, updated_at = ?
WHERE reservation_id = ? AND revoke_event_id IS NULL AND revoked_at IS NULL`,
		candidate.RevokeEventID, candidate.RevokedAt.UTC(), candidate.RevokedAt.UTC(), candidate.ReservationID,
	)
	if err != nil {
		var mysqlError *mysql.MySQLError
		if errors.As(err, &mysqlError) && mysqlError.Number == 1062 {
			return ProxyReservationGrant{}, ErrProxyReservationConflict
		}
		return ProxyReservationGrant{}, fmt.Errorf("revoke proxy reservation grant: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	stored.RevokeEventID = candidate.RevokeEventID
	revokedAt := candidate.RevokedAt.UTC()
	stored.RevokedAt = &revokedAt
	stored.UpdatedAt = revokedAt
	if err := tx.Commit(); err != nil {
		return ProxyReservationGrant{}, fmt.Errorf("commit proxy reservation revocation: %w", err)
	}
	return stored, nil
}

func (r *Repository) GetProxyReservation(ctx context.Context, reservationID string) (ProxyReservationGrant, error) {
	if ValidateProxyReservationOpaqueID(reservationID) != nil {
		return ProxyReservationGrant{}, ErrProxyReservationNotFound
	}
	grant, err := getProxyReservation(ctx, r.db, reservationID, false)
	if errors.Is(err, sql.ErrNoRows) {
		return ProxyReservationGrant{}, ErrProxyReservationNotFound
	}
	return grant, err
}

func (r *Repository) ValidateCurrentProxyReservation(
	ctx context.Context,
	accountID string,
	desiredGeneration uint64,
	reservationID string,
	bindingRevision uint64,
	checkedAt time.Time,
) error {
	if validateCurrentProxyReservationInput(accountID, desiredGeneration, reservationID, bindingRevision, checkedAt) != nil {
		return ErrProxyReservationNotFound
	}
	var storedReservationID string
	err := r.db.QueryRowContext(ctx, `
SELECT reservation_id
FROM proxy_reservation_grants
WHERE reservation_id = ? AND account_id = ? AND desired_generation = ? AND binding_revision = ?
  AND revoked_at IS NULL AND created_at <= ?`,
		reservationID, accountID, desiredGeneration, bindingRevision, checkedAt.UTC(),
	).Scan(&storedReservationID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProxyReservationNotFound
	}
	if err != nil {
		return fmt.Errorf("validate current proxy reservation grant: %w", err)
	}
	return nil
}

func getProxyReservation(ctx context.Context, queryer slotQueryer, reservationID string, forUpdate bool) (ProxyReservationGrant, error) {
	query := `
SELECT reservation_id, account_id, desired_generation, proxy_binding_id, binding_revision,
       grant_event_id, revoke_event_id, revoked_at, created_at, updated_at
FROM proxy_reservation_grants WHERE reservation_id = ?`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var grant ProxyReservationGrant
	var revokeEventID sql.NullString
	var revokedAt sql.NullTime
	err := queryer.QueryRowContext(ctx, query, reservationID).Scan(
		&grant.ReservationID, &grant.AccountID, &grant.DesiredGeneration, &grant.ProxyBindingID,
		&grant.BindingRevision, &grant.GrantEventID, &revokeEventID, &revokedAt,
		&grant.CreatedAt, &grant.UpdatedAt,
	)
	if err != nil {
		return ProxyReservationGrant{}, err
	}
	if revokeEventID.Valid {
		grant.RevokeEventID = revokeEventID.String
	}
	if revokedAt.Valid {
		value := revokedAt.Time.UTC()
		grant.RevokedAt = &value
	}
	grant.CreatedAt = grant.CreatedAt.UTC()
	grant.UpdatedAt = grant.UpdatedAt.UTC()
	if validateProxyReservationGrantStored(grant) != nil {
		return ProxyReservationGrant{}, ErrProxyReservationConflict
	}
	return grant, nil
}

func validateProxyReservationGrant(grant ProxyReservationGrant) error {
	for _, value := range []string{
		grant.ReservationID, grant.AccountID, grant.GrantEventID,
	} {
		if ValidateProxyReservationOpaqueID(value) != nil {
			return ErrProxyReservationConflict
		}
	}
	if ValidateProxyBindingID(grant.ProxyBindingID) != nil || grant.DesiredGeneration == 0 || grant.BindingRevision == 0 ||
		grant.RevokeEventID != "" || grant.RevokedAt != nil ||
		grant.CreatedAt.IsZero() || !grant.UpdatedAt.Equal(grant.CreatedAt) {
		return ErrProxyReservationConflict
	}
	return nil
}

func validateProxyReservationGrantStored(grant ProxyReservationGrant) error {
	revokeEventID := grant.RevokeEventID
	revokedAt := grant.RevokedAt
	updatedAt := grant.UpdatedAt
	grant.RevokeEventID = ""
	grant.RevokedAt = nil
	grant.UpdatedAt = grant.CreatedAt
	if validateProxyReservationGrant(grant) != nil {
		return ErrProxyReservationConflict
	}
	if (revokeEventID == "") != (revokedAt == nil) ||
		revokeEventID != "" && ValidateProxyReservationOpaqueID(revokeEventID) != nil ||
		revokedAt != nil && revokedAt.Before(grant.CreatedAt) ||
		revokedAt == nil && !updatedAt.Equal(grant.CreatedAt) ||
		revokedAt != nil && !updatedAt.Equal(*revokedAt) {
		return ErrProxyReservationConflict
	}
	return nil
}

func validateProxyReservationRevocation(candidate ProxyReservationRevocation) error {
	for _, value := range []string{
		candidate.ReservationID, candidate.AccountID, candidate.RevokeEventID,
	} {
		if ValidateProxyReservationOpaqueID(value) != nil {
			return ErrProxyReservationConflict
		}
	}
	if ValidateProxyBindingID(candidate.ProxyBindingID) != nil || candidate.DesiredGeneration == 0 ||
		candidate.BindingRevision == 0 || candidate.RevokedAt.IsZero() {
		return ErrProxyReservationConflict
	}
	return nil
}

func validateCurrentProxyReservationInput(
	accountID string,
	desiredGeneration uint64,
	reservationID string,
	bindingRevision uint64,
	checkedAt time.Time,
) error {
	if ValidateProxyReservationOpaqueID(accountID) != nil || ValidateProxyReservationOpaqueID(reservationID) != nil ||
		desiredGeneration == 0 || bindingRevision == 0 || checkedAt.IsZero() {
		return ErrProxyReservationNotFound
	}
	return nil
}

// ValidateProxyReservationOpaqueID accepts only bounded ASCII identifiers and
// rejects common credential and URL fingerprints. It is shared by the trusted
// outbox ingress and the durable store boundary.
func ValidateProxyReservationOpaqueID(value string) error {
	if credential.ValidateTransportID(value) != nil || !proxyReservationOpaqueID.MatchString(value) {
		return ErrProxyReservationConflict
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{
		"authorization", "credential", "password", "cookie", "secret", "session_key",
		"access_token", "refresh_token", "api_key", "bearer", "sk-",
	} {
		if strings.Contains(lower, marker) {
			return ErrProxyReservationConflict
		}
	}
	return nil
}

// ValidateProxyBindingID enforces the exact CCMAX authority contract. A proxy
// binding is a positive base-10 row id with no sign, leading zero, exponent,
// host, IP address or other free-form representation.
func ValidateProxyBindingID(value string) error {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != value {
		return ErrProxyReservationConflict
	}
	return nil
}

func sameProxyReservationGrantIdentity(left, right ProxyReservationGrant) bool {
	return left.ReservationID == right.ReservationID && left.AccountID == right.AccountID &&
		left.DesiredGeneration == right.DesiredGeneration && left.ProxyBindingID == right.ProxyBindingID &&
		left.BindingRevision == right.BindingRevision && left.GrantEventID == right.GrantEventID &&
		left.CreatedAt.Equal(right.CreatedAt)
}

func sameProxyReservationRevocationBinding(grant ProxyReservationGrant, candidate ProxyReservationRevocation) bool {
	return grant.ReservationID == candidate.ReservationID && grant.AccountID == candidate.AccountID &&
		grant.DesiredGeneration == candidate.DesiredGeneration && grant.ProxyBindingID == candidate.ProxyBindingID &&
		grant.BindingRevision == candidate.BindingRevision
}

var _ ProxyReservationGrantRepository = (*Repository)(nil)
