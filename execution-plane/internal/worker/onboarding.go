package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	OnboardingSessionKey       OnboardingSource = "session_key"
	OnboardingOAuthCode        OnboardingSource = "oauth_code"
	OnboardingSetupToken       OnboardingSource = "setup_token"
	OnboardingAPIKey           OnboardingSource = "api_key"
	OnboardingCookie           OnboardingSource = "cookie"
	OnboardingCredentialImport OnboardingSource = "credential_import"

	AuthTypeOAuth      = "oauth"
	AuthTypeSetupToken = "setup_token"
	AuthTypeAPIKey     = "api_key"

	defaultOAuthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	defaultOAuthRedirectURI = "https://platform.claude.com/oauth/code/callback"
	defaultOAuthScope       = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	defaultSetupScope       = "user:inference"
	maxOnboardingMaterial   = 1 << 20
	maxOnboardingResponse   = 512 << 10
	maxOnboardingToken      = 16 << 10
	maxOnboardingCookie     = 64 << 10
	maxOnboardingCode       = 8 << 10
	onboardingWireHeader    = 14
)

type OnboardingSource string

var onboardingWireMagic = [6]byte{'S', '2', 'O', 'B', '0', '1'}

type OnboardingInput struct {
	Source    OnboardingSource
	AuthType  string
	Secret    []byte
	Auxiliary []byte
}

func (i OnboardingInput) String() string {
	return fmt.Sprintf("OnboardingInput{Source:%q AuthType:%q Secret:[REDACTED] Auxiliary:[REDACTED]}", i.Source, i.AuthType)
}

func (i OnboardingInput) GoString() string { return i.String() }

func (i OnboardingInput) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Source   OnboardingSource `json:"source"`
		AuthType string           `json:"auth_type"`
	}{i.Source, i.AuthType})
}

func (i *OnboardingInput) Destroy() {
	if i == nil {
		return
	}
	zero(i.Secret)
	zero(i.Auxiliary)
	i.Secret = nil
	i.Auxiliary = nil
}

func (i OnboardingInput) Validate() error {
	if len(i.Secret) == 0 || len(i.Secret) > maxOnboardingMaterial || !utf8.Valid(i.Secret) || bytes.ContainsAny(i.Secret, "\x00\r\n") {
		return errors.New("onboarding secret is invalid")
	}
	if len(i.Auxiliary) > maxOnboardingMaterial || !utf8.Valid(i.Auxiliary) || bytes.ContainsAny(i.Auxiliary, "\x00\r\n") {
		return errors.New("onboarding auxiliary material is invalid")
	}
	switch i.Source {
	case OnboardingSessionKey, OnboardingCookie:
		if i.AuthType != AuthTypeOAuth && i.AuthType != AuthTypeSetupToken || len(i.Auxiliary) != 0 {
			return errors.New("session onboarding type is invalid")
		}
		limit := maxOnboardingToken
		if i.Source == OnboardingCookie {
			limit = maxOnboardingCookie
		}
		if len(i.Secret) > limit {
			return errors.New("session onboarding material is too large")
		}
	case OnboardingOAuthCode:
		if i.AuthType != AuthTypeOAuth && i.AuthType != AuthTypeSetupToken || len(i.Auxiliary) == 0 ||
			len(i.Secret) > maxOnboardingCode || len(i.Auxiliary) > maxOnboardingCode {
			return errors.New("OAuth onboarding material is invalid")
		}
	case OnboardingSetupToken:
		if i.AuthType != AuthTypeSetupToken || len(i.Auxiliary) != 0 || len(i.Secret) > maxOnboardingToken {
			return errors.New("setup token onboarding type is invalid")
		}
	case OnboardingAPIKey:
		if i.AuthType != AuthTypeAPIKey || len(i.Auxiliary) != 0 || len(i.Secret) > maxOnboardingToken {
			return errors.New("API key onboarding type is invalid")
		}
	case OnboardingCredentialImport:
		if i.AuthType != AuthTypeOAuth && i.AuthType != AuthTypeSetupToken && i.AuthType != AuthTypeAPIKey || len(i.Auxiliary) != 0 {
			return errors.New("credential import type is invalid")
		}
	default:
		return errors.New("onboarding source is unsupported")
	}
	return nil
}

