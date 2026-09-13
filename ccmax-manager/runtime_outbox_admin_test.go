package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

const testRuntimeOutboxConsumer = "sub2api-execution-runtime-v1"

type runtimeOutboxBlockedRetryFixture struct {
	app      *app
	handler  http.Handler
	admin    panelUser
	cookie   *http.Cookie
	consumer string
	first    int64
	second   int64
	version  uint64
}

func newRuntimeOutboxBlockedRetryFixture(t *testing.T) runtimeOutboxBlockedRetryFixture {
	t.Helper()
	t.Setenv("CCMAX_AUTH_DISABLED", "")
	t.Setenv("CCMAX_ADMIN_PASSWORD", "blocked-retry-admin-password")
	t.Setenv("EXECUTION_RUNTIME_OUTBOX_CONSUMER_NAME", testRuntimeOutboxConsumer)
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-outbox-retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.db.Close() })
	handler := a.routes()
	cookie := loginCookie(t, handler, "admin", "blocked-retry-admin-password")
	var admin panelUser
	if err := a.db.QueryRow(`SELECT id, username, role, status FROM users
		WHERE username = 'admin' AND deleted_at IS NULL`).Scan(
		&admin.ID, &admin.Username, &admin.Role, &admin.Status,
	); err != nil {
		t.Fatal(err)
	}
	firstResult, err := a.db.Exec(`INSERT INTO runtime_outbox
		(event_id, account_id, event_type, desired_generation, payload_json)
		VALUES ('11111111-1111-4111-8111-111111111111', 7, 'account.runtime.restore_requested', 1, '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := firstResult.LastInsertId()
	secondResult, err := a.db.Exec(`INSERT INTO runtime_outbox
		(event_id, account_id, event_type, desired_generation, payload_json)
		VALUES ('22222222-2222-4222-8222-222222222222', 8, 'account.runtime.drain_requested', 1, '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := secondResult.LastInsertId()
	fixture := runtimeOutboxBlockedRetryFixture{
		app: a, handler: handler, admin: admin, cookie: cookie,
		consumer: testRuntimeOutboxConsumer, first: first, second: second, version: 9,
	}
	fixture.setBlockedCheckpoint(t, first)
	return fixture
}

func (f runtimeOutboxBlockedRetryFixture) setBlockedCheckpoint(t *testing.T, sequence int64) {
	t.Helper()
	if _, err := f.app.db.Exec(`DELETE FROM runtime_outbox_consumers WHERE consumer_name = ?`, f.consumer); err != nil {
		t.Fatal(err)
	}
	if _, err := f.app.db.Exec(`INSERT INTO runtime_outbox_consumers (
		consumer_name, last_sequence, claimed_sequence, locked_by, lease_expires_at,
		claim_version, failure_state, failure_sequence, failure_class, failure_code,
		failure_count, first_failed_at, last_failed_at, next_attempt_at,
		blocked_claim_version, last_error
	) VALUES (?, 0, 0, '', 0, ?, 'blocked', ?, 'integrity', 'receipt_conflict',
		2, 2000000000000, 2000000001000, 0, ?, 'receipt_conflict')`,
		f.consumer, f.version, sequence, f.version,
	); err != nil {
		t.Fatal(err)
	}
}

func (f runtimeOutboxBlockedRetryFixture) input() runtimeOutboxBlockedRetryInput {
	return runtimeOutboxBlockedRetryInput{
		ConsumerName: f.consumer, Sequence: f.first, BlockedClaimVersion: f.version,
		ReasonCode: "operator_verified_fix", ChangeTicket: "OPS-42",
	}
}

func TestRuntimeOutboxBlockedRetrySucceedsAtomicallyAndExactReplayIsIdempotent(t *testing.T) {
	fixture := newRuntimeOutboxBlockedRetryFixture(t)
	input := fixture.input()
	var first runtimeOutboxBlockedRetryResponse
	requestJSON(t, fixture.handler, http.MethodPost, runtimeOutboxBlockedRetryPath, input,
		fixture.cookie, "", http.StatusOK, &first)
	if !first.Retried || first.Replayed || first.ConsumerName != fixture.consumer || first.Sequence != fixture.first {
		t.Fatalf("first retry response = %+v", first)
	}
	checkpoint := readRuntimeOutboxBlockedRetryCheckpointForTest(t, fixture.app, fixture.consumer)
	if checkpoint.LastSequence != 0 || checkpoint.ClaimVersion != fixture.version ||
		!runtimeOutboxCheckpointIsExactRetryPostState(checkpoint, input) {
		t.Fatalf("retry advanced or corrupted checkpoint: %+v", checkpoint)
	}

	var actorID int64
	var statusCode int
	var actorUsername, actorRole, action, method, path, targetType, targetID, requestBody string
	if err := fixture.app.db.QueryRow(`SELECT actor_user_id, actor_username, actor_role, action,
		method, path, target_type, target_id, request_body, status_code
		FROM audit_logs WHERE action = ?`,
		runtimeOutboxBlockedRetryAction,
	).Scan(&actorID, &actorUsername, &actorRole, &action, &method, &path,
		&targetType, &targetID, &requestBody, &statusCode); err != nil {
		t.Fatal(err)
	}
	if actorID != fixture.admin.ID || actorUsername != fixture.admin.Username || actorRole != "admin" ||
		action != runtimeOutboxBlockedRetryAction || method != http.MethodPost ||
		path != runtimeOutboxBlockedRetryPath || statusCode != http.StatusOK ||
		targetType != "runtime_outbox_consumer" ||
		targetID != runtimeOutboxBlockedRetryTargetID(input) {
		t.Fatalf("unsafe or wrong retry audit identity: %d/%s/%s/%s/%s/%s/%d/%s/%s",
			actorID, actorUsername, actorRole, action, method, path, statusCode, targetType, targetID)
	}
	var audit runtimeOutboxBlockedRetryAudit
	if err := json.Unmarshal([]byte(requestBody), &audit); err != nil {
		t.Fatal(err)
	}
	if audit.Request != input || audit.OriginalFailureClass != "integrity" ||
		audit.OriginalFailureCode != "receipt_conflict" || audit.OriginalFailureCount != 2 ||
		strings.Contains(requestBody, "last_error") || strings.Contains(requestBody, "payload") {
		t.Fatalf("unsafe retry audit body = %s", requestBody)
	}

	var replay runtimeOutboxBlockedRetryResponse
	requestJSON(t, fixture.handler, http.MethodPost, runtimeOutboxBlockedRetryPath, input,
		fixture.cookie, "", http.StatusOK, &replay)
	if !replay.Retried || !replay.Replayed {
		t.Fatalf("replay response = %+v", replay)
	}
	var auditCount int
	if err := fixture.app.db.QueryRow(`SELECT COUNT(*) FROM audit_logs WHERE action = ?`,
		runtimeOutboxBlockedRetryAction).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("retry audit count = %d, err=%v", auditCount, err)
	}
}

