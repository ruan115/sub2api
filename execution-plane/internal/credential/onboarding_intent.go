package credential

import (
	"context"
	"encoding/json"
	"fmt"
)

const onboardingIntentAADSchema = "ccmax.onboarding-intent.v1"

var allowedOnboardingIntentSources = map[string]struct{}{
	"session_key": {}, "oauth_code": {}, "setup_token": {},
	"api_key": {}, "cookie": {}, "credential_import": {},
}

type OnboardingIntentMetadata struct {
	IntentID          string
	AccountID         string
	DesiredGeneration uint64
	Source            string
	AuthType          string
}

type onboardingIntentAADPayload struct {
	Schema            string `json:"schema"`
	IntentID          string `json:"intent_id"`
	AccountID         string `json:"account_id"`
	DesiredGeneration uint64 `json:"desired_generation"`
	Source            string `json:"source"`
	AuthType          string `json:"auth_type"`
}

func (s *Service) SealOnboardingIntent(ctx context.Context, metadata OnboardingIntentMetadata, plaintext []byte) (Envelope, error) {
	aad, err := metadata.canonicalAAD()
	if err != nil {
		return Envelope{}, err
	}
	return s.sealWithAAD(ctx, aad, plaintext)
}

func (s *Service) OpenOnboardingIntent(ctx context.Context, metadata OnboardingIntentMetadata, envelope Envelope) ([]byte, error) {
	aad, err := metadata.canonicalAAD()
	if err != nil {
		return nil, err
	}
	return s.openWithAAD(ctx, aad, envelope)
}

func (m OnboardingIntentMetadata) Validate() error {
	_, err := m.canonicalAAD()
	return err
}

func (m OnboardingIntentMetadata) canonicalAAD() ([]byte, error) {
	if ValidateTransportID(m.IntentID) != nil || validateMetadataString(m.AccountID, 128, false) != nil ||
		m.DesiredGeneration == 0 || validateMetadataString(m.Source, 32, true) != nil ||
		validateMetadataString(m.AuthType, 32, true) != nil {
		return nil, fmt.Errorf("%w: onboarding intent metadata", ErrInvalidMetadata)
	}
	if _, allowed := allowedOnboardingIntentSources[m.Source]; !allowed {
		return nil, fmt.Errorf("%w: onboarding source", ErrInvalidMetadata)
	}
	payload := onboardingIntentAADPayload{
		Schema: onboardingIntentAADSchema, IntentID: m.IntentID, AccountID: m.AccountID,
		DesiredGeneration: m.DesiredGeneration, Source: m.Source, AuthType: m.AuthType,
	}
	aad, err := json.Marshal(payload)
	if err != nil || len(aad) > maxAADBytes {
		return nil, fmt.Errorf("%w: onboarding intent AAD", ErrInvalidMetadata)
	}
	return aad, nil
}
