package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
)

var (
	ErrResultProjectionRejected = errors.New("onboarding result projection rejected")
	ErrResultPending            = errors.New("onboarding result is pending")
)

const (
	ProvisioningOutcomeSucceeded = "succeeded"
	ProvisioningOutcomeFailed    = "failed"
	ProvisioningOutcomeExpired   = "expired"

	ProvisioningErrorFailed    = "onboarding_failed"
	ProvisioningErrorExpired   = "onboarding_expired"
	ProvisioningSummaryFailed  = "onboarding workflow failed"
	ProvisioningSummaryExpired = "onboarding request expired"
)

type ResultProjection struct {
	AuthType          string
	ExpiresAt         time.Time
	EmailAddress      string
	OrganizationID    string
	UpstreamAccountID string
	Scope             string
	SubscriptionType  string
	RateLimitTier     string
}

func (p ResultProjection) Validate() error {
	if p.AuthType != "oauth" && p.AuthType != "setup_token" && p.AuthType != "api_key" {
		return ErrResultProjectionRejected
	}
	if !p.ExpiresAt.IsZero() && (p.ExpiresAt.Location() != time.UTC || p.ExpiresAt.Year() < 2020 || p.ExpiresAt.Year() > 2200) {
		return ErrResultProjectionRejected
	}
	if p.EmailAddress != "" && (p.EmailAddress != strings.ToLower(p.EmailAddress) || strings.Count(p.EmailAddress, "@") != 1 ||
		strings.HasPrefix(p.EmailAddress, "@") || strings.HasSuffix(p.EmailAddress, "@") || !validProjectionText(p.EmailAddress, 320)) {
		return ErrResultProjectionRejected
	}
	for _, value := range []struct {
		text string
		max  int
	}{
		{p.OrganizationID, 128}, {p.UpstreamAccountID, 128}, {p.Scope, 1024},
		{p.SubscriptionType, 128}, {p.RateLimitTier, 128},
	} {
		if value.text != "" && !validProjectionText(value.text, value.max) {
			return ErrResultProjectionRejected
		}
	}
	return nil
}

func (p ResultProjection) String() string {
	return fmt.Sprintf("ResultProjection{AuthType:%q ExpiresAt:%s EmailAddress:%q OrganizationID:%q UpstreamAccountID:%q Scope:%q SubscriptionType:%q RateLimitTier:%q}",
		p.AuthType, p.ExpiresAt.UTC().Format(time.RFC3339Nano), p.EmailAddress, p.OrganizationID,
		p.UpstreamAccountID, p.Scope, p.SubscriptionType, p.RateLimitTier)
}

func (p ResultProjection) GoString() string { return p.String() }

func (p ResultProjection) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AuthType          string    `json:"auth_type"`
		ExpiresAt         time.Time `json:"expires_at,omitempty"`
		EmailAddress      string    `json:"email_address,omitempty"`
		OrganizationID    string    `json:"organization_id,omitempty"`
		UpstreamAccountID string    `json:"upstream_account_id,omitempty"`
		Scope             string    `json:"scope,omitempty"`
		SubscriptionType  string    `json:"subscription_type,omitempty"`
		RateLimitTier     string    `json:"rate_limit_tier,omitempty"`
	}{p.AuthType, p.ExpiresAt, p.EmailAddress, p.OrganizationID, p.UpstreamAccountID, p.Scope, p.SubscriptionType, p.RateLimitTier})
}

type ResultProjectionCommit struct {
	AccountBinding      string
	SlotID              string
	ExecutionEpoch      uint64
	CredentialLeaseID   string
	ProxyLeaseID        string
	CredentialVersionID string
	Projection          ResultProjection
	CommittedAt         time.Time
}

func (c ResultProjectionCommit) Validate() error {
	for _, value := range []string{c.AccountBinding, c.SlotID, c.CredentialLeaseID, c.ProxyLeaseID, c.CredentialVersionID} {
		if credential.ValidateTransportID(value) != nil {
			return ErrResultProjectionRejected
		}
	}
	if c.ExecutionEpoch == 0 || c.CommittedAt.IsZero() || c.Projection.Validate() != nil {
		return ErrResultProjectionRejected
	}
	return nil
}

