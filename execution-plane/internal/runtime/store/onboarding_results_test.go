package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

func TestProjectProvisioningResultPersistsOnlyBoundSafeMetadata(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	workflow := resultReadyWorkflow(t)
	commit := onboarding.ResultProjectionCommit{
		AccountBinding: provider.RuntimeAccountID(workflow.AccountID), SlotID: workflow.SlotID,
		ExecutionEpoch: workflow.ExecutionEpoch, CredentialLeaseID: workflow.CredentialLeaseID,
		ProxyLeaseID: workflow.ProxyLeaseID, CredentialVersionID: "credential-version-10380",
		Projection: onboarding.ResultProjection{
			AuthType: "oauth", EmailAddress: "owner@example.com", OrganizationID: "org-10380",
			UpstreamAccountID: "upstream-10380", Scope: "user:inference", SubscriptionType: "max", RateLimitTier: "tier-1",
		},
		CommittedAt: workflow.UpdatedAt.Add(time.Second),
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE credential_lease_id = \? FOR UPDATE`).
		WithArgs(workflow.CredentialLeaseID).WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT result.workflow_id.*FROM onboarding_results result.*JOIN onboarding_workflows.*WHERE result.workflow_id = \? FOR UPDATE`).
		WithArgs(workflow.ID).WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`(?s)INSERT INTO onboarding_results.*VALUES`).WithArgs(
		workflow.ID, workflow.IntentID, workflow.AccountID, workflow.DesiredGeneration, workflow.CredentialLeaseID,
		commit.CredentialVersionID, "oauth", "owner@example.com", "org-10380", "upstream-10380",
		"user:inference", "max", "tier-1", nil, commit.CommittedAt,
	).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	result, err := repository.ProjectProvisioningResult(context.Background(), commit)
	if err != nil || result.Projection.EmailAddress != "owner@example.com" || result.CredentialVersionID != commit.CredentialVersionID {
		t.Fatalf("project provisioning result = %+v, %v", result, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetProvisioningResultRequiresCompletedWorkflow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	workflow := resultReadyWorkflow(t)
	workflow.IntentID = "intent-10380"
	workflow.Status = onboarding.ProvisioningCompleted
	workflow.LastCommandID = workflow.ActivationCommandID
	workflow.UpdatedAt = now.Add(time.Minute)
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows.*WHERE intent_id = \?.*ORDER BY created_at DESC`).
		WithArgs("intent-10380", "account-10380", uint64(7)).
		WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT result.workflow_id.*FROM onboarding_results result.*JOIN onboarding_workflows.*WHERE result.workflow_id = \?`).
		WithArgs(workflow.ID).
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_id", "intent_id", "account_id", "desired_generation", "slot_id", "execution_epoch", "credential_lease_id",
			"credential_version_id", "auth_type", "email_address", "organization_id", "upstream_account_id",
			"scope_text", "subscription_type", "rate_limit_tier", "expires_at", "created_at",
		}).AddRow(
			"workflow-10380", "intent-10380", "account-10380", 7, "slot-10380", 19, "lease-10380",
			"credential-version-10380", "oauth", "owner@example.com", "org-10380", "upstream-10380",
			"user:inference", "max", "tier-1", nil, now,
		))
	outcome, err := repository.GetProvisioningResult(context.Background(), "intent-10380", "account-10380", 7)
	if err != nil || outcome.Validate() != nil || outcome.Status != onboarding.ProvisioningOutcomeSucceeded ||
		outcome.Result == nil || outcome.Result.Projection.EmailAddress != "owner@example.com" ||
		outcome.SlotID != "slot-10380" || outcome.ExecutionEpoch != 19 || !outcome.FinishedAt.Equal(workflow.UpdatedAt) {
		t.Fatalf("get provisioning result = %+v, %v", outcome, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetProvisioningResultClassifiesPendingStorageAndCorruptRows(t *testing.T) {
	t.Parallel()
	request := func(t *testing.T, prepare func(sqlmock.Sqlmock)) (onboarding.ProvisioningOutcome, error) {
		t.Helper()
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		prepare(mock)
		repository, _ := NewRepository(db)
		outcome, lookupErr := repository.GetProvisioningResult(context.Background(), "intent-10380", "account-10380", 7)
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		return outcome, lookupErr
	}
	latestWorkflow := func(mock sqlmock.Sqlmock) *sqlmock.ExpectedQuery {
		return mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows.*WHERE intent_id = \?.*ORDER BY created_at DESC`).
			WithArgs("intent-10380", "account-10380", uint64(7))
	}
	intentState := func(mock sqlmock.Sqlmock) *sqlmock.ExpectedQuery {
		return mock.ExpectQuery(`(?s)SELECT status, expires_at, expires_at <= UTC_TIMESTAMP\(6\).*FROM onboarding_intents`).
			WithArgs("intent-10380", "account-10380", uint64(7))
	}
	now := time.Unix(2_000_000_000, 0).UTC()

	t.Run("pending", func(t *testing.T) {
		_, err := request(t, func(mock sqlmock.Sqlmock) {
			latestWorkflow(mock).WillReturnError(sql.ErrNoRows)
			intentState(mock).WillReturnRows(sqlmock.NewRows([]string{"status", "expires_at", "expired"}).
				AddRow(onboarding.IntentPending, now.Add(time.Hour), false))
		})
		if !errors.Is(err, onboarding.ErrResultPending) {
			t.Fatalf("pending lookup error = %v", err)
		}
	})
	t.Run("storage", func(t *testing.T) {
		storageErr := errors.New("database unavailable")
		_, err := request(t, func(mock sqlmock.Sqlmock) {
			latestWorkflow(mock).WillReturnError(storageErr)
		})
		if !errors.Is(err, storageErr) || errors.Is(err, onboarding.ErrResultPending) {
			t.Fatalf("storage lookup error = %v", err)
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		workflow := testOnboardingWorkflow()
		workflow.IntentID = "intent-10380"
		workflow.SlotID = ""
		_, err := request(t, func(mock sqlmock.Sqlmock) {
			latestWorkflow(mock).WillReturnRows(onboardingWorkflowRows(workflow))
		})
		if !errors.Is(err, onboarding.ErrResultProjectionRejected) || errors.Is(err, onboarding.ErrResultPending) {
			t.Fatalf("corrupt lookup error = %v", err)
		}
	})
	t.Run("active workflow remains pending", func(t *testing.T) {
		workflow := testOnboardingWorkflow()
		workflow.IntentID = "intent-10380"
		_, err := request(t, func(mock sqlmock.Sqlmock) {
			latestWorkflow(mock).WillReturnRows(onboardingWorkflowRows(workflow))
			intentState(mock).WillReturnRows(sqlmock.NewRows([]string{"status", "expires_at", "expired"}).
				AddRow(onboarding.IntentClaimed, now.Add(time.Hour), false))
		})
		if !errors.Is(err, onboarding.ErrResultPending) {
			t.Fatalf("active workflow lookup error = %v", err)
		}
	})
}

func TestGetProvisioningResultReturnsSafeTerminalFailedAndExpiredOutcomes(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000_000_000, 0).UTC()
	tests := []struct {
		name         string
		internalCode string
		status       string
		publicCode   string
		publicText   string
	}{
		{
			name: "failed workflow", internalCode: "worker_key_failed", status: onboarding.ProvisioningOutcomeFailed,
			publicCode: onboarding.ProvisioningErrorFailed, publicText: onboarding.ProvisioningSummaryFailed,
		},
		{
			name: "expired workflow", internalCode: "workflow_deadline_exceeded", status: onboarding.ProvisioningOutcomeExpired,
			publicCode: onboarding.ProvisioningErrorExpired, publicText: onboarding.ProvisioningSummaryExpired,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			workflow := testOnboardingWorkflow()
			workflow.IntentID = "intent-10380"
			workflow.Status = onboarding.ProvisioningFailed
			workflow.ErrorCode = test.internalCode
			workflow.LastCommandID = workflow.KeyCommandID
			workflow.UpdatedAt = now.Add(time.Minute)
			mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows.*WHERE intent_id = \?.*ORDER BY created_at DESC`).
				WithArgs("intent-10380", "account-10380", uint64(7)).
				WillReturnRows(onboardingWorkflowRows(workflow))
			repository, _ := NewRepository(db)
			outcome, err := repository.GetProvisioningResult(context.Background(), "intent-10380", "account-10380", 7)
			if err != nil || outcome.Validate() != nil || outcome.Status != test.status || outcome.Result != nil ||
				outcome.ErrorCode != test.publicCode || outcome.ErrorSummary != test.publicText ||
				!outcome.FinishedAt.Equal(workflow.UpdatedAt) {
				t.Fatalf("terminal workflow outcome = %+v, %v", outcome, err)
			}
			if strings.Contains(outcome.String(), test.internalCode) {
				t.Fatal("terminal outcome exposed internal failure code")
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("expired intent without workflow", func(t *testing.T) {
		t.Parallel()
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		expiresAt := now.Add(-time.Minute)
		mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows.*WHERE intent_id = \?.*ORDER BY created_at DESC`).
			WithArgs("intent-10380", "account-10380", uint64(7)).WillReturnError(sql.ErrNoRows)
		mock.ExpectQuery(`(?s)SELECT status, expires_at, expires_at <= UTC_TIMESTAMP\(6\).*FROM onboarding_intents`).
			WithArgs("intent-10380", "account-10380", uint64(7)).
			WillReturnRows(sqlmock.NewRows([]string{"status", "expires_at", "expired"}).
				AddRow(onboarding.IntentPending, expiresAt, true))
		repository, _ := NewRepository(db)
		outcome, err := repository.GetProvisioningResult(context.Background(), "intent-10380", "account-10380", 7)
		if err != nil || outcome.Validate() != nil || outcome.Status != onboarding.ProvisioningOutcomeExpired ||
			outcome.WorkflowID != "" || outcome.SlotID != "" || outcome.ExecutionEpoch != 0 ||
			!outcome.FinishedAt.Equal(expiresAt) {
			t.Fatalf("expired intent outcome = %+v, %v", outcome, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func resultReadyWorkflow(t *testing.T) onboarding.Provisioning {
	t.Helper()
	workflow := testOnboardingWorkflow()
	recipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	workflow.KeyID, workflow.KeyPublicKey, err = recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	workflow.Status = onboarding.ProvisioningKeyReady
	workflow.LastCommandID = workflow.KeyCommandID
	workflow.UpdatedAt = workflow.CreatedAt.Add(time.Second)
	return workflow
}
