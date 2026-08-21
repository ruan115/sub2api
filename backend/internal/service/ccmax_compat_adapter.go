package service

// This file exposes a narrow adapter over the existing Anthropic gateway
// implementation. The standalone CCMAX manager uses this adapter so its
// compatibility lane cannot drift into a second, approximate implementation.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/anthropicfp"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// CCMaxCompatibilityInput contains the state owned by the standalone manager
// that the Sub2API request pipeline normally obtains from its repositories.
type CCMaxCompatibilityInput struct {
	Body            []byte
	ClientHeaders   http.Header
	Model           string
	Stream          bool
	CountTokens     bool
	OAuth           bool
	AccessToken     string
	APIKey          string
	AccountID       int64
	AccountUUID     string
	ClaudeUserID    string
	ClientIP        string
	ClientUserAgent string
	APIKeyID        int64
	MappedModel     string
	Fingerprint     *Fingerprint
	// ForceNonClaudeCode keeps protocol bridges on the same mimicry lane as
	// Sub2API's Chat Completions handler, regardless of the caller User-Agent.
	ForceNonClaudeCode bool
	// NormalRequestMode retains the OAuth mimicry wire shape while replacing
	// the security-oriented Claude Code expansion with a neutral assistant
	// prompt. The default false value preserves the original Sub2API lane.
	NormalRequestMode bool
}

const ccmaxNormalRequestExpansionPrompt = "You are an interactive assistant. Follow the user's instructions and provide a direct, helpful response."

// CCMaxCompatibilityPrepared is the exact wire request produced by the
// original Sub2API Anthropic OAuth defaults.
type CCMaxCompatibilityPrepared struct {
	Body          []byte
	LogicalBody   []byte
	Headers       http.Header
	OriginalModel string
	Model         string
	Mimic         bool
	ClaudeCode    bool
	ToolRewrite   *ToolNameRewrite
	input         CCMaxCompatibilityInput
}

type CCMaxCompatibilityRetryMode string

type CCMaxCompatibilityAccountPolicy struct {
	PoolMode                 bool
	PoolRetryable            bool
	PoolRetryCount           int
	SkipDefaultErrorHandling bool
}

const (
	CCMaxCompatibilityRetryThinking      CCMaxCompatibilityRetryMode = "thinking"
	CCMaxCompatibilityRetryThinkingTools CCMaxCompatibilityRetryMode = "thinking_tools"
	CCMaxCompatibilityRetryBudget        CCMaxCompatibilityRetryMode = "budget"
)

// ResolveCCMaxCompatibilityFingerprint applies the same create/upgrade rules
// used by IdentityService while leaving persistence to the standalone manager.
func ResolveCCMaxCompatibilityFingerprint(headers http.Header, existing *Fingerprint) (*Fingerprint, bool) {
	identity := &IdentityService{}
	now := time.Now()
	if existing == nil || existing.UpdatedAt > 0 && now.Sub(time.Unix(existing.UpdatedAt, 0)) >= 7*24*time.Hour {
		created := identity.createFingerprintFromHeaders(headers)
		created.ClientID = generateClientID()
		created.UpdatedAt = now.Unix()
		return created, true
	}

	next := *existing
	changed := false
	clientUA := strings.TrimSpace(headers.Get("User-Agent"))
	uaAcceptable := isAcceptableFingerprintUserAgent(clientUA)
	if !isAcceptableFingerprintUserAgent(next.UserAgent) {
		if uaAcceptable {
			mergeHeadersIntoFingerprint(&next, headers)
		} else {
			next.UserAgent = defaultFingerprint.UserAgent
		}
		changed = true
	} else if uaAcceptable && isNewerVersion(clientUA, next.UserAgent) {
		mergeHeadersIntoFingerprint(&next, headers)
		changed = true
	}
	if strings.TrimSpace(next.ClientID) == "" {
		next.ClientID = generateClientID()
		changed = true
	}
	if next.UpdatedAt <= 0 || now.Sub(time.Unix(next.UpdatedAt, 0)) >= 24*time.Hour {
		changed = true
	}
	if changed {
		next.UpdatedAt = now.Unix()
	}
	return &next, changed
}

