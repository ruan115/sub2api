package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	sub2claude "github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
)

type gatewayKey struct {
	ID                   int64
	UserID               int64
	GroupID              string
	Quota                float64
	QuotaUsed            float64
	UserBalance          sql.NullFloat64
	UserRPM              int
	Allowed              string
	UserRole             string
	ExpiresAt            sql.NullString
	NormalRequestMode    bool
	StreamHedgeEnabled   bool
	AdaptiveHedgeEnabled bool
	RPMDispatchEnabled   bool
}

type gatewayAccount struct {
	ID               int64
	Name             string
	AuthType         string
	CredentialsJSON  string
	ExtraJSON        string
	Concurrency      int
	BaseRPM          int
	RPMStrategy      string
	StickyBuffer     int
	UserMsgQueueMode string
	ProxyID          sql.NullInt64
	Fingerprint      *sub2service.Fingerprint
}

type messageEnvelope struct {
	Model    string         `json:"model"`
	Stream   bool           `json:"stream"`
	Metadata map[string]any `json:"metadata"`
}

type gatewayAPIKeyContextKey struct{}

type gatewayProtocolContextKey struct{}

type gatewayNormalRequestModeContextKey struct{}

type gatewayProtocolContext struct {
	openAIChat   bool
	clientStream bool
}

type tokenUsage struct {
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
}

func (u tokenUsage) hasUsage() bool {
	return u.Input > 0 || u.Output > 0 || u.CacheCreation > 0 || u.CacheRead > 0
}

func (a *app) handleMessages(w http.ResponseWriter, r *http.Request) {
	a.handleClaudeGateway(w, r, false)
}

func (a *app) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	a.handleClaudeGateway(w, r, true)
}

type gatewayUpstreamFailure struct {
	status  int
	header  http.Header
	body    []byte
	account gatewayAccount
}

type gatewayPreOutputStreamError struct {
	status int
	body   []byte
}

func (e *gatewayPreOutputStreamError) Error() string {
	return "upstream stream returned an error before output"
}

