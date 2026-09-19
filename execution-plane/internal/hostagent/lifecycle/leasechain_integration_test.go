// Package leasechain proves the execution-lease chain end to end on real
// dependencies: a lease granted through the coordinator into both stores, an
// issuance gate that consults both, custody holding a real authenticated
// transport, and reclamation when the durable record alone is revoked.
//
// It runs only when both real stores are configured, and it creates and removes
// only its own rows and keys. It never reads production data, never copies an
// account or a key, and never touches anything it did not create.
package lifecycle

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/lease"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/runtime/store"
	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func leaseChainID(t *testing.T) string {
	t.Helper()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw[:])
}

type leaseChainFixture struct {
	repository  *store.Repository
	backend     *lease.RedisBackend
	coordinator *lease.Coordinator
	claim       lease.Claim
}

// newChainFixture creates one node, slot and assignment of its own, then grants
// a lease through the coordinator so that both stores hold it.
func newLeaseChainFixture(t *testing.T, ctx context.Context) *leaseChainFixture {
	t.Helper()
	dsn := os.Getenv("EXECUTION_MYSQL_TEST_DSN")
	redisURL := os.Getenv("EXECUTION_REDIS_TEST_URL")
	if dsn == "" || redisURL == "" {
		t.Skip("set EXECUTION_MYSQL_TEST_DSN and EXECUTION_REDIS_TEST_URL to run the lease chain integration")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	repository, err := store.NewRepository(db)
	if err != nil {
		t.Fatal(err)
	}
	options, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatal(err)
	}
	options.MaxRetries = -1 // go-redis treats 0 as "use the default"; -1 disables
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}

	suffix := leaseChainID(t)
	// A run-scoped key prefix, so this test can never observe or disturb a key
	// another run or another component owns.
	backend, err := lease.NewRedisBackend(client, fmt.Sprintf("execution:leasechain:%s:", suffix))
	if err != nil {
		t.Fatal(err)
	}
	// The TTL must outlast this test's context budget: one assertion below is
	// that the fencing token is still alive, and an expired key would fail it
	// with a misleading message.
	coordinator, err := lease.NewCoordinator(backend, repository, 5*time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	nodeID := "lc-node-" + suffix
	slotID := "lc-slot-" + suffix
	accountID := "lc-account-" + suffix
	enrollmentID, assignmentID, leaseID := leaseChainID(t), leaseChainID(t), leaseChainID(t)
	sessionID := "lc-session-" + suffix
	token := sha256.Sum256([]byte("leasechain-enrollment-" + suffix))

	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// Only rows this run created, addressed by its own identifiers.
		for _, statement := range []struct {
			query string
			arg   string
		}{
			{"DELETE FROM execution_leases WHERE slot_id = ?", slotID},
			{"DELETE FROM slot_assignments WHERE slot_id = ?", slotID},
			{"DELETE FROM slots WHERE slot_id = ?", slotID},
			{"DELETE FROM node_certificates WHERE node_id = ?", nodeID},
			{"DELETE FROM nodes WHERE node_id = ?", nodeID},
			{"DELETE FROM node_enrollments WHERE enrollment_id = ?", enrollmentID},
		} {
			if _, err := db.ExecContext(cleanup, statement.query, statement.arg); err != nil {
				t.Errorf("cleanup %s: %v", statement.query, err)
			}
		}
	})

	if err := repository.CreateEnrollment(ctx, store.Enrollment{
		ID: enrollmentID, TokenSHA256: token, ExpectedNodeID: nodeID,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	node := store.Node{
		ID: nodeID, Status: "active", Labels: map[string]string{"failure_domain": "leasechain"},
		Capabilities: []string{"docker"}, ProtocolMajor: 1,
		Capacity: store.Capacity{
			MaxSlots: 1, MaxActiveCLI: 1, MaxActiveAPI: 1, MaxActiveTotal: 1,
			AllocatableCPUMillis: 1000, AllocatableMemoryBytes: 1 << 30,
		},
		CreatedAt: now, UpdatedAt: now,
	}
	certificate := store.Certificate{
		SerialNumber: leaseChainID(t), NodeID: nodeID,
		CertificateSHA256: sha256.Sum256([]byte("leasechain-cert-" + suffix)),
		PublicKeySHA256:   sha256.Sum256([]byte("leasechain-key-" + suffix)),
		Status:            "active", NotBefore: now.Add(-time.Minute),
		ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}
	if err := repository.CommitEnrollment(ctx, token, node, certificate, now); err != nil {
		t.Fatal(err)
	}
	if err := repository.AcceptHello(ctx, store.Hello{
		NodeID: nodeID, SessionID: sessionID, Labels: node.Labels, Capabilities: node.Capabilities,
		ProtocolMajor: 1, Capacity: node.Capacity, ReceivedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.RecordHeartbeat(ctx, store.Heartbeat{
		NodeID: nodeID, SessionID: sessionID, ReceivedAt: now.Add(2 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	slotRecord, err := repository.PutDesiredSlot(ctx, store.Slot{
		ID: slotID, AccountID: accountID, Provider: "docker", DesiredState: "ready",
		DesiredGeneration: 1, RequiredLabels: map[string]string{"failure_domain": "leasechain"},
		ImageDigest:      "sha256:" + hex.EncodeToString(certificate.CertificateSHA256[:]),
		CPURequestMillis: 100, MemoryRequestBytes: 1 << 20,
		CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	assignment, err := repository.ReserveAssignment(ctx, store.AssignmentReservation{
		ID: assignmentID, SlotID: slotRecord.ID, NodeID: nodeID, ExpectedNodeSessionID: sessionID,
		NodeSeenAfter: now, ReservedAt: now.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}

	claim := lease.Claim{
		SlotID: slotID, NodeID: nodeID,
		ExecutionEpoch: assignment.ExecutionEpoch, OwnerID: "lc-owner-" + suffix,
	}
	// One grant, both stores. This is the authority doing the writing.
	if err := coordinator.Grant(ctx, leaseID, claim); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Drop this run's fencing token. Rows are removed by the cleanup above.
		_ = backend.Revoke(cleanup, claim)
	})
	return &leaseChainFixture{repository: repository, backend: backend, coordinator: coordinator, claim: claim}
}

// A durable revocation must stop issuance and reclaim an already authenticated
// worker transport, even while the fencing token is still alive. That token is
// what a backend-only check would keep trusting, so leaving it in place is the
// whole point of the exercise.
func TestDurableRevocationStopsIssuanceAndReclaimsTheTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture := newLeaseChainFixture(t, ctx)

	// Both stores agree, so the authority holds.
	if err := fixture.coordinator.Validate(ctx, fixture.claim); err != nil {
		t.Fatalf("a freshly granted lease did not validate: %v", err)
	}

	// A real transport to a real listener, held under that authority.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	connection, err := grpc.NewClient(listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithNoProxy())
	if err != nil {
		t.Fatal(err)
	}
	connection.Connect()
	for connection.GetState() != connectivity.Ready {
		if !connection.WaitForStateChange(ctx, connection.GetState()) {
			t.Fatalf("transport never became ready: %s", connection.GetState())
		}
	}
	worker := executionv1.NewWorkerRuntimeServiceClient(connection)
	if _, err := worker.Health(ctx, &executionv1.HealthRequest{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("the held transport did not reach the server: %v", err)
	}

	custody, err := NewCustody(CustodyConfig{
		Validator: fixture.coordinator, NodeID: fixture.claim.NodeID, RevalidateInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { custody.Drain() })
	adopted, err := custody.take(ctx, provider.Instance{
		ProviderRef: "lc-" + fixture.claim.SlotID, RuntimeID: "lc-container-" + fixture.claim.SlotID,
		SlotID: fixture.claim.SlotID, Epoch: fixture.claim.ExecutionEpoch, RuntimeGeneration: 1,
	}, fixture.claim.OwnerID, connection)
	if err != nil || !adopted {
		t.Fatalf("custody refused a lease both stores agree on: %v %v", adopted, err)
	}
	if custody.Len() != 1 {
		t.Fatalf("custody holds %d runtimes, want 1", custody.Len())
	}

	// Revoke the durable record only.
	if err := fixture.repository.RevokeExecutionLease(ctx,
		fixture.claim.SlotID, fixture.claim.ExecutionEpoch, fixture.claim.OwnerID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	// The fencing token is deliberately still alive: a backend-only gate would
	// go on calling this lease current.
	if err := fixture.backend.Validate(ctx, fixture.claim); err != nil {
		t.Fatalf("the fencing token was supposed to survive: %v", err)
	}

	// Issuance stops.
	if err := fixture.coordinator.Validate(ctx, fixture.claim); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("the authority still accepted a durably revoked lease: %v", err)
	}
	// Custody reclaims.
	if err := custody.Revalidate(ctx); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("revalidate after durable revocation = %v", err)
	}
	if custody.Len() != 0 {
		t.Fatal("a durably revoked lease is still under custody")
	}
	// And the transport is actually gone, not merely forgotten.
	if state := connection.GetState(); state != connectivity.Shutdown {
		t.Fatalf("reclaimed transport state = %s, want Shutdown", state)
	}
	after, err := worker.Health(ctx, &executionv1.HealthRequest{})
	if err == nil {
		t.Fatalf("an RPC succeeded on a reclaimed transport: %v", after)
	}
	if status.Code(err) == codes.Unimplemented {
		t.Fatalf("the reclaimed transport still reached the server: %v", err)
	}
}

// The other direction stays covered on real stores: a lease whose token is gone
// is refused even though the durable row is still active.
func TestMissingFencingTokenAlsoStopsTheChain(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fixture := newLeaseChainFixture(t, ctx)

	if err := fixture.backend.Revoke(ctx, fixture.claim); err != nil {
		t.Fatal(err)
	}
	durable, err := fixture.repository.GetExecutionLease(ctx, fixture.claim.SlotID, fixture.claim.ExecutionEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if durable.RevokedAt != nil {
		t.Fatal("the durable row was supposed to stay active")
	}
	if err := fixture.coordinator.Validate(ctx, fixture.claim); !errors.Is(err, lease.ErrLeaseNotCurrent) {
		t.Fatalf("the authority accepted a lease with no fencing token: %v", err)
	}
}
