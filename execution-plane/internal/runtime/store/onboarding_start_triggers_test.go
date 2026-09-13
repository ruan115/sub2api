package store

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

func TestMySQLProjectOnboardingStartTriggerAtomicallyProjectsReadySlotAndFrozenAuthority(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 123_456_000).UTC()
	projection := testStartTriggerProjection(now)
	intentExpiry := now.Add(10 * time.Minute)
	labelsJSON, _ := json.Marshal(projection.RequiredLabels)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT source_sequence, event_id, event_type.*FROM onboarding_start_triggers WHERE event_id = \? FOR UPDATE`).
		WithArgs(projection.EventID).WillReturnRows(emptyOnboardingStartTriggerRows())
	mock.ExpectQuery(regexp.QuoteMeta(onboardingStartTriggerIntentMetadataSelect)).
		WithArgs(projection.IntentID).WillReturnRows(sqlmock.NewRows([]string{
		"intent_id", "account_id", "desired_generation", "status", "expires_at",
	}).AddRow(projection.IntentID, projection.AccountID, projection.DesiredGeneration, onboarding.IntentPending, intentExpiry))
	mock.ExpectQuery(`(?s)SELECT slot_id, account_id, provider, desired_state.*FROM slots WHERE slot_id = \? FOR UPDATE`).
		WithArgs(projection.SlotID).WillReturnRows(emptySlotRows())
	mock.ExpectExec(`(?s)INSERT INTO slots .*VALUES \(\?, \?, \?, 'ready', \?, 1`).WithArgs(
		projection.SlotID, projection.AccountID, projection.Provider, projection.DesiredGeneration,
		labelsJSON, projection.ImageDigest, projection.CPURequestMillis, projection.MemoryRequestBytes,
		projection.ProjectedAt, projection.ProjectedAt,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`(?s)SELECT reservation_id, binding_revision.*FROM proxy_reservation_grants.*FOR UPDATE`).
		WithArgs(projection.AccountID, projection.DesiredGeneration, projection.ProjectedAt).
		WillReturnRows(sqlmock.NewRows([]string{"reservation_id", "binding_revision"}).AddRow("reservation-10380", 9))
	mock.ExpectExec(`(?s)INSERT INTO onboarding_start_triggers`).WithArgs(
		projection.SourceSequence, projection.EventID, projection.EventType, projection.EventCreatedAt,
		projection.IntentID, intentExpiry, projection.AccountID, projection.DesiredGeneration,
		projection.SlotID, projection.Provider, labelsJSON, projection.ImageDigest,
		projection.CPURequestMillis, projection.MemoryRequestBytes, "reservation-10380", uint64(9),
		projection.ProjectedAt, projection.ProjectedAt,
	).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	stored, created, err := repository.ProjectOnboardingStartTrigger(context.Background(), projection)
	if err != nil || !created || stored.Status != onboarding.StartTriggerPending ||
		stored.ReservationID != "reservation-10380" || stored.BindingRevision != 9 ||
		!stored.IntentExpiresAt.Equal(intentExpiry) || !stored.NextAttemptAt.Equal(now) {
		t.Fatalf("projected start trigger = %+v/%t/%v", stored, created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLProjectOnboardingStartTriggerExactReplayUsesFrozenPolicyAndNoMutableState(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	originalProjection := testStartTriggerProjection(now)
	stored := testOnboardingStartTrigger(originalProjection, now.Add(10*time.Minute))
	claimExpiry := now.Add(5 * time.Minute)
	startedAt := now.Add(time.Minute)
	stored.Status = onboarding.StartTriggerStarted
	stored.ClaimOwner = "starter-1"
	stored.ClaimVersion = 4
	stored.ClaimExpiresAt = &claimExpiry
	stored.StartedWorkflowID = "workflow-10380"
	stored.StartedAt = &startedAt

	replay := originalProjection
	replay.ProjectedAt = now.Add(30 * time.Minute)
	replay.RequiredLabels = map[string]string{"region": "changed"}
	replay.ImageDigest = ""
	replay.CPURequestMillis = 0
	replay.MemoryRequestBytes = 0
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT source_sequence, event_id, event_type.*FROM onboarding_start_triggers WHERE event_id = \? FOR UPDATE`).
		WithArgs(replay.EventID).WillReturnRows(onboardingStartTriggerRows(stored))
	mock.ExpectCommit()

	replayed, created, err := repository.ProjectOnboardingStartTrigger(context.Background(), replay)
	if err != nil || created || replayed.Status != onboarding.StartTriggerStarted ||
		replayed.ImageDigest != originalProjection.ImageDigest || replayed.CPURequestMillis != originalProjection.CPURequestMillis {
		t.Fatalf("exact trigger replay = %+v/%t/%v", replayed, created, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLClaimDueOnboardingStartTriggersExpiresDeadRowsAndFencesClaims(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_100, 0).UTC()
	query := onboarding.StartTriggerClaimQuery{Owner: "starter-2", ClaimedAt: now, ClaimTTL: 30 * time.Second, Limit: 2}
	first := testOnboardingStartTrigger(testStartTriggerProjection(now.Add(-time.Minute)), now.Add(10*time.Minute))
	secondProjection := testStartTriggerProjection(now.Add(-2 * time.Minute))
	secondProjection.SourceSequence = 43
	secondProjection.EventID = "event-onboarding-43"
	secondProjection.IntentID = "22222222-3333-4444-8555-666666666666"
	secondProjection.AccountID = "10381"
	secondProjection.SlotID = "slot-10381"
	second := testOnboardingStartTrigger(secondProjection, now.Add(20*time.Minute))
	oldClaimExpiry := now.Add(-time.Second)
	second.Status = onboarding.StartTriggerClaimed
	second.ClaimOwner = "dead-starter"
	second.ClaimVersion = 7
	second.ClaimExpiresAt = &oldClaimExpiry

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE onboarding_start_triggers.*SET status = 'expired'.*intent_expires_at <= \?`).
		WithArgs(now, now, query.Limit).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(onboardingStartTriggerClaimSelect)).
		WithArgs(now, now, now, query.Limit).WillReturnRows(onboardingStartTriggerRows(first, second))
	claimExpiry := now.Add(query.ClaimTTL)
	mock.ExpectExec(`(?s)UPDATE onboarding_start_triggers.*SET status = 'claimed'.*claim_version = claim_version \+ 1`).
		WithArgs(query.Owner, claimExpiry, first.EventID, first.ClaimVersion, now, now, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE onboarding_start_triggers.*SET status = 'claimed'.*claim_version = claim_version \+ 1`).
		WithArgs(query.Owner, claimExpiry, second.EventID, second.ClaimVersion, now, now, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claimed, err := repository.ClaimDueOnboardingStartTriggers(context.Background(), query)
	if err != nil || len(claimed) != 2 || claimed[0].ClaimVersion != 1 || claimed[1].ClaimVersion != 8 {
		t.Fatalf("claimed triggers = %+v/%v", claimed, err)
	}
	for _, trigger := range claimed {
		if trigger.Status != onboarding.StartTriggerClaimed || trigger.ClaimOwner != query.Owner ||
			trigger.ClaimExpiresAt == nil || !trigger.ClaimExpiresAt.Equal(claimExpiry) {
			t.Fatalf("claim fence = %+v", trigger)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMySQLRetryOnboardingStartTriggerRequiresCurrentUnexpiredClaim(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	retry := onboarding.StartTriggerRetry{
		EventID: "event-onboarding-42", Owner: "starter-1", ClaimVersion: 3,
		RetriedAt: now, NextAttemptAt: now.Add(time.Minute), ErrorCode: "slot_not_healthy",
	}
	mock.ExpectExec(`(?s)UPDATE onboarding_start_triggers.*SET status = 'pending'.*attempt_count = attempt_count \+ 1`).
		WithArgs(retry.NextAttemptAt, retry.ErrorCode, retry.EventID, retry.Owner, retry.ClaimVersion, retry.RetriedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.RetryOnboardingStartTrigger(context.Background(), retry); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(`(?s)UPDATE onboarding_start_triggers.*SET status = 'pending'.*attempt_count = attempt_count \+ 1`).
		WithArgs(retry.NextAttemptAt, retry.ErrorCode, retry.EventID, retry.Owner, retry.ClaimVersion, retry.RetriedAt).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repository.RetryOnboardingStartTrigger(context.Background(), retry); !errors.Is(err, onboarding.ErrStartTriggerClaimLost) {
		t.Fatalf("stale retry error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingStartTriggerQueriesNeverReadIntentCiphertextOrJoinMutableHealth(t *testing.T) {
	lowerIntentQuery := strings.ToLower(onboardingStartTriggerIntentMetadataSelect)
	for _, forbidden := range []string{"ciphertext", "encrypted_dek", "nonce", "aad_json", "kms_key"} {
		if strings.Contains(lowerIntentQuery, forbidden) {
			t.Fatalf("intent metadata query reads %q", forbidden)
		}
	}
	lowerClaimQuery := strings.ToLower(onboardingStartTriggerClaimSelect)
	for _, forbidden := range []string{"slot_assignments", "execution_leases", "proxy_reservation_grants", "onboarding_workflows"} {
		if strings.Contains(lowerClaimQuery, forbidden) {
			t.Fatalf("claim query depends on mutable %q", forbidden)
		}
	}
	for _, required := range []string{"order by next_attempt_at, source_sequence, intent_id", "for update skip locked"} {
		if !strings.Contains(lowerClaimQuery, required) {
			t.Fatalf("claim query missing %q", required)
		}
	}
}

func testStartTriggerProjection(now time.Time) onboarding.StartTriggerProjection {
	return onboarding.StartTriggerProjection{
		SourceSequence: 42, EventID: "event-onboarding-42",
		EventType: "account.runtime.provision_requested", EventCreatedAt: now.Add(-time.Second),
		IntentID: "11111111-2222-4333-8444-555555555555", AccountID: "10380", DesiredGeneration: 7,
		SlotID: "slot-10380", Provider: "docker", RequiredLabels: map[string]string{"region": "ap-shanghai"},
		ImageDigest: "sha256:" + strings.Repeat("a", 64), CPURequestMillis: 500, MemoryRequestBytes: 256 << 20,
		ProjectedAt: now,
	}
}

func testOnboardingStartTrigger(projection onboarding.StartTriggerProjection, intentExpiry time.Time) onboarding.OnboardingStartTrigger {
	return onboarding.OnboardingStartTrigger{
		StartTriggerProjection: projection,
		IntentExpiresAt:        intentExpiry,
		ReservationID:          "reservation-" + projection.AccountID,
		BindingRevision:        projection.DesiredGeneration,
		Status:                 onboarding.StartTriggerPending,
		NextAttemptAt:          projection.ProjectedAt,
	}
}

func emptyOnboardingStartTriggerRows() *sqlmock.Rows {
	return onboardingStartTriggerRows()
}

func onboardingStartTriggerRows(triggers ...onboarding.OnboardingStartTrigger) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"source_sequence", "event_id", "event_type", "event_created_at",
		"intent_id", "intent_expires_at", "account_id", "desired_generation",
		"slot_id", "provider", "required_labels_json", "image_digest", "cpu_request_millis", "memory_request_bytes",
		"reservation_id", "binding_revision", "status", "claim_owner", "claim_version", "claim_expires_at",
		"next_attempt_at", "attempt_count", "last_error_code", "started_workflow_id", "started_at", "projected_at",
	})
	for _, trigger := range triggers {
		labelsJSON, _ := json.Marshal(trigger.RequiredLabels)
		var claimExpiresAt any
		var startedAt any
		if trigger.ClaimExpiresAt != nil {
			claimExpiresAt = *trigger.ClaimExpiresAt
		}
		if trigger.StartedAt != nil {
			startedAt = *trigger.StartedAt
		}
		rows.AddRow(
			trigger.SourceSequence, trigger.EventID, trigger.EventType, trigger.EventCreatedAt,
			trigger.IntentID, trigger.IntentExpiresAt, trigger.AccountID, trigger.DesiredGeneration,
			trigger.SlotID, trigger.Provider, labelsJSON, trigger.ImageDigest,
			trigger.CPURequestMillis, trigger.MemoryRequestBytes, trigger.ReservationID,
			trigger.BindingRevision, trigger.Status, trigger.ClaimOwner, trigger.ClaimVersion,
			claimExpiresAt, trigger.NextAttemptAt, trigger.AttemptCount, trigger.LastErrorCode,
			trigger.StartedWorkflowID, startedAt, trigger.ProjectedAt,
		)
	}
	return rows
}

func emptySlotRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"slot_id", "account_id", "provider", "desired_state", "desired_generation", "next_execution_epoch",
		"required_labels_json", "image_digest", "cpu_request_millis", "memory_request_bytes", "created_at", "updated_at",
	})
}
