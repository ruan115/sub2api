package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	sub2claude "github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	sub2service "github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	extraUsageDiagnosticDirEnv = "CCMAX_DIAGNOSTIC_DIR"
	extraUsageDiagnosticArm    = "capture-extra-usage-once"
)

var (
	billingVersionPattern    = regexp.MustCompile(`(?:^|[;\s])cc_version=([0-9]+\.[0-9]+\.[0-9]+)(?:\.([^;\s]+))?`)
	billingEntrypointPattern = regexp.MustCompile(`(?:^|[;\s])cc_entrypoint=([^;\s]+)`)
)

type extraUsageIdentityDiagnostic struct {
	CapturedAt    string                          `json:"captured_at"`
	Endpoint      string                          `json:"endpoint"`
	RequestID     string                          `json:"upstream_request_id,omitempty"`
	AccountID     int64                           `json:"account_id"`
	GroupID       string                          `json:"group_id"`
	APIKeyID      int64                           `json:"api_key_id"`
	Model         string                          `json:"model"`
	Mode          extraUsageDiagnosticMode        `json:"mode"`
	Headers       extraUsageDiagnosticHeaders     `json:"headers"`
	Body          extraUsageDiagnosticBody        `json:"body"`
	Consistency   extraUsageDiagnosticConsistency `json:"consistency"`
	UpstreamError string                          `json:"upstream_error"`
	PrivacyNotice string                          `json:"privacy_notice"`
}

type extraUsageDiagnosticMode struct {
	OAuth       bool `json:"oauth"`
	Mimic       bool `json:"mimic"`
	ClaudeCode  bool `json:"claude_code"`
	Passthrough bool `json:"passthrough"`
	CountTokens bool `json:"count_tokens"`
	Stream      bool `json:"stream"`
}

type extraUsageDiagnosticHeaders struct {
	UserAgent               string   `json:"user_agent"`
	AnthropicVersion        string   `json:"anthropic_version"`
	AnthropicBetas          []string `json:"anthropic_betas"`
	StainlessLang           string   `json:"x_stainless_lang"`
	StainlessRuntime        string   `json:"x_stainless_runtime"`
	StainlessRuntimeVer     string   `json:"x_stainless_runtime_version"`
	StainlessPackageVer     string   `json:"x_stainless_package_version"`
	StainlessOS             string   `json:"x_stainless_os"`
	StainlessArch           string   `json:"x_stainless_arch"`
	AuthorizationScheme     string   `json:"authorization_scheme,omitempty"`
	APIKeyCredentialPresent bool     `json:"api_key_credential_present"`
}

type extraUsageDiagnosticBody struct {
	TopLevelKeys      []string                     `json:"top_level_keys"`
	System            extraUsageDiagnosticSystem   `json:"system"`
	Metadata          extraUsageDiagnosticMetadata `json:"metadata"`
	MessageCount      int                          `json:"message_count"`
	MessageRoles      []string                     `json:"message_roles"`
	MessageBlockTypes map[string]int               `json:"message_block_types"`
	ToolCount         int                          `json:"tool_count"`
	ToolNameHashes    []string                     `json:"tool_name_hashes,omitempty"`
	ThinkingType      string                       `json:"thinking_type,omitempty"`
	MaxTokensPresent  bool                         `json:"max_tokens_present"`
}

type extraUsageDiagnosticSystem struct {
	Shape                 string         `json:"shape"`
	BlockCount            int            `json:"block_count"`
	BlockTypes            map[string]int `json:"block_types"`
	BillingBlockPresent   bool           `json:"billing_block_present"`
	BillingBlockIndex     int            `json:"billing_block_index"`
	BillingCLIVersion     string         `json:"billing_cli_version,omitempty"`
	BillingFingerprintSet bool           `json:"billing_fingerprint_present"`
	BillingEntrypoint     string         `json:"billing_entrypoint,omitempty"`
	ClaudeCodeIdentity    bool           `json:"claude_code_identity_present"`
	TextBlockLengths      []int          `json:"text_block_lengths"`
	CacheControlTTLs      []string       `json:"cache_control_ttls,omitempty"`
}

type extraUsageDiagnosticMetadata struct {
	Present          bool   `json:"present"`
	Valid            bool   `json:"valid"`
	Format           string `json:"format,omitempty"`
	DeviceIDLength   int    `json:"device_id_length,omitempty"`
	SessionIDPresent bool   `json:"session_id_present"`
	AccountUUIDSet   bool   `json:"account_uuid_present"`
	AccountUUIDMatch bool   `json:"account_uuid_matches_credential"`
}