// EncodeOnboardingInput produces the plaintext payload that is immediately
// sealed to a worker transport key. It is deliberately not JSON so secret
// bytes do not acquire additional immutable string copies during decoding.
func EncodeOnboardingInput(input OnboardingInput) ([]byte, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}
	source, authType, ok := onboardingWireCodes(input.Source, input.AuthType)
	if !ok {
		return nil, errors.New("onboarding input cannot be encoded")
	}
	size := onboardingWireHeader + len(input.Secret) + len(input.Auxiliary)
	if size > maxOnboardingMaterial+onboardingWireHeader {
		return nil, errors.New("onboarding input is too large")
	}
	payload := make([]byte, size)
	copy(payload, onboardingWireMagic[:])
	payload[6], payload[7] = source, authType
	binary.BigEndian.PutUint32(payload[8:12], uint32(len(input.Secret)))
	binary.BigEndian.PutUint16(payload[12:14], uint16(len(input.Auxiliary)))
	copy(payload[onboardingWireHeader:], input.Secret)
	copy(payload[onboardingWireHeader+len(input.Secret):], input.Auxiliary)
	return payload, nil
}

func DecodeOnboardingInput(payload []byte) (OnboardingInput, error) {
	if len(payload) < onboardingWireHeader || len(payload) > maxOnboardingMaterial+onboardingWireHeader ||
		!bytes.Equal(payload[:6], onboardingWireMagic[:]) {
		return OnboardingInput{}, errors.New("onboarding payload is invalid")
	}
	source, authType, ok := onboardingWireValues(payload[6], payload[7])
	if !ok {
		return OnboardingInput{}, errors.New("onboarding payload is invalid")
	}
	secretLength := int(binary.BigEndian.Uint32(payload[8:12]))
	auxiliaryLength := int(binary.BigEndian.Uint16(payload[12:14]))
	if secretLength <= 0 || onboardingWireHeader+secretLength+auxiliaryLength != len(payload) {
		return OnboardingInput{}, errors.New("onboarding payload is invalid")
	}
	input := OnboardingInput{
		Source: source, AuthType: authType,
		Secret:    append([]byte(nil), payload[onboardingWireHeader:onboardingWireHeader+secretLength]...),
		Auxiliary: append([]byte(nil), payload[onboardingWireHeader+secretLength:]...),
	}
	if err := input.Validate(); err != nil {
		input.Destroy()
		return OnboardingInput{}, errors.New("onboarding payload is invalid")
	}
	return input, nil
}

func onboardingWireCodes(source OnboardingSource, authType string) (byte, byte, bool) {
	sourceCode := map[OnboardingSource]byte{
		OnboardingSessionKey: 1, OnboardingOAuthCode: 2, OnboardingSetupToken: 3,
		OnboardingAPIKey: 4, OnboardingCookie: 5, OnboardingCredentialImport: 6,
	}[source]
	authCode := map[string]byte{AuthTypeOAuth: 1, AuthTypeSetupToken: 2, AuthTypeAPIKey: 3}[authType]
	return sourceCode, authCode, sourceCode != 0 && authCode != 0
}

func onboardingWireValues(sourceCode, authCode byte) (OnboardingSource, string, bool) {
	source := map[byte]OnboardingSource{
		1: OnboardingSessionKey, 2: OnboardingOAuthCode, 3: OnboardingSetupToken,
		4: OnboardingAPIKey, 5: OnboardingCookie, 6: OnboardingCredentialImport,
	}[sourceCode]
	authType := map[byte]string{1: AuthTypeOAuth, 2: AuthTypeSetupToken, 3: AuthTypeAPIKey}[authCode]
	return source, authType, source != "" && authType != ""
}

type OnboardingResult struct {
	AuthType         string
	CredentialJSON   []byte
	ExpiresAt        time.Time
	EmailAddress     string
	OrganizationID   string
	AccountID        string
	Scope            string
	SubscriptionType string
	RateLimitTier    string
}

// CredentialProjection contains only identity and plan metadata that may cross
// back to the CCMAX control plane. It never contains tokens, API keys, cookies
// or source onboarding material.
type CredentialProjection struct {
	AuthType          string
	ExpiresAt         time.Time
	EmailAddress      string
	OrganizationID    string
	UpstreamAccountID string
	Scope             string
	SubscriptionType  string
	RateLimitTier     string
}

