package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOnboarderSessionKeyAndCookieUseConfiguredProxy(t *testing.T) {
	for _, source := range []OnboardingSource{OnboardingSessionKey, OnboardingCookie} {
		source := source
		t.Run(string(source), func(t *testing.T) {
			t.Parallel()
			fixture := newOnboardingFixture(t)
			defer fixture.Close()
			secret := []byte("session-secret-value")
			if source == OnboardingCookie {
				secret = []byte("other=value; sessionKey=session-secret-value; theme=dark")
			}
			input := OnboardingInput{Source: source, AuthType: AuthTypeOAuth, Secret: secret}
			result, err := fixture.onboarder.Onboard(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			defer result.Destroy()
			credential := decodeNormalizedCredential(t, result.CredentialJSON)
			if result.AuthType != AuthTypeOAuth || credential.AccessToken != "issued-access" ||
				credential.RefreshToken != "issued-refresh" || result.OrganizationID != "org-1" ||
				result.EmailAddress != "owner@example.com" || result.SubscriptionType != "max" || result.RateLimitTier != "tier-4" {
				t.Fatalf("unexpected onboarding result: %+v credential=%+v", result, credential)
			}
			fixture.requirePaths(t, "/organizations", "/oauth/org-1/authorize", "/token", "/profile")
			fixture.requireAllProxied(t)
			for _, serialized := range []string{input.String(), fmt.Sprintf("%+v", input), string(mustJSON(t, input)), result.String(), string(mustJSON(t, result))} {
				for _, secretValue := range []string{"session-secret-value", "issued-access", "issued-refresh"} {
					if strings.Contains(serialized, secretValue) {
						t.Fatalf("serialization leaked %q: %s", secretValue, serialized)
					}
				}
			}
		})
	}
}

func TestOnboarderOAuthCodeSetupTokenAPIKeyAndImports(t *testing.T) {
	tests := []struct {
		name      string
		input     OnboardingInput
		wantAuth  string
		wantPaths []string
		assert    func(*testing.T, normalizedCredential)
	}{
		{
			name: "oauth code", input: OnboardingInput{Source: OnboardingOAuthCode, AuthType: AuthTypeSetupToken, Secret: []byte("manual-code#manual-state"), Auxiliary: []byte("manual-verifier")},
			wantAuth: AuthTypeSetupToken, wantPaths: []string{"/token", "/profile"},
			assert: func(t *testing.T, credential normalizedCredential) {
				if credential.AccessToken != "issued-access" || credential.RefreshToken != "issued-refresh" {
					t.Fatalf("unexpected OAuth credential: %+v", credential)
				}
			},
		},
		{
			name: "setup token", input: OnboardingInput{Source: OnboardingSetupToken, AuthType: AuthTypeSetupToken, Secret: []byte("setup-secret")},
			wantAuth: AuthTypeSetupToken, wantPaths: []string{"/profile"},
			assert: func(t *testing.T, credential normalizedCredential) {
				if credential.AccessToken != "setup-secret" || credential.RefreshToken != "" {
					t.Fatalf("unexpected setup-token credential: %+v", credential)
				}
			},
		},
		{
			name: "api key", input: OnboardingInput{Source: OnboardingAPIKey, AuthType: AuthTypeAPIKey, Secret: []byte("api-secret")},
			wantAuth: AuthTypeAPIKey, wantPaths: []string{"/models"},
			assert: func(t *testing.T, credential normalizedCredential) {
				if credential.APIKey != "api-secret" || credential.AccessToken != "" {
					t.Fatalf("unexpected API-key credential: %+v", credential)
				}
			},
		},
		{
			name: "oauth import", input: OnboardingInput{Source: OnboardingCredentialImport, AuthType: AuthTypeOAuth, Secret: []byte(`{"access_token":"imported-access","refresh_token":"imported-refresh","expires_at":4102444800}`)},
			wantAuth: AuthTypeOAuth, wantPaths: []string{"/profile"},
			assert: func(t *testing.T, credential normalizedCredential) {
				if credential.AccessToken != "imported-access" || credential.RefreshToken != "imported-refresh" {
					t.Fatalf("unexpected imported OAuth credential: %+v", credential)
				}
			},
		},
		{
			name: "api key import", input: OnboardingInput{Source: OnboardingCredentialImport, AuthType: AuthTypeAPIKey, Secret: []byte(`{"api_key":"imported-api"}`)},
			wantAuth: AuthTypeAPIKey, wantPaths: []string{"/models"},
			assert: func(t *testing.T, credential normalizedCredential) {
				if credential.APIKey != "imported-api" {
					t.Fatalf("unexpected imported API key: %+v", credential)
				}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newOnboardingFixture(t)
			defer fixture.Close()
			result, err := fixture.onboarder.Onboard(context.Background(), test.input)
			if err != nil {
				t.Fatal(err)
			}
			defer result.Destroy()
			if result.AuthType != test.wantAuth {
				t.Fatalf("auth type = %q, want %q", result.AuthType, test.wantAuth)
			}
			test.assert(t, decodeNormalizedCredential(t, result.CredentialJSON))
			fixture.requirePaths(t, test.wantPaths...)
			fixture.requireAllProxied(t)
		})
	}
}

func TestOnboarderSanitizesFailuresAndBoundsInputs(t *testing.T) {
	t.Parallel()
	secret := "must-never-appear-in-error"
	proxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(response, `{"error":"`+secret+`"}`)
	}))
	defer proxy.Close()
	config := onboardingTestConfig(t, proxy.URL)
	onboarder, err := NewOnboarder(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = onboarder.Onboard(context.Background(), OnboardingInput{Source: OnboardingAPIKey, AuthType: AuthTypeAPIKey, Secret: []byte(secret)})
	var onboardingErr *OnboardingError
	if !errorsAs(err, &onboardingErr) || onboardingErr.ReasonCode != "authentication_rejected" || strings.Contains(err.Error(), secret) {
		t.Fatalf("unexpected sanitized error: %v", err)
	}

	invalid := []OnboardingInput{
		{},
		{Source: OnboardingSessionKey, AuthType: AuthTypeAPIKey, Secret: []byte("secret")},
		{Source: OnboardingOAuthCode, AuthType: AuthTypeOAuth, Secret: []byte("code")},
		{Source: OnboardingAPIKey, AuthType: AuthTypeAPIKey, Secret: []byte("bad\nkey")},
		{Source: OnboardingSource("unknown"), AuthType: AuthTypeOAuth, Secret: []byte("secret")},
	}
	for _, input := range invalid {
		if _, err := onboarder.Onboard(context.Background(), input); err == nil {
			t.Fatalf("invalid onboarding input succeeded: %+v", input)
		}
	}
	if _, err := NewOnboarder(func() OnboardingConfig {
		bad := config
		bad.TokenURL = "http://example.com/token"
		return bad
	}()); err == nil {
		t.Fatal("non-TLS public onboarding endpoint was accepted")
	}
}