type extraUsageDiagnosticConsistency struct {
	RequiredBetas         []string `json:"required_betas"`
	MissingBetas          []string `json:"missing_betas"`
	UserAgentCLIVersion   string   `json:"user_agent_cli_version,omitempty"`
	BillingCLIVersion     string   `json:"billing_cli_version,omitempty"`
	VersionsMatch         bool     `json:"ua_billing_versions_match"`
	CurrentVersionMatch   bool     `json:"ua_matches_gateway_cli_version"`
	BillingFirst          bool     `json:"billing_is_first_system_block"`
	BillingEntrypointCLI  bool     `json:"billing_entrypoint_is_cli"`
	MetadataValid         bool     `json:"metadata_valid"`
	MetadataAccountMatch  bool     `json:"metadata_account_matches_credential"`
	CompleteFirstPartySet bool     `json:"complete_first_party_identity_set"`
	MissingOrMismatched   []string `json:"missing_or_mismatched"`
}

func (a *app) captureExtraUsageIdentityDiagnosticOnce(r *http.Request, key gatewayKey, account gatewayAccount, prepared claudePreparedRequest, responseHeader http.Header, failureBody []byte) {
	if !accountExtraUsageRejected(upstreamErrorMessage(failureBody)) {
		return
	}
	dir := strings.TrimSpace(os.Getenv(extraUsageDiagnosticDirEnv))
	if dir == "" {
		dir = "/var/lib/ccmax-manager/diagnostics"
	}
	armPath := filepath.Join(dir, extraUsageDiagnosticArm)
	claimPath := fmt.Sprintf("%s.claim-%d-%d", armPath, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(armPath, claimPath); err != nil {
		return
	}
	defer os.Remove(claimPath)

	report := buildExtraUsageIdentityDiagnostic(r, key, account, prepared, responseHeader, failureBody)
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Printf("extra usage diagnostic marshal failed: %v", err)
		return
	}
	name := "extra-usage-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".json"
	target := filepath.Join(dir, name)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o600); err != nil {
		log.Printf("extra usage diagnostic write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		log.Printf("extra usage diagnostic publish failed: %v", err)
		return
	}
	log.Printf("extra usage identity diagnostic captured: %s", target)
}

func extraUsageIdentityDiagnosticArmed() bool {
	dir := strings.TrimSpace(os.Getenv(extraUsageDiagnosticDirEnv))
	if dir == "" {
		dir = "/var/lib/ccmax-manager/diagnostics"
	}
	_, err := os.Stat(filepath.Join(dir, extraUsageDiagnosticArm))
	return err == nil
}

func buildExtraUsageIdentityDiagnostic(r *http.Request, key gatewayKey, account gatewayAccount, prepared claudePreparedRequest, responseHeader http.Header, failureBody []byte) extraUsageIdentityDiagnostic {
	headers := make(http.Header)
	_ = buildClaudeHeaders(headers, r.Header, prepared, account.CredentialsJSON)
	body := summarizeExtraUsageRequestBody(prepared.Body, account)
	requiredBetas := append([]string(nil), sub2claude.FullClaudeCodeMimicryBetas()...)
	if prepared.CountTokens {
		requiredBetas = append(requiredBetas, sub2claude.BetaTokenCounting)
	}
	actualBetas := splitHeaderTokens(headers.Values("anthropic-beta"))
	missingBetas := missingHeaderTokens(requiredBetas, actualBetas)
	uaVersion := sub2service.ExtractCLIVersion(headers.Get("User-Agent"))
	consistency := extraUsageDiagnosticConsistency{
		RequiredBetas:        requiredBetas,
		MissingBetas:         missingBetas,
		UserAgentCLIVersion:  uaVersion,
		BillingCLIVersion:    body.System.BillingCLIVersion,
		VersionsMatch:        uaVersion != "" && uaVersion == body.System.BillingCLIVersion,
		CurrentVersionMatch:  uaVersion == sub2claude.CLICurrentVersion,
		BillingFirst:         body.System.BillingBlockPresent && body.System.BillingBlockIndex == 0,
		BillingEntrypointCLI: body.System.BillingEntrypoint == "cli",
		MetadataValid:        body.Metadata.Valid,
		MetadataAccountMatch: body.Metadata.AccountUUIDMatch,
	}
	consistency.MissingOrMismatched = extraUsageIdentityProblems(headers, body, consistency)
	consistency.CompleteFirstPartySet = len(consistency.MissingOrMismatched) == 0
	return extraUsageIdentityDiagnostic{
		CapturedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Endpoint:   r.URL.Path,
		RequestID:  responseHeader.Get("request-id"),
		AccountID:  account.ID,
		GroupID:    key.GroupID,
		APIKeyID:   key.ID,
		Model:      prepared.Model,
		Mode: extraUsageDiagnosticMode{
			OAuth: prepared.OAuth, Mimic: prepared.Mimic, ClaudeCode: prepared.ClaudeCode,
			Passthrough: prepared.Passthrough, CountTokens: prepared.CountTokens, Stream: prepared.Stream,
		},
		Headers: extraUsageDiagnosticHeaders{
			UserAgent: headers.Get("User-Agent"), AnthropicVersion: headers.Get("anthropic-version"),
			AnthropicBetas: actualBetas, StainlessLang: headers.Get("X-Stainless-Lang"),
			StainlessRuntime: headers.Get("X-Stainless-Runtime"), StainlessRuntimeVer: headers.Get("X-Stainless-Runtime-Version"),
			StainlessPackageVer: headers.Get("X-Stainless-Package-Version"), StainlessOS: headers.Get("X-Stainless-OS"),
			StainlessArch: headers.Get("X-Stainless-Arch"), AuthorizationScheme: authorizationScheme(headers.Get("Authorization")),
			APIKeyCredentialPresent: strings.TrimSpace(headers.Get("x-api-key")) != "",
		},
		Body:          body,
		Consistency:   consistency,
		UpstreamError: upstreamErrorMessage(failureBody),
		PrivacyNotice: "No prompt text, message text, credentials, raw metadata.user_id, account UUID, or proxy URL is stored.",
	}
}

