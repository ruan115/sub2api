package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	claudeAPIVersion       = "2023-06-01"
	claudeCLIVersion       = "2.1.220"
	claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
)

type claudePreparedRequest struct {
	Body                     []byte
	Model                    string
	AuthType                 string
	OAuth                    bool
	Mimic                    bool
	ClaudeCode               bool
	Passthrough              bool
	Stream                   bool
	NonStreamBridge          bool
	CountTokens              bool
	RejectAnthropicDowngrade bool
	MaskQuotaHeaders         bool
	Compat                   *sub2service.CCMaxCompatibilityPrepared
	Credentials              map[string]any
}

func prepareClaudeRequest(r *http.Request, body []byte, account gatewayAccount, requestedModel string, countTokens bool) (claudePreparedRequest, error) {
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return claudePreparedRequest{}, errorsWithMessage("invalid Anthropic message request", err)
	}
	model, _ := payload["model"].(string)
	if strings.TrimSpace(model) == "" {
		return claudePreparedRequest{}, fmt.Errorf("invalid Anthropic message request: model is required")
	}
	if requestedModel == "" {
		requestedModel = model
	}
	credentials := decodeObject(account.CredentialsJSON)
	oauth := gatewayAccountUsesOAuth(account)
	stream, _ := payload["stream"].(bool)
	if accountRequestPassthrough(account) {
		return claudePreparedRequest{
			Body: append([]byte(nil), body...), Model: model, AuthType: account.AuthType, OAuth: oauth,
			Passthrough: true, Stream: stream, CountTokens: countTokens,
			Credentials: credentials,
		}, nil
	}

	accessToken, _ := stringObjectValue(credentials, "access_token")
	apiKey, _ := stringObjectValue(credentials, "api_key")
	accountUUID, _ := stringObjectValue(credentials, "account_uuid")
	extra := decodeObject(account.ExtraJSON)
	if accountUUID == "" {
		accountUUID, _ = stringObjectValue(extra, "account_uuid")
	}
	claudeUserID, _ := stringObjectValue(extra, "claude_user_id")
	mappedModel := requestedModel
	if !oauth {
		mappedModel = mappedAccountModel(account, requestedModel)
	}
	fingerprint := accountCompatibilityFingerprint(account)
	fieldPassthrough := gatewayFieldPassthroughConfig(r.Context())
	compat, err := sub2service.PrepareCCMaxCompatibilityRequest(sub2service.CCMaxCompatibilityInput{
		Body: body, ClientHeaders: r.Header, Model: requestedModel, Stream: stream,
		CountTokens: countTokens, OAuth: oauth, AccessToken: accessToken,
		APIKey: apiKey, AccountID: account.ID, AccountUUID: accountUUID,
		ClaudeUserID: claudeUserID, ClientIP: gatewayClientIP(r),
		ClientUserAgent: r.UserAgent(), APIKeyID: gatewayAPIKeyID(r.Context()),
		MappedModel: mappedModel, Fingerprint: fingerprint,
		ForceNonClaudeCode:        gatewayOpenAIChatRequest(r.Context()),
		NormalRequestMode:         gatewayNormalRequestMode(r.Context()),
		ClaudeCodeIdentityEnabled: gatewayClaudeCodeIdentity(r.Context()),
		MCPToolNames:              accountMCPToolNames(account, gatewayMCPToolNames(r.Context())),
		ServiceTierPassthrough:    fieldPassthrough.ServiceTier,
		InferenceGeoPassthrough:   fieldPassthrough.InferenceGeo,
		SpeedPassthrough:          fieldPassthrough.Speed,
		AnthropicBetaPassthrough:  fieldPassthrough.AnthropicBeta,
	})
	if err != nil {
		return claudePreparedRequest{}, err
	}
	if gatewayAnthropicNonStreamBridge(r.Context()) && !countTokens {
		compat, err = sub2service.ForceCCMaxCompatibilityStream(compat)
		if err != nil {
			return claudePreparedRequest{}, err
		}
		stream = true
	}
	return claudePreparedRequest{
		Body: compat.Body, Model: compat.Model, AuthType: account.AuthType, OAuth: oauth,
		Mimic: compat.Mimic, ClaudeCode: compat.ClaudeCode, Stream: stream,
		NonStreamBridge: gatewayAnthropicNonStreamBridge(r.Context()),
		CountTokens:     countTokens, Compat: compat,
		Credentials: credentials,
	}, nil
}