type ProvisioningResult struct {
	WorkflowID          string
	IntentID            string
	AccountID           string
	DesiredGeneration   uint64
	SlotID              string
	ExecutionEpoch      uint64
	CredentialLeaseID   string
	CredentialVersionID string
	Projection          ResultProjection
	CreatedAt           time.Time
}

// ProvisioningOutcome is the public, secret-free terminal view of one exact
// intent/account/generation. Result is present only for success. Failure data
// is collapsed to fixed public categories instead of forwarding worker or
// infrastructure error text.
type ProvisioningOutcome struct {
	Status            string
	WorkflowID        string
	IntentID          string
	AccountID         string
	DesiredGeneration uint64
	SlotID            string
	ExecutionEpoch    uint64
	Result            *ProvisioningResult
	ErrorCode         string
	ErrorSummary      string
	FinishedAt        time.Time
}

func (o ProvisioningOutcome) Validate() error {
	if credential.ValidateTransportID(o.IntentID) != nil || credential.ValidateTransportID(o.AccountID) != nil ||
		o.DesiredGeneration == 0 || o.FinishedAt.IsZero() || o.FinishedAt.Location() != time.UTC {
		return ErrResultProjectionRejected
	}
	hasWorkflow := o.WorkflowID != "" || o.SlotID != "" || o.ExecutionEpoch != 0
	if hasWorkflow && (credential.ValidateTransportID(o.WorkflowID) != nil || credential.ValidateTransportID(o.SlotID) != nil || o.ExecutionEpoch == 0) {
		return ErrResultProjectionRejected
	}
	switch o.Status {
	case ProvisioningOutcomeSucceeded:
		if !hasWorkflow || o.Result == nil || o.Result.Validate() != nil || o.ErrorCode != "" || o.ErrorSummary != "" ||
			o.Result.WorkflowID != o.WorkflowID || o.Result.IntentID != o.IntentID || o.Result.AccountID != o.AccountID ||
			o.Result.DesiredGeneration != o.DesiredGeneration || o.Result.SlotID != o.SlotID ||
			o.Result.ExecutionEpoch != o.ExecutionEpoch || o.FinishedAt.Before(o.Result.CreatedAt) {
			return ErrResultProjectionRejected
		}
	case ProvisioningOutcomeFailed:
		if !hasWorkflow || o.Result != nil || o.ErrorCode != ProvisioningErrorFailed || o.ErrorSummary != ProvisioningSummaryFailed {
			return ErrResultProjectionRejected
		}
	case ProvisioningOutcomeExpired:
		if o.Result != nil || o.ErrorCode != ProvisioningErrorExpired || o.ErrorSummary != ProvisioningSummaryExpired {
			return ErrResultProjectionRejected
		}
	default:
		return ErrResultProjectionRejected
	}
	return nil
}

func (o ProvisioningOutcome) String() string {
	return fmt.Sprintf("ProvisioningOutcome{Status:%q WorkflowID:%q IntentID:%q AccountID:%q DesiredGeneration:%d SlotID:%q ExecutionEpoch:%d ErrorCode:%q ErrorSummary:%q FinishedAt:%s Result:[SAFE_PROJECTION]}",
		o.Status, o.WorkflowID, o.IntentID, o.AccountID, o.DesiredGeneration, o.SlotID, o.ExecutionEpoch,
		o.ErrorCode, o.ErrorSummary, o.FinishedAt.UTC().Format(time.RFC3339Nano))
}

func (o ProvisioningOutcome) GoString() string { return o.String() }

func NewSucceededProvisioningOutcome(result ProvisioningResult, finishedAt time.Time) (ProvisioningOutcome, error) {
	outcome := ProvisioningOutcome{
		Status: ProvisioningOutcomeSucceeded, WorkflowID: result.WorkflowID, IntentID: result.IntentID,
		AccountID: result.AccountID, DesiredGeneration: result.DesiredGeneration, SlotID: result.SlotID,
		ExecutionEpoch: result.ExecutionEpoch, Result: &result, FinishedAt: finishedAt.UTC(),
	}
	if outcome.Validate() != nil {
		return ProvisioningOutcome{}, ErrResultProjectionRejected
	}
	return outcome, nil
}

