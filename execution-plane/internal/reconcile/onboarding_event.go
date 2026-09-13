package reconcile

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/credential"
)

var ErrOnboardingRuntimeEvent = errors.New("CCMAX onboarding runtime event is invalid")

type OnboardingRuntimePayload struct {
	IntentID string
}

func IsOnboardingRuntimeEvent(eventType string) bool {
	switch eventType {
	case "account.runtime.provision_requested", "account.credential.migrate_requested", "account.credential.rotate_requested":
		return true
	default:
		return false
	}
}

// DecodeOnboardingRuntimePayload accepts exactly one opaque intent reference.
// A permissive map decode would silently accept duplicate or future fields and
// could turn an unaudited CCMAX payload change into execution authority.
func DecodeOnboardingRuntimePayload(payloadJSON []byte) (OnboardingRuntimePayload, error) {
	decoder := json.NewDecoder(bytes.NewReader(payloadJSON))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') || !decoder.More() {
		return OnboardingRuntimePayload{}, ErrOnboardingRuntimeEvent
	}
	token, err := decoder.Token()
	key, ok := token.(string)
	if err != nil || !ok || key != "onboarding_intent_id" {
		return OnboardingRuntimePayload{}, ErrOnboardingRuntimeEvent
	}
	var payload OnboardingRuntimePayload
	if err := decoder.Decode(&payload.IntentID); err != nil || credential.ValidateTransportID(payload.IntentID) != nil ||
		secretLookingOnboardingID(payload.IntentID) || decoder.More() {
		return OnboardingRuntimePayload{}, ErrOnboardingRuntimeEvent
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || decoder.Decode(&struct{}{}) != io.EOF {
		return OnboardingRuntimePayload{}, ErrOnboardingRuntimeEvent
	}
	return payload, nil
}

func secretLookingOnboardingID(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "sk-") || strings.Contains(lower, "bearer ")
}
