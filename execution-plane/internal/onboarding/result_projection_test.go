package onboarding

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

func TestMemoryProvisioningResultProjectionIsBoundIdempotentAndCompletionGated(t *testing.T) {
	now := time.Unix(2_000_000_000, 0).UTC()
	repository := NewMemoryProvisioningRepository()
	workflow := testProvisioning(now)
	if _, _, err := repository.CreateProvisioning(context.Background(), workflow); err != nil {
		t.Fatal(err)
	}
	recipient, _ := credential.NewRecipient(rand.Reader)
	defer recipient.Destroy()
	keyID, publicKey, _ := recipient.PublicKey()
	if _, err := repository.ObserveProvisioningCommand(context.Background(), ProvisioningCommandObservation{
		CommandID: workflow.KeyCommandID, NodeID: workflow.NodeID, SlotID: workflow.SlotID,
		ExecutionEpoch: workflow.ExecutionEpoch, ImageDigest: workflow.ImageDigest, Healthy: true, Succeeded: true,
		KeyID: keyID, KeyPublicKey: publicKey, ReceivedAt: now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	commit := ResultProjectionCommit{
		AccountBinding: provider.RuntimeAccountID(workflow.AccountID), SlotID: workflow.SlotID,
		ExecutionEpoch: workflow.ExecutionEpoch, CredentialLeaseID: workflow.CredentialLeaseID,
		ProxyLeaseID: workflow.ProxyLeaseID, CredentialVersionID: "credential-version-10380",
		Projection:  ResultProjection{AuthType: "oauth", EmailAddress: "owner@example.com", SubscriptionType: "max"},
		CommittedAt: now.Add(2 * time.Second),
	}
	stored, err := repository.ProjectProvisioningResult(context.Background(), commit)
	if err != nil || stored.IntentID != workflow.IntentID || stored.Projection.EmailAddress != "owner@example.com" {
		t.Fatalf("project result = %+v, %v", stored, err)
	}
	commit.CommittedAt = now.Add(3 * time.Second)
	replayed, err := repository.ProjectProvisioningResult(context.Background(), commit)
	if err != nil || !replayed.CreatedAt.Equal(stored.CreatedAt) {
		t.Fatalf("replay result = %+v, %v", replayed, err)
	}
	if _, err := repository.GetProvisioningResult(context.Background(), workflow.IntentID, workflow.AccountID, workflow.DesiredGeneration); !errors.Is(err, ErrResultPending) {
		t.Fatalf("pre-completion result error = %v", err)
	}
	if err := repository.MarkProvisioningActivationDispatched(context.Background(), workflow.ID, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ObserveProvisioningCommand(context.Background(), ProvisioningCommandObservation{
		CommandID: workflow.ActivationCommandID, NodeID: workflow.NodeID, SlotID: workflow.SlotID,
		ExecutionEpoch: workflow.ExecutionEpoch, ImageDigest: workflow.ImageDigest, Healthy: true, Succeeded: true,
		ReceivedAt: now.Add(4 * time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.CompleteProvisioning(context.Background(), workflow.ID, now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	completed, err := repository.GetProvisioningResult(context.Background(), workflow.IntentID, workflow.AccountID, workflow.DesiredGeneration)
	if err != nil || completed.Status != ProvisioningOutcomeSucceeded || completed.Result == nil ||
		completed.Result.CredentialVersionID != commit.CredentialVersionID || !completed.FinishedAt.Equal(now.Add(5*time.Second)) {
		t.Fatalf("completed result = %+v, %v", completed, err)
	}

	tampered := commit
	tampered.ProxyLeaseID = "proxy-other"
	if _, err := repository.ProjectProvisioningResult(context.Background(), tampered); !errors.Is(err, ErrResultProjectionRejected) {
		t.Fatalf("tampered projection error = %v", err)
	}
}

func TestMemoryProvisioningResultReturnsSafeFailedAndExpiredOutcomes(t *testing.T) {
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
			name: "failed", internalCode: "worker_key_failed", status: ProvisioningOutcomeFailed,
			publicCode: ProvisioningErrorFailed, publicText: ProvisioningSummaryFailed,
		},
		{
			name: "expired", internalCode: "workflow_deadline_exceeded", status: ProvisioningOutcomeExpired,
			publicCode: ProvisioningErrorExpired, publicText: ProvisioningSummaryExpired,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repository := NewMemoryProvisioningRepository()
			workflow := testProvisioning(now)
			if _, _, err := repository.CreateProvisioning(context.Background(), workflow); err != nil {
				t.Fatal(err)
			}
			finishedAt := now.Add(time.Minute)
			if err := repository.FailProvisioning(context.Background(), workflow.ID, test.internalCode, finishedAt); err != nil {
				t.Fatal(err)
			}
			outcome, err := repository.GetProvisioningResult(
				context.Background(), workflow.IntentID, workflow.AccountID, workflow.DesiredGeneration,
			)
			if err != nil || outcome.Validate() != nil || outcome.Status != test.status || outcome.Result != nil ||
				outcome.ErrorCode != test.publicCode || outcome.ErrorSummary != test.publicText ||
				!outcome.FinishedAt.Equal(finishedAt) {
				t.Fatalf("terminal outcome = %+v, %v", outcome, err)
			}
			if strings.Contains(outcome.String(), test.internalCode) {
				t.Fatal("terminal outcome exposed internal failure code")
			}
		})
	}
}

func TestMemoryProvisioningResultLookupDoesNotMaskInvalidInputOrCancellationAsPending(t *testing.T) {
	t.Parallel()
	repository := NewMemoryProvisioningRepository()
	if _, err := repository.GetProvisioningResult(context.Background(), "", "account-10380", 1); !errors.Is(err, ErrResultProjectionRejected) {
		t.Fatalf("invalid lookup error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.GetProvisioningResult(canceled, "intent-10380", "account-10380", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup error = %v", err)
	}
}