func (a *app) handleClaudeGateway(w http.ResponseWriter, r *http.Request, countTokens bool) {
	secret := bearerOrAPIKey(r)
	if secret == "" {
		writeError(w, http.StatusUnauthorized, "API key is required")
		return
	}
	key, err := a.authenticateGatewayKey(secret)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid or unavailable API key")
		return
	}
	quotaRelease := func() {}
	if key.Quota > 0 {
		quotaRelease = a.acquireGatewayQuotaLock(key.ID)
		defer quotaRelease()
		key, err = a.authenticateGatewayKey(secret)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or unavailable API key")
			return
		}
	}
	if key.Quota > 0 && key.QuotaUsed >= key.Quota {
		writeAnthropicGatewayError(w, http.StatusForbidden, "permission_error", "API key quota exhausted")
		return
	}
	if key.UserBalance.Valid && key.UserBalance.Float64 <= 0 {
		writeAnthropicGatewayError(w, http.StatusPaymentRequired, "billing_error", "User balance exhausted")
		return
	}
	if ok := groupAllowedJSON(key.UserRole, key.Allowed, key.GroupID); !ok {
		writeError(w, http.StatusForbidden, "API key group is no longer allowed")
		return
	}
	if !countTokens {
		budgetRelease, lockErr := a.acquireGatewayBudgetLock(key.GroupID)
		if lockErr != nil {
			writeError(w, http.StatusForbidden, lockErr.Error())
			return
		}
		defer budgetRelease()
		if err := a.checkGatewayGroupBudget(key.GroupID); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
	}
	var body []byte
	if gatewayOpenAIChatRequest(r.Context()) {
		// The public Chat Completions body was already limited before protocol
		// conversion. Do not apply the same limit again to the expanded internal
		// Anthropic representation.
		body, err = io.ReadAll(r.Body)
	} else {
		body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "request body is too large or invalid")
		return
	}
	var envelope messageEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || strings.TrimSpace(envelope.Model) == "" {
		writeError(w, http.StatusBadRequest, "invalid Anthropic message request")
		return
	}
	if err := a.checkAndIncrementUserRPM(key); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), gatewayAPIKeyContextKey{}, key.ID))
	r = r.WithContext(context.WithValue(r.Context(), gatewayNormalRequestModeContextKey{}, key.NormalRequestMode))
	session := sub2service.GenerateCCMaxCompatibilitySessionHash(body, gatewayClientIP(r), r.UserAgent(), key.ID)
	excluded := map[int64]bool{}
	var lastFailure *gatewayUpstreamFailure
	var lastDispatchError error
	if !key.RPMDispatchEnabled && (key.StreamHedgeEnabled || key.AdaptiveHedgeEnabled) && envelope.Stream && !countTokens && a.gatewayStickyAccountID(key.ID, session) == 0 {
		handled, hedgeFailure, hedgeErr := a.handleGatewayStreamHedge(w, r, key, body, envelope.Model, session, excluded, key.AdaptiveHedgeEnabled)
		if handled {
			return
		}
		lastFailure = hedgeFailure
		lastDispatchError = hedgeErr
	}
	maxAccountAttempts := gatewayMaxAttempts(key.RPMDispatchEnabled)
	if countTokens {
		// Sub2API count_tokens selects one account and returns that upstream result.
		maxAccountAttempts = 1
	}
	for attempt := 0; attempt < maxAccountAttempts; attempt++ {
		account, acquireErr := a.acquireGatewayAccount(key, session, envelope.Model, excluded)
		if acquireErr != nil {
			lastDispatchError = acquireErr
			break
		}
		excluded[account.ID] = true
		account, err = a.ensureGatewayAccountToken(r.Context(), account)
		if err != nil {
			a.releaseGatewayAccount(account.ID)
			message := "Upstream request failed"
			if countTokens {
				message = "Failed to get access token"
			}
			writeAnthropicGatewayError(w, http.StatusBadGateway, "upstream_error", message)
			return
		}
		if err := validateGatewayAccountCredential(account); err != nil {
			a.releaseGatewayAccount(account.ID)
			message := "Upstream request failed"
			if countTokens {
				message = "Failed to get access token"
			}
			writeAnthropicGatewayError(w, http.StatusBadGateway, "upstream_error", message)
			return
		}
		if !accountRequestPassthrough(account) {
			// Sub2API treats identity persistence as best-effort. A repository
			// failure degrades to a request without unified fingerprint metadata.
			if resolved, fingerprintErr := a.ensureGatewayAccountFingerprint(account, r.Header); fingerprintErr == nil {
				account = resolved
			}
		}
		prepared, prepareErr := prepareClaudeRequest(r, body, account, envelope.Model, countTokens)
		if prepareErr != nil {
			a.releaseGatewayAccount(account.ID)
			writeError(w, http.StatusBadRequest, prepareErr.Error())
			return
		}
		path := "/v1/messages"
		if countTokens {
			path = "/v1/messages/count_tokens"
		}
		upstreamURL, urlErr := upstreamClaudeURL(account.ExtraJSON, path)
		if urlErr != nil {
			a.releaseGatewayAccount(account.ID)
			if !prepared.Passthrough {
				writeAnthropicGatewayError(w, http.StatusBadGateway, "upstream_error", "Upstream request failed")
				return
			}
			lastDispatchError = urlErr
			continue
		}
		upstreamRequest, requestErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(prepared.Body))
		if requestErr != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = requestErr
			continue
		}
		if headerErr := buildClaudeHeaders(upstreamRequest.Header, r.Header, prepared, account.CredentialsJSON); headerErr != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = headerErr
			continue
		}
		if !account.ProxyID.Valid {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = errors.New("CCMAX account must bind an active proxy")
			continue
		}
		proxyURL, err := a.proxyURL(account.ProxyID.Int64)
		if err != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = errors.New("CCMAX account proxy is unavailable")
			continue
		}
		client, clientErr := clientForProxy(proxyURL)
		if clientErr != nil {
			a.releaseGatewayAccount(account.ID)
			lastDispatchError = clientErr
			continue
		}
		queueRelease, queueErr := a.acquireUserMessageQueue(r.Context(), account, body, countTokens)
		if queueErr != nil {
			// Sub2API's user-message queue is fail-open: timeout or throttle
			// bookkeeping failure must not reject an otherwise valid request.
			queueRelease = func() {}
		}
		started := time.Now()
		if !key.RPMDispatchEnabled {
			a.recordGatewayAccountRPM(account.ID)
		}
		response, requestErr := doGatewayUpstreamRequest(r, client, upstreamRequest, prepared)
		if requestErr != nil {
			queueRelease()
			a.releaseGatewayAccount(account.ID)
			if !prepared.Passthrough {
				message := "Upstream request failed"
				if countTokens {
					message = "Request failed"
				}
				writeAnthropicGatewayError(w, http.StatusBadGateway, "upstream_error", message)
				return
			}
			lastDispatchError = requestErr
			continue
		}
		response = retryGatewayCompatibility400(client, upstreamRequest, response, prepared, started)
		if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
			a.captureAccountUpstreamState(account.ID, response)
		}
		if retryableGatewayStatus(response.StatusCode) {
			queueRelease()
			failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			a.releaseGatewayAccount(account.ID)
			if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
				a.captureAccountUpstreamFailure(account, response.StatusCode, failureBody)
			}
			lastFailure = &gatewayUpstreamFailure{status: response.StatusCode, header: response.Header.Clone(), body: failureBody, account: account}
			continue
		}
		if response.StatusCode >= 400 && !prepared.Passthrough {
			queueRelease()
			failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			a.releaseGatewayAccount(account.ID)
			if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
				a.captureAccountUpstreamFailure(account, response.StatusCode, failureBody)
			}
			writeSub2CompatibilityError(w, response.StatusCode, failureBody, countTokens)
			return
		}
		queueRelease()
		usage, forwardErr := forwardGatewayResponse(w, response, prepared.Stream && !countTokens, account, key.GroupID, prepared)
		response.Body.Close()
		a.releaseGatewayAccount(account.ID)
		var preOutputErr *gatewayPreOutputStreamError
		if errors.As(forwardErr, &preOutputErr) {
			if preOutputErr.status == 529 && !skipGatewayDefaultErrorHandling(prepared, preOutputErr.status) {
				a.captureAccountUpstreamState(account.ID, &http.Response{StatusCode: 529, Header: response.Header.Clone()})
			}
			lastFailure = &gatewayUpstreamFailure{status: preOutputErr.status, header: response.Header.Clone(), body: preOutputErr.body, account: account}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if countTokens {
				if forwardErr != nil {
					return
				}
				_, _ = a.db.Exec(`UPDATE api_keys SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, key.ID)
				return
			}
			if forwardErr == nil || usage.hasUsage() {
				a.recordGatewayUsage(key, account, envelope.Model, gatewayRecordedStream(r.Context(), prepared.Stream), response.Header.Get("request-id"), usage, started)
			}
		}
		if forwardErr != nil {
			return
		}
		return
	}
	if lastFailure != nil {
		if !accountRequestPassthrough(lastFailure.account) {
			writeSub2CompatibilityError(w, lastFailure.status, lastFailure.body, countTokens)
			return
		}
		copyGatewayResponseHeaders(w.Header(), lastFailure.header)
		w.Header().Set("X-CCMAX-Account", lastFailure.account.Name)
		w.Header().Set("X-CCMAX-Group", key.GroupID)
		w.WriteHeader(lastFailure.status)
		_, _ = w.Write(lastFailure.body)
		return
	}
	status, errorType, message := a.classifyGatewayNoAccount(key.GroupID, envelope.Model, lastDispatchError)
	writeAnthropicGatewayError(w, status, errorType, message)
}

func (a *app) classifyGatewayNoAccount(groupID, model string, cause error) (int, string, string) {
	message := "Service temporarily unavailable"
	if cause != nil {
		message = "No available accounts: " + cause.Error()
	}
	rows, err := a.db.Query(`SELECT a.extra_json
		FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND a.status = 'active' AND a.schedulable = 1`, groupID)
	if err != nil {
		return http.StatusServiceUnavailable, "api_error", message
	}
	defer rows.Close()
	hasAccounts := false
	hasModelSupport := false
	for rows.Next() {
		var extraJSON string
		if rows.Scan(&extraJSON) != nil {
			return http.StatusServiceUnavailable, "api_error", message
		}
		hasAccounts = true
		if accountSupportsModel(gatewayAccount{ExtraJSON: extraJSON}, model) {
			hasModelSupport = true
		}
	}
	if hasAccounts && !hasModelSupport {
		return http.StatusNotFound, "model_not_found", fmt.Sprintf("Model %q is not supported by any configured account in this group", model)
	}
	return http.StatusServiceUnavailable, "api_error", message
}

func validateGatewayAccountCredential(account gatewayAccount) error {
	credentials := decodeObject(account.CredentialsJSON)
	switch account.AuthType {
	case "oauth", "setup_token", "setup-token":
		if _, ok := stringObjectValue(credentials, "access_token"); !ok {
			return errors.New("access_token not found in credentials")
		}
	case "api_key":
		if _, ok := stringObjectValue(credentials, "api_key"); !ok {
			return errors.New("api_key not found in credentials")
		}
	}
	return nil
}

func writeSub2CompatibilityError(w http.ResponseWriter, upstreamStatus int, body []byte, countTokens bool) {
	if countTokens {
		message := "Upstream request failed"
		switch upstreamStatus {
		case http.StatusTooManyRequests:
			message = "Rate limit exceeded"
		case 529:
			message = "Service overloaded"
		}
		writeAnthropicGatewayError(w, upstreamStatus, "upstream_error", message)
		return
	}
	if upstreamStatus == http.StatusBadRequest {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
		return
	}
	status, errorType, message := http.StatusBadGateway, "upstream_error", "Upstream request failed"
	switch upstreamStatus {
	case http.StatusUnauthorized:
		message = "Upstream authentication failed, please contact administrator"
	case http.StatusForbidden:
		message = "Upstream access forbidden, please contact administrator"
	case http.StatusTooManyRequests:
		status, errorType, message = http.StatusTooManyRequests, "rate_limit_error", "Upstream rate limit exceeded, please retry later"
	case 529:
		status, errorType, message = http.StatusServiceUnavailable, "overloaded_error", "Upstream service overloaded, please retry later"
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		message = "Upstream service temporarily unavailable"
	}
	writeAnthropicGatewayError(w, status, errorType, message)
}

func writeAnthropicGatewayError(w http.ResponseWriter, status int, errorType, message string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errorType, "message": message},
	})
}

func gatewayAPIKeyID(ctx context.Context) int64 {
	value, _ := ctx.Value(gatewayAPIKeyContextKey{}).(int64)
	return value
}

func gatewayOpenAIChatRequest(ctx context.Context) bool {
	protocol, _ := ctx.Value(gatewayProtocolContextKey{}).(gatewayProtocolContext)
	return protocol.openAIChat
}

func gatewayNormalRequestMode(ctx context.Context) bool {
	value, _ := ctx.Value(gatewayNormalRequestModeContextKey{}).(bool)
	return value
}

func gatewayRecordedStream(ctx context.Context, upstreamStream bool) bool {
	protocol, ok := ctx.Value(gatewayProtocolContextKey{}).(gatewayProtocolContext)
	if ok && protocol.openAIChat {
		return protocol.clientStream
	}
	return upstreamStream
}

func (a *app) acquireGatewayQuotaLock(keyID int64) func() {
	value, _ := a.quotaLocks.LoadOrStore(keyID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock
}

func (a *app) acquireGatewayBudgetLock(groupID string) (func(), error) {
	var dailyLimit, monthlyLimit sql.NullFloat64
	if err := a.db.QueryRow(`SELECT daily_limit_usd, monthly_limit_usd FROM groups WHERE id = ? AND status = 'active'`, groupID).Scan(&dailyLimit, &monthlyLimit); err != nil {
		return nil, errors.New("API key group is unavailable")
	}
	if (!dailyLimit.Valid || dailyLimit.Float64 <= 0) && (!monthlyLimit.Valid || monthlyLimit.Float64 <= 0) {
		return func() {}, nil
	}
	value, _ := a.budgetLocks.LoadOrStore(groupID, &sync.Mutex{})
	lock := value.(*sync.Mutex)
	lock.Lock()
	return lock.Unlock, nil
}

func (a *app) checkGatewayGroupBudget(groupID string) error {
	var dailyLimit, monthlyLimit sql.NullFloat64
	if err := a.db.QueryRow(`SELECT daily_limit_usd, monthly_limit_usd FROM groups WHERE id = ? AND status = 'active'`, groupID).Scan(&dailyLimit, &monthlyLimit); err != nil {
		return errors.New("API key group is unavailable")
	}
	if dailyLimit.Valid && dailyLimit.Float64 > 0 {
		var spent float64
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0) FROM usage_logs WHERE group_id = ? AND created_at >= ?`, groupID, startOfTodayUTC()).Scan(&spent); err != nil {
			return err
		}
		if spent >= dailyLimit.Float64 {
			return errors.New("group daily billing limit reached")
		}
	}
	if monthlyLimit.Valid && monthlyLimit.Float64 > 0 {
		var spent float64
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0) FROM usage_logs WHERE group_id = ? AND created_at >= ?`, groupID, startOfMonthUTC()).Scan(&spent); err != nil {
			return err
		}
		if spent >= monthlyLimit.Float64 {
			return errors.New("group monthly billing limit reached")
		}
	}
	return nil
}

func gatewayMaxAttempts(rpmDispatchEnabled bool) int {
	if rpmDispatchEnabled {
		// Concentrated dispatch permits one failover without walking the entire pool.
		return 2
	}
	// The compatibility lane preserves Sub2API's initial account plus ten switches.
	return 11
}

func retryableGatewayStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return status == 529 || status >= http.StatusInternalServerError
	}
}

func doGatewayUpstreamRequest(r *http.Request, client *http.Client, request *http.Request, prepared claudePreparedRequest) (*http.Response, error) {
	poolRetryCount := 0
	for {
		response, err := doGatewayUpstreamForward(r, client, request, prepared)
		if err != nil || response == nil || prepared.Passthrough || prepared.CountTokens {
			return response, err
		}
		policy := sub2service.ResolveCCMaxCompatibilityAccountPolicy(gatewayPreparedAuthType(prepared), prepared.Credentials, response.StatusCode)
		if !policy.PoolRetryable || poolRetryCount >= policy.PoolRetryCount {
			return response, nil
		}
		poolRetryCount++
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2<<20))
		_ = response.Body.Close()
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-r.Context().Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, r.Context().Err()
		case <-timer.C:
		}
	}
}

func doGatewayUpstreamForward(r *http.Request, client *http.Client, request *http.Request, prepared claudePreparedRequest) (*http.Response, error) {
	maxAttempts := 1
	if !prepared.Passthrough && !prepared.CountTokens {
		// The actual status is evaluated below. OAuth retries only 403; API-key
		// custom policies may enable retries for other statuses.
		maxAttempts = 5
	}
	started := time.Now()
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		current := request.Clone(request.Context())
		if request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			current.Body = body
		} else if attempt > 1 {
			return nil, errors.New("upstream request body cannot be replayed")
		}
		response, err := client.Do(current)
		if err != nil || response == nil || prepared.Passthrough || prepared.CountTokens ||
			!sub2service.ShouldRetryCCMaxCompatibilityStatus(gatewayPreparedAuthType(prepared), prepared.Credentials, response.StatusCode) || attempt == maxAttempts {
			return response, err
		}
		elapsed := time.Since(started)
		if elapsed >= 10*time.Second {
			return response, nil
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 2<<20))
		_ = response.Body.Close()
		delay := 300 * time.Millisecond * time.Duration(1<<(attempt-1))
		if delay > 3*time.Second {
			delay = 3 * time.Second
		}
		if remaining := 10*time.Second - elapsed; delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-r.Context().Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, r.Context().Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("upstream request failed: empty response")
}

func skipGatewayDefaultErrorHandling(prepared claudePreparedRequest, status int) bool {
	if prepared.Passthrough || prepared.Compat == nil {
		return false
	}
	return sub2service.ResolveCCMaxCompatibilityAccountPolicy(
		gatewayPreparedAuthType(prepared), prepared.Credentials, status,
	).SkipDefaultErrorHandling
}

func gatewayPreparedAuthType(prepared claudePreparedRequest) string {
	if prepared.AuthType != "" {
		return prepared.AuthType
	}
	if prepared.OAuth {
		return "oauth"
	}
	return "api_key"
}

func retryGatewayCompatibility400(client *http.Client, request *http.Request, response *http.Response, prepared claudePreparedRequest, started time.Time) *http.Response {
	if prepared.Passthrough || prepared.Compat == nil || response == nil || response.StatusCode != http.StatusBadRequest {
		return response
	}
	originalBody, readErr := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(originalBody))
	if readErr != nil {
		return response
	}
	if time.Since(started) >= 10*time.Second {
		return response
	}

	if sub2service.IsCCMaxCompatibilitySignatureError(originalBody, prepared.Model) {
		retryPrepared, applied, err := sub2service.PrepareCCMaxCompatibilityRetry(prepared.Compat, sub2service.CCMaxCompatibilityRetryThinking)
		if err != nil || !applied {
			return response
		}
		retryResponse, retryErr := sendGatewayCompatibilityRetry(client, request, retryPrepared)
		if retryErr != nil || retryResponse == nil {
			return response
		}
		if prepared.CountTokens || retryResponse.StatusCode < 400 {
			return retryResponse
		}
		retryBody, retryReadErr := io.ReadAll(io.LimitReader(retryResponse.Body, 2<<20))
		_ = retryResponse.Body.Close()
		if retryReadErr != nil {
			return response
		}
		retryResponse.Body = io.NopCloser(bytes.NewReader(retryBody))
		if retryReadErr == nil && retryResponse.StatusCode == http.StatusBadRequest &&
			sub2service.IsCCMaxCompatibilitySignatureError(retryBody, prepared.Model) &&
			sub2service.IsCCMaxCompatibilityToolSignatureError(retryBody) && time.Since(started) < 10*time.Second {
			strongPrepared, strongApplied, strongErr := sub2service.PrepareCCMaxCompatibilityRetry(prepared.Compat, sub2service.CCMaxCompatibilityRetryThinkingTools)
			if strongErr == nil && strongApplied {
				strongResponse, sendErr := sendGatewayCompatibilityRetry(client, request, strongPrepared)
				if sendErr == nil && strongResponse != nil {
					// Sub2API treats the strong retry as the final upstream response,
					// including when it is still an error.
					return strongResponse
				}
			}
		}
		// Once the weak retry reached upstream, Sub2API returns that response
		// instead of falling back to the first 400 response.
		return retryResponse
	}

	if !prepared.CountTokens && sub2service.IsCCMaxCompatibilityBudgetError(originalBody) {
		retryPrepared, applied, err := sub2service.PrepareCCMaxCompatibilityRetry(prepared.Compat, sub2service.CCMaxCompatibilityRetryBudget)
		if err == nil && applied {
			if retryResponse, sendErr := sendGatewayCompatibilityRetry(client, request, retryPrepared); sendErr == nil && retryResponse != nil {
				return retryResponse
			}
		}
	}
	return response
}

func sendGatewayCompatibilityRetry(client *http.Client, base *http.Request, prepared *sub2service.CCMaxCompatibilityPrepared) (*http.Response, error) {
	retry, err := http.NewRequestWithContext(base.Context(), http.MethodPost, base.URL.String(), bytes.NewReader(prepared.Body))
	if err != nil {
		return nil, err
	}
	retry.Header = prepared.Headers.Clone()
	return client.Do(retry)
}

func forwardGatewayResponse(w http.ResponseWriter, response *http.Response, stream bool, account gatewayAccount, groupID string, prepared claudePreparedRequest) (tokenUsage, error) {
	if !stream || response.StatusCode < 200 || response.StatusCode >= 300 {
		copyGatewayResponseHeaders(w.Header(), response.Header)
		w.Header().Set("X-CCMAX-Account", account.Name)
		w.Header().Set("X-CCMAX-Group", groupID)
		body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		if err != nil {
			writeError(w, http.StatusBadGateway, "failed to read upstream response")
			return tokenUsage{}, err
		}
		body = restoreGatewayResponse(body, prepared)
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(body)
		return parseAnthropicUsage(body, false), err
	}
	reader := bufio.NewReaderSize(response.Body, 64<<10)
	usage := tokenUsage{}
	terminalSeen := false
	var clientWriteErr error
	committed := false
	flusher, _ := w.(http.Flusher)
	for {
		block, err := readGatewaySSEBlock(reader)
		if len(block) > 0 {
			eventType, eventBody := gatewaySSEEvent(block)
			if !committed && !prepared.Passthrough && eventType == "error" {
				status := http.StatusForbidden
				if gjson.GetBytes(eventBody, "error.type").String() == "overloaded_error" {
					status = 529
				}
				return tokenUsage{}, &gatewayPreOutputStreamError{status: status, body: eventBody}
			}
			if !committed {
				copyGatewayResponseHeaders(w.Header(), response.Header)
				w.Header().Set("X-CCMAX-Account", account.Name)
				w.Header().Set("X-CCMAX-Group", groupID)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(response.StatusCode)
				committed = true
			}
			usage = mergeTokenUsage(usage, parseAnthropicUsage(block, true))
			if eventType == "message_stop" {
				terminalSeen = true
			}
			if eventType == "error" && !prepared.Passthrough {
				writeGatewaySSEError(w, flusher, "upstream_error", "Upstream access forbidden, please contact administrator")
				return usage, errors.New("upstream stream returned an error event")
			}
			if clientWriteErr == nil {
				if _, writeErr := w.Write(restoreGatewaySSEBlock(block, prepared)); writeErr != nil {
					clientWriteErr = writeErr
				}
			}
			if clientWriteErr == nil && flusher != nil {
				flusher.Flush()
			}
			if eventType == "error" {
				return usage, errors.New("upstream stream returned an error event")
			}
		}
		if err != nil {
			if clientWriteErr != nil {
				return usage, fmt.Errorf("client disconnected during streaming: %w", clientWriteErr)
			}
			if errors.Is(err, io.EOF) {
				if terminalSeen {
					return usage, nil
				}
				if !committed {
					copyGatewayResponseHeaders(w.Header(), response.Header)
					w.Header().Set("X-CCMAX-Account", account.Name)
					w.Header().Set("X-CCMAX-Group", groupID)
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(response.StatusCode)
					committed = true
				}
				writeGatewaySSEError(w, flusher, "upstream_disconnected", "upstream stream ended before message_stop")
				return usage, errors.New("upstream stream ended before message_stop")
			}
			writeGatewaySSEError(w, flusher, "stream_read_error", "upstream stream disconnected")
			return usage, fmt.Errorf("upstream stream read failed: %w", err)
		}
	}
}

func readGatewaySSEBlock(reader *bufio.Reader) ([]byte, error) {
	var block []byte
	for {
		line, err := reader.ReadBytes('\n')
		block = append(block, line...)
		if len(bytes.TrimRight(line, "\r\n")) == 0 && len(block) > 0 {
			return block, err
		}
		if err != nil {
			return block, err
		}
	}
}

func restoreGatewayResponse(body []byte, prepared claudePreparedRequest) []byte {
	if prepared.Passthrough || prepared.Compat == nil {
		return body
	}
	return sub2service.RestoreCCMaxCompatibilityResponse(body, prepared.Compat)
}

func restoreGatewaySSELine(line []byte, prepared claudePreparedRequest) []byte {
	if prepared.Passthrough || prepared.Compat == nil {
		return line
	}
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return sub2service.RestoreCCMaxCompatibilityResponse(line, prepared.Compat)
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return line
	}
	restored := sub2service.RestoreCCMaxCompatibilityResponse(payload, prepared.Compat)
	ending := []byte{}
	if bytes.HasSuffix(line, []byte("\r\n")) {
		ending = []byte("\r\n")
	} else if bytes.HasSuffix(line, []byte("\n")) {
		ending = []byte("\n")
	}
	result := append([]byte("data: "), restored...)
	return append(result, ending...)
}

func restoreGatewaySSEBlock(block []byte, prepared claudePreparedRequest) []byte {
	if prepared.Passthrough || prepared.Compat == nil {
		return block
	}
	lines := bytes.SplitAfter(block, []byte("\n"))
	var restored []byte
	for _, line := range lines {
		if len(line) > 0 {
			restored = append(restored, restoreGatewaySSELine(line, prepared)...)
		}
	}
	return restored
}

func gatewaySSEEvent(block []byte) (string, []byte) {
	var eventType string
	var eventBody []byte
	for _, line := range bytes.Split(block, []byte("\n")) {
		value := strings.TrimSpace(string(line))
		if strings.HasPrefix(value, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(value, "event:"))
			continue
		}
		if !strings.HasPrefix(value, "data:") || len(eventBody) > 0 {
			continue
		}
		eventBody = bytes.TrimSpace(bytes.TrimPrefix([]byte(value), []byte("data:")))
		if eventType == "" {
			var payload map[string]any
			if json.Unmarshal(eventBody, &payload) == nil {
				eventType, _ = payload["type"].(string)
			}
		}
	}
	return eventType, eventBody
}

func writeGatewaySSEError(w http.ResponseWriter, flusher http.Flusher, errorType, message string) {
	payload, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": errorType, "message": message}})
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	if flusher != nil {
		flusher.Flush()
	}
}

func mergeTokenUsage(current, next tokenUsage) tokenUsage {
	current.Input = max(current.Input, next.Input)
	current.Output = max(current.Output, next.Output)
	current.CacheCreation = max(current.CacheCreation, next.CacheCreation)
	current.CacheRead = max(current.CacheRead, next.CacheRead)
	return current
}

func copyGatewayResponseHeaders(target, source http.Header) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

func (a *app) recordGatewayUsage(key gatewayKey, account gatewayAccount, model string, stream bool, requestID string, usage tokenUsage, started time.Time) {
	_, _, usageErr := a.recordUsage(usageInput{
		UserID: key.UserID, APIKeyID: key.ID, RequestID: requestID, PurposeKey: "default", GroupID: key.GroupID, AccountID: account.ID, Model: model,
		InputTokens: usage.Input, OutputTokens: usage.Output, CacheCreationTokens: usage.CacheCreation,
		CacheReadTokens: usage.CacheRead, Stream: stream, DurationMS: int(time.Since(started).Milliseconds()),
	})
	if usageErr == nil {
		return
	}
	_, _ = a.db.Exec(`UPDATE api_keys SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, key.ID)
}

