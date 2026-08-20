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
}

type claudeRefreshError struct {
	Status int
}

func (e *claudeRefreshError) Error() string {
	return fmt.Sprintf("Claude token refresh failed (status %d)", e.Status)
}

func (e *claudeRefreshError) permanent() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
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
	if err := a.db.QueryRow(`SELECT proxy_id FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&proxyID); err != nil {
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
	if _, err := url.ParseRequestURI(proxyURL); err != nil {
		return nil, fmt.Errorf("invalid proxy URL: %w", err)
	}
	client.SetProxyURL(proxyURL)
	return client, nil
}

func (a *app) handleAccountAuthURL(w http.ResponseWriter, r *http.Request) {
	accountID, ok := pathID(w, r)
	if !ok {
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
	var input struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	session, ok := a.oauthSessions.take(strings.TrimSpace(input.SessionID), accountID)
	if !ok {
		a.recordAuthorization(&accountID, nil, "", "oauth", false, "OAuth session not found or expired", "", requestIP(r))
		writeError(w, http.StatusBadRequest, "OAuth session not found or expired")
		return
	}
	token, err := exchangeClaudeCode(r.Context(), input.Code, session.CodeVerifier, session.ProxyURL)
	if err != nil {
		a.markAccountReauth(accountID, err.Error())
		a.recordAuthorization(&accountID, nil, "", "oauth", false, err.Error(), "", requestIP(r))
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	authType := "oauth"
	if session.Scope == claudeOAuthInferenceOnly {
		authType = "setup_token"
	}
	if err := a.saveClaudeToken(accountID, authType, token, false); err != nil {
		a.recordAuthorization(&accountID, nil, "", "oauth", false, err.Error(), token.SubscriptionType, requestIP(r))
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
		a.markAccountReauth(accountID, err.Error())
		a.recordAuthorization(&accountID, nil, "", "session_key", false, err.Error(), "", requestIP(r))
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := a.saveClaudeToken(accountID, authType, token, false); err != nil {
		a.recordAuthorization(&accountID, nil, "", "session_key", false, err.Error(), token.SubscriptionType, requestIP(r))
		writeDBError(w, err)
		return
	}
	a.recordAuthorization(&accountID, nil, "", "session_key", true, "authorization succeeded", token.SubscriptionType, requestIP(r))
	writeJSON(w, http.StatusOK, tokenMetadata(token))
}

func exchangeClaudeSessionKey(ctx context.Context, sessionKey, scope, proxyURL string) (*claudeTokenInfo, error) {
	client, err := claudeReqClient(proxyURL)
	if err != nil {
		return nil, err
	}
	var organizations []struct {
		UUID      string  `json:"uuid"`
		RavenType *string `json:"raven_type"`
	}
	response, err := client.R().SetContext(ctx).SetCookies(&http.Cookie{Name: "sessionKey", Value: sessionKey}).SetSuccessResult(&organizations).Get(claudeOrganizationsEndpoint)
	if err != nil {
		return nil, fmt.Errorf("get Claude organization: %w", err)
	}
	if !response.IsSuccessState() || len(organizations) == 0 {
		return nil, fmt.Errorf("Claude Session Key is invalid or has no organization (status %d)", response.StatusCode)
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
	response, err = client.R().SetContext(ctx).SetCookies(&http.Cookie{Name: "sessionKey", Value: sessionKey}).
		SetHeader("Accept", "application/json").SetHeader("Origin", "https://claude.ai").SetHeader("Referer", "https://claude.ai/new").
		SetHeader("Content-Type", "application/json").SetBody(body).SetSuccessResult(&authorization).
		Post(strings.TrimRight(claudeSessionAuthorizeBaseURL, "/") + "/" + url.PathEscape(organizationID) + "/authorize")
	if err != nil {
		return nil, fmt.Errorf("authorize Claude session: %w", err)
	}
	if !response.IsSuccessState() || authorization.RedirectURI == "" {
		return nil, fmt.Errorf("Claude Session Key authorization failed (status %d)", response.StatusCode)
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
		return nil, fmt.Errorf("Claude OAuth token exchange failed (status %d)", response.StatusCode)
	}
	return normalizeClaudeToken(&responseBody), nil
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

func tokenMetadata(token *claudeTokenInfo) map[string]any {
	return map[string]any{"expires_at": token.ExpiresAt, "scope": token.Scope, "org_uuid": token.OrgUUID, "account_uuid": token.AccountUUID, "email_address": token.EmailAddress, "subscription_type": token.SubscriptionType, "has_refresh_token": token.RefreshToken != ""}
}

func (a *app) saveClaudeToken(accountID int64, authType string, token *claudeTokenInfo, preserveRefresh bool) error {
	credentials := map[string]any{}
	var previousOnboarded, previousInvalidated sql.NullString
	if preserveRefresh {
		var raw string
		if err := a.db.QueryRow(`SELECT credentials_json, onboarded_at, invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&raw, &previousOnboarded, &previousInvalidated); err != nil {
			return err
		}
		credentials = decodeObject(raw)
	} else if err := a.db.QueryRow(`SELECT onboarded_at, invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&previousOnboarded, &previousInvalidated); err != nil {
		return err
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
	_, err := a.db.Exec(`UPDATE accounts SET auth_type = ?, credentials_json = ?, credential_hint = ?, auth_status = 'valid', auth_error = '', auth_checked_at = `+nowSQL+`, token_expires_at = ?, subscription_type = ?, onboarded_at = CASE WHEN onboarded_at IS NULL OR invalidated_at IS NOT NULL THEN `+nowSQL+` ELSE onboarded_at END, invalidated_at = NULL, error_message = '', status = CASE WHEN status = 'error' THEN 'active' ELSE status END, updated_at = `+nowSQL+` WHERE id = ? AND deleted_at IS NULL`, authType, string(credentialsJSON), credentialHint(string(credentialsJSON)), expiresAt, subscription, accountID)
	if err == nil && (!previousOnboarded.Valid || previousInvalidated.Valid) {
		a.recordAccountLifecycle(accountID, "onboarded")
	}
	return err
}

func (a *app) markAccountReauth(accountID int64, reason string) {
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		reason = reason[:500]
	}
	tx, err := a.db.Begin()
	if err != nil {
		return
	}
	defer tx.Rollback()
	var invalidated sql.NullString
	if err := tx.QueryRow(`SELECT invalidated_at FROM accounts WHERE id = ? AND deleted_at IS NULL`, accountID).Scan(&invalidated); err != nil {
		return
	}
	if _, err := tx.Exec(`UPDATE accounts SET auth_status = 'reauth_required', auth_error = ?, auth_checked_at = `+nowSQL+`, invalidated_at = COALESCE(invalidated_at, `+nowSQL+`), error_message = ?, status = CASE WHEN status = 'disabled' THEN status ELSE 'error' END, updated_at = `+nowSQL+` WHERE id = ?`, reason, reason, accountID); err != nil {
		return
	}
	if !invalidated.Valid {
		if _, err := tx.Exec(`INSERT INTO account_lifecycle_events (account_id, event_type) VALUES (?, 'invalidated')`, accountID); err != nil {
			return
		}
	}
	_ = tx.Commit()
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
		return nil, &claudeRefreshError{Status: response.StatusCode}
	}
	return normalizeClaudeToken(&responseBody), nil
}

func (a *app) ensureGatewayAccountToken(ctx context.Context, account gatewayAccount) (gatewayAccount, error) {
	if !gatewayTokenNeedsRefresh(account.CredentialsJSON) {
		return account, nil
	}
	lockValue, _ := a.tokenLocks.LoadOrStore(account.ID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	var latestAuthType, latestCredentials string
	var latestProxyID sql.NullInt64
	if err := a.db.QueryRow(`SELECT auth_type, credentials_json, proxy_id FROM accounts WHERE id = ? AND deleted_at IS NULL`, account.ID).Scan(&latestAuthType, &latestCredentials, &latestProxyID); err != nil {
		return account, err
	}
	account.AuthType = latestAuthType
	account.CredentialsJSON = latestCredentials
	account.ProxyID = latestProxyID
	if !gatewayTokenNeedsRefresh(account.CredentialsJSON) {
		return account, nil
	}
	credentials := decodeObject(account.CredentialsJSON)
	refreshToken, _ := credentials["refresh_token"].(string)
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
			a.markAccountReauth(account.ID, err.Error())
		}
		return account, err
	}
	if err := a.saveClaudeToken(account.ID, account.AuthType, token, true); err != nil {
		return account, err
	}
	var raw string
	if err := a.db.QueryRow(`SELECT credentials_json FROM accounts WHERE id = ?`, account.ID).Scan(&raw); err != nil {
		return account, err
	}
	account.CredentialsJSON = raw
	return account, nil
}

func gatewayTokenNeedsRefresh(credentialsJSON string) bool {
	credentials := decodeObject(credentialsJSON)
	refreshToken, _ := credentials["refresh_token"].(string)
	expiresAt := int64FromAny(credentials["expires_at"])
	return strings.TrimSpace(refreshToken) != "" && expiresAt > 0 && time.Until(time.Unix(expiresAt, 0)) <= 5*time.Minute
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
