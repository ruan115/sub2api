package store

import (
	"context"
	"crypto/sha256"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestCommitEnrollmentConsumesDigestAndCommitsNodeCertificateAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	digest := sha256.Sum256([]byte("one-time-secret-that-must-not-reach-sql"))
	node := testNode(now)
	certificate := testCertificate(now)

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE node_enrollments.*token_sha256 = \?.*consumed_at IS NULL`).
		WithArgs(now, node.ID, digest[:], now, node.ID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)INSERT INTO nodes`).
		WithArgs(
			node.ID, sqlmock.AnyArg(), sqlmock.AnyArg(), node.ProtocolMajor, node.ProtocolMinor,
			node.Capacity.MaxSlots, node.Capacity.MaxActiveCLI, node.Capacity.MaxActiveAPI, node.Capacity.MaxActiveTotal,
			node.Capacity.AllocatableCPUMillis, node.Capacity.AllocatableMemoryBytes,
			node.CreatedAt, node.UpdatedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`(?s)INSERT INTO node_certificates`).
		WithArgs(
			certificate.SerialNumber, certificate.NodeID, certificate.CertificateSHA256[:], certificate.PublicKeySHA256[:],
			certificate.Status, certificate.NotBefore, certificate.ExpiresAt, certificate.CreatedAt,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := repository.CommitEnrollment(context.Background(), digest, node, certificate, now); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitEnrollmentRollsBackWhenTokenWasConsumed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	digest := HashToken("already-used")

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)UPDATE node_enrollments`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	if err := repository.CommitEnrollment(context.Background(), digest, testNode(now), testCertificate(now), now); err != ErrEnrollmentRejected {
		t.Fatalf("expected enrollment rejection, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRotateCertificateIsTransactional(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	replacement := testCertificate(now)
	replacement.SerialNumber = "02"

	mock.ExpectBegin()
	mock.ExpectExec(`(?s)INSERT INTO node_certificates`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE node_certificates")+`(?s:.*)status = 'active'`).
		WithArgs(now, replacement.SerialNumber, "01", replacement.NodeID, now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := repository.RotateCertificate(context.Background(), replacement.NodeID, "01", replacement, now); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidateCertificateRequiresActiveUnexpiredRecord(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()

	mock.ExpectQuery(`(?s)SELECT 1.*node_certificates.*status = 'active'.*expires_at > \?`).
		WithArgs("srv74", "01", now).
		WillReturnRows(sqlmock.NewRows([]string{"active"}).AddRow(1))
	if err := repository.ValidateCertificate(context.Background(), "srv74", "01", now); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestMarkDisconnectedIsFencedByControlSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repository, _ := NewRepository(db)
	now := time.Unix(2_000_000_000, 0).UTC()
	mock.ExpectExec(`(?s)UPDATE nodes SET status = 'disconnected'.*control_session_id = NULL.*WHERE node_id = \? AND control_session_id = \?`).
		WithArgs(now, now, "srv74", "session-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repository.MarkDisconnected(context.Background(), "srv74", "session-1", now); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func testNode(now time.Time) Node {
	return Node{
		ID: "srv74", Status: "active", Labels: map[string]string{"region": "ap-shanghai"},
		Capabilities: []string{"docker"}, ProtocolMajor: 1,
		Capacity: Capacity{
			MaxSlots: 20, MaxActiveCLI: 4, MaxActiveAPI: 12, MaxActiveTotal: 12,
			AllocatableCPUMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
		},
		CreatedAt: now, UpdatedAt: now,
	}
}

func testCertificate(now time.Time) Certificate {
	return Certificate{
		SerialNumber: "01", NodeID: "srv74", Status: "active",
		CertificateSHA256: sha256.Sum256([]byte("certificate")), PublicKeySHA256: sha256.Sum256([]byte("public-key")),
		NotBefore: now.Add(-time.Minute), ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
	}
}
