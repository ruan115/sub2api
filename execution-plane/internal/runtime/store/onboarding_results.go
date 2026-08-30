package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/onboarding"
)

func (r *Repository) ProjectProvisioningResult(ctx context.Context, commit onboarding.ResultProjectionCommit) (onboarding.ProvisioningResult, error) {
	if commit.Validate() != nil {
		return onboarding.ProvisioningResult{}, onboarding.ErrResultProjectionRejected
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return onboarding.ProvisioningResult{}, fmt.Errorf("begin onboarding result projection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	workflow, err := getOnboardingWorkflowByCredentialLease(ctx, tx, commit.CredentialLeaseID, true)
	if err != nil {
		return onboarding.ProvisioningResult{}, onboarding.ErrResultProjectionRejected
	}
	defer workflow.Destroy()
	existing, existingErr := getProvisioningResultByWorkflow(ctx, tx, workflow.ID, true)
	var existingPointer *onboarding.ProvisioningResult
	if existingErr == nil {
		existingPointer = &existing
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return onboarding.ProvisioningResult{}, fmt.Errorf("read onboarding result projection: %w", existingErr)
	}
	result, err := onboarding.ApplyResultProjection(workflow, existingPointer, commit)
	if err != nil {
		return onboarding.ProvisioningResult{}, onboarding.ErrResultProjectionRejected
	}
	if existingPointer == nil {
		var expiresAt any
		if !result.Projection.ExpiresAt.IsZero() {
			expiresAt = result.Projection.ExpiresAt.UTC()
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO onboarding_results (
  workflow_id, intent_id, account_id, desired_generation, credential_lease_id,
  credential_version_id, auth_type, email_address, organization_id, upstream_account_id,
  scope_text, subscription_type, rate_limit_tier, expires_at, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			result.WorkflowID, result.IntentID, result.AccountID, result.DesiredGeneration, result.CredentialLeaseID,
			result.CredentialVersionID, result.Projection.AuthType, result.Projection.EmailAddress,
			result.Projection.OrganizationID, result.Projection.UpstreamAccountID, result.Projection.Scope,
			result.Projection.SubscriptionType, result.Projection.RateLimitTier, expiresAt, result.CreatedAt.UTC())
		if err != nil {
			return onboarding.ProvisioningResult{}, fmt.Errorf("insert onboarding result projection: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return onboarding.ProvisioningResult{}, fmt.Errorf("commit onboarding result projection: %w", err)
	}
	return result, nil
}

func (r *Repository) GetProvisioningResult(ctx context.Context, intentID, accountID string, desiredGeneration uint64) (onboarding.ProvisioningOutcome, error) {
	if ctx == nil || credential.ValidateTransportID(intentID) != nil || credential.ValidateTransportID(accountID) != nil || desiredGeneration == 0 {
		return onboarding.ProvisioningOutcome{}, onboarding.ErrResultProjectionRejected
	}
	if err := ctx.Err(); err != nil {
		return onboarding.ProvisioningOutcome{}, err
	}
	workflow, err := getLatestOnboardingWorkflowByIntent(ctx, r.db, intentID, accountID, desiredGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		state, stateErr := getOnboardingIntentResultState(ctx, r.db, intentID, accountID, desiredGeneration)
		if errors.Is(stateErr, sql.ErrNoRows) {
			return onboarding.ProvisioningOutcome{}, onboarding.ErrResultPending
		}
		if stateErr != nil {
			return onboarding.ProvisioningOutcome{}, stateErr
		}
		if state.status == onboarding.IntentConsumed {
			return onboarding.ProvisioningOutcome{}, onboarding.ErrResultProjectionRejected
		}
		if state.expired {
			return onboarding.NewTerminalProvisioningOutcome(nil, intentID, accountID, desiredGeneration, true, state.expiresAt)
		}
		return onboarding.ProvisioningOutcome{}, onboarding.ErrResultPending
	}
	if errors.Is(err, onboarding.ErrProvisioningRejected) {
		return onboarding.ProvisioningOutcome{}, onboarding.ErrResultProjectionRejected
	}
	if err != nil {
		return onboarding.ProvisioningOutcome{}, err
	}
	defer workflow.Destroy()
	switch workflow.Status {
	case onboarding.ProvisioningCompleted:
		result, resultErr := getProvisioningResultByWorkflow(ctx, r.db, workflow.ID, false)
		if errors.Is(resultErr, sql.ErrNoRows) || errors.Is(resultErr, onboarding.ErrResultProjectionRejected) {
			return onboarding.ProvisioningOutcome{}, onboarding.ErrResultProjectionRejected
		}
		if resultErr != nil {
			return onboarding.ProvisioningOutcome{}, resultErr
		}
		return onboarding.NewSucceededProvisioningOutcome(result, workflow.UpdatedAt)
	case onboarding.ProvisioningFailed:
		return onboarding.NewTerminalProvisioningOutcome(
			&workflow, intentID, accountID, desiredGeneration,
			onboarding.ProvisioningFailureExpired(workflow.ErrorCode), workflow.UpdatedAt,
		)
	default:
		state, stateErr := getOnboardingIntentResultState(ctx, r.db, intentID, accountID, desiredGeneration)
		if errors.Is(stateErr, sql.ErrNoRows) {
			return onboarding.ProvisioningOutcome{}, onboarding.ErrResultProjectionRejected
		}
		if stateErr != nil {
			return onboarding.ProvisioningOutcome{}, stateErr
		}
		if state.expired {
			return onboarding.NewTerminalProvisioningOutcome(
				&workflow, intentID, accountID, desiredGeneration, true, state.expiresAt,
			)
		}
		return onboarding.ProvisioningOutcome{}, onboarding.ErrResultPending
	}
}

type onboardingIntentResultState struct {
	status    string
	expiresAt time.Time
	expired   bool
}

func getLatestOnboardingWorkflowByIntent(
	ctx context.Context,
	queryer onboardingWorkflowQueryer,
	intentID, accountID string,
	desiredGeneration uint64,
) (onboarding.Provisioning, error) {
	return scanOnboardingWorkflow(queryer.QueryRowContext(ctx, onboardingWorkflowSelect+`
 WHERE intent_id = ? AND account_id = ? AND desired_generation = ?
 ORDER BY created_at DESC, workflow_id DESC LIMIT 1`, intentID, accountID, desiredGeneration))
}

func getOnboardingIntentResultState(
	ctx context.Context,
	queryer onboardingWorkflowQueryer,
	intentID, accountID string,
	desiredGeneration uint64,
) (onboardingIntentResultState, error) {
	var state onboardingIntentResultState
	err := queryer.QueryRowContext(ctx, `
SELECT status, expires_at, expires_at <= UTC_TIMESTAMP(6)
FROM onboarding_intents
WHERE intent_id = ? AND account_id = ? AND desired_generation = ?`, intentID, accountID, desiredGeneration).
		Scan(&state.status, &state.expiresAt, &state.expired)
	if err != nil {
		return onboardingIntentResultState{}, err
	}
	state.expiresAt = state.expiresAt.UTC()
	if state.expiresAt.IsZero() ||
		(state.status != onboarding.IntentPending && state.status != onboarding.IntentClaimed && state.status != onboarding.IntentConsumed) {
		return onboardingIntentResultState{}, onboarding.ErrResultProjectionRejected
	}
	return state, nil
}

func getOnboardingWorkflowByCredentialLease(ctx context.Context, queryer onboardingWorkflowQueryer, leaseID string, forUpdate bool) (onboarding.Provisioning, error) {
	query := onboardingWorkflowSelect + " WHERE credential_lease_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanOnboardingWorkflow(queryer.QueryRowContext(ctx, query, leaseID))
}

const onboardingResultSelect = `
SELECT result.workflow_id, result.intent_id, result.account_id, result.desired_generation,
       w.slot_id, w.execution_epoch,
       result.credential_lease_id, result.credential_version_id, result.auth_type,
       result.email_address, result.organization_id, result.upstream_account_id,
       result.scope_text, result.subscription_type, result.rate_limit_tier,
       result.expires_at, result.created_at
FROM onboarding_results result
JOIN onboarding_workflows w ON w.workflow_id = result.workflow_id `

func getProvisioningResultByWorkflow(ctx context.Context, queryer onboardingWorkflowQueryer, workflowID string, forUpdate bool) (onboarding.ProvisioningResult, error) {
	query := onboardingResultSelect + "WHERE result.workflow_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	return scanProvisioningResult(queryer.QueryRowContext(ctx, query, workflowID))
}

func scanProvisioningResult(row rowScanner) (onboarding.ProvisioningResult, error) {
	var result onboarding.ProvisioningResult
	var expiresAt sql.NullTime
	err := row.Scan(
		&result.WorkflowID, &result.IntentID, &result.AccountID, &result.DesiredGeneration,
		&result.SlotID, &result.ExecutionEpoch,
		&result.CredentialLeaseID, &result.CredentialVersionID, &result.Projection.AuthType,
		&result.Projection.EmailAddress, &result.Projection.OrganizationID, &result.Projection.UpstreamAccountID,
		&result.Projection.Scope, &result.Projection.SubscriptionType, &result.Projection.RateLimitTier,
		&expiresAt, &result.CreatedAt,
	)
	if err != nil {
		return onboarding.ProvisioningResult{}, err
	}
	if expiresAt.Valid {
		result.Projection.ExpiresAt = expiresAt.Time.UTC()
	}
	result.CreatedAt = result.CreatedAt.UTC()
	if result.Validate() != nil {
		return onboarding.ProvisioningResult{}, onboarding.ErrResultProjectionRejected
	}
	return result, nil
}

var _ onboarding.ResultProjectionRepository = (*Repository)(nil)