func (p CredentialProjection) String() string {
	return fmt.Sprintf("CredentialProjection{AuthType:%q ExpiresAt:%s EmailAddress:%q OrganizationID:%q UpstreamAccountID:%q Scope:%q SubscriptionType:%q RateLimitTier:%q}",
		p.AuthType, p.ExpiresAt.UTC().Format(time.RFC3339Nano), p.EmailAddress, p.OrganizationID,
		p.UpstreamAccountID, p.Scope, p.SubscriptionType, p.RateLimitTier)
}

func (p CredentialProjection) GoString() string { return p.String() }

func (p CredentialProjection) MarshalJSON() ([]byte, error) {
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

// ProjectCredential parses the exact normalized credential accepted by the
// oauth_api worker and returns only bounded non-secret fields. Unknown fields,
// mixed auth material and malformed metadata fail closed.
func ProjectCredential(authType string, payload []byte) (CredentialProjection, error) {
	if len(payload) == 0 || len(payload) > maxOnboardingMaterial {
		return CredentialProjection{}, ErrActivationRejected
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var normalized normalizedCredential
	if err := decoder.Decode(&normalized); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return CredentialProjection{}, ErrActivationRejected
	}
	switch authType {
	case AuthTypeOAuth, AuthTypeSetupToken:
		if strings.TrimSpace(normalized.AccessToken) == "" || normalized.APIKey != "" || strings.ContainsAny(normalized.AccessToken, "\x00\r\n") {
			return CredentialProjection{}, ErrActivationRejected
		}
	case AuthTypeAPIKey:
		if strings.TrimSpace(normalized.APIKey) == "" || normalized.AccessToken != "" || normalized.RefreshToken != "" || strings.ContainsAny(normalized.APIKey, "\x00\r\n") {
			return CredentialProjection{}, ErrActivationRejected
		}
	default:
		return CredentialProjection{}, ErrActivationRejected
	}
	email := strings.ToLower(strings.TrimSpace(normalized.EmailAddress))
	if email != "" && (!safeProjectionValue(email, 320) || strings.Count(email, "@") != 1 || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@")) {
		return CredentialProjection{}, ErrActivationRejected
	}
	for _, value := range []struct {
		text string
		max  int
	}{
		{normalized.OrgUUID, 128}, {normalized.AccountUUID, 128}, {normalized.Scope, 1024},
		{normalized.SubscriptionType, 128}, {normalized.RateLimitTier, 128},
	} {
		if value.text != "" && !safeProjectionValue(value.text, value.max) {
			return CredentialProjection{}, ErrActivationRejected
		}
	}
	projection := CredentialProjection{
		AuthType: authType, EmailAddress: email, OrganizationID: strings.TrimSpace(normalized.OrgUUID),
		UpstreamAccountID: strings.TrimSpace(normalized.AccountUUID), Scope: strings.TrimSpace(normalized.Scope),
		SubscriptionType: strings.TrimSpace(normalized.SubscriptionType), RateLimitTier: strings.TrimSpace(normalized.RateLimitTier),
	}
	if normalized.ExpiresAt > 0 {
		projection.ExpiresAt = time.Unix(normalized.ExpiresAt, 0).UTC()
		if projection.ExpiresAt.Year() < 2020 || projection.ExpiresAt.Year() > 2200 {
			return CredentialProjection{}, ErrActivationRejected
		}
	}
	return projection, nil
}

func safeProjectionValue(value string, maxBytes int) bool {
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

func (r OnboardingResult) String() string {
	return fmt.Sprintf("OnboardingResult{AuthType:%q ExpiresAt:%s EmailAddress:%q OrganizationID:%q AccountID:%q Scope:%q SubscriptionType:%q RateLimitTier:%q CredentialJSON:[REDACTED]}",
		r.AuthType, r.ExpiresAt.UTC().Format(time.RFC3339Nano), r.EmailAddress, r.OrganizationID,
		r.AccountID, r.Scope, r.SubscriptionType, r.RateLimitTier)
}

func (r OnboardingResult) GoString() string { return r.String() }

func (r OnboardingResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AuthType         string    `json:"auth_type"`
		ExpiresAt        time.Time `json:"expires_at,omitempty"`
		EmailAddress     string    `json:"email_address,omitempty"`
		OrganizationID   string    `json:"organization_id,omitempty"`
		AccountID        string    `json:"account_id,omitempty"`
		Scope            string    `json:"scope,omitempty"`
		SubscriptionType string    `json:"subscription_type,omitempty"`
		RateLimitTier    string    `json:"rate_limit_tier,omitempty"`
	}{r.AuthType, r.ExpiresAt, r.EmailAddress, r.OrganizationID, r.AccountID, r.Scope, r.SubscriptionType, r.RateLimitTier})
}

