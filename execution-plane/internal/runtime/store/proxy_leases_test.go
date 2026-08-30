package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/go-sql-driver/mysql"
)

func TestMemoryProxyLeaseRequiresHealthyCurrentExecutionAndTrustedReservation(t *testing.T) {
	_, repository, _, now := credentialTestRuntime(t)
	markProxyLeaseAssignmentHealthy(t, repository, now)
	lease := testProxyLease(now)
	if _, err := repository.GrantProxyLease(context.Background(), lease); !errors.Is(err, ErrProxyLeaseConflict) {
		t.Fatalf("proxy lease without trusted reservation = %v", err)
	}
	grantTestProxyReservation(t, repository, lease, now.Add(-time.Second))
	granted, err := repository.GrantProxyLease(context.Background(), lease)
	if err != nil || granted.ID != lease.ID {
		t.Fatalf("grant proxy lease = %+v, %v", granted, err)
	}
	if err := repository.ValidateCurrentProxyLease(
		context.Background(), provider.RuntimeAccountID(lease.AccountID), lease.SlotID, lease.ExecutionEpoch, lease.ID, now,
	); err != nil {
		t.Fatalf("validate current proxy lease: %v", err)
	}
	if err := repository.ValidateCurrentProxyLease(
		context.Background(), provider.RuntimeAccountID(lease.AccountID), lease.SlotID, lease.ExecutionEpoch,
		lease.ID, now.Add(-time.Microsecond),
	); !errors.Is(err, ErrProxyLeaseNotFound) {
		t.Fatalf("future proxy lease validated before creation: %v", err)
	}
	if err := repository.ValidateCurrentProxyLease(
		context.Background(), provider.RuntimeAccountID("other-account"), lease.SlotID, lease.ExecutionEpoch, lease.ID, now,
	); !errors.Is(err, ErrProxyLeaseNotFound) {
		t.Fatalf("wrong account binding error = %v", err)
	}
	if err := repository.RevokeProxyLease(context.Background(), lease.ID, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repository.ValidateCurrentProxyLease(
		context.Background(), provider.RuntimeAccountID(lease.AccountID), lease.SlotID, lease.ExecutionEpoch, lease.ID, now.Add(time.Second),
	); !errors.Is(err, ErrProxyLeaseNotFound) {
		t.Fatalf("revoked proxy lease error = %v", err)
	}
}

func TestMemoryProxyLeaseFailsClosedAfterExecutionExpiryOrReservationRevoke(t *testing.T) {
	_, repository, _, now := credentialTestRuntime(t)
	markProxyLeaseAssignmentHealthy(t, repository, now)
	lease := testProxyLease(now)
	grantTestProxyReservation(t, repository, lease, now.Add(-time.Second))
	if _, err := repository.GrantProxyLease(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	if err := repository.ValidateCurrentProxyLease(
		context.Background(), provider.RuntimeAccountID(lease.AccountID), lease.SlotID, lease.ExecutionEpoch,
		lease.ID, now.Add(2*time.Minute),
	); !errors.Is(err, ErrProxyLeaseNotFound) {
		t.Fatalf("expired execution proxy validation error = %v", err)
	}
	if _, err := repository.RevokeProxyReservation(context.Background(), ProxyReservationRevocation{
		ReservationID: lease.ReservationID, AccountID: lease.AccountID, DesiredGeneration: lease.DesiredGeneration,
		ProxyBindingID: "1", BindingRevision: lease.BindingRevision,
		RevokeEventID: "event-revoke-1", RevokedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ValidateCurrentProxyLease(
		context.Background(), provider.RuntimeAccountID(lease.AccountID), lease.SlotID, lease.ExecutionEpoch,
		lease.ID, now.Add(time.Second),
	); !errors.Is(err, ErrProxyLeaseNotFound) {
		t.Fatalf("revoked reservation proxy validation error = %v", err)
	}
}

func TestMemoryProxyLeaseRejectsAssignmentFromStaleDesiredGeneration(t *testing.T) {
	_, repository, _, now := credentialTestRuntime(t)
	markProxyLeaseAssignmentHealthy(t, repository, now)
	slot, err := repository.GetSlot(context.Background(), "slot-1")
	if err != nil {
		t.Fatal(err)
	}
	slot.DesiredGeneration = 2
	slot.UpdatedAt = now.Add(time.Second)
	if _, err := repository.PutDesiredSlot(context.Background(), slot); err != nil {
		t.Fatal(err)
	}
	lease := testProxyLease(now.Add(time.Second))
	lease.DesiredGeneration = 2
	grantTestProxyReservation(t, repository, lease, now)
	if _, err := repository.GrantProxyLease(context.Background(), lease); !errors.Is(err, ErrProxyLeaseConflict) {
		t.Fatalf("stale assignment desired generation proxy grant = %v", err)
	}
}

func TestMemoryProxyLeaseRejectsExecutionLeaseCreatedAfterGrant(t *testing.T) {
	_, repository, _, now := credentialTestRuntime(t)
	markProxyLeaseAssignmentHealthy(t, repository, now)
	repository.mu.Lock()
	key := executionLeaseKey("slot-1", 1)
	executionLease := repository.executionLeases[key]
	executionLease.CreatedAt = now.Add(time.Microsecond)
	repository.executionLeases[key] = executionLease
	repository.mu.Unlock()
	lease := testProxyLease(now)
	grantTestProxyReservation(t, repository, lease, now.Add(-time.Second))
	if _, err := repository.GrantProxyLease(context.Background(), lease); !errors.Is(err, ErrProxyLeaseConflict) {
		t.Fatalf("future execution lease proxy grant = %v", err)
	}
}

func TestMySQLProxyLeaseStoresOnlyOpaqueTrustedRuntimeBinding(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 123_456_789).UTC()
	durableNow := canonicalRuntimeTime(now)
	lease := ProxyLease{
		ID: "proxy-lease-10380", ReservationID: "reservation-10380", AccountID: "account-10380",
		DesiredGeneration: 7, BindingRevision: 3, SlotID: "slot-10380", ExecutionEpoch: 19,
		CreatedAt: now, UpdatedAt: now,
	}
	columns := []string{
		"proxy_lease_id", "reservation_id", "account_id", "desired_generation", "binding_revision",
		"slot_id", "execution_epoch", "revoked_at", "created_at", "updated_at",
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT s.account_id.*FROM slots.*slot_assignments.*execution_leases.*proxy_reservation_grants.*sa.desired_generation = s.desired_generation.*FOR UPDATE`).
		WithArgs(lease.ReservationID, lease.BindingRevision, lease.SlotID, lease.AccountID,
			lease.DesiredGeneration, lease.ExecutionEpoch, durableNow, durableNow, durableNow).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(lease.AccountID))
	mock.ExpectQuery(`(?s)SELECT proxy_lease_id, reservation_id, account_id.*FROM proxy_leases.*FOR UPDATE`).
		WithArgs(lease.ID).
		WillReturnRows(sqlmock.NewRows(columns))
	mock.ExpectExec(`(?s)INSERT INTO proxy_leases`).
		WithArgs(lease.ID, lease.ReservationID, lease.AccountID, lease.DesiredGeneration, lease.BindingRevision,
			lease.SlotID, lease.ExecutionEpoch, durableNow, durableNow).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`(?s)SELECT proxy_lease_id, reservation_id, account_id.*FROM proxy_leases WHERE proxy_lease_id = \?`).
		WithArgs(lease.ID).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(
			lease.ID, lease.ReservationID, lease.AccountID, lease.DesiredGeneration, lease.BindingRevision,
			lease.SlotID, lease.ExecutionEpoch, nil, durableNow, durableNow,
		))
	granted, err := repository.GrantProxyLease(context.Background(), lease)
	if err != nil || granted.ID != lease.ID {
		t.Fatalf("grant mysql proxy lease = %+v, %v", granted, err)
	}
	if !granted.CreatedAt.Equal(durableNow) {
		t.Fatalf("durable proxy lease time = %s, want %s", granted.CreatedAt, durableNow)
	}

	// Replay the caller's original nanosecond candidate. The repository must
	// canonicalize it to MySQL DATETIME(6) before exact identity comparison.
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT s.account_id.*FROM slots.*slot_assignments.*execution_leases.*proxy_reservation_grants.*sa.desired_generation = s.desired_generation.*FOR UPDATE`).
		WithArgs(lease.ReservationID, lease.BindingRevision, lease.SlotID, lease.AccountID,
			lease.DesiredGeneration, lease.ExecutionEpoch, durableNow, durableNow, durableNow).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(lease.AccountID))
	mock.ExpectQuery(`(?s)SELECT proxy_lease_id, reservation_id, account_id.*FROM proxy_leases.*FOR UPDATE`).
		WithArgs(lease.ID).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(
			lease.ID, lease.ReservationID, lease.AccountID, lease.DesiredGeneration, lease.BindingRevision,
			lease.SlotID, lease.ExecutionEpoch, nil, durableNow, durableNow,
		))
	mock.ExpectCommit()
	mock.ExpectQuery(`(?s)SELECT proxy_lease_id, reservation_id, account_id.*FROM proxy_leases WHERE proxy_lease_id = \?`).
		WithArgs(lease.ID).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(
			lease.ID, lease.ReservationID, lease.AccountID, lease.DesiredGeneration, lease.BindingRevision,
			lease.SlotID, lease.ExecutionEpoch, nil, durableNow, durableNow,
		))
	if _, err := repository.GrantProxyLease(context.Background(), lease); err != nil {
		t.Fatalf("replay mysql proxy lease with nanoseconds: %v", err)
	}

	mock.ExpectQuery(`(?s)SELECT pl.account_id.*FROM proxy_leases pl.*proxy_reservation_grants.*pl.created_at <= \?.*sa.desired_generation = s.desired_generation.*el.created_at <= \?.*prg.created_at <= \?`).
		WithArgs(lease.ID, lease.SlotID, lease.ExecutionEpoch, durableNow, durableNow, durableNow, durableNow).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(lease.AccountID))
	if err := repository.ValidateCurrentProxyLease(
		context.Background(), provider.RuntimeAccountID(lease.AccountID), lease.SlotID, lease.ExecutionEpoch, lease.ID, now,
	); err != nil {
		t.Fatalf("validate mysql proxy lease: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLProxyLeaseMapsUniqueEpochConflict(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	lease := ProxyLease{
		ID: "proxy-lease-second", ReservationID: "reservation-10380", AccountID: "account-10380",
		DesiredGeneration: 7, BindingRevision: 3, SlotID: "slot-10380", ExecutionEpoch: 19,
		CreatedAt: now, UpdatedAt: now,
	}
	columns := []string{
		"proxy_lease_id", "reservation_id", "account_id", "desired_generation", "binding_revision",
		"slot_id", "execution_epoch", "revoked_at", "created_at", "updated_at",
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT s.account_id.*FROM slots.*slot_assignments.*execution_leases.*proxy_reservation_grants.*sa.desired_generation = s.desired_generation.*FOR UPDATE`).
		WithArgs(lease.ReservationID, lease.BindingRevision, lease.SlotID, lease.AccountID,
			lease.DesiredGeneration, lease.ExecutionEpoch, now, now, now).
		WillReturnRows(sqlmock.NewRows([]string{"account_id"}).AddRow(lease.AccountID))
	mock.ExpectQuery(`(?s)SELECT proxy_lease_id, reservation_id, account_id.*FROM proxy_leases.*FOR UPDATE`).
		WithArgs(lease.ID).
		WillReturnRows(sqlmock.NewRows(columns))
	mock.ExpectExec(`(?s)INSERT INTO proxy_leases`).
		WithArgs(lease.ID, lease.ReservationID, lease.AccountID, lease.DesiredGeneration, lease.BindingRevision,
			lease.SlotID, lease.ExecutionEpoch, now, now).
		WillReturnError(&mysql.MySQLError{Number: 1062, Message: "uq_proxy_leases_slot_epoch"})
	mock.ExpectRollback()

	if _, err := repository.GrantProxyLease(context.Background(), lease); !errors.Is(err, ErrProxyLeaseConflict) {
		t.Fatalf("same epoch unique conflict = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func testProxyLease(now time.Time) ProxyLease {
	return ProxyLease{
		ID: "proxy-lease-1", ReservationID: "reservation-1", AccountID: "account-1",
		DesiredGeneration: 1, BindingRevision: 1, SlotID: "slot-1", ExecutionEpoch: 1,
		CreatedAt: now.UTC(), UpdatedAt: now.UTC(),
	}
}

func markProxyLeaseAssignmentHealthy(t *testing.T, repository *MemoryRepository, now time.Time) {
	t.Helper()
	if _, err := repository.ObserveAssignment(context.Background(), AssignmentObservation{
		SlotID: "slot-1", ExecutionEpoch: 1, ProviderRef: "container-1", ActualState: "running",
		Healthy: true, ObservedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func grantTestProxyReservation(t *testing.T, repository *MemoryRepository, lease ProxyLease, grantedAt time.Time) {
	t.Helper()
	if _, err := repository.GrantProxyReservation(context.Background(), ProxyReservationGrant{
		ReservationID: lease.ReservationID, AccountID: lease.AccountID, DesiredGeneration: lease.DesiredGeneration,
		ProxyBindingID: "1", BindingRevision: lease.BindingRevision,
		GrantEventID: "event-grant-" + lease.ReservationID, CreatedAt: grantedAt.UTC(), UpdatedAt: grantedAt.UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}
