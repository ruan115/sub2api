package worker

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestProcessConfigAllowsSecureActivationAndValidatesOnboarding(t *testing.T) {
	publicKey := ed25519.PublicKey(strings.Repeat("k", ed25519.PublicKeySize))
	values := map[string]string{
		"EXECUTION_EPOCH":                 "7",
		"EXECUTION_TICKET_PUBLIC_KEY":     base64.RawStdEncoding.EncodeToString(publicKey),
		"EXECUTION_UPSTREAM_BASE_URL":     "https://api.anthropic.com",
		"EXECUTION_ALLOW_FAKE_ACTIVATION": "false",
		"EXECUTION_LISTEN_ADDRESS":        "0.0.0.0:8093",
		"EXECUTION_ACCOUNT_HASH":          "95a7c9f1f7654af7a836061a6561b839",
		"EXECUTION_SLOT_ID":               "slot-10380",
		"EXECUTION_NODE_ID":               "srv74",
		"EXECUTION_IMAGE_DIGEST":          "sha256:" + strings.Repeat("a", 64),
	}
	config, err := LoadProcessConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	if config.AllowFakeActivation || config.Onboarding.TokenURL == "" {
		t.Fatalf("secure process config = %+v", config)
	}
	config.Onboarding.TokenURL = "http://public.example.test/token"
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "onboarding") {
		t.Fatalf("unsafe onboarding URL validation error = %v", err)
	}
}

func TestApplyActiveCredentialBuildsOnlyCanonicalUpstreamAuth(t *testing.T) {
	for name, testCase := range map[string]struct {
		active        ActiveCredential
		authorization string
		apiKey        string
	}{
		"oauth": {
			active:        ActiveCredential{VersionID: "version-oauth", AuthType: AuthTypeOAuth, CredentialJSON: []byte(`{"access_token":"oauth-secret","refresh_token":"refresh-secret"}`)},
			authorization: "Bearer oauth-secret",
		},
		"setup token": {
			active:        ActiveCredential{VersionID: "version-setup", AuthType: AuthTypeSetupToken, CredentialJSON: []byte(`{"access_token":"setup-secret"}`)},
			authorization: "Bearer setup-secret",
		},
		"api key": {
			active: ActiveCredential{VersionID: "version-api", AuthType: AuthTypeAPIKey, CredentialJSON: []byte(`{"api_key":"api-secret"}`)},
			apiKey: "api-secret",
		},
	} {
		t.Run(name, func(t *testing.T) {
			headers := make(http.Header)
			headers.Set("Authorization", "Bearer downstream-must-not-survive")
			headers.Set("x-api-key", "downstream-must-not-survive")
			if err := applyActiveCredential(headers, testCase.active); err != nil {
				t.Fatal(err)
			}
			if headers.Get("Authorization") != testCase.authorization || headers.Get("x-api-key") != testCase.apiKey || headers.Get("anthropic-version") != "2023-06-01" {
				t.Fatalf("upstream headers = %v", headers)
			}
		})
	}
	for _, active := range []ActiveCredential{
		{VersionID: "version-mixed", AuthType: AuthTypeOAuth, CredentialJSON: []byte(`{"access_token":"oauth","api_key":"api"}`)},
		{VersionID: "version-unknown", AuthType: AuthTypeOAuth, CredentialJSON: []byte(`{"access_token":"oauth","unexpected":"value"}`)},
		{VersionID: " bad-version", AuthType: AuthTypeAPIKey, CredentialJSON: []byte(`{"api_key":"api"}`)},
		{VersionID: "version-trailing", AuthType: AuthTypeAPIKey, CredentialJSON: []byte(`{"api_key":"api"}{}`)},
	} {
		if err := applyActiveCredential(make(http.Header), active); err == nil || strings.Contains(err.Error(), "oauth") || strings.Contains(err.Error(), "api") {
			t.Fatalf("invalid active credential error = %v", err)
		}
	}
}

func TestSecureProcessConfigCanUseLoopbackOnboardingFixture(t *testing.T) {
	baseURL, _ := url.Parse("https://api.anthropic.com")
	config := ProcessConfig{
		ListenAddress:   "127.0.0.1:8093",
		Identity:        Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", NodeID: "srv74", Epoch: 1},
		TicketPublicKey: ed25519.PublicKey(strings.Repeat("p", ed25519.PublicKeySize)),
		UpstreamBaseURL: baseURL, ImageDigest: "sha256:" + strings.Repeat("b", 64),
		Onboarding: DefaultOnboardingConfig(),
	}
	config.Onboarding.OrganizationsURL = "http://127.0.0.1:18081/organizations"
	config.Onboarding.SessionAuthorizeBaseURL = "http://127.0.0.1:18081/oauth"
	config.Onboarding.TokenURL = "http://127.0.0.1:18081/token"
	config.Onboarding.ProfileURL = "http://127.0.0.1:18081/profile"
	config.Onboarding.APIKeyValidationURL = "http://127.0.0.1:18081/models"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
}
