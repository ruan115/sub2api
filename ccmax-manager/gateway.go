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
	"sync/atomic"
	"time"

	sub2claude "github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const defaultGatewayStreamHeartbeatInterval = 10 * time.Second

// defaultGatewayUpstreamStreamIdleTimeout bounds how long a streaming request
// may sit without a single upstream event. Our own heartbeat keeps the socket
// alive indefinitely, so without this bound a stalled upstream is only noticed
// when the client gives up minutes later.
const defaultGatewayUpstreamStreamIdleTimeout = 90 * time.Second

// gatewayStreamIdleCooldown keeps a stalled account out of the pool briefly so
// the next request does not immediately pick it again.
const gatewayStreamIdleCooldown = 2 * time.Minute

// gatewayExtraUsageCooldown parks an account that just rejected a third-party
// extra-usage request, so the retry and nearby traffic land elsewhere. The
// account stays schedulable afterwards: first-party-looking requests on the
// same org can still succeed.
const gatewayExtraUsageCooldown = 2 * time.Minute

type gatewayKey struct {
	ID                         int64
	UserID                     int64
	GroupID                    string
	Quota                      float64
	QuotaUsed                  float64
	UserBalance                sql.NullFloat64
	UserRPM                    int
	Allowed                    string
	UserRole                   string
	ExpiresAt                  sql.NullString
	NormalRequestMode          bool
	ClaudeCodeIdentityEnabled  bool
	ClaudeCLIVersion           string
	StreamHedgeEnabled         bool
	AdaptiveHedgeEnabled       bool
	RPMDispatchEnabled         bool
	MCPToolNamesEnabled        bool
	ServiceTierPassthrough     bool
	InferenceGeoPassthrough    bool
	SpeedPassthrough           bool
	AnthropicBetaPassthrough   bool
	RejectAnthropicDowngrade   bool
	RejectDistillation         bool
	RequestFormatFilter        bool
	QuotaHeaderMasking         bool
	CacheCreationDetail        bool
	DatelineNormalization      bool
	ExtraUsageFailover         bool
	OpenCodeScrub              bool
	OverloadCooldownSeconds    int
	RateLimitDownweightEnabled bool
	RateLimitCoolingThreshold  int
	RateLimitWaitSeconds       int
	RateLimitSteppedCooldown   bool
	RateLimitCooldownStep      int
	RateLimitDownweightStepped bool
	RateLimitDownweightBase    int
	RateLimitDownweightStep    int
	FiveHourStaggerEnabled     bool
	FiveHourStaggerMin         int
	FiveHourStaggerMax         int
	CapacityQueueEnabled       bool
	CapacityQueueTimeout       int
	StrategyRequiredEnabled    bool
}

type gatewayAccount struct {
	ID               int64
	Name             string
	AuthType         string
	CredentialsJSON  string
	SourceSKHint     string
	ExtraJSON        string
	Concurrency      int
	BaseRPM          int
	RPMStrategy      string
	StickyBuffer     int
	UserMsgQueueMode string
	ProxyID          sql.NullInt64
	Fingerprint      *sub2service.Fingerprint
	TLSProfile       string
	// RPMReserved is true when the RPM event was already inserted inside the
	// selection transaction, so the caller must not record it again.
	RPMReserved bool
	// ITPMReservationID tracks the request's cross-process uncached-input
	// reservation. It is released on every failure path and only after actual
	// usage has been committed on success.
	ITPMReservationID string
	EstimatedITPM     int64
	ITPMExclusive     bool
	ITPMProtection    bool
	ITPMWindowSeconds int
	ITPMSoftLimit     int64
	ITPMHardLimit     int64
	ITPMStrictHard    bool
}

type messageEnvelope struct {
	Model     string         `json:"model"`
	Stream    bool           `json:"stream"`
	MaxTokens int64          `json:"max_tokens"`
	Metadata  map[string]any `json:"metadata"`
}

type gatewaySpendState struct {
	mu      sync.Mutex
	pending float64
}

type gatewaySpendError struct {
	status    int
	errorType string
	message   string
}

func (e *gatewaySpendError) Error() string { return e.message }

type gatewayAPIKeyContextKey struct{}

type gatewayProtocolContextKey struct{}

type gatewayNormalRequestModeContextKey struct{}

type gatewayClaudeCodeIdentityContextKey struct{}

type gatewayClaudeCLIVersionContextKey struct{}

type gatewayMCPToolNamesContextKey struct{}

type gatewayDatelineNormalizationContextKey struct{}

type gatewayOpenCodeScrubContextKey struct{}

type gatewayFieldPassthroughContextKey struct{}

type gatewayProtocolContext struct {
	openAIChat               bool
	clientStream             bool
	anthropicNonStreamBridge bool
}

type gatewayFieldPassthrough struct {
	ServiceTier   bool
	InferenceGeo  bool
	Speed         bool
	AnthropicBeta bool
}

type tokenUsage struct {
	Input         int64
	Output        int64
	CacheCreation int64
	CacheRead     int64
	// CacheCreation5M and CacheCreation1H mirror the upstream
	// usage.cache_creation bucket split. Anthropic only reports it on
	// message_start, so the distilled usage patch reads it from here when it
	// rewrites message_delta.
	CacheCreation5M int64
	CacheCreation1H int64
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
	status    int
	header    http.Header
	body      []byte
	account   gatewayAccount
	preOutput bool
}

type gatewayPreOutputStreamError struct {
	status int
	body   []byte
}

type gatewayModelDowngradeError struct {
	requested string
	actual    string
	committed bool
}

// gatewayStreamIdleError reports that the upstream stopped producing stream
// events. streamed tells the caller whether real SSE events already reached the
// client: if they did the response cannot be retried on another account, since
// the partial output is already on the wire.
type gatewayStreamIdleError struct {
	idle     time.Duration
	streamed bool
}

func (e *gatewayStreamIdleError) Error() string {
	return fmt.Sprintf("upstream sent no stream events for %s", e.idle)
}

func (e *gatewayModelDowngradeError) Error() string {
	return fmt.Sprintf("upstream silently downgraded model from %s to %s", e.requested, e.actual)
}

func (e *gatewayPreOutputStreamError) Error() string {
	return "upstream stream returned an error before output"
}

