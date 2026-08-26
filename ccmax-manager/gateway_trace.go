package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

const (
	gatewayTraceHeader        = "X-CCMAX-Request-Id"
	newAPIRequestIDHeader     = "X-Oneapi-Request-Id"
	maxGatewayCorrelationID   = 128
	maxDispatchDiagnosticsLen = 2048
)

type gatewayTraceContextKey struct{}

type gatewayRequestTrace struct {
	TraceID         string
	ClientRequestID string
}

func newGatewayRequestTrace(r *http.Request) gatewayRequestTrace {
	clientRequestID := ""
	if r != nil {
		clientRequestID = normalizeGatewayCorrelationID(r.Header.Get(newAPIRequestIDHeader))
	}
	return gatewayRequestTrace{
		TraceID:         "ccm_" + strings.TrimPrefix(newRequestID(), "req_"),
		ClientRequestID: clientRequestID,
	}
}

func normalizeGatewayCorrelationID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxGatewayCorrelationID {
		value = value[:maxGatewayCorrelationID]
	}
	return value
}

func gatewayTraceFromContext(ctx context.Context) gatewayRequestTrace {
	trace, _ := ctx.Value(gatewayTraceContextKey{}).(gatewayRequestTrace)
	return trace
}

func gatewayCorrelationIDs(ctx context.Context, upstreamRequestID string) (requestID, clientRequestID, traceID, upstreamID string) {
	trace := gatewayTraceFromContext(ctx)
	clientRequestID = normalizeGatewayCorrelationID(trace.ClientRequestID)
	traceID = normalizeGatewayCorrelationID(trace.TraceID)
	upstreamID = normalizeGatewayCorrelationID(upstreamRequestID)
	// request_id is the unique usage idempotency key. Never derive it from a
	// caller-controlled header, otherwise reusing a client ID could suppress
	// billing for a later request.
	requestID = upstreamID
	if requestID == "" {
		requestID = traceID
	}
	return
}

type gatewayCapacityDiagnostics struct {
	Candidates           int `json:"candidates"`
	Excluded             int `json:"excluded"`
	StickyPinned         int `json:"sticky_pinned"`
	StrategyMissing      int `json:"strategy_missing"`
	StrategyShareBlocked int `json:"strategy_share_blocked"`
	ModelCooldown        int `json:"model_cooldown"`
	ModelUnsupported     int `json:"model_unsupported"`
	ConcurrencyBlocked   int `json:"concurrency_blocked"`
	TemporaryRPMBlocked  int `json:"temporary_rpm_blocked"`
	TPMBlocked           int `json:"tpm_blocked"`
	ITPMBlocked          int `json:"itpm_blocked"`
	RPMBlocked           int `json:"rpm_blocked"`
}

type gatewayCapacityError struct {
	groupID     string
	diagnostics gatewayCapacityDiagnostics
}

func (e *gatewayCapacityError) Error() string {
	return fmt.Sprintf("no gateway account capacity for group %s (model, concurrency, or RPM limit)", strings.ToUpper(e.groupID))
}

func (e *gatewayCapacityError) Unwrap() error {
	return errNoGatewayAccountCapacity
}

func gatewayCapacityDiagnosticsFromError(err error) (gatewayCapacityDiagnostics, bool) {
	var capacityErr *gatewayCapacityError
	if !errors.As(err, &capacityErr) || capacityErr == nil {
		return gatewayCapacityDiagnostics{}, false
	}
	return capacityErr.diagnostics, true
}