func bearerOrAPIKey(r *http.Request) string {
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		parts := strings.SplitN(header, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

func (a *app) authenticateGatewayKey(secret string) (gatewayKey, error) {
	var key gatewayKey
	var normalRequestMode, streamHedgeEnabled, adaptiveHedgeEnabled int
	err := a.db.QueryRow(`SELECT k.id, k.user_id, k.group_id, k.quota, k.quota_used, u.balance, u.rpm_limit, u.allowed_group_ids_json, u.role, k.expires_at, g.normal_request_mode, g.stream_hedge_enabled, g.adaptive_hedge_enabled, g.rpm_dispatch_enabled
		FROM api_keys k JOIN users u ON u.id = k.user_id JOIN groups g ON g.id = k.group_id
		WHERE k.key_hash = ? AND k.status = 'active' AND k.deleted_at IS NULL AND u.status = 'active' AND u.deleted_at IS NULL AND g.status = 'active'
		AND (k.expires_at IS NULL OR k.expires_at > `+nowSQL+`)`, hashToken(secret)).Scan(&key.ID, &key.UserID, &key.GroupID, &key.Quota, &key.QuotaUsed, &key.UserBalance, &key.UserRPM, &key.Allowed, &key.UserRole, &key.ExpiresAt, &normalRequestMode, &streamHedgeEnabled, &adaptiveHedgeEnabled, &key.RPMDispatchEnabled)
	key.NormalRequestMode = normalRequestMode == 1
	key.StreamHedgeEnabled = streamHedgeEnabled == 1
	key.AdaptiveHedgeEnabled = adaptiveHedgeEnabled == 1
	return key, err
}

type gatewayModel = sub2claude.Model

func newGatewayModel(id string) gatewayModel {
	return gatewayModel{
		ID:          id,
		Type:        "model",
		DisplayName: id,
		CreatedAt:   "2024-01-01T00:00:00Z",
	}
}

func (a *app) handleModels(w http.ResponseWriter, r *http.Request) {
	secret := bearerOrAPIKey(r)
	if secret == "" {
		writeError(w, http.StatusUnauthorized, "API key is required")
		return
	}
	key, err := a.authenticateGatewayKey(secret)
	if err != nil || !groupAllowedJSON(key.UserRole, key.Allowed, key.GroupID) {
		writeError(w, http.StatusUnauthorized, "invalid or unavailable API key")
		return
	}
	models, err := a.gatewayModels(key.GroupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list models")
		return
	}
	requested := strings.TrimSpace(r.PathValue("id"))
	if requested != "" {
		for _, model := range models {
			if model.ID == requested {
				writeJSON(w, http.StatusOK, model)
				return
			}
		}
		writeError(w, http.StatusNotFound, "model not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (a *app) gatewayModels(groupID string) ([]gatewayModel, error) {
	accountRows, err := a.db.Query(`SELECT a.extra_json FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND a.status = 'active' AND a.schedulable = 1 AND a.auth_status != 'reauth_required'
		AND EXISTS (SELECT 1 FROM proxies p WHERE p.id = a.proxy_id AND p.status = 'active' AND p.deleted_at IS NULL)`, groupID)
	if err != nil {
		return nil, err
	}
	defer accountRows.Close()
	modelIDs := map[string]struct{}{}
	for accountRows.Next() {
		var raw string
		if err := accountRows.Scan(&raw); err != nil {
			return nil, err
		}
		extra := decodeObject(raw)
		if mapping, ok := extra["model_mapping"].(map[string]any); ok {
			for modelID := range mapping {
				if modelID = strings.TrimSpace(modelID); modelID != "" {
					modelIDs[modelID] = struct{}{}
				}
			}
		}
	}
	if err := accountRows.Err(); err != nil {
		return nil, err
	}
	if len(modelIDs) == 0 {
		return append([]gatewayModel(nil), sub2claude.DefaultModels...), nil
	}
	result := make([]gatewayModel, 0, len(modelIDs))
	for modelID := range modelIDs {
		result = append(result, newGatewayModel(modelID))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func groupAllowedJSON(role, raw, groupID string) bool {
	if role == "admin" || role == "readonly_admin" {
		return true
	}
	groups := []string{}
	_ = json.Unmarshal([]byte(raw), &groups)
	for _, group := range groups {
		if group == groupID {
			return true
		}
	}
	return false
}

func (a *app) checkAndIncrementUserRPM(key gatewayKey) error {
	if key.UserRPM <= 0 {
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		return nil
	}
	defer tx.Rollback()
	_, _ = tx.Exec(`DELETE FROM user_rpm_events WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`)
	var current int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM user_rpm_events WHERE user_id = ? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`, key.UserID).Scan(&current); err != nil {
		return nil
	}
	if current >= key.UserRPM {
		return errors.New("user RPM limit reached")
	}
	if _, err := tx.Exec(`INSERT INTO user_rpm_events (user_id) VALUES (?)`, key.UserID); err != nil {
		return nil
	}
	if err := tx.Commit(); err != nil {
		return nil
	}
	return nil
}

func gatewayClientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}

func (a *app) acquireGatewayAccount(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool) (gatewayAccount, error) {
	return a.acquireGatewayAccountWithPolicy(key, sessionHash, requestedModel, excluded, false)
}

func (a *app) acquireGatewayAccountWithPolicy(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, loadAware bool) (gatewayAccount, error) {
	if key.RPMDispatchEnabled {
		lockValue, _ := a.dispatchLocks.LoadOrStore(key.GroupID, &sync.Mutex{})
		dispatchLock := lockValue.(*sync.Mutex)
		dispatchLock.Lock()
		defer dispatchLock.Unlock()
	}

	tx, err := a.db.Begin()
	if err != nil {
		return gatewayAccount{}, err
	}
	defer tx.Rollback()
	_, _ = tx.Exec(`DELETE FROM account_rpm_events WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`)
	_, _ = tx.Exec(`DELETE FROM dispatch_sessions WHERE expires_at <= ` + nowSQL)
	var stickyID int64
	if sessionHash != "" {
		_ = tx.QueryRow(`SELECT account_id FROM dispatch_sessions WHERE session_hash = ? AND api_key_id = ? AND expires_at > `+nowSQL, sessionHash, key.ID).Scan(&stickyID)
	}
	orderClause := `ORDER BY CASE WHEN a.id = ? THEN 0 ELSE 1 END, ag.priority, a.priority`
	if key.RPMDispatchEnabled && !loadAware {
		orderClause += `, CASE WHEN current_rpm > 0 OR current_inflight > 0 THEN 0 ELSE 1 END, current_rpm DESC, current_inflight DESC, COALESCE(a.last_used_at, '') DESC, a.id`
	} else {
		orderClause += `, COALESCE(a.last_used_at, ''), a.id`
	}
	rows, err := tx.Query(`SELECT a.id, a.name, a.auth_type, a.credentials_json, a.extra_json, a.concurrency, a.base_rpm, a.rpm_strategy, a.rpm_sticky_buffer, a.user_msg_queue_mode, a.proxy_id, ag.priority, a.priority,
		COALESCE((SELECT COUNT(*) FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0) AS current_rpm,
		COALESCE((SELECT requests FROM account_inflight f WHERE f.account_id = a.id), 0) AS current_inflight
		FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND `+accountStatePredicate("a", "normal")+`
		`+orderClause, key.GroupID, stickyID)
	if err != nil {
		return gatewayAccount{}, err
	}
	type candidate struct {
		account         gatewayAccount
		groupPriority   int
		accountPriority int
		rpm             int
		inflight        int
	}
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.account.ID, &item.account.Name, &item.account.AuthType, &item.account.CredentialsJSON, &item.account.ExtraJSON, &item.account.Concurrency, &item.account.BaseRPM, &item.account.RPMStrategy, &item.account.StickyBuffer, &item.account.UserMsgQueueMode, &item.account.ProxyID, &item.groupPriority, &item.accountPriority, &item.rpm, &item.inflight); err != nil {
			rows.Close()
			return gatewayAccount{}, err
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	selectedIndex := -1
	for index, candidate := range candidates {
		if excluded[candidate.account.ID] || !accountSupportsModel(candidate.account, requestedModel) {
			continue
		}
		if candidate.inflight >= candidate.account.Concurrency {
			continue
		}
		sticky := stickyID == candidate.account.ID
		if !rpmSchedulable(candidate.account, candidate.rpm, sticky) {
			continue
		}
		if selectedIndex < 0 {
			selectedIndex = index
			if !loadAware {
				break
			}
			continue
		}
		selected := candidates[selectedIndex]
		if candidate.groupPriority != selected.groupPriority || candidate.accountPriority != selected.accountPriority {
			break
		}
		if gatewayCandidateLoad(candidate.inflight, candidate.account.Concurrency) < gatewayCandidateLoad(selected.inflight, selected.account.Concurrency) ||
			(gatewayCandidateLoad(candidate.inflight, candidate.account.Concurrency) == gatewayCandidateLoad(selected.inflight, selected.account.Concurrency) && gatewayCandidateRPMLoad(candidate.rpm, candidate.account.BaseRPM) < gatewayCandidateRPMLoad(selected.rpm, selected.account.BaseRPM)) {
			selectedIndex = index
		}
	}
	if selectedIndex < 0 {
		return gatewayAccount{}, errors.New("no account capacity or model support available for group " + strings.ToUpper(key.GroupID) + " (model, concurrency, or RPM limit)")
	}
	selected := candidates[selectedIndex].account
	if key.RPMDispatchEnabled {
		// Reserve concentrated RPM in the same transaction as selection so a
		// concurrent burst cannot overfill one account before opening the next.
		if _, err := tx.Exec(`INSERT INTO account_rpm_events (account_id) VALUES (?)`, selected.ID); err != nil {
			return gatewayAccount{}, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 1) ON CONFLICT(account_id) DO UPDATE SET requests = requests + 1`, selected.ID); err != nil {
		return gatewayAccount{}, err
	}
	if sessionHash != "" {
		expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
		if _, err := tx.Exec(`INSERT INTO dispatch_sessions (session_hash, api_key_id, account_id, expires_at) VALUES (?, ?, ?, ?) ON CONFLICT(session_hash, api_key_id) DO UPDATE SET account_id = excluded.account_id, expires_at = excluded.expires_at`, sessionHash, key.ID, selected.ID, expires); err != nil {
			return gatewayAccount{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE accounts SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, selected.ID); err != nil {
		return gatewayAccount{}, err
	}
	if err := tx.Commit(); err != nil {
		return gatewayAccount{}, err
	}
	return selected, nil
}

func gatewayCandidateLoad(current, capacity int) float64 {
	if capacity <= 0 {
		return float64(current)
	}
	return float64(current) / float64(capacity)
}

func gatewayCandidateRPMLoad(current, limit int) float64 {
	if limit <= 0 {
		return 0
	}
	return float64(current) / float64(limit)
}

func (a *app) recordGatewayAccountRPM(accountID int64) {
	_, _ = a.db.Exec(`INSERT INTO account_rpm_events (account_id) VALUES (?)`, accountID)
}

func (a *app) gatewayStickyAccountID(apiKeyID int64, sessionHash string) int64 {
	if sessionHash == "" {
		return 0
	}
	var accountID int64
	_ = a.db.QueryRow(`SELECT account_id FROM dispatch_sessions WHERE session_hash = ? AND api_key_id = ? AND expires_at > `+nowSQL, sessionHash, apiKeyID).Scan(&accountID)
	return accountID
}

func (a *app) bindGatewayStickySession(apiKeyID int64, sessionHash string, accountID int64) {
	if sessionHash == "" || accountID == 0 {
		return
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	_, _ = a.db.Exec(`INSERT INTO dispatch_sessions (session_hash, api_key_id, account_id, expires_at) VALUES (?, ?, ?, ?) ON CONFLICT(session_hash, api_key_id) DO UPDATE SET account_id = excluded.account_id, expires_at = excluded.expires_at`, sessionHash, apiKeyID, accountID, expires)
}

func rpmSchedulable(account gatewayAccount, current int, sticky bool) bool {
	extra := decodeObject(account.ExtraJSON)
	maxSessions := intFromJSON(extra["max_sessions"])
	return sub2service.IsCCMaxCompatibilityRPMSchedulable(
		account.AuthType, account.BaseRPM, account.Concurrency, account.StickyBuffer,
		int(maxSessions), current, account.RPMStrategy, sticky,
	)
}

func (a *app) releaseGatewayAccount(accountID int64) {
	_, _ = a.db.Exec(`UPDATE account_inflight SET requests = CASE WHEN requests > 0 THEN requests - 1 ELSE 0 END WHERE account_id = ?`, accountID)
}

func upstreamClaudeURL(extraJSON, endpointPath string) (string, error) {
	extra := decodeObject(extraJSON)
	value, _ := extra["custom_forward_url"].(string)
	if strings.TrimSpace(value) == "" {
		value, _ = extra["base_url"].(string)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "https://api.anthropic.com" + endpointPath + "?beta=true", nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return "", errors.New("account custom forward URL is invalid")
	}
	if parsed.Scheme == "http" {
		host := parsed.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return "", errors.New("custom forward URL must use HTTPS")
		}
	}
	path := strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{"/v1/messages/count_tokens", "/v1/messages"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	parsed.Path = strings.TrimRight(path, "/") + endpointPath
	query := parsed.Query()
	query.Set("beta", "true")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func parseAnthropicUsage(body []byte, stream bool) tokenUsage {
	usage := tokenUsage{}
	var apply func(map[string]any)
	apply = func(value map[string]any) {
		if nested, ok := value["usage"].(map[string]any); ok {
			usage.Input = max(usage.Input, intFromJSON(nested["input_tokens"]))
			usage.Output = max(usage.Output, intFromJSON(nested["output_tokens"]))
			usage.CacheCreation = max(usage.CacheCreation, intFromJSON(nested["cache_creation_input_tokens"]))
			usage.CacheRead = max(usage.CacheRead, intFromJSON(nested["cache_read_input_tokens"]))
		}
		if message, ok := value["message"].(map[string]any); ok {
			apply(message)
		}
	}
	if !stream {
		value := map[string]any{}
		_ = json.Unmarshal(body, &value)
		apply(value)
		return usage
	}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		value := map[string]any{}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &value) == nil {
			apply(value)
		}
	}
	return usage
}

func intFromJSON(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case json.Number:
		result, _ := number.Int64()
		return result
	default:
		return 0
	}
}