// PrepareCCMaxCompatibilityRequest runs the original Sub2API default CCMAX
// transformations in their production order. Optional Sub2API settings retain
// their defaults: system injection on, dateline normalization on, metadata
// passthrough off, message-cache rewrite off, and 1h TTL injection off.
func PrepareCCMaxCompatibilityRequest(input CCMaxCompatibilityInput) (*CCMaxCompatibilityPrepared, error) {
	if !gjson.ValidBytes(input.Body) {
		return nil, fmt.Errorf("invalid Anthropic message request")
	}
	body := append([]byte(nil), input.Body...)
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		return nil, fmt.Errorf("invalid Anthropic message request: model is required")
	}
	if strings.TrimSpace(input.Model) == "" {
		input.Model = model
	}
	originalModel := input.Model
	metadataUserID := gjson.GetBytes(body, "metadata.user_id").String()
	claudeCode := isClaudeCodeClient(input.ClientHeaders.Get("User-Agent"), metadataUserID)
	if !input.CountTokens && !claudeCode && metadataUserID != "" {
		claudeCode = systemHasBillingAttributionBlock(body)
	}
	if input.ForceNonClaudeCode {
		claudeCode = false
	}
	mimic := input.OAuth && !claudeCode
	var toolRewrite *ToolNameRewrite

	if input.CountTokens {
		body = StripEmptyTextBlocks(body)
		if mimic {
			body, model = normalizeClaudeOAuthRequestBody(body, model, claudeOAuthNormalizeOptions{stripSystemCacheControl: true})
			if toolRewrite = buildToolNameRewriteFromBody(body); toolRewrite != nil {
				body = applyToolNameRewriteToBody(body, toolRewrite)
			} else {
				body = applyToolsLastCacheBreakpoint(body)
			}
		}
	} else {
		if mimic {
			var system any
			if raw := gjson.GetBytes(body, "system"); raw.Exists() {
				_ = json.Unmarshal([]byte(raw.Raw), &system)
			}
			expansionPrompt := ""
			if input.NormalRequestMode {
				expansionPrompt = ccmaxNormalRequestExpansionPrompt
			}
			body = rewriteSystemForNonClaudeCodeWithPromptBlocks(body, system, expansionPrompt, "")
			opts := claudeOAuthNormalizeOptions{}
			if metadataUserID == "" && input.Fingerprint != nil {
				userID := strings.TrimSpace(input.ClaudeUserID)
				if userID == "" {
					userID = input.Fingerprint.ClientID
				}
				sessionContext := &SessionContext{
					ClientIP: input.ClientIP, UserAgent: input.ClientUserAgent, APIKeyID: input.APIKeyID,
				}
				seed := buildStableSessionSeed(input.AccountID, sessionContextDiscriminator(sessionContext), extractFirstUserText(body))
				opts.injectMetadata = true
				opts.metadataUserID = FormatMetadataUserID(
					userID,
					strings.TrimSpace(input.AccountUUID),
					generateSessionUUID(seed),
					ExtractCLIVersion(input.Fingerprint.UserAgent),
				)
			}
			body, model = normalizeClaudeOAuthRequestBody(body, model, opts)
			if toolRewrite = buildToolNameRewriteFromBody(body); toolRewrite != nil {
				body = applyToolNameRewriteToBody(body, toolRewrite)
			} else {
				body = applyToolsLastCacheBreakpoint(body)
			}
		}
		if input.OAuth {
			if next, _, changed := anthropicfp.NormalizeDateline(body); changed {
				body = next
			}
		}
		body = enforceCacheControlLimit(body)
	}

	if !input.OAuth && strings.TrimSpace(input.MappedModel) != "" && input.MappedModel != model {
		body = ReplaceModelInBody(body, input.MappedModel)
		model = input.MappedModel
	} else if input.OAuth {
		mapped := claude.NormalizeModelID(model)
		if mapped != model {
			body = ReplaceModelInBody(body, mapped)
			model = mapped
		}
	}
	if !input.CountTokens {
		body = StripEmptyTextBlocks(body)
		body = FilterWebSearchHistoryBlocks(body, model)
		body = FilterThinkingBlocks(body, model)
	}

	logicalBody := append([]byte(nil), body...)
	body, headers, err := finalizeCCMaxCompatibilityWire(input, logicalBody, model, mimic)
	if err != nil {
		return nil, err
	}

	return &CCMaxCompatibilityPrepared{
		Body: body, LogicalBody: logicalBody, Headers: headers,
		OriginalModel: originalModel, Model: model,
		Mimic: mimic, ClaudeCode: claudeCode, ToolRewrite: toolRewrite, input: input,
	}, nil
}

