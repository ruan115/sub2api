package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

const (
	runtimeProxyReservationActive  = "active"
	runtimeProxyReservationRevoked = "revoked"
	runtimeProxyReservationGranted = "account.proxy_reservation.granted"
	runtimeProxyReservationRevoke  = "account.proxy_reservation.revoked"
)

var errRuntimeProxyReservationActive = errors.New("proxy binding is owned by runtime execution")

type runtimeProxyReservation struct {
	ReservationID     string
	AccountID         int64
	ProxyID           int64
	DesiredGeneration uint64
	BindingRevision   uint64
	GrantEventID      string
	RevokeEventID     sql.NullString
	Status            string
	RevokedAt         sql.NullString
}

type runtimeProxyReservationPayload struct {
	ReservationID   string `json:"reservation_id"`
	ProxyBindingID  string `json:"proxy_binding_id"`
	BindingRevision uint64 `json:"binding_revision"`
}

type runtimeRowScanner interface {
	Scan(dest ...any) error
}

func newRuntimeProxyReservationID() string {
	return "rpr-" + newRuntimeEventID()
}

func validRuntimeOpaqueID(value string) bool {
	return runtimeOpaqueIntentIDPattern.MatchString(value) && !runtimeSecretString(value)
}

func validateRuntimeProxyReservation(reservation runtimeProxyReservation) error {
	if !validRuntimeOpaqueID(reservation.ReservationID) || reservation.AccountID <= 0 || reservation.ProxyID <= 0 ||
		reservation.DesiredGeneration == 0 || reservation.BindingRevision != reservation.DesiredGeneration ||
		!validRuntimeOpaqueID(reservation.GrantEventID) {
		return errRuntimeMigration
	}
	switch reservation.Status {
	case runtimeProxyReservationActive:
		if reservation.RevokeEventID.Valid || reservation.RevokedAt.Valid {
			return errRuntimeMigration
		}
	case runtimeProxyReservationRevoked:
		if !reservation.RevokeEventID.Valid || !validRuntimeOpaqueID(reservation.RevokeEventID.String) || !reservation.RevokedAt.Valid {
			return errRuntimeMigration
		}
	default:
		return errRuntimeMigration
	}
	return nil
}

func scanRuntimeProxyReservation(row runtimeRowScanner) (runtimeProxyReservation, error) {
	var reservation runtimeProxyReservation
	err := row.Scan(
		&reservation.ReservationID, &reservation.AccountID, &reservation.ProxyID,
		&reservation.DesiredGeneration, &reservation.BindingRevision, &reservation.GrantEventID,
		&reservation.RevokeEventID, &reservation.Status, &reservation.RevokedAt,
	)
	if err != nil {
		return runtimeProxyReservation{}, err
	}
	if err := validateRuntimeProxyReservation(reservation); err != nil {
		return runtimeProxyReservation{}, err
	}
	return reservation, nil
}

func runtimeProxyReservationPayloadJSON(reservation runtimeProxyReservation) (string, error) {
	if err := validateRuntimeProxyReservation(reservation); err != nil {
		return "", err
	}
	payload := runtimeProxyReservationPayload{
		ReservationID:   reservation.ReservationID,
		ProxyBindingID:  strconv.FormatInt(reservation.ProxyID, 10),
		BindingRevision: reservation.BindingRevision,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", errRuntimeMigration
	}
	return safeRuntimePayloadJSON(string(encoded))
}

