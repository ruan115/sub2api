package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/imroc/req/v3"
)

const (
	claudeOAuthClientID      = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthAuthorizeURL  = "https://claude.com/cai/oauth/authorize"
	claudeOAuthTokenURL      = "https://platform.claude.com/v1/oauth/token"
	claudeOAuthRedirectURI   = "https://platform.claude.com/oauth/code/callback"
	claudeOAuthScope         = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	claudeOAuthAPIScope      = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
	claudeOAuthInferenceOnly = "user:inference"
)

var (
	claudeTokenEndpoint           = claudeOAuthTokenURL
	claudeOrganizationsEndpoint   = "https://claude.ai/api/organizations"
	claudeSessionAuthorizeBaseURL = "https://claude.ai/v1/oauth"
	claudeProfileEndpoint         = "https://api.anthropic.com/api/oauth/profile"
	claudeSessionChallengeDelays  = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
)

type oauthSession struct {
	AccountID    int64
	State        string
	CodeVerifier string
	Scope        string
	ProxyURL     string
	CreatedAt    time.Time
}

type oauthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]oauthSession
}

type claudeTokenResponse struct {
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

type claudeTokenInfo struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	ExpiresIn        int64  `json:"expires_in"`
	ExpiresAt        int64  `json:"expires_at"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	Scope            string `json:"scope,omitempty"`
	OrgUUID          string `json:"org_uuid,omitempty"`
	AccountUUID      string `json:"account_uuid,omitempty"`
	EmailAddress     string `json:"email_address,omitempty"`
	SubscriptionType string `json:"subscription_type,omitempty"`
	RateLimitTier    string `json:"rate_limit_tier,omitempty"`
}

type claudeOAuthProfile struct {
	Account struct {
		HasClaudeMax bool `json:"has_claude_max"`
		HasClaudePro bool `json:"has_claude_pro"`
	} `json:"account"`
	Organization struct {
		OrganizationType string `json:"organization_type"`
		RateLimitTier    string `json:"rate_limit_tier"`
	} `json:"organization"`
}

type claudeOrganization struct {
	UUID      string  `json:"uuid"`
	RavenType *string `json:"raven_type"`
}

type claudeSessionUpstreamError struct {
	Stage               string
	Status              int
	CloudflareChallenge bool
	InvalidSession      bool
}

func (e *claudeSessionUpstreamError) Error() string {
	if e.CloudflareChallenge {
		return fmt.Sprintf("Claude authorization proxy was blocked by Cloudflare challenge during %s (status %d)", e.Stage, e.Status)
	}
	if e.InvalidSession {
		return fmt.Sprintf("Claude Session Key is invalid or has no organization (status %d)", e.Status)
	}
	return fmt.Sprintf("Claude Session Key authorization failed during %s (status %d)", e.Stage, e.Status)
}

func isClaudeSessionProxyChallenge(err error) bool {
	var upstreamErr *claudeSessionUpstreamError
	return errors.As(err, &upstreamErr) && upstreamErr.CloudflareChallenge
}

func isClaudeSessionInvalid(err error) bool {
	var upstreamErr *claudeSessionUpstreamError
	return errors.As(err, &upstreamErr) && upstreamErr.InvalidSession
}

func isCloudflareChallengeResponse(response *req.Response) bool {
	if response == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(response.Header.Get("cf-mitigated")), "challenge") {
		return true
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	body := strings.ToLower(response.String())
	return strings.EqualFold(strings.TrimSpace(response.Header.Get("Server")), "cloudflare") &&
		strings.Contains(contentType, "text/html") &&
		(strings.Contains(body, "just a moment") || strings.Contains(body, "challenge-platform"))
}

type claudeRefreshError struct {
	Status int
	Detail string
}

func (e *claudeRefreshError) Error() string {
	message := fmt.Sprintf("Claude token refresh failed (status %d)", e.Status)
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

func (e *claudeRefreshError) permanent() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

func claudeRefreshFailureReason(err error) string {
	reason := strings.TrimSpace(err.Error())
	var refreshErr *claudeRefreshError
	if !errors.As(err, &refreshErr) {
		return reason
	}
	detail := strings.ToLower(strings.TrimSpace(refreshErr.Detail))
	if refreshErr.Status == http.StatusUnauthorized ||
		strings.Contains(detail, "invalid_grant") ||
		strings.Contains(detail, "account_on_hold") ||
		strings.Contains(detail, "access token has been revoked") ||
		strings.Contains(detail, "access token was revoked") {
		return "OAuth 401: " + reason
	}
	return reason
}

func claudeRefreshFailureDetail(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(body), &payload) == nil {
		parts := make([]string, 0, 2)
		appendPart := func(value string) {
			value = strings.Join(strings.Fields(value), " ")
			if value == "" {
				return
			}
			for _, existing := range parts {
				if existing == value {
					return
				}
			}
			parts = append(parts, value)
		}
		switch value := payload["error"].(type) {
		case string:
			appendPart(value)
		case map[string]any:
			if errorType, ok := stringObjectValue(value, "type"); ok {
				appendPart(errorType)
			} else if code, ok := stringObjectValue(value, "code"); ok {
				appendPart(code)
			}
			if message, ok := stringObjectValue(value, "message"); ok {
				appendPart(message)
			} else if description, ok := stringObjectValue(value, "error_description"); ok {
				appendPart(description)
			}
		}
		for _, key := range []string{"error_description", "message", "detail"} {
			if value, ok := stringObjectValue(payload, key); ok {
				appendPart(value)
			}
		}
		if len(parts) > 0 {
			body = strings.Join(parts, ": ")
		} else {
			body = ""
		}
	} else if strings.HasPrefix(body, "<") {
		body = "non-JSON response from token endpoint"
	} else {
		body = strings.Join(strings.Fields(body), " ")
	}
	if len(body) > 350 {
		body = body[:350]
	}
	return body
}

func newOAuthSessionStore() *oauthSessionStore {
	return &oauthSessionStore{sessions: map[string]oauthSession{}}
}

func (s *oauthSessionStore) put(id string, session oauthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, item := range s.sessions {
		if time.Since(item.CreatedAt) > 30*time.Minute {
			delete(s.sessions, key)
		}
	}
	s.sessions[id] = session
}

func (s *oauthSessionStore) take(id string, accountID int64) (oauthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[id]
	if !ok || session.AccountID != accountID || time.Since(session.CreatedAt) > 30*time.Minute {
		return oauthSession{}, false
	}
	delete(s.sessions, id)
	return session, true
}

func secureBase64(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(buffer), "="), nil
}

func secureHex(size int) (string, error) {
	buffer := make([]byte, size)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func pkceChallenge(verifier string) string {
	hash := sha256.Sum256([]byte(verifier))
	return strings.TrimRight(base64.URLEncoding.EncodeToString(hash[:]), "=")
}

func claudeAuthorizationURL(state, challenge, scope string) string {
	values := url.Values{}
	values.Set("code", "true")
	values.Set("client_id", claudeOAuthClientID)
	values.Set("response_type", "code")
	values.Set("redirect_uri", claudeOAuthRedirectURI)
	values.Set("scope", scope)
	values.Set("code_challenge", challenge)
	values.Set("code_challenge_method", "S256")
	values.Set("state", state)
	return claudeOAuthAuthorizeURL + "?" + values.Encode()
}

func normalizeClaudeAuthMode(mode string) (authType, browserScope, apiScope string, err error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "oauth", "full":
		return "oauth", claudeOAuthScope, claudeOAuthAPIScope, nil
	case "setup_token", "inference", "cookie":
		return "setup_token", claudeOAuthInferenceOnly, claudeOAuthInferenceOnly, nil
	default:
		return "", "", "", errors.New("mode must be oauth or setup_token")
	}
}

func (a *app) accountProxyString(accountID int64) (string, error) {
	var proxyID *int64
	if err := a.db.QueryRow(`SELECT proxy_id FROM accounts WHERE id = ? AND deleted_at IS NULL AND `+legacyExecutionPredicate("accounts"), accountID).Scan(&proxyID); err != nil {
		return "", err
	}
	if proxyID == nil {
		return "", errors.New("CCMAX account must bind an active proxy")
	}
	proxyURL, err := a.proxyURL(*proxyID)
	if err != nil {
		return "", errors.New("CCMAX account proxy is unavailable")
	}
	return proxyURL.String(), nil
}

func claudeReqClient(proxyURL string) (*req.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil, errors.New("CCMAX requests require an account proxy")
	}
	client := req.C().SetTimeout(60 * time.Second).ImpersonateChrome().SetCookieJar(nil)
	parsed, err := url.ParseRequestURI(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	if isSSHProxyURL(parsed) {
		// req has no notion of an ssh:// proxy, so tunnel at the dial layer.
		client.SetDial(sshProxyDialContext(parsed))
		return client, nil
	}
	client.SetProxyURL(proxyURL)
	return client, nil
}

func (a *app) handleAccountAuthURL(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.requireLegacyCredentialOwner(r.Context(), accountID); err != nil {
		if writeRuntimeCredentialOwnerError(w, err) {
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		writeDBError(w, err)
		return
	}
	var input struct {
		Mode string `json:"mode"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	_, scope, _, err := normalizeClaudeAuthMode(input.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	proxyURL, err := a.accountProxyString(accountID)
	if err != nil {
		a.recordAuthorization(&accountID, nil, "", "oauth_start", false, err.Error(), "", requestIP(r))
		status := http.StatusConflict
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	state, err := secureBase64(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to generate OAuth state")
		return
	}
	verifier, _ := secureBase64(32)
	sessionID, _ := secureHex(16)
	a.oauthSessions.put(sessionID, oauthSession{AccountID: accountID, State: state, CodeVerifier: verifier, Scope: scope, ProxyURL: proxyURL, CreatedAt: time.Now()})
	writeJSON(w, http.StatusOK, map[string]string{"auth_url": claudeAuthorizationURL(state, pkceChallenge(verifier), scope), "session_id": sessionID})
}

func (a *app) handleAccountOAuthExchange(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.requireLegacyCredentialOwner(r.Context(), accountID); err != nil {
		if writeRuntimeCredentialOwnerError(w, err) {
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		writeDBError(w, err)
		return
	}
	var input struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	leaseOwner, err := a.acquireAccountTokenLease(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer a.releaseAccountTokenLease(accountID, leaseOwner)
	session, ok := a.oauthSessions.take(strings.TrimSpace(input.SessionID), accountID)
	if !ok {
		a.recordAuthorization(&accountID, nil, "", "oauth", false, "OAuth session not found or expired", "", requestIP(r))
		writeError(w, http.StatusBadRequest, "OAuth session not found or expired")
		return
	}
	proxyURL, err := a.accountProxyString(accountID)
	if err != nil {
		a.recordAuthorization(&accountID, nil, "", "oauth", false, err.Error(), "", requestIP(r))
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if proxyURL != session.ProxyURL {
		message := "account proxy changed during OAuth; generate a new authorization link"
		a.recordAuthorization(&accountID, nil, "", "oauth", false, message, "", requestIP(r))
		writeError(w, http.StatusConflict, message)
		return
	}
	token, err := exchangeClaudeCode(r.Context(), input.Code, session.CodeVerifier, proxyURL)
	if err != nil {
		a.recordAuthorization(&accountID, nil, "", "oauth", false, err.Error(), "", requestIP(r))
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	authType := "oauth"
	if session.Scope == claudeOAuthInferenceOnly {
		authType = "setup_token"
	}
	if err := a.saveAuthorizedClaudeToken(accountID, authType, token, leaseOwner); err != nil {
		a.recordAuthorization(&accountID, nil, "", "oauth", false, err.Error(), token.SubscriptionType, requestIP(r))
		if writeRuntimeCredentialOwnerError(w, err) {
			return
		}
		writeDBError(w, err)
		return
	}
	a.recordAuthorization(&accountID, nil, "", "oauth", true, "authorization succeeded", token.SubscriptionType, requestIP(r))
	writeJSON(w, http.StatusOK, tokenMetadata(token))
}

func (a *app) handleAccountSessionAuth(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := a.requireLegacyCredentialOwner(r.Context(), accountID); err != nil {
		if writeRuntimeCredentialOwnerError(w, err) {
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "account not found")
			return
		}
		writeDBError(w, err)
		return
	}
	var input struct {
		SessionKey string `json:"session_key"`
		Mode       string `json:"mode"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.SessionKey) == "" {
		writeError(w, http.StatusBadRequest, "Claude Session Key is required")
		return
	}
	authType, _, apiScope, err := normalizeClaudeAuthMode(input.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	leaseOwner, err := a.acquireAccountTokenLease(r.Context(), accountID)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	defer a.releaseAccountTokenLease(accountID, leaseOwner)
	proxyURL, err := a.accountProxyString(accountID)
	if err != nil {
		a.recordAuthorization(&accountID, nil, "", "session_key", false, err.Error(), "", requestIP(r))
		status := http.StatusConflict
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeError(w, status, err.Error())
		return
	}
	token, err := exchangeClaudeSessionKey(r.Context(), strings.TrimSpace(input.SessionKey), apiScope, proxyURL)
	if err != nil {
		a.recordAuthorization(&accountID, nil, "", "session_key", false, err.Error(), "", requestIP(r))
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := a.saveAuthorizedClaudeToken(accountID, authType, token, leaseOwner, sourceSKHint(input.SessionKey)); err != nil {
		a.recordAuthorization(&accountID, nil, "", "session_key", false, err.Error(), token.SubscriptionType, requestIP(r))
		if writeRuntimeCredentialOwnerError(w, err) {
			return
		}
		writeDBError(w, err)
		return
	}
	a.recordAuthorization(&accountID, nil, "", "session_key", true, "authorization succeeded", token.SubscriptionType, requestIP(r))
	writeJSON(w, http.StatusOK, tokenMetadata(token))
}

func exchangeClaudeSessionKey(ctx context.Context, sessionKey, scope, proxyURL string) (*claudeTokenInfo, error) {
	for attempt := 0; ; attempt++ {
		organizations, err := getClaudeOrganizations(ctx, sessionKey, proxyURL)
		if err == nil {
			var token *claudeTokenInfo
			token, err = exchangeClaudeSessionKeyWithOrganizations(ctx, sessionKey, scope, proxyURL, organizations)
			if err == nil {
				return token, nil
			}
		}
		if !isClaudeSessionProxyChallenge(err) || attempt >= len(claudeSessionChallengeDelays) {
			return nil, err
		}
		timer := time.NewTimer(claudeSessionChallengeDelays[attempt])
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func getClaudeOrganizations(ctx context.Context, sessionKey, proxyURL string) ([]claudeOrganization, error) {
	client, err := claudeReqClient(proxyURL)
	if err != nil {
		return nil, err
	}
	var organizations []claudeOrganization
	response, err := client.R().SetContext(ctx).SetCookies(&http.Cookie{Name: "sessionKey", Value: sessionKey}).SetSuccessResult(&organizations).Get(claudeOrganizationsEndpoint)
	if err != nil {
		return nil, fmt.Errorf("get Claude organization: %w", err)
	}
	if !response.IsSuccessState() {
		return nil, &claudeSessionUpstreamError{
			Stage:               "organization lookup",
			Status:              response.StatusCode,
			CloudflareChallenge: isCloudflareChallengeResponse(response),
			InvalidSession:      response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden,
		}
	}
	if len(organizations) == 0 {
		return nil, &claudeSessionUpstreamError{Stage: "organization lookup", Status: response.StatusCode, InvalidSession: true}
	}
	return organizations, nil
}

func exchangeClaudeSessionKeyWithOrganizations(ctx context.Context, sessionKey, scope, proxyURL string, organizations []claudeOrganization) (*claudeTokenInfo, error) {
	if len(organizations) == 0 {
		return nil, errors.New("Claude Session Key has no organization")
	}
	client, err := claudeReqClient(proxyURL)
	if err != nil {
		return nil, err
	}
	organizationID := organizations[0].UUID
	subscriptionType := ""
	if organizations[0].RavenType != nil {
		subscriptionType = normalizeSubscriptionType(*organizations[0].RavenType)
	}
	for _, organization := range organizations {
		if organization.RavenType != nil && *organization.RavenType == "team" {
			organizationID = organization.UUID
			subscriptionType = normalizeSubscriptionType(*organization.RavenType)
			break
		}
	}
	verifier, _ := secureBase64(32)
	state, _ := secureBase64(32)
	body := map[string]any{
		"response_type": "code", "client_id": claudeOAuthClientID, "organization_uuid": organizationID,
		"redirect_uri": claudeOAuthRedirectURI, "scope": scope, "state": state,
		"code_challenge": pkceChallenge(verifier), "code_challenge_method": "S256",
	}
	var authorization struct {
		RedirectURI string `json:"redirect_uri"`
	}
	response, err := client.R().SetContext(ctx).SetCookies(&http.Cookie{Name: "sessionKey", Value: sessionKey}).
		SetHeader("Accept", "application/json").SetHeader("Origin", "https://claude.ai").SetHeader("Referer", "https://claude.ai/new").
		SetHeader("Content-Type", "application/json").SetBody(body).SetSuccessResult(&authorization).
		Post(strings.TrimRight(claudeSessionAuthorizeBaseURL, "/") + "/" + url.PathEscape(organizationID) + "/authorize")
	if err != nil {
		return nil, fmt.Errorf("authorize Claude session: %w", err)
	}
	if !response.IsSuccessState() || authorization.RedirectURI == "" {
		return nil, &claudeSessionUpstreamError{
			Stage:               "authorization code exchange",
			Status:              response.StatusCode,
			CloudflareChallenge: isCloudflareChallengeResponse(response),
			InvalidSession:      response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden,
		}
	}
	redirect, err := url.Parse(authorization.RedirectURI)
	if err != nil || redirect.Query().Get("code") == "" {
		return nil, errors.New("Claude authorization did not return a code")
	}
	code := redirect.Query().Get("code")
	if returnedState := redirect.Query().Get("state"); returnedState != "" {
		code += "#" + returnedState
	}
	token, err := exchangeClaudeCode(ctx, code, verifier, proxyURL)
	if err != nil {
		return nil, err
	}
	if token.OrgUUID == "" {
		token.OrgUUID = organizationID
	}
	if subscriptionType != "" {
		token.SubscriptionType = subscriptionType
	}
	return token, nil
}

func exchangeClaudeCode(ctx context.Context, code, verifier, proxyURL string) (*claudeTokenInfo, error) {
	client, err := claudeReqClient(proxyURL)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(strings.TrimSpace(code), "#", 2)
	body := map[string]any{"code": parts[0], "grant_type": "authorization_code", "client_id": claudeOAuthClientID, "redirect_uri": claudeOAuthRedirectURI, "code_verifier": verifier}
	if len(parts) == 2 && parts[1] != "" {
		body["state"] = parts[1]
	}
	var responseBody claudeTokenResponse
	response, err := client.R().SetContext(ctx).SetHeader("Accept", "application/json, text/plain, */*").SetHeader("Content-Type", "application/json").SetHeader("User-Agent", "axios/1.13.6").SetBody(body).SetSuccessResult(&responseBody).Post(claudeTokenEndpoint)
	if err != nil {
		return nil, fmt.Errorf("exchange Claude OAuth code: %w", err)
	}
	if !response.IsSuccessState() || responseBody.AccessToken == "" {
		return nil, &claudeSessionUpstreamError{
			Stage:               "token exchange",
			Status:              response.StatusCode,
			CloudflareChallenge: isCloudflareChallengeResponse(response),
			InvalidSession:      response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden,
		}
	}
	token := normalizeClaudeToken(&responseBody)
	// Profile enrichment is best-effort: a temporary metadata failure must not
	// invalidate an otherwise usable OAuth authorization.
	_ = enrichClaudeTokenProfile(ctx, token, proxyURL)
	return token, nil
}

func normalizeClaudeToken(response *claudeTokenResponse) *claudeTokenInfo {
	token := &claudeTokenInfo{AccessToken: response.AccessToken, TokenType: response.TokenType, ExpiresIn: response.ExpiresIn, ExpiresAt: time.Now().Unix() + response.ExpiresIn, RefreshToken: response.RefreshToken, Scope: response.Scope}
	if response.Organization != nil {
		token.OrgUUID = response.Organization.UUID
		for _, value := range []string{response.Organization.SubscriptionType, response.Organization.RavenType, response.Organization.OrganizationType} {
			if subscription := normalizeSubscriptionType(value); subscription != "" {
				token.SubscriptionType = subscription
				break
			}
		}
	}
	if response.Account != nil {
		token.AccountUUID = response.Account.UUID
		token.EmailAddress = response.Account.EmailAddress
	}
	if token.SubscriptionType == "" {
		token.SubscriptionType = subscriptionTypeFromToken(response.AccessToken)
	}
	return token
}

func enrichClaudeTokenProfile(ctx context.Context, token *claudeTokenInfo, proxyURL string) error {
	if token == nil || strings.TrimSpace(token.AccessToken) == "" {
		return errors.New("Claude OAuth profile requires an access token")
	}
	client, err := claudeReqClient(proxyURL)
	if err != nil {
		return err
	}
	var profile claudeOAuthProfile
	response, err := client.R().SetContext(ctx).
		SetHeader("Accept", "application/json, text/plain, */*").
		SetHeader("Authorization", "Bearer "+token.AccessToken).
		SetHeader("anthropic-beta", "oauth-2025-04-20").
		SetHeader("User-Agent", "claude-code/2.1.7").
		SetSuccessResult(&profile).
		Get(claudeProfileEndpoint)
	if err != nil {
		return fmt.Errorf("fetch Claude OAuth profile: %w", err)
	}
	if !response.IsSuccessState() {
		return fmt.Errorf("Claude OAuth profile returned status %d", response.StatusCode)
	}
	if subscription := normalizeSubscriptionType(profile.Organization.OrganizationType); subscription != "" {
		token.SubscriptionType = subscription
	} else if profile.Account.HasClaudeMax {
		token.SubscriptionType = "max"
	} else if profile.Account.HasClaudePro {
		token.SubscriptionType = "pro"
	}
	token.RateLimitTier = normalizeRateLimitTier(profile.Organization.RateLimitTier)
	return nil
}

func (a *app) syncClaudeAccountProfile(ctx context.Context, accountID int64, credentialsJSON, proxyURL string) error {
	credentials := decodeObject(credentialsJSON)
	accessToken, _ := credentials["access_token"].(string)
	token := &claudeTokenInfo{AccessToken: strings.TrimSpace(accessToken)}
	if err := enrichClaudeTokenProfile(ctx, token, proxyURL); err != nil {
		return err
	}
	_, err := a.db.Exec(`UPDATE accounts SET subscription_type = CASE WHEN ? = '' THEN subscription_type ELSE ? END, rate_limit_tier = CASE WHEN ? = '' THEN rate_limit_tier ELSE ? END, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL AND `+legacyExecutionPredicate("accounts"), token.SubscriptionType, token.SubscriptionType, token.RateLimitTier, token.RateLimitTier, accountID)
	return err
}

func tokenMetadata(token *claudeTokenInfo) map[string]any {
	return map[string]any{"expires_at": token.ExpiresAt, "scope": token.Scope, "org_uuid": token.OrgUUID, "account_uuid": token.AccountUUID, "email_address": token.EmailAddress, "subscription_type": token.SubscriptionType, "rate_limit_tier": token.RateLimitTier, "has_refresh_token": token.RefreshToken != ""}
}

type claudeTokenSaveCondition struct {
	ExpectedCredentials *string
	LeaseOwner          string
}

func (a *app) saveClaudeToken(accountID int64, authType string, token *claudeTokenInfo, preserveRefresh bool, sourceHints ...string) error {
	return a.saveClaudeTokenWithCondition(accountID, authType, token, preserveRefresh, nil, sourceHints...)
}

func (a *app) saveRefreshedClaudeToken(accountID int64, authType string, token *claudeTokenInfo, expectedCredentials, leaseOwner string) error {
	return a.saveClaudeTokenWithCondition(accountID, authType, token, true, &claudeTokenSaveCondition{ExpectedCredentials: &expectedCredentials, LeaseOwner: leaseOwner})
}

func (a *app) saveAuthorizedClaudeToken(accountID int64, authType string, token *claudeTokenInfo, leaseOwner string, sourceHints ...string) error {
	return a.saveClaudeTokenWithCondition(accountID, authType, token, false, &claudeTokenSaveCondition{LeaseOwner: leaseOwner}, sourceHints...)
}

func (a *app) saveClaudeTokenWithCondition(accountID int64, authType string, token *claudeTokenInfo, preserveRefresh bool, condition *claudeTokenSaveCondition, sourceHints ...string) error {
	var currentCredentials string
	var previousOnboarded, previousInvalidated sql.NullString
	if err := a.db.QueryRow(`SELECT credentials_json, onboarded_at, invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL AND `+legacyExecutionPredicate("accounts"), accountID).Scan(&currentCredentials, &previousOnboarded, &previousInvalidated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if ownerErr := a.requireLegacyCredentialOwner(context.Background(), accountID); errors.Is(ownerErr, errRuntimeCredentialOwner) {
				return ownerErr
			}
		}
		return err
	}
	if condition != nil && condition.ExpectedCredentials != nil && currentCredentials != *condition.ExpectedCredentials {
		return errAccountTokenLeaseLost
	}
	credentials := map[string]any{}
	if preserveRefresh {
		credentials = decodeObject(currentCredentials)
	}
	sourceHint := ""
	if len(sourceHints) > 0 {
		sourceHint = strings.TrimSpace(sourceHints[0])
	}
	encoded, _ := json.Marshal(token)
	var fresh map[string]any
	_ = json.Unmarshal(encoded, &fresh)
	for key, value := range fresh {
		if key == "refresh_token" && preserveRefresh && strings.TrimSpace(fmt.Sprint(value)) == "" {
			continue
		}
		credentials[key] = value
	}
	credentialsJSON, _ := json.Marshal(credentials)
	expiresAt := time.Unix(token.ExpiresAt, 0).UTC().Format(time.RFC3339Nano)
	subscription := token.SubscriptionType
	if subscription == "" {
		subscription = subscriptionTypeFromCredentials(credentials)
	}
	rateLimitTier := token.RateLimitTier
	if rateLimitTier == "" {
		rateLimitTier = rateLimitTierFromCredentials(credentials)
	}
	schedulingUpdate := `status = 'active', schedulable = 1,`
	rateLimitStateUpdate := `rate_limit_window = '', rate_limit_reason = '', consecutive_429 = 0, last_429_at = NULL, rate_limit_downweight_until = NULL,`
	if preserveRefresh {
		schedulingUpdate = `status = CASE WHEN status = 'error' THEN 'active' ELSE status END,`
		rateLimitStateUpdate = ``
	}
	isReauthorization := !preserveRefresh && previousOnboarded.Valid
	query := `UPDATE accounts SET auth_type = ?, credentials_json = ?, credential_hint = ?, source_sk_hint = CASE WHEN ? = '' THEN source_sk_hint ELSE ? END, auth_status = 'valid', rate_limit_reset_at = CASE WHEN auth_error LIKE 'OAuth 401:%' OR auth_error LIKE 'token refresh retry exhausted:%' THEN NULL ELSE rate_limit_reset_at END, auth_error = '', auth_checked_at = ` + nowSQL + `, token_expires_at = ?, subscription_type = ?, rate_limit_tier = ?, reauthorized_at = CASE WHEN ? THEN ` + nowSQL + ` ELSE reauthorized_at END, reauthorization_count = reauthorization_count + CASE WHEN ? THEN 1 ELSE 0 END, onboarded_at = CASE WHEN onboarded_at IS NULL OR invalidated_at IS NOT NULL THEN ` + nowSQL + ` ELSE onboarded_at END, invalidated_at = NULL, error_message = '', ` + rateLimitStateUpdate + schedulingUpdate + ` updated_at = ` + nowSQL + ` WHERE id = ? AND deleted_at IS NULL AND ` + legacyExecutionPredicate("accounts")
	args := []any{authType, string(credentialsJSON), credentialHint(string(credentialsJSON)), sourceHint, sourceHint, expiresAt, subscription, rateLimitTier, isReauthorization, isReauthorization, accountID}
	if condition != nil {
		if condition.ExpectedCredentials != nil {
			query += ` AND credentials_json = ?`
			args = append(args, *condition.ExpectedCredentials)
		}
		if condition.LeaseOwner != "" {
			query += ` AND EXISTS (SELECT 1 FROM account_token_leases lease WHERE lease.account_id = accounts.id AND lease.owner = ? AND lease.expires_at > CAST(strftime('%s','now') AS INTEGER))`
			args = append(args, condition.LeaseOwner)
		}
	}
	result, err := a.db.Exec(query, args...)
	if err == nil {
		if affected, affectedErr := result.RowsAffected(); affectedErr != nil {
			err = affectedErr
		} else if affected == 0 {
			if ownerErr := a.requireLegacyCredentialOwner(context.Background(), accountID); errors.Is(ownerErr, errRuntimeCredentialOwner) {
				err = ownerErr
			} else if condition != nil {
				err = errAccountTokenLeaseLost
			} else {
				err = sql.ErrNoRows
			}
		}
	}
	if err == nil && !preserveRefresh {
		_, err = a.db.Exec(`DELETE FROM account_rpm_thresholds WHERE account_id = ?`, accountID)
	}
	if err == nil && (!previousOnboarded.Valid || previousInvalidated.Valid) {
		a.recordAccountLifecycle(accountID, "onboarded")
	}
	return err
}

func (a *app) markAccountReauth(accountID int64, reason string) {
	a.markAccountReauthIfRefreshTokenCurrent(accountID, reason, "", false)
}

func (a *app) markAccountReauthIfRefreshTokenCurrent(accountID int64, reason, expectedRefreshToken string, conditional bool) bool {
	return a.markAccountReauthIfCurrent(accountID, reason, expectedRefreshToken, "", conditional)
}

func (a *app) markAccountReauthIfCredentialsCurrent(accountID int64, reason, expectedCredentials string) bool {
	return a.markAccountReauthIfCurrent(accountID, reason, "", expectedCredentials, true)
}

func (a *app) markAccountReauthIfCurrent(accountID int64, reason, expectedRefreshToken, expectedCredentials string, conditional bool) bool {
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		reason = reason[:500]
	}
	tx, err := a.db.Begin()
	if err != nil {
		return false
	}
	defer tx.Rollback()
	var invalidated sql.NullString
	if err := tx.QueryRow(`SELECT invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL AND `+legacyExecutionPredicate("accounts"), accountID).Scan(&invalidated); err != nil {
		return false
	}
	query := `UPDATE accounts SET ` + accumulateAccountSurvivalSQL + `, auth_status = 'reauth_required', auth_error = ?, auth_checked_at = ` + nowSQL + `, invalidated_at = COALESCE(invalidated_at, ` + nowSQL + `), error_message = ?, status = CASE WHEN status = 'disabled' THEN status ELSE 'error' END, updated_at = ` + nowSQL + ` WHERE id = ? AND ` + legacyExecutionPredicate("accounts")
	args := []any{reason, reason, accountID}
	if conditional {
		if expectedCredentials != "" {
			query += ` AND credentials_json = ?`
			args = append(args, expectedCredentials)
		} else {
			query += ` AND COALESCE(json_extract(credentials_json, '$.refresh_token'), '') = ?`
			args = append(args, expectedRefreshToken)
		}
	}
	result, err := tx.Exec(query, args...)
	if err != nil {
		return false
	}
	if affected, affectedErr := result.RowsAffected(); affectedErr != nil || affected == 0 {
		return false
	}
	if !invalidated.Valid {
		if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type, reason) VALUES (?, 'invalidated', ?)`, accountID, reason); err != nil {
			return false
		}
	}
	return tx.Commit() == nil
}

func refreshClaudeToken(ctx context.Context, refreshToken, proxyURL string) (*claudeTokenInfo, error) {
	client, err := claudeReqClient(proxyURL)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"grant_type": "refresh_token", "refresh_token": refreshToken, "client_id": claudeOAuthClientID}
	var responseBody claudeTokenResponse
	response, err := client.R().SetContext(ctx).SetHeader("Accept", "application/json, text/plain, */*").SetHeader("Content-Type", "application/json").SetHeader("User-Agent", "axios/1.13.6").SetBody(body).SetSuccessResult(&responseBody).Post(claudeTokenEndpoint)
	if err != nil {
		return nil, err
	}
	if !response.IsSuccessState() || responseBody.AccessToken == "" {
		detail := claudeRefreshFailureDetail(response.String())
		if response.IsSuccessState() && detail == "" {
			detail = "response did not include access_token"
		}
		return nil, &claudeRefreshError{Status: response.StatusCode, Detail: detail}
	}
	return normalizeClaudeToken(&responseBody), nil
}

func (a *app) ensureGatewayAccountToken(ctx context.Context, account gatewayAccount) (gatewayAccount, error) {
	return a.refreshGatewayAccountToken(ctx, account, false)
}

func (a *app) refreshGatewayAccountToken(ctx context.Context, account gatewayAccount, force bool) (gatewayAccount, error) {
	if !force && !gatewayTokenNeedsRefresh(account.CredentialsJSON) {
		return account, nil
	}
	originalAccessToken := gatewayAccountAccessToken(account)
	originalRefreshToken := gatewayAccountRefreshToken(account)
	lockValue, _ := a.tokenLocks.LoadOrStore(account.ID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	leaseOwner, err := a.acquireAccountTokenLease(ctx, account.ID)
	if err != nil {
		return account, err
	}
	defer a.releaseAccountTokenLease(account.ID, leaseOwner)
	account, err = a.reloadGatewayAccountToken(account)
	if err != nil {
		return account, err
	}
	if force && gatewayAccountTokenAdvanced(account, originalAccessToken, originalRefreshToken) && !gatewayTokenNeedsRefresh(account.CredentialsJSON) {
		return account, nil
	}
	if !force && !gatewayTokenNeedsRefresh(account.CredentialsJSON) {
		return account, nil
	}
	attemptedCredentials := account.CredentialsJSON
	refreshToken := gatewayAccountRefreshToken(account)
	if refreshToken == "" {
		a.markAccountReauth(account.ID, "OAuth access token was rejected and no refresh token is available")
		return account, errors.New("OAuth account has no refresh token; reauthorization is required")
	}
	if !account.ProxyID.Valid {
		return account, errors.New("CCMAX account must bind an active proxy")
	}
	value, err := a.proxyURL(account.ProxyID.Int64)
	if err != nil {
		return account, errors.New("CCMAX account proxy is unavailable")
	}
	token, err := refreshClaudeToken(ctx, refreshToken, value.String())
	if err != nil {
		var refreshErr *claudeRefreshError
		if errors.As(err, &refreshErr) && refreshErr.permanent() {
			failureReason := claudeRefreshFailureReason(err)
			latest, reloadErr := a.reloadGatewayAccountToken(account)
			if reloadErr == nil && gatewayAccountTokenAdvanced(latest, gatewayAccountAccessToken(account), refreshToken) {
				return latest, nil
			}
			if !a.markAccountReauthIfRefreshTokenCurrent(account.ID, failureReason, refreshToken, true) {
				if latest, reloadErr = a.reloadGatewayAccountToken(account); reloadErr == nil && gatewayAccountTokenAdvanced(latest, gatewayAccountAccessToken(account), refreshToken) {
					return latest, nil
				}
			}
		}
		return account, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		err = a.saveRefreshedClaudeToken(account.ID, account.AuthType, token, attemptedCredentials, leaseOwner)
		if err == nil {
			break
		}
		if !errors.Is(err, errAccountTokenLeaseLost) {
			return account, err
		}
		latest, reloadErr := a.reloadGatewayAccountToken(account)
		if reloadErr != nil {
			return account, reloadErr
		}
		if gatewayAccountTokenAdvanced(latest, gatewayAccountAccessToken(account), refreshToken) {
			return latest, nil
		}
		attemptedCredentials = latest.CredentialsJSON
		account = latest
	}
	if err != nil {
		return account, err
	}
	account, err = a.reloadGatewayAccountToken(account)
	if err != nil {
		return account, err
	}
	return account, nil
}

func (a *app) reloadGatewayAccountToken(account gatewayAccount) (gatewayAccount, error) {
	if err := a.db.QueryRow(`SELECT auth_type, credentials_json, proxy_id FROM accounts WHERE id = ? AND deleted_at IS NULL AND `+legacyExecutionPredicate("accounts"), account.ID).Scan(&account.AuthType, &account.CredentialsJSON, &account.ProxyID); err != nil {
		return account, err
	}
	return account, nil
}

func gatewayAccountTokenAdvanced(account gatewayAccount, previousAccessToken, previousRefreshToken string) bool {
	currentAccessToken := gatewayAccountAccessToken(account)
	currentRefreshToken := gatewayAccountRefreshToken(account)
	return currentAccessToken != "" && (currentAccessToken != previousAccessToken || currentRefreshToken != previousRefreshToken)
}

func gatewayAccountHasRefreshToken(account gatewayAccount) bool {
	credentials := decodeObject(account.CredentialsJSON)
	refreshToken, _ := credentials["refresh_token"].(string)
	return strings.TrimSpace(refreshToken) != ""
}

func gatewayAccountAccessToken(account gatewayAccount) string {
	credentials := decodeObject(account.CredentialsJSON)
	accessToken, _ := credentials["access_token"].(string)
	return strings.TrimSpace(accessToken)
}

func gatewayAccountRefreshToken(account gatewayAccount) string {
	credentials := decodeObject(account.CredentialsJSON)
	refreshToken, _ := credentials["refresh_token"].(string)
	return strings.TrimSpace(refreshToken)
}

func gatewayTokenNeedsRefresh(credentialsJSON string) bool {
	credentials := decodeObject(credentialsJSON)
	refreshToken, _ := credentials["refresh_token"].(string)
	expiresAt := int64FromAny(credentials["expires_at"])
	return strings.TrimSpace(refreshToken) != "" && expiresAt > 0 && time.Until(time.Unix(expiresAt, 0)) <= gatewayTokenRefreshBefore
}

func int64FromAny(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		result, _ := typed.Int64()
		return result
	case string:
		parsed, _ := time.Parse(time.RFC3339, typed)
		return parsed.Unix()
	default:
		return 0
	}
}