func summarizeExtraUsageRequestBody(raw []byte, account gatewayAccount) extraUsageDiagnosticBody {
	result := extraUsageDiagnosticBody{MessageBlockTypes: map[string]int{}}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		return result
	}
	for key := range payload {
		result.TopLevelKeys = append(result.TopLevelKeys, key)
	}
	sort.Strings(result.TopLevelKeys)
	result.System = summarizeExtraUsageSystem(payload["system"])
	result.Metadata = summarizeExtraUsageMetadata(payload["metadata"], account)
	_, result.MaxTokensPresent = payload["max_tokens"]
	if thinking, ok := payload["thinking"].(map[string]any); ok {
		result.ThinkingType, _ = thinking["type"].(string)
	}
	if messages, ok := payload["messages"].([]any); ok {
		result.MessageCount = len(messages)
		for _, value := range messages {
			message, _ := value.(map[string]any)
			role, _ := message["role"].(string)
			result.MessageRoles = append(result.MessageRoles, role)
			countContentBlockTypes(message["content"], result.MessageBlockTypes)
		}
	}
	if tools, ok := payload["tools"].([]any); ok {
		result.ToolCount = len(tools)
		for _, value := range tools {
			tool, _ := value.(map[string]any)
			name, _ := tool["name"].(string)
			if name != "" {
				result.ToolNameHashes = append(result.ToolNameHashes, diagnosticHash(name))
			}
		}
	}
	return result
}

func summarizeExtraUsageSystem(value any) extraUsageDiagnosticSystem {
	result := extraUsageDiagnosticSystem{BillingBlockIndex: -1, BlockTypes: map[string]int{}}
	var blocks []any
	switch typed := value.(type) {
	case string:
		result.Shape = "string"
		blocks = []any{typed}
	case []any:
		result.Shape = "array"
		blocks = typed
	case nil:
		result.Shape = "missing"
	default:
		result.Shape = fmt.Sprintf("%T", value)
	}
	result.BlockCount = len(blocks)
	for index, raw := range blocks {
		blockType := "text"
		text := ""
		var cacheControl map[string]any
		switch block := raw.(type) {
		case string:
			text = block
		case map[string]any:
			blockType, _ = block["type"].(string)
			if blockType == "" {
				blockType = "unknown"
			}
			text, _ = block["text"].(string)
			cacheControl, _ = block["cache_control"].(map[string]any)
		default:
			blockType = "unknown"
		}
		result.BlockTypes[blockType]++
		if blockType == "text" {
			result.TextBlockLengths = append(result.TextBlockLengths, len(text))
		}
		trimmed := strings.TrimSpace(text)
		if !result.BillingBlockPresent && strings.HasPrefix(trimmed, "x-anthropic-billing-header:") {
			result.BillingBlockPresent = true
			result.BillingBlockIndex = index
			if match := billingVersionPattern.FindStringSubmatch(trimmed); len(match) > 1 {
				result.BillingCLIVersion = match[1]
				result.BillingFingerprintSet = len(match) > 2 && strings.TrimSpace(match[2]) != ""
			}
			if match := billingEntrypointPattern.FindStringSubmatch(trimmed); len(match) > 1 {
				result.BillingEntrypoint = match[1]
			}
		}
		if strings.Contains(trimmed, claudeCodeSystemPrompt) {
			result.ClaudeCodeIdentity = true
		}
		if ttl, _ := cacheControl["ttl"].(string); ttl != "" {
			result.CacheControlTTLs = append(result.CacheControlTTLs, ttl)
		}
	}
	return result
}

