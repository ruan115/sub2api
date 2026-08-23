package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestUsageSnapshotsMaskedAccountSKAndSurvivesCallingKeyDeletion(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "true")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()

	account := createGatewayTestAccount(t, a, handler, "account-sk-attribution", "https://example.test", 1, nil, map[string]any{
		"access_token": "access-token",
	})
	secret := "sk-ant-sid02-this-is-a-source-session-key-ABCDEF"
	hint := sourceSKHint(secret)
	if hint == "" || strings.Contains(hint, secret) {
		t.Fatalf("unsafe source SK hint %q", hint)
	}
	if _, err := a.db.Exec(`UPDATE accounts SET source_sk_hint = ? WHERE id = ?`, hint, account.ID); err != nil {
		t.Fatal(err)
	}

	key := createGatewayTestKey(t, handler)
	created, wasCreated, err := a.recordUsage(usageInput{
		UserID:       key.UserID,
		APIKeyID:     key.ID,
		RequestID:    "account-sk-snapshot",
		GroupID:      "a",
		AccountID:    account.ID,
		Model:        "claude-test",
		InputTokens:  9,
		OutputTokens: 3,
	})
	if err != nil || !wasCreated {
		t.Fatalf("recordUsage created=%v err=%v", wasCreated, err)
	}
	if created.AccountSKHint != hint {
		t.Fatalf("created account SK hint = %q, want %q", created.AccountSKHint, hint)
	}

	putJSON(t, handler, http.MethodDelete, "/api/api-keys/"+strconv.FormatInt(key.ID, 10), nil, http.StatusNoContent, nil)
	fetched, err := a.getUsageByRequestID("account-sk-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if fetched.AccountSKHint != hint {
		t.Fatalf("persisted account SK hint = %q, want %q", fetched.AccountSKHint, hint)
	}
	if fetched.APIKeyID == nil || *fetched.APIKeyID != key.ID {
		t.Fatalf("calling key attribution was lost: %+v", fetched.APIKeyID)
	}
}
