package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

type recordingProvisioningAdvancer struct {
	mu      sync.Mutex
	ids     []string
	failIDs map[string]bool
}

func (a *recordingProvisioningAdvancer) Advance(_ context.Context, workflowID string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ids = append(a.ids, workflowID)
	if a.failIDs[workflowID] {
		return "", ErrProvisioningAdvance
	}
	return onboarding.ProvisioningKeyDispatched, nil
}

func TestProvisioningRunnerScansBoundedActiveWorkAndIsolatesFailure(t *testing.T) {
	repository := onboarding.NewMemoryProvisioningRepository()
	first := runnerProvisioningRecord()
	second := first
	second.ID, second.IdempotencyKey, second.IntentID = "workflow-2", "event-2", "intent-2"
	second.AccountID, second.SlotID = "account-2", "slot-2"
	second.CredentialLeaseID, second.ProxyLeaseID = "lease-2", "proxy-2"
	second.KeyCommandID, second.ActivationCommandID = "key-2", "activate-2"
	second.UpdatedAt = second.UpdatedAt.Add(time.Second)
	for _, record := range []onboarding.Provisioning{first, second} {
		if _, _, err := repository.CreateProvisioning(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	advancer := &recordingProvisioningAdvancer{failIDs: map[string]bool{second.ID: true}}
	var errorsSeen []string
	runner, err := NewProvisioningRunner(repository, advancer, ProvisioningRunnerConfig{
		BatchSize: 2, PollInterval: time.Hour,
		OnError: func(workflowID string, _ error) { errorsSeen = append(errorsSeen, workflowID) },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Step(context.Background())
	if err != nil || result.Scanned != 2 || result.Failed != 1 {
		t.Fatalf("Step() = %+v, %v", result, err)
	}
	sort.Strings(advancer.ids)
	if len(advancer.ids) != 2 || advancer.ids[0] != first.ID || advancer.ids[1] != second.ID ||
		len(errorsSeen) != 1 || errorsSeen[0] != second.ID {
		t.Fatalf("advanced=%v errors=%v", advancer.ids, errorsSeen)
	}
}

func runnerProvisioningRecord() onboarding.Provisioning {
	now := time.Unix(2_000_000_000, 0).UTC()
	return onboarding.Provisioning{
		ID: "workflow-1", IdempotencyKey: "event-1", IntentID: "intent-1", Owner: "owner-1",
		AccountID: "account-1", DesiredGeneration: 1, NodeID: "srv74", SlotID: "slot-1",
		ExecutionEpoch: 1, ImageDigest: "sha256:" + strings.Repeat("a", 64),
		CredentialLeaseID: "lease-1", ProxyLeaseID: "proxy-1",
		KeyCommandID: "key-1", ActivationCommandID: "activate-1", CommandDeadline: now.Add(time.Hour),
		Status: onboarding.ProvisioningPendingKey, CreatedAt: now, UpdatedAt: now,
	}
}

func TestProvisioningRunnerStopsCleanly(t *testing.T) {
	repository := onboarding.NewMemoryProvisioningRepository()
	advancer := &recordingProvisioningAdvancer{failIDs: make(map[string]bool)}
	runner, err := NewProvisioningRunner(repository, advancer, ProvisioningRunnerConfig{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop")
	}
	if _, err := NewProvisioningRunner(nil, advancer, ProvisioningRunnerConfig{}); !errors.Is(err, ErrProvisioningRun) {
		t.Fatalf("nil repository error = %v", err)
	}
}
