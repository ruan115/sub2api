package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	gatewayOAuthRefreshFailedMessage    = "Upstream 401: OAuth access token has been revoked ac刷新失败"
	gatewayOAuthRefreshRejectedMessage  = "Upstream 401: OAuth access token has been revoked ac刷新后仍失败"
	gatewayOAuthRefreshFailureBodyLimit = 2 << 20
	gatewayOAuthRefreshFailureCooldown  = 10 * time.Minute
)

// retryGatewayRevokedOAuth gives an OAuth account one same-account recovery
// attempt before normal failover handles the response. The refresh function
// owns the cross-process lease and rotated refresh-token persistence.
func (a *app) retryGatewayRevokedOAuth(
	r *http.Request,
	client *http.Client,
	upstreamURL string,
	body []byte,
	requestedModel string,
	countTokens bool,
	key gatewayKey,
	account gatewayAccount,
	prepared claudePreparedRequest,
	response *http.Response,
) (*http.Response, gatewayAccount, claudePreparedRequest, bool, error) {
	if response == nil || response.StatusCode != http.StatusUnauthorized || !prepared.OAuth {
		return response, account, prepared, false, nil
	}
	_, revoked, readErr := readGatewayRevokedOAuthResponse(response)
	if readErr != nil || !revoked {
		return response, account, prepared, false, nil
	}

	refreshed, refreshErr := a.refreshGatewayAccountToken(r.Context(), account, true)
	if refreshErr != nil {
		a.recordGatewayOAuthRefreshFailure(account, gatewayOAuthRefreshFailedMessage, refreshErr)
		return replaceGatewayOAuthFailureResponse(response, gatewayOAuthRefreshFailedMessage), account, prepared, true, nil
	}
	account = refreshed

	retryPrepared, prepareErr := prepareClaudeRequest(r, body, account, requestedModel, countTokens)
	if prepareErr != nil {
		return nil, account, prepared, false, prepareErr
	}
	retryPrepared.RejectAnthropicDowngrade = key.RejectAnthropicDowngrade
	retryPrepared.MaskQuotaHeaders = key.QuotaHeaderMasking
	retryPrepared.CacheCreationDetail = key.CacheCreationDetail
	retryRequest, requestErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(retryPrepared.Body))
	if requestErr != nil {
		return nil, account, retryPrepared, false, requestErr
	}
	if headerErr := buildClaudeHeaders(retryRequest.Header, r.Header, retryPrepared, account.CredentialsJSON); headerErr != nil {
		return nil, account, retryPrepared, false, headerErr
	}

	// Account selection reserved the original attempt only. The replay is a
	// second real upstream request and must consume RPM capacity as such.
	a.recordGatewayAccountRPM(account.ID)
	retryStarted := time.Now()
	retryResponse, retryErr := doGatewayUpstreamRequest(r, client, retryRequest, retryPrepared)
	if retryErr != nil {
		return nil, account, retryPrepared, false, retryErr
	}
	retryResponse = retryGatewayCompatibility400(client, retryRequest, retryResponse, retryPrepared, retryStarted)
	_, stillRevoked, retryReadErr := readGatewayRevokedOAuthResponse(retryResponse)
	if retryReadErr != nil {
		return retryResponse, account, retryPrepared, false, nil
	}
	if stillRevoked {
		a.markAccountReauthIfCredentialsCurrent(account.ID, gatewayOAuthRefreshRejectedMessage, account.CredentialsJSON)
		return replaceGatewayOAuthFailureResponse(retryResponse, gatewayOAuthRefreshRejectedMessage), account, retryPrepared, true, nil
	}
	return retryResponse, account, retryPrepared, false, nil
}

func readGatewayRevokedOAuthResponse(response *http.Response) ([]byte, bool, error) {
	if response == nil || response.StatusCode != http.StatusUnauthorized || response.Body == nil {
		return nil, false, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, gatewayOAuthRefreshFailureBodyLimit))
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	if err != nil {
		return body, false, err
	}
	return body, accountAuthenticationFailureIsTerminal(upstreamErrorMessage(body)), nil
}

func replaceGatewayOAuthFailureResponse(response *http.Response, message string) *http.Response {
	payload, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    "authentication_error",
			"message": message,
		},
	})
	if response == nil {
		response = &http.Response{Header: make(http.Header)}
	}
	if response.Header == nil {
		response.Header = make(http.Header)
	}
	response.StatusCode = http.StatusUnauthorized
	response.Status = strconv.Itoa(http.StatusUnauthorized) + " " + http.StatusText(http.StatusUnauthorized)
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("Content-Length", strconv.Itoa(len(payload)))
	response.Header.Del("Content-Encoding")
	response.ContentLength = int64(len(payload))
	response.Body = io.NopCloser(bytes.NewReader(payload))
	return response
}

func (a *app) recordGatewayOAuthRefreshFailure(account gatewayAccount, publicMessage string, refreshErr error) {
	reason := publicMessage
	if detail := strings.TrimSpace(refreshErr.Error()); detail != "" {
		reason += " · " + detail
	}
	var typed *claudeRefreshError
	permanent := errors.As(refreshErr, &typed) && typed.permanent()
	if permanent || strings.Contains(strings.ToLower(refreshErr.Error()), "no refresh token") {
		a.markAccountReauthIfCredentialsCurrent(account.ID, reason, account.CredentialsJSON)
		return
	}
	until := time.Now().UTC().Add(gatewayOAuthRefreshFailureCooldown).Format(time.RFC3339Nano)
	_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, error_message = ?, auth_error = ?, auth_checked_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ? AND credentials_json = ? AND `+legacyExecutionPredicate("accounts"), until, reason, reason, account.ID, account.CredentialsJSON)
	logDatabaseWriteError("record OAuth refresh failure cooldown", err)
}

func gatewayOAuthRefreshCompatibilityMessage(body []byte) (string, bool) {
	message := strings.TrimSpace(upstreamErrorMessage(body))
	switch message {
	case gatewayOAuthRefreshFailedMessage, gatewayOAuthRefreshRejectedMessage:
		return message, true
	default:
		return "", false
	}
}