func decodeRuntimeProxyReservationPayload(payload string) (runtimeProxyReservationPayload, error) {
	if len(payload) == 0 || len(payload) > 64<<10 {
		return runtimeProxyReservationPayload{}, errRuntimeMigration
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return runtimeProxyReservationPayload{}, errRuntimeMigration
	}
	var decoded runtimeProxyReservationPayload
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok || seen[key] {
			return runtimeProxyReservationPayload{}, errRuntimeMigration
		}
		seen[key] = true
		switch key {
		case "reservation_id":
			err = decoder.Decode(&decoded.ReservationID)
		case "proxy_binding_id":
			err = decoder.Decode(&decoded.ProxyBindingID)
		case "binding_revision":
			err = decoder.Decode(&decoded.BindingRevision)
		default:
			return runtimeProxyReservationPayload{}, errRuntimeMigration
		}
		if err != nil {
			return runtimeProxyReservationPayload{}, errRuntimeMigration
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != 3 ||
		!seen["reservation_id"] || !seen["proxy_binding_id"] || !seen["binding_revision"] {
		return runtimeProxyReservationPayload{}, errRuntimeMigration
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) ||
		!validRuntimeOpaqueID(decoded.ReservationID) || decoded.BindingRevision == 0 {
		return runtimeProxyReservationPayload{}, errRuntimeMigration
	}
	proxyID, err := strconv.ParseInt(decoded.ProxyBindingID, 10, 64)
	if err != nil || proxyID <= 0 || strconv.FormatInt(proxyID, 10) != decoded.ProxyBindingID {
		return runtimeProxyReservationPayload{}, errRuntimeMigration
	}
	canonical, err := safeRuntimePayloadJSON(payload)
	if err != nil {
		return runtimeProxyReservationPayload{}, err
	}
	// Re-encoding enforces the exact three-field authority payload. Unknown or
	// omitted fields cannot be smuggled through the general safe JSON filter.
	expected, err := json.Marshal(decoded)
	if err != nil {
		return runtimeProxyReservationPayload{}, errRuntimeMigration
	}
	expectedCanonical, err := safeRuntimePayloadJSON(string(expected))
	if err != nil || expectedCanonical != canonical {
		return runtimeProxyReservationPayload{}, errRuntimeMigration
	}
	return decoded, nil
}

func loadRuntimeProxyReservationByGenerationTx(
	ctx context.Context,
	tx *databaseTx,
	accountID int64,
	desiredGeneration uint64,
	lock bool,
) (runtimeProxyReservation, error) {
	query := `SELECT reservation_id, account_id, proxy_id, desired_generation, binding_revision,
		grant_event_id, revoke_event_id, status, revoked_at
		FROM runtime_proxy_reservations WHERE account_id = ? AND desired_generation = ?`
	if lock && tx.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	return scanRuntimeProxyReservation(tx.QueryRowContext(ctx, query, accountID, desiredGeneration))
}

func loadRuntimeProxyReservationByIDTx(
	ctx context.Context,
	tx *databaseTx,
	reservationID string,
	lock bool,
) (runtimeProxyReservation, error) {
	query := `SELECT reservation_id, account_id, proxy_id, desired_generation, binding_revision,
		grant_event_id, revoke_event_id, status, revoked_at
		FROM runtime_proxy_reservations WHERE reservation_id = ?`
	if lock && tx.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	return scanRuntimeProxyReservation(tx.QueryRowContext(ctx, query, reservationID))
}

// lockRuntimeProxyBindingsForMutationTx serializes every identity-bearing
// proxy mutation with runtime reservation grants. Callers must keep the check
// and the proxy write in this transaction. A grant that committed first is
// rejected; a grant waiting behind this lock validates the post-mutation row.
func lockRuntimeProxyBindingsForMutationTx(ctx context.Context, tx *databaseTx, proxyIDs []int64) error {
	if ctx == nil || ctx.Err() != nil || tx == nil {
		return errRuntimeMigration
	}
	unique := make(map[int64]struct{}, len(proxyIDs))
	ids := make([]int64, 0, len(proxyIDs))
	for _, proxyID := range proxyIDs {
		if proxyID <= 0 {
			return errRuntimeMigration
		}
		if _, exists := unique[proxyID]; exists {
			continue
		}
		unique[proxyID] = struct{}{}
		ids = append(ids, proxyID)
	}
	if len(ids) == 0 {
		return nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index, proxyID := range ids {
		args[index] = proxyID
	}
	query := `SELECT id FROM proxies WHERE id IN (` + placeholders + `) ORDER BY id`
	if tx.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var ignored int64
		if err := rows.Scan(&ignored); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	reservationQuery := `SELECT reservation_id FROM runtime_proxy_reservations
		WHERE proxy_id IN (` + placeholders + `) AND status = 'active'
		ORDER BY proxy_id, desired_generation LIMIT 1`
	if tx.dialect == dialectMySQL {
		reservationQuery += ` FOR UPDATE`
	}
	var reservationID string
	err = tx.QueryRowContext(ctx, reservationQuery, args...).Scan(&reservationID)
	if err == nil {
		return errRuntimeProxyReservationActive
	}
	if errors.Is(err, sql.ErrNoRows) {
		accountQuery := `SELECT id FROM accounts WHERE execution_migration_status != 'legacy'
			AND deleted_at IS NULL AND (proxy_id IN (` + placeholders + `)
				OR archived_proxy_id IN (` + placeholders + `))
			ORDER BY id LIMIT 1`
		accountArgs := make([]any, 0, len(args)*2)
		accountArgs = append(accountArgs, args...)
		accountArgs = append(accountArgs, args...)
		var accountID int64
		err = tx.QueryRowContext(ctx, accountQuery, accountArgs...).Scan(&accountID)
		if err == nil {
			return errRuntimeProxyReservationActive
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	return err
}

// validateRuntimeProxyBindingForGrantTx locks the exact proxy row so no
// endpoint, credential, protocol, lifecycle, or pool mutation can race the
// durable grant. The opaque binding id is only meaningful while this row is
// an active proxy in an active non-system pool.
func validateRuntimeProxyBindingForGrantTx(ctx context.Context, tx *databaseTx, accountID, proxyID int64) error {
	if ctx == nil || ctx.Err() != nil || tx == nil || accountID <= 0 || proxyID <= 0 {
		return errRuntimeMigration
	}
	query := `SELECT id, pool_id, status, deleted_at FROM proxies WHERE id = ?`
	if tx.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var storedID, poolID int64
	var proxyStatus string
	var proxyDeletedAt sql.NullString
	if err := tx.QueryRowContext(ctx, query, proxyID).Scan(&storedID, &poolID, &proxyStatus, &proxyDeletedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errRuntimeMigration
		}
		return err
	}
	if storedID != proxyID || poolID <= 0 || proxyStatus != "active" || proxyDeletedAt.Valid {
		return errRuntimeMigration
	}
	// Binding-affecting pool writes lock every member proxy before changing the
	// pool, so this read need not lock the pool row (which would invert the
	// pool->proxy mutation order). Holding the proxy lock is the serialization
	// point: a preceding pool change is visible here; a following one blocks and
	// then observes the committed reservation.
	var poolStatus, systemKind string
	var poolDeletedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT status, system_kind, deleted_at FROM proxy_pools WHERE id = ?`, poolID).Scan(
		&poolStatus, &systemKind, &poolDeletedAt,
	); err != nil {
		return err
	}
	if poolStatus != "active" || systemKind != "" || poolDeletedAt.Valid {
		return errRuntimeMigration
	}
	reservationQuery := `SELECT reservation_id FROM runtime_proxy_reservations
		WHERE proxy_id = ? AND account_id != ? AND status = 'active'
		ORDER BY account_id, desired_generation LIMIT 1`
	if tx.dialect == dialectMySQL {
		reservationQuery += ` FOR UPDATE`
	}
	var conflictingReservationID string
	err := tx.QueryRowContext(ctx, reservationQuery, proxyID, accountID).Scan(&conflictingReservationID)
	if err == nil {
		return errRuntimeProxyReservationActive
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// requireLegacyAccountLifecycleTx is called only after the account row is
// locked. Runtime onboarding takes the same account lock before it can create
// a reservation, so the migration-status and active-grant checks remain a
// single atomic fence with the legacy archive/delete/restore write.
func requireLegacyAccountLifecycleTx(ctx context.Context, tx *databaseTx, accountID int64) error {
	if ctx == nil || ctx.Err() != nil || tx == nil || accountID <= 0 {
		return errRuntimeMigration
	}
	var migrationStatus string
	if err := tx.QueryRowContext(ctx, `SELECT execution_migration_status FROM accounts WHERE id = ?`, accountID).Scan(&migrationStatus); err != nil {
		return err
	}
	var activeReservations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runtime_proxy_reservations
		WHERE account_id = ? AND status = 'active'`, accountID).Scan(&activeReservations); err != nil {
		return err
	}
	if migrationStatus != "legacy" || activeReservations != 0 {
		return errRuntimeRoutingOwner
	}
	return nil
}

func loadRuntimeOutboxEventTx(ctx context.Context, tx *databaseTx, eventID string) (runtimeOutboxEvent, error) {
	if tx == nil || !validRuntimeOpaqueID(eventID) {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	var event runtimeOutboxEvent
	err := tx.QueryRowContext(ctx, `SELECT sequence, event_id, account_id, event_type, desired_generation, payload_json, created_at
		FROM runtime_outbox WHERE event_id = ?`, eventID).Scan(
		&event.Sequence, &event.EventID, &event.AccountID, &event.EventType,
		&event.DesiredGeneration, &event.PayloadJSON, &event.CreatedAt,
	)
	if err != nil {
		return runtimeOutboxEvent{}, err
	}
	return event, nil
}

func validateRuntimeProxyAuthorityEventTx(
	ctx context.Context,
	tx *databaseTx,
	event runtimeOutboxEvent,
	currentGeneration uint64,
) error {
	if tx == nil || !runtimeAuthorityEventTypes[event.EventType] || !validRuntimeOpaqueID(event.EventID) {
		return errRuntimeMigration
	}
	payload, err := decodeRuntimeProxyReservationPayload(event.PayloadJSON)
	if err != nil {
		return errRuntimeMigration
	}
	reservation, err := loadRuntimeProxyReservationByIDTx(ctx, tx, payload.ReservationID, false)
	if err != nil {
		return err
	}
	if event.AccountID != reservation.AccountID || event.DesiredGeneration != reservation.DesiredGeneration ||
		payload.ProxyBindingID != strconv.FormatInt(reservation.ProxyID, 10) ||
		payload.BindingRevision != reservation.BindingRevision {
		return errRuntimeMigration
	}
	switch event.EventType {
	case runtimeProxyReservationGranted:
		if reservation.Status != runtimeProxyReservationActive || reservation.GrantEventID != event.EventID ||
			currentGeneration != reservation.DesiredGeneration {
			return errRuntimeMigration
		}
	case runtimeProxyReservationRevoke:
		if reservation.Status != runtimeProxyReservationRevoked || !reservation.RevokeEventID.Valid ||
			reservation.RevokeEventID.String != event.EventID ||
			currentGeneration < reservation.DesiredGeneration {
			return errRuntimeMigration
		}
	default:
		return errRuntimeMigration
	}
	return nil
}

func validateStoredRuntimeProxyAuthorityEventTx(
	ctx context.Context,
	tx *databaseTx,
	event runtimeOutboxEvent,
) error {
	var currentGeneration uint64
	if err := tx.QueryRowContext(ctx, `SELECT runtime_generation FROM accounts WHERE id = ?`, event.AccountID).Scan(&currentGeneration); err != nil {
		return err
	}
	return validateRuntimeProxyAuthorityEventTx(ctx, tx, event, currentGeneration)
}

// ensureRuntimeProxyReservationTx requires the caller to hold the account row
// lock after advancing it to desiredGeneration. It creates the reservation and
// its grant event atomically, or returns the exact already-durable grant.
func ensureRuntimeProxyReservationTx(
	ctx context.Context,
	tx *databaseTx,
	accountID int64,
	proxyID int64,
	desiredGeneration uint64,
) (runtimeProxyReservation, runtimeOutboxEvent, error) {
	if ctx == nil || ctx.Err() != nil || tx == nil || accountID <= 0 || proxyID <= 0 || desiredGeneration == 0 {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, errRuntimeMigration
	}
	if err := validateRuntimeProxyBindingForGrantTx(ctx, tx, accountID, proxyID); err != nil {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, err
	}
	activeQuery := `SELECT reservation_id FROM runtime_proxy_reservations
		WHERE account_id = ? AND desired_generation != ? AND status = 'active'
		ORDER BY desired_generation LIMIT 1`
	if tx.dialect == dialectMySQL {
		activeQuery += ` FOR UPDATE`
	}
	var conflictingReservationID string
	err := tx.QueryRowContext(ctx, activeQuery, accountID, desiredGeneration).Scan(&conflictingReservationID)
	if err == nil {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, errRuntimeMigration
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, err
	}
	stored, err := loadRuntimeProxyReservationByGenerationTx(ctx, tx, accountID, desiredGeneration, true)
	if err == nil {
		if stored.AccountID != accountID || stored.ProxyID != proxyID || stored.DesiredGeneration != desiredGeneration ||
			stored.BindingRevision != desiredGeneration || stored.Status != runtimeProxyReservationActive {
			return runtimeProxyReservation{}, runtimeOutboxEvent{}, errRuntimeMigration
		}
		event, loadErr := loadRuntimeOutboxEventTx(ctx, tx, stored.GrantEventID)
		if loadErr != nil || event.EventType != runtimeProxyReservationGranted {
			return runtimeProxyReservation{}, runtimeOutboxEvent{}, errRuntimeMigration
		}
		if err := validateStoredRuntimeProxyAuthorityEventTx(ctx, tx, event); err != nil {
			return runtimeProxyReservation{}, runtimeOutboxEvent{}, errRuntimeMigration
		}
		return stored, event, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, err
	}
	reservation := runtimeProxyReservation{
		ReservationID: newRuntimeProxyReservationID(), AccountID: accountID, ProxyID: proxyID,
		DesiredGeneration: desiredGeneration, BindingRevision: desiredGeneration,
		GrantEventID: newRuntimeEventID(), Status: runtimeProxyReservationActive,
	}
	if err := validateRuntimeProxyReservation(reservation); err != nil {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runtime_proxy_reservations
		(reservation_id, account_id, proxy_id, desired_generation, binding_revision, grant_event_id, status)
		VALUES (?, ?, ?, ?, ?, ?, 'active')`, reservation.ReservationID, reservation.AccountID,
		reservation.ProxyID, reservation.DesiredGeneration, reservation.BindingRevision, reservation.GrantEventID); err != nil {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, fmt.Errorf("insert runtime proxy reservation: %w", err)
	}
	payload, err := runtimeProxyReservationPayloadJSON(reservation)
	if err != nil {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, err
	}
	event, err := enqueueRuntimeEventTx(ctx, tx, runtimeOutboxEvent{
		EventID: reservation.GrantEventID, AccountID: accountID, EventType: runtimeProxyReservationGranted,
		DesiredGeneration: desiredGeneration, PayloadJSON: payload,
	})
	if err != nil {
		return runtimeProxyReservation{}, runtimeOutboxEvent{}, err
	}
	return reservation, event, nil
}

func revokeRuntimeProxyReservationTx(
	ctx context.Context,
	tx *databaseTx,
	expected runtimeProxyReservation,
	currentGeneration uint64,
) (runtimeOutboxEvent, error) {
	if ctx == nil || ctx.Err() != nil || tx == nil || currentGeneration < expected.DesiredGeneration ||
		validateRuntimeProxyReservation(expected) != nil {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	reservation, err := loadRuntimeProxyReservationByIDTx(ctx, tx, expected.ReservationID, true)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimeOutboxEvent{}, errRuntimeMigration
		}
		return runtimeOutboxEvent{}, err
	}
	if reservation.AccountID != expected.AccountID || reservation.ProxyID != expected.ProxyID ||
		reservation.DesiredGeneration != expected.DesiredGeneration || reservation.BindingRevision != expected.BindingRevision ||
		reservation.GrantEventID != expected.GrantEventID {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	if reservation.Status == runtimeProxyReservationRevoked {
		event, err := loadRuntimeOutboxEventTx(ctx, tx, reservation.RevokeEventID.String)
		if err != nil || event.EventType != runtimeProxyReservationRevoke ||
			validateRuntimeProxyAuthorityEventTx(ctx, tx, event, currentGeneration) != nil {
			return runtimeOutboxEvent{}, errRuntimeMigration
		}
		return event, nil
	}
	if reservation.Status != runtimeProxyReservationActive {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	revokeEventID := newRuntimeEventID()
	result, err := tx.ExecContext(ctx, `UPDATE runtime_proxy_reservations SET
		status = 'revoked', revoke_event_id = ?, revoked_at = `+nowSQL+`, updated_at = `+nowSQL+`
		WHERE reservation_id = ? AND account_id = ? AND proxy_id = ? AND desired_generation = ?
		  AND binding_revision = ? AND grant_event_id = ? AND status = 'active'
		  AND revoke_event_id IS NULL AND revoked_at IS NULL`,
		revokeEventID, reservation.ReservationID, reservation.AccountID, reservation.ProxyID,
		reservation.DesiredGeneration, reservation.BindingRevision, reservation.GrantEventID)
	if err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("revoke runtime proxy reservation: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	reservation.Status = runtimeProxyReservationRevoked
	reservation.RevokeEventID = sql.NullString{String: revokeEventID, Valid: true}
	reservation.RevokedAt = sql.NullString{String: "revoked", Valid: true}
	payload, err := runtimeProxyReservationPayloadJSON(reservation)
	if err != nil {
		return runtimeOutboxEvent{}, err
	}
	return enqueueRuntimeEventTx(ctx, tx, runtimeOutboxEvent{
		EventID: revokeEventID, AccountID: reservation.AccountID, EventType: runtimeProxyReservationRevoke,
		DesiredGeneration: reservation.DesiredGeneration, PayloadJSON: payload,
	})
}

// revokeActiveRuntimeProxyReservationsTx closes all grants owned by the
// account's current-or-older generations. The caller must hold the account row
// lock. Multiple rows are tolerated to repair states produced before
// supersession revocation was introduced; a future-generation row fails
// closed as corruption.
func revokeActiveRuntimeProxyReservationsTx(
	ctx context.Context,
	tx *databaseTx,
	accountID int64,
	currentGeneration uint64,
) ([]runtimeOutboxEvent, error) {
	if ctx == nil || ctx.Err() != nil || tx == nil || accountID <= 0 {
		return nil, errRuntimeMigration
	}
	query := `SELECT reservation_id, account_id, proxy_id, desired_generation, binding_revision,
		grant_event_id, revoke_event_id, status, revoked_at
		FROM runtime_proxy_reservations WHERE account_id = ? AND status = 'active'
		ORDER BY desired_generation, reservation_id`
	if tx.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	rows, err := tx.QueryContext(ctx, query, accountID)
	if err != nil {
		return nil, err
	}
	reservations := make([]runtimeProxyReservation, 0, 1)
	for rows.Next() {
		reservation, err := scanRuntimeProxyReservation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if reservation.AccountID != accountID || reservation.DesiredGeneration > currentGeneration {
			rows.Close()
			return nil, errRuntimeMigration
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	events := make([]runtimeOutboxEvent, 0, len(reservations))
	for _, reservation := range reservations {
		event, err := revokeRuntimeProxyReservationTx(ctx, tx, reservation, currentGeneration)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

// revokeRuntimeProxyReservation revokes only the exact durable binding. A
// later generation may already exist; the old reservation remains addressable
// by its opaque id without granting authority over that newer binding.
func (a *app) revokeRuntimeProxyReservation(
	ctx context.Context,
	reservationID string,
	accountID int64,
	proxyID int64,
	desiredGeneration uint64,
	bindingRevision uint64,
) (runtimeOutboxEvent, error) {
	if a == nil || a.db == nil || ctx == nil || ctx.Err() != nil || !validRuntimeOpaqueID(reservationID) ||
		accountID <= 0 || proxyID <= 0 || desiredGeneration == 0 || bindingRevision != desiredGeneration {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	// Reject mismatched ownership before taking any account lock. The exact row
	// is loaded and revalidated under lock below; this preflight prevents a bad
	// caller from inducing an account-A -> reservation-B lock order.
	preflight, err := scanRuntimeProxyReservation(a.db.QueryRowContext(ctx, `SELECT
		reservation_id, account_id, proxy_id, desired_generation, binding_revision,
		grant_event_id, revoke_event_id, status, revoked_at
		FROM runtime_proxy_reservations WHERE reservation_id = ?`, reservationID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, errRuntimeMigration) {
			return runtimeOutboxEvent{}, errRuntimeMigration
		}
		return runtimeOutboxEvent{}, err
	}
	if preflight.AccountID != accountID || preflight.ProxyID != proxyID ||
		preflight.DesiredGeneration != desiredGeneration || preflight.BindingRevision != bindingRevision {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("begin runtime proxy reservation revoke: %w", err)
	}
	defer tx.Rollback()
	accountQuery := `SELECT runtime_generation FROM accounts WHERE id = ?`
	if a.db.dialect == dialectMySQL {
		accountQuery += ` FOR UPDATE`
	}
	var currentGeneration uint64
	if err := tx.QueryRowContext(ctx, accountQuery, accountID).Scan(&currentGeneration); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtimeOutboxEvent{}, errRuntimeMigration
		}
		return runtimeOutboxEvent{}, err
	}
	if currentGeneration < desiredGeneration {
		return runtimeOutboxEvent{}, errRuntimeMigration
	}
	event, err := revokeRuntimeProxyReservationTx(ctx, tx, preflight, currentGeneration)
	if err != nil {
		return runtimeOutboxEvent{}, err
	}
	if err := tx.Commit(); err != nil {
		return runtimeOutboxEvent{}, fmt.Errorf("commit runtime proxy reservation revoke: %w", err)
	}
	return event, nil
}
