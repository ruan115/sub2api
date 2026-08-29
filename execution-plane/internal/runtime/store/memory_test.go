package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryRepositoryFencesStaleControlSession(t *testing.T) {
	repository := NewMemoryRepository()
	now := time.Unix(2_000_000_000, 0).UTC()
	token := HashToken("one-time-token")
	if err := repository.CreateEnrollment(context.Background(), Enrollment{
		ID: "enrollment-1", TokenSHA256: token, ExpectedNodeID: "srv74",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CommitEnrollment(context.Background(), token, testNode(now), testCertificate(now), now); err != nil {
		t.Fatal(err)
	}
	capacity := Capacity{
		MaxSlots: 20, MaxActiveCLI: 4, MaxActiveAPI: 12, MaxActiveTotal: 12,
		AllocatableCPUMillis: 3_200, AllocatableMemoryBytes: 6 << 30,
	}
	for _, sessionID := range []string{"session-old", "session-new"} {
		if err := repository.AcceptHello(context.Background(), Hello{
			NodeID: "srv74", SessionID: sessionID, ProtocolMajor: 1,
			Capacity: capacity, ReceivedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.RecordHeartbeat(context.Background(), Heartbeat{
		NodeID: "srv74", SessionID: "session-old", ReceivedAt: now.Add(time.Second),
	}); !errors.Is(err, ErrNodeNotFound) {
		t.Fatalf("stale heartbeat error = %v", err)
	}
	if err := repository.MarkDisconnected(context.Background(), "srv74", "session-old", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	node, err := repository.GetNode(context.Background(), "srv74")
	if err != nil {
		t.Fatal(err)
	}
	if node.Status != "connected" || node.ControlSessionID != "session-new" {
		t.Fatalf("stale disconnect changed current session: %+v", node)
	}
}
