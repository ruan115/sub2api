package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	runtimeOutboxBlockedRetryPath   = "/api/runtime-outbox/blocked/retry"
	runtimeOutboxBlockedRetryAction = "runtime_outbox.blocked_retry"
)

var (
	errRuntimeOutboxBlockedRetryConflict = errors.New("blocked runtime outbox retry identity is stale or conflicting")
	errRuntimeOutboxBlockedRetryAuth     = errors.New("blocked runtime outbox retry administrator is no longer authorized")
	runtimeOutboxConsumerNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	runtimeOutboxRetryReasonPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
	runtimeOutboxChangeTicketPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
)

type runtimeOutboxBlockedRetryInput struct {
	ConsumerName        string `json:"consumer_name"`
	Sequence            int64  `json:"sequence"`
	BlockedClaimVersion uint64 `json:"blocked_claim_version"`
	ReasonCode          string `json:"reason_code"`
	ChangeTicket        string `json:"change_ticket,omitempty"`
}

type runtimeOutboxBlockedRetryResponse struct {
	ConsumerName        string `json:"consumer_name"`
	Sequence            int64  `json:"sequence"`
	BlockedClaimVersion uint64 `json:"blocked_claim_version"`
	Retried             bool   `json:"retried"`
	Replayed            bool   `json:"replayed"`
}

type runtimeOutboxBlockedRetryAudit struct {
	Request              runtimeOutboxBlockedRetryInput `json:"request"`
	OriginalFailureClass string                         `json:"original_failure_class"`
	OriginalFailureCode  string                         `json:"original_failure_code"`
	OriginalFailureCount uint64                         `json:"original_failure_count"`
}

type runtimeOutboxBlockedCheckpoint struct {
	LastSequence        int64
	ClaimedSequence     int64
	LockedBy            string
	LeaseExpiresAt      int64
	ClaimVersion        uint64
	FailureState        string
	FailureSequence     int64
	FailureClass        string
	FailureCode         string
	FailureCount        uint64
	FirstFailedAt       int64
	LastFailedAt        int64
	NextAttemptAt       int64
	BlockedClaimVersion uint64
	LastError           string
}