func summarizeExtraUsageMetadata(value any, account gatewayAccount) extraUsageDiagnosticMetadata {
	result := extraUsageDiagnosticMetadata{}
	metadata, ok := value.(map[string]any)
	if !ok {
		return result
	}
	raw, _ := metadata["user_id"].(string)
	result.Present = strings.TrimSpace(raw) != ""
	parsed := sub2service.ParseMetadataUserID(raw)
	if parsed == nil {
		return result
	}
	result.Valid = true
	result.Format = "legacy"
	if parsed.IsNewFormat {
		result.Format = "json"
	}
	result.DeviceIDLength = len(parsed.DeviceID)
	result.SessionIDPresent = parsed.SessionID != ""
	result.AccountUUIDSet = parsed.AccountUUID != ""
	credentials := decodeObject(account.CredentialsJSON)
	accountUUID, _ := stringObjectValue(credentials, "account_uuid")
	if accountUUID == "" {
		accountUUID, _ = stringObjectValue(decodeObject(account.ExtraJSON), "account_uuid")
	}
	result.AccountUUIDMatch = accountUUID != "" && parsed.AccountUUID == accountUUID
	return result
}

func extraUsageIdentityProblems(headers http.Header, body extraUsageDiagnosticBody, consistency extraUsageDiagnosticConsistency) []string {
	problems := make([]string, 0, 10)
	if consistency.UserAgentCLIVersion == "" {
		problems = append(problems, "user_agent_not_claude_cli")
	}
	if !body.System.BillingBlockPresent {
		problems = append(problems, "billing_system_block_missing")
	} else {
		if !consistency.BillingFirst {
			problems = append(problems, "billing_system_block_not_first")
		}
		if !body.System.BillingFingerprintSet {
			problems = append(problems, "billing_fingerprint_missing")
		}
		if !consistency.BillingEntrypointCLI {
			problems = append(problems, "billing_entrypoint_not_cli")
		}
	}
	if !consistency.VersionsMatch {
		problems = append(problems, "user_agent_billing_version_mismatch")
	}
	if !consistency.CurrentVersionMatch {
		problems = append(problems, "user_agent_gateway_version_mismatch")
	}
	if len(consistency.MissingBetas) > 0 {
		problems = append(problems, "required_anthropic_betas_missing")
	}
	if !consistency.MetadataValid {
		problems = append(problems, "metadata_user_id_invalid_or_missing")
	} else if !consistency.MetadataAccountMatch {
		problems = append(problems, "metadata_account_uuid_mismatch")
	}
	if !body.System.ClaudeCodeIdentity {
		problems = append(problems, "claude_code_identity_system_block_missing")
	}
	if headers.Get("anthropic-version") == "" {
		problems = append(problems, "anthropic_version_missing")
	}
	if headers.Get("X-Stainless-Lang") != "js" || headers.Get("X-Stainless-Runtime") != "node" {
		problems = append(problems, "stainless_node_identity_mismatch")
	}
	if headers.Get("X-Stainless-Runtime-Version") == "" {
		problems = append(problems, "stainless_node_runtime_version_missing")
	}
	return problems
}

func countContentBlockTypes(value any, counts map[string]int) {
	switch content := value.(type) {
	case string:
		counts["text"]++
	case []any:
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			blockType, _ := block["type"].(string)
			if blockType == "" {
				blockType = "unknown"
			}
			counts[blockType]++
		}
	}
}

func splitHeaderTokens(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" && !seen[token] {
				seen[token] = true
				result = append(result, token)
			}
		}
	}
	return result
}

func missingHeaderTokens(required, actual []string) []string {
	seen := make(map[string]bool, len(actual))
	for _, token := range actual {
		seen[token] = true
	}
	var missing []string
	for _, token := range required {
		if !seen[token] {
			missing = append(missing, token)
		}
	}
	return missing
}

func authorizationScheme(value string) string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func diagnosticHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:6])
}