func (r *OnboardingResult) Destroy() {
	if r == nil {
		return
	}
	zero(r.CredentialJSON)
	r.CredentialJSON = nil
}

type OnboardingError struct {
	Stage      string
	ReasonCode string
	StatusCode int
}

func (e *OnboardingError) Error() string {
	if e == nil {
		return "worker onboarding failed"
	}
	if e.StatusCode != 0 {
		return fmt.Sprintf("worker onboarding failed during %s (%s, status %d)", e.Stage, e.ReasonCode, e.StatusCode)
	}
	return fmt.Sprintf("worker onboarding failed during %s (%s)", e.Stage, e.ReasonCode)
}

type OnboardingConfig struct {
	HTTPClient              *http.Client
	OrganizationsURL        string
	SessionAuthorizeBaseURL string
	TokenURL                string
	ProfileURL              string
	APIKeyValidationURL     string
	OAuthClientID           string
	OAuthRedirectURI        string
	OAuthScope              string
	SetupScope              string
	Now                     func() time.Time
	Random                  io.Reader
	SessionChallengeDelays  []time.Duration
}

func DefaultOnboardingConfig() OnboardingConfig {
	return OnboardingConfig{
		HTTPClient:              &http.Client{Timeout: 60 * time.Second},
		OrganizationsURL:        "https://claude.ai/api/organizations",
		SessionAuthorizeBaseURL: "https://claude.ai/v1/oauth",
		TokenURL:                "https://platform.claude.com/v1/oauth/token",
		ProfileURL:              "https://api.anthropic.com/api/oauth/profile",
		APIKeyValidationURL:     "https://api.anthropic.com/v1/models",
		OAuthClientID:           defaultOAuthClientID,
		OAuthRedirectURI:        defaultOAuthRedirectURI,
		OAuthScope:              defaultOAuthScope,
		SetupScope:              defaultSetupScope,
		Now:                     time.Now,
		Random:                  rand.Reader,
		SessionChallengeDelays:  []time.Duration{time.Second, 2 * time.Second, 4 * time.Second},
	}
}

type Onboarder struct {
	config OnboardingConfig
}

func NewOnboarder(config OnboardingConfig) (*Onboarder, error) {
	if config.HTTPClient == nil || config.Now == nil || config.Random == nil || config.HTTPClient.Timeout <= 0 || config.HTTPClient.Timeout > 2*time.Minute {
		return nil, errors.New("onboarding client, clock and random source are required")
	}
	for _, delay := range config.SessionChallengeDelays {
		if delay < 0 || delay > 30*time.Second {
			return nil, errors.New("session challenge retry timing is invalid")
		}
	}
	for name, raw := range map[string]string{
		"organizations": config.OrganizationsURL, "session authorization": config.SessionAuthorizeBaseURL,
		"token": config.TokenURL, "profile": config.ProfileURL, "API key validation": config.APIKeyValidationURL,
	} {
		if err := validateOnboardingURL(raw); err != nil {
			return nil, fmt.Errorf("%s endpoint: %w", name, err)
		}
	}
	for _, value := range []string{config.OAuthClientID, config.OAuthRedirectURI, config.OAuthScope, config.SetupScope} {
		if value == "" || len(value) > 2048 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, errors.New("onboarding OAuth configuration is invalid")
		}
	}
	client := *config.HTTPClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	config.HTTPClient = &client
	return &Onboarder{config: config}, nil
}