func accountRequestPassthrough(account gatewayAccount) bool {
	extra := decodeObject(account.ExtraJSON)
	value, _ := extra["request_passthrough"].(bool)
	return value
}

// accountMCPToolNames resolves the MCP tool naming lane for one account. The
// account setting is a three-state override: absent inherits the group switch,
// while an explicit value wins so a single account can be pulled out of the
// lane without touching the rest of the group.
func accountMCPToolNames(account gatewayAccount, groupEnabled bool) bool {
	extra := decodeObject(account.ExtraJSON)
	if value, ok := extra["mcp_tool_names"].(bool); ok {
		return value
	}
	return groupEnabled
}

func gatewayAccountUsesOAuth(account gatewayAccount) bool {
	switch account.AuthType {
	case "oauth", "setup_token", "setup-token":
		return true
	default:
		return false
	}
}

func errorsWithMessage(message string, err error) error {
	if err == nil {
		return fmt.Errorf("%s", message)
	}
	return fmt.Errorf("%s: %w", message, err)
}

func stringObjectValue(object map[string]any, key string) (string, bool) {
	value, ok := object[key].(string)
	value = strings.TrimSpace(value)
	return value, ok && value != ""
}

func accountSupportsModel(account gatewayAccount, requestedModel string) bool {
	extra := decodeObject(account.ExtraJSON)
	var configured []string
	for _, key := range []string{"supported_models", "available_models"} {
		switch values := extra[key].(type) {
		case []any:
			for _, value := range values {
				if model, ok := value.(string); ok {
					configured = append(configured, strings.TrimSpace(model))
				}
			}
		case []string:
			configured = append(configured, values...)
		case string:
			for _, model := range strings.Split(values, ",") {
				configured = append(configured, strings.TrimSpace(model))
			}
		}
	}
	if len(configured) == 0 {
		return true
	}
	for _, pattern := range configured {
		if modelPatternMatches(pattern, requestedModel) {
			return true
		}
	}
	return false
}

func modelPatternMatches(pattern, requestedModel string) bool {
	return pattern == "*" || pattern == requestedModel ||
		strings.HasSuffix(pattern, "*") && strings.HasPrefix(requestedModel, strings.TrimSuffix(pattern, "*"))
}

func mappedAccountModel(account gatewayAccount, requestedModel string) string {
	extra := decodeObject(account.ExtraJSON)
	mapping, ok := extra["model_mapping"].(map[string]any)
	if !ok {
		return requestedModel
	}
	if value, ok := mapping[requestedModel].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return requestedModel
}

func buildClaudeHeaders(target, source http.Header, prepared claudePreparedRequest, credentialsJSON string) error {
	if prepared.Compat != nil {
		for key, values := range prepared.Compat.Headers {
			for _, value := range values {
				target.Add(key, value)
			}
		}
		return nil
	}
	if prepared.Passthrough {
		return buildClaudePassthroughHeaders(target, source, prepared, credentialsJSON)
	}
	return fmt.Errorf("request has no Sub2API compatibility or passthrough mode")
}

func buildClaudePassthroughHeaders(target, source http.Header, prepared claudePreparedRequest, credentialsJSON string) error {
	for key, values := range source {
		lower := strings.ToLower(strings.TrimSpace(key))
		if lower == "authorization" || lower == "x-api-key" || lower == "cookie" ||
			lower == "host" || lower == "content-length" || isHopByHopHeader(key) {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
	credentials := decodeObject(credentialsJSON)
	if token, ok := stringObjectValue(credentials, "access_token"); prepared.OAuth && ok {
		target.Set("Authorization", "Bearer "+token)
	} else if token, ok := stringObjectValue(credentials, "api_key"); !prepared.OAuth && ok {
		target.Set("x-api-key", token)
	} else if prepared.OAuth {
		return fmt.Errorf("OAuth account has no access_token credential")
	} else {
		return fmt.Errorf("API-key account has no api_key credential")
	}
	if target.Get("Content-Type") == "" {
		target.Set("Content-Type", "application/json")
	}
	if target.Get("anthropic-version") == "" {
		target.Set("anthropic-version", claudeAPIVersion)
	}
	return nil
}
