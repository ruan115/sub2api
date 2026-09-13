package reconcile

import (
	"errors"
	"testing"
)

func TestDecodeOnboardingRuntimePayloadRequiresOneOpaqueIntent(t *testing.T) {
	payload, err := DecodeOnboardingRuntimePayload([]byte(`{"onboarding_intent_id":"11111111-2222-4333-8444-555555555555"}`))
	if err != nil || payload.IntentID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("payload = %+v, %v", payload, err)
	}
	for _, invalid := range []string{
		`{}`,
		`[]`,
		`{"onboarding_intent_id":"intent-1","extra":true}`,
		`{"onboarding_intent_id":"intent-1","onboarding_intent_id":"intent-1"}`,
		`{"onboarding_intent_id":7}`,
		`{"intent_id":"intent-1"}`,
		`{"onboarding_intent_id":"sk-ant-secret"}`,
		`{"onboarding_intent_id":"intent-1"} {}`,
	} {
		if _, err := DecodeOnboardingRuntimePayload([]byte(invalid)); !errors.Is(err, ErrOnboardingRuntimeEvent) {
			t.Fatalf("payload %s error = %v", invalid, err)
		}
	}
}

func TestIsOnboardingRuntimeEventUsesClosedAllowlist(t *testing.T) {
	for _, eventType := range []string{
		"account.runtime.provision_requested",
		"account.credential.migrate_requested",
		"account.credential.rotate_requested",
	} {
		if !IsOnboardingRuntimeEvent(eventType) {
			t.Fatalf("event %q was not classified as onboarding", eventType)
		}
	}
	for _, eventType := range []string{
		"account.runtime.restore_requested",
		"account.proxy.change_requested",
		"account.runtime.destroy_requested",
		"account.runtime.provision_requested.extra",
	} {
		if IsOnboardingRuntimeEvent(eventType) {
			t.Fatalf("event %q was classified as onboarding", eventType)
		}
	}
}