func TestOnboardingDestroyErasesCallerOwnedBuffers(t *testing.T) {
	t.Parallel()
	input := OnboardingInput{Source: OnboardingOAuthCode, AuthType: AuthTypeOAuth, Secret: []byte("code-secret"), Auxiliary: []byte("verifier-secret")}
	secretAlias, auxiliaryAlias := input.Secret, input.Auxiliary
	input.Destroy()
	if !allZero(secretAlias) || !allZero(auxiliaryAlias) || input.Secret != nil || input.Auxiliary != nil {
		t.Fatal("onboarding input destroy did not erase buffers")
	}
	result := OnboardingResult{CredentialJSON: []byte(`{"access_token":"secret"}`)}
	credentialAlias := result.CredentialJSON
	result.Destroy()
	if !allZero(credentialAlias) || result.CredentialJSON != nil {
		t.Fatal("onboarding result destroy did not erase credential buffer")
	}
}

func TestOnboardingBinaryCodecRoundTripAndRejectsTampering(t *testing.T) {
	t.Parallel()
	input := OnboardingInput{
		Source: OnboardingOAuthCode, AuthType: AuthTypeOAuth,
		Secret: []byte("oauth-code#state"), Auxiliary: []byte("pkce-verifier"),
	}
	payload, err := EncodeOnboardingInput(input)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeOnboardingInput(payload)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Destroy()
	if decoded.Source != input.Source || decoded.AuthType != input.AuthType ||
		!bytes.Equal(decoded.Secret, input.Secret) || !bytes.Equal(decoded.Auxiliary, input.Auxiliary) {
		t.Fatalf("decoded onboarding input = %+v", decoded)
	}
	for _, candidate := range [][]byte{
		payload[:5],
		append([]byte("BADMAG"), payload[6:]...),
		func() []byte { changed := append([]byte(nil), payload...); changed[6] = 99; return changed }(),
		append(append([]byte(nil), payload...), 0),
	} {
		if decoded, err := DecodeOnboardingInput(candidate); err == nil {
			decoded.Destroy()
			t.Fatalf("tampered onboarding payload decoded: %x", candidate)
		}
	}
}

