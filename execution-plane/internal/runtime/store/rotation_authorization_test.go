package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

func TestBeginAndCompleteCredentialRotationAreDurablyFenced(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	workflow := rotationReadyWorkflow(t, onboarding.ProvisioningActivationDispatched)
	authorizedAt := workflow.CreatedAt.Add(time.Second)
	digest := sha256.Sum256([]byte("authenticated-canonical-worker-frame"))
	accountBinding := provider.RuntimeAccountID(workflow.AccountID)

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT workflow_id.*FROM onboarding_workflows WHERE credential_lease_id = \? FOR UPDATE`).
		WithArgs(workflow.CredentialLeaseID).
		WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT credential_lease_id, material_sha256.*FROM credential_rotation_commits.*FOR UPDATE`).
		WithArgs(workflow.CredentialLeaseID).
		WillReturnRows(rotationAuthorizationRows())
	mock.ExpectQuery(`(?s)SELECT el.lease_id.*FROM slots.*slot_assignments.*execution_leases.*s.desired_generation = \?.*sa.actual_state = 'running'.*sa.image_digest = \?.*FOR UPDATE`).
		WithArgs(
			workflow.SlotID, workflow.AccountID, workflow.DesiredGeneration, workflow.ImageDigest,
			workflow.NodeID, workflow.ExecutionEpoch, workflow.ImageDigest, authorizedAt,
		).
		WillReturnRows(sqlmock.NewRows([]string{"lease_id"}).AddRow("execution-lease-10380"))
	mock.ExpectExec(`(?s)INSERT INTO credential_rotation_commits`).
		WithArgs(
			workflow.CredentialLeaseID, digest[:], workflow.AccountID, workflow.SlotID,
			workflow.ExecutionEpoch, workflow.ProxyLeaseID, authorizedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	accountID, versionID, err := repository.BeginCredentialRotation(
		context.Background(), accountBinding, workflow.SlotID, workflow.ExecutionEpoch,
		workflow.CredentialLeaseID, workflow.ProxyLeaseID, digest, authorizedAt,
	)
	if err != nil || accountID != workflow.AccountID || versionID != "" {
		t.Fatalf("begin credential rotation = %q/%q/%v", accountID, versionID, err)
	}

	committedAt := authorizedAt.Add(time.Second)
	versionID = "11111111-2222-4333-8444-555555555555"
	pending := credentialRotationAuthorization{
		CredentialLeaseID: workflow.CredentialLeaseID, MaterialSHA256: digest, AccountID: workflow.AccountID,
		SlotID: workflow.SlotID, ExecutionEpoch: workflow.ExecutionEpoch, ProxyLeaseID: workflow.ProxyLeaseID,
		AuthorizedAt: authorizedAt,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT credential_lease_id, material_sha256.*FROM credential_rotation_commits.*FOR UPDATE`).
		WithArgs(workflow.CredentialLeaseID).
		WillReturnRows(rotationAuthorizationRows(pending))
	mock.ExpectQuery(`(?s)SELECT cvo.version_id.*FROM credential_version_operations.*FOR UPDATE`).
		WithArgs(workflow.CredentialLeaseID, workflow.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"version_id"}).AddRow(versionID))
	mock.ExpectExec(`(?s)UPDATE credential_rotation_commits.*credential_version_id`).
		WithArgs(versionID, committedAt, workflow.CredentialLeaseID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := repository.CompleteCredentialRotation(context.Background(), workflow.CredentialLeaseID, versionID, committedAt); err != nil {
		t.Fatalf("complete credential rotation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginCredentialRotationRepairsVaultCommitAfterLeaseExpiry(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	workflow := rotationReadyWorkflow(t, onboarding.ProvisioningActivationDispatched)
	digest := sha256.Sum256([]byte("authenticated-canonical-worker-frame"))
	record := credentialRotationAuthorization{
		CredentialLeaseID: workflow.CredentialLeaseID, MaterialSHA256: digest, AccountID: workflow.AccountID,
		SlotID: workflow.SlotID, ExecutionEpoch: workflow.ExecutionEpoch, ProxyLeaseID: workflow.ProxyLeaseID,
		AuthorizedAt: workflow.CreatedAt,
	}
	mappedVersionID := "11111111-2222-4333-8444-555555555555"
	recoveredAt := workflow.CommandDeadline.Add(time.Hour)
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT workflow_id.*credential_lease_id = \? FOR UPDATE`).
		WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT credential_lease_id, material_sha256.*credential_rotation_commits.*FOR UPDATE`).
		WillReturnRows(rotationAuthorizationRows(record))
	mock.ExpectQuery(`(?s)SELECT cvo.version_id.*credential_version_operations.*FOR UPDATE`).
		WithArgs(workflow.CredentialLeaseID, workflow.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"version_id"}).AddRow(mappedVersionID))
	mock.ExpectExec(`(?s)UPDATE credential_rotation_commits.*credential_version_id`).
		WithArgs(mappedVersionID, recoveredAt, workflow.CredentialLeaseID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	accountID, versionID, err := repository.BeginCredentialRotation(
		context.Background(), provider.RuntimeAccountID(workflow.AccountID), workflow.SlotID, workflow.ExecutionEpoch,
		workflow.CredentialLeaseID, workflow.ProxyLeaseID, digest, recoveredAt,
	)
	if err != nil || accountID != workflow.AccountID || versionID != mappedVersionID {
		t.Fatalf("recovered rotation = %q/%q/%v", accountID, versionID, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginCredentialRotationCommittedReplayDoesNotRequireLiveExecution(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	workflow := rotationReadyWorkflow(t, onboarding.ProvisioningCompleted)
	digest := sha256.Sum256([]byte("authenticated-canonical-worker-frame"))
	committedAt := workflow.CreatedAt.Add(time.Second)
	record := credentialRotationAuthorization{
		CredentialLeaseID: workflow.CredentialLeaseID, MaterialSHA256: digest, AccountID: workflow.AccountID,
		SlotID: workflow.SlotID, ExecutionEpoch: workflow.ExecutionEpoch, ProxyLeaseID: workflow.ProxyLeaseID,
		CredentialVersionID: "11111111-2222-4333-8444-555555555555", AuthorizedAt: workflow.CreatedAt,
		CommittedAt: &committedAt,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT workflow_id.*credential_lease_id = \? FOR UPDATE`).
		WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT credential_lease_id, material_sha256.*credential_rotation_commits.*FOR UPDATE`).
		WillReturnRows(rotationAuthorizationRows(record))
	mock.ExpectCommit()
	accountID, versionID, err := repository.BeginCredentialRotation(
		context.Background(), provider.RuntimeAccountID(workflow.AccountID), workflow.SlotID, workflow.ExecutionEpoch,
		workflow.CredentialLeaseID, workflow.ProxyLeaseID, digest, workflow.CommandDeadline.Add(time.Hour),
	)
	if err != nil || accountID != workflow.AccountID || versionID != record.CredentialVersionID {
		t.Fatalf("committed replay = %q/%q/%v", accountID, versionID, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginCredentialRotationRejectsChangedMaterialBeforeExecution(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	workflow := rotationReadyWorkflow(t, onboarding.ProvisioningActivationDispatched)
	originalDigest := sha256.Sum256([]byte("original-frame"))
	changedDigest := sha256.Sum256([]byte("changed-frame"))
	record := credentialRotationAuthorization{
		CredentialLeaseID: workflow.CredentialLeaseID, MaterialSHA256: originalDigest, AccountID: workflow.AccountID,
		SlotID: workflow.SlotID, ExecutionEpoch: workflow.ExecutionEpoch, ProxyLeaseID: workflow.ProxyLeaseID,
		AuthorizedAt: workflow.CreatedAt,
	}
	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT workflow_id.*credential_lease_id = \? FOR UPDATE`).
		WillReturnRows(onboardingWorkflowRows(workflow))
	mock.ExpectQuery(`(?s)SELECT credential_lease_id, material_sha256.*credential_rotation_commits.*FOR UPDATE`).
		WillReturnRows(rotationAuthorizationRows(record))
	mock.ExpectRollback()
	_, _, err = repository.BeginCredentialRotation(
		context.Background(), provider.RuntimeAccountID(workflow.AccountID), workflow.SlotID, workflow.ExecutionEpoch,
		workflow.CredentialLeaseID, workflow.ProxyLeaseID, changedDigest, workflow.CreatedAt.Add(time.Second),
	)
	if !errors.Is(err, ErrRotationAuthorizationRejected) {
		t.Fatalf("changed material error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func rotationAuthorizationRows(records ...credentialRotationAuthorization) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"credential_lease_id", "material_sha256", "account_id", "slot_id", "execution_epoch",
		"proxy_lease_id", "credential_version_id", "authorized_at", "committed_at",
	})
	for _, record := range records {
		var versionID any
		if record.CredentialVersionID != "" {
			versionID = record.CredentialVersionID
		}
		var committedAt any
		if record.CommittedAt != nil {
			committedAt = *record.CommittedAt
		}
		rows.AddRow(
			record.CredentialLeaseID, record.MaterialSHA256[:], record.AccountID, record.SlotID, record.ExecutionEpoch,
			record.ProxyLeaseID, versionID, record.AuthorizedAt, committedAt,
		)
	}
	return rows
}

func rotationReadyWorkflow(t *testing.T, status string) onboarding.Provisioning {
	t.Helper()
	workflow := testOnboardingWorkflow()
	recipient, err := credential.NewRecipient(bytes.NewReader(bytes.Repeat([]byte{0x43}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	defer recipient.Destroy()
	workflow.KeyID, workflow.KeyPublicKey, err = recipient.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	workflow.Status = status
	workflow.LastCommandID = workflow.ActivationCommandID
	return workflow
}