func (o *Onboarder) Onboard(ctx context.Context, input OnboardingInput) (OnboardingResult, error) {
	if err := input.Validate(); err != nil {
		return OnboardingResult{}, err
	}
	switch input.Source {
	case OnboardingSessionKey:
		return o.exchangeSessionKey(ctx, input.Secret, input.AuthType)
	case OnboardingCookie:
		sessionKey, err := sessionKeyFromCookie(input.Secret)
		if err != nil {
			return OnboardingResult{}, err
		}
		defer zero(sessionKey)
		return o.exchangeSessionKey(ctx, sessionKey, input.AuthType)
	case OnboardingOAuthCode:
		return o.exchangeCode(ctx, input.Secret, input.Auxiliary, input.AuthType)
	case OnboardingSetupToken:
		return o.validateSetupToken(ctx, input.Secret)
	case OnboardingAPIKey:
		return o.validateAPIKey(ctx, input.Secret)
	case OnboardingCredentialImport:
		return o.validateImport(ctx, input.Secret, input.AuthType)
	default:
		return OnboardingResult{}, errors.New("onboarding source is unsupported")
	}
}

type organizationResponse struct {
	UUID      string  `json:"uuid"`
	RavenType *string `json:"raven_type"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
	Organization *struct {
		UUID             string `json:"uuid"`
		RavenType        string `json:"raven_type,omitempty"`
		OrganizationType string `json:"organization_type,omitempty"`
		SubscriptionType string `json:"subscription_type,omitempty"`
	} `json:"organization,omitempty"`
	Account *struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account,omitempty"`
}

type normalizedCredential struct {
	AccessToken      string `json:"access_token,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	ExpiresIn        int64  `json:"expires_in,omitempty"`
	ExpiresAt        int64  `json:"expires_at,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	Scope            string `json:"scope,omitempty"`
	OrgUUID          string `json:"org_uuid,omitempty"`
	AccountUUID      string `json:"account_uuid,omitempty"`
	EmailAddress     string `json:"email_address,omitempty"`
	SubscriptionType string `json:"subscription_type,omitempty"`
	RateLimitTier    string `json:"rate_limit_tier,omitempty"`
	APIKey           string `json:"api_key,omitempty"`
}

type profileResponse struct {
	Account struct {
		HasClaudeMax bool `json:"has_claude_max"`
		HasClaudePro bool `json:"has_claude_pro"`
	} `json:"account"`
	Organization struct {
		OrganizationType string `json:"organization_type"`
		RateLimitTier    string `json:"rate_limit_tier"`
	} `json:"organization"`
}