func gatewaySSEErrorStatus(body []byte) int {
	switch strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, "error.type").String())) {
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return 529
	case "api_error":
		return http.StatusBadGateway
	default:
		return http.StatusBadGateway
	}
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
	if ok := groupAllowedJSON(key.UserRole, key.Allowed, key.GroupID); !ok {
		writeError(w, http.StatusForbidden, "API key group is no longer allowed")
		return
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
	if key.RejectDistillation && isDistillationProbeRequest(body) {
		attributeGatewayErrorEvent(w, "distillation_blocked", distillationRejectedStatus, "Not allowed")
		writeAnthropicGatewayError(w, distillationRejectedStatus, "permission_error", "Not allowed")
		return
	}
	if key.RequestFormatFilter {
		if formatErr := validateFilteredRequestFormat(body); formatErr != nil {
			message := formatErr.Error()
			attributeGatewayErrorEvent(w, requestFormatBlockedCategory, http.StatusBadRequest, message)
			writeAnthropicGatewayError(w, http.StatusBadRequest, "invalid_request_error", message)
			return
		}
	}
	if !countTokens && !envelope.Stream && !gatewayOpenAIChatRequest(r.Context()) {
		adapter := newAnthropicNonStreamResponseWriter(w)
		w = adapter
		r = r.WithContext(context.WithValue(r.Context(), gatewayProtocolContextKey{}, gatewayProtocolContext{
			clientStream: false, anthropicNonStreamBridge: true,
		}))
		defer adapter.finish()
	}
	spendRelease, spendErr := a.reserveGatewaySpend(key, envelope, len(body), countTokens)
	if spendErr != nil {
		if spendErr.errorType != "" {
			writeAnthropicGatewayError(w, spendErr.status, spendErr.errorType, spendErr.message)
		} else {
			writeError(w, spendErr.status, spendErr.message)
		}
		return
	}
	defer spendRelease()
	if err := a.checkAndIncrementUserRPM(key); err != nil {
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), gatewayAPIKeyContextKey{}, key.ID))
	r = r.WithContext(context.WithValue(r.Context(), gatewayNormalRequestModeContextKey{}, key.NormalRequestMode))
	r = r.WithContext(context.WithValue(r.Context(), gatewayClaudeCodeIdentityContextKey{}, key.ClaudeCodeIdentityEnabled))
	r = r.WithContext(context.WithValue(r.Context(), gatewayClaudeCLIVersionContextKey{}, key.ClaudeCLIVersion))
	r = r.WithContext(context.WithValue(r.Context(), gatewayMCPToolNamesContextKey{}, key.MCPToolNamesEnabled))
	r = r.WithContext(context.WithValue(r.Context(), gatewayDatelineNormalizationContextKey{}, key.DatelineNormalization))
	r = r.WithContext(context.WithValue(r.Context(), gatewayOpenCodeScrubContextKey{}, key.OpenCodeScrub))
	r = r.WithContext(context.WithValue(r.Context(), gatewayFieldPassthroughContextKey{}, gatewayFieldPassthrough{
		ServiceTier:   key.ServiceTierPassthrough,
		InferenceGeo:  key.InferenceGeoPassthrough,
		Speed:         key.SpeedPassthrough,
		AnthropicBeta: key.AnthropicBetaPassthrough,
	}))
	session := sub2service.GenerateCCMaxCompatibilitySessionHash(body, gatewayClientIP(r), r.UserAgent(), key.ID)
	demand := estimateGatewayDispatchDemand(body, countTokens)
	excluded := map[int64]bool{}
	var lastFailure *gatewayUpstreamFailure
	var lastDispatchError error
	if !demand.Oversized && !key.RPMDispatchEnabled && (key.StreamHedgeEnabled || key.AdaptiveHedgeEnabled) && envelope.Stream && !countTokens && a.gatewayStickyAccountID(key.ID, session) == 0 {
		handled, hedgeFailure, hedgeErr := a.handleGatewayStreamHedge(w, r, key, body, envelope.Model, session, excluded, key.AdaptiveHedgeEnabled, demand)
		if handled {
			return
		}
		lastFailure = hedgeFailure
		lastDispatchError = hedgeErr
	}
	forbiddenFailures := 0
	if lastFailure != nil && lastFailure.status == http.StatusForbidden {
		forbiddenFailures = 1
	}
	strategyLimitsFailover := a.groupUsesRPMReservationStrategy(key.GroupID)
	maxAccountAttempts := gatewayRequestMaxAttempts(key.RPMDispatchEnabled || strategyLimitsFailover, envelope, len(body))
	if countTokens {
		// Sub2API count_tokens selects one account and returns that upstream result.
		maxAccountAttempts = 1
	}
	// excluded also contains accounts already consumed by stream hedging. Keeping
	// one shared budget prevents a failed two-account hedge from falling through
	// to another full pass over the pool.
	// A large request on a warm session stays on its own account: failing over
	// discards the cached prefix and re-creates it elsewhere, turning a nearly
	// free cache read into a full-price cache write — which is what makes one
	// 429 cascade into several. The pin covers ordinary capacity pressure, but a
	// learned 429 RPM ceiling or an actual retryable upstream failure releases it:
	// at that point availability and rate-limit safety outweigh cache affinity.
	pinLarge := !countTokens && gatewayRequestHoldsLargeCache(len(body))
	releaseStickyPin := false
	for len(excluded) < maxAccountAttempts {
		if pinLarge && !releaseStickyPin && stickyAlreadyTried(excluded, a.gatewayStickyAccountID(key.ID, session)) {
			// The pinned account is the only one allowed and it has already failed
			// this request. Retrying would search an empty candidate set and, with
			// queueing on, block for the full timeout before failing anyway.
			break
		}
		account, acquireErr := a.acquireGatewayAccountPinnedDemand(key, session, envelope.Model, excluded, false, pinLarge && !releaseStickyPin, demand)
		if acquireErr != nil && !countTokens && errors.Is(acquireErr, errNoGatewayAccountCapacity) && (a.gatewayShouldQueue(key) || gatewayITPMCapacityBlocked(acquireErr) || gatewayPacingBlocked(acquireErr)) {
			account, acquireErr = a.waitForGatewayCapacityPinnedDemand(r.Context(), key, session, envelope.Model, excluded, acquireErr, pinLarge && !releaseStickyPin, demand)
			if acquireErr != nil && recordGatewayContextFailure(w, r, acquireErr) {
				return
			}
		}
		if acquireErr != nil {
			if pinLarge && !releaseStickyPin && errors.Is(acquireErr, errNoGatewayAccountCapacity) && gatewayStickyWaitExhausted(acquireErr) {
				// The pinned account did not free ITPM budget within the queue
				// window. Rebuilding beats failing: drop the session binding so
				// this acquisition places the request like a cold start — on the
				// account with the most input budget — and the account it lands
				// on becomes the session's new sticky home. The session is never
				// migrated back; its cache now lives there.
				a.releaseGatewaySessionBinding(session, key.ID)
				releaseStickyPin = true
				continue
			}
			lastDispatchError = acquireErr
			break
		}
		excluded[account.ID] = true
		attributeGatewayErrorAccount(w, account.ID)
		stopITPMKeepAlive := a.keepGatewayITPMReservation(account)
		// Only one large request per (account, session) talks to the upstream at
		// a time. Concurrent siblings would each miss the cache the first one is
		// still writing and pay a full creation for the same prefix.
		flightRelease, flightErr := a.acquireColdCacheFlight(r.Context(), account.ID, session, pinLarge)
		if flightErr != nil {
			stopITPMKeepAlive()
			a.releaseGatewayAccount(account.ID)
			a.releaseGatewayITPM(account)
			recordGatewayContextFailure(w, r, flightErr)
			return
		}
		// The cache is written while the response streams, so the flight has to
		// outlive forwardGatewayResponse. releaseGatewayAccount already marks
		// every path that abandons this attempt, including the success path
		// after forwarding, so pairing the two keeps the lifetimes identical.
		releaseAccount := func() {
			stopITPMKeepAlive()
			flightRelease()
			a.releaseGatewayAccount(account.ID)
			a.releaseGatewayITPM(account)
		}
		releaseExecution := func() {
			stopITPMKeepAlive()
			flightRelease()
			a.releaseGatewayAccount(account.ID)
		}
		account, err = a.ensureGatewayAccountToken(r.Context(), account)
		if err != nil {
			releaseAccount()
			if recordGatewayContextFailure(w, r, err) {
				return
			}
			message := "Upstream request failed"
			if countTokens {
				message = "Failed to get access token"
			}
			writeAnthropicGatewayError(w, http.StatusBadGateway, "upstream_error", message)
			return
		}
		if err := validateGatewayAccountCredential(account); err != nil {
			releaseAccount()
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
			releaseAccount()
			writeError(w, http.StatusBadRequest, prepareErr.Error())
			return
		}
		prepared.RejectAnthropicDowngrade = key.RejectAnthropicDowngrade
		prepared.MaskQuotaHeaders = key.QuotaHeaderMasking
		prepared.CacheCreationDetail = key.CacheCreationDetail
		path := "/v1/messages"
		if countTokens {
			path = "/v1/messages/count_tokens"
		}
		upstreamURL, urlErr := upstreamClaudeURL(account.ExtraJSON, path)
		if urlErr != nil {
			releaseAccount()
			if !prepared.Passthrough {
				writeAnthropicGatewayError(w, http.StatusBadGateway, "upstream_error", "Upstream request failed")
				return
			}
			lastDispatchError = urlErr
			continue
		}
		upstreamRequest, requestErr := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(prepared.Body))
		if requestErr != nil {
			releaseAccount()
			lastDispatchError = requestErr
			continue
		}
		// Observation only: records when the cacheable prefix of a session changes,
		// so we can tell a cache miss caused by our own injections apart from one
		// caused by an account switch or a TTL expiry. Nothing here edits the body.
		a.recordCachePrefixChange(session, account.ID, envelope.Model, prepared.Body)
		if headerErr := buildClaudeHeaders(upstreamRequest.Header, r.Header, prepared, account.CredentialsJSON); headerErr != nil {
			releaseAccount()
			lastDispatchError = headerErr
			continue
		}
		if !account.ProxyID.Valid {
			releaseAccount()
			lastDispatchError = errors.New("CCMAX account must bind an active proxy")
			continue
		}
		proxyURL, err := a.proxyURL(account.ProxyID.Int64)
		if err != nil {
			releaseAccount()
			lastDispatchError = errors.New("CCMAX account proxy is unavailable")
			continue
		}
		client, clientErr := clientForAccountProxy(proxyURL, account)
		if clientErr != nil {
			releaseAccount()
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
		if !account.RPMReserved {
			a.recordGatewayAccountRPM(account.ID)
		}
		response, requestErr := doGatewayUpstreamRequest(r, client, upstreamRequest, prepared)
		if requestErr != nil {
			queueRelease()
			releaseAccount()
			if recordGatewayContextFailure(w, r, requestErr) {
				return
			}
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
		var oauthRefreshHandled bool
		response, account, prepared, oauthRefreshHandled, requestErr = a.retryGatewayRevokedOAuth(
			r, client, upstreamURL, body, envelope.Model, countTokens, key, account, prepared, response,
		)
		if requestErr != nil {
			queueRelease()
			releaseAccount()
			if recordGatewayContextFailure(w, r, requestErr) {
				return
			}
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
		if !oauthRefreshHandled && !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
			if response.StatusCode == http.StatusTooManyRequests {
				_, _ = a.ensureReserveCapacity(key.GroupID, envelope.Model, "rate_limit", excluded)
			}
			a.captureGatewayUpstreamState(account.ID, key.GroupID, envelope.Model, key.OverloadCooldownSeconds, key.rateLimitPolicy(), response)
		}
		if response.StatusCode == http.StatusBadRequest && gatewayRequestDiagnosticArmed() {
			failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(failureBody))
			a.captureGatewayRequestDiagnosticsOnce(r, key, account, prepared, response.Header, failureBody)
		}
		if retryableGatewayStatus(response.StatusCode) {
			queueRelease()
			failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			releaseAccount()
			if !oauthRefreshHandled && !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
				a.captureAccountUpstreamFailure(account, response.StatusCode, failureBody)
			}
			lastFailure = &gatewayUpstreamFailure{status: response.StatusCode, header: response.Header.Clone(), body: failureBody, account: account}
			// 401/403/5xx mean the account itself is suspect, so availability
			// outweighs the cache and the retry may move on. A 429 does not: the
			// account is merely rate limited, and moving a large warm request
			// re-creates its cached prefix elsewhere at full price — which is what
			// pushed the account into the limit to begin with. Only permanent
			// account failure justifies migrating and rebuilding the cache.
			if response.StatusCode != http.StatusTooManyRequests {
				releaseStickyPin = true
			}
			if response.StatusCode == http.StatusForbidden {
				forbiddenFailures++
				if forbiddenFailures >= gatewayForbiddenFailoverAttempts {
					break
				}
			}
			continue
		}
		if key.ExtraUsageFailover && response.StatusCode == http.StatusBadRequest {
			failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			if extraUsageFailoverEligible(true, response.StatusCode, failureBody) {
				queueRelease()
				releaseAccount()
				if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
					a.captureAccountExtraUsageRejection(account.ID)
				}
				lastFailure = &gatewayUpstreamFailure{status: response.StatusCode, header: response.Header.Clone(), body: failureBody, account: account}
				releaseStickyPin = true
				continue
			}
			response.Body = io.NopCloser(bytes.NewReader(failureBody))
		}
		if response.StatusCode >= 400 && !prepared.Passthrough {
			queueRelease()
			failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
			response.Body.Close()
			releaseAccount()
			if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
				a.captureAccountUpstreamFailure(account, response.StatusCode, failureBody)
			}
			attributeGatewayUpstreamError(w, response.StatusCode, failureBody, countTokens)
			writeSub2CompatibilityError(w, response.StatusCode, failureBody, countTokens)
			return
		}
		queueRelease()
		usage, forwardErr := forwardGatewayResponse(w, response, prepared.Stream && !countTokens, account, key.GroupID, prepared)
		response.Body.Close()
		usageCommitted := false
		var earlyDowngradeErr *gatewayModelDowngradeError
		if response.StatusCode >= 200 && response.StatusCode < 300 && !countTokens && (forwardErr == nil || usage.hasUsage()) {
			if errors.As(forwardErr, &earlyDowngradeErr) {
				if usage.hasUsage() {
					usageCommitted = a.recordGatewayRejectedDowngradeUsage(r.Context(), key, account, earlyDowngradeErr.actual, gatewayRecordedStream(r.Context(), prepared.Stream), response.Header.Get("request-id"), usage, started)
				}
			} else {
				usageCommitted = a.recordGatewayUsage(r.Context(), key, account, envelope.Model, gatewayRecordedStream(r.Context(), prepared.Stream), response.Header.Get("request-id"), usage, started)
			}
		}
		releaseExecution()
		if usageCommitted || countTokens || response.StatusCode < 200 || response.StatusCode >= 300 {
			a.releaseGatewayITPM(account)
		} else {
			a.settleGatewayITPM(account, usage)
		}
		if !countTokens && response.StatusCode >= 200 && response.StatusCode < 300 {
			a.updateGatewaySessionCost(session, key.ID, usage)
		}
		var preOutputErr *gatewayPreOutputStreamError
		if errors.As(forwardErr, &preOutputErr) {
			a.captureGatewayRequestDiagnosticsOnce(r, key, account, prepared, response.Header, preOutputErr.body)
			if !skipGatewayDefaultErrorHandling(prepared, preOutputErr.status) {
				if extraUsageFailoverEligible(key.ExtraUsageFailover, preOutputErr.status, preOutputErr.body) {
					a.captureAccountExtraUsageRejection(account.ID)
				} else {
					if preOutputErr.status == http.StatusTooManyRequests {
						_, _ = a.ensureReserveCapacity(key.GroupID, envelope.Model, "rate_limit", excluded)
					}
					a.captureGatewayUpstreamState(account.ID, key.GroupID, envelope.Model, key.OverloadCooldownSeconds, key.rateLimitPolicy(), &http.Response{StatusCode: preOutputErr.status, Header: response.Header.Clone()})
					a.captureAccountUpstreamFailure(account, preOutputErr.status, preOutputErr.body)
				}
			}
			lastFailure = &gatewayUpstreamFailure{status: preOutputErr.status, header: response.Header.Clone(), body: preOutputErr.body, account: account, preOutput: true}
			// Same rule as the header-status path above: rate limiting keeps the
			// pin, account-level failure releases it.
			if preOutputErr.status != http.StatusTooManyRequests {
				releaseStickyPin = true
			}
			if preOutputErr.status == http.StatusForbidden {
				forbiddenFailures++
				if forbiddenFailures >= gatewayForbiddenFailoverAttempts {
					break
				}
			}
			continue
		}
		var idleErr *gatewayStreamIdleError
		if errors.As(forwardErr, &idleErr) {
			a.captureGatewayStreamIdle(account.ID, idleErr.idle)
			if idleErr.streamed {
				// The client already holds part of this answer; it was told the
				// stream failed and must retry the request itself.
				attributeGatewayErrorEvent(w, "upstream_stream_idle", http.StatusGatewayTimeout, idleErr.Error())
				return
			}
			attributeGatewayErrorEvent(w, "upstream_stream_idle", http.StatusGatewayTimeout, idleErr.Error())
			lastDispatchError = idleErr
			continue
		}
		var downgradeErr *gatewayModelDowngradeError
		if errors.As(forwardErr, &downgradeErr) {
			if !downgradeErr.committed {
				copyGatewayRequestID(w.Header(), response.Header)
				writeAnthropicGatewayError(w, http.StatusBadGateway, "api_error", downgradeErr.Error())
			}
			return
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			// Success deliberately does not lift a rate-limit penalty. The
			// configured downweight deadline, an explicit upstream quota reset,
			// or an administrator action clears it.
			if countTokens {
				if forwardErr != nil {
					return
				}
				_, writeErr := a.db.Exec(`UPDATE api_keys SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, key.ID)
				logDatabaseWriteError("update API key last-used time", writeErr)
				return
			}
		}
		if forwardErr != nil {
			recordGatewayContextFailure(w, r, forwardErr)
			return
		}
		return
	}
	if lastFailure != nil {
		attributeGatewayUpstreamError(w, lastFailure.status, lastFailure.body, countTokens)
		if gatewayResponseStatus(w) != 0 {
			// A previous attempt already sent the status line (stream heartbeat),
			// so the only way left to report the failure is an SSE error event.
			_, errorType, message := sub2CompatibilityErrorWithBody(lastFailure.status, lastFailure.body)
			flusher, _ := w.(http.Flusher)
			writeGatewaySSEError(w, flusher, errorType, message)
			return
		}
		if !accountRequestPassthrough(lastFailure.account) || lastFailure.preOutput {
			writeSub2CompatibilityError(w, lastFailure.status, lastFailure.body, countTokens)
			return
		}
		copyGatewayResponseHeaders(w.Header(), lastFailure.header, key.QuotaHeaderMasking)
		setGatewayAttributionHeaders(w.Header(), lastFailure.account.Name, key.GroupID, key.QuotaHeaderMasking)
		w.WriteHeader(lastFailure.status)
		_, _ = w.Write(lastFailure.body)
		return
	}
	status, errorType, message := a.classifyGatewayNoAccount(key.GroupID, envelope.Model, lastDispatchError)
	attributeGatewayCapacityDiagnostics(w, lastDispatchError)
	if _, ok := gatewayCapacityDiagnosticsFromError(lastDispatchError); ok {
		attributeGatewayErrorEvent(w, "gateway_capacity", status, message)
	}
	if gatewayResponseStatus(w) != 0 {
		flusher, _ := w.(http.Flusher)
		writeGatewaySSEError(w, flusher, errorType, message)
		return
	}
	writeAnthropicGatewayError(w, status, errorType, message)
}

// captureGatewayStreamIdle parks an account that accepted a stream but produced
// no events, so the retry lands on a different one.
func (a *app) captureGatewayStreamIdle(accountID int64, idle time.Duration) {
	until := time.Now().UTC().Add(gatewayStreamIdleCooldown).Format(time.RFC3339Nano)
	message := fmt.Sprintf("上游 %s 未产出流式事件，已临时下线 %s", idle, gatewayStreamIdleCooldown)
	_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, error_message = ?, updated_at = `+nowSQL+`
		WHERE id = ? AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at < ?)`, until, message, accountID, until)
	logDatabaseWriteError("record stream idle cooldown", err)
}

func recordGatewayContextFailure(w http.ResponseWriter, r *http.Request, err error) bool {
	requestErr := r.Context().Err()
	if errors.Is(requestErr, context.Canceled) || errors.Is(err, context.Canceled) {
		attributeGatewayErrorEvent(w, "client_canceled", 499, "Client canceled request before completion")
		return true
	}
	if errors.Is(requestErr, context.DeadlineExceeded) {
		attributeGatewayErrorEvent(w, "timeout", http.StatusRequestTimeout, "Client request timed out before completion")
		return true
	}
	var networkErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkErr) && networkErr.Timeout()) {
		attributeGatewayErrorEvent(w, "timeout", http.StatusGatewayTimeout, "Upstream request timed out before completion")
		if gatewayResponseStatus(w) == 0 {
			writeAnthropicGatewayError(w, http.StatusGatewayTimeout, "timeout_error", "Upstream request timed out before completion")
		}
		return true
	}
	return false
}