func NewTerminalProvisioningOutcome(workflow *Provisioning, intentID, accountID string, desiredGeneration uint64, expired bool, finishedAt time.Time) (ProvisioningOutcome, error) {
	outcome := ProvisioningOutcome{
		Status: ProvisioningOutcomeFailed, IntentID: intentID, AccountID: accountID,
		DesiredGeneration: desiredGeneration, ErrorCode: ProvisioningErrorFailed,
		ErrorSummary: ProvisioningSummaryFailed, FinishedAt: finishedAt.UTC(),
	}
	if expired {
		outcome.Status = ProvisioningOutcomeExpired
		outcome.ErrorCode = ProvisioningErrorExpired
		outcome.ErrorSummary = ProvisioningSummaryExpired
	}
	if workflow != nil {
		if workflow.Validate() != nil || workflow.IntentID != intentID || workflow.AccountID != accountID ||
			workflow.DesiredGeneration != desiredGeneration {
			return ProvisioningOutcome{}, ErrResultProjectionRejected
		}
		outcome.WorkflowID = workflow.ID
		outcome.SlotID = workflow.SlotID
		outcome.ExecutionEpoch = workflow.ExecutionEpoch
	}
	if outcome.Validate() != nil {
		return ProvisioningOutcome{}, ErrResultProjectionRejected
	}
	return outcome, nil
}

func ProvisioningFailureExpired(errorCode string) bool {
	switch errorCode {
	case "workflow_deadline_exceeded", "command_deadline_exceeded", "intent_expired":
		return true
	default:
		return false
	}
}

func (r ProvisioningResult) Validate() error {
	for _, value := range []string{r.WorkflowID, r.IntentID, r.AccountID, r.SlotID, r.CredentialLeaseID, r.CredentialVersionID} {
		if credential.ValidateTransportID(value) != nil {
			return ErrResultProjectionRejected
		}
	}
	if r.DesiredGeneration == 0 || r.ExecutionEpoch == 0 || r.CreatedAt.IsZero() || r.Projection.Validate() != nil {
		return ErrResultProjectionRejected
	}
	return nil
}

type ResultProjectionRepository interface {
	ProjectProvisioningResult(ctx context.Context, commit ResultProjectionCommit) (ProvisioningResult, error)
	GetProvisioningResult(ctx context.Context, intentID, accountID string, desiredGeneration uint64) (ProvisioningOutcome, error)
}

func ApplyResultProjection(workflow Provisioning, existing *ProvisioningResult, commit ResultProjectionCommit) (ProvisioningResult, error) {
	if workflow.Validate() != nil || commit.Validate() != nil || provider.RuntimeAccountID(workflow.AccountID) != commit.AccountBinding ||
		workflow.SlotID != commit.SlotID || workflow.ExecutionEpoch != commit.ExecutionEpoch ||
		workflow.CredentialLeaseID != commit.CredentialLeaseID || workflow.ProxyLeaseID != commit.ProxyLeaseID {
		return ProvisioningResult{}, ErrResultProjectionRejected
	}
	switch workflow.Status {
	case ProvisioningKeyReady, ProvisioningActivationDispatched, ProvisioningActivationSucceeded, ProvisioningCompleted:
	default:
		return ProvisioningResult{}, ErrResultProjectionRejected
	}
	result := ProvisioningResult{
		WorkflowID: workflow.ID, IntentID: workflow.IntentID, AccountID: workflow.AccountID,
		DesiredGeneration: workflow.DesiredGeneration, SlotID: workflow.SlotID, ExecutionEpoch: workflow.ExecutionEpoch,
		CredentialLeaseID:   workflow.CredentialLeaseID,
		CredentialVersionID: commit.CredentialVersionID, Projection: commit.Projection, CreatedAt: commit.CommittedAt.UTC(),
	}
	if existing != nil {
		result.CreatedAt = existing.CreatedAt
	}
	if result.Validate() != nil {
		return ProvisioningResult{}, ErrResultProjectionRejected
	}
	if existing != nil && *existing != result {
		return ProvisioningResult{}, ErrResultProjectionRejected
	}
	return result, nil
}

func validProjectionText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}
