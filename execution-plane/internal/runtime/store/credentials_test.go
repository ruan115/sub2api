package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

func TestCommitCredentialVersionSwitchesActiveAndRevokesOldLeasesAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	version := testCredentialVersion(t, now)

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO credential_vault.*ON DUPLICATE KEY UPDATE`).
		WithArgs(version.AccountID, version.AuthType, now, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT active_version_id, auth_type FROM credential_vault.*FOR UPDATE`).
		WithArgs(version.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"active_version_id", "auth_type"}).AddRow(nil, version.AuthType))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version_number\), 0\) FROM credential_versions`).
		WithArgs(version.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"version_number"}).AddRow(0))
	mock.ExpectExec(`(?s)INSERT INTO credential_versions`).
		WithArgs(
			version.ID, version.AccountID, version.VersionNumber, version.Envelope.Ciphertext,
			version.Envelope.EncryptedDEK, version.Envelope.Nonce, version.Envelope.AADJSON,
			version.Envelope.KMSKeyID, version.Envelope.KMSKeyVersion, version.Hint, now,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)UPDATE credential_vault SET active_version_id`).
		WithArgs(version.ID, version.AuthType, now, version.AccountID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE credential_leases SET revoked_at`).
		WithArgs(now, version.AccountID, version.ID).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	if err := repository.CommitCredentialVersion(context.Background(), version); err != nil {
		t.Fatalf("commit credential version: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitCredentialVersionRollsBackBeforeActiveSwitchOnInsertFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	version := testCredentialVersion(t, now)

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO credential_vault`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT active_version_id, auth_type FROM credential_vault.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"active_version_id", "auth_type"}).AddRow("old-version", version.AuthType))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version_number\), 0\) FROM credential_versions`).
		WillReturnRows(sqlmock.NewRows([]string{"version_number"}).AddRow(0))
	mock.ExpectExec(`(?s)INSERT INTO credential_versions`).WillReturnError(errors.New("database write failed"))
	mock.ExpectRollback()

	if err := repository.CommitCredentialVersion(context.Background(), version); err == nil {
		t.Fatal("credential version insert failure was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitCredentialVersionForOperationBindsVersionInSameTransaction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	version := testCredentialVersion(t, now)
	operationID := "credential-lease-10380"

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO credential_vault.*ON DUPLICATE KEY UPDATE`).
		WithArgs(version.AccountID, version.AuthType, now, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT active_version_id, auth_type FROM credential_vault.*FOR UPDATE`).
		WithArgs(version.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"active_version_id", "auth_type"}).AddRow(nil, version.AuthType))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version_number\), 0\) FROM credential_versions`).
		WithArgs(version.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"version_number"}).AddRow(0))
	mock.ExpectExec(`(?s)INSERT INTO credential_versions`).
		WithArgs(
			version.ID, version.AccountID, version.VersionNumber, version.Envelope.Ciphertext,
			version.Envelope.EncryptedDEK, version.Envelope.Nonce, version.Envelope.AADJSON,
			version.Envelope.KMSKeyID, version.Envelope.KMSKeyVersion, version.Hint, now,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)INSERT INTO credential_version_operations`).
		WithArgs(operationID, version.ID, version.AccountID, version.AuthType, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)UPDATE credential_vault SET active_version_id`).
		WithArgs(version.ID, version.AuthType, now, version.AccountID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE credential_leases SET revoked_at`).
		WithArgs(now, version.AccountID, version.ID).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if err := repository.CommitCredentialVersionForOperation(context.Background(), operationID, version); err != nil {
		t.Fatalf("commit credential operation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitCredentialVersionForOperationRollsBackWhenBindingFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	version := testCredentialVersion(t, now)

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO credential_vault`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT active_version_id, auth_type FROM credential_vault.*FOR UPDATE`).
		WillReturnRows(sqlmock.NewRows([]string{"active_version_id", "auth_type"}).AddRow(nil, version.AuthType))
	mock.ExpectQuery(`SELECT COALESCE\(MAX\(version_number\), 0\) FROM credential_versions`).
		WillReturnRows(sqlmock.NewRows([]string{"version_number"}).AddRow(0))
	mock.ExpectExec(`(?s)INSERT INTO credential_versions`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)INSERT INTO credential_version_operations`).WillReturnError(errors.New("operation identity already bound"))
	mock.ExpectRollback()

	if err := repository.CommitCredentialVersionForOperation(context.Background(), "credential-lease-10380", version); err == nil {
		t.Fatal("operation mapping failure was accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetCredentialVersionByOperationReturnsBoundVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	version := testCredentialVersion(t, now)
	operationID := "credential-lease-10380"
	columns := []string{
		"version_id", "account_id", "version_number", "auth_type", "ciphertext", "encrypted_dek",
		"nonce", "aad_json", "kms_key_id", "kms_key_version", "credential_hint", "created_at",
	}
	mock.ExpectQuery(`(?s)FROM credential_version_operations cvo.*WHERE cvo.operation_id = \?`).
		WithArgs(operationID).
		WillReturnRows(sqlmock.NewRows(columns).AddRow(
			version.ID, version.AccountID, version.VersionNumber, version.AuthType,
			version.Envelope.Ciphertext, version.Envelope.EncryptedDEK, version.Envelope.Nonce,
			version.Envelope.AADJSON, version.Envelope.KMSKeyID, version.Envelope.KMSKeyVersion,
			version.Hint, version.CreatedAt,
		))

	got, err := repository.GetCredentialVersionByOperation(context.Background(), operationID)
	if err != nil || got.ID != version.ID || got.AuthType != version.AuthType {
		t.Fatalf("get credential operation = %+v, err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestIssueCredentialLeaseStoresOnlyDigestAndCurrentBinding(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	digest := sha256.Sum256([]byte("clt_token-that-never-reaches-the-database"))
	candidate := credential.LeaseRecord{
		ID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", TokenSHA256: digest,
		AccountID: "account-1", SlotID: "slot-1", ExecutionEpoch: 7,
		ExpiresAt: now.Add(30 * time.Second), CreatedAt: now,
	}
	versionID := "11111111-2222-4333-8444-555555555555"

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT active_version_id FROM credential_vault.*FOR UPDATE`).
		WithArgs(candidate.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"active_version_id"}).AddRow(versionID))
	mock.ExpectQuery(`(?s)SELECT sa.node_id.*FROM slots.*execution_leases.*FOR UPDATE`).
		WithArgs(candidate.SlotID, candidate.AccountID, candidate.ExecutionEpoch, now).
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}).AddRow("srv74"))
	mock.ExpectExec(`(?s)INSERT INTO credential_leases`).
		WithArgs(candidate.ID, digest[:], candidate.AccountID, versionID, candidate.SlotID, candidate.ExecutionEpoch, candidate.ExpiresAt, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	issued, err := repository.IssueCredentialLease(context.Background(), candidate)
	if err != nil {
		t.Fatalf("issue credential lease: %v", err)
	}
	if issued.VersionID != versionID {
		t.Fatalf("issued version id = %s, want %s", issued.VersionID, versionID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestConsumeCredentialLeaseIsAtomicAndReplayCreatesSecurityEvent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	version := testCredentialVersion(t, now.Add(-time.Minute))
	digest := sha256.Sum256([]byte("one-time-credential-token"))
	lease := credential.LeaseRecord{
		ID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", TokenSHA256: digest,
		AccountID: version.AccountID, VersionID: version.ID, SlotID: "slot-1", ExecutionEpoch: 7,
		ExpiresAt: now.Add(30 * time.Second), CreatedAt: now.Add(-time.Second),
	}
	claim := credential.LeaseClaim{
		TokenSHA256: digest, AccountID: lease.AccountID, SlotID: lease.SlotID, ExecutionEpoch: lease.ExecutionEpoch,
		SecurityEventID: "99999999-8888-4777-8666-555555555555", ConsumedAt: now,
	}
	leaseColumns := []string{"lease_id", "token_sha256", "account_id", "version_id", "slot_id", "execution_epoch", "expires_at", "consumed_at", "revoked_at", "created_at"}
	versionColumns := []string{"version_id", "account_id", "version_number", "auth_type", "ciphertext", "encrypted_dek", "nonce", "aad_json", "kms_key_id", "kms_key_version", "credential_hint", "created_at"}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT active_version_id FROM credential_vault.*FOR UPDATE`).
		WithArgs(claim.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"active_version_id"}).AddRow(lease.VersionID))
	mock.ExpectQuery(`(?s)SELECT lease_id, token_sha256.*credential_leases.*FOR UPDATE`).
		WithArgs(digest[:]).
		WillReturnRows(sqlmock.NewRows(leaseColumns).AddRow(
			lease.ID, digest[:], lease.AccountID, lease.VersionID, lease.SlotID, lease.ExecutionEpoch,
			lease.ExpiresAt, nil, nil, lease.CreatedAt,
		))
	mock.ExpectQuery(`(?s)SELECT sa.node_id.*slot_assignments.*execution_leases.*FOR UPDATE`).
		WithArgs(lease.SlotID, lease.AccountID, lease.ExecutionEpoch, now).
		WillReturnRows(sqlmock.NewRows([]string{"node_id"}).AddRow("srv74"))
	mock.ExpectExec(`(?s)UPDATE credential_leases SET consumed_at`).
		WithArgs(now, lease.ID, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`(?s)SELECT v.version_id.*FROM credential_versions`).
		WithArgs(lease.VersionID).
		WillReturnRows(sqlmock.NewRows(versionColumns).AddRow(
			version.ID, version.AccountID, version.VersionNumber, version.AuthType,
			version.Envelope.Ciphertext, version.Envelope.EncryptedDEK, version.Envelope.Nonce,
			version.Envelope.AADJSON, version.Envelope.KMSKeyID, version.Envelope.KMSKeyVersion,
			version.Hint, version.CreatedAt,
		))
	mock.ExpectCommit()

	consumedVersion, err := repository.ConsumeCredentialLease(context.Background(), claim)
	if err != nil || consumedVersion.ID != version.ID {
		t.Fatalf("consume credential lease: version=%+v err=%v", consumedVersion, err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(`(?s)SELECT active_version_id FROM credential_vault.*FOR UPDATE`).
		WithArgs(claim.AccountID).
		WillReturnRows(sqlmock.NewRows([]string{"active_version_id"}).AddRow(lease.VersionID))
	mock.ExpectQuery(`(?s)SELECT lease_id, token_sha256.*credential_leases.*FOR UPDATE`).
		WithArgs(digest[:]).
		WillReturnRows(sqlmock.NewRows(leaseColumns).AddRow(
			lease.ID, digest[:], lease.AccountID, lease.VersionID, lease.SlotID, lease.ExecutionEpoch,
			lease.ExpiresAt, now, nil, lease.CreatedAt,
		))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO credential_security_events")+`(?s:.*credential_lease_rejected.*)`).
		WithArgs(claim.SecurityEventID, "replayed", lease.AccountID, lease.SlotID, lease.ExecutionEpoch, lease.ID, now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if _, err := repository.ConsumeCredentialLease(context.Background(), claim); !errors.Is(err, credential.ErrCredentialLeaseRejected) {
		t.Fatalf("replay error = %v, want ErrCredentialLeaseRejected", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func testCredentialVersion(t *testing.T, now time.Time) credential.VersionRecord {
	t.Helper()
	kms, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x19}, 32), "kms-test", "v1")
	if err != nil {
		t.Fatal(err)
	}
	service, err := credential.NewService(kms)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := service.Seal(context.Background(), credential.Metadata{
		AccountID: "account-1", VersionNumber: 1, AuthType: "oauth",
	}, []byte("test-credential-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return credential.VersionRecord{
		ID: "11111111-2222-4333-8444-555555555555", AccountID: "account-1",
		VersionNumber: 1, AuthType: "oauth", Envelope: envelope, Hint: "oauth:***cret", CreatedAt: now.UTC(),
	}
}