func finalizeCCMaxCompatibilityWire(input CCMaxCompatibilityInput, logicalBody []byte, model string, mimic bool) ([]byte, http.Header, error) {
	body := stripDeferredToolCacheControl(logicalBody)
	if input.OAuth && input.Fingerprint != nil {
		identity := &IdentityService{}
		if rewritten, err := identity.RewriteUserID(body, input.AccountID, strings.TrimSpace(input.AccountUUID), input.Fingerprint.ClientID, input.Fingerprint.UserAgent); err == nil {
			body = rewritten
		}
		body = syncBillingHeaderVersion(body, input.Fingerprint.UserAgent)
	}

	gateway := &GatewayService{}
	tokenType := "apikey"
	if input.OAuth {
		tokenType = "oauth"
	}
	var beta string
	var setBeta bool
	if input.CountTokens {
		beta, setBeta = gateway.computeFinalCountTokensAnthropicBeta(tokenType, mimic, model, input.ClientHeaders, body, nil)
	} else {
		beta, setBeta = gateway.computeFinalAnthropicBeta(tokenType, mimic, model, input.ClientHeaders, body, nil)
	}
	if sanitized, changed := sanitizeAnthropicBodyForBetaTokens(body, beta); changed {
		body = sanitized
	}
	if input.CountTokens {
		body = sanitizeCountTokensRequestBody(body)
	}

	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	if input.OAuth {
		setHeaderRaw(req.Header, "authorization", "Bearer "+input.AccessToken)
	} else {
		setHeaderRaw(req.Header, "x-api-key", input.APIKey)
	}
	if !input.OAuth || !mimic || input.CountTokens {
		for key, values := range input.ClientHeaders {
			if !allowedHeaders[strings.ToLower(key)] {
				continue
			}
			wireKey := resolveWireCasing(key)
			for _, value := range values {
				addHeaderRaw(req.Header, wireKey, value)
			}
		}
	}
	if input.OAuth && input.Fingerprint != nil {
		(&IdentityService{}).ApplyFingerprint(req, input.Fingerprint)
	}
	if getHeaderRaw(req.Header, "content-type") == "" {
		setHeaderRaw(req.Header, "content-type", "application/json")
	}
	if getHeaderRaw(req.Header, "anthropic-version") == "" {
		setHeaderRaw(req.Header, "anthropic-version", "2023-06-01")
	}
	if input.OAuth {
		applyClaudeOAuthHeaderDefaults(req)
		if mimic {
			applyClaudeCodeMimicHeaders(req, input.Stream && !input.CountTokens)
		}
	}
	deleteHeaderAllForms(req.Header, "anthropic-beta")
	if setBeta {
		setHeaderRaw(req.Header, "anthropic-beta", beta)
	}
	if getHeaderRaw(req.Header, "X-Claude-Code-Session-Id") != "" {
		if parsed := ParseMetadataUserID(gjson.GetBytes(body, "metadata.user_id").String()); parsed != nil {
			setHeaderRaw(req.Header, "X-Claude-Code-Session-Id", parsed.SessionID)
		}
	}

	return body, req.Header, nil
}

func IsCCMaxCompatibilitySignatureError(body []byte, model string) bool {
	if !ShouldRectifyThinkingSignatureError(model) {
		return false
	}
	return (&GatewayService{}).isThinkingBlockSignatureError(body)
}

