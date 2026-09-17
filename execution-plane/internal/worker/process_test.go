package worker

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker/fixedtransport"
)

func TestProcessConfigAllowsSecureActivationAndValidatesOnboarding(t *testing.T) {
	values := processConfigEnvironment()
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

func processConfigEnvironment() map[string]string {
	publicKey := ed25519.PublicKey(strings.Repeat("k", ed25519.PublicKeySize))
	return map[string]string{
		"EXECUTION_RUNTIME_GENERATION":    "11",
		"EXECUTION_IDENTITY_DIRECTORY":    "/home/worker/identity",
		"EXECUTION_RUNTIME_TRUST_FILE":    "/etc/execution/runtime-ca.pem",
		"EXECUTION_EPOCH":                 "7",
		"EXECUTION_TICKET_PUBLIC_KEY":     base64.RawStdEncoding.EncodeToString(publicKey),
		"EXECUTION_UPSTREAM_BASE_URL":     "https://api.anthropic.com",
		"EXECUTION_EGRESS_PROXY_URL":      "http://host-agent.execution.internal:8094",
		"EXECUTION_ALLOW_FAKE_ACTIVATION": "false",
		"EXECUTION_LISTEN_ADDRESS":        "0.0.0.0:8093",
		"EXECUTION_ACCOUNT_HASH":          "95a7c9f1f7654af7a836061a6561b839",
		"EXECUTION_SLOT_ID":               "slot-10380",
		"EXECUTION_NODE_ID":               "srv74",
		"EXECUTION_IMAGE_DIGEST":          "sha256:" + strings.Repeat("a", 64),
	}
}

func TestProcessConfigRequiresExplicitProxyAndSecureOrigin(t *testing.T) {
	for _, test := range []struct{ name, key, value string }{
		{"missing explicit proxy", "EXECUTION_EGRESS_PROXY_URL", ""},
		{"plaintext production origin", "EXECUTION_UPSTREAM_BASE_URL", "http://api.anthropic.com"},
		{"plaintext loopback onboarding", "EXECUTION_ONBOARDING_TOKEN_URL", "http://127.0.0.1:18081/token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			values := processConfigEnvironment()
			values[test.key] = test.value
			if _, err := LoadProcessConfig(func(name string) string { return values[name] }); err == nil {
				t.Fatal("unsafe production configuration was accepted")
			}
		})
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

func TestProcessRejectsPlaintextOnboardingButLibraryFixtureRemainsAvailable(t *testing.T) {
	baseURL, _ := url.Parse("https://api.anthropic.com")
	config := ProcessConfig{
		RuntimeGeneration: 11, IdentityDirectory: "/home/worker/identity", RuntimeTrustFile: "/etc/execution/runtime-ca.pem",
		ListenAddress:   "127.0.0.1:8093",
		Identity:        Identity{AccountID: "95a7c9f1f7654af7a836061a6561b839", SlotID: "slot-1", NodeID: "srv74", Epoch: 1},
		TicketPublicKey: ed25519.PublicKey(strings.Repeat("p", ed25519.PublicKeySize)),
		UpstreamBaseURL: baseURL, ImageDigest: "sha256:" + strings.Repeat("b", 64),
		EgressProxyURL: "http://host-agent.execution.internal:8094", Onboarding: DefaultOnboardingConfig(),
	}
	config.Onboarding.OrganizationsURL = "http://127.0.0.1:18081/organizations"
	config.Onboarding.SessionAuthorizeBaseURL = "http://127.0.0.1:18081/oauth"
	config.Onboarding.TokenURL = "http://127.0.0.1:18081/token"
	config.Onboarding.ProfileURL = "http://127.0.0.1:18081/profile"
	config.Onboarding.APIKeyValidationURL = "http://127.0.0.1:18081/models"
	if err := config.Validate(); err == nil {
		t.Fatal("production process accepted plaintext onboarding")
	}
	if _, err := NewOnboarder(config.Onboarding); err != nil {
		t.Fatalf("standalone library's explicit loopback fixture was broken: %v", err)
	}
	config.AllowFakeActivation = true
	if err := config.Validate(); err != nil {
		t.Fatalf("explicit fake process configuration failed: %v", err)
	}
}

func TestProcessURLsRequireStrictHTTPSOriginsAndEndpoints(t *testing.T) {
	for _, raw := range []string{
		"http://origin.example.test", "ftp://origin.example.test", "https:origin.example.test", "https:///missing-host",
		(&url.URL{Scheme: "https", Host: "origin.example.test", User: url.UserPassword("synthetic-user", "synthetic-password")}).String(),
		"https://origin.example.test/path", "https://origin.example.test/?", "https://origin.example.test/#",
		"https://origin.example.test:0", "https://origin.example.test:65536", "https://origin.example.test:", "https://origin.example.test:0443",
		"https://origin.example.test\\other", "https://origin.example.test/%2f", "https://origin.example.test ",
	} {
		t.Run(raw, func(t *testing.T) {
			values := processConfigEnvironment()
			values["EXECUTION_UPSTREAM_BASE_URL"] = raw
			if _, err := LoadProcessConfig(func(name string) string { return values[name] }); err == nil {
				t.Fatal("unsafe origin accepted")
			}
		})
	}
	for _, name := range []string{
		"EXECUTION_ONBOARDING_ORGANIZATIONS_URL", "EXECUTION_ONBOARDING_SESSION_AUTHORIZE_BASE_URL", "EXECUTION_ONBOARDING_TOKEN_URL",
		"EXECUTION_ONBOARDING_PROFILE_URL", "EXECUTION_ONBOARDING_API_KEY_VALIDATION_URL",
	} {
		for _, raw := range []string{"http://127.0.0.1/token",
			(&url.URL{Scheme: "https", Host: "origin.example.test", Path: "/token", User: url.UserPassword("synthetic-user", "synthetic-password")}).String(),
			"https://origin.example.test:0/token", "https://origin.example.test/token?", "https://origin.example.test/token#"} {
			t.Run(name+"/"+raw, func(t *testing.T) {
				values := processConfigEnvironment()
				values[name] = raw
				if _, err := LoadProcessConfig(func(name string) string { return values[name] }); err == nil {
					t.Fatal("unsafe onboarding endpoint accepted")
				}
			})
		}
	}
	for _, raw := range []string{"https://origin.example.test", "https://origin.example.test/", "https://127.0.0.1:8443", "https://[::1]:8443"} {
		values := processConfigEnvironment()
		values["EXECUTION_UPSTREAM_BASE_URL"] = raw
		if _, err := LoadProcessConfig(func(name string) string { return values[name] }); err != nil {
			t.Fatalf("valid HTTPS origin %q rejected: %v", raw, err)
		}
	}
}

func TestFakeProcessStillRequiresExplicitProxyAndHTTPProtocol(t *testing.T) {
	values := processConfigEnvironment()
	values["EXECUTION_ALLOW_FAKE_ACTIVATION"] = "true"
	values["EXECUTION_UPSTREAM_BASE_URL"] = "http://fake.anthropic.local:8080"
	config, err := LoadProcessConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	config.EgressProxyURL = ""
	if err := config.Validate(); err != fixedtransport.ErrProxyURL {
		t.Fatalf("fake process proxy error = %v", err)
	}
	// Validation must fail before RunProcess attempts to bind its RPC listener.
	config.ListenAddress = "203.0.113.1:8093"
	if err := RunProcess(context.Background(), config, nil); err != fixedtransport.ErrProxyURL {
		t.Fatalf("invalid process reached runtime startup: %v", err)
	}
	values["EXECUTION_UPSTREAM_BASE_URL"] = "ftp://fake.anthropic.local:8080"
	if _, err := LoadProcessConfig(func(name string) string { return values[name] }); err == nil {
		t.Fatal("fake activation enabled a non-HTTP protocol")
	}
}

func TestDirectProcessConfigCannotBypassOriginValidation(t *testing.T) {
	values := processConfigEnvironment()
	config, err := LoadProcessConfig(func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*url.URL){
		"credentials":     func(u *url.URL) { u.User = url.UserPassword("secret", "password") },
		"plaintext":       func(u *url.URL) { u.Scheme = "http" },
		"opaque":          func(u *url.URL) { u.Opaque = "//other.example.test" },
		"path":            func(u *url.URL) { u.Path = "/v1/messages" },
		"encoded path":    func(u *url.URL) { u.RawPath = "/%2f" },
		"query":           func(u *url.URL) { u.RawQuery = "token=secret" },
		"empty query":     func(u *url.URL) { u.ForceQuery = true },
		"fragment":        func(u *url.URL) { u.Fragment = "secret" },
		"raw fragment":    func(u *url.URL) { u.RawFragment = "secret" },
		"bad port":        func(u *url.URL) { u.Host = "origin.example.test:invalid" },
		"empty port":      func(u *url.URL) { u.Host = "origin.example.test:" },
		"empty host":      func(u *url.URL) { u.Host = "" },
		"ambiguous host":  func(u *url.URL) { u.Host = "origin.example.test." },
		"IPv6 zone":       func(u *url.URL) { u.Host = "[fe80::1%lo0]:8443" },
		"invalid bracket": func(u *url.URL) { u.Host = "[origin.example.test]:443" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := config
			base := *config.UpstreamBaseURL
			mutate(&base)
			copy.UpstreamBaseURL = &base
			if err := copy.Validate(); err == nil || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), "token=") {
				t.Fatalf("unsafe direct URL object validation = %v", err)
			}
		})
	}
	config.UpstreamBaseURL = nil
	if err := config.Validate(); err == nil {
		t.Fatal("missing direct origin accepted")
	}
}
