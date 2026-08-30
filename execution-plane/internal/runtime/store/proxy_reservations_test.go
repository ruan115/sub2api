package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestMemoryProxyReservationGrantConflictAndRevocationReplay(t *testing.T) {
	repository := NewMemoryRepository()
	now := time.Unix(2_000_000_000, 0).UTC()
	grant := testProxyReservationGrant(now)
	stored, err := repository.GrantProxyReservation(context.Background(), grant)
	if err != nil || !sameProxyReservationGrantIdentity(stored, grant) || stored.RevokedAt != nil {
		t.Fatalf("grant proxy reservation = %+v, %v", stored, err)
	}
	replayed, err := repository.GrantProxyReservation(context.Background(), grant)
	if err != nil || !sameProxyReservationGrantIdentity(replayed, grant) {
		t.Fatalf("replay proxy reservation = %+v, %v", replayed, err)
	}
	conflict := grant
	conflict.ReservationID = "reservation-other"
	conflict.GrantEventID = "event-grant-other"
	if _, err := repository.GrantProxyReservation(context.Background(), conflict); !errors.Is(err, ErrProxyReservationConflict) {
		t.Fatalf("same account generation conflict = %v", err)
	}
	if err := repository.ValidateCurrentProxyReservation(
		context.Background(), grant.AccountID, grant.DesiredGeneration, grant.ReservationID, grant.BindingRevision, now,
	); err != nil {
		t.Fatalf("validate current proxy reservation: %v", err)
	}
	revocation := ProxyReservationRevocation{
		ReservationID: grant.ReservationID, AccountID: grant.AccountID, DesiredGeneration: grant.DesiredGeneration,
		ProxyBindingID: grant.ProxyBindingID, BindingRevision: grant.BindingRevision,
		RevokeEventID: "event-revoke-1", RevokedAt: now.Add(time.Second),
	}
	revoked, err := repository.RevokeProxyReservation(context.Background(), revocation)
	if err != nil || revoked.RevokedAt == nil || revoked.RevokeEventID != revocation.RevokeEventID {
		t.Fatalf("revoke proxy reservation = %+v, %v", revoked, err)
	}
	replayed, err = repository.RevokeProxyReservation(context.Background(), revocation)
	if err != nil || replayed.RevokedAt == nil || !replayed.RevokedAt.Equal(revocation.RevokedAt) {
		t.Fatalf("replay proxy reservation revocation = %+v, %v", replayed, err)
	}
	if err := repository.ValidateCurrentProxyReservation(
		context.Background(), grant.AccountID, grant.DesiredGeneration, grant.ReservationID, grant.BindingRevision, now.Add(time.Second),
	); !errors.Is(err, ErrProxyReservationNotFound) {
		t.Fatalf("revoked proxy reservation validation = %v", err)
	}
	changedRevision := revocation
	changedRevision.BindingRevision++
	if _, err := repository.RevokeProxyReservation(context.Background(), changedRevision); !errors.Is(err, ErrProxyReservationConflict) {
		t.Fatalf("changed revocation revision = %v", err)
	}
	grantReplay, err := repository.GrantProxyReservation(context.Background(), grant)
	if err != nil || grantReplay.RevokedAt == nil {
		t.Fatalf("historical grant replay resurrected reservation = %+v, %v", grantReplay, err)
	}
}

func TestProxyReservationOpaqueIDsRejectSecretAndURLFingerprints(t *testing.T) {
	for _, value := range []string{
		"", " leading", "snowman-☃", "https://proxy.example", "sk-ant-secret", "Bearer-token",
		"proxy-password", "access_token_value", "cookie-value",
	} {
		if err := ValidateProxyReservationOpaqueID(value); !errors.Is(err, ErrProxyReservationConflict) {
			t.Errorf("opaque id %q error = %v", value, err)
		}
	}
	for _, value := range []string{"7", "reservation-7", "proxy-binding_7", "event.grant-7"} {
		if err := ValidateProxyReservationOpaqueID(value); err != nil {
			t.Errorf("valid opaque id %q error = %v", value, err)
		}
	}
}

