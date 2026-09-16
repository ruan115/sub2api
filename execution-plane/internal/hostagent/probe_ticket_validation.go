package hostagent

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	executionv1 "github.com/Wei-Shaw/sub2api/execution-plane/gen/go/execution/v1"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/ticket"
)

var probeRequestIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func validProbeScope(scope string) bool { return scope == "health" || scope == "credential_key" }

// This checks wire shape and correlation, not authenticity. Only the worker's
// Guard verifies the control-plane signature and consumes its one-use nonce.
// No error returned here includes the bearer token or its claims.
func parseProbeTicketResponse(r *executionv1.ControlProbeTicketResponse) (ticket.Claims, time.Time, error) {
	var claims ticket.Claims
	deny := func() (ticket.Claims, time.Time, error) {
		return ticket.Claims{}, time.Time{}, ErrProbeTicketUnavailable
	}
	if r == nil || credential.ValidateTransportID(r.GetCommandId()) != nil || !probeRequestIDPattern.MatchString(r.GetRequestId()) || !validProbeScope(r.GetScope()) {
		return deny()
	}
	if r.GetErrorCode() != "" {
		if r.GetErrorCode() != "probe_ticket_unavailable" || r.GetExecutionTicket() != "" || r.GetExpiresAt() != nil {
			return deny()
		}
		return claims, time.Time{}, nil
	}
	if len(r.GetExecutionTicket()) == 0 || len(r.GetExecutionTicket()) > 4096 || r.GetExpiresAt() == nil ||
		r.GetExpiresAt().CheckValid() != nil || r.GetExpiresAt().GetNanos() != 0 {
		return deny()
	}
	parts := strings.Split(r.GetExecutionTicket(), ".")
	if len(parts) != 2 {
		return deny()
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] {
		return deny()
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != parts[1] || json.Unmarshal(payload, &claims) != nil {
		return deny()
	}
	if claims.Validate(time.Time{}) != nil || !probeRequestIDPattern.MatchString(claims.Nonce) || len(claims.Scopes) != 1 || claims.Scopes[0] != r.GetScope() ||
		claims.ExpiresAt != r.GetExpiresAt().GetSeconds() || claims.ExpiresAt-claims.IssuedAt > int64(maxProbeTicketWait/time.Second) {
		return deny()
	}
	for _, id := range []string{claims.AccountID, claims.SlotID, claims.NodeID} {
		if credential.ValidateTransportID(id) != nil {
			return deny()
		}
	}
	return claims, r.GetExpiresAt().AsTime(), nil
}
