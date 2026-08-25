package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

var gatewayStreamHedgeDelay = 20 * time.Millisecond

const (
	gatewayStreamBootstrapLimit   = 1 << 20
	gatewayAdaptiveDefaultDelay   = 250 * time.Millisecond
	gatewayAdaptiveMinDelay       = 150 * time.Millisecond
	gatewayAdaptiveMaxDelay       = 750 * time.Millisecond
	gatewayAdaptiveSampleLimit    = 64
	gatewayAdaptiveCreditUnit     = 10
	gatewayAdaptiveCreditCapacity = 20
)

type gatewayHedgeController struct {
	mu     sync.Mutex
	groups map[string]*gatewayHedgeGroupState
}

type gatewayHedgeGroupState struct {
	credits int
	samples []time.Duration
}

func newGatewayHedgeController() *gatewayHedgeController {
	return &gatewayHedgeController{groups: map[string]*gatewayHedgeGroupState{}}
}

func (c *gatewayHedgeController) begin(groupID string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.group(groupID)
	if state.credits < gatewayAdaptiveCreditCapacity {
		state.credits++
	}
	if len(state.samples) < 5 {
		return gatewayAdaptiveDefaultDelay
	}
	samples := append([]time.Duration(nil), state.samples...)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	index := (len(samples)*9+9)/10 - 1
	delay := samples[index]
	if delay < gatewayAdaptiveMinDelay {
		return gatewayAdaptiveMinDelay
	}
	if delay > gatewayAdaptiveMaxDelay {
		return gatewayAdaptiveMaxDelay
	}
	return delay
}

func (c *gatewayHedgeController) reserve(groupID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.group(groupID)
	if state.credits < gatewayAdaptiveCreditUnit {
		return false
	}
	state.credits -= gatewayAdaptiveCreditUnit
	return true
}

func (c *gatewayHedgeController) refund(groupID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.group(groupID)
	state.credits += gatewayAdaptiveCreditUnit
	if state.credits > gatewayAdaptiveCreditCapacity {
		state.credits = gatewayAdaptiveCreditCapacity
	}
}

func (c *gatewayHedgeController) observe(groupID string, ttft time.Duration) {
	if ttft <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.group(groupID)
	state.samples = append(state.samples, ttft)
	if len(state.samples) > gatewayAdaptiveSampleLimit {
		copy(state.samples, state.samples[len(state.samples)-gatewayAdaptiveSampleLimit:])
		state.samples = state.samples[:gatewayAdaptiveSampleLimit]
	}
}

func (c *gatewayHedgeController) group(groupID string) *gatewayHedgeGroupState {
	state := c.groups[groupID]
	if state == nil {
		state = &gatewayHedgeGroupState{credits: gatewayAdaptiveCreditUnit - 1}
		c.groups[groupID] = state
	}
	return state
}

type gatewayHedgeAttempt struct {
	account  gatewayAccount
	prepared claudePreparedRequest
	response *http.Response
	started  time.Time
}

type gatewayHedgeTerminal struct {
	status       int
	body         []byte
	plainMessage string
	accountID    int64
}

type gatewayHedgeOutcome struct {
	attempt  *gatewayHedgeAttempt
	failure  *gatewayUpstreamFailure
	terminal *gatewayHedgeTerminal
	err      error
}

type gatewayReplayReadCloser struct {
	io.Reader
	io.Closer
}