func (a *app) handleRuntimeOutboxBlockedRetry(w http.ResponseWriter, r *http.Request) {
	// Middleware authorization is necessary but not sufficient for this
	// recovery operation. In particular, auth-disabled development mode creates
	// a synthetic admin with ID 0 and must never authorize production recovery.
	actor := currentUser(r)
	if actor.ID <= 0 || actor.Role != "admin" || actor.Status != "active" {
		writeError(w, http.StatusForbidden, "authenticated administrator required")
		return
	}
	input, err := decodeRuntimeOutboxBlockedRetryInput(w, r)
	if err != nil {
		if !errors.Is(err, errResponseWritten) {
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	response, err := a.retryBlockedRuntimeOutbox(r.Context(), actor, input, requestIP(r), r.UserAgent())
	if err != nil {
		if errors.Is(err, errRuntimeOutboxBlockedRetryAuth) {
			writeError(w, http.StatusForbidden, "authenticated administrator required")
			return
		}
		if errors.Is(err, errRuntimeOutboxBlockedRetryConflict) {
			writeError(w, http.StatusConflict, "blocked runtime outbox retry identity is stale or conflicting")
			return
		}
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// errResponseWritten tells the handler that the strict decoder already wrote
// the size/JSON error using the shared API envelope.
var errResponseWritten = errors.New("HTTP response already written")

func decodeRuntimeOutboxBlockedRetryInput(w http.ResponseWriter, r *http.Request) (runtimeOutboxBlockedRetryInput, error) {
	var input runtimeOutboxBlockedRetryInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		if err == nil {
			err = errors.New("request must be a JSON object")
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return runtimeOutboxBlockedRetryInput{}, errResponseWritten
	}
	seen := make(map[string]bool, 5)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return runtimeOutboxBlockedRetryInput{}, errResponseWritten
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			writeError(w, http.StatusBadRequest, "invalid JSON: duplicate or invalid field")
			return runtimeOutboxBlockedRetryInput{}, errResponseWritten
		}
		seen[key] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil || string(raw) == "null" {
			if err == nil {
				err = errors.New("null field")
			}
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return runtimeOutboxBlockedRetryInput{}, errResponseWritten
		}
		switch key {
		case "consumer_name":
			err = json.Unmarshal(raw, &input.ConsumerName)
		case "sequence":
			err = json.Unmarshal(raw, &input.Sequence)
		case "blocked_claim_version":
			err = json.Unmarshal(raw, &input.BlockedClaimVersion)
		case "reason_code":
			err = json.Unmarshal(raw, &input.ReasonCode)
		case "change_ticket":
			err = json.Unmarshal(raw, &input.ChangeTicket)
		default:
			err = fmt.Errorf("unknown field %q", key)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return runtimeOutboxBlockedRetryInput{}, errResponseWritten
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		if err == nil {
			err = errors.New("unterminated JSON object")
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return runtimeOutboxBlockedRetryInput{}, errResponseWritten
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid JSON: request must contain exactly one object")
		return runtimeOutboxBlockedRetryInput{}, errResponseWritten
	}
	for _, required := range []string{"consumer_name", "sequence", "blocked_claim_version", "reason_code"} {
		if !seen[required] {
			return runtimeOutboxBlockedRetryInput{}, errors.New("invalid blocked runtime outbox retry request")
		}
	}
	if !validRuntimeOutboxBlockedRetryInput(input) {
		return runtimeOutboxBlockedRetryInput{}, errors.New("invalid blocked runtime outbox retry request")
	}
	return input, nil
}

func validRuntimeOutboxBlockedRetryInput(input runtimeOutboxBlockedRetryInput) bool {
	if !runtimeOutboxConsumerNamePattern.MatchString(input.ConsumerName) || input.Sequence <= 0 ||
		input.BlockedClaimVersion == 0 || !runtimeOutboxRetryReasonPattern.MatchString(input.ReasonCode) ||
		runtimeSecretString(input.ConsumerName) || runtimeSecretString(input.ReasonCode) {
		return false
	}
	if input.ChangeTicket != "" && (!runtimeOutboxChangeTicketPattern.MatchString(input.ChangeTicket) ||
		runtimeSecretString(input.ChangeTicket)) {
		return false
	}
	return true
}

func (a *app) retryBlockedRuntimeOutbox(
	ctx context.Context,
	actor panelUser,
	input runtimeOutboxBlockedRetryInput,
	clientIP string,
	userAgent string,
) (runtimeOutboxBlockedRetryResponse, error) {
	response := runtimeOutboxBlockedRetryResponse{
		ConsumerName: input.ConsumerName, Sequence: input.Sequence,
		BlockedClaimVersion: input.BlockedClaimVersion, Retried: true,
	}
	if a == nil || a.db == nil || ctx == nil || ctx.Err() != nil || actor.ID <= 0 ||
		actor.Role != "admin" || actor.Status != "active" || !validRuntimeOutboxBlockedRetryInput(input) {
		return runtimeOutboxBlockedRetryResponse{}, errRuntimeOutboxBlockedRetryConflict
	}
	if input.ConsumerName != a.runtimeOutboxConsumer {
		return runtimeOutboxBlockedRetryResponse{}, errRuntimeOutboxBlockedRetryConflict
	}
	targetID := runtimeOutboxBlockedRetryTargetID(input)
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("begin blocked runtime outbox retry: %w", err)
	}
	defer tx.Rollback()
	actor, err = lockRuntimeOutboxRetryActorTx(ctx, tx, actor.ID)
	if errors.Is(err, errRuntimeOutboxBlockedRetryAuth) {
		return runtimeOutboxBlockedRetryResponse{}, err
	}
	if err != nil {
		return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("revalidate runtime outbox retry administrator: %w", err)
	}

	// SQLite file DSNs use BEGIN IMMEDIATE, but in-memory test databases do not.
	// This harmless write obtains the equivalent write lock before inspection.
	if tx.dialect == dialectSQLite {
		if _, err := tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers
			SET consumer_name = consumer_name WHERE consumer_name = ?`, input.ConsumerName); err != nil {
			return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("lock SQLite runtime outbox checkpoint: %w", err)
		}
	}
	checkpointQuery := `SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at,
		claim_version, failure_state, failure_sequence, failure_class, failure_code,
		failure_count, first_failed_at, last_failed_at, next_attempt_at,
		blocked_claim_version, last_error
		FROM runtime_outbox_consumers WHERE consumer_name = ?`
	if tx.dialect == dialectMySQL {
		checkpointQuery += ` FOR UPDATE`
	}
	checkpoint, err := readRuntimeOutboxBlockedCheckpoint(ctx, tx, checkpointQuery, input.ConsumerName)
	if errors.Is(err, sql.ErrNoRows) {
		return runtimeOutboxBlockedRetryResponse{}, errRuntimeOutboxBlockedRetryConflict
	}
	if err != nil {
		return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("lock runtime outbox checkpoint: %w", err)
	}
	nextSequence, err := nextRuntimeOutboxSequenceTx(ctx, tx, checkpoint.LastSequence)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && nextSequence != input.Sequence) {
		return runtimeOutboxBlockedRetryResponse{}, errRuntimeOutboxBlockedRetryConflict
	}
	if err != nil {
		return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("read next runtime outbox event: %w", err)
	}

	if runtimeOutboxCheckpointIsExactBlocked(checkpoint, input) {
		auditBody, err := json.Marshal(runtimeOutboxBlockedRetryAudit{
			Request: input, OriginalFailureClass: checkpoint.FailureClass,
			OriginalFailureCode: checkpoint.FailureCode, OriginalFailureCount: checkpoint.FailureCount,
		})
		if err != nil {
			return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("encode blocked runtime outbox retry audit: %w", err)
		}
		result, err := tx.ExecContext(ctx, `UPDATE runtime_outbox_consumers SET
			failure_state = 'ready', failure_sequence = 0, failure_class = '', failure_code = '',
			failure_count = 0, first_failed_at = 0, last_failed_at = 0,
			next_attempt_at = 0, blocked_claim_version = 0, last_error = '', updated_at = `+nowSQL+`
			WHERE consumer_name = ? AND last_sequence = ? AND claim_version = ?
			  AND failure_state = 'blocked' AND failure_sequence = ? AND blocked_claim_version = ?
			  AND claimed_sequence = 0 AND locked_by = '' AND lease_expires_at = 0`,
			input.ConsumerName, checkpoint.LastSequence, checkpoint.ClaimVersion,
			input.Sequence, input.BlockedClaimVersion,
		)
		if err != nil {
			return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("clear blocked runtime outbox failure: %w", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return runtimeOutboxBlockedRetryResponse{}, errRuntimeOutboxBlockedRetryConflict
		}
		if err := insertRuntimeOutboxBlockedRetryAuditTx(
			ctx, tx, actor, targetID, string(auditBody), clientIP, userAgent,
		); err != nil {
			return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("insert blocked runtime outbox retry audit: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("commit blocked runtime outbox retry: %w", err)
		}
		return response, nil
	}

	if runtimeOutboxCheckpointIsExactRetryPostState(checkpoint, input) {
		replayed, err := runtimeOutboxBlockedRetryAuditExistsTx(ctx, tx, actor.ID, targetID, input)
		if err != nil {
			return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("read blocked runtime outbox retry audit: %w", err)
		}
		if replayed {
			if err := tx.Commit(); err != nil {
				return runtimeOutboxBlockedRetryResponse{}, fmt.Errorf("commit blocked runtime outbox retry replay: %w", err)
			}
			response.Replayed = true
			return response, nil
		}
	}
	return runtimeOutboxBlockedRetryResponse{}, errRuntimeOutboxBlockedRetryConflict
}

func lockRuntimeOutboxRetryActorTx(ctx context.Context, tx *databaseTx, actorID int64) (panelUser, error) {
	if tx == nil || actorID <= 0 {
		return panelUser{}, errRuntimeOutboxBlockedRetryAuth
	}
	if tx.dialect == dialectSQLite {
		result, err := tx.ExecContext(ctx, `UPDATE users SET id = id WHERE id = ? AND deleted_at IS NULL`, actorID)
		if err != nil {
			return panelUser{}, err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return panelUser{}, err
		} else if affected != 1 {
			return panelUser{}, errRuntimeOutboxBlockedRetryAuth
		}
	}
	query := `SELECT id, username, role, status FROM users WHERE id = ? AND deleted_at IS NULL`
	if tx.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	var actor panelUser
	if err := tx.QueryRowContext(ctx, query, actorID).Scan(&actor.ID, &actor.Username, &actor.Role, &actor.Status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return panelUser{}, errRuntimeOutboxBlockedRetryAuth
		}
		return panelUser{}, err
	}
	if actor.ID <= 0 || actor.Role != "admin" || actor.Status != "active" || strings.TrimSpace(actor.Username) == "" {
		return panelUser{}, errRuntimeOutboxBlockedRetryAuth
	}
	return actor, nil
}

func readRuntimeOutboxBlockedCheckpoint(
	ctx context.Context,
	tx *databaseTx,
	query string,
	consumerName string,
) (runtimeOutboxBlockedCheckpoint, error) {
	var checkpoint runtimeOutboxBlockedCheckpoint
	err := tx.QueryRowContext(ctx, query, consumerName).Scan(
		&checkpoint.LastSequence, &checkpoint.ClaimedSequence, &checkpoint.LockedBy,
		&checkpoint.LeaseExpiresAt, &checkpoint.ClaimVersion, &checkpoint.FailureState,
		&checkpoint.FailureSequence, &checkpoint.FailureClass, &checkpoint.FailureCode,
		&checkpoint.FailureCount, &checkpoint.FirstFailedAt, &checkpoint.LastFailedAt,
		&checkpoint.NextAttemptAt, &checkpoint.BlockedClaimVersion, &checkpoint.LastError,
	)
	return checkpoint, err
}

func nextRuntimeOutboxSequenceTx(ctx context.Context, tx *databaseTx, lastSequence int64) (int64, error) {
	var sequence int64
	query := `SELECT sequence FROM runtime_outbox WHERE sequence > ? ORDER BY sequence LIMIT 1`
	if tx.dialect == dialectMySQL {
		query += ` FOR UPDATE`
	}
	err := tx.QueryRowContext(ctx, query, lastSequence).Scan(&sequence)
	return sequence, err
}

func runtimeOutboxCheckpointHasNoClaim(checkpoint runtimeOutboxBlockedCheckpoint) bool {
	return checkpoint.ClaimedSequence == 0 && checkpoint.LockedBy == "" && checkpoint.LeaseExpiresAt == 0
}

func runtimeOutboxCheckpointIsExactBlocked(
	checkpoint runtimeOutboxBlockedCheckpoint,
	input runtimeOutboxBlockedRetryInput,
) bool {
	return checkpoint.LastSequence >= 0 && checkpoint.LastSequence < input.Sequence &&
		runtimeOutboxCheckpointHasNoClaim(checkpoint) && checkpoint.ClaimVersion == input.BlockedClaimVersion &&
		checkpoint.FailureState == "blocked" && checkpoint.FailureSequence == input.Sequence &&
		checkpoint.BlockedClaimVersion == input.BlockedClaimVersion && checkpoint.FailureCount > 0 &&
		checkpoint.FirstFailedAt > 0 && checkpoint.LastFailedAt >= checkpoint.FirstFailedAt &&
		checkpoint.NextAttemptAt == 0 && runtimeOutboxBlockingFailureClass(checkpoint.FailureClass) &&
		runtimeOutboxRetryReasonPattern.MatchString(checkpoint.FailureCode) &&
		checkpoint.LastError == checkpoint.FailureCode
}

func runtimeOutboxCheckpointIsExactRetryPostState(
	checkpoint runtimeOutboxBlockedCheckpoint,
	input runtimeOutboxBlockedRetryInput,
) bool {
	return checkpoint.LastSequence >= 0 && checkpoint.LastSequence < input.Sequence &&
		runtimeOutboxCheckpointHasNoClaim(checkpoint) && checkpoint.ClaimVersion == input.BlockedClaimVersion &&
		checkpoint.FailureState == "ready" && checkpoint.FailureSequence == 0 &&
		checkpoint.FailureClass == "" && checkpoint.FailureCode == "" && checkpoint.FailureCount == 0 &&
		checkpoint.FirstFailedAt == 0 && checkpoint.LastFailedAt == 0 && checkpoint.NextAttemptAt == 0 &&
		checkpoint.BlockedClaimVersion == 0 && checkpoint.LastError == ""
}

func runtimeOutboxBlockingFailureClass(value string) bool {
	switch value {
	case "terminal", "authority", "integrity", "security", "indeterminate":
		return true
	default:
		return false
	}
}

func runtimeOutboxBlockedRetryTargetID(input runtimeOutboxBlockedRetryInput) string {
	return input.ConsumerName + ":" + strconv.FormatInt(input.Sequence, 10) + ":" +
		strconv.FormatUint(input.BlockedClaimVersion, 10)
}

func insertRuntimeOutboxBlockedRetryAuditTx(
	ctx context.Context,
	tx *databaseTx,
	actor panelUser,
	targetID string,
	requestBody string,
	clientIP string,
	userAgent string,
) error {
	clientIP = truncateRuntimeOutboxAuditValue(strings.TrimSpace(clientIP), 64)
	userAgent = truncateRuntimeOutboxAuditValue(strings.TrimSpace(userAgent), 1024)
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_logs (
		actor_user_id, actor_username, actor_role, action, method, path,
		target_type, target_id, request_body, client_ip, user_agent, status_code, duration_ms
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		actor.ID, actor.Username, actor.Role, runtimeOutboxBlockedRetryAction,
		http.MethodPost, runtimeOutboxBlockedRetryPath, "runtime_outbox_consumer", targetID,
		requestBody, clientIP, userAgent, http.StatusOK, int64(0),
	)
	return err
}

func runtimeOutboxBlockedRetryAuditExistsTx(
	ctx context.Context,
	tx *databaseTx,
	actorID int64,
	targetID string,
	input runtimeOutboxBlockedRetryInput,
) (bool, error) {
	var count int
	var requestBody string
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(request_body), '') FROM audit_logs
		WHERE actor_user_id = ? AND action = ? AND method = ? AND path = ?
		  AND target_type = ? AND target_id = ? AND status_code = ?`,
		actorID, runtimeOutboxBlockedRetryAction, http.MethodPost, runtimeOutboxBlockedRetryPath,
		"runtime_outbox_consumer", targetID, http.StatusOK,
	).Scan(&count, &requestBody)
	if err != nil || count != 1 {
		return false, err
	}
	var audit runtimeOutboxBlockedRetryAudit
	decoder := json.NewDecoder(strings.NewReader(requestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&audit); err != nil {
		return false, nil
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return false, nil
	}
	if audit.Request != input || !runtimeOutboxBlockingFailureClass(audit.OriginalFailureClass) ||
		!runtimeOutboxRetryReasonPattern.MatchString(audit.OriginalFailureCode) ||
		runtimeSecretString(audit.OriginalFailureCode) || audit.OriginalFailureCount == 0 {
		return false, nil
	}
	return true, nil
}

func truncateRuntimeOutboxAuditValue(value string, limit int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		_, size := utf8.DecodeLastRuneInString(value)
		if size <= 0 {
			size = 1
		}
		value = value[:len(value)-size]
	}
	return value
}