func (k gatewayKey) rateLimitPolicy() rateLimitPolicy {
	return rateLimitPolicy{
		DownweightEnabled:      k.RateLimitDownweightEnabled,
		CoolingThreshold:       k.RateLimitCoolingThreshold,
		CooldownSeconds:        k.RateLimitWaitSeconds,
		SteppedCooldownEnabled: k.RateLimitSteppedCooldown,
		CooldownStepSeconds:    k.RateLimitCooldownStep,
		DownweightStepped:      k.RateLimitDownweightStepped,
		DownweightBaseMinutes:  k.RateLimitDownweightBase,
		DownweightStepMinutes:  k.RateLimitDownweightStep,
		FiveHourStaggerSet:     true,
		FiveHourStaggerEnabled: k.FiveHourStaggerEnabled,
		FiveHourStaggerMin:     k.FiveHourStaggerMin,
		FiveHourStaggerMax:     k.FiveHourStaggerMax,
	}
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
	if upstreamStatus == http.StatusUnauthorized {
		if message, ok := gatewayOAuthRefreshCompatibilityMessage(body); ok {
			writeAnthropicGatewayError(w, http.StatusBadGateway, "upstream_error", message)
			return
		}
	}
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
	status, errorType, message := sub2CompatibilityErrorWithBody(upstreamStatus, body)
	writeAnthropicGatewayError(w, status, errorType, message)
}

func sub2CompatibilityErrorWithBody(upstreamStatus int, body []byte) (int, string, string) {
	if upstreamStatus == http.StatusUnauthorized {
		if message, ok := gatewayOAuthRefreshCompatibilityMessage(body); ok {
			return http.StatusBadGateway, "upstream_error", message
		}
	}
	return sub2CompatibilityError(upstreamStatus)
}