func TestRuntimeOutboxBlockedRetryRequiresRealAuthenticatedAdmin(t *testing.T) {
	fixture := newRuntimeOutboxBlockedRetryFixture(t)
	requestJSON(t, fixture.handler, http.MethodPost, runtimeOutboxBlockedRetryPath, fixture.input(),
		nil, "", http.StatusUnauthorized, nil)

	requestJSON(t, fixture.handler, http.MethodPost, "/api/users", map[string]any{
		"username": "runtime-auditor", "name": "Runtime Auditor", "password": "runtime-auditor-password",
		"role": "readonly_admin", "status": "active", "allowed_group_ids": []string{"a", "b"}, "rpm_limit": 0,
	}, fixture.cookie, "", http.StatusCreated, nil)
	readonlyCookie := loginCookie(t, fixture.handler, "runtime-auditor", "runtime-auditor-password")
	requestJSON(t, fixture.handler, http.MethodPost, runtimeOutboxBlockedRetryPath, fixture.input(),
		readonlyCookie, "", http.StatusForbidden, nil)
	checkpoint := readRuntimeOutboxBlockedRetryCheckpointForTest(t, fixture.app, fixture.consumer)
	if checkpoint.FailureState != "blocked" {
		t.Fatalf("unauthorized retry changed checkpoint: %+v", checkpoint)
	}
}

