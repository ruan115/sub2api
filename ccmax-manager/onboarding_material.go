package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// prepareRuntimeOnboardingMaterial removes temporary material from the account
// input before normal account normalization can serialize it. The returned
// byte slices are owned by the caller and must be destroyed on every path.
func prepareRuntimeOnboardingMaterial(input *accountInput) (*runtimeOnboardingMaterial, error) {
	if input == nil || !input.ExecutionOnboarding {
		return nil, nil
	}
	if len(bytes.TrimSpace(input.Credentials)) != 0 && !emptyRawJSONObject(input.Credentials) {
		return nil, errors.New("execution onboarding credentials must use onboarding_secret")
	}
	source := strings.ToLower(strings.TrimSpace(input.OnboardingSource))
	secret := strings.TrimSpace(input.OnboardingSecret)
	legacySession := strings.TrimSpace(input.SessionKey)
	auxiliary := strings.TrimSpace(input.OnboardingAuxiliary)
	input.OnboardingSource = ""
	input.OnboardingSecret = ""
	input.OnboardingAuxiliary = ""
	input.SessionKey = ""
	input.Credentials = json.RawMessage(`{}`)
	if source == "" {
		source = "session_key"
	}
	if source == "session_key" {
		if secret != "" && legacySession != "" {
			return nil, errors.New("provide Session Key only once")
		}
		if secret == "" {
			secret = legacySession
		}
	} else if legacySession != "" {
		return nil, errors.New("session_key is only valid for Session Key onboarding")
	}
	if secret == "" || len(secret) > maxRuntimeOnboardingMaterialBytes || !utf8.ValidString(secret) || strings.ContainsAny(secret, "\x00\r\n") {
		return nil, errors.New("execution onboarding secret is invalid")
	}
	if len(auxiliary) > maxRuntimeOnboardingMaterialBytes || !utf8.ValidString(auxiliary) || strings.ContainsAny(auxiliary, "\x00\r\n") {
		return nil, errors.New("execution onboarding auxiliary material is invalid")
	}
	authType := strings.ToLower(strings.TrimSpace(input.AuthType))
	if authType == "" {
		authType = "oauth"
	}
	switch source {
	case "session_key", "cookie":
		if authType != "oauth" && authType != "setup_token" || auxiliary != "" {
			return nil, errors.New("execution onboarding source and auth type do not match")
		}
	case "oauth_code":
		if (authType != "oauth" && authType != "setup_token") || auxiliary == "" {
			return nil, errors.New("OAuth code onboarding requires a PKCE verifier")
		}
	case "setup_token":
		if authType != "setup_token" || auxiliary != "" {
			return nil, errors.New("Setup Token onboarding requires setup_token auth type")
		}
	case "api_key":
		if authType != "api_key" || auxiliary != "" {
			return nil, errors.New("API Key onboarding requires api_key auth type")
		}
	case "credential_import":
		if (authType != "oauth" && authType != "setup_token" && authType != "api_key") || auxiliary != "" || !json.Valid([]byte(secret)) {
			return nil, errors.New("credential import material is invalid")
		}
	default:
		return nil, errors.New("execution onboarding source is unsupported")
	}
	input.AuthType = authType
	return &runtimeOnboardingMaterial{
		Source: source, AuthType: authType, Secret: []byte(secret), Auxiliary: []byte(auxiliary),
	}, nil
}

func emptyRawJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte(`{}`))
}