func TestOnboarderRetriesOnlyCloudflareChallengeAndRejectsRedirect(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	organizationAttempts := 0
	leakReached := false
	proxy := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if request.URL.Path == "/organizations" {
			organizationAttempts++
			if organizationAttempts == 1 {
				response.Header().Set("CF-Mitigated", "challenge")
				response.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = io.WriteString(response, `[]`)
			return
		}
		if request.URL.Path == "/models" {
			response.Header().Set("Location", "http://127.0.0.2:9/leak")
			response.WriteHeader(http.StatusFound)
			return
		}
		if request.URL.Path == "/leak" {
			leakReached = true
		}
	}))
	defer proxy.Close()
	config := onboardingTestConfig(t, proxy.URL)
	config.SessionChallengeDelays = []time.Duration{0}
	onboarder, err := NewOnboarder(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = onboarder.Onboard(context.Background(), OnboardingInput{Source: OnboardingSessionKey, AuthType: AuthTypeOAuth, Secret: []byte("session")})
	var onboardingErr *OnboardingError
	mu.Lock()
	attempts := organizationAttempts
	mu.Unlock()
	if !errors.As(err, &onboardingErr) || onboardingErr.ReasonCode != "invalid_session" || attempts != 2 {
		t.Fatalf("challenge retry result: attempts=%d err=%v", attempts, err)
	}
	_, err = onboarder.Onboard(context.Background(), OnboardingInput{Source: OnboardingAPIKey, AuthType: AuthTypeAPIKey, Secret: []byte("api-secret")})
	mu.Lock()
	leaked := leakReached
	mu.Unlock()
	if !errors.As(err, &onboardingErr) || onboardingErr.StatusCode != http.StatusFound || leaked {
		t.Fatalf("redirect handling: leak=%t err=%v", leaked, err)
	}
	if !sameOAuthCallback(mustParseURL(t, defaultOAuthRedirectURI+"?code=x"), defaultOAuthRedirectURI) ||
		sameOAuthCallback(mustParseURL(t, "https://attacker.example/callback?code=x"), defaultOAuthRedirectURI) {
		t.Fatal("OAuth callback origin/path validation is incorrect")
	}
}

type onboardingObservation struct {
	path      string
	host      string
	cookie    string
	authorize string
	apiKey    string
	body      []byte
}

type onboardingFixture struct {
	onboarder    *Onboarder
	server       *httptest.Server
	mu           sync.Mutex
	observations []onboardingObservation
}