func (o *Onboarder) exchangeSessionKey(ctx context.Context, sessionKey []byte, authType string) (OnboardingResult, error) {
	for attempt := 0; ; attempt++ {
		result, err := o.exchangeSessionKeyOnce(ctx, sessionKey, authType)
		var onboardingErr *OnboardingError
		if err == nil || !errors.As(err, &onboardingErr) || onboardingErr.ReasonCode != "proxy_challenge" || attempt >= len(o.config.SessionChallengeDelays) {
			return result, err
		}
		timer := time.NewTimer(o.config.SessionChallengeDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return OnboardingResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (o *Onboarder) exchangeSessionKeyOnce(ctx context.Context, sessionKey []byte, authType string) (OnboardingResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, o.config.OrganizationsURL, nil)
	if err != nil {
		return OnboardingResult{}, onboardingInternal("organization_lookup")
	}
	request.AddCookie(&http.Cookie{Name: "sessionKey", Value: string(sessionKey), Secure: true, HttpOnly: true})
	var organizations []organizationResponse
	if err := o.doJSON(request, &organizations, "organization_lookup"); err != nil {
		return OnboardingResult{}, err
	}
	if len(organizations) == 0 {
		return OnboardingResult{}, &OnboardingError{Stage: "organization_lookup", ReasonCode: "invalid_session"}
	}
	selected := organizations[0]
	for _, organization := range organizations {
		if organization.RavenType != nil && *organization.RavenType == "team" {
			selected = organization
			break
		}
	}
	if strings.TrimSpace(selected.UUID) == "" {
		return OnboardingResult{}, &OnboardingError{Stage: "organization_lookup", ReasonCode: "invalid_session"}
	}
	verifier, err := randomBase64(o.config.Random, 32)
	if err != nil {
		return OnboardingResult{}, onboardingInternal("session_authorize")
	}
	defer zero(verifier)
	state, err := randomBase64(o.config.Random, 32)
	if err != nil {
		return OnboardingResult{}, onboardingInternal("session_authorize")
	}
	defer zero(state)
	challengeHash := sha256.Sum256(verifier)
	scope := o.scope(authType)
	body, err := json.Marshal(map[string]string{
		"response_type": "code", "client_id": o.config.OAuthClientID, "organization_uuid": selected.UUID,
		"redirect_uri": o.config.OAuthRedirectURI, "scope": scope, "state": string(state),
		"code_challenge": base64.RawURLEncoding.EncodeToString(challengeHash[:]), "code_challenge_method": "S256",
	})
	if err != nil {
		return OnboardingResult{}, onboardingInternal("session_authorize")
	}
	defer zero(body)
	endpoint := strings.TrimRight(o.config.SessionAuthorizeBaseURL, "/") + "/" + url.PathEscape(selected.UUID) + "/authorize"
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return OnboardingResult{}, onboardingInternal("session_authorize")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", "https://claude.ai")
	request.Header.Set("Referer", "https://claude.ai/new")
	request.AddCookie(&http.Cookie{Name: "sessionKey", Value: string(sessionKey), Secure: true, HttpOnly: true})
	var authorization struct {
		RedirectURI string `json:"redirect_uri"`
	}
	if err := o.doJSON(request, &authorization, "session_authorize"); err != nil {
		return OnboardingResult{}, err
	}
	redirect, err := url.Parse(authorization.RedirectURI)
	if err != nil || redirect.Query().Get("code") == "" || !sameOAuthCallback(redirect, o.config.OAuthRedirectURI) {
		return OnboardingResult{}, &OnboardingError{Stage: "session_authorize", ReasonCode: "invalid_response"}
	}
	if returnedState := redirect.Query().Get("state"); returnedState != "" && returnedState != string(state) {
		return OnboardingResult{}, &OnboardingError{Stage: "session_authorize", ReasonCode: "state_mismatch"}
	}
	code := []byte(redirect.Query().Get("code"))
	if returnedState := redirect.Query().Get("state"); returnedState != "" {
		code = append(code, '#')
		code = append(code, returnedState...)
	}
	defer zero(code)
	result, err := o.exchangeCode(ctx, code, verifier, authType)
	if err == nil && result.OrganizationID == "" {
		result.OrganizationID = selected.UUID
	}
	return result, err
}

func (o *Onboarder) exchangeCode(ctx context.Context, rawCode, verifier []byte, authType string) (OnboardingResult, error) {
	code, returnedState, _ := bytes.Cut(bytes.TrimSpace(rawCode), []byte{'#'})
	requestBody := map[string]string{
		"code": string(code), "grant_type": "authorization_code", "client_id": o.config.OAuthClientID,
		"redirect_uri": o.config.OAuthRedirectURI, "code_verifier": string(verifier),
	}
	if len(returnedState) != 0 {
		requestBody["state"] = string(returnedState)
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return OnboardingResult{}, onboardingInternal("token_exchange")
	}
	defer zero(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, o.config.TokenURL, bytes.NewReader(body))
	if err != nil {
		return OnboardingResult{}, onboardingInternal("token_exchange")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	var token tokenResponse
	if err := o.doJSON(request, &token, "token_exchange"); err != nil {
		return OnboardingResult{}, err
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return OnboardingResult{}, &OnboardingError{Stage: "token_exchange", ReasonCode: "invalid_response"}
	}
	credential := normalizeToken(o.config.Now().UTC(), token)
	profile, profileErr := o.fetchProfile(ctx, []byte(credential.AccessToken))
	if profileErr == nil {
		applyProfile(&credential, profile)
	}
	return credentialResult(authType, credential)
}

func (o *Onboarder) validateSetupToken(ctx context.Context, token []byte) (OnboardingResult, error) {
	profile, err := o.fetchProfile(ctx, token)
	if err != nil {
		return OnboardingResult{}, err
	}
	credential := normalizedCredential{AccessToken: string(token), TokenType: "Bearer"}
	applyProfile(&credential, profile)
	return credentialResult(AuthTypeSetupToken, credential)
}

func (o *Onboarder) validateAPIKey(ctx context.Context, apiKey []byte) (OnboardingResult, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, o.config.APIKeyValidationURL, nil)
	if err != nil {
		return OnboardingResult{}, onboardingInternal("api_key_validation")
	}
	request.Header.Set("X-Api-Key", string(apiKey))
	request.Header.Set("Anthropic-Version", "2023-06-01")
	if err := o.doStatus(request, "api_key_validation"); err != nil {
		return OnboardingResult{}, err
	}
	return credentialResult(AuthTypeAPIKey, normalizedCredential{APIKey: string(apiKey)})
}

func (o *Onboarder) validateImport(ctx context.Context, payload []byte, authType string) (OnboardingResult, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var credential normalizedCredential
	if err := decoder.Decode(&credential); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return OnboardingResult{}, &OnboardingError{Stage: "credential_import", ReasonCode: "invalid_format"}
	}
	if authType == AuthTypeAPIKey {
		if credential.APIKey == "" || credential.AccessToken != "" || credential.RefreshToken != "" {
			return OnboardingResult{}, &OnboardingError{Stage: "credential_import", ReasonCode: "invalid_format"}
		}
		return o.validateAPIKey(ctx, []byte(credential.APIKey))
	}
	if credential.AccessToken == "" || credential.APIKey != "" {
		return OnboardingResult{}, &OnboardingError{Stage: "credential_import", ReasonCode: "invalid_format"}
	}
	profile, err := o.fetchProfile(ctx, []byte(credential.AccessToken))
	if err != nil {
		return OnboardingResult{}, err
	}
	applyProfile(&credential, profile)
	return credentialResult(authType, credential)
}

func (o *Onboarder) fetchProfile(ctx context.Context, token []byte) (profileResponse, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, o.config.ProfileURL, nil)
	if err != nil {
		return profileResponse{}, onboardingInternal("oauth_profile")
	}
	request.Header.Set("Authorization", "Bearer "+string(token))
	request.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	var profile profileResponse
	if err := o.doJSON(request, &profile, "oauth_profile"); err != nil {
		return profileResponse{}, err
	}
	return profile, nil
}