func (a *app) handleGatewayStreamHedge(w http.ResponseWriter, r *http.Request, key gatewayKey, body []byte, model, session string, excluded map[int64]bool, adaptive bool) (bool, *gatewayUpstreamFailure, error) {
	primary, err := a.acquireGatewayAccountWithPolicy(key, "", model, excluded, adaptive)
	if err != nil {
		return false, nil, err
	}
	excluded[primary.ID] = true
	if accountRequestPassthrough(primary) {
		a.releaseGatewayAccount(primary.ID)
		delete(excluded, primary.ID)
		return false, nil, nil
	}

	results := make(chan gatewayHedgeOutcome, 2)
	type runningCandidate struct {
		accountID int64
		cancel    context.CancelFunc
	}
	running := make([]runningCandidate, 0, 2)
	launch := func(account gatewayAccount) {
		attemptCtx, cancel := context.WithCancel(r.Context())
		running = append(running, runningCandidate{accountID: account.ID, cancel: cancel})
		candidateRequest := r.Clone(attemptCtx)
		go func() {
			results <- a.executeGatewayHedgeCandidate(candidateRequest, key, body, model, account)
		}()
	}
	launch(primary)

	pending := 1
	secondStarted := false
	delay := gatewayStreamHedgeDelay
	if adaptive {
		delay = a.streamHedges.begin(key.GroupID)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var lastFailure *gatewayUpstreamFailure
	var lastErr error
	var terminal *gatewayHedgeTerminal

	startSecond := func() bool {
		if secondStarted {
			return false
		}
		secondStarted = true
		secondary, acquireErr := a.acquireGatewayAccountWithPolicy(key, "", model, excluded, adaptive)
		if acquireErr != nil {
			lastErr = acquireErr
			return false
		}
		excluded[secondary.ID] = true
		if accountRequestPassthrough(secondary) {
			a.releaseGatewayAccount(secondary.ID)
			delete(excluded, secondary.ID)
			return false
		}
		pending++
		launch(secondary)
		return true
	}

	for pending > 0 {
		select {
		case outcome := <-results:
			pending--
			if outcome.attempt != nil {
				if adaptive && outcome.attempt.account.ID == primary.ID {
					a.streamHedges.observe(key.GroupID, time.Since(outcome.attempt.started))
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				for _, candidate := range running {
					if candidate.accountID != outcome.attempt.account.ID {
						candidate.cancel()
					}
				}
				a.bindGatewayStickySession(key.ID, session, outcome.attempt.account.ID)
				drained := a.drainGatewayHedgeOutcomes(results, pending)
				a.forwardGatewayHedgeWinner(w, r, key, model, outcome.attempt, drained)
				for _, candidate := range running {
					if candidate.accountID == outcome.attempt.account.ID {
						candidate.cancel()
					}
				}
				return true, nil, nil
			}
			if outcome.failure != nil {
				lastFailure = outcome.failure
			}
			if outcome.terminal != nil && terminal == nil {
				terminal = outcome.terminal
			}
			if outcome.err != nil && !errors.Is(outcome.err, context.Canceled) {
				lastErr = outcome.err
			}
			if outcome.terminal != nil {
				continue
			}
			if !secondStarted {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				startSecond()
			}
		case <-timer.C:
			if !adaptive || a.streamHedges.reserve(key.GroupID) {
				if !startSecond() && adaptive {
					a.streamHedges.refund(key.GroupID)
				}
			}
		case <-r.Context().Done():
			for _, candidate := range running {
				candidate.cancel()
			}
			_ = a.drainGatewayHedgeOutcomes(results, pending)
			return true, nil, r.Context().Err()
		}
	}

	if terminal != nil {
		attributeGatewayErrorAccount(w, terminal.accountID)
		if terminal.plainMessage != "" {
			writeError(w, terminal.status, terminal.plainMessage)
		} else {
			writeSub2CompatibilityError(w, terminal.status, terminal.body, false)
		}
		return true, nil, nil
	}
	return false, lastFailure, lastErr
}

func (a *app) executeGatewayHedgeCandidate(r *http.Request, key gatewayKey, body []byte, model string, account gatewayAccount) (outcome gatewayHedgeOutcome) {
	queueRelease := func() {}
	var response *http.Response
	keepResponse := false
	defer func() {
		queueRelease()
		if !keepResponse {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			a.releaseGatewayAccount(account.ID)
		}
	}()

	resolved, err := a.ensureGatewayAccountToken(r.Context(), account)
	if err != nil {
		return gatewayHedgeOutcome{err: err}
	}
	account = resolved
	if err := validateGatewayAccountCredential(account); err != nil {
		return gatewayHedgeOutcome{err: err}
	}
	if resolvedAccount, fingerprintErr := a.ensureGatewayAccountFingerprint(account, r.Header); fingerprintErr == nil {
		account = resolvedAccount
	}
	prepared, err := prepareClaudeRequest(r, body, account, model, false)
	if err != nil {
		return gatewayHedgeOutcome{terminal: &gatewayHedgeTerminal{status: http.StatusBadRequest, plainMessage: err.Error(), accountID: account.ID}, err: err}
	}
	prepared.RejectAnthropicDowngrade = key.RejectAnthropicDowngrade
	upstreamURL, err := upstreamClaudeURL(account.ExtraJSON, "/v1/messages")
	if err != nil {
		return gatewayHedgeOutcome{err: err}
	}
	upstreamRequest, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL, bytes.NewReader(prepared.Body))
	if err != nil {
		return gatewayHedgeOutcome{err: err}
	}
	if err := buildClaudeHeaders(upstreamRequest.Header, r.Header, prepared, account.CredentialsJSON); err != nil {
		return gatewayHedgeOutcome{err: err}
	}
	if !account.ProxyID.Valid {
		return gatewayHedgeOutcome{err: errors.New("CCMAX account must bind an active proxy")}
	}
	proxyURL, err := a.proxyURL(account.ProxyID.Int64)
	if err != nil {
		return gatewayHedgeOutcome{err: errors.New("CCMAX account proxy is unavailable")}
	}
	client, err := clientForProxy(proxyURL)
	if err != nil {
		return gatewayHedgeOutcome{err: err}
	}
	queueRelease, _ = a.acquireUserMessageQueue(r.Context(), account, body, false)
	if queueRelease == nil {
		queueRelease = func() {}
	}
	started := time.Now()
	// Every candidate consumes upstream request capacity even when it loses the
	// race and is canceled. Logical usage and billing are still winner-only.
	if !account.RPMReserved {
		a.recordGatewayAccountRPM(account.ID)
	}
	response, err = doGatewayUpstreamRequest(r, client, upstreamRequest, prepared)
	if err != nil {
		return gatewayHedgeOutcome{err: err}
	}
	response = retryGatewayCompatibility400(client, upstreamRequest, response, prepared, started)
	queueRelease()
	queueRelease = func() {}
	if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
		a.captureGatewayUpstreamState(account.ID, model, key.OverloadCooldownSeconds, response)
	}
	if retryableGatewayStatus(response.StatusCode) {
		failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
			a.captureAccountUpstreamFailure(account, response.StatusCode, failureBody)
		}
		return gatewayHedgeOutcome{failure: &gatewayUpstreamFailure{status: response.StatusCode, header: response.Header.Clone(), body: failureBody, account: account}}
	}
	if response.StatusCode >= 400 {
		failureBody, _ := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		if !skipGatewayDefaultErrorHandling(prepared, response.StatusCode) {
			a.captureAccountUpstreamFailure(account, response.StatusCode, failureBody)
		}
		return gatewayHedgeOutcome{terminal: &gatewayHedgeTerminal{status: response.StatusCode, body: failureBody, accountID: account.ID}}
	}

	buffered, reader, bootstrapErr := readGatewayStreamBootstrap(response.Body)
	if bootstrapErr != nil {
		var preOutputErr *gatewayPreOutputStreamError
		if errors.As(bootstrapErr, &preOutputErr) {
			if !skipGatewayDefaultErrorHandling(prepared, preOutputErr.status) {
				a.captureGatewayUpstreamState(account.ID, model, key.OverloadCooldownSeconds, &http.Response{StatusCode: preOutputErr.status, Header: response.Header.Clone()})
				a.captureAccountUpstreamFailure(account, preOutputErr.status, preOutputErr.body)
			}
			return gatewayHedgeOutcome{failure: &gatewayUpstreamFailure{status: preOutputErr.status, header: response.Header.Clone(), body: preOutputErr.body, account: account}, err: bootstrapErr}
		}
		return gatewayHedgeOutcome{err: bootstrapErr}
	}
	response.Body = &gatewayReplayReadCloser{Reader: io.MultiReader(bytes.NewReader(buffered), reader), Closer: response.Body}
	keepResponse = true
	return gatewayHedgeOutcome{attempt: &gatewayHedgeAttempt{account: account, prepared: prepared, response: response, started: started}}
}

