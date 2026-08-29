package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	_ "github.com/go-sql-driver/mysql"
)

func TestMySQLRepositoryIntegration(t *testing.T) {
	dsn := os.Getenv("EXECUTION_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set EXECUTION_MYSQL_TEST_DSN to run the MySQL repository integration test")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	repository, err := NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	suffix := integrationID(t)
	nodeID := "mysql-node-" + suffix[:12]
	slotID := "mysql-slot-" + suffix[:12]
	accountID := "account-" + suffix[:12]
	enrollmentID := integrationID(t)
	assignmentID := integrationID(t)
	jobID := integrationID(t)
	leaseID := integrationID(t)
	digest := sha256.Sum256([]byte("enrollment-" + suffix))
	certificateDigest := sha256.Sum256([]byte("certificate-" + suffix))
	publicKeyDigest := sha256.Sum256([]byte("public-key-" + suffix))

	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		for _, statement := range []struct {
			query string
			arg   string
		}{
			{"DELETE FROM node_command_results WHERE node_id = ?", nodeID},
			{"DELETE FROM credential_security_events WHERE account_id = ?", accountID},
			{"DELETE FROM credential_leases WHERE account_id = ?", accountID},
			{"DELETE FROM credential_versions WHERE account_id = ?", accountID},
			{"DELETE FROM credential_vault WHERE account_id = ?", accountID},
			{"DELETE FROM execution_leases WHERE slot_id = ?", slotID},
			{"DELETE FROM provisioning_jobs WHERE slot_id = ?", slotID},
			{"DELETE FROM slot_assignments WHERE slot_id = ?", slotID},
			{"DELETE FROM slots WHERE slot_id = ?", slotID},
			{"DELETE FROM node_certificates WHERE node_id = ?", nodeID},
			{"DELETE FROM nodes WHERE node_id = ?", nodeID},
			{"DELETE FROM node_enrollments WHERE enrollment_id = ?", enrollmentID},
		} {
			if _, cleanupErr := db.ExecContext(cleanupCtx, statement.query, statement.arg); cleanupErr != nil {
				t.Errorf("cleanup integration record: %v", cleanupErr)
			}
		}
	})

	if err := repository.CreateEnrollment(ctx, Enrollment{
		ID: enrollmentID, TokenSHA256: digest, ExpectedNodeID: nodeID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	node := Node{
		ID: nodeID, Status: "active", Labels: map[string]string{"failure_domain": "local-a"},
		Capabilities: []string{"docker", "image.sha256." + integrationHexDigest()}, ProtocolMajor: 1,
		Capacity: Capacity{
			MaxSlots: 4, MaxActiveCLI: 2, MaxActiveAPI: 4, MaxActiveTotal: 4,
			AllocatableCPUMillis: 4_000, AllocatableMemoryBytes: 8 << 30,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	certificate := Certificate{
		SerialNumber: integrationID(t), NodeID: nodeID, CertificateSHA256: certificateDigest,
		PublicKeySHA256: publicKeyDigest, Status: "active", NotBefore: now.Add(-time.Minute),
		ExpiresAt: now.Add(24 * time.Hour), CreatedAt: now,
	}
	if err := repository.CommitEnrollment(ctx, digest, node, certificate, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.AcceptHello(ctx, Hello{
		NodeID: nodeID, SessionID: "session-1", Labels: node.Labels, Capabilities: node.Capabilities,
		ProtocolMajor: 1, Capacity: node.Capacity, ReceivedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.RecordHeartbeat(ctx, Heartbeat{
		NodeID: nodeID, SessionID: "session-1", ReceivedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	imageDigest := "sha256:" + integrationHexDigest()
	slot, err := repository.PutDesiredSlot(ctx, Slot{
		ID: slotID, AccountID: accountID, Provider: "docker", DesiredState: "ready",
		DesiredGeneration: 1, RequiredLabels: map[string]string{"failure_domain": "local-a"},
		ImageDigest: imageDigest, CPURequestMillis: 500, MemoryRequestBytes: 1 << 30,
		CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := repository.ReserveAssignment(ctx, AssignmentReservation{
		ID: assignmentID, SlotID: slot.ID, NodeID: nodeID, ExpectedNodeSessionID: "session-1",
		NodeSeenAfter: now, ReservedAt: now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if assignment.ExecutionEpoch != 1 {
		t.Fatalf("first execution epoch = %d, want 1", assignment.ExecutionEpoch)
	}
	if _, err := repository.ReserveAssignment(ctx, AssignmentReservation{
		ID: integrationID(t), SlotID: slot.ID, NodeID: nodeID, ExpectedNodeSessionID: "session-1",
		NodeSeenAfter: now, ReservedAt: now.Add(5 * time.Second),
	}); !errors.Is(err, ErrAssignmentConflict) {
		t.Fatalf("duplicate active assignment error = %v", err)
	}

	job, claimed, err := repository.ClaimProvisioningJob(ctx, ProvisioningJob{
		ID: jobID, SlotID: slot.ID, IdempotencyKey: "create/" + slot.ID + "/1/1",
		DesiredGeneration: 1, Step: "create",
	}, now.Add(5*time.Second), time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim provisioning job: claimed=%v err=%v", claimed, err)
	}
	if err := repository.MarkProvisioningJobDispatched(ctx, job.ID, now.Add(6*time.Second)); err != nil {
		t.Fatal(err)
	}
	observation := AssignmentObservation{
		SlotID: slot.ID, ExecutionEpoch: assignment.ExecutionEpoch, ProviderRef: "container-local",
		ActualState: "created", ReasonCode: "created", ObservedAt: now.Add(7 * time.Second),
	}
	if err := repository.ApplyCommandResult(ctx, CommandResult{
		CommandID: job.ID, NodeID: nodeID, Succeeded: true, SlotObservationJSON: []byte(`{"actual_state":"created"}`),
		Observation: &observation, ReceivedAt: now.Add(7 * time.Second), RetryAt: now.Add(8 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	observed, err := repository.GetActiveAssignment(ctx, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.ActualState != "created" || observed.ActualGeneration != 2 {
		t.Fatalf("unexpected observed assignment: %+v", observed)
	}

	lease, err := repository.GrantExecutionLease(ctx, ExecutionLease{
		ID: leaseID, SlotID: slot.ID, NodeID: nodeID, ExecutionEpoch: assignment.ExecutionEpoch,
		OwnerID: "host-agent-1", ExpiresAt: now.Add(45 * time.Second), CreatedAt: now.Add(8 * time.Second),
		UpdatedAt: now.Add(8 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.ExecutionEpoch != 1 {
		t.Fatalf("lease execution epoch = %d, want 1", lease.ExecutionEpoch)
	}
	fakeKMS, err := credential.NewFakeKMS(bytes.Repeat([]byte{0x52}, 32), "kms-integration", "v1")
	if err != nil {
		t.Fatal(err)
	}
	cryptoService, err := credential.NewService(fakeKMS)
	if err != nil {
		t.Fatal(err)
	}
	vault, err := credential.NewVault(cryptoService, repository, credential.VaultConfig{
		LeaseTTL: 30 * time.Second,
		Now:      func() time.Time { return now.Add(9 * time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := vault.Rotate(ctx, accountID, "oauth", "oauth:***test", []byte(`{"access_token":"integration-at","refresh_token":"integration-rt"}`))
	if err != nil {
		t.Fatal(err)
	}
	credentialLease, err := vault.IssueLease(ctx, accountID, slot.ID, assignment.ExecutionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	material, err := vault.RedeemLease(ctx, credentialLease.Token, accountID, slot.ID, assignment.ExecutionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if material.VersionID != version.ID || !bytes.Contains(material.Plaintext, []byte("integration-at")) {
		t.Fatalf("unexpected leased credential material: version=%s", material.VersionID)
	}
	material.Destroy()
	if _, err := vault.RedeemLease(ctx, credentialLease.Token, accountID, slot.ID, assignment.ExecutionEpoch); !errors.Is(err, credential.ErrCredentialLeaseRejected) {
		t.Fatalf("credential lease replay error = %v", err)
	}
	securityEvents, err := repository.ListCredentialSecurityEvents(ctx, accountID, 10)
	if err != nil || len(securityEvents) != 1 || securityEvents[0].ReasonCode != "replayed" {
		t.Fatalf("credential replay events = %+v, err=%v", securityEvents, err)
	}
	if err := repository.RenewExecutionLease(ctx, slot.ID, 1, "host-agent-1", now.Add(75*time.Second), now.Add(15*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := repository.RevokeExecutionLease(ctx, slot.ID, 1, "host-agent-1", now.Add(16*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ObserveAssignment(ctx, AssignmentObservation{
		SlotID: slot.ID, ExecutionEpoch: 1, ProviderRef: "container-local", ActualState: "destroyed",
		ReasonCode: "destroyed", ObservedAt: now.Add(17 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReleaseAssignment(ctx, slot.ID, 1, now.Add(18*time.Second)); err != nil {
		t.Fatal(err)
	}
	replacement, err := repository.ReserveAssignment(ctx, AssignmentReservation{
		ID: integrationID(t), SlotID: slot.ID, NodeID: nodeID, ExpectedNodeSessionID: "session-1",
		NodeSeenAfter: now, ReservedAt: now.Add(19 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if replacement.ExecutionEpoch != 2 {
		t.Fatalf("replacement execution epoch = %d, want 2", replacement.ExecutionEpoch)
	}
	if err := repository.ForceReleaseAssignment(ctx, slot.ID, 2, "integration_cleanup", now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	storedNode, err := repository.GetNode(ctx, nodeID)
	if err != nil {
		t.Fatal(err)
	}
	if storedNode.ReservedSlots != 0 || storedNode.ReservedCPUMillis != 0 || storedNode.ReservedMemoryBytes != 0 {
		t.Fatalf("node reservation leaked: %+v", storedNode)
	}
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET reserved_slots = max_slots + 1 WHERE node_id = ?`, nodeID); err == nil {
		t.Fatal("MySQL accepted a node reservation beyond max_slots")
	}
}

func integrationID(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	encoded := hex.EncodeToString(raw[:])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}

func integrationHexDigest() string {
	digest := sha256.Sum256([]byte("integration-runtime-image"))
	return hex.EncodeToString(digest[:])
}