func TestRuntimeOutboxBlockedRetryRejectsSyntheticDevelopmentAdmin(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	t.Setenv("EXECUTION_RUNTIME_OUTBOX_CONSUMER_NAME", testRuntimeOutboxConsumer)
	a, err := newApp(filepath.Join(t.TempDir(), "runtime-outbox-dev-admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	requestJSON(t, a.routes(), http.MethodPost, runtimeOutboxBlockedRetryPath,
		runtimeOutboxBlockedRetryInput{
			ConsumerName: testRuntimeOutboxConsumer, Sequence: 1, BlockedClaimVersion: 1,
			ReasonCode: "operator_verified_fix",
		}, nil, "", http.StatusForbidden, nil)
}

func TestRuntimeOutboxBlockedRetryRevalidatesAdminInsideTransaction(t *testing.T) {
	fixture := newRuntimeOutboxBlockedRetryFixture(t)
	if _, err := fixture.app.db.Exec(`UPDATE users SET status = 'disabled' WHERE id = ?`, fixture.admin.ID); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.app.retryBlockedRuntimeOutbox(
		context.Background(), fixture.admin, fixture.input(), "127.0.0.1", "test-agent",
	)
	if !errors.Is(err, errRuntimeOutboxBlockedRetryAuth) {
		t.Fatalf("stale administrator error = %v", err)
	}
	checkpoint := readRuntimeOutboxBlockedRetryCheckpointForTest(t, fixture.app, fixture.consumer)
	if checkpoint.FailureState != "blocked" || checkpoint.FailureSequence != fixture.first {
		t.Fatalf("stale administrator changed checkpoint: %+v", checkpoint)
	}
}

func TestRuntimeOutboxBlockedRetryRejectsStaleOrNoncanonicalIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*runtimeOutboxBlockedRetryFixture, *runtimeOutboxBlockedRetryInput)
	}{
		{
			name: "stale blocked claim version",
			mutate: func(_ *runtimeOutboxBlockedRetryFixture, input *runtimeOutboxBlockedRetryInput) {
				input.BlockedClaimVersion++
			},
		},
		{
			name: "nonblocked without exact audit",
			mutate: func(f *runtimeOutboxBlockedRetryFixture, _ *runtimeOutboxBlockedRetryInput) {
				if _, err := f.app.db.Exec(`UPDATE runtime_outbox_consumers SET
					failure_state = 'ready', failure_sequence = 0, failure_class = '', failure_code = '',
					failure_count = 0, first_failed_at = 0, last_failed_at = 0,
					next_attempt_at = 0, blocked_claim_version = 0, last_error = ''
					WHERE consumer_name = ?`, f.consumer); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "blocked failure fingerprint mismatch",
			mutate: func(f *runtimeOutboxBlockedRetryFixture, _ *runtimeOutboxBlockedRetryInput) {
				if _, err := f.app.db.Exec(`UPDATE runtime_outbox_consumers SET
					last_error = 'different_failure' WHERE consumer_name = ?`, f.consumer); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "blocked event is not next",
			mutate: func(f *runtimeOutboxBlockedRetryFixture, input *runtimeOutboxBlockedRetryInput) {
				f.setBlockedCheckpoint(t, f.second)
				input.Sequence = f.second
			},
		},
		{
			name: "checkpoint has active claim",
			mutate: func(f *runtimeOutboxBlockedRetryFixture, _ *runtimeOutboxBlockedRetryInput) {
				if _, err := f.app.db.Exec(`UPDATE runtime_outbox_consumers SET
					claimed_sequence = ?, locked_by = 'worker-1', lease_expires_at = 2000000005000
					WHERE consumer_name = ?`, f.first, f.consumer); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "different configured consumer",
			mutate: func(_ *runtimeOutboxBlockedRetryFixture, input *runtimeOutboxBlockedRetryInput) {
				input.ConsumerName = "another-runtime-consumer"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newRuntimeOutboxBlockedRetryFixture(t)
			input := fixture.input()
			test.mutate(&fixture, &input)
			requestJSON(t, fixture.handler, http.MethodPost, runtimeOutboxBlockedRetryPath, input,
				fixture.cookie, "", http.StatusConflict, nil)
			checkpoint := readRuntimeOutboxBlockedRetryCheckpointForTest(t, fixture.app, fixture.consumer)
			if test.name != "nonblocked without exact audit" &&
				(checkpoint.FailureState != "blocked" || checkpoint.FailureSequence == 0) {
				t.Fatalf("rejected retry changed checkpoint: %+v", checkpoint)
			}
		})
	}
}

func TestRuntimeOutboxBlockedRetryAuditFailureRollsBackCheckpoint(t *testing.T) {
	fixture := newRuntimeOutboxBlockedRetryFixture(t)
	if _, err := fixture.app.db.Exec(`CREATE TRIGGER reject_runtime_outbox_retry_audit
		BEFORE INSERT ON audit_logs
		WHEN NEW.action = 'runtime_outbox.blocked_retry'
		BEGIN SELECT RAISE(ABORT, 'forced audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	requestJSON(t, fixture.handler, http.MethodPost, runtimeOutboxBlockedRetryPath, fixture.input(),
		fixture.cookie, "", http.StatusInternalServerError, nil)
	checkpoint := readRuntimeOutboxBlockedRetryCheckpointForTest(t, fixture.app, fixture.consumer)
	if checkpoint.FailureState != "blocked" || checkpoint.FailureSequence != fixture.first ||
		checkpoint.FailureCount != 2 || checkpoint.BlockedClaimVersion != fixture.version {
		t.Fatalf("audit failure did not roll back checkpoint: %+v", checkpoint)
	}
}

func TestRuntimeOutboxBlockedRetryStrictInputAndUTF8SafeAuditTruncation(t *testing.T) {
	fixture := newRuntimeOutboxBlockedRetryFixture(t)
	for _, body := range []string{
		`{"consumer_name":"sub2api-execution-runtime-v1","sequence":1,"blocked_claim_version":9,"reason_code":"ok","unknown":true}`,
		`{"consumer_name":"sub2api-execution-runtime-v1","consumer_name":"other","sequence":1,"blocked_claim_version":9,"reason_code":"ok"}`,
		`{"consumer_name":"sub2api-execution-runtime-v1","sequence":1,"blocked_claim_version":9,"reason_code":"ok"} {}`,
	} {
		request := httptest.NewRequest(http.MethodPost, runtimeOutboxBlockedRetryPath, bytes.NewBufferString(body))
		request.AddCookie(fixture.cookie)
		response := httptest.NewRecorder()
		fixture.handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("strict input status=%d body=%s", response.Code, response.Body.String())
		}
	}
	value := strings.Repeat("界", 400)
	truncated := truncateRuntimeOutboxAuditValue(value, 1024)
	if len(truncated) > 1024 || !json.Valid([]byte(`"`+truncated+`"`)) {
		t.Fatalf("UTF-8 truncation produced %d invalid bytes", len(truncated))
	}
}

func readRuntimeOutboxBlockedRetryCheckpointForTest(
	t *testing.T,
	a *app,
	consumerName string,
) runtimeOutboxBlockedCheckpoint {
	t.Helper()
	var checkpoint runtimeOutboxBlockedCheckpoint
	if err := a.db.QueryRow(`SELECT last_sequence, claimed_sequence, locked_by, lease_expires_at,
		claim_version, failure_state, failure_sequence, failure_class, failure_code,
		failure_count, first_failed_at, last_failed_at, next_attempt_at,
		blocked_claim_version, last_error
		FROM runtime_outbox_consumers WHERE consumer_name = ?`, consumerName).Scan(
		&checkpoint.LastSequence, &checkpoint.ClaimedSequence, &checkpoint.LockedBy,
		&checkpoint.LeaseExpiresAt, &checkpoint.ClaimVersion, &checkpoint.FailureState,
		&checkpoint.FailureSequence, &checkpoint.FailureClass, &checkpoint.FailureCode,
		&checkpoint.FailureCount, &checkpoint.FirstFailedAt, &checkpoint.LastFailedAt,
		&checkpoint.NextAttemptAt, &checkpoint.BlockedClaimVersion, &checkpoint.LastError,
	); err != nil {
		t.Fatal(err)
	}
	return checkpoint
}