func TestProxyBindingIDRequiresCanonicalPositiveDecimal(t *testing.T) {
	for _, value := range []string{"1", "7", "9223372036854775807"} {
		if err := ValidateProxyBindingID(value); err != nil {
			t.Errorf("valid proxy binding %q error = %v", value, err)
		}
	}
	for _, value := range []string{"", "0", "-1", "+1", "01", "1.0", "1e3", "1.2.3.4", "proxy.example.com", "hunter2", "9223372036854775808"} {
		if err := ValidateProxyBindingID(value); !errors.Is(err, ErrProxyReservationConflict) {
			t.Errorf("invalid proxy binding %q error = %v", value, err)
		}
	}
}

func TestMySQLProxyReservationGrantAndRevokeAreDurableAndIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	grant := testProxyReservationGrant(now)

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO proxy_reservation_grants.*ON DUPLICATE KEY UPDATE`).
		WithArgs(grant.ReservationID, grant.AccountID, grant.DesiredGeneration, grant.ProxyBindingID,
			grant.BindingRevision, grant.GrantEventID, now, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`(?s)SELECT reservation_id, account_id.*FROM proxy_reservation_grants WHERE reservation_id = \? FOR UPDATE`).
		WithArgs(grant.ReservationID).
		WillReturnRows(proxyReservationRows(grant))
	mock.ExpectCommit()
	stored, err := repository.GrantProxyReservation(context.Background(), grant)
	if err != nil || !sameProxyReservationGrantIdentity(stored, grant) {
		t.Fatalf("grant mysql proxy reservation = %+v, %v", stored, err)
	}

	revocation := ProxyReservationRevocation{
		ReservationID: grant.ReservationID, AccountID: grant.AccountID, DesiredGeneration: grant.DesiredGeneration,
		ProxyBindingID: grant.ProxyBindingID, BindingRevision: grant.BindingRevision,
		RevokeEventID: "event-revoke-1", RevokedAt: now.Add(time.Second),
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT reservation_id, account_id.*FROM proxy_reservation_grants WHERE reservation_id = \? FOR UPDATE`).
		WithArgs(grant.ReservationID).
		WillReturnRows(proxyReservationRows(grant))
	mock.ExpectExec(`(?s)UPDATE proxy_reservation_grants.*revoke_event_id = \?.*revoked_at = \?`).
		WithArgs(revocation.RevokeEventID, revocation.RevokedAt, revocation.RevokedAt, grant.ReservationID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	revoked, err := repository.RevokeProxyReservation(context.Background(), revocation)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("revoke mysql proxy reservation = %+v, %v", revoked, err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT reservation_id, account_id.*FROM proxy_reservation_grants WHERE reservation_id = \? FOR UPDATE`).
		WithArgs(grant.ReservationID).
		WillReturnRows(proxyReservationRows(revoked))
	mock.ExpectCommit()
	replayed, err := repository.RevokeProxyReservation(context.Background(), revocation)
	if err != nil || replayed.RevokedAt == nil {
		t.Fatalf("replay mysql revocation = %+v, %v", replayed, err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func testProxyReservationGrant(now time.Time) ProxyReservationGrant {
	return ProxyReservationGrant{
		ReservationID: "reservation-1", AccountID: "account-1", DesiredGeneration: 1,
		ProxyBindingID: "1", BindingRevision: 1, GrantEventID: "event-grant-1",
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
}

func proxyReservationRows(grants ...ProxyReservationGrant) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"reservation_id", "account_id", "desired_generation", "proxy_binding_id", "binding_revision",
		"grant_event_id", "revoke_event_id", "revoked_at", "created_at", "updated_at",
	})
	for _, grant := range grants {
		var revokeEventID any
		var revokedAt any
		if grant.RevokeEventID != "" {
			revokeEventID = grant.RevokeEventID
		}
		if grant.RevokedAt != nil {
			revokedAt = *grant.RevokedAt
		}
		rows.AddRow(
			grant.ReservationID, grant.AccountID, grant.DesiredGeneration, grant.ProxyBindingID,
			grant.BindingRevision, grant.GrantEventID, revokeEventID, revokedAt, grant.CreatedAt, grant.UpdatedAt,
		)
	}
	return rows
}