func sub2CompatibilityError(upstreamStatus int) (int, string, string) {
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
	return status, errorType, message
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

func gatewayClaudeCodeIdentity(ctx context.Context) bool {
	value, _ := ctx.Value(gatewayClaudeCodeIdentityContextKey{}).(bool)
	return value
}

func gatewayClaudeCLIVersion(ctx context.Context) string {
	value, _ := ctx.Value(gatewayClaudeCLIVersionContextKey{}).(string)
	return value
}

func gatewayMCPToolNames(ctx context.Context) bool {
	value, _ := ctx.Value(gatewayMCPToolNamesContextKey{}).(bool)
	return value
}

// gatewayDatelineNormalization defaults to true when unset so any path that
// does not populate the context keeps the anti-fingerprint normalization on.
func gatewayDatelineNormalization(ctx context.Context) bool {
	value, ok := ctx.Value(gatewayDatelineNormalizationContextKey{}).(bool)
	if !ok {
		return true
	}
	return value
}

func gatewayOpenCodeScrub(ctx context.Context) bool {
	value, _ := ctx.Value(gatewayOpenCodeScrubContextKey{}).(bool)
	return value
}

func gatewayFieldPassthroughConfig(ctx context.Context) gatewayFieldPassthrough {
	value, _ := ctx.Value(gatewayFieldPassthroughContextKey{}).(gatewayFieldPassthrough)
	return value
}

func gatewayRecordedStream(ctx context.Context, upstreamStream bool) bool {
	protocol, ok := ctx.Value(gatewayProtocolContextKey{}).(gatewayProtocolContext)
	if ok && (protocol.openAIChat || protocol.anthropicNonStreamBridge) {
		return protocol.clientStream
	}
	return upstreamStream
}

func gatewayAnthropicNonStreamBridge(ctx context.Context) bool {
	protocol, _ := ctx.Value(gatewayProtocolContextKey{}).(gatewayProtocolContext)
	return protocol.anthropicNonStreamBridge
}

func gatewaySpendStateFor(store *sync.Map, key any) *gatewaySpendState {
	value, _ := store.LoadOrStore(key, &gatewaySpendState{})
	return value.(*gatewaySpendState)
}

func gatewaySpendReservation(limit, spent, pending, estimate float64) (float64, bool) {
	if limit <= 0 {
		return 0, true
	}
	remaining := money(limit - spent - pending)
	if remaining <= 0 {
		return 0, false
	}
	if estimate <= 0 {
		return 0, true
	}
	if estimate > remaining {
		return remaining, true
	}
	return estimate, true
}

func (a *app) estimateGatewayBilledCost(groupRate float64, envelope messageEnvelope, bodyBytes int) float64 {
	if groupRate <= 0 {
		return 0
	}
	var inputRate, outputRate, cacheCreateRate, cacheReadRate float64
	err := a.db.QueryRow(`SELECT input_per_million, output_per_million, cache_creation_per_million, cache_read_per_million
		FROM model_prices WHERE model IN (?, '*') ORDER BY CASE WHEN model = ? THEN 0 ELSE 1 END LIMIT 1`, envelope.Model, envelope.Model).
		Scan(&inputRate, &outputRate, &cacheCreateRate, &cacheReadRate)
	if err != nil {
		inputRate, outputRate, cacheCreateRate, cacheReadRate = 3, 15, 3.75, 0.3
	}
	maxTokens := envelope.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	inputTokens := int64(max(bodyBytes, 1))
	inputRate = max(inputRate, cacheCreateRate, cacheReadRate)
	estimate := (float64(inputTokens)*inputRate + float64(maxTokens)*outputRate) / 1_000_000
	return money(estimate * groupRate)
}

func (a *app) reserveGatewaySpend(key gatewayKey, envelope messageEnvelope, bodyBytes int, countTokens bool) (func(), *gatewaySpendError) {
	quotaState := gatewaySpendStateFor(&a.quotaLocks, key.ID)
	balanceState := gatewaySpendStateFor(&a.balanceLocks, key.UserID)
	budgetState := gatewaySpendStateFor(&a.budgetLocks, key.GroupID)
	quotaState.mu.Lock()
	defer quotaState.mu.Unlock()
	balanceState.mu.Lock()
	defer balanceState.mu.Unlock()
	budgetState.mu.Lock()
	defer budgetState.mu.Unlock()

	var quota, quotaUsed float64
	var balance sql.NullFloat64
	if err := a.db.QueryRow(`SELECT k.quota, k.quota_used, u.balance FROM api_keys k JOIN users u ON u.id = k.user_id
		WHERE k.id = ? AND k.user_id = ? AND k.status = 'active' AND k.deleted_at IS NULL AND u.status = 'active' AND u.deleted_at IS NULL`, key.ID, key.UserID).
		Scan(&quota, &quotaUsed, &balance); err != nil {
		return nil, &gatewaySpendError{status: http.StatusUnauthorized, message: "invalid or unavailable API key"}
	}
	var groupRate float64
	var dailyLimit, monthlyLimit sql.NullFloat64
	if err := a.db.QueryRow(`SELECT rate_multiplier, daily_limit_usd, monthly_limit_usd FROM groups WHERE id = ? AND status = 'active'`, key.GroupID).
		Scan(&groupRate, &dailyLimit, &monthlyLimit); err != nil {
		return nil, &gatewaySpendError{status: http.StatusForbidden, message: "API key group is unavailable"}
	}

	estimate := 0.0
	if !countTokens {
		estimate = a.estimateGatewayBilledCost(groupRate, envelope, bodyBytes)
	}
	quotaReservation, ok := gatewaySpendReservation(quota, quotaUsed, quotaState.pending, estimate)
	if !ok {
		return nil, &gatewaySpendError{status: http.StatusForbidden, errorType: "permission_error", message: "API key quota exhausted"}
	}
	balanceReservation := 0.0
	if balance.Valid {
		remainingBalance := money(balance.Float64 - balanceState.pending)
		if remainingBalance <= 0 {
			return nil, &gatewaySpendError{status: http.StatusPaymentRequired, errorType: "billing_error", message: "User balance exhausted"}
		}
		if estimate > 0 {
			balanceReservation = min(estimate, remainingBalance)
		}
	}

	dailySpent, monthlySpent := 0.0, 0.0
	if !countTokens && dailyLimit.Valid && dailyLimit.Float64 > 0 {
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0) FROM usage_logs WHERE group_id = ? AND created_at >= ?`, key.GroupID, startOfTodayUTC()).Scan(&dailySpent); err != nil {
			return nil, &gatewaySpendError{status: http.StatusInternalServerError, message: "failed to check group daily billing limit"}
		}
		if _, ok = gatewaySpendReservation(dailyLimit.Float64, dailySpent, budgetState.pending, estimate); !ok {
			return nil, &gatewaySpendError{status: http.StatusForbidden, message: "group daily billing limit reached"}
		}
	}
	if !countTokens && monthlyLimit.Valid && monthlyLimit.Float64 > 0 {
		if err := a.db.QueryRow(`SELECT COALESCE(SUM(billed_cost), 0) FROM usage_logs WHERE group_id = ? AND created_at >= ?`, key.GroupID, startOfMonthUTC()).Scan(&monthlySpent); err != nil {
			return nil, &gatewaySpendError{status: http.StatusInternalServerError, message: "failed to check group monthly billing limit"}
		}
		if _, ok = gatewaySpendReservation(monthlyLimit.Float64, monthlySpent, budgetState.pending, estimate); !ok {
			return nil, &gatewaySpendError{status: http.StatusForbidden, message: "group monthly billing limit reached"}
		}
	}
	budgetReservation := 0.0
	if !countTokens {
		budgetReservation = estimate
		if dailyLimit.Valid && dailyLimit.Float64 > 0 {
			budgetReservation = min(budgetReservation, money(dailyLimit.Float64-dailySpent-budgetState.pending))
		}
		if monthlyLimit.Valid && monthlyLimit.Float64 > 0 {
			budgetReservation = min(budgetReservation, money(monthlyLimit.Float64-monthlySpent-budgetState.pending))
		}
		if budgetReservation < 0 {
			budgetReservation = 0
		}
	}
	quotaState.pending = money(quotaState.pending + quotaReservation)
	balanceState.pending = money(balanceState.pending + balanceReservation)
	budgetState.pending = money(budgetState.pending + budgetReservation)

	var once sync.Once
	return func() {
		once.Do(func() {
			quotaState.mu.Lock()
			quotaState.pending = max(0, money(quotaState.pending-quotaReservation))
			quotaState.mu.Unlock()
			balanceState.mu.Lock()
			balanceState.pending = max(0, money(balanceState.pending-balanceReservation))
			balanceState.mu.Unlock()
			budgetState.mu.Lock()
			budgetState.pending = max(0, money(budgetState.pending-budgetReservation))
			budgetState.mu.Unlock()
		})
	}, nil
}

func gatewayMaxAttempts(rpmDispatchEnabled bool) int {
	if rpmDispatchEnabled {
		// Concentrated dispatch permits one failover without walking the entire pool.
		return 2
	}
	// The compatibility lane preserves Sub2API's initial account plus ten switches.
	return 11
}

const (
	gatewayLargeRequestBodyBytes = 512 << 10
	gatewayLargeRequestMaxTokens = 16_384
	// gatewayCachePinBodyBytes is the body size from which a warm session is
	// pinned to its bound account (queue first, rebuild elsewhere only after
	// the queue window). 128KB ≈ 40k prompt tokens — sessions in the
	// 100k-150k token band were previously unprotected by the 512KB bound and
	// kept losing their cache to account switches.
	gatewayCachePinBodyBytes = 128 << 10
)

func gatewayRequestIsLarge(envelope messageEnvelope, bodyBytes int) bool {
	return bodyBytes >= gatewayLargeRequestBodyBytes || envelope.MaxTokens >= gatewayLargeRequestMaxTokens
}

// gatewayRequestHoldsLargeCache reports whether a request carries enough context
// to make its cached prefix worth protecting. It deliberately ignores
// max_tokens: that is the output cap and says nothing about how much cache is at
// stake, and Claude Code routinely sends 32k-64k on ordinary requests — keying
// cache affinity off it would pin and serialise essentially all traffic.
func gatewayRequestHoldsLargeCache(bodyBytes int) bool {
	return bodyBytes >= gatewayCachePinBodyBytes
}

func gatewayRequestMaxAttempts(strategyLimited bool, envelope messageEnvelope, bodyBytes int) int {
	limit := gatewayMaxAttempts(strategyLimited)
	if gatewayRequestIsLarge(envelope, bodyBytes) && limit > 2 {
		// Large prompts and generations are expensive to replay. One failover is
		// enough to distinguish a bad account from a bad request without walking
		// the whole compatibility pool.
		return 2
	}
	return limit
}

// A forbidden response usually identifies an account, organization, or proxy
// policy problem. Trying the entire pool only amplifies one client request into
// a long chain of identical upstream failures.
const gatewayForbiddenFailoverAttempts = 2

func retryableGatewayStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return true
	default:
		return status == 529 || status >= http.StatusInternalServerError
	}
}

func extraUsageFailoverEligible(enabled bool, status int, body []byte) bool {
	return enabled && status == http.StatusBadRequest && accountExtraUsageRejected(upstreamErrorMessage(body))
}

func (a *app) captureAccountExtraUsageRejection(accountID int64) {
	until := time.Now().UTC().Add(gatewayExtraUsageCooldown).Format(time.RFC3339Nano)
	message := fmt.Sprintf("上游拒绝第三方 extra usage，已临时下线 %s", gatewayExtraUsageCooldown)
	_, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, error_message = ?, updated_at = `+nowSQL+`
		WHERE id = ? AND (rate_limit_reset_at IS NULL OR rate_limit_reset_at < ?)`, until, message, accountID, until)
	logDatabaseWriteError("record extra usage cooldown", err)
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
	authType := gatewayPreparedAuthType(prepared)
	if !prepared.Passthrough && !prepared.CountTokens && authType != "oauth" && authType != "setup_token" && authType != "setup-token" {
		// OAuth 403s are account or proxy policy failures and immediately fail
		// over. API-key custom policies may still retry the same account.
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
	upstreamSSE := strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	if !stream || response.StatusCode < 200 || response.StatusCode >= 300 || (prepared.NonStreamBridge && !upstreamSSE) {
		body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		if err != nil {
			var networkErr net.Error
			if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &networkErr) && networkErr.Timeout()) {
				writeError(w, http.StatusBadGateway, "failed to read upstream response")
			}
			return tokenUsage{}, err
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			if downgrade := detectGatewayAnthropicModelDowngrade(body, prepared); downgrade != nil {
				return parseAnthropicUsage(body, false), downgrade
			}
		}
		copyGatewayResponseHeaders(w.Header(), response.Header, prepared.MaskQuotaHeaders)
		setGatewayAttributionHeaders(w.Header(), account.Name, groupID, prepared.MaskQuotaHeaders)
		body = restoreGatewayResponse(body, prepared)
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(body)
		return parseAnthropicUsage(body, false), err
	}
	reader := bufio.NewReaderSize(response.Body, 64<<10)
	usage := tokenUsage{}
	terminalSeen := false
	var clientWriteErr error
	// A retry on another account reuses this writer, so the status line may
	// already be on the wire from the previous attempt's heartbeat.
	committed := gatewayResponseStatus(w) != 0
	streamedEvent := false
	flusher, _ := w.(http.Flusher)
	commitResponse := func() {
		if committed {
			return
		}
		copyGatewayResponseHeaders(w.Header(), response.Header, prepared.MaskQuotaHeaders)
		setGatewayAttributionHeaders(w.Header(), account.Name, groupID, prepared.MaskQuotaHeaders)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(response.StatusCode)
		committed = true
	}

	readContext := context.Background()
	if response.Request != nil {
		readContext = response.Request.Context()
	}
	readContext, cancelRead := context.WithCancel(readContext)
	defer cancelRead()
	type streamReadResult struct {
		block []byte
		err   error
	}
	readResults := make(chan streamReadResult, 1)
	go func() {
		for {
			block, err := readGatewaySSEBlock(reader)
			select {
			case readResults <- streamReadResult{block: block, err: err}:
			case <-readContext.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	heartbeatInterval := durationFromEnv("CCMAX_STREAM_HEARTBEAT_INTERVAL", defaultGatewayStreamHeartbeatInterval)
	if heartbeatInterval <= 0 {
		heartbeatInterval = defaultGatewayStreamHeartbeatInterval
	}
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	idleTimeout := durationFromEnv("CCMAX_UPSTREAM_STREAM_IDLE_TIMEOUT", defaultGatewayUpstreamStreamIdleTimeout)
	if idleTimeout <= 0 {
		idleTimeout = defaultGatewayUpstreamStreamIdleTimeout
	}
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	for {
		select {
		case result := <-readResults:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(idleTimeout)
			block, err := result.block, result.err
			if len(block) > 0 {
				eventType, eventBody := gatewaySSEEvent(block)
				if !committed && eventType == "error" {
					return tokenUsage{}, &gatewayPreOutputStreamError{status: gatewaySSEErrorStatus(eventBody), body: eventBody}
				}
				usage = mergeTokenUsage(usage, parseAnthropicUsage(block, true))
				if downgrade := detectGatewayAnthropicModelDowngrade(eventBody, prepared); downgrade != nil {
					downgrade.committed = committed
					if committed {
						writeGatewaySSEError(w, flusher, "api_error", downgrade.Error())
					}
					return usage, downgrade
				}
				commitResponse()
				if eventType == "message_stop" {
					terminalSeen = true
				}
				if eventType == "error" && !prepared.Passthrough {
					writeGatewaySSEError(w, flusher, "upstream_error", "Upstream access forbidden, please contact administrator")
					return usage, errors.New("upstream stream returned an error event")
				}
				if clientWriteErr == nil {
					if _, writeErr := w.Write(restoreGatewaySSEBlock(block, prepared, usage)); writeErr != nil {
						clientWriteErr = writeErr
					} else {
						streamedEvent = true
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
					commitResponse()
					writeGatewaySSEError(w, flusher, "upstream_disconnected", "upstream stream ended before message_stop")
					return usage, errors.New("upstream stream ended before message_stop")
				}
				writeGatewaySSEError(w, flusher, "stream_read_error", "upstream stream disconnected")
				return usage, fmt.Errorf("upstream stream read failed: %w", err)
			}
		case <-heartbeat.C:
			// The heartbeat deliberately does not reset the idle timer: it only
			// proves our own liveness, not the upstream's.
			commitResponse()
			if _, writeErr := io.WriteString(w, ": ping\n\n"); writeErr != nil {
				return usage, fmt.Errorf("client disconnected during streaming heartbeat: %w", writeErr)
			}
			if flusher != nil {
				flusher.Flush()
			}
		case <-idle.C:
			cancelRead()
			if streamedEvent {
				// Partial output already reached the client, so failing over
				// would splice a second answer onto the first one.
				writeGatewaySSEError(w, flusher, "upstream_stream_idle", fmt.Sprintf("upstream sent no stream events for %s", idleTimeout))
				return usage, &gatewayStreamIdleError{idle: idleTimeout, streamed: true}
			}
			return usage, &gatewayStreamIdleError{idle: idleTimeout, streamed: false}
		case <-readContext.Done():
			return usage, fmt.Errorf("stream request canceled: %w", readContext.Err())
		}
	}
}

func detectGatewayAnthropicModelDowngrade(body []byte, prepared claudePreparedRequest) *gatewayModelDowngradeError {
	if !prepared.RejectAnthropicDowngrade || len(body) == 0 {
		return nil
	}
	requested := strings.TrimSpace(prepared.Model)
	if !isAnthropicProtectedModel(requested) {
		requested = strings.TrimSpace(prepared.CompatOriginalModel())
	}
	if !isAnthropicProtectedModel(requested) {
		return nil
	}
	actual := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if actual == "" {
		actual = strings.TrimSpace(gjson.GetBytes(body, "message.model").String())
	}
	if !modelIDMatchesFamily(actual, "claude-opus-4-8") {
		return nil
	}
	return &gatewayModelDowngradeError{requested: requested, actual: actual}
}

func (p claudePreparedRequest) CompatOriginalModel() string {
	if p.Compat == nil {
		return p.Model
	}
	return p.Compat.OriginalModel
}

func isAnthropicProtectedModel(model string) bool {
	return modelIDMatchesFamily(model, "claude-fable-5") || modelIDMatchesFamily(model, "claude-opus-5")
}

func modelIDMatchesFamily(model, family string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	family = strings.ToLower(strings.TrimSpace(family))
	return model == family || strings.HasPrefix(model, family+"-")
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
	body = sub2service.RestoreCCMaxCompatibilityResponse(body, prepared.Compat)
	if prepared.Compat.Distilled {
		body = sub2service.NormalizeCCMaxDistilledResponse(body)
	}
	return body
}

func restoreGatewaySSELine(line []byte, prepared claudePreparedRequest, usage tokenUsage) []byte {
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
	if prepared.Compat.Distilled {
		restored = patchCCMaxDistilledStreamUsage(restored, usage, prepared.CacheCreationDetail)
		restored = sub2service.NormalizeCCMaxDistilledResponse(restored)
	}
	ending := []byte{}
	if bytes.HasSuffix(line, []byte("\r\n")) {
		ending = []byte("\r\n")
	} else if bytes.HasSuffix(line, []byte("\n")) {
		ending = []byte("\n")
	}
	result := append([]byte("data: "), restored...)
	return append(result, ending...)
}

func restoreGatewaySSEBlock(block []byte, prepared claudePreparedRequest, usage tokenUsage) []byte {
	if prepared.Passthrough || prepared.Compat == nil {
		return block
	}
	lines := bytes.SplitAfter(block, []byte("\n"))
	var restored []byte
	for _, line := range lines {
		if len(line) > 0 {
			restored = append(restored, restoreGatewaySSELine(line, prepared, usage)...)
		}
	}
	return restored
}

func patchCCMaxDistilledStreamUsage(body []byte, usage tokenUsage, cacheDetail bool) []byte {
	if gjson.GetBytes(body, "type").String() != "message_delta" || !gjson.GetBytes(body, "usage").Exists() {
		return body
	}
	type usagePatch struct {
		path  string
		value int64
	}
	updates := []usagePatch{
		{"usage.input_tokens", usage.Input},
		{"usage.output_tokens", usage.Output},
		{"usage.cache_creation_input_tokens", usage.CacheCreation},
		{"usage.cache_read_input_tokens", usage.CacheRead},
	}
	if cacheDetail {
		// Anthropic reports the cache_creation bucket split only on
		// message_start. Stamping the tracked values here keeps the final
		// message_delta consistent with its own totals, both for streaming
		// clients and for the non-stream bridge whose aggregation merges this
		// usage over message_start.
		updates = append(updates,
			usagePatch{"usage.cache_creation.ephemeral_5m_input_tokens", usage.CacheCreation5M},
			usagePatch{"usage.cache_creation.ephemeral_1h_input_tokens", usage.CacheCreation1H},
		)
	}
	for _, update := range updates {
		if next, err := sjson.SetBytes(body, update.path, update.value); err == nil {
			body = next
		}
	}
	for _, path := range []string{
		"usage.cache_creation.ephemeral_5m_input_tokens",
		"usage.cache_creation.ephemeral_1h_input_tokens",
	} {
		if !gjson.GetBytes(body, path).Exists() {
			if next, err := sjson.SetBytes(body, path, 0); err == nil {
				body = next
			}
		}
	}
	return body
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
	current.CacheCreation5M = max(current.CacheCreation5M, next.CacheCreation5M)
	current.CacheCreation1H = max(current.CacheCreation1H, next.CacheCreation1H)
	return current
}

func copyGatewayResponseHeaders(target, source http.Header, maskQuota bool) {
	for key, values := range source {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		if maskQuota && isGatewayQuotaIdentityHeader(key) {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

// isGatewayQuotaIdentityHeader matches upstream headers that reveal the pooled
// account's quota state or identity. Claude Code reads the unified ratelimit
// headers and silently drops a session from Opus 5 to Opus 4.8 once the
// account's utilization crosses the advertised fallback percentage, so
// customer-facing groups must not leak them.
func isGatewayQuotaIdentityHeader(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(lower, "anthropic-ratelimit-") {
		return true
	}
	switch lower {
	case "anthropic-organization-id", "anthropic-workspace-id":
		return true
	}
	return false
}

// setGatewayAttributionHeaders tags the response with the serving account and
// group. Masked groups omit the account header because its value is the pooled
// account's email address.
func setGatewayAttributionHeaders(header http.Header, accountName, groupID string, maskQuota bool) {
	if !maskQuota {
		header.Set("X-CCMAX-Account", accountName)
	}
	header.Set("X-CCMAX-Group", groupID)
}

func copyGatewayRequestID(target, source http.Header) {
	for _, name := range []string{"request-id", "x-request-id"} {
		if value := strings.TrimSpace(source.Get(name)); value != "" {
			target.Set(name, value)
		}
	}
}

func (a *app) recordGatewayUsage(ctx context.Context, key gatewayKey, account gatewayAccount, model string, stream bool, upstreamRequestID string, usage tokenUsage, started time.Time) bool {
	requestID, clientRequestID, traceID, upstreamRequestID := gatewayCorrelationIDs(ctx, upstreamRequestID)
	_, _, usageErr := a.recordUsage(usageInput{
		UserID: key.UserID, APIKeyID: key.ID, RequestID: requestID, ClientRequestID: clientRequestID, TraceID: traceID, UpstreamRequestID: upstreamRequestID,
		PurposeKey: "default", GroupID: key.GroupID, AccountID: account.ID, AccountSKHint: account.SourceSKHint, Model: model,
		InputTokens: usage.Input, OutputTokens: usage.Output, CacheCreationTokens: usage.CacheCreation,
		CacheReadTokens: usage.CacheRead, Stream: stream, DurationMS: int(time.Since(started).Milliseconds()),
	})
	if usageErr == nil {
		return true
	}
	_, writeErr := a.db.Exec(`UPDATE api_keys SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, key.ID)
	logDatabaseWriteError("update API key last-used fallback", writeErr)
	return false
}

func (a *app) recordGatewayRejectedDowngradeUsage(ctx context.Context, key gatewayKey, account gatewayAccount, actualModel string, stream bool, upstreamRequestID string, usage tokenUsage, started time.Time) bool {
	requestID, clientRequestID, traceID, upstreamRequestID := gatewayCorrelationIDs(ctx, upstreamRequestID)
	zero := 0.0
	_, _, usageErr := a.recordUsage(usageInput{
		UserID: key.UserID, APIKeyID: key.ID, RequestID: requestID, ClientRequestID: clientRequestID, TraceID: traceID, UpstreamRequestID: upstreamRequestID,
		PurposeKey: "default", GroupID: key.GroupID, AccountID: account.ID, AccountSKHint: account.SourceSKHint, Model: actualModel,
		InputTokens: usage.Input, OutputTokens: usage.Output, CacheCreationTokens: usage.CacheCreation,
		CacheReadTokens: usage.CacheRead, BilledCostOverride: &zero, Stream: stream, DurationMS: int(time.Since(started).Milliseconds()),
	})
	if usageErr == nil {
		return true
	}
	_, writeErr := a.db.Exec(`UPDATE api_keys SET last_used_at = `+nowSQL+`, updated_at = `+nowSQL+` WHERE id = ?`, key.ID)
	logDatabaseWriteError("update rejected-downgrade API key last-used fallback", writeErr)
	return false
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
	var normalRequestMode, claudeCodeIdentity, streamHedgeEnabled, adaptiveHedgeEnabled, mcpToolNames int
	var serviceTierPassthrough, inferenceGeoPassthrough, speedPassthrough, anthropicBetaPassthrough, rejectAnthropicDowngrade, rejectDistillation, requestFormatFilter, quotaHeaderMasking, cacheCreationDetail, datelineNormalization, extraUsageFailover, openCodeScrub, rateLimitDownweight, rateLimitSteppedCooldown, rateLimitDownweightStepped, fiveHourStaggerEnabled, capacityQueueEnabled, strategyRequired int
	err := a.db.QueryRow(`SELECT k.id, k.user_id, k.group_id, k.quota, k.quota_used, u.balance, u.rpm_limit, u.allowed_group_ids_json, u.role, k.expires_at, g.normal_request_mode, g.claude_code_identity_enabled, g.claude_cli_version, g.stream_hedge_enabled, g.adaptive_hedge_enabled, g.rpm_dispatch_enabled, g.mcp_tool_names_enabled, g.service_tier_passthrough_enabled, g.inference_geo_passthrough_enabled, g.speed_passthrough_enabled, g.anthropic_beta_passthrough_enabled, g.reject_anthropic_downgrade_enabled, g.reject_distillation_enabled, g.request_format_filter_enabled, g.quota_header_masking_enabled, g.cache_creation_detail_enabled, g.dateline_normalization_enabled, g.extra_usage_failover_enabled, g.opencode_scrub_enabled, g.overload_cooldown_seconds, g.rate_limit_downweight_enabled, g.rate_limit_cooling_threshold, g.rate_limit_wait_seconds, g.rate_limit_stepped_cooldown_enabled, g.rate_limit_cooldown_step_seconds, g.rate_limit_downweight_stepped_cooldown_enabled, g.rate_limit_downweight_base_minutes, g.rate_limit_downweight_step_minutes, g.five_hour_release_stagger_enabled, g.five_hour_release_stagger_min_minutes, g.five_hour_release_stagger_max_minutes, g.capacity_queue_enabled, g.capacity_queue_timeout_seconds, g.strategy_required_enabled
		FROM api_keys k JOIN users u ON u.id = k.user_id JOIN groups g ON g.id = k.group_id
		WHERE k.key_hash = ? AND k.status = 'active' AND k.deleted_at IS NULL AND u.status = 'active' AND u.deleted_at IS NULL AND g.status = 'active' AND g.reserve_pool_enabled = 0
		AND (k.expires_at IS NULL OR k.expires_at > `+nowSQL+`)`, hashToken(secret)).Scan(&key.ID, &key.UserID, &key.GroupID, &key.Quota, &key.QuotaUsed, &key.UserBalance, &key.UserRPM, &key.Allowed, &key.UserRole, &key.ExpiresAt, &normalRequestMode, &claudeCodeIdentity, &key.ClaudeCLIVersion, &streamHedgeEnabled, &adaptiveHedgeEnabled, &key.RPMDispatchEnabled, &mcpToolNames, &serviceTierPassthrough, &inferenceGeoPassthrough, &speedPassthrough, &anthropicBetaPassthrough, &rejectAnthropicDowngrade, &rejectDistillation, &requestFormatFilter, &quotaHeaderMasking, &cacheCreationDetail, &datelineNormalization, &extraUsageFailover, &openCodeScrub, &key.OverloadCooldownSeconds, &rateLimitDownweight, &key.RateLimitCoolingThreshold, &key.RateLimitWaitSeconds, &rateLimitSteppedCooldown, &key.RateLimitCooldownStep, &rateLimitDownweightStepped, &key.RateLimitDownweightBase, &key.RateLimitDownweightStep, &fiveHourStaggerEnabled, &key.FiveHourStaggerMin, &key.FiveHourStaggerMax, &capacityQueueEnabled, &key.CapacityQueueTimeout, &strategyRequired)
	key.NormalRequestMode = normalRequestMode == 1
	key.ClaudeCodeIdentityEnabled = claudeCodeIdentity == 1
	key.StreamHedgeEnabled = streamHedgeEnabled == 1
	key.AdaptiveHedgeEnabled = adaptiveHedgeEnabled == 1
	key.MCPToolNamesEnabled = mcpToolNames == 1
	key.ServiceTierPassthrough = serviceTierPassthrough == 1
	key.InferenceGeoPassthrough = inferenceGeoPassthrough == 1
	key.SpeedPassthrough = speedPassthrough == 1
	key.AnthropicBetaPassthrough = anthropicBetaPassthrough == 1
	key.RejectAnthropicDowngrade = rejectAnthropicDowngrade == 1
	key.RejectDistillation = rejectDistillation == 1
	key.RequestFormatFilter = requestFormatFilter == 1
	key.QuotaHeaderMasking = quotaHeaderMasking == 1
	key.CacheCreationDetail = cacheCreationDetail == 1
	key.DatelineNormalization = datelineNormalization == 1
	key.ExtraUsageFailover = extraUsageFailover == 1
	key.OpenCodeScrub = openCodeScrub == 1
	key.RateLimitDownweightEnabled = rateLimitDownweight == 1
	key.RateLimitSteppedCooldown = rateLimitSteppedCooldown == 1
	key.RateLimitDownweightStepped = rateLimitDownweightStepped == 1
	key.FiveHourStaggerEnabled = fiveHourStaggerEnabled == 1
	key.CapacityQueueEnabled = capacityQueueEnabled == 1
	key.StrategyRequiredEnabled = strategyRequired == 1
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
	if a.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		allowed, err := a.redis.allowUserRPM(ctx, key.UserID, key.UserRPM)
		if err != nil {
			return fmt.Errorf("user RPM limiter unavailable: %w", err)
		}
		if !allowed {
			return errors.New("user RPM limit reached")
		}
		return nil
	}
	tx, err := a.db.Begin()
	if err != nil {
		logDatabaseWriteError("begin user RPM transaction", err)
		return nil
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM user_rpm_events WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`); err != nil {
		logDatabaseWriteError("prune user RPM events", err)
	}
	var current int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM user_rpm_events WHERE user_id = ? AND created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')`, key.UserID).Scan(&current); err != nil {
		logDatabaseWriteError("read user RPM events", err)
		return nil
	}
	if current >= key.UserRPM {
		return errors.New("user RPM limit reached")
	}
	if _, err := tx.Exec(`INSERT INTO user_rpm_events (user_id) VALUES (?)`, key.UserID); err != nil {
		logDatabaseWriteError("insert user RPM event", err)
		return nil
	}
	if err := tx.Commit(); err != nil {
		logDatabaseWriteError("commit user RPM transaction", err)
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
	return a.acquireGatewayAccountWithPolicyDemand(key, sessionHash, requestedModel, excluded, false, gatewayDispatchDemand{})
}

// pinSticky turns the session's bound account from a preference into a
// requirement. Moving a warm session to another account throws away its cached
// prefix, so a request that would re-create a large cache is cheaper to queue —
// or refuse — than to serve somewhere else. See handleClaudeGateway for where
// this is decided.
func (a *app) acquireGatewayAccountPinned(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, loadAware, pinSticky bool) (gatewayAccount, error) {
	return a.acquireGatewayAccountPinnedDemand(key, sessionHash, requestedModel, excluded, loadAware, pinSticky, gatewayDispatchDemand{})
}

func (a *app) acquireGatewayAccountPinnedDemand(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, loadAware, pinSticky bool, demand gatewayDispatchDemand) (gatewayAccount, error) {
	account, err := a.tryAcquireGatewayAccountPinned(key, sessionHash, requestedModel, excluded, loadAware, pinSticky, demand)
	if err == nil || !errors.Is(err, errNoGatewayAccountCapacity) {
		return account, err
	}
	ready, reserveErr := a.ensureReserveCapacity(key.GroupID, requestedModel, "capacity", excluded)
	if reserveErr != nil {
		return gatewayAccount{}, reserveActivationError(key.GroupID, reserveErr)
	}
	if !ready {
		return gatewayAccount{}, err
	}
	return a.tryAcquireGatewayAccountPinned(key, sessionHash, requestedModel, excluded, loadAware, pinSticky, demand)
}

const (
	gatewayCapacityQueuePollInterval = 300 * time.Millisecond
	gatewayCapacityQueueMaxWaiters   = 256
)

// waitForGatewayCapacity parks a request whose group is fully saturated
// (concurrency or RPM) and keeps retrying account acquisition until capacity
// frees up, the configured timeout elapses, or the client goes away. Groups
// with the queue disabled never reach this path and keep rejecting instantly.
func (a *app) waitForGatewayCapacity(ctx context.Context, key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, initialErr error) (gatewayAccount, error) {
	return a.waitForGatewayCapacityPinned(ctx, key, sessionHash, requestedModel, excluded, initialErr, false)
}

func (a *app) waitForGatewayCapacityPinned(ctx context.Context, key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, initialErr error, pinSticky bool) (gatewayAccount, error) {
	return a.waitForGatewayCapacityPinnedDemand(ctx, key, sessionHash, requestedModel, excluded, initialErr, pinSticky, gatewayDispatchDemand{})
}

func (a *app) waitForGatewayCapacityPinnedDemand(ctx context.Context, key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, initialErr error, pinSticky bool, demand gatewayDispatchDemand) (gatewayAccount, error) {
	counterValue, _ := a.capacityWaiters.LoadOrStore(key.GroupID, new(int64))
	counter := counterValue.(*int64)
	if atomic.AddInt64(counter, 1) > gatewayCapacityQueueMaxWaiters {
		atomic.AddInt64(counter, -1)
		return gatewayAccount{}, fmt.Errorf("capacity queue for group %s is full (%d waiting requests): %w", strings.ToUpper(key.GroupID), gatewayCapacityQueueMaxWaiters, initialErr)
	}
	defer atomic.AddInt64(counter, -1)

	timeout := time.Duration(key.CapacityQueueTimeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if !key.CapacityQueueEnabled && gatewayITPMCapacityBlocked(initialErr) {
		autoTimeout := gatewayITPMAutoQueueTimeout
		if pinSticky {
			// A pinned warm session is worth a longer wait: its rolling ITPM
			// window drains within a minute, while rebuilding the prefix on
			// another account costs a full-price cache write.
			autoTimeout = durationFromEnv("CCMAX_ITPM_STICKY_QUEUE_TIMEOUT", gatewayITPMStickyQueueTimeout)
		}
		if timeout > autoTimeout {
			timeout = autoTimeout
		}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(gatewayCapacityQueuePollInterval)
	defer ticker.Stop()
	lastCapacityErr := initialErr
	for {
		select {
		case <-ctx.Done():
			return gatewayAccount{}, ctx.Err()
		case <-deadline.C:
			return gatewayAccount{}, fmt.Errorf("request waited %s in the capacity queue without a free account: %w", timeout, lastCapacityErr)
		case <-ticker.C:
			account, err := a.acquireGatewayAccountPinnedDemand(key, sessionHash, requestedModel, excluded, false, pinSticky, demand)
			if err == nil {
				return account, nil
			}
			if !errors.Is(err, errNoGatewayAccountCapacity) {
				return gatewayAccount{}, err
			}
			lastCapacityErr = err
		}
	}
}

func (a *app) acquireGatewayAccountWithPolicy(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, loadAware bool) (gatewayAccount, error) {
	return a.acquireGatewayAccountWithPolicyDemand(key, sessionHash, requestedModel, excluded, loadAware, gatewayDispatchDemand{})
}

func (a *app) acquireGatewayAccountWithPolicyDemand(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, loadAware bool, demand gatewayDispatchDemand) (gatewayAccount, error) {
	account, err := a.tryAcquireGatewayAccountWithPolicyDemand(key, sessionHash, requestedModel, excluded, loadAware, demand)
	if err == nil || !errors.Is(err, errNoGatewayAccountCapacity) {
		return account, err
	}
	ready, reserveErr := a.ensureReserveCapacity(key.GroupID, requestedModel, "capacity", excluded)
	if reserveErr != nil {
		return gatewayAccount{}, reserveActivationError(key.GroupID, reserveErr)
	}
	if !ready {
		return gatewayAccount{}, err
	}
	return a.tryAcquireGatewayAccountWithPolicyDemand(key, sessionHash, requestedModel, excluded, loadAware, demand)
}

func (a *app) tryAcquireGatewayAccountWithPolicy(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, loadAware bool) (gatewayAccount, error) {
	return a.tryAcquireGatewayAccountWithPolicyDemand(key, sessionHash, requestedModel, excluded, loadAware, gatewayDispatchDemand{})
}

func (a *app) tryAcquireGatewayAccountWithPolicyDemand(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, loadAware bool, demand gatewayDispatchDemand) (gatewayAccount, error) {
	return a.tryAcquireGatewayAccountPinned(key, sessionHash, requestedModel, excluded, loadAware, false, demand)
}

func (a *app) tryAcquireGatewayAccountPinned(key gatewayKey, sessionHash, requestedModel string, excluded map[int64]bool, loadAware, pinSticky bool, demand gatewayDispatchDemand) (gatewayAccount, error) {
	serializedDispatch := key.RPMDispatchEnabled || a.groupUsesRPMReservationStrategy(key.GroupID) || a.groupUsesDispatchPacing(key.GroupID)
	if serializedDispatch {
		lockValue, _ := a.dispatchLocks.LoadOrStore(key.GroupID, &sync.Mutex{})
		dispatchLock := lockValue.(*sync.Mutex)
		dispatchLock.Lock()
		defer dispatchLock.Unlock()
		if a.redis != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			release, err := a.redis.acquireDispatchLock(ctx, key.GroupID)
			if err != nil {
				return gatewayAccount{}, err
			}
			defer release()
		}
	}

	tx, err := a.db.Begin()
	if err != nil {
		return gatewayAccount{}, err
	}
	defer tx.Rollback()
	// Expired runtime rows are tidied by startAccountRateLimitSweeper. Every
	// predicate below already ignores expired rows, so cleanup must stay out of
	// this per-request transaction: doing full-table DELETEs here made dispatch
	// scan and lock the same tables millions of times under production load.
	var stickyID, stickyLastInput int64
	if sessionHash != "" {
		_ = tx.QueryRow(`SELECT account_id, last_input_tokens FROM dispatch_sessions WHERE session_hash = ? AND api_key_id = ? AND expires_at > `+nowSQL, sessionHash, key.ID).Scan(&stickyID, &stickyLastInput)
	}
	// A warm session mostly re-reads its cached prefix, so its true ITPM cost
	// tracks the last settled uncached input, not the full body estimate. The
	// discounted demand applies only on the session's own account; anywhere
	// else the prefix would be re-created at the full estimate. Factor 2
	// leaves room for the conversation growing between turns. A binding whose
	// cost has not settled yet is assumed warm and small: the overshoot is
	// bounded to a single request and settles right after, while the full
	// estimate would evict a session whose real input the cache absorbs.
	stickyDemand := demand
	if stickyID != 0 && demand.EstimatedITPM > 0 {
		discounted := stickyLastInput * 2
		if stickyLastInput <= 0 {
			discounted = gatewayITPMSmallRequest
		}
		if discounted < demand.EstimatedITPM {
			stickyDemand.EstimatedITPM = discounted
			stickyDemand.Oversized = discounted > gatewayITPMLargeRequest
		}
	}
	// Rate-limit history splits equal-priority accounts into three tiers: an
	// account whose quota window just rolled over has a full allowance and goes
	// first, an account still carrying this window's 429 penalty goes last.
	//
	// The tier is applied at read time, not baked into the stored state, because
	// an account can serve several groups: one group may run the feature while
	// another ignores it. A group with the switch off flattens every candidate
	// to the neutral tier, so a penalty earned elsewhere never reorders its
	// dispatch — and turning the switch off takes effect immediately instead of
	// lingering until the quota window rolls.
	freshCutoff := time.Now().UTC().Add(-accountQuotaFreshPriorityWindow).Format(time.RFC3339Nano)
	weightTier := `CASE
		WHEN a.quota_refreshed_at IS NOT NULL AND a.quota_refreshed_at > ? THEN 0
		WHEN a.rate_limit_downweight_until IS NOT NULL AND a.rate_limit_downweight_until > ` + nowSQL + ` THEN 2
		ELSE 1
	END`
	if !key.RateLimitDownweightEnabled {
		// Still consumes the freshCutoff placeholder so the argument list stays
		// aligned with the branch above.
		weightTier = `CASE WHEN ? IS NULL THEN 1 ELSE 1 END`
	}
	temporaryRPM := `CASE WHEN a.rate_limit_downweight_until IS NOT NULL AND a.rate_limit_downweight_until > ` + nowSQL + `
		THEN COALESCE((SELECT rpm_limit FROM account_rpm_thresholds t WHERE t.account_id = a.id AND t.reset_at > ` + nowSQL + `), 0)
		ELSE 0 END`
	if !key.RateLimitDownweightEnabled {
		temporaryRPM = `0`
	}
	orderClause := `ORDER BY CASE WHEN a.id = ? THEN 0 ELSE 1 END, ag.priority, a.priority, weight_tier`
	if stickyID == 0 {
		// Cold start: no cache exists anywhere, so placement is free and follows
		// the upstream's own remaining-input report. Bucketed because the header
		// is rounded and raw values would reshuffle the pool on noise. A warm
		// session skips this entirely — its account is already sorted first, and
		// re-placing it is what pinSticky exists to prevent.
		orderClause += fmt.Sprintf(`, CASE WHEN a.itpm_remaining IS NULL THEN 0 ELSE -CAST(a.itpm_remaining / %d AS INTEGER) END`, coldStartBudgetBucketSize)
	}
	if key.RateLimitDownweightEnabled {
		// Strike count is the finer grain of the same penalty, so it drops out
		// with the switch too.
		orderClause += `, a.consecutive_429`
	}
	if key.RPMDispatchEnabled && !loadAware {
		orderClause += `, CASE WHEN current_rpm > 0 OR current_inflight > 0 THEN 0 ELSE 1 END, current_rpm DESC, current_inflight DESC, COALESCE(a.last_used_at, '') DESC, a.id`
	} else {
		orderClause += `, COALESCE(a.last_used_at, ''), a.id`
	}
	rows, err := tx.Query(`SELECT a.id, a.name, a.auth_type, a.credentials_json, a.source_sk_hint, a.extra_json, a.concurrency, a.base_rpm, a.rpm_strategy, a.rpm_sticky_buffer, a.user_msg_queue_mode, a.proxy_id, ag.priority, a.priority, COALESCE(a.last_used_at, ''),
		COALESCE((SELECT COUNT(*) FROM account_rpm_events e WHERE e.account_id = a.id AND e.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0) AS current_rpm,
		COALESCE((SELECT requests FROM account_inflight f WHERE f.account_id = a.id), 0) AS current_inflight,
		`+temporaryRPM+` AS temporary_rpm_limit,
		a.consecutive_429, `+weightTier+` AS weight_tier,
		COALESCE(a.itpm_remaining, -1) AS itpm_remaining,
		COALESCE(ds.id, 0), COALESCE(ds.rpm_limit, 0), COALESCE(ds.tpm_limit, 0), COALESCE(ds.itpm_limit, 0), COALESCE(ds.concurrency_limit, 0), COALESCE(ds.rpm_strategy, ''), COALESCE(ds.rpm_sticky_buffer, 0), COALESCE(ds.dispatch_mode, ''), COALESCE(ds.capacity_enabled, 1),
		CASE WHEN ds.tpm_limit > 0 THEN COALESCE((SELECT SUM(u.input_tokens + u.output_tokens + u.cache_creation_tokens + u.cache_read_tokens) FROM usage_logs u WHERE u.account_id = a.id AND u.created_at >= strftime('%Y-%m-%dT%H:%M:%fZ','now','-60 seconds')), 0) ELSE 0 END AS current_tpm,
		0 AS current_itpm,
		COALESCE(ds.itpm_protection_enabled, 1), COALESCE(ds.itpm_window_seconds, 60), COALESCE(ds.itpm_soft_limit, 100000), COALESCE(ds.itpm_hard_limit, 150000),
		COALESCE(ds.dispatch_pacing, ''), COALESCE(ds.pacing_concurrency, 0), COALESCE(ds.pacing_interval_seconds, 0),
		COALESCE(ds.smooth_cold_start_enabled, 0), COALESCE(ds.smooth_cold_start_rpm, 8), COALESCE(ds.smooth_cold_start_tpm, 100000),
		CASE WHEN EXISTS (SELECT 1 FROM account_model_cooldowns mc WHERE mc.account_id = a.id AND mc.model = ? AND mc.reset_at > `+nowSQL+`) THEN 1 ELSE 0 END AS model_cooldown
		FROM accounts a JOIN account_groups ag ON ag.account_id = a.id
		LEFT JOIN groups g ON g.id = ag.group_id
		LEFT JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, g.strategy_id) AND ds.deleted_at IS NULL
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND `+accountStatePredicate("a", "normal")+`
		`+orderClause, freshCutoff, modelCooldownKey(requestedModel), key.GroupID, stickyID)
	if err != nil {
		return gatewayAccount{}, err
	}
	type candidate struct {
		account          gatewayAccount
		groupPriority    int
		accountPriority  int
		lastUsed         string
		rpm              int
		inflight         int
		temporaryRPM     int
		consecutive429   int
		weightTier       int
		itpmRemaining    int64
		strategyID       int64
		strategyRPM      int
		strategyTPM      int64
		strategyITPM     int64
		strategyConc     int
		strategyRPMMode  string
		strategyBuffer   int
		strategyMode     string
		strategyCapacity bool
		tpm              int64
		itpm             int64
		itpmProtection   bool
		itpmWindow       int
		pacing           string
		pacingN          int
		pacingInterval   int
		itpmSoft         int64
		itpmHard         int64
		smoothEnabled    bool
		smoothRPM        int
		smoothTPM        int64
		modelCooldown    int
	}
	candidates := []candidate{}
	for rows.Next() {
		var item candidate
		var itpmProtection, strategyCapacity, strategySmoothEnabled int
		if err := rows.Scan(&item.account.ID, &item.account.Name, &item.account.AuthType, &item.account.CredentialsJSON, &item.account.SourceSKHint, &item.account.ExtraJSON, &item.account.Concurrency, &item.account.BaseRPM, &item.account.RPMStrategy, &item.account.StickyBuffer, &item.account.UserMsgQueueMode, &item.account.ProxyID, &item.groupPriority, &item.accountPriority, &item.lastUsed, &item.rpm, &item.inflight, &item.temporaryRPM, &item.consecutive429, &item.weightTier, &item.itpmRemaining, &item.strategyID, &item.strategyRPM, &item.strategyTPM, &item.strategyITPM, &item.strategyConc, &item.strategyRPMMode, &item.strategyBuffer, &item.strategyMode, &strategyCapacity, &item.tpm, &item.itpm, &itpmProtection, &item.itpmWindow, &item.itpmSoft, &item.itpmHard, &item.pacing, &item.pacingN, &item.pacingInterval, &strategySmoothEnabled, &item.smoothRPM, &item.smoothTPM, &item.modelCooldown); err != nil {
			rows.Close()
			return gatewayAccount{}, err
		}
		item.strategyCapacity = strategyCapacity == 1
		item.itpmProtection = itpmProtection == 1
		item.itpmProtection, item.itpmWindow, item.itpmSoft, item.itpmHard = normalizeStrategyITPMConfig(item.itpmProtection, item.itpmWindow, item.itpmSoft, item.itpmHard, item.strategyITPM)
		// A bound strategy overrides the account's own limits where it sets them.
		if item.strategyID > 0 && item.strategyCapacity {
			if item.strategyConc > 0 && item.strategyConc < item.account.Concurrency {
				item.account.Concurrency = item.strategyConc
			}
			if item.strategyRPM > 0 {
				item.account.BaseRPM = item.strategyRPM
				item.account.RPMStrategy = item.strategyRPMMode
				item.account.StickyBuffer = item.strategyBuffer
			}
		} else if item.strategyID > 0 {
			item.strategyTPM = 0
			item.strategyMode = ""
			item.pacing = ""
			item.pacingN = 0
			item.pacingInterval = 0
		}
		accountSmooth := smoothColdStartFromExtra(item.account.ExtraJSON)
		if accountSmooth.Enabled {
			item.smoothEnabled = true
			item.smoothRPM = accountSmooth.RPM
			item.smoothTPM = accountSmooth.TPM
		} else {
			item.smoothEnabled = strategySmoothEnabled == 1
		}
		if item.smoothEnabled {
			item.smoothRPM, item.smoothTPM = normalizeSmoothColdStartLimits(item.smoothRPM, item.smoothTPM)
			item.account.BaseRPM = minPositiveInt(item.account.BaseRPM, item.smoothRPM)
			item.account.RPMStrategy = "fixed"
			item.account.StickyBuffer = 0
			item.pacing = "interval"
			item.pacingN, item.pacingInterval = smoothColdStartPacing(item.smoothRPM)
			item.itpmProtection = true
			item.itpmWindow = defaultStrategyITPMWindowSeconds
			item.itpmHard = minPositiveInt64(item.itpmHard, item.smoothTPM)
			item.itpmSoft = item.itpmHard
			item.account.ITPMStrictHard = true
		}
		candidates = append(candidates, item)
	}
	rows.Close()
	itpmWindows := map[int64]int{}
	for _, item := range candidates {
		if item.itpmProtection {
			itpmWindows[item.account.ID] = item.itpmWindow
		}
	}
	itpmUsage, err := loadAccountITPMUsage(tx, itpmWindows)
	if err != nil {
		return gatewayAccount{}, err
	}
	for index := range candidates {
		candidates[index].itpm = itpmUsage[candidates[index].account.ID]
	}
	// Interval pacing spreads admissions across the minute instead of letting
	// the whole RPM budget burn in the first seconds: each account admits at
	// most pacing_concurrency new requests per pacing_interval_seconds window.
	pacingWindows := map[int64]int{}
	for _, item := range candidates {
		if item.pacing == "interval" && item.pacingN > 0 && item.pacingInterval > 0 {
			pacingWindows[item.account.ID] = item.pacingInterval
		}
	}
	pacingAdmissions, err := loadAccountRecentAdmissions(tx, pacingWindows)
	if err != nil {
		return gatewayAccount{}, err
	}
	// Group traffic is split between strategies by configured weight. Tally the
	// last minute per strategy so each candidate can be measured against its
	// share; with no split configured this stays empty and costs nothing.
	shareWeights, shareTotalWeight := groupStrategyWeights(tx, key.GroupID)
	strategyRPM := map[int64]int{}
	groupRPM := 0
	if shareTotalWeight > 0 {
		for _, item := range candidates {
			strategyRPM[item.strategyID] += item.rpm
			groupRPM += item.rpm
		}
	}
	if pinSticky && stickyID != 0 {
		// The pin is only meaningful while the bound account can still come back.
		// Absence from the candidate set is ambiguous — a short cooldown (worth
		// waiting for) looks the same as an archived or unschedulable account
		// (nothing to wait for) — so check directly, and release the pin in the
		// second case rather than blacklisting an otherwise healthy pool.
		stickyPresent := false
		for _, item := range candidates {
			if item.account.ID == stickyID {
				stickyPresent = true
				break
			}
		}
		if !stickyPresent {
			var recoverable int
			// Mirrors accountStatePredicate("normal") minus its cooldown clause: a
			// cooldown is exactly the transient case worth waiting for, while any
			// other reason it fails that predicate — expired, invalidated, or a
			// dead proxy — means the account is not coming back on its own.
			_ = tx.QueryRow(`SELECT COUNT(*) FROM accounts a
				WHERE a.id = ? AND a.deleted_at IS NULL AND a.archived_at IS NULL
				AND a.status = 'active' AND a.schedulable = 1 AND a.auth_status = 'valid'
				AND a.invalidated_at IS NULL
				AND (a.expires_at IS NULL OR a.expires_at > `+nowSQL+`)
				AND EXISTS (SELECT 1 FROM proxies sticky_proxy
					WHERE sticky_proxy.id = a.proxy_id AND sticky_proxy.status = 'active' AND sticky_proxy.deleted_at IS NULL)`, stickyID).Scan(&recoverable)
			if recoverable == 0 {
				pinSticky = false
			}
		}
	}
	diagnostics := gatewayCapacityDiagnostics{Candidates: len(candidates)}
	reservationBlocked := map[int64]bool{}
	var selectedCandidate candidate
	var selected gatewayAccount
	allowStrategyFallback := false
	for {
		selectedIndex := -1
		shareBlockedThisPass := 0
		for index, candidate := range candidates {
			if reservationBlocked[candidate.account.ID] {
				continue
			}
			if pinSticky && !stickyDemand.Oversized && stickyID != 0 && candidate.account.ID != stickyID {
				// The session is warm on stickyID. Serving it anywhere else would
				// turn a cache read into a cache creation, which costs far more
				// upstream ITPM than waiting does. Report no capacity instead so the
				// group's capacity queue holds the request on its own account. The
				// pin follows the discounted demand: a large body whose prefix is
				// already cached is not an oversized request on its own account.
				diagnostics.StickyPinned++
				continue
			}
			if excluded[candidate.account.ID] {
				diagnostics.Excluded++
				continue
			}
			if !allowStrategyFallback && strategyOverShare(shareWeights, shareTotalWeight, strategyRPM, groupRPM, candidate.strategyID) {
				diagnostics.StrategyShareBlocked++
				shareBlockedThisPass++
				continue
			}
			if key.StrategyRequiredEnabled && candidate.strategyID == 0 {
				diagnostics.StrategyMissing++
				continue
			}
			if candidate.modelCooldown == 1 {
				diagnostics.ModelCooldown++
				continue
			}
			if !accountSupportsModel(candidate.account, requestedModel) {
				diagnostics.ModelUnsupported++
				continue
			}
			if candidate.inflight >= candidate.account.Concurrency {
				diagnostics.ConcurrencyBlocked++
				continue
			}
			effective := demand
			if candidate.account.ID == stickyID {
				effective = stickyDemand
			}
			if effective.Oversized && candidate.inflight > 0 {
				diagnostics.ConcurrencyBlocked++
				continue
			}
			switch candidate.pacing {
			case "completion":
				// 成功接续：保持 pacingN 个在途，空出一个立即接入下一个，
				// 不等整批完成。被挡下的请求由容量队列轮询补位。
				if candidate.pacingN > 0 && candidate.inflight >= candidate.pacingN {
					diagnostics.PacingBlocked++
					continue
				}
			case "interval":
				// 按秒间隔：滑动窗口内最多准入 pacingN 个新请求，把 RPM
				// 预算摊到整分钟，而不是前几秒全部烧掉。
				if candidate.pacingN > 0 && pacingAdmissions[candidate.account.ID] >= int64(candidate.pacingN) {
					diagnostics.PacingBlocked++
					continue
				}
			}
			if candidate.temporaryRPM > 0 && candidate.rpm >= candidate.temporaryRPM {
				// A learned threshold comes from a 429, so it is rate limiting, not
				// account failure, and it does not release a pinned session: moving a
				// large warm request off its account converts a nearly free cache read
				// into a full-price cache creation, which is what made the account hit
				// the limit in the first place. The ceiling itself now survives to the
				// 5h window, but the block is "rolling 60s count >= ceiling", so it
				// clears as traffic ages out and the capacity queue holds the request
				// until the account can take it again.
				diagnostics.TemporaryRPMBlocked++
				continue
			}
			if candidate.strategyTPM > 0 && candidate.tpm >= candidate.strategyTPM {
				diagnostics.TPMBlocked++
				continue
			}
			sticky := stickyID == candidate.account.ID
			if candidate.itpmProtection && candidate.account.ITPMStrictHard {
				if (effective.EstimatedITPM > 0 && candidate.itpm+effective.EstimatedITPM > candidate.itpmHard) || (effective.EstimatedITPM == 0 && candidate.itpm >= candidate.itpmHard) {
					diagnostics.ITPMBlocked++
					continue
				}
			} else if candidate.itpmProtection && effective.Oversized && candidate.itpm >= candidate.itpmHard {
				diagnostics.ITPMBlocked++
				continue
			} else if candidate.itpmProtection && effective.EstimatedITPM > 0 && !effective.Oversized {
				softLimit, hardLimit := candidate.itpmSoft, candidate.itpmHard
				projected := candidate.itpm + effective.EstimatedITPM
				if candidate.itpm >= hardLimit || projected > hardLimit || (projected > softLimit && !sticky && effective.EstimatedITPM > gatewayITPMSmallRequest) {
					diagnostics.ITPMBlocked++
					continue
				}
			} else if candidate.itpmProtection && effective.EstimatedITPM == 0 && candidate.itpm >= candidate.itpmHard {
				diagnostics.ITPMBlocked++
				continue
			}
			if !rpmSchedulable(candidate.account, candidate.rpm, sticky) {
				diagnostics.RPMBlocked++
				continue
			}
			if sticky {
				// A warm session's cache outweighs every placement heuristic
				// below: once the bound account passes the capacity filters it
				// serves the request. In particular the lowest-ITPM re-placement
				// for oversized requests applies to cold sessions only — moving a
				// warm oversized request rewrites its whole prefix elsewhere,
				// which is exactly the input load the heuristics try to avoid.
				// The reservation below still increments this account's RPM, so
				// sticky traffic uses its normal share and new sessions fill the
				// lower-load accounts.
				selectedIndex = index
				break
			}
			if selectedIndex < 0 {
				selectedIndex = index
				if !demand.Oversized && !loadAware && candidate.strategyMode == "" {
					break
				}
				continue
			}
			selected := candidates[selectedIndex]
			if demand.Oversized {
				if candidate.itpm < selected.itpm || (candidate.itpm == selected.itpm && candidate.account.ID < selected.account.ID) {
					selectedIndex = index
				}
				continue
			}
			if candidate.groupPriority != selected.groupPriority || candidate.accountPriority != selected.accountPriority {
				break
			}
			// Cold start only: with no bound account the session has no cache
			// anywhere, so placement is free and should go where the upstream says
			// there is the most input budget left. A warm session never reaches this
			// — moving it is what pinSticky exists to prevent.
			if stickyID == 0 && candidate.itpmRemaining >= 0 && selected.itpmRemaining >= 0 &&
				coldStartBudgetBucket(candidate.itpmRemaining) != coldStartBudgetBucket(selected.itpmRemaining) {
				if coldStartBudgetBucket(candidate.itpmRemaining) > coldStartBudgetBucket(selected.itpmRemaining) {
					selectedIndex = index
				}
				continue
			}
			// Mirrors the weight_tier ordering: freshly refreshed beats normal beats
			// rate-limited, and within a tier fewer strikes wins.
			if candidate.weightTier != selected.weightTier {
				if candidate.weightTier < selected.weightTier {
					selectedIndex = index
				}
				continue
			}
			if key.RateLimitDownweightEnabled && candidate.consecutive429 != selected.consecutive429 {
				if candidate.consecutive429 < selected.consecutive429 {
					selectedIndex = index
				}
				continue
			}
			if loadAware {
				if gatewayCandidateLoad(candidate.inflight, candidate.account.Concurrency) < gatewayCandidateLoad(selected.inflight, selected.account.Concurrency) ||
					(gatewayCandidateLoad(candidate.inflight, candidate.account.Concurrency) == gatewayCandidateLoad(selected.inflight, selected.account.Concurrency) && gatewayCandidateRPMLoad(candidate.rpm, candidate.account.BaseRPM) < gatewayCandidateRPMLoad(selected.rpm, selected.account.BaseRPM)) {
					selectedIndex = index
				}
				continue
			}
			// Strategy dispatch modes compare only accounts bound to the same
			// strategy: "serial" concentrates traffic on the busiest account,
			// "balance" spreads it to the least loaded one.
			if candidate.strategyID > 0 && candidate.strategyID == selected.strategyID {
				switch candidate.strategyMode {
				case "serial":
					if candidate.rpm > selected.rpm || (candidate.rpm == selected.rpm && candidate.inflight > selected.inflight) {
						selectedIndex = index
					}
				case "balance":
					if gatewayCandidateLoad(candidate.inflight, candidate.account.Concurrency) < gatewayCandidateLoad(selected.inflight, selected.account.Concurrency) ||
						(gatewayCandidateLoad(candidate.inflight, candidate.account.Concurrency) == gatewayCandidateLoad(selected.inflight, selected.account.Concurrency) && gatewayCandidateRPMLoad(candidate.rpm, candidate.account.BaseRPM) < gatewayCandidateRPMLoad(selected.rpm, selected.account.BaseRPM)) {
						selectedIndex = index
					}
				case "round_robin":
					candidateRPM := gatewayCandidateRPMLoad(candidate.rpm, candidate.account.BaseRPM)
					selectedRPM := gatewayCandidateRPMLoad(selected.rpm, selected.account.BaseRPM)
					if candidateRPM < selectedRPM ||
						(candidateRPM == selectedRPM && gatewayCandidateOlder(candidate.lastUsed, selected.lastUsed)) ||
						(candidateRPM == selectedRPM && candidate.lastUsed == selected.lastUsed && candidate.account.ID < selected.account.ID) {
						selectedIndex = index
					}
				case "concentrated":
					// Accounts already carrying traffic form the active set; an
					// account at zero RPM is 待调度 and only gets promoted when no
					// active account can take the request. Within the active set the
					// busiest account is filled first.
					candidateActive := candidate.rpm > 0
					selectedActive := selected.rpm > 0
					if candidateActive != selectedActive {
						if candidateActive {
							selectedIndex = index
						}
						continue
					}
					if candidateActive {
						if candidate.rpm > selected.rpm || (candidate.rpm == selected.rpm && candidate.inflight > selected.inflight) {
							selectedIndex = index
						}
						continue
					}
					// Promote deterministically so a burst does not wake several
					// idle accounts at once.
					if gatewayCandidateOlder(candidate.lastUsed, selected.lastUsed) ||
						(candidate.lastUsed == selected.lastUsed && candidate.account.ID < selected.account.ID) {
						selectedIndex = index
					}
				}
			}
		}
		if selectedIndex < 0 {
			if !allowStrategyFallback && shareBlockedThisPass > 0 {
				// Strategy weights select the preferred branch, but they must not turn
				// usable capacity in another strategy of this group into a 503. Retry
				// once with only the share gate relaxed; every actual capacity check
				// above remains active.
				reservationFailures := diagnostics.ITPMReservationBlocked
				shareBlocks := diagnostics.StrategyShareBlocked
				diagnostics = gatewayCapacityDiagnostics{
					Candidates:             len(candidates),
					StrategyShareBlocked:   shareBlocks,
					ITPMReservationBlocked: reservationFailures,
				}
				allowStrategyFallback = true
				continue
			}
			return gatewayAccount{}, &gatewayCapacityError{groupID: key.GroupID, diagnostics: diagnostics}
		}
		selectedCandidate = candidates[selectedIndex]
		selected = selectedCandidate.account
		selected.ITPMProtection = selectedCandidate.itpmProtection
		selected.ITPMWindowSeconds = selectedCandidate.itpmWindow
		selected.ITPMSoftLimit = selectedCandidate.itpmSoft
		selected.ITPMHardLimit = selectedCandidate.itpmHard
		selected.ITPMStrictHard = selectedCandidate.account.ITPMStrictHard
		reserveDemand := demand
		if selected.ID == stickyID {
			reserveDemand = stickyDemand
		}
		allowed, reserveErr := a.reserveGatewayITPM(&selected, reserveDemand, selectedCandidate.itpm, stickyID == selected.ID, selectedCandidate.inflight)
		if reserveErr != nil {
			return gatewayAccount{}, reserveErr
		}
		if !allowed {
			diagnostics.ITPMReservationBlocked++
			reservationBlocked[selected.ID] = true
			allowStrategyFallback = false
			continue
		}
		break
	}
	reservationHeld := selected.ITPMReservationID != ""
	defer func() {
		if reservationHeld {
			a.releaseGatewayITPM(selected)
		}
	}()
	if key.RPMDispatchEnabled || selected.RPMStrategy == "fixed" || dispatchModeReservesRPM(selectedCandidate.strategyMode) || selectedCandidate.pacing == "interval" {
		// Reserve RPM in the same transaction as selection so a concurrent
		// burst cannot overfill one account before opening the next. Fixed,
		// round-robin and concentrated accounts reserve here to keep their
		// policy hard; concentrated in particular would otherwise let a burst
		// see every account at zero RPM and promote all of them at once.
		// Interval pacing reserves too: its per-interval admission count reads
		// this table, so the event must land before the transaction commits.
		if _, err := tx.Exec(`INSERT INTO account_rpm_events (account_id) VALUES (?)`, selected.ID); err != nil {
			return gatewayAccount{}, err
		}
		selected.RPMReserved = true
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
	reservationHeld = false
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

func gatewayCandidateOlder(candidateLastUsed, selectedLastUsed string) bool {
	if candidateLastUsed == selectedLastUsed {
		return false
	}
	if candidateLastUsed == "" {
		return true
	}
	if selectedLastUsed == "" {
		return false
	}
	return candidateLastUsed < selectedLastUsed
}

// gatewayShouldQueue reports whether an exhausted group should park the request.
// The group toggle wins outright; otherwise a strategy that governs how many
// accounts are in play opts the group in automatically.
func (a *app) gatewayShouldQueue(key gatewayKey) bool {
	return key.CapacityQueueEnabled || a.groupQueuesWhenFull(key.GroupID) || a.groupUsesDispatchPacing(key.GroupID)
}

// dispatchModeReservesRPM reports whether a mode needs its RPM counted inside
// the selection transaction rather than after the upstream call.
func dispatchModeReservesRPM(mode string) bool {
	return mode == "round_robin" || mode == "concentrated"
}

// groupUsesRPMReservationStrategy reports whether any account reachable through
// the group resolves a strategy whose mode needs serialized selection.
func (a *app) groupUsesRPMReservationStrategy(groupID string) bool {
	return a.groupHasDispatchMode(groupID, dispatchModesQueueWhenFull)
}

// groupQueuesWhenFull reports whether the group's strategies should park a
// request instead of rejecting it when every account is capped.
func (a *app) groupQueuesWhenFull(groupID string) bool {
	return a.groupHasDispatchMode(groupID, dispatchModesQueueWhenFull)
}

func (a *app) groupHasDispatchMode(groupID string, modes []string) bool {
	if len(modes) == 0 {
		return false
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(modes)), ",")
	args := make([]any, 0, len(modes)+1)
	args = append(args, groupID)
	for _, mode := range modes {
		args = append(args, mode)
	}
	var found int
	err := a.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM account_groups ag
		JOIN accounts a ON a.id = ag.account_id
		LEFT JOIN groups g ON g.id = ag.group_id
		LEFT JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, g.strategy_id) AND ds.deleted_at IS NULL
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND a.archived_at IS NULL AND COALESCE(ds.capacity_enabled, 1) = 1 AND ds.dispatch_mode IN (`+placeholders+`)
		LIMIT 1)`, args...).Scan(&found)
	return err == nil && found > 0
}

// groupUsesDispatchPacing reports whether any account reachable through the
// group resolves a strategy with a dispatch pacing mode. Paced admission reads
// its counters inside the selection transaction, so selections must be
// serialized or a concurrent burst would see the same count and overfill the
// per-interval / in-flight cap.
func (a *app) groupUsesDispatchPacing(groupID string) bool {
	var found int
	err := a.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM account_groups ag
		JOIN accounts a ON a.id = ag.account_id
		LEFT JOIN groups g ON g.id = ag.group_id
		LEFT JOIN dispatch_strategies ds ON ds.id = COALESCE(a.strategy_id, g.strategy_id) AND ds.deleted_at IS NULL
		WHERE ag.group_id = ? AND a.deleted_at IS NULL AND a.archived_at IS NULL AND (
			(COALESCE(ds.capacity_enabled, 1) = 1 AND COALESCE(ds.dispatch_pacing, '') != '')
			OR COALESCE(ds.smooth_cold_start_enabled, 0) = 1
			OR a.extra_json LIKE '%"smooth_cold_start_enabled":true%'
		)
		LIMIT 1)`, groupID).Scan(&found)
	return err == nil && found > 0
}

func (a *app) recordGatewayAccountRPM(accountID int64) {
	_, err := a.db.Exec(`INSERT INTO account_rpm_events (account_id) VALUES (?)`, accountID)
	logDatabaseWriteError("insert account RPM event", err)
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
	_, err := a.db.Exec(`INSERT INTO dispatch_sessions (session_hash, api_key_id, account_id, expires_at) VALUES (?, ?, ?, ?) ON CONFLICT(session_hash, api_key_id) DO UPDATE SET account_id = excluded.account_id, expires_at = excluded.expires_at`, sessionHash, apiKeyID, accountID, expires)
	logDatabaseWriteError("bind gateway sticky session", err)
}

// updateGatewaySessionCost remembers the request's settled uncached input so
// the session's next dispatch reserves ITPM by real cost instead of the body
// estimate. cache_read stays excluded, mirroring settleGatewayITPM. The
// cache_creation contribution is capped: a steady turn writes a few thousand
// incremental tokens that do predict the next turn, while a full prefix
// rebuild is a one-off write that would poison the estimate for the following
// request.
func (a *app) updateGatewaySessionCost(sessionHash string, apiKeyID int64, usage tokenUsage) {
	actual := usage.Input + min(usage.CacheCreation, gatewayITPMSmallRequest)
	if sessionHash == "" || apiKeyID <= 0 || actual <= 0 {
		return
	}
	_, err := a.db.Exec(`UPDATE dispatch_sessions SET last_input_tokens = ? WHERE session_hash = ? AND api_key_id = ?`, actual, sessionHash, apiKeyID)
	logDatabaseWriteError("update gateway session input cost", err)
}

// releaseGatewaySessionBinding drops the session's account binding so the next
// acquisition places it like a cold start — on the account with the most input
// budget — and that account becomes the session's new sticky home. Used when
// the bound account cannot absorb the request within the queue window:
// rebuilding the cache elsewhere beats failing the request.
func (a *app) releaseGatewaySessionBinding(sessionHash string, apiKeyID int64) {
	if sessionHash == "" || apiKeyID <= 0 {
		return
	}
	_, err := a.db.Exec(`DELETE FROM dispatch_sessions WHERE session_hash = ? AND api_key_id = ?`, sessionHash, apiKeyID)
	logDatabaseWriteError("release gateway session binding", err)
}

// gatewayStickyWaitExhausted reports whether the capacity failure came from the
// pinned account itself (the sticky pin or its ITPM budget) rather than the
// whole pool, i.e. rebinding to another account could still serve the request.
func gatewayStickyWaitExhausted(err error) bool {
	diagnostics, ok := gatewayCapacityDiagnosticsFromError(err)
	return ok && (diagnostics.StickyPinned > 0 || diagnostics.ITPMBlocked > 0 || diagnostics.ITPMReservationBlocked > 0)
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
	_, err := a.db.Exec(`UPDATE account_inflight SET requests = CASE WHEN requests > 0 THEN requests - 1 ELSE 0 END WHERE account_id = ?`, accountID)
	logDatabaseWriteError("release gateway account", err)
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
			if buckets, ok := nested["cache_creation"].(map[string]any); ok {
				usage.CacheCreation5M = max(usage.CacheCreation5M, intFromJSON(buckets["ephemeral_5m_input_tokens"]))
				usage.CacheCreation1H = max(usage.CacheCreation1H, intFromJSON(buckets["ephemeral_1h_input_tokens"]))
			}
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

func stickyAlreadyTried(excluded map[int64]bool, stickyID int64) bool {
	return stickyID != 0 && excluded[stickyID]
}