func readGatewayStreamBootstrap(body io.Reader) ([]byte, *bufio.Reader, error) {
	reader := bufio.NewReaderSize(body, 64<<10)
	buffered := bytes.Buffer{}
	for buffered.Len() <= gatewayStreamBootstrapLimit {
		block, err := readGatewaySSEBlock(reader)
		if len(block) > 0 {
			_, _ = buffered.Write(block)
			eventType, eventBody := gatewaySSEEvent(block)
			if eventType == "error" {
				status := http.StatusForbidden
				if gjson.GetBytes(eventBody, "error.type").String() == "overloaded_error" {
					status = 529
				}
				return nil, reader, &gatewayPreOutputStreamError{status: status, body: eventBody}
			}
			if eventType != "" && eventType != "ping" {
				return append([]byte(nil), buffered.Bytes()...), reader, nil
			}
			if eventType == "" && len(eventBody) > 0 {
				return append([]byte(nil), buffered.Bytes()...), reader, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, reader, errors.New("upstream stream ended before first event")
			}
			return nil, reader, fmt.Errorf("upstream stream bootstrap failed: %w", err)
		}
	}
	return nil, reader, errors.New("upstream stream bootstrap exceeded buffer limit")
}

func (a *app) drainGatewayHedgeOutcomes(results <-chan gatewayHedgeOutcome, count int) <-chan struct{} {
	done := make(chan struct{})
	if count == 0 {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		for i := 0; i < count; i++ {
			outcome := <-results
			if outcome.attempt != nil {
				_ = outcome.attempt.response.Body.Close()
				a.releaseGatewayAccount(outcome.attempt.account.ID)
			}
		}
	}()
	return done
}

