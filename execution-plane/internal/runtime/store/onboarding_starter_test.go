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
	databaseNow := now.Add(5 * time.Second)
	spec := testHealthySlotStartSpec(now)
	intentExpiry := now.Add(90 * time.Second)
	trigger := testClaimedHealthySlotStartTrigger(spec, now, intentExpiry)
	workflow := testAtomicStarterWorkflowAt(spec, intentExpiry, databaseNow)
	binding := testAtomicStarterRuntimeBinding(workflow, databaseNow)

	mock.ExpectBegin()
	expectAtomicStarterTrigger(mock, spec.TriggerEventID, trigger)
	expectAtomicStarterIntent(mock, spec.IntentID, "account-10380", 7, onboarding.IntentPending, intentExpiry)
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
		WithArgs(spec.IntentID).WillReturnRows(emptyOnboardingWorkflowRows())
	expectAtomicStarterRuntimeBinding(mock, spec, "account-10380", 7).
		WillReturnRows(atomicStarterRuntimeBindingRows(binding))
	expectAtomicStarterDatabaseTime(mock, databaseNow)
	mock.ExpectExec(`(?s)INSERT INTO proxy_leases`).WithArgs(
		spec.ProxyLeaseID, spec.ReservationID, "account-10380", uint64(7), spec.BindingRevision,
		spec.SlotID, uint64(19), databaseNow, databaseNow,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)INSERT INTO onboarding_workflows`).WithArgs(
		workflow.ID, workflow.IdempotencyKey, workflow.IntentID, workflow.Owner, workflow.AccountID,
		workflow.DesiredGeneration, workflow.NodeID, workflow.SlotID, workflow.ExecutionEpoch,
		workflow.ImageDigest, workflow.CredentialLeaseID, workflow.ProxyLeaseID, workflow.KeyCommandID,
		workflow.ActivationCommandID, workflow.CommandDeadline, workflow.CreatedAt, workflow.UpdatedAt,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	expectAtomicStarterTriggerStarted(mock, trigger, workflow.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
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
	trigger := testClaimedHealthySlotStartTrigger(originalSpec, now, intentExpiry)
	trigger.Status = onboarding.StartTriggerStarted
	trigger.StartedWorkflowID = workflow.ID
	trigger.StartedAt = healthySlotStartTimePointer(now)
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
	expectAtomicStarterTrigger(mock, replaySpec.TriggerEventID, trigger)
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

func TestMySQLHealthySlotOnboardingStartRecoversClaimedExistingWorkflowByMarkingTriggerStarted(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	spec := testHealthySlotStartSpec(now)
	intentExpiry := now.Add(5 * time.Minute)
	trigger := testClaimedHealthySlotStartTrigger(spec, now, intentExpiry)
	workflow := testAtomicStarterWorkflow(spec, spec.RequestedCommandDeadline)
	lease := ProxyLease{
		ID: spec.ProxyLeaseID, ReservationID: spec.ReservationID, AccountID: workflow.AccountID,
		DesiredGeneration: workflow.DesiredGeneration, BindingRevision: spec.BindingRevision,
		SlotID: workflow.SlotID, ExecutionEpoch: workflow.ExecutionEpoch, CreatedAt: now, UpdatedAt: now,
	}

	mock.ExpectBegin()
	expectAtomicStarterTrigger(mock, spec.TriggerEventID, trigger)
	expectAtomicStarterIntent(mock, spec.IntentID, workflow.AccountID, workflow.DesiredGeneration, onboarding.IntentClaimed, intentExpiry)
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
		WithArgs(spec.IntentID).WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT proxy_lease_id, reservation_id, account_id.*FROM proxy_leases WHERE proxy_lease_id = \? FOR UPDATE`).
		WithArgs(lease.ID).WillReturnRows(proxyLeaseRows(lease))
	expectAtomicStarterDatabaseTime(mock, now)
	expectAtomicStarterTriggerStarted(mock, trigger, workflow.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	stored, created, err := repository.StartHealthySlotOnboarding(context.Background(), spec)
	if err != nil || created || stored.ID != workflow.ID {
		t.Fatalf("claimed workflow recovery = %+v/%t/%v", stored, created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLHealthySlotOnboardingStartRejectsClaimedRecoveryExpiredAtDatabaseTime(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	callerNow := time.Unix(2_000_000_000, 0).UTC()
	databaseNow := callerNow.Add(5 * time.Minute)
	spec := testHealthySlotStartSpec(callerNow)
	intentExpiry := callerNow.Add(10 * time.Minute)
	trigger := testClaimedHealthySlotStartTrigger(spec, callerNow, intentExpiry)
	workflow := testAtomicStarterWorkflow(spec, spec.RequestedCommandDeadline)
	lease := ProxyLease{
		ID: spec.ProxyLeaseID, ReservationID: spec.ReservationID, AccountID: workflow.AccountID,
		DesiredGeneration: workflow.DesiredGeneration, BindingRevision: spec.BindingRevision,
		SlotID: workflow.SlotID, ExecutionEpoch: workflow.ExecutionEpoch, CreatedAt: callerNow, UpdatedAt: callerNow,
	}

	mock.ExpectBegin()
	expectAtomicStarterTrigger(mock, spec.TriggerEventID, trigger)
	expectAtomicStarterIntent(mock, spec.IntentID, workflow.AccountID, workflow.DesiredGeneration, onboarding.IntentClaimed, intentExpiry)
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
		WithArgs(spec.IntentID).WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT proxy_lease_id, reservation_id, account_id.*FROM proxy_leases WHERE proxy_lease_id = \? FOR UPDATE`).
		WithArgs(lease.ID).WillReturnRows(proxyLeaseRows(lease))
	expectAtomicStarterDatabaseTime(mock, databaseNow)
	mock.ExpectRollback()

	_, created, err := repository.StartHealthySlotOnboarding(context.Background(), spec)
	if created || !errors.Is(err, onboarding.ErrHealthySlotStartRejected) {
		t.Fatalf("expired claimed recovery = %t/%v", created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLHealthySlotOnboardingStartRejectsNonCurrentTriggerWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*onboarding.OnboardingStartTrigger)
	}{
		{name: "expired", mutate: func(trigger *onboarding.OnboardingStartTrigger) {
			trigger.Status = onboarding.StartTriggerExpired
			trigger.ClaimOwner = ""
			trigger.ClaimExpiresAt = nil
		}},
		{name: "stale owner", mutate: func(trigger *onboarding.OnboardingStartTrigger) {
			trigger.ClaimOwner = "starter-stale"
		}},
		{name: "stale version", mutate: func(trigger *onboarding.OnboardingStartTrigger) {
			trigger.ClaimVersion++
		}},
		{name: "pending", mutate: func(trigger *onboarding.OnboardingStartTrigger) {
			trigger.Status = onboarding.StartTriggerPending
			trigger.ClaimOwner = ""
			trigger.ClaimExpiresAt = nil
		}},
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
			trigger := testClaimedHealthySlotStartTrigger(spec, now, now.Add(5*time.Minute))
			test.mutate(&trigger)

			mock.ExpectBegin()
			expectAtomicStarterTrigger(mock, spec.TriggerEventID, trigger)
			mock.ExpectRollback()

			_, created, err := repository.StartHealthySlotOnboarding(context.Background(), spec)
			if created || !errors.Is(err, onboarding.ErrHealthySlotStartRejected) {
				t.Fatalf("non-current trigger result = %t/%v", created, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLHealthySlotOnboardingStartFailsClosedWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name          string
		intentStatus  string
		intentExpiry  time.Duration
		bindingResult bool
		checkClock    bool
	}{
		{name: "non-pending intent", intentStatus: onboarding.IntentClaimed, intentExpiry: time.Minute},
		{name: "expired intent", intentStatus: onboarding.IntentPending, intentExpiry: 0, bindingResult: true, checkClock: true},
		{name: "stale or missing last_observed_at", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute},
		{name: "revoked or expired execution lease", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute},
		{name: "wrong or null assignment generation", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute},
		{name: "wrong assignment image", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute},
		{name: "revoked or mismatched reservation", intentStatus: onboarding.IntentPending, intentExpiry: time.Minute},
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
			trigger := testClaimedHealthySlotStartTrigger(spec, now, now.Add(5*time.Minute))
			mock.ExpectBegin()
			expectAtomicStarterTrigger(mock, spec.TriggerEventID, trigger)
			expectAtomicStarterIntent(mock, spec.IntentID, "account-10380", 7, test.intentStatus, now.Add(test.intentExpiry))
			mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
				WithArgs(spec.IntentID).WillReturnRows(emptyOnboardingWorkflowRows())
			if test.intentStatus == onboarding.IntentPending {
				binding := testAtomicStarterRuntimeBinding(testAtomicStarterWorkflow(spec, spec.RequestedCommandDeadline), now)
				rows := sqlmock.NewRows(atomicStarterRuntimeBindingColumns())
				if test.bindingResult {
					rows = atomicStarterRuntimeBindingRows(binding)
				}
				expectAtomicStarterRuntimeBinding(mock, spec, "account-10380", 7).
					WillReturnRows(rows)
			}
			if test.checkClock {
				expectAtomicStarterDatabaseTime(mock, now)
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

func TestMySQLHealthySlotOnboardingStartRevalidatesLockedAuthorityAgainstDatabaseTime(t *testing.T) {
	tests := []struct {
		name        string
		databaseNow time.Time
		mutate      func(*onboarding.OnboardingStartTrigger, *time.Time, *healthySlotStartRuntimeBinding)
	}{
		{
			name:        "caller clock becomes stale while waiting for locks",
			databaseNow: time.Unix(2_000_000_000, 0).UTC().Add(5 * time.Minute),
		},
		{
			name: "trigger intent authorization expired",
			mutate: func(trigger *onboarding.OnboardingStartTrigger, _ *time.Time, _ *healthySlotStartRuntimeBinding) {
				trigger.IntentExpiresAt = time.Unix(2_000_000_010, 0).UTC()
			},
		},
		{
			name: "intent expired",
			mutate: func(_ *onboarding.OnboardingStartTrigger, intentExpiry *time.Time, _ *healthySlotStartRuntimeBinding) {
				*intentExpiry = time.Unix(2_000_000_010, 0).UTC()
			},
		},
		{
			name: "observation too old",
			mutate: func(_ *onboarding.OnboardingStartTrigger, _ *time.Time, binding *healthySlotStartRuntimeBinding) {
				binding.LastObservedAt = time.Unix(2_000_000_010, 0).UTC().Add(-30*time.Second - time.Microsecond)
			},
		},
		{
			name: "observation from database future",
			mutate: func(_ *onboarding.OnboardingStartTrigger, _ *time.Time, binding *healthySlotStartRuntimeBinding) {
				binding.LastObservedAt = time.Unix(2_000_000_010, 0).UTC().Add(time.Microsecond)
			},
		},
		{
			name: "execution lease not created yet",
			mutate: func(_ *onboarding.OnboardingStartTrigger, _ *time.Time, binding *healthySlotStartRuntimeBinding) {
				binding.ExecutionLeaseCreatedAt = time.Unix(2_000_000_010, 0).UTC().Add(time.Microsecond)
			},
		},
		{
			name: "execution lease expired",
			mutate: func(_ *onboarding.OnboardingStartTrigger, _ *time.Time, binding *healthySlotStartRuntimeBinding) {
				binding.ExecutionLeaseExpiresAt = time.Unix(2_000_000_010, 0).UTC()
			},
		},
		{
			name: "reservation not created yet",
			mutate: func(_ *onboarding.OnboardingStartTrigger, _ *time.Time, binding *healthySlotStartRuntimeBinding) {
				binding.ReservationCreatedAt = time.Unix(2_000_000_010, 0).UTC().Add(time.Microsecond)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repository, _ := NewRepository(db)
			callerNow := time.Unix(2_000_000_000, 0).UTC()
			databaseNow := test.databaseNow
			if databaseNow.IsZero() {
				databaseNow = callerNow.Add(10 * time.Second)
			}
			spec := testHealthySlotStartSpec(callerNow)
			intentExpiry := databaseNow.Add(5 * time.Minute)
			trigger := testClaimedHealthySlotStartTrigger(spec, callerNow, intentExpiry)
			workflow := testAtomicStarterWorkflowAt(spec, databaseNow.Add(2*time.Minute), databaseNow)
			binding := testAtomicStarterRuntimeBinding(workflow, databaseNow)
			if test.mutate != nil {
				test.mutate(&trigger, &intentExpiry, &binding)
			}

			mock.ExpectBegin()
			expectAtomicStarterTrigger(mock, spec.TriggerEventID, trigger)
			expectAtomicStarterIntent(mock, spec.IntentID, workflow.AccountID, workflow.DesiredGeneration, onboarding.IntentPending, intentExpiry)
			mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
				WithArgs(spec.IntentID).WillReturnRows(emptyOnboardingWorkflowRows())
			expectAtomicStarterRuntimeBinding(mock, spec, workflow.AccountID, workflow.DesiredGeneration).
				WillReturnRows(atomicStarterRuntimeBindingRows(binding))
			expectAtomicStarterDatabaseTime(mock, databaseNow)
			mock.ExpectRollback()

			_, created, err := repository.StartHealthySlotOnboarding(context.Background(), spec)
			if created || !errors.Is(err, onboarding.ErrHealthySlotStartRejected) {
				t.Fatalf("database-time authority result = %t/%v", created, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMySQLHealthySlotOnboardingStartFinalTransitionUsesDatabaseTimeFence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	callerNow := time.Unix(2_000_000_000, 0).UTC()
	databaseNow := callerNow.Add(5 * time.Second)
	spec := testHealthySlotStartSpec(callerNow)
	intentExpiry := databaseNow.Add(5 * time.Minute)
	trigger := testClaimedHealthySlotStartTrigger(spec, callerNow, intentExpiry)
	workflow := testAtomicStarterWorkflowAt(spec, databaseNow.Add(2*time.Minute), databaseNow)
	binding := testAtomicStarterRuntimeBinding(workflow, databaseNow)

	mock.ExpectBegin()
	expectAtomicStarterTrigger(mock, spec.TriggerEventID, trigger)
	expectAtomicStarterIntent(mock, spec.IntentID, workflow.AccountID, workflow.DesiredGeneration, onboarding.IntentPending, intentExpiry)
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
		WithArgs(spec.IntentID).WillReturnRows(emptyOnboardingWorkflowRows())
	expectAtomicStarterRuntimeBinding(mock, spec, workflow.AccountID, workflow.DesiredGeneration).
		WillReturnRows(atomicStarterRuntimeBindingRows(binding))
	expectAtomicStarterDatabaseTime(mock, databaseNow)
	mock.ExpectExec(`(?s)INSERT INTO proxy_leases`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)INSERT INTO onboarding_workflows`).WillReturnResult(sqlmock.NewResult(1, 1))
	expectAtomicStarterTriggerStarted(mock, trigger, workflow.ID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	_, created, err := repository.StartHealthySlotOnboarding(context.Background(), spec)
	if created || !errors.Is(err, onboarding.ErrHealthySlotStartRejected) {
		t.Fatalf("final database-time fence result = %t/%v", created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLHealthySlotOnboardingStartRollsBackProxyLeaseAndDoesNotMarkTriggerStartedWhenWorkflowInsertFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	spec := testHealthySlotStartSpec(now)
	trigger := testClaimedHealthySlotStartTrigger(spec, now, now.Add(5*time.Minute))
	workflow := testAtomicStarterWorkflow(spec, spec.RequestedCommandDeadline)
	binding := testAtomicStarterRuntimeBinding(workflow, now)
	mock.ExpectBegin()
	expectAtomicStarterTrigger(mock, spec.TriggerEventID, trigger)
	expectAtomicStarterIntent(mock, spec.IntentID, workflow.AccountID, workflow.DesiredGeneration, onboarding.IntentPending, now.Add(5*time.Minute))
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE intent_id = \? FOR UPDATE`).
		WithArgs(spec.IntentID).WillReturnRows(emptyOnboardingWorkflowRows())
	expectAtomicStarterRuntimeBinding(mock, spec, workflow.AccountID, workflow.DesiredGeneration).
		WillReturnRows(atomicStarterRuntimeBindingRows(binding))
	expectAtomicStarterDatabaseTime(mock, now)
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
		TriggerEventID: "event-onboarding-42", TriggerClaimOwner: "starter-1", TriggerClaimVersion: 3,
		IntentID: "11111111-2222-4333-8444-555555555555", SlotID: "slot-10380",
		ReservationID: "reservation-10380", BindingRevision: 7,
		WorkflowID: "workflow-10380", IdempotencyKey: "start-10380", Owner: "workflow-10380",
		CredentialLeaseID: "credential-lease-10380", ProxyLeaseID: "proxy-lease-10380",
		KeyCommandID: "key-command-10380", ActivationCommandID: "activation-command-10380",
		StartedAt: now, ObservationFreshAfter: now.Add(-30 * time.Second),
		RequestedCommandDeadline: now.Add(2 * time.Minute),
	}
}

func testClaimedHealthySlotStartTrigger(
	spec onboarding.HealthySlotStartSpec,
	now time.Time,
	intentExpiry time.Time,
) onboarding.OnboardingStartTrigger {
	claimExpiry := now.Add(4 * time.Minute)
	projectedAt := now.Add(-time.Minute)
	return onboarding.OnboardingStartTrigger{
		StartTriggerProjection: onboarding.StartTriggerProjection{
			SourceSequence: 42, EventID: spec.TriggerEventID,
			EventType: "account.runtime.provision_requested", EventCreatedAt: projectedAt.Add(-time.Second),
			IntentID: spec.IntentID, AccountID: "account-10380", DesiredGeneration: 7,
			SlotID: spec.SlotID, Provider: "docker", RequiredLabels: map[string]string{"region": "ap-shanghai"},
			ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500,
			MemoryRequestBytes: 256 << 20, ProjectedAt: projectedAt,
		},
		IntentExpiresAt: intentExpiry, ReservationID: spec.ReservationID, BindingRevision: spec.BindingRevision,
		Status: onboarding.StartTriggerClaimed, ClaimOwner: spec.TriggerClaimOwner,
		ClaimVersion: spec.TriggerClaimVersion, ClaimExpiresAt: &claimExpiry, NextAttemptAt: projectedAt,
	}
}

func testAtomicStarterWorkflow(spec onboarding.HealthySlotStartSpec, deadline time.Time) onboarding.Provisioning {
	return testAtomicStarterWorkflowAt(spec, deadline, spec.StartedAt)
}

func testAtomicStarterWorkflowAt(
	spec onboarding.HealthySlotStartSpec,
	deadline time.Time,
	createdAt time.Time,
) onboarding.Provisioning {
	return onboarding.Provisioning{
		ID: spec.WorkflowID, IdempotencyKey: spec.IdempotencyKey, IntentID: spec.IntentID, Owner: spec.Owner,
		AccountID: "account-10380", DesiredGeneration: 7, NodeID: "srv74", SlotID: spec.SlotID,
		ExecutionEpoch: 19, ImageDigest: "sha256:" + strings.Repeat("a", 64),
		CredentialLeaseID: spec.CredentialLeaseID, ProxyLeaseID: spec.ProxyLeaseID,
		KeyCommandID: spec.KeyCommandID, ActivationCommandID: spec.ActivationCommandID,
		CommandDeadline: deadline, Status: onboarding.ProvisioningPendingKey,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
}

func testAtomicStarterRuntimeBinding(
	workflow onboarding.Provisioning,
	databaseNow time.Time,
) healthySlotStartRuntimeBinding {
	return healthySlotStartRuntimeBinding{
		NodeID: workflow.NodeID, ExecutionEpoch: workflow.ExecutionEpoch, ImageDigest: workflow.ImageDigest,
		LastObservedAt:          databaseNow.Add(-5 * time.Second),
		ExecutionLeaseCreatedAt: databaseNow.Add(-time.Minute),
		ExecutionLeaseExpiresAt: databaseNow.Add(5 * time.Minute),
		ReservationCreatedAt:    databaseNow.Add(-time.Minute),
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

func expectAtomicStarterTrigger(
	mock sqlmock.Sqlmock,
	eventID string,
	trigger onboarding.OnboardingStartTrigger,
) {
	mock.ExpectQuery(`(?s)SELECT source_sequence, event_id, event_type.*FROM onboarding_start_triggers WHERE event_id = \? FOR UPDATE`).
		WithArgs(eventID).WillReturnRows(onboardingStartTriggerRows(trigger))
}

func expectAtomicStarterTriggerStarted(
	mock sqlmock.Sqlmock,
	trigger onboarding.OnboardingStartTrigger,
	workflowID string,
) *sqlmock.ExpectedExec {
	return mock.ExpectExec(`(?s)UPDATE onboarding_start_triggers.*SET status = 'started'.*started_workflow_id = \?.*started_at = UTC_TIMESTAMP\(6\).*WHERE event_id = \? AND status = 'claimed'.*claim_owner = \?.*claim_version = \?.*claim_expires_at > UTC_TIMESTAMP\(6\).*intent_expires_at > UTC_TIMESTAMP\(6\)`).
		WithArgs(workflowID, trigger.EventID, trigger.ClaimOwner, trigger.ClaimVersion)
}

func expectAtomicStarterDatabaseTime(mock sqlmock.Sqlmock, databaseNow time.Time) {
	mock.ExpectQuery(`SELECT UTC_TIMESTAMP\(6\)`).
		WillReturnRows(sqlmock.NewRows([]string{"UTC_TIMESTAMP(6)"}).AddRow(databaseNow))
}

func healthySlotStartTimePointer(value time.Time) *time.Time {
	return &value
}

func expectAtomicStarterRuntimeBinding(
	mock sqlmock.Sqlmock,
	spec onboarding.HealthySlotStartSpec,
	accountID string,
	desiredGeneration uint64,
) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`(?s)SELECT sa.node_id, sa.execution_epoch, s.image_digest, sa.last_observed_at,.*
FROM slots s.*slot_assignments.*execution_leases.*proxy_reservation_grants.*
sa.desired_generation = s.desired_generation.*
sa.actual_state = 'running' AND sa.healthy = TRUE.*
sa.image_digest = s.image_digest.*
sa.last_observed_at IS NOT NULL.*el.node_id = sa.node_id AND el.revoked_at IS NULL.*
prg.revoked_at IS NULL.*FOR UPDATE`).WithArgs(
		spec.ReservationID, spec.BindingRevision, spec.SlotID, accountID, desiredGeneration,
	)
}

func atomicStarterRuntimeBindingColumns() []string {
	return []string{
		"node_id", "execution_epoch", "image_digest", "last_observed_at",
		"execution_lease_created_at", "execution_lease_expires_at", "reservation_created_at",
	}
}

func atomicStarterRuntimeBindingRows(binding healthySlotStartRuntimeBinding) *sqlmock.Rows {
	return sqlmock.NewRows(atomicStarterRuntimeBindingColumns()).AddRow(
		binding.NodeID, binding.ExecutionEpoch, binding.ImageDigest, binding.LastObservedAt,
		binding.ExecutionLeaseCreatedAt, binding.ExecutionLeaseExpiresAt, binding.ReservationCreatedAt,
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
