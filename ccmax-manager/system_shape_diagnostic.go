package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const systemShapeDiagnosticArm = "capture-system-shape-once"

type systemShapeDiagnostic struct {
	CapturedAt    string                       `json:"captured_at"`
	Endpoint      string                       `json:"endpoint"`
	RequestID     string                       `json:"upstream_request_id,omitempty"`
	AccountID     int64                        `json:"account_id"`
	GroupID       string                       `json:"group_id"`
	APIKeyID      int64                        `json:"api_key_id"`
	Model         string                       `json:"model"`
	System        systemShapeDiagnosticSummary `json:"system"`
	UpstreamError string                       `json:"upstream_error"`
	PrivacyNotice string                       `json:"privacy_notice"`
}

type systemShapeDiagnosticSummary struct {
	Shape  string                       `json:"shape"`
	Blocks []systemShapeDiagnosticBlock `json:"blocks,omitempty"`
}

type systemShapeDiagnosticBlock struct {
	Index        int                                `json:"index"`
	Shape        string                             `json:"shape"`
	Keys         []string                           `json:"keys,omitempty"`
	ValueTypes   map[string]string                  `json:"value_types,omitempty"`
	Type         string                             `json:"type,omitempty"`
	TextLength   int                                `json:"text_length,omitempty"`
	CacheControl *systemShapeDiagnosticCacheControl `json:"cache_control,omitempty"`
}

type systemShapeDiagnosticCacheControl struct {
	Shape      string            `json:"shape"`
	Keys       []string          `json:"keys,omitempty"`
	ValueTypes map[string]string `json:"value_types,omitempty"`
	Type       string            `json:"type,omitempty"`
	TTL        string            `json:"ttl,omitempty"`
}

func gatewayRequestDiagnosticArmed() bool {
	return extraUsageIdentityDiagnosticArmed() || systemShapeDiagnosticArmed()
}

func systemShapeDiagnosticArmed() bool {
	_, err := os.Stat(filepath.Join(gatewayDiagnosticDir(), systemShapeDiagnosticArm))
	return err == nil
}

func gatewayDiagnosticDir() string {
	dir := strings.TrimSpace(os.Getenv(extraUsageDiagnosticDirEnv))
	if dir == "" {
		dir = "/var/lib/ccmax-manager/diagnostics"
	}
	return dir
}

func (a *app) captureGatewayRequestDiagnosticsOnce(r *http.Request, key gatewayKey, account gatewayAccount, prepared claudePreparedRequest, responseHeader http.Header, failureBody []byte) {
	a.captureExtraUsageIdentityDiagnosticOnce(r, key, account, prepared, responseHeader, failureBody)
	a.captureSystemShapeDiagnosticOnce(r, key, account, prepared, responseHeader, failureBody)
}

func (a *app) captureSystemShapeDiagnosticOnce(r *http.Request, key gatewayKey, account gatewayAccount, prepared claudePreparedRequest, responseHeader http.Header, failureBody []byte) {
	if !upstreamSystemShapeRejected(failureBody) {
		return
	}
	dir := gatewayDiagnosticDir()
	armPath := filepath.Join(dir, systemShapeDiagnosticArm)
	claimPath := fmt.Sprintf("%s.claim-%d-%d", armPath, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(armPath, claimPath); err != nil {
		return
	}
	defer os.Remove(claimPath)

	report := systemShapeDiagnostic{
		CapturedAt:    time.Now().UTC().Format(time.RFC3339Nano),
		Endpoint:      r.URL.Path,
		RequestID:     responseHeader.Get("request-id"),
		AccountID:     account.ID,
		GroupID:       key.GroupID,
		APIKeyID:      key.ID,
		Model:         prepared.Model,
		System:        summarizeSystemShape(prepared.Body),
		UpstreamError: upstreamErrorMessage(failureBody),
		PrivacyNotice: "No prompt text, message text, credentials, raw metadata, account UUID, or proxy URL is stored.",
	}
	payload, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		log.Printf("system shape diagnostic marshal failed: %v", err)
		return
	}
	name := "system-shape-" + time.Now().UTC().Format("20060102T150405.000000000Z") + ".json"
	target := filepath.Join(dir, name)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, append(payload, '\n'), 0o600); err != nil {
		log.Printf("system shape diagnostic write failed: %v", err)
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		log.Printf("system shape diagnostic publish failed: %v", err)
		return
	}
	log.Printf("system shape diagnostic captured: %s", target)
}

func upstreamSystemShapeRejected(body []byte) bool {
	message := strings.ToLower(upstreamErrorMessage(body))
	return strings.Contains(message, "system.") && strings.Contains(message, "input does not match the expected shape")
}

func summarizeSystemShape(body []byte) systemShapeDiagnosticSummary {
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if decoder.Decode(&payload) != nil {
		return systemShapeDiagnosticSummary{Shape: "invalid_json"}
	}
	value, exists := payload["system"]
	if !exists {
		return systemShapeDiagnosticSummary{Shape: "missing"}
	}
	result := systemShapeDiagnosticSummary{Shape: jsonDiagnosticKind(value)}
	blocks, ok := value.([]any)
	if !ok {
		return result
	}
	for index, value := range blocks {
		block := systemShapeDiagnosticBlock{Index: index, Shape: jsonDiagnosticKind(value)}
		object, ok := value.(map[string]any)
		if !ok {
			result.Blocks = append(result.Blocks, block)
			continue
		}
		block.Keys, block.ValueTypes = diagnosticObjectShape(object)
		block.Type, _ = object["type"].(string)
		if text, ok := object["text"].(string); ok {
			block.TextLength = len(text)
		}
		if cacheControl, exists := object["cache_control"]; exists {
			summary := systemShapeDiagnosticCacheControl{Shape: jsonDiagnosticKind(cacheControl)}
			if cacheObject, ok := cacheControl.(map[string]any); ok {
				summary.Keys, summary.ValueTypes = diagnosticObjectShape(cacheObject)
				summary.Type, _ = cacheObject["type"].(string)
				summary.TTL, _ = cacheObject["ttl"].(string)
			}
			block.CacheControl = &summary
		}
		result.Blocks = append(result.Blocks, block)
	}
	return result
}

func diagnosticObjectShape(object map[string]any) ([]string, map[string]string) {
	keys := make([]string, 0, len(object))
	types := make(map[string]string, len(object))
	for key, value := range object {
		keys = append(keys, key)
		types[key] = jsonDiagnosticKind(value)
	}
	sort.Strings(keys)
	return keys, types
}

func jsonDiagnosticKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number, float64:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", value)
	}
}
