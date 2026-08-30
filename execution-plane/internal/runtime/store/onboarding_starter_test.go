package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/go-sql-driver/mysql"
)

func TestMySQLHealthySlotOnboardingStartAtomicallyCreatesWorkflowAndTrustedProxyLease(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	spec := testHealthySlotStartSpec(now)
	intentExpiry := now.Add(90 * time.Second)
	workflow := testAtomicStarterWorkflow(spec, intentExpiry)

	mock.ExpectBegin()
	expectAtomicStarterIntent(mock, spec.IntentID, "account-10380", 7, onboarding.IntentPending, intentExpiry)
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
		WithArgs(spec.IntentID).WillReturnRows(emptyOnboardingWorkflowRows())
	expectAtomicStarterRuntimeBinding(mock, spec, "account-10380", 7).
		WillReturnRows(sqlmock.NewRows([]string{"node_id", "execution_epoch", "image_digest"}).
			AddRow("srv74", 19, workflow.ImageDigest))
	mock.ExpectExec(`(?s)INSERT INTO proxy_leases`).WithArgs(
		spec.ProxyLeaseID, spec.ReservationID, "account-10380", uint64(7), spec.BindingRevision,
		spec.SlotID, uint64(19), now, now,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)INSERT INTO onboarding_workflows`).WithArgs(
		workflow.ID, workflow.IdempotencyKey, workflow.IntentID, workflow.Owner, workflow.AccountID,
		workflow.DesiredGeneration, workflow.NodeID, workflow.SlotID, workflow.ExecutionEpoch,
		workflow.ImageDigest, workflow.CredentialLeaseID, workflow.ProxyLeaseID, workflow.KeyCommandID,
		workflow.ActivationCommandID, workflow.CommandDeadline, workflow.CreatedAt, workflow.UpdatedAt,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	stored, created, err := repository.StartHealthySlotOnboarding(context.Background(), spec)
	if err != nil || !created || stored.ID != workflow.ID || !stored.CommandDeadline.Equal(intentExpiry) {
		t.Fatalf("atomic starter = %+v/%t/%v", stored, created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLHealthySlotOnboardingStartExactReplayPrecedesMutableHealthChecks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	originalSpec := testHealthySlotStartSpec(now)
	intentExpiry := now.Add(5 * time.Minute)
	workflow := testAtomicStarterWorkflow(originalSpec, originalSpec.RequestedCommandDeadline)
	replaySpec := originalSpec
	replaySpec.StartedAt = now.Add(time.Minute)
	replaySpec.ObservationFreshAfter = replaySpec.StartedAt.Add(-30 * time.Second)
	replaySpec.RequestedCommandDeadline = replaySpec.StartedAt.Add(2 * time.Minute)
	lease := ProxyLease{
		ID: originalSpec.ProxyLeaseID, ReservationID: originalSpec.ReservationID, AccountID: workflow.AccountID,
		DesiredGeneration: workflow.DesiredGeneration, BindingRevision: originalSpec.BindingRevision,
		SlotID: workflow.SlotID, ExecutionEpoch: workflow.ExecutionEpoch, CreatedAt: now, UpdatedAt: now,
	}

	mock.ExpectBegin()
	expectAtomicStarterIntent(mock, replaySpec.IntentID, workflow.AccountID, workflow.DesiredGeneration, onboarding.IntentClaimed, intentExpiry)
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
		WithArgs(replaySpec.IntentID).WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT proxy_lease_id, reservation_id, account_id.*FROM proxy_leases WHERE proxy_lease_id = \? FOR UPDATE`).
		WithArgs(lease.ID).WillReturnRows(proxyLeaseRows(lease))
	mock.ExpectCommit()

	stored, created, err := repository.StartHealthySlotOnboarding(context.Background(), replaySpec)
	if err != nil || created || stored.ID != workflow.ID || !stored.CreatedAt.Equal(now) {
		t.Fatalf("atomic starter replay = %+v/%t/%v", stored, created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLHealthySlotOnboardingStartFailsClosedWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name         string
		intentStatus string
		intentExpiry time.Duration
		queryBinding bool
	}{
		{name: "non-pending intent", intentStatus: onboarding.IntentClaimed, intentExpiry: time.Minute},
		{name: "expired intent", intentStatus: onboarding.IntentPending, intentExpiry: 0},
		{name: "stale or missing last_observed_at", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute, queryBinding: true},
		{name: "revoked or expired execution lease", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute, queryBinding: true},
		{name: "wrong or null assignment generation", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute, queryBinding: true},
		{name: "wrong assignment image", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute, queryBinding: true},
		{name: "revoked or mismatched reservation", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute, queryBinding: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository, _ := NewRepository(db)
			now := time.Unix(2_000_000_000, 0).UTC()
			spec := testHealthySlotStartSpec(now)
			mock.ExpectBegin()
			expectAtomicStarterIntent(mock, spec.IntentID, "account-10380", 7, test.intentStatus, now.Add(test.intentExpiry))
			mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
				WithArgs(spec.IntentID).WillReturnRows(emptyOnboardingWorkflowRows())
			if test.queryBinding {
				expectAtomicStarterRuntimeBinding(mock, spec, "account-10380", 7).
					WillReturnRows(sqlmock.NewRows([]string{"node_id", "execution_epoch", "image_digest"}))
			}
			mock.ExpectRollback()
			_, created, err := repository.StartHealthySlotOnboarding(context.Background(), spec)
			if created || !errors.Is(err, onboarding.ErrHealthySlotStartRejected) {
				t.Fatalf("fail-closed result = %t/%v", created, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLHealthySlotOnboardingStartRollsBackProxyLeaseWhenWorkflowInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	spec := testHealthySlotStartSpec(now)
	workflow := testAtomicStarterWorkflow(spec, spec.RequestedCommandDeadline)
	mock.ExpectBegin()
	expectAtomicStarterIntent(mock, spec.IntentID, workflow.AccountID, workflow.DesiredGeneration, onboarding.IntentPending, now.Add(5*time.Minute))
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
		WithArgs(spec.IntentID).WillReturnRows(emptyOnboardingWorkflowRows())
	expectAtomicStarterRuntimeBinding(mock, spec, workflow.AccountID, workflow.DesiredGeneration).
		WillReturnRows(sqlmock.NewRows([]string{"node_id", "execution_epoch", "image_digest"}).
			AddRow(workflow.NodeID, workflow.ExecutionEpoch, workflow.ImageDigest))
	mock.ExpectExec(`(?s)INSERT INTO proxy_leases`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)INSERT INTO onboarding_workflows`).
		WillReturnError(&mysql.MySQLError{Number: 1062, Message: "uq_onboarding_workflows_intent"})
	mock.ExpectRollback()
	_, created, err := repository.StartHealthySlotOnboarding(context.Background(), spec)
	if created || !errors.Is(err, onboarding.ErrHealthySlotStartRejected) {
		t.Fatalf("workflow conflict result = %t/%v", created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func testHealthySlotStartSpec(now time.Time) onboarding.HealthySlotStartSpec {
	return onboarding.HealthySlotStartSpec{
		IntentID: "11111111-2222-4333-8444-555555555555", SlotID: "slot-10380",
		ReservationID: "reservation-10380", BindingRevision: 7,
		WorkflowID: "workflow-10380", IdempotencyKey: "start-10380", Owner: "workflow-10380",
		CredentialLeaseID: "credential-lease-10380", ProxyLeaseID: "proxy-lease-10380",
		KeyCommandID: "key-command-10380", ActivationCommandID: "activation-command-10380",
		StartedAt: now, ObservationFreshAfter: now.Add(-30 * time.Second),
		RequestedCommandDeadline: now.Add(2 * time.Minute),
	}
}

func testAtomicStarterWorkflow(spec onboarding.HealthySlotStartSpec, deadline time.Time) onboarding.Provisioning {
	return onboarding.Provisioning{
		ID: spec.WorkflowID, IdempotencyKey: spec.IdempotencyKey, IntentID: spec.IntentID, Owner: spec.Owner,
		AccountID: "account-10380", DesiredGeneration: 7, NodeID: "srv74", SlotID: spec.SlotID,
		ExecutionEpoch: 19, ImageDigest: "sha256:" + strings.Repeat("a", 64),
		CredentialLeaseID: spec.CredentialLeaseID, ProxyLeaseID: spec.ProxyLeaseID,
		KeyCommandID: spec.KeyCommandID, ActivationCommandID: spec.ActivationCommandID,
		CommandDeadline: deadline, Status: onboarding.ProvisioningPendingKey,
		CreatedAt: spec.StartedAt, UpdatedAt: spec.StartedAt,
	}
}

func expectAtomicStarterIntent(
	mock sqlmock.Sqlmock,
	intentID, accountID string,
	desiredGeneration uint64,
	status string,
	expiresAt time.Time,
) {
	mock.ExpectQuery(`(?s)SELECT intent_id, account_id, desired_generation, status, expires_at.*FROM onboarding_intents.*FOR UPDATE`).
		WithArgs(intentID).WillReturnRows(sqlmock.NewRows([]string{
		"intent_id", "account_id", "desired_generation", "status", "expires_at",
	}).AddRow(intentID, accountID, desiredGeneration, status, expiresAt))
}

func expectAtomicStarterRuntimeBinding(
	mock sqlmock.Sqlmock,
	spec onboarding.HealthySlotStartSpec,
	accountID string,
	desiredGeneration uint64,
) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`(?s)SELECT sa.node_id, sa.execution_epoch, s.image_digest.*
FROM slots s.*slot_assignments.*execution_leases.*proxy_reservation_grants.*
sa.desired_generation = s.desired_generation.*
sa.actual_state = 'running' AND sa.healthy = TRUE.*
sa.image_digest = s.image_digest.*
sa.last_observed_at IS NOT NULL.*sa.last_observed_at >= \?.*sa.last_observed_at <= \?.*
el.revoked_at IS NULL AND el.expires_at > \?.*prg.revoked_at IS NULL.*FOR UPDATE`).WithArgs(
		spec.ReservationID, spec.BindingRevision, spec.SlotID, accountID, desiredGeneration,
		spec.ObservationFreshAfter, spec.StartedAt, spec.StartedAt, spec.StartedAt, spec.StartedAt,
	)
}

func emptyOnboardingWorkflowRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"workflow_id", "idempotency_key", "intent_id", "claim_owner", "account_id", "desired_generation",
		"node_id", "slot_id", "execution_epoch", "image_digest", "credential_lease_id", "proxy_lease_id",
		"key_command_id", "activation_command_id", "command_deadline", "status", "key_id", "key_public_key",
		"error_code", "last_command_id", "created_at", "updated_at",
	})
}

func proxyLeaseRows(lease ProxyLease) *sqlmock.Rows {
	var revokedAt any
	if lease.RevokedAt != nil {
		revokedAt = *lease.RevokedAt
	}
	return sqlmock.NewRows([]string{
		"proxy_lease_id", "reservation_id", "account_id", "desired_generation", "binding_revision",
		"slot_id", "execution_epoch", "revoked_at", "created_at", "updated_at",
	}).AddRow(
		lease.ID, lease.ReservationID, lease.AccountID, lease.DesiredGeneration, lease.BindingRevision,
		lease.SlotID, lease.ExecutionEpoch, revokedAt, lease.CreatedAt, lease.UpdatedAt,
	)
}