func newOnboardingFixture(t *testing.T) *onboardingFixture {
	t.Helper()
	fixture := &onboardingFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(request.Body, 64<<10))
		fixture.mu.Lock()
		fixture.observations = append(fixture.observations, onboardingObservation{
			path: request.URL.Path, host: request.URL.Host, cookie: request.Header.Get("Cookie"),
			authorize: request.Header.Get("Authorization"), apiKey: request.Header.Get("X-Api-Key"), body: append([]byte(nil), body...),
		})
		fixture.mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/organizations":
			if !strings.Contains(request.Header.Get("Cookie"), "sessionKey=session-secret-value") {
				http.Error(response, "missing session cookie", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(response, `[{"uuid":"org-1","raven_type":"team"}]`)
		case "/oauth/org-1/authorize":
			if !strings.Contains(request.Header.Get("Cookie"), "sessionKey=session-secret-value") || !bytes.Contains(body, []byte(`"code_challenge"`)) {
				http.Error(response, "bad authorization", http.StatusUnauthorized)
				return
			}
			var authorization map[string]string
			if json.Unmarshal(body, &authorization) != nil || authorization["state"] == "" {
				http.Error(response, "bad state", http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprintf(response, `{"redirect_uri":"https://platform.claude.com/oauth/code/callback?code=issued-code&state=%s"}`, url.QueryEscape(authorization["state"]))
		case "/token":
			if !bytes.Contains(body, []byte(`"code":"`)) || !bytes.Contains(body, []byte(`"code_verifier":"`)) {
				http.Error(response, "bad token exchange", http.StatusBadRequest)
				return
			}
			_, _ = io.WriteString(response, `{"access_token":"issued-access","refresh_token":"issued-refresh","token_type":"Bearer","expires_in":3600,"scope":"user:inference","organization":{"uuid":"org-1","subscription_type":"pro"},"account":{"uuid":"account-1","email_address":"owner@example.com"}}`)
		case "/profile":
			if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
				http.Error(response, "missing bearer", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(response, `{"account":{"has_claude_max":true},"organization":{"organization_type":"max","rate_limit_tier":"tier-4"}}`)
		case "/models":
			if request.Header.Get("X-Api-Key") == "" {
				http.Error(response, "missing api key", http.StatusUnauthorized)
				return
			}
			_, _ = io.WriteString(response, `{"data":[]}`)
		default:
			http.Error(response, "unexpected path", http.StatusNotFound)
		}
	}))
	config := onboardingTestConfig(t, fixture.server.URL)
	var err error
	fixture.onboarder, err = NewOnboarder(config)
	if err != nil {
		fixture.server.Close()
		t.Fatal(err)
	}
	return fixture
}

func onboardingTestConfig(t *testing.T, proxyURL string) OnboardingConfig {
	t.Helper()
	parsedProxy, err := url.Parse(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	target := "http://127.0.0.2:9"
	config := DefaultOnboardingConfig()
	config.HTTPClient = &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(parsedProxy)}, Timeout: 3 * time.Second}
	config.OrganizationsURL = target + "/organizations"
	config.SessionAuthorizeBaseURL = target + "/oauth"
	config.TokenURL = target + "/token"
	config.ProfileURL = target + "/profile"
	config.APIKeyValidationURL = target + "/models"
	config.Now = func() time.Time { return time.Unix(2_000_000_000, 0).UTC() }
	config.Random = bytes.NewReader(bytes.Repeat([]byte{0x5a}, 4096))
	return config
}

func (f *onboardingFixture) Close() { f.server.Close() }

func (f *onboardingFixture) requirePaths(t *testing.T, expected ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	actual := make([]string, 0, len(f.observations))
	for _, observation := range f.observations {
		actual = append(actual, observation.path)
	}
	if strings.Join(actual, ",") != strings.Join(expected, ",") {
		t.Fatalf("onboarding paths = %v, want %v", actual, expected)
	}
}

func (f *onboardingFixture) requireAllProxied(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.observations) == 0 {
		t.Fatal("proxy observed no onboarding requests")
	}
	for _, observation := range f.observations {
		if observation.host != "127.0.0.2:9" {
			t.Fatalf("request did not preserve configured upstream through proxy: %+v", observation)
		}
	}
}

func decodeNormalizedCredential(t *testing.T, payload []byte) normalizedCredential {
	t.Helper()
	var credential normalizedCredential
	if err := json.Unmarshal(payload, &credential); err != nil {
		t.Fatal(err)
	}
	return credential
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}

func allZero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