func (o *Onboarder) doJSON(request *http.Request, output any, stage string) error {
	response, payload, err := o.do(request, stage)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		reason := responseReason(response, payload)
		zero(payload)
		return &OnboardingError{Stage: stage, ReasonCode: reason, StatusCode: response.StatusCode}
	}
	defer zero(payload)
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(output); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return &OnboardingError{Stage: stage, ReasonCode: "invalid_response", StatusCode: response.StatusCode}
	}
	return nil
}

func (o *Onboarder) doStatus(request *http.Request, stage string) error {
	response, payload, err := o.do(request, stage)
	if err != nil {
		return err
	}
	reason := responseReason(response, payload)
	zero(payload)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &OnboardingError{Stage: stage, ReasonCode: reason, StatusCode: response.StatusCode}
	}
	return nil
}

func (o *Onboarder) do(request *http.Request, stage string) (*http.Response, []byte, error) {
	request.Header.Set("User-Agent", "sub2api-execution-worker/onboarding-v1")
	response, err := o.config.HTTPClient.Do(request)
	if err != nil {
		return nil, nil, &OnboardingError{Stage: stage, ReasonCode: "upstream_unavailable"}
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxOnboardingResponse+1))
	if err != nil {
		return nil, nil, &OnboardingError{Stage: stage, ReasonCode: "upstream_unavailable"}
	}
	if len(payload) > maxOnboardingResponse {
		zero(payload)
		return nil, nil, &OnboardingError{Stage: stage, ReasonCode: "response_too_large"}
	}
	return response, payload, nil
}

func credentialResult(authType string, credential normalizedCredential) (OnboardingResult, error) {
	encoded, err := json.Marshal(credential)
	if err != nil || len(encoded) == 0 || len(encoded) > maxOnboardingMaterial {
		zero(encoded)
		return OnboardingResult{}, onboardingInternal("credential_normalization")
	}
	result := OnboardingResult{
		AuthType: authType, CredentialJSON: encoded, EmailAddress: credential.EmailAddress,
		OrganizationID: credential.OrgUUID, AccountID: credential.AccountUUID, Scope: credential.Scope,
		SubscriptionType: credential.SubscriptionType, RateLimitTier: credential.RateLimitTier,
	}
	if credential.ExpiresAt > 0 {
		result.ExpiresAt = time.Unix(credential.ExpiresAt, 0).UTC()
	}
	return result, nil
}