func (a *app) forwardGatewayHedgeWinner(w http.ResponseWriter, r *http.Request, key gatewayKey, model string, attempt *gatewayHedgeAttempt, drained <-chan struct{}) {
	defer a.releaseGatewayAccount(attempt.account.ID)
	defer attempt.response.Body.Close()
	attributeGatewayErrorAccount(w, attempt.account.ID)
	usage, forwardErr := forwardGatewayResponse(w, attempt.response, true, attempt.account, key.GroupID, attempt.prepared)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
	}
	var downgradeErr *gatewayModelDowngradeError
	if errors.As(forwardErr, &downgradeErr) {
		if usage.hasUsage() {
			a.recordGatewayRejectedDowngradeUsage(r.Context(), key, attempt.account, downgradeErr.actual, gatewayRecordedStream(r.Context(), true), attempt.response.Header.Get("request-id"), usage, attempt.started)
		}
	} else if forwardErr == nil || usage.hasUsage() {
		a.recordGatewayUsage(r.Context(), key, attempt.account, model, gatewayRecordedStream(r.Context(), true), attempt.response.Header.Get("request-id"), usage, attempt.started)
	}
	if errors.As(forwardErr, &downgradeErr) && !downgradeErr.committed {
		copyGatewayRequestID(w.Header(), attempt.response.Header)
		writeAnthropicGatewayError(w, http.StatusBadGateway, "api_error", downgradeErr.Error())
	}
	var idleErr *gatewayStreamIdleError
	if errors.As(forwardErr, &idleErr) {
		a.captureGatewayStreamIdle(attempt.account.ID, idleErr.idle)
		attributeGatewayErrorEvent(w, "upstream_stream_idle", http.StatusGatewayTimeout, idleErr.Error())
		// The hedge lane owns the response, so a stalled winner cannot fall back
		// to the account loop; tell the client instead of hanging up silently.
		if !idleErr.streamed {
			if gatewayResponseStatus(w) != 0 {
				flusher, _ := w.(http.Flusher)
				writeGatewaySSEError(w, flusher, "upstream_stream_idle", idleErr.Error())
				return
			}
			writeAnthropicGatewayError(w, http.StatusGatewayTimeout, "timeout_error", idleErr.Error())
		}
	}
}
