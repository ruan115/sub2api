package worker

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestProjectCredentialReturnsOnlySafeIdentityMetadata(t *testing.T) {
	secret := "oauth-projection-secret"
	projection, err := ProjectCredential(AuthTypeOAuth, []byte(`{
		"access_token":"`+secret+`",
		"refresh_token":"refresh-projection-secret",
		"expires_at":2000003600,
		"email_address":" Owner@Example.COM ",
		"org_uuid":"org-10380",
		"account_uuid":"upstream-10380",
		"scope":"user:inference",
		"subscription_type":"max",
		"rate_limit_tier":"tier-1"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if projection.EmailAddress != "owner@example.com" || projection.OrganizationID != "org-10380" ||
		projection.UpstreamAccountID != "upstream-10380" || projection.AuthType != AuthTypeOAuth || projection.ExpiresAt.IsZero() {
		t.Fatalf("projection = %+v", projection)
	}
	for _, serialized := range []string{projection.String(), fmt.Sprintf("%+v", projection), string(mustProjectionJSON(t, projection))} {
		if strings.Contains(serialized, secret) || strings.Contains(serialized, "refresh-projection-secret") {
			t.Fatalf("projection leaked credential material: %s", serialized)
		}
	}
}

func TestProjectCredentialRejectsMixedOrMalformedCredential(t *testing.T) {
	for name, payload := range map[string][]byte{
		"unknown field": []byte(`{"access_token":"secret","unexpected":"value"}`),
		"mixed auth":    []byte(`{"access_token":"secret","api_key":"other"}`),
		"invalid email": []byte(`{"access_token":"secret","email_address":"not-an-email"}`),
		"trailing JSON": []byte(`{"api_key":"secret"}{}`),
	} {
		t.Run(name, func(t *testing.T) {
			authType := AuthTypeOAuth
			if name == "trailing JSON" {
				authType = AuthTypeAPIKey
			}
			if _, err := ProjectCredential(authType, payload); err != ErrActivationRejected {
				t.Fatalf("projection error = %v", err)
			}
		})
	}
}

func TestProjectAPIKeyNeverReturnsKeyMaterial(t *testing.T) {
	projection, err := ProjectCredential(AuthTypeAPIKey, []byte(`{"api_key":"api-projection-secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	payload := string(mustProjectionJSON(t, projection))
	if strings.Contains(payload, "api-projection-secret") || projection.EmailAddress != "" || projection.AuthType != AuthTypeAPIKey {
		t.Fatalf("API key projection = %s", payload)
	}
}

func mustProjectionJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