func normalizeToken(now time.Time, response tokenResponse) normalizedCredential {
	credential := normalizedCredential{
		AccessToken: response.AccessToken, TokenType: response.TokenType, ExpiresIn: response.ExpiresIn,
		RefreshToken: response.RefreshToken, Scope: response.Scope,
	}
	if response.ExpiresIn > 0 {
		credential.ExpiresAt = now.Unix() + response.ExpiresIn
	}
	if response.Organization != nil {
		credential.OrgUUID = response.Organization.UUID
		credential.SubscriptionType = firstNonEmpty(response.Organization.SubscriptionType, response.Organization.RavenType, response.Organization.OrganizationType)
	}
	if response.Account != nil {
		credential.AccountUUID = response.Account.UUID
		credential.EmailAddress = response.Account.EmailAddress
	}
	return credential
}

func applyProfile(credential *normalizedCredential, profile profileResponse) {
	if credential == nil {
		return
	}
	credential.RateLimitTier = strings.TrimSpace(profile.Organization.RateLimitTier)
	if value := strings.TrimSpace(profile.Organization.OrganizationType); value != "" {
		credential.SubscriptionType = value
	} else if profile.Account.HasClaudeMax {
		credential.SubscriptionType = "max"
	} else if profile.Account.HasClaudePro {
		credential.SubscriptionType = "pro"
	}
}

func sessionKeyFromCookie(raw []byte) ([]byte, error) {
	if bytes.ContainsAny(raw, "\x00\r\n") {
		return nil, &OnboardingError{Stage: "cookie_parse", ReasonCode: "invalid_cookie"}
	}
	request := &http.Request{Header: http.Header{"Cookie": []string{string(raw)}}}
	cookie, err := request.Cookie("sessionKey")
	if err != nil || strings.TrimSpace(cookie.Value) == "" || len(cookie.Value) > maxOnboardingMaterial {
		return nil, &OnboardingError{Stage: "cookie_parse", ReasonCode: "invalid_cookie"}
	}
	return []byte(cookie.Value), nil
}

func randomBase64(random io.Reader, size int) ([]byte, error) {
	buffer := make([]byte, size)
	if _, err := io.ReadFull(random, buffer); err != nil {
		zero(buffer)
		return nil, err
	}
	encoded := make([]byte, base64.RawURLEncoding.EncodedLen(len(buffer)))
	base64.RawURLEncoding.Encode(encoded, buffer)
	zero(buffer)
	return encoded, nil
}

func validateOnboardingURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("URL is invalid")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" {
		ip := net.ParseIP(parsed.Hostname())
		if ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("URL must use HTTPS except for a literal loopback test endpoint")
}

func statusReason(statusCode int) string {
	switch statusCode {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_rejected"
	case http.StatusTooManyRequests:
		return "rate_limited"
	default:
		if statusCode >= 500 {
			return "upstream_unavailable"
		}
		return "upstream_rejected"
	}
}

func responseReason(response *http.Response, payload []byte) string {
	if response == nil {
		return "upstream_unavailable"
	}
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("CF-Mitigated")), "challenge") {
		return "proxy_challenge"
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	body := bytes.ToLower(payload)
	defer zero(body)
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("Server")), "cloudflare") &&
		strings.Contains(contentType, "text/html") &&
		(bytes.Contains(body, []byte("just a moment")) || bytes.Contains(body, []byte("challenge-platform"))) {
		return "proxy_challenge"
	}
	return statusReason(response.StatusCode)
}

func sameOAuthCallback(actual *url.URL, expectedRaw string) bool {
	expected, err := url.Parse(expectedRaw)
	return err == nil && actual != nil && actual.Scheme == expected.Scheme && actual.Host == expected.Host &&
		actual.EscapedPath() == expected.EscapedPath() && actual.User == nil && actual.Fragment == ""
}

func onboardingInternal(stage string) error {
	return &OnboardingError{Stage: stage, ReasonCode: "internal_error"}
}

func (o *Onboarder) scope(authType string) string {
	if authType == AuthTypeSetupToken {
		return o.config.SetupScope
	}
	return o.config.OAuthScope
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
