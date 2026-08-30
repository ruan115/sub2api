package store

import (
	"context"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

func TestMySQLRepositoryDoesNotExposeUncheckedWorkflowCreation(t *testing.T) {
	type uncheckedWorkflowCreator interface {
		CreateProvisioning(context.Context, onboarding.Provisioning) (onboarding.Provisioning, bool, error)
	}
	if _, exposed := any(&Repository{}).(uncheckedWorkflowCreator); exposed {
		t.Fatal("durable repository exposes workflow creation outside the atomic healthy-slot starter")
	}
}

func TestObserveOnboardingWorkflowProcessKeyPersistsOnlyPublicMaterial(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	pending := testOnboardingWorkflow()
	recipient, err := credential.NewRecipient(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	observedAt := pending.CreatedAt.Add(time.Second)
	observation := onboarding.ProvisioningCommandObservation{
		CommandID: pending.KeyCommandID, NodeID: pending.NodeID, SlotID: pending.SlotID,
		ExecutionEpoch: pending.ExecutionEpoch, ImageDigest: pending.ImageDigest,
		Healthy: true, Succeeded: true, KeyID: keyID, KeyPublicKey: publicKey, ReceivedAt: observedAt,
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE key_command_id = \? OR activation_command_id = \? FOR UPDATE`).
		WithArgs(observation.CommandID, observation.CommandID).
		WillReturnRows(onboardingWorkflowRows(pending))
	mock.ExpectExec(`(?s)UPDATE onboarding_workflows SET.*status = \?.*key_public_key = \?.*WHERE workflow_id = \?`).
		WithArgs(onboarding.ProvisioningKeyReady, keyID, publicKey, "", observation.CommandID, observedAt, pending.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	stored, err := repository.ObserveProvisioningCommand(context.Background(), observation)
	if err != nil || stored.Status != onboarding.ProvisioningKeyReady || stored.KeyID != keyID {
		t.Fatalf("observe onboarding key = %+v/%v", stored, err)
	}
	stored.Destroy()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListActiveOnboardingWorkflowIDsIsBoundedAndOrdered(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	mock.ExpectQuery(`(?s)SELECT workflow_id.*status NOT IN \('completed', 'failed'\).*ORDER BY updated_at, workflow_id.*LIMIT \?`).
		WithArgs(100).
		WillReturnRows(sqlmock.NewRows([]string{"workflow_id"}).AddRow("workflow-1").AddRow("workflow-2"))
	ids, err := repository.ListActiveProvisioningIDs(context.Background(), 100)
	if err != nil || len(ids) != 2 || ids[0] != "workflow-1" || ids[1] != "workflow-2" {
		t.Fatalf("ListActiveProvisioningIDs() = %v, %v", ids, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeferOnboardingWorkflowRetryMovesOnlyActiveWork(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	retryAt := time.Unix(2_000_000_100, 0).UTC()
	mock.ExpectExec(`(?s)UPDATE onboarding_workflows.*GREATEST\(updated_at, \?\).*status NOT IN \('completed', 'failed'\)`).
		WithArgs(retryAt, "workflow-1").WillReturnResult(sqlmock.NewResult(0, 1))
	if err := repository.DeferProvisioningRetry(context.Background(), "workflow-1", retryAt); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func testOnboardingWorkflow() onboarding.Provisioning {
	now := time.Unix(2_000_000_000, 0).UTC()
	return onboarding.Provisioning{
		ID: "workflow-10380", IdempotencyKey: "event-10380", IntentID: "11111111-2222-4333-8444-555555555555",
		Owner: "workflow-10380", AccountID: "account-10380", DesiredGeneration: 7, NodeID: "srv74",
		SlotID: "slot-10380", ExecutionEpoch: 19, ImageDigest: "sha256:" + strings.Repeat("a", 64),
		CredentialLeaseID: "lease-10380", ProxyLeaseID: "proxy-10380", KeyCommandID: "cmd-key-10380",
		ActivationCommandID: "cmd-activate-10380", CommandDeadline: now.Add(2 * time.Minute),
		Status: onboarding.ProvisioningPendingKey, CreatedAt: now, UpdatedAt: now,
	}
}

func onboardingWorkflowRows(record onboarding.Provisioning) *sqlmock.Rows {
	var publicKey any
	if len(record.KeyPublicKey) > 0 {
		publicKey = record.KeyPublicKey
	}
	return sqlmock.NewRows([]string{
		"workflow_id", "idempotency_key", "intent_id", "claim_owner", "account_id", "desired_generation",
		"node_id", "slot_id", "execution_epoch", "image_digest", "credential_lease_id", "proxy_lease_id",
		"key_command_id", "activation_command_id", "command_deadline", "status", "key_id", "key_public_key",
		"error_code", "last_command_id", "created_at", "updated_at",
	}).AddRow(
		record.ID, record.IdempotencyKey, record.IntentID, record.Owner, record.AccountID, record.DesiredGeneration,
		record.NodeID, record.SlotID, record.ExecutionEpoch, record.ImageDigest, record.CredentialLeaseID, record.ProxyLeaseID,
		record.KeyCommandID, record.ActivationCommandID, record.CommandDeadline, record.Status, record.KeyID, publicKey,
		record.ErrorCode, record.LastCommandID, record.CreatedAt, record.UpdatedAt,
	)
}