func IsCCMaxCompatibilityToolSignatureError(body []byte) bool {
	message := strings.ToLower(extractUpstreamErrorMessage(body))
	for _, marker := range []string{"tool_use", "tool_result", "functioncall", "function_call", "functionresponse", "function_response"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func IsCCMaxCompatibilityBudgetError(body []byte) bool {
	return isThinkingBudgetConstraintError(extractUpstreamErrorMessage(body))
}

// PrepareCCMaxCompatibilityRetry runs the same default request rectifiers used
// by Sub2API after an Anthropic 400 response, then rebuilds the exact wire body
// and headers from the retained logical request.
func PrepareCCMaxCompatibilityRetry(prepared *CCMaxCompatibilityPrepared, mode CCMaxCompatibilityRetryMode) (*CCMaxCompatibilityPrepared, bool, error) {
	if prepared == nil {
		return nil, false, fmt.Errorf("missing compatibility request")
	}
	logicalBody := prepared.LogicalBody
	var applied bool
	switch mode {
	case CCMaxCompatibilityRetryThinking:
		next := FilterThinkingBlocksForRetry(logicalBody, prepared.Model)
		applied = !bytes.Equal(next, logicalBody)
		logicalBody = next
	case CCMaxCompatibilityRetryThinkingTools:
		next := FilterSignatureSensitiveBlocksForRetry(logicalBody, prepared.Model)
		applied = !bytes.Equal(next, logicalBody)
		logicalBody = next
	case CCMaxCompatibilityRetryBudget:
		logicalBody, applied = RectifyThinkingBudget(logicalBody)
	default:
		return nil, false, fmt.Errorf("unsupported compatibility retry mode %q", mode)
	}
	if !applied {
		return prepared, false, nil
	}
	body, headers, err := finalizeCCMaxCompatibilityWire(prepared.input, logicalBody, prepared.Model, prepared.Mimic)
	if err != nil {
		return nil, false, err
	}
	next := *prepared
	next.Body = body
	next.LogicalBody = logicalBody
	next.Headers = headers
	return &next, true, nil
}

// RestoreCCMaxCompatibilityResponse applies the response-side inverse mapping
// from the same Sub2API tool/model pipeline. It accepts either a JSON body or a
// single SSE data payload.
func RestoreCCMaxCompatibilityResponse(body []byte, prepared *CCMaxCompatibilityPrepared) []byte {
	if prepared == nil {
		return body
	}
	body = restoreToolNamesInBytes(body, prepared.ToolRewrite)
	if prepared.OriginalModel == prepared.Model {
		return body
	}
	for _, path := range []string{"model", "message.model"} {
		if value := gjson.GetBytes(body, path); value.Exists() && value.String() == prepared.Model {
			if next, err := sjson.SetBytes(body, path, prepared.OriginalModel); err == nil {
				body = next
			}
		}
	}
	return body
}

// GenerateCCMaxCompatibilitySessionHash exposes the original three-level
// sticky-session hash (metadata session, cacheable content, request digest).
func GenerateCCMaxCompatibilitySessionHash(body []byte, clientIP, userAgent string, apiKeyID int64) string {
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	if err != nil || parsed == nil {
		return ""
	}
	parsed.SessionContext = &SessionContext{ClientIP: clientIP, UserAgent: userAgent, APIKeyID: apiKeyID}
	return (&GatewayService{}).GenerateSessionHash(parsed)
}

// ResolveCCMaxCompatibilityCooldown exposes the default Anthropic account
// cooldown decisions used by RateLimitService. The caller owns persistence.
func ResolveCCMaxCompatibilityCooldown(status int, headers http.Header) (time.Time, bool) {
	switch status {
	case http.StatusTooManyRequests:
		if result := calculateAnthropic429ResetTime(headers); result != nil {
			return result.resetAt, true
		}
		if raw := strings.TrimSpace(headers.Get("anthropic-ratelimit-unified-reset")); raw != "" {
			if timestamp, err := strconv.ParseInt(raw, 10, 64); err == nil {
				return time.Unix(timestamp, 0), true
			}
		}
		return time.Now().Add(defaultRateLimit429CooldownSeconds * time.Second), true
	case 529:
		return time.Now().Add(10 * time.Minute), true
	default:
		return time.Time{}, false
	}
}

// IsCCMaxCompatibilityRPMSchedulable delegates the three-zone RPM decision to
// Account.CheckRPMSchedulability, including its concurrency + max_sessions
// fallback buffer. Anthropic API-key accounts are not covered by this limiter.
func IsCCMaxCompatibilityRPMSchedulable(authType string, baseRPM, concurrency, stickyBuffer, maxSessions, currentRPM int, strategy string, sticky bool) bool {
	authType = normalizeCCMaxCompatibilityAuthType(authType)
	if authType != AccountTypeOAuth && authType != AccountTypeSetupToken {
		return true
	}
	extra := map[string]any{
		"base_rpm":          baseRPM,
		"rpm_strategy":      strategy,
		"rpm_sticky_buffer": stickyBuffer,
		"max_sessions":      maxSessions,
	}
	account := &Account{Platform: PlatformAnthropic, Type: authType, Concurrency: concurrency, Extra: extra}
	switch account.CheckRPMSchedulability(currentRPM) {
	case WindowCostSchedulable:
		return true
	case WindowCostStickyOnly:
		return sticky
	default:
		return false
	}
}

// ShouldRetryCCMaxCompatibilityStatus delegates the same-account retry policy
// to GatewayService. OAuth retries only 403; API-key accounts retry status
// codes excluded by their enabled custom-error-code policy.
func ShouldRetryCCMaxCompatibilityStatus(authType string, credentials map[string]any, status int) bool {
	account := &Account{Type: normalizeCCMaxCompatibilityAuthType(authType), Credentials: credentials}
	return (&GatewayService{}).shouldRetryUpstreamError(account, status)
}

// ResolveCCMaxCompatibilityAccountPolicy exposes the API-key pool behavior
// applied by Sub2API's handler after GatewayService.Forward returns a failover
// error. Pool mode retries the same account and, unless custom error handling
// is enabled, does not apply the normal account status side effects.
func ResolveCCMaxCompatibilityAccountPolicy(authType string, credentials map[string]any, status int) CCMaxCompatibilityAccountPolicy {
	account := &Account{Type: normalizeCCMaxCompatibilityAuthType(authType), Credentials: credentials}
	poolMode := account.IsPoolMode()
	return CCMaxCompatibilityAccountPolicy{
		PoolMode:                 poolMode,
		PoolRetryable:            poolMode && account.IsPoolModeRetryableStatus(status),
		PoolRetryCount:           account.GetPoolModeRetryCount(),
		SkipDefaultErrorHandling: poolMode && !account.IsCustomErrorCodesEnabled(),
	}
}

func normalizeCCMaxCompatibilityAuthType(authType string) string {
	switch authType {
	case "setup_token":
		return AccountTypeSetupToken
	case "api_key":
		return AccountTypeAPIKey
	}
	return authType
}
