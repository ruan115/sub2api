package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAccountPoolRoutingAndBilling(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "主池", "rate_multiplier": 2.0, "status": "active",
	}, http.StatusOK, nil)

	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "ccmax-a-01", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "secret-token-a"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 3, "priority": 0,
		"rate_multiplier": 0.5, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
	}, http.StatusCreated, &created)
	if created.Priority != 0 {
		t.Fatalf("priority = %d, want 0", created.Priority)
	}
	if created.Credentials != nil {
		t.Fatal("account create response must not reveal credentials")
	}
	var fingerprintJSON, tlsProfile string
	if err := a.db.QueryRow(`SELECT fingerprint_json, tls_profile FROM account_fingerprints WHERE account_id = ?`, created.ID).Scan(&fingerprintJSON, &tlsProfile); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fingerprintJSON, "ClientID") || tlsProfile != defaultAccountTLSProfile {
		t.Fatalf("account fingerprint = %s, tls_profile = %q", fingerprintJSON, tlsProfile)
	}

	var resolved struct {
		GroupID string  `json:"group_id"`
		Account account `json:"account"`
	}
	putJSON(t, handler, http.MethodPost, "/api/pool/resolve", map[string]any{"purpose_key": "default"}, http.StatusOK, &resolved)
	if resolved.GroupID != "a" || resolved.Account.ID != created.ID {
		t.Fatalf("resolved %+v, want group a account %d", resolved, created.ID)
	}
	if resolved.Account.Credentials["access_token"] != "secret-token-a" {
		t.Fatal("pool resolve must return credentials to the authenticated integration")
	}

	usagePayload := map[string]any{
		"request_id": "req-test-1", "purpose_key": "default", "model": "claude-test",
		"input_tokens": 1_000_000, "output_tokens": 1_000_000,
		"cache_creation_tokens": 0, "cache_read_tokens": 0,
	}
	var usage usageLog
	putJSON(t, handler, http.MethodPost, "/api/usage", usagePayload, http.StatusCreated, &usage)
	assertClose(t, usage.BaseCost, 18)
	assertClose(t, usage.BilledCost, 36)
	assertClose(t, usage.ActualCost, 9)

	var duplicate usageLog
	putJSON(t, handler, http.MethodPost, "/api/usage", usagePayload, http.StatusOK, &duplicate)
	if duplicate.ID != usage.ID {
		t.Fatalf("duplicate usage id = %d, want %d", duplicate.ID, usage.ID)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE request_id = 'req-test-1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("usage count = %d, want 1", count)
	}
}

func TestStartupBackfillsStableAccountFingerprints(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	databasePath := filepath.Join(t.TempDir(), "test.db")
	a, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"legacy-one", "legacy-two"} {
		if _, err := a.db.Exec(`INSERT INTO accounts (name) VALUES (?)`, name); err != nil {
			a.db.Close()
			t.Fatal(err)
		}
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	rows, err := a.db.Query(`SELECT fingerprint_json, tls_profile FROM account_fingerprints ORDER BY account_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	clientIDs := map[string]bool{}
	for rows.Next() {
		var raw, profile string
		if err := rows.Scan(&raw, &profile); err != nil {
			t.Fatal(err)
		}
		var fingerprint struct {
			ClientID string
		}
		if err := json.Unmarshal([]byte(raw), &fingerprint); err != nil {
			t.Fatal(err)
		}
		if fingerprint.ClientID == "" || profile != defaultAccountTLSProfile {
			t.Fatalf("fingerprint = %s, profile = %q", raw, profile)
		}
		clientIDs[fingerprint.ClientID] = true
	}
	if len(clientIDs) != 2 {
		t.Fatalf("unique ClientIDs = %d, want 2", len(clientIDs))
	}
}

func TestAccountAndGroupListsWorkWithSingleDatabaseConnection(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()

	result, err := a.db.Exec(`INSERT INTO accounts (name) VALUES ('single-connection-account')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_groups (account_id, group_id) VALUES (?, 'a')`, accountID); err != nil {
		t.Fatal(err)
	}
	a.db.SetMaxOpenConns(1)
	a.db.SetMaxIdleConns(1)

	groups, err := a.listGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) == 0 {
		t.Fatal("listGroups returned no groups")
	}

	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/accounts", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /api/accounts status = %d, body = %s", response.Code, response.Body.String())
	}
	var accounts []account
	if err := json.Unmarshal(response.Body.Bytes(), &accounts); err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 || len(accounts[0].GroupIDs) != 1 || accounts[0].GroupIDs[0] != "a" {
		t.Fatalf("accounts = %+v", accounts)
	}
}

func TestGroupFieldPassthroughSettingsPersistAndSurviveLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "字段透传", "rate_multiplier": 1, "status": "active",
		"service_tier_passthrough_enabled":   true,
		"inference_geo_passthrough_enabled":  true,
		"speed_passthrough_enabled":          true,
		"anthropic_beta_passthrough_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.ServiceTierPassthrough || !updated.InferenceGeoPassthrough || !updated.SpeedPassthrough || !updated.AnthropicBetaPassthrough {
		t.Fatalf("group passthrough settings were not persisted: %+v", updated)
	}

	// An older cached page omits the new fields. Its unrelated save must not
	// silently disable a configuration that was already enabled.
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if !updated.ServiceTierPassthrough || !updated.InferenceGeoPassthrough || !updated.SpeedPassthrough || !updated.AnthropicBetaPassthrough {
		t.Fatalf("legacy update reset group passthrough settings: %+v", updated)
	}
}

func TestGroupQuotaHeaderMaskingPersistsAndSurvivesLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "屏蔽限额头", "rate_multiplier": 1, "status": "active",
		"quota_header_masking_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.QuotaHeaderMasking {
		t.Fatalf("group quota header masking was not persisted: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if !updated.QuotaHeaderMasking {
		t.Fatalf("legacy update reset group quota header masking: %+v", updated)
	}
}

func TestGroupDatelineNormalizationDefaultsOnAndTogglesOff(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	// Seeded groups default to normalization ON (anti-fingerprint protection).
	var groups []group
	putJSON(t, handler, http.MethodGet, "/api/groups", nil, http.StatusOK, &groups)
	if len(groups) == 0 {
		t.Fatal("no seeded groups")
	}
	for _, g := range groups {
		if !g.DatelineNormalization {
			t.Fatalf("group %s defaulted dateline normalization off: %+v", g.ID, g)
		}
	}

	// Disabling persists, and a legacy save that omits the field keeps it off.
	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "逐 token 兼容测试", "rate_multiplier": 1, "status": "active",
		"dateline_normalization_enabled": false,
	}, http.StatusOK, &updated)
	if updated.DatelineNormalization {
		t.Fatalf("group dateline normalization was not disabled: %+v", updated)
	}
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if updated.DatelineNormalization {
		t.Fatalf("legacy update re-enabled dateline normalization: %+v", updated)
	}
}

func TestGroupOpenCodeScrubPersistsAndSurvivesLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "擦除 OpenCode 指纹", "rate_multiplier": 1, "status": "active",
		"opencode_scrub_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.OpenCodeScrub {
		t.Fatalf("group opencode scrub was not persisted: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if !updated.OpenCodeScrub {
		t.Fatalf("legacy update reset group opencode scrub: %+v", updated)
	}
}

func TestGroupExtraUsageFailoverPersistsAndSurvivesLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "extra usage 换号", "rate_multiplier": 1, "status": "active",
		"extra_usage_failover_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.ExtraUsageFailover {
		t.Fatalf("group extra usage failover was not persisted: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if !updated.ExtraUsageFailover {
		t.Fatalf("legacy update reset group extra usage failover: %+v", updated)
	}
}

func TestGroupCacheCreationDetailPersistsAndSurvivesLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "真实缓存明细", "rate_multiplier": 1, "status": "active",
		"cache_creation_detail_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.CacheCreationDetail {
		t.Fatalf("group cache creation detail was not persisted: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if !updated.CacheCreationDetail {
		t.Fatalf("legacy update reset group cache creation detail: %+v", updated)
	}
}

func TestGroupRateLimitDownweightSettingsPersistAndSurviveLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	// Existing groups keep the behaviour they had before the switch existed.
	var current []group
	putJSON(t, handler, http.MethodGet, "/api/groups", nil, http.StatusOK, &current)
	for _, item := range current {
		if !item.RateLimitDownweightEnabled || item.RateLimitCoolingThreshold != defaultRateLimitCoolingThreshold || item.RateLimitWaitSeconds != defaultRateLimitCooldownSeconds || item.RateLimitSteppedCooldown || item.RateLimitCooldownStep != defaultRateLimitCooldownStepSeconds || item.RateLimitDownweightStepped || item.RateLimitDownweightBase != defaultRateLimitDownweightBaseMinutes || item.RateLimitDownweightStep != defaultRateLimitDownweightStepMinutes {
			t.Fatalf("group %s did not default to the previous always-on behaviour: %+v", item.ID, item)
		}
	}

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "429 降权", "rate_multiplier": 1, "status": "active",
		"rate_limit_downweight_enabled": false, "rate_limit_cooling_threshold": 7,
		"rate_limit_wait_seconds": 90, "rate_limit_stepped_cooldown_enabled": true,
		"rate_limit_cooldown_step_seconds":               15,
		"rate_limit_downweight_stepped_cooldown_enabled": true,
		"rate_limit_downweight_base_minutes":             20, "rate_limit_downweight_step_minutes": 35,
	}, http.StatusOK, &updated)
	if updated.RateLimitDownweightEnabled || updated.RateLimitCoolingThreshold != 7 || updated.RateLimitWaitSeconds != 90 || !updated.RateLimitSteppedCooldown || updated.RateLimitCooldownStep != 15 || !updated.RateLimitDownweightStepped || updated.RateLimitDownweightBase != 20 || updated.RateLimitDownweightStep != 35 {
		t.Fatalf("group 429 downweight settings were not persisted: %+v", updated)
	}

	// A page that predates the fields must not silently reset them.
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if updated.RateLimitDownweightEnabled || updated.RateLimitCoolingThreshold != 7 || updated.RateLimitWaitSeconds != 90 || !updated.RateLimitSteppedCooldown || updated.RateLimitCooldownStep != 15 || !updated.RateLimitDownweightStepped || updated.RateLimitDownweightBase != 20 || updated.RateLimitDownweightStep != 35 {
		t.Fatalf("legacy update reset group 429 downweight settings: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "无效阈值", "rate_multiplier": 1, "status": "active",
		"rate_limit_cooling_threshold": 0,
	}, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "无效阈值", "rate_multiplier": 1, "status": "active",
		"rate_limit_cooling_threshold": 99,
	}, http.StatusBadRequest, nil)
	for _, seconds := range []int{59, 121} {
		putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
			"name": "A 分组", "description": "无效冷却", "rate_multiplier": 1, "status": "active",
			"rate_limit_wait_seconds": seconds,
		}, http.StatusBadRequest, nil)
	}
	for _, seconds := range []int{0, maxRateLimitCooldownStepSeconds + 1} {
		putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
			"name": "A 分组", "description": "无效阶梯", "rate_multiplier": 1, "status": "active",
			"rate_limit_cooldown_step_seconds": seconds,
		}, http.StatusBadRequest, nil)
	}
	for _, field := range []string{"rate_limit_downweight_base_minutes", "rate_limit_downweight_step_minutes"} {
		for _, minutes := range []int{0, maxRateLimitDownweightMinutes + 1} {
			putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
				"name": "A 分组", "description": "无效降峰阶梯", "rate_multiplier": 1, "status": "active",
				field: minutes,
			}, http.StatusBadRequest, nil)
		}
	}
}

func TestGroupFiveHourReleaseStaggerSettingsPersistAndSurviveLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var current []group
	putJSON(t, handler, http.MethodGet, "/api/groups", nil, http.StatusOK, &current)
	for _, item := range current {
		if !item.FiveHourStaggerEnabled || item.FiveHourStaggerMin != defaultFiveHourReleaseStaggerMinMinutes || item.FiveHourStaggerMax != defaultFiveHourReleaseStaggerMaxMinutes {
			t.Fatalf("group %s did not preserve the existing 15-30 minute stagger: %+v", item.ID, item)
		}
	}

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "5h 错峰", "rate_multiplier": 1, "status": "active",
		"five_hour_release_stagger_enabled":     false,
		"five_hour_release_stagger_min_minutes": 2,
		"five_hour_release_stagger_max_minutes": 4,
	}, http.StatusOK, &updated)
	if updated.FiveHourStaggerEnabled || updated.FiveHourStaggerMin != 2 || updated.FiveHourStaggerMax != 4 {
		t.Fatalf("group 5h stagger settings were not persisted: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if updated.FiveHourStaggerEnabled || updated.FiveHourStaggerMin != 2 || updated.FiveHourStaggerMax != 4 {
		t.Fatalf("legacy update reset group 5h stagger settings: %+v", updated)
	}

	for _, payload := range []map[string]any{
		{"five_hour_release_stagger_min_minutes": 5},
		{"five_hour_release_stagger_min_minutes": 5, "five_hour_release_stagger_max_minutes": 4},
		{"five_hour_release_stagger_min_minutes": 0, "five_hour_release_stagger_max_minutes": maxFiveHourReleaseStaggerMinutes + 1},
	} {
		payload["name"] = "A 分组"
		payload["description"] = "无效错峰"
		payload["rate_multiplier"] = 1
		payload["status"] = "active"
		putJSON(t, handler, http.MethodPut, "/api/groups/a", payload, http.StatusBadRequest, nil)
	}
}

func TestGroupRejectAnthropicDowngradePersistsAndSurvivesLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "拒绝降级", "rate_multiplier": 1, "status": "active",
		"reject_anthropic_downgrade_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.RejectAnthropicDowngrade {
		t.Fatalf("group downgrade setting was not persisted: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if !updated.RejectAnthropicDowngrade {
		t.Fatalf("legacy update reset group downgrade setting: %+v", updated)
	}
}

func TestGroupRejectDistillationPersistsAndSurvivesLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "拒绝用户蒸馏", "rate_multiplier": 1, "status": "active",
		"reject_distillation_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.RejectDistillation {
		t.Fatalf("group distillation guard was not persisted: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if !updated.RejectDistillation {
		t.Fatalf("legacy update reset group distillation guard: %+v", updated)
	}
}

func TestGroupRequestFormatFilterPersistsAndSurvivesLegacyUpdate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var updated group
	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "请求格式过滤", "rate_multiplier": 1, "status": "active",
		"request_format_filter_enabled": true,
	}, http.StatusOK, &updated)
	if !updated.RequestFormatFilter {
		t.Fatalf("group request format filter was not persisted: %+v", updated)
	}

	putJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "A 分组", "description": "旧页面保存", "rate_multiplier": 1, "status": "active",
	}, http.StatusOK, &updated)
	if !updated.RequestFormatFilter {
		t.Fatalf("legacy update reset group request format filter: %+v", updated)
	}
}

func TestDynamicGroupCreateRenameAndAssignment(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var created group
	putJSON(t, handler, http.MethodPost, "/api/groups", map[string]any{
		"name": "高峰业务", "description": "动态分组", "rate_multiplier": 1.25, "status": "active",
	}, http.StatusCreated, &created)
	if created.ID == "" || created.ID == "a" || created.ID == "b" {
		t.Fatalf("dynamic group id = %q", created.ID)
	}

	var renamed group
	putJSON(t, handler, http.MethodPut, "/api/groups/"+created.ID, map[string]any{
		"name": "高峰业务二组", "description": "名称已更新", "rate_multiplier": 1.25, "status": "active",
	}, http.StatusOK, &renamed)
	if renamed.ID != created.ID || renamed.Name != "高峰业务二组" {
		t.Fatalf("renamed group = %+v", renamed)
	}

	var purposeItem purpose
	putJSON(t, handler, http.MethodPost, "/api/purposes", map[string]any{
		"key": "peak", "name": "高峰用途", "active_group_id": created.ID,
	}, http.StatusCreated, &purposeItem)
	if purposeItem.ActiveGroupID != created.ID {
		t.Fatalf("purpose group = %q, want %q", purposeItem.ActiveGroupID, created.ID)
	}

	var accountItem account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "dynamic-group-account", "platform": "anthropic", "auth_type": "oauth",
		"status": "active", "schedulable": false, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{created.ID},
	}, http.StatusCreated, &accountItem)
	if len(accountItem.GroupIDs) != 1 || accountItem.GroupIDs[0] != created.ID {
		t.Fatalf("account groups = %v, want %q", accountItem.GroupIDs, created.ID)
	}
}

func TestLegacyABGroupConstraintIsMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE groups (
		id TEXT PRIMARY KEY CHECK (id IN ('a', 'b')),
		name TEXT NOT NULL,
		description TEXT NOT NULL DEFAULT '',
		rate_multiplier REAL NOT NULL DEFAULT 1,
		daily_limit_usd REAL,
		monthly_limit_usd REAL,
		status TEXT NOT NULL DEFAULT 'active',
		created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
		updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
	)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO groups (id, name) VALUES ('a', 'A 分组'), ('b', 'B 分组')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a, err := newApp(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	if _, err := a.db.Exec(`INSERT INTO groups (id, name) VALUES ('c', 'C 分组')`); err != nil {
		t.Fatalf("dynamic group insert after migration: %v", err)
	}
	groups, err := a.listGroups()
	if err != nil {
		t.Fatalf("read groups after legacy migration: %v", err)
	}
	if len(groups) != 3 {
		t.Fatalf("groups after legacy migration = %d, want 3", len(groups))
	}
	for _, item := range groups {
		if item.RateLimitDownweightStepped || item.RateLimitDownweightBase != defaultRateLimitDownweightBaseMinutes || item.RateLimitDownweightStep != defaultRateLimitDownweightStepMinutes {
			t.Fatalf("legacy group %s has invalid downweight defaults: %+v", item.ID, item)
		}
	}
	var invalidReferences int
	rows, err := a.db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		invalidReferences++
	}
	if invalidReferences != 0 {
		t.Fatalf("foreign key violations after migration = %d", invalidReferences)
	}
}

func TestMinuteFilterTimeUsesShanghaiAndInclusiveEndMinute(t *testing.T) {
	if got := normalizeDateStart("2026-08-24T10:52"); got != "2026-08-24T02:52:00Z" {
		t.Fatalf("start = %q", got)
	}
	if got := normalizeDateEnd("2026-08-24T10:52"); got != "2026-08-24T02:53:00Z" {
		t.Fatalf("end = %q", got)
	}
	if got := normalizeDateEnd("2026-08-24"); got != "2026-08-24T16:00:00Z" {
		t.Fatalf("date end = %q", got)
	}
}

func TestPurposeSwitchChangesResolvedPool(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	for _, groupID := range []string{"a", "b"} {
		proxyID := createTestForwardProxy(t, a)
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": "ccmax-" + groupID, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": "token-" + groupID}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 0.0, "group_ids": []string{groupID}, "proxy_pool_id": 1, "proxy_id": proxyID,
		}, http.StatusCreated, nil)
	}

	var purposes []purpose
	putJSON(t, handler, http.MethodGet, "/api/purposes", nil, http.StatusOK, &purposes)
	if len(purposes) != 1 {
		t.Fatalf("purpose count = %d, want 1", len(purposes))
	}
	item := purposes[0]
	putJSON(t, handler, http.MethodPut, "/api/purposes/1", map[string]any{
		"key": item.Key, "name": item.Name, "description": item.Description, "active_group_id": "b",
	}, http.StatusOK, nil)

	var resolved struct {
		GroupID string  `json:"group_id"`
		Account account `json:"account"`
	}
	putJSON(t, handler, http.MethodPost, "/api/pool/resolve", map[string]any{"purpose_key": "default"}, http.StatusOK, &resolved)
	if resolved.GroupID != "b" || resolved.Account.Name != "ccmax-b" {
		t.Fatalf("resolved group=%s account=%s, want b/ccmax-b", resolved.GroupID, resolved.Account.Name)
	}
	if resolved.Account.RateMultiplier != 0 {
		t.Fatalf("zero account multiplier changed to %v", resolved.Account.RateMultiplier)
	}
}

func TestAccountBatchDelete(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	ids := make([]int64, 0, 3)
	for index := 1; index <= 3; index++ {
		var created account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": "batch-delete-" + strconv.Itoa(index), "platform": "anthropic", "auth_type": "oauth",
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
		}, http.StatusCreated, &created)
		ids = append(ids, created.ID)
	}

	var result struct {
		Deleted int64 `json:"deleted"`
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-delete", map[string]any{"ids": []int64{ids[0], ids[1], ids[1]}}, http.StatusOK, &result)
	if result.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2", result.Deleted)
	}
	var active, disabled int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE deleted_at IS NULL`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE deleted_at IS NOT NULL AND schedulable = 0 AND status = 'disabled'`).Scan(&disabled); err != nil {
		t.Fatal(err)
	}
	if active != 1 || disabled != 2 {
		t.Fatalf("active=%d disabled=%d, want 1 and 2", active, disabled)
	}
	var action string
	if err := a.db.QueryRow(`SELECT action FROM audit_logs WHERE path = '/api/accounts/batch-delete' ORDER BY id DESC LIMIT 1`).Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != "account.delete" {
		t.Fatalf("audit action = %q, want account.delete", action)
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-delete", map[string]any{"ids": []int64{}}, http.StatusBadRequest, nil)
}

func TestAccountBatchScheduleOnlyEnablesAuthorizedProxiedAccounts(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	proxyID := createTestForwardProxy(t, a)
	var ready, pending account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "ready@example.com", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "ready-token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1,
		"proxy_id": proxyID, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &ready)
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "pending@example.com", "platform": "anthropic", "auth_type": "oauth",
		"status": "active", "schedulable": false, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &pending)
	futureCooldown := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if _, err := a.db.Exec(`UPDATE accounts SET rate_limit_reset_at = ?, rate_limit_window = '5h', rate_limit_reason = '429_cooling', consecutive_429 = 3, last_429_at = ?, rate_limit_downweight_until = ? WHERE id = ?`, futureCooldown, time.Now().UTC().Format(time.RFC3339Nano), futureCooldown, ready.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO account_rpm_thresholds (account_id, rpm_limit, reset_at) VALUES (?, 1, ?)`, ready.ID, futureCooldown); err != nil {
		t.Fatal(err)
	}

	var paused struct {
		Matched int64 `json:"matched"`
		Updated int64 `json:"updated"`
		Skipped int64 `json:"skipped"`
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-schedule", map[string]any{
		"ids": []int64{ready.ID}, "schedulable": false,
	}, http.StatusOK, &paused)
	if paused.Matched != 1 || paused.Updated != 1 || paused.Skipped != 0 {
		t.Fatalf("pause result = %+v", paused)
	}
	var enabled struct {
		Matched int64 `json:"matched"`
		Updated int64 `json:"updated"`
		Skipped int64 `json:"skipped"`
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-schedule", map[string]any{
		"ids": []int64{ready.ID, pending.ID}, "schedulable": true,
	}, http.StatusOK, &enabled)
	if enabled.Matched != 2 || enabled.Updated != 1 || enabled.Skipped != 1 {
		t.Fatalf("enable result = %+v", enabled)
	}
	var readyScheduled, pendingScheduled, consecutive429 int
	var readyCooldown, last429 sql.NullString
	var rateLimitWindow, rateLimitReason string
	if err := a.db.QueryRow(`SELECT schedulable, rate_limit_reset_at, rate_limit_window, rate_limit_reason, consecutive_429, last_429_at FROM accounts WHERE id = ?`, ready.ID).Scan(&readyScheduled, &readyCooldown, &rateLimitWindow, &rateLimitReason, &consecutive429, &last429); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT schedulable FROM accounts WHERE id = ?`, pending.ID).Scan(&pendingScheduled); err != nil {
		t.Fatal(err)
	}
	if readyScheduled != 1 || pendingScheduled != 0 {
		t.Fatalf("scheduled states = %d/%d", readyScheduled, pendingScheduled)
	}
	if readyCooldown.Valid {
		t.Fatalf("manual resume kept cooldown = %q", readyCooldown.String)
	}
	if rateLimitWindow != "" || rateLimitReason != "" || consecutive429 != 0 || last429.Valid {
		t.Fatalf("manual resume kept 429 state = window %q reason %q consecutive %d last %v", rateLimitWindow, rateLimitReason, consecutive429, last429)
	}
	var thresholdCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM account_rpm_thresholds WHERE account_id = ?`, ready.ID).Scan(&thresholdCount); err != nil {
		t.Fatal(err)
	}
	if thresholdCount != 0 {
		t.Fatalf("manual resume kept %d learned RPM thresholds", thresholdCount)
	}
}

func TestAccountBatchUpdateOnlyChangesSelectedSharedFields(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	created := make([]account, 0, 2)
	for index := 0; index < 2; index++ {
		proxyID := createTestForwardProxy(t, a)
		var item account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": fmt.Sprintf("batch-%d@example.com", index), "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": fmt.Sprintf("token-%d", index)}, "extra": map[string]any{"request_passthrough": true},
			"status": "active", "schedulable": true, "concurrency": index + 1, "priority": 50,
			"rate_multiplier": 1, "account_price": 2, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
			"base_rpm": 15, "rpm_strategy": "tiered", "rpm_sticky_buffer": 0, "user_msg_queue_mode": "off",
		}, http.StatusCreated, &item)
		created = append(created, item)
	}
	originalCredentials := make(map[int64]string, len(created))
	originalProxies := make(map[int64]int64, len(created))
	for _, item := range created {
		var proxyID int64
		var credentials string
		if err := a.db.QueryRow(`SELECT credentials_json, proxy_id FROM accounts WHERE id = ?`, item.ID).Scan(&credentials, &proxyID); err != nil {
			t.Fatal(err)
		}
		originalCredentials[item.ID] = credentials
		originalProxies[item.ID] = proxyID
	}
	var result struct {
		Matched int `json:"matched"`
		Updated int `json:"updated"`
		Skipped int `json:"skipped"`
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{
		"ids":         []int64{created[0].ID, created[1].ID, created[1].ID, 99999},
		"concurrency": 7, "priority": 8, "rate_multiplier": 1.25, "account_price": 4.5,
		"base_rpm": 42, "rpm_strategy": "sticky_exempt", "rpm_sticky_buffer": 3,
		"user_msg_queue_mode": "soft", "group_ids": []string{"b"},
	}, http.StatusOK, &result)
	if result.Matched != 2 || result.Updated != 2 || result.Skipped != 1 {
		t.Fatalf("batch update result = %+v", result)
	}
	for _, before := range created {
		after, err := a.getAccount(before.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if after.Name != before.Name || after.ProxyID == nil || *after.ProxyID != originalProxies[before.ID] {
			t.Fatalf("exclusive fields changed for account %d: name=%q proxy=%v", before.ID, after.Name, after.ProxyID)
		}
		var credentials string
		if err := a.db.QueryRow(`SELECT credentials_json FROM accounts WHERE id = ?`, before.ID).Scan(&credentials); err != nil {
			t.Fatal(err)
		}
		if credentials != originalCredentials[before.ID] {
			t.Fatalf("credentials changed for account %d", before.ID)
		}
		if after.Concurrency != 7 || after.Priority != 8 || after.RateMultiplier != 1.25 || after.AccountPrice != 4.5 || after.BaseRPM != 42 || after.RPMStrategy != "sticky_exempt" || after.RPMStickyBuffer != 3 || after.UserMsgQueueMode != "soft" || strings.Join(after.GroupIDs, ",") != "b" {
			t.Fatalf("shared fields not updated for account %d: %+v", before.ID, after)
		}
		if after.Extra["request_passthrough"] != true || fmt.Sprint(after.Extra["base_rpm"]) != "42" || after.Extra["rpm_strategy"] != "sticky_exempt" {
			t.Fatalf("extra fields were not preserved/synchronized: %+v", after.Extra)
		}
		var groupPriority int
		if err := a.db.QueryRow(`SELECT priority FROM account_groups WHERE account_id = ? AND group_id = 'b'`, before.ID).Scan(&groupPriority); err != nil {
			t.Fatal(err)
		}
		if groupPriority != 8 {
			t.Fatalf("group priority = %d, want 8", groupPriority)
		}
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{"ids": []int64{created[0].ID}}, http.StatusBadRequest, nil)
}

func TestAccountBatchUpdateBindsAndClearsDispatchStrategy(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	var strategy struct {
		ID int64 `json:"id"`
	}
	putJSON(t, handler, http.MethodPost, "/api/strategies", map[string]any{
		"name": "serial-15", "rpm_limit": 15, "rpm_strategy": "fixed", "dispatch_mode": "serial",
	}, http.StatusCreated, &strategy)
	created := make([]account, 0, 2)
	for index := 0; index < 2; index++ {
		proxyID := createTestForwardProxy(t, a)
		var item account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": fmt.Sprintf("bind-%d@example.com", index), "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": fmt.Sprintf("token-%d", index)},
			"status":      "active", "schedulable": true, "concurrency": 5, "priority": 50,
			"rate_multiplier": 1, "account_price": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1, "proxy_id": proxyID,
			"base_rpm": 15, "rpm_strategy": "tiered", "rpm_sticky_buffer": 0, "user_msg_queue_mode": "off",
		}, http.StatusCreated, &item)
		created = append(created, item)
	}
	ids := []int64{created[0].ID, created[1].ID}
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{
		"ids": ids, "strategy_id": strategy.ID,
	}, http.StatusOK, nil)
	for _, item := range created {
		after, err := a.getAccount(item.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if after.StrategyID == nil || *after.StrategyID != strategy.ID {
			t.Fatalf("account %d strategy = %v, want %d", item.ID, after.StrategyID, strategy.ID)
		}
	}
	// A bound strategy must not be deletable until every account is unbound.
	putJSON(t, handler, http.MethodDelete, fmt.Sprintf("/api/strategies/%d", strategy.ID), nil, http.StatusConflict, nil)
	// Omitting strategy_id keeps the binding; only an explicit 0 clears it.
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{
		"ids": ids, "concurrency": 9,
	}, http.StatusOK, nil)
	kept, err := a.getAccount(created[0].ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if kept.StrategyID == nil || *kept.StrategyID != strategy.ID {
		t.Fatalf("strategy binding was lost by an unrelated batch update: %v", kept.StrategyID)
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{
		"ids": ids, "strategy_id": 0,
	}, http.StatusOK, nil)
	for _, item := range created {
		after, err := a.getAccount(item.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if after.StrategyID != nil {
			t.Fatalf("account %d strategy = %v, want unbound", item.ID, *after.StrategyID)
		}
	}
	putJSON(t, handler, http.MethodDelete, fmt.Sprintf("/api/strategies/%d", strategy.ID), nil, http.StatusOK, nil)
	putJSON(t, handler, http.MethodPost, "/api/accounts/batch-update", map[string]any{
		"ids": ids, "strategy_id": strategy.ID,
	}, http.StatusBadRequest, nil)
}

func TestAutomaticProxyAssignmentIsExclusive(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": 1, "text": "socks5://127.0.0.1:12001\nsocks5://127.0.0.1:12002",
	}, http.StatusCreated, nil)

	proxyIDs := map[int64]bool{}
	for _, name := range []string{"ccmax-proxy-1", "ccmax-proxy-2"} {
		var created account
		putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1,
			"auto_proxy": true, "base_rpm": 15, "rpm_strategy": "tiered",
			"rpm_sticky_buffer": 0, "user_msg_queue_mode": "off",
		}, http.StatusCreated, &created)
		if created.ProxyID == nil {
			t.Fatal("automatic proxy assignment returned no proxy")
		}
		if proxyIDs[*created.ProxyID] {
			t.Fatalf("proxy %d was assigned to two automatic accounts", *created.ProxyID)
		}
		proxyIDs[*created.ProxyID] = true
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "ccmax-proxy-overflow", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1,
		"auto_proxy": true, "base_rpm": 15, "rpm_strategy": "tiered",
		"rpm_sticky_buffer": 0, "user_msg_queue_mode": "off",
	}, http.StatusConflict, nil)
}

func TestProxyDeleteReassignsAutomaticAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": 1, "text": "http://127.0.0.1:14001\nhttp://127.0.0.1:14002",
	}, http.StatusCreated, nil)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "auto-delete", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1,
		"auto_proxy": true, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	if created.ProxyID == nil {
		t.Fatal("automatic account has no proxy")
	}
	deletedID := *created.ProxyID
	var result struct {
		Reassigned int `json:"reassigned_accounts"`
		Paused     int `json:"paused_accounts"`
	}
	putJSON(t, handler, http.MethodDelete, "/api/proxies/"+strconv.FormatInt(deletedID, 10), nil, http.StatusOK, &result)
	if result.Reassigned != 1 || result.Paused != 0 {
		t.Fatalf("delete result = %+v", result)
	}
	updated, err := a.getAccount(created.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Schedulable || updated.ProxyID == nil || *updated.ProxyID == deletedID {
		t.Fatalf("automatic account was not moved to another proxy: %+v", updated)
	}
}

func TestProxyDeletePausesManualAccount(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": 1, "text": "http://127.0.0.1:15001",
	}, http.StatusCreated, nil)
	var proxies []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "manual-delete", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1,
		"proxy_id": proxies[0].ID, "auto_proxy": false, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}, http.StatusCreated, &created)
	var result struct {
		Reassigned int `json:"reassigned_accounts"`
		Paused     int `json:"paused_accounts"`
	}
	putJSON(t, handler, http.MethodDelete, "/api/proxies/"+strconv.FormatInt(proxies[0].ID, 10), nil, http.StatusOK, &result)
	if result.Reassigned != 0 || result.Paused != 1 {
		t.Fatalf("delete result = %+v", result)
	}
	updated, err := a.getAccount(created.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Schedulable || updated.ProxyID != nil || !strings.Contains(updated.ErrorMessage, "独享 IP") {
		t.Fatalf("manual account was not safely paused: %+v", updated)
	}
	var archived []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies/archived", nil, http.StatusOK, &archived)
	if len(archived) != 1 || !strings.Contains(archived[0].HistoricalAccounts, created.Name) || archived[0].ArchivedAt == "" {
		t.Fatalf("archived proxy did not retain account history: %+v", archived)
	}
}

func TestProxyBatchDeleteRemovesSelectedProxies(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{
		"pool_id": 1, "text": "http://127.0.0.1:15101\nsocks5://127.0.0.1:15102",
	}, http.StatusCreated, nil)
	var proxies []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	if len(proxies) != 2 {
		t.Fatalf("proxy count = %d, want 2", len(proxies))
	}
	ids := []int64{proxies[0].ID, proxies[1].ID}
	var result proxyBatchDeleteResponse
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch-delete", map[string]any{"ids": ids}, http.StatusOK, &result)
	if result.Matched != 2 || result.Deleted != 2 || result.Archived != 2 || result.ReassignedAccounts != 0 || result.PausedAccounts != 0 {
		t.Fatalf("batch delete result = %+v", result)
	}
	proxies = nil
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	if len(proxies) != 0 {
		t.Fatalf("remaining proxies = %+v", proxies)
	}
	var archived []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies/archived", nil, http.StatusOK, &archived)
	if len(archived) != 2 {
		t.Fatalf("archived proxies = %+v, want 2 records", archived)
	}
	for _, item := range archived {
		if item.ArchivedAt == "" || item.Status != "disabled" {
			t.Fatalf("archive record is incomplete: %+v", item)
		}
	}
}

func TestAccountCreateSupportsDeferredSessionAuthorization(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/organizations":
			cookie, err := r.Cookie("sessionKey")
			if err != nil || cookie.Value != "valid-session" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeJSON(w, http.StatusOK, []map[string]any{{"uuid": "org-1"}})
		case "/v1/oauth/org-1/authorize":
			writeJSON(w, http.StatusOK, map[string]string{
				"redirect_uri": "https://platform.claude.com/oauth/code/callback?code=code-1&state=state-1",
			})
		case "/token":
			writeJSON(w, http.StatusOK, map[string]any{
				"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "bearer",
				"expires_in": 3600, "scope": claudeOAuthAPIScope,
				"organization": map[string]string{"uuid": "org-1"},
				"account":      map[string]string{"uuid": "account-1", "email_address": "user@example.com"},
			})
		case "/profile":
			writeJSON(w, http.StatusOK, map[string]any{
				"account": map[string]bool{"has_claude_max": true},
				"organization": map[string]string{
					"organization_type": "claude_max",
					"rate_limit_tier":   "default_claude_max_20x",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	previousOrganizations := claudeOrganizationsEndpoint
	previousAuthorize := claudeSessionAuthorizeBaseURL
	previousToken := claudeTokenEndpoint
	previousProfile := claudeProfileEndpoint
	claudeOrganizationsEndpoint = server.URL + "/organizations"
	claudeSessionAuthorizeBaseURL = server.URL + "/v1/oauth"
	claudeTokenEndpoint = server.URL + "/token"
	claudeProfileEndpoint = server.URL + "/profile"
	defer func() {
		claudeOrganizationsEndpoint = previousOrganizations
		claudeSessionAuthorizeBaseURL = previousAuthorize
		claudeTokenEndpoint = previousToken
		claudeProfileEndpoint = previousProfile
	}()

	payload := map[string]any{
		"name": "session-create", "platform": "anthropic", "auth_type": "oauth", "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}
	var deferred account
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusCreated, &deferred)
	if deferred.AuthStatus != "reauth_required" || deferred.HasCredentials || deferred.Schedulable || deferred.AuthError != "等待授权" {
		t.Fatalf("deferred account has unsafe state: %+v", deferred)
	}
	if deferred.DispatchStatus != "unavailable" {
		t.Fatalf("deferred account dispatch status = %q, want unavailable", deferred.DispatchStatus)
	}
	payload["name"] = "session-invalid"
	payload["session_key"] = "invalid-session"
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusConflict, nil)
	proxyID := createTestForwardProxy(t, a)
	payload["proxy_pool_id"] = 1
	payload["proxy_id"] = proxyID
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusBadGateway, nil)
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE deleted_at IS NULL`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed authorization left %d accounts, err=%v", count, err)
	}

	payload["name"] = "session-valid"
	payload["session_key"] = "valid-session"
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusCreated, &created)
	if created.AuthStatus != "valid" || !created.HasCredentials || created.TokenExpiresAt == "" || created.SubscriptionType != "max" || created.RateLimitTier != "default_claude_max_20x" {
		t.Fatalf("created account is not fully authorized: %+v", created)
	}
	var credentials string
	if err := a.db.QueryRow(`SELECT credentials_json FROM accounts WHERE id = ?`, created.ID).Scan(&credentials); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(credentials, "access-1") || !strings.Contains(credentials, "refresh-1") {
		t.Fatalf("authorized credentials were not stored: %s", credentialHint(credentials))
	}
}

func TestIndexUsesContentVersionedAssets(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	response := httptest.NewRecorder()
	a.routes().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("index status = %d", response.Code)
	}
	body := response.Body.String()
	if strings.Contains(body, "__ASSET_VERSION__") || !strings.Contains(body, "/styles.css?v=") || !strings.Contains(body, "/app.js?v=") {
		t.Fatalf("index assets are not versioned: %s", body[:min(len(body), 500)])
	}
	if !strings.Contains(response.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("index Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
}

func TestOAuthLinkIsDisplayedAndCopiedWithoutOpeningTab(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	indexResponse := httptest.NewRecorder()
	handler.ServeHTTP(indexResponse, httptest.NewRequest(http.MethodGet, "/", nil))
	index := indexResponse.Body.String()
	if !strings.Contains(index, `id="oauth-link"`) || !strings.Contains(index, `id="copy-oauth-link"`) {
		t.Fatalf("OAuth link display or copy control is missing")
	}
	if strings.Contains(index, `class="oauth-link"`) || strings.Contains(index, `>在新窗口完成授权</a`) {
		t.Fatalf("OAuth link must be displayed as read-only content instead of a navigation link")
	}

	appResponse := httptest.NewRecorder()
	handler.ServeHTTP(appResponse, httptest.NewRequest(http.MethodGet, "/app.js", nil))
	app := appResponse.Body.String()
	if strings.Contains(app, `window.open(result.auth_url`) {
		t.Fatalf("OAuth generation still opens a browser tab")
	}
	if !strings.Contains(app, `copyToClipboard(result.auth_url)`) {
		t.Fatalf("OAuth generation does not copy the generated URL")
	}
}

func TestReadOnlyAdministratorCannotWrite(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")

	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "auditor", "name": "Auditor", "password": "auditor-password",
		"role": "readonly_admin", "status": "active", "allowed_group_ids": []string{"a", "b"}, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, nil)
	readonlyCookie := loginCookie(t, handler, "auditor", "auditor-password")

	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, readonlyCookie, "", http.StatusOK, nil)
	requestJSON(t, handler, http.MethodPut, "/api/groups/a", map[string]any{
		"name": "changed", "description": "", "rate_multiplier": 1, "status": "active",
	}, readonlyCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodPost, "/api/accounts/1/archive", map[string]any{}, readonlyCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodPost, "/api/accounts/batch-archive", map[string]any{"ids": []int64{1}}, readonlyCookie, "", http.StatusForbidden, nil)
	requestJSON(t, handler, http.MethodPost, "/api/pool/resolve", map[string]any{"purpose_key": "default"}, readonlyCookie, "", http.StatusForbidden, nil)
}

func TestOrdinaryUserSeesAccountPoolButCannotMutateAccounts(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")
	var user panelUser
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "pool-viewer", "name": "Pool Viewer", "password": "viewer-password",
		"role": "user", "status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, &user)
	if strings.Join(user.VisiblePages, ",") != "accounts,access" {
		t.Fatalf("ordinary user visible pages = %v", user.VisiblePages)
	}

	userCookie := loginCookie(t, handler, "pool-viewer", "viewer-password")
	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, userCookie, "", http.StatusOK, nil)
	mutations := []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/accounts", map[string]any{}},
		{http.MethodPut, "/api/accounts/1", map[string]any{}},
		{http.MethodDelete, "/api/accounts/1", nil},
		{http.MethodPost, "/api/accounts/batch-delete", map[string]any{"ids": []int64{1}}},
		{http.MethodPost, "/api/accounts/batch-archive", map[string]any{"ids": []int64{1}}},
		{http.MethodPost, "/api/accounts/1/archive", map[string]any{}},
		{http.MethodPost, "/api/accounts/batch-schedule", map[string]any{"ids": []int64{1}, "schedulable": true}},
		{http.MethodPost, "/api/accounts/batch-update", map[string]any{"ids": []int64{1}, "concurrency": 2}},
		{http.MethodPost, "/api/accounts/health/refresh", map[string]any{"ids": []int64{1}}},
		{http.MethodPost, "/api/accounts/1/quota/refresh", map[string]any{}},
		{http.MethodPost, "/api/accounts/1/auth-url", map[string]any{}},
		{http.MethodPost, "/api/accounts/1/oauth-exchange", map[string]any{}},
		{http.MethodPost, "/api/accounts/1/session-auth", map[string]any{}},
		{http.MethodPost, "/api/proxies/batch-test", map[string]any{"pool_id": 1}},
	}
	for _, mutation := range mutations {
		requestJSON(t, handler, mutation.method, mutation.path, mutation.body, userCookie, "", http.StatusForbidden, nil)
	}
	requestJSON(t, handler, http.MethodPost, "/api/auth/logout", nil, userCookie, "", http.StatusNoContent, nil)
	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, userCookie, "", http.StatusUnauthorized, nil)
}

func TestOrdinaryUserAccountPageMigrationRunsOnlyOnce(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "test.db")
	a, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`INSERT INTO users (username, password_hash, role, status, allowed_group_ids_json, visible_pages_json) VALUES ('legacy-user', 'unused', 'user', 'active', '["a"]', '["access"]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`DELETE FROM feature_migrations WHERE name = 'ordinary-user-account-page-v1'`); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var visible string
	if err := a.db.QueryRow(`SELECT visible_pages_json FROM users WHERE username = 'legacy-user'`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != `["accounts","access"]` {
		t.Fatalf("migrated visible pages = %s", visible)
	}
	if _, err := a.db.Exec(`UPDATE users SET visible_pages_json = '["access"]' WHERE username = 'legacy-user'`); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	if err := a.db.QueryRow(`SELECT visible_pages_json FROM users WHERE username = 'legacy-user'`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if visible != `["access"]` {
		t.Fatalf("custom visible pages were overwritten after restart: %s", visible)
	}
}

func TestManualProxyIsImportedAndExclusivelyBoundOnAccountCreate(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	payload := map[string]any{
		"name": "manual-proxy-account", "platform": "anthropic", "auth_type": "oauth",
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
		"rate_multiplier": 1, "group_ids": []string{"a"}, "proxy_pool_id": 1,
		"proxy_text": "http://proxy-user:proxy-password@127.0.0.1:18080",
	}
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusCreated, &created)
	if created.ProxyID == nil || created.ProxyPoolID == nil || *created.ProxyPoolID != 1 {
		t.Fatalf("created account proxy binding = pool %v proxy %v", created.ProxyPoolID, created.ProxyID)
	}
	var protocol, host, username, password string
	var port int
	if err := a.db.QueryRow(`SELECT protocol, host, port, username, password FROM proxies WHERE id = ?`, *created.ProxyID).Scan(&protocol, &host, &port, &username, &password); err != nil {
		t.Fatal(err)
	}
	if protocol != "http" || host != "127.0.0.1" || port != 18080 || username != "proxy-user" || password != "proxy-password" {
		t.Fatalf("stored proxy = %s://%s:%s@%s:%d", protocol, username, password, host, port)
	}

	payload["name"] = "duplicate-proxy-account"
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusConflict, nil)
	var accountCount, proxyCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE deleted_at IS NULL`).Scan(&accountCount); err != nil {
		t.Fatal(err)
	}
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxies WHERE deleted_at IS NULL`).Scan(&proxyCount); err != nil {
		t.Fatal(err)
	}
	if accountCount != 1 || proxyCount != 1 {
		t.Fatalf("exclusive binding left accounts=%d proxies=%d", accountCount, proxyCount)
	}
}

func TestAuditRedactsManualProxyCredentials(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/accounts", strings.NewReader(`{"proxy_text":"socks5://proxy-user:proxy-password@127.0.0.1:1080"}`))
	body := readAuditBody(request)
	if strings.Contains(body, "proxy-user") || strings.Contains(body, "proxy-password") || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("manual proxy was not redacted from audit body: %s", body)
	}
	batchRequest := httptest.NewRequest(http.MethodPost, "/api/proxies/batch", strings.NewReader(`{"pool_id":1,"text":"proxy-user:proxy-password@127.0.0.1:1080"}`))
	batchBody := readAuditBody(batchRequest)
	if strings.Contains(batchBody, "proxy-user") || strings.Contains(batchBody, "proxy-password") || !strings.Contains(batchBody, "[REDACTED]") {
		t.Fatalf("proxy batch was not redacted from audit body: %s", batchBody)
	}
	poolRequest := httptest.NewRequest(http.MethodPost, "/api/proxy-pools", strings.NewReader(`{"api_url":"https://proxy.example.test?token=private","api_headers":"{\"Authorization\":\"Bearer private\"}"}`))
	poolBody := readAuditBody(poolRequest)
	if strings.Contains(poolBody, "private") || !strings.Contains(poolBody, "[REDACTED]") {
		t.Fatalf("proxy API credentials were not redacted from audit body: %s", poolBody)
	}
}

func TestAPIKeyGatewayDispatchAndBilling(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("upstream path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer upstream-token" {
			t.Fatalf("upstream authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req-gateway-test")
		_, _ = w.Write([]byte(`{"id":"msg_1","usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":5,"cache_read_input_tokens":2}}`))
	}))
	defer upstream.Close()

	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var user panelUser
	putJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "client", "name": "Client", "password": "client-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 10,
	}, http.StatusCreated, &user)
	var key apiKeyRecord
	putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"user_id": user.ID, "name": "test-key", "group_id": "a", "status": "active", "quota": 10,
	}, http.StatusCreated, &key)
	if !strings.HasPrefix(key.Key, "sk-") {
		t.Fatalf("generated key = %q", key.Key)
	}
	proxyID := createTestForwardProxy(t, a)
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "gateway-account", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "upstream-token"},
		"extra":       map[string]any{"custom_forward_url": upstream.URL}, "status": "active", "schedulable": true,
		"concurrency": 2, "priority": 1, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID,
		"base_rpm": 15, "rpm_strategy": "tiered", "rpm_sticky_buffer": 0, "user_msg_queue_mode": "off",
	}, http.StatusCreated, nil)

	var response map[string]any
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 32, "messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusOK, &response)
	if response["id"] != "msg_1" {
		t.Fatalf("gateway response = %#v", response)
	}
	var usageCount int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM usage_logs WHERE request_id = 'req-gateway-test' AND input_tokens = 100 AND output_tokens = 20`).Scan(&usageCount); err != nil || usageCount != 1 {
		t.Fatalf("usageCount=%d err=%v", usageCount, err)
	}
}

func TestUsageLedgerAttributesRequestsToTheProducingKey(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req-key-attribution")
		_, _ = w.Write([]byte(`{"id":"msg_key","usage":{"input_tokens":30,"output_tokens":7}}`))
	}))
	defer upstream.Close()

	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()

	var user panelUser
	putJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "ledger-client", "name": "Ledger", "password": "client-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 0,
	}, http.StatusCreated, &user)
	var caller, other apiKeyRecord
	putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"user_id": user.ID, "name": "caller-sk", "group_id": "a", "status": "active", "quota": 0,
	}, http.StatusCreated, &caller)
	putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"user_id": user.ID, "name": "idle-sk", "group_id": "a", "status": "active", "quota": 0,
	}, http.StatusCreated, &other)
	proxyID := createTestForwardProxy(t, a)
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "ledger-account", "platform": "anthropic", "auth_type": "oauth",
		"credentials": map[string]any{"access_token": "upstream-token"},
		"extra":       map[string]any{"custom_forward_url": upstream.URL}, "status": "active", "schedulable": true,
		"concurrency": 2, "priority": 1, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxyID,
		"base_rpm": 15, "rpm_strategy": "tiered", "rpm_sticky_buffer": 0, "user_msg_queue_mode": "off",
	}, http.StatusCreated, nil)

	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 32, "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil, caller.Key, http.StatusOK, nil)

	var ledger struct {
		Items []usageLog `json:"items"`
		Total int64      `json:"total"`
	}
	putJSON(t, handler, http.MethodGet, "/api/usage", nil, http.StatusOK, &ledger)
	if len(ledger.Items) != 1 {
		t.Fatalf("ledger items = %d, want 1", len(ledger.Items))
	}
	entry := ledger.Items[0]
	if entry.APIKeyID == nil || *entry.APIKeyID != caller.ID {
		t.Fatalf("ledger api_key_id = %v, want %d", entry.APIKeyID, caller.ID)
	}
	if entry.APIKeyName != "caller-sk" || entry.APIKeyPrefix != caller.KeyPrefix {
		t.Fatalf("ledger key attribution = %q/%q", entry.APIKeyName, entry.APIKeyPrefix)
	}

	// Filtering by the key that never called must not return the other key's row.
	var filtered struct {
		Items []usageLog `json:"items"`
		Total int64      `json:"total"`
	}
	putJSON(t, handler, http.MethodGet, fmt.Sprintf("/api/usage?api_key_id=%d", other.ID), nil, http.StatusOK, &filtered)
	if filtered.Total != 0 || len(filtered.Items) != 0 {
		t.Fatalf("idle key ledger total=%d items=%d", filtered.Total, len(filtered.Items))
	}
	putJSON(t, handler, http.MethodGet, fmt.Sprintf("/api/usage?api_key_id=%d", caller.ID), nil, http.StatusOK, &filtered)
	if filtered.Total != 1 {
		t.Fatalf("caller key ledger total = %d, want 1", filtered.Total)
	}

	var summary billingSummary
	putJSON(t, handler, http.MethodGet, "/api/billing", nil, http.StatusOK, &summary)
	if len(summary.ByAPIKey) != 1 {
		t.Fatalf("billing by_api_key = %+v", summary.ByAPIKey)
	}
	if summary.ByAPIKey[0].Name != "caller-sk" || summary.ByAPIKey[0].Requests != 1 {
		t.Fatalf("billing key breakdown = %+v", summary.ByAPIKey[0])
	}
}

func TestIndependentAPIKeysCanBeDisabledImmediately(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	users := make([]panelUser, 2)
	keys := make([]apiKeyRecord, 2)
	for i := range users {
		putJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
			"username": "client-" + string(rune('a'+i)), "name": "Client", "password": "client-password", "role": "user",
			"status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 0,
		}, http.StatusCreated, &users[i])
		putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
			"user_id": users[i].ID, "name": "key", "group_id": "a", "status": "active", "quota": 0,
		}, http.StatusCreated, &keys[i])
	}
	if keys[0].UserID == keys[1].UserID || keys[0].Key == keys[1].Key {
		t.Fatal("API keys must have independent owners and secrets")
	}
	if _, err := a.authenticateGatewayKey(keys[0].Key); err != nil {
		t.Fatalf("active key rejected: %v", err)
	}
	putJSON(t, handler, http.MethodPatch, "/api/api-keys/"+strconv.FormatInt(keys[0].ID, 10)+"/status", map[string]any{"status": "disabled"}, http.StatusOK, nil)
	if _, err := a.authenticateGatewayKey(keys[0].Key); err == nil {
		t.Fatal("disabled key remained usable")
	}
	if _, err := a.authenticateGatewayKey(keys[1].Key); err != nil {
		t.Fatalf("disabling one user's key affected another user: %v", err)
	}
}

func TestCreatedAPIKeyRemainsCopyable(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	var user panelUser
	putJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "copy-client", "name": "Copy Client", "password": "client-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 0,
	}, http.StatusCreated, &user)
	var created apiKeyRecord
	putJSON(t, handler, http.MethodPost, "/api/api-keys", map[string]any{
		"user_id": user.ID, "name": "copyable-key", "group_id": "a", "status": "active", "quota": 0,
	}, http.StatusCreated, &created)
	if !strings.HasPrefix(created.Key, "sk-") {
		t.Fatalf("created key = %q", created.Key)
	}
	var listed []apiKeyRecord
	putJSON(t, handler, http.MethodGet, "/api/api-keys", nil, http.StatusOK, &listed)
	if len(listed) != 1 || listed[0].Key != created.Key {
		t.Fatalf("listed keys = %+v, want stored secret %q", listed, created.Key)
	}
	if _, err := a.authenticateGatewayKey(listed[0].Key); err != nil {
		t.Fatalf("listed secret was not usable: %v", err)
	}
}

func TestAuditLogsLoginMutationsAndRedactsSecrets(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	cookie := loginCookie(t, handler, "admin", "admin-password")
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "logged-user", "name": "Logged", "password": "private-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{"a"}, "rpm_limit": 0,
	}, cookie, "", http.StatusCreated, nil)
	var page struct {
		Items []auditLog `json:"items"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/audit-logs", nil, cookie, "", http.StatusOK, &page)
	logs := page.Items
	var loginSeen, createSeen bool
	for _, item := range logs {
		switch item.Action {
		case "auth.login":
			loginSeen = true
		case "user.create":
			createSeen = true
			if strings.Contains(item.RequestBody, "private-password") || !strings.Contains(item.RequestBody, "[REDACTED]") {
				t.Fatalf("audit body was not redacted: %s", item.RequestBody)
			}
		}
	}
	if !loginSeen || !createSeen {
		t.Fatalf("audit logs missing login=%v create=%v: %+v", loginSeen, createSeen, logs)
	}
}

func TestAuditLogsUseServerSidePagination(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	cookie := loginCookie(t, handler, "admin", "admin-password")
	for index := 0; index < 25; index++ {
		a.insertAudit(panelUser{Username: "admin", Role: "admin"}, "test.event", http.MethodPost, "/api/test", "test", strconv.Itoa(index), "{}", "127.0.0.1", "test", http.StatusOK, 1)
	}
	var page struct {
		Items      []auditLog `json:"items"`
		Total      int64      `json:"total"`
		Page       int        `json:"page"`
		PageSize   int        `json:"page_size"`
		TotalPages int        `json:"total_pages"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/audit-logs?action=test.event&page=2&page_size=10", nil, cookie, "", http.StatusOK, &page)
	if page.Total != 25 || page.Page != 2 || page.PageSize != 10 || page.TotalPages != 3 || len(page.Items) != 10 {
		t.Fatalf("audit page=%+v", page)
	}
}

func TestCachePrefixAuditLogsSupportSearchSummaryAndPagination(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	cookie := loginCookie(t, handler, "admin", "admin-password")
	result, err := a.db.Exec(`INSERT INTO accounts (name) VALUES ('audit-account@example.com')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	for _, event := range []struct {
		session string
		segment string
		model   string
	}{
		{session: "session-alpha", segment: "tools", model: "claude-fable-5"},
		{session: "session-alpha", segment: "system", model: "claude-fable-5"},
		{session: "session-beta", segment: "initial", model: "claude-opus-4-8"},
	} {
		if _, err := a.db.Exec(`INSERT INTO cache_prefix_events
			(session_hash, account_id, model, prefix_hash, tools_hash, system_hash, changed_segment, previous_prefix_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, event.session, accountID, event.model, "prefix-"+event.segment, "tools", "system", event.segment, "previous"); err != nil {
			t.Fatal(err)
		}
	}
	var page struct {
		Items   []cachePrefixAuditLog   `json:"items"`
		Summary cachePrefixAuditSummary `json:"summary"`
		Total   int64                   `json:"total"`
		Page    int                     `json:"page"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/audit-logs/cache-prefixes?search=alpha&segment=tools&page=1&page_size=1", nil, cookie, "", http.StatusOK, &page)
	if page.Total != 1 || page.Page != 1 || len(page.Items) != 1 || page.Items[0].ChangedSegment != "tools" {
		t.Fatalf("cache audit page=%+v", page)
	}
	if page.Summary.Sessions != 1 || page.Summary.ToolsChanged != 1 || page.Summary.SystemChanged != 0 || page.Summary.Accounts != 1 {
		t.Fatalf("cache audit summary=%+v", page.Summary)
	}
	if page.Items[0].AccountName != "audit-account@example.com" || page.Items[0].AccountID == nil || *page.Items[0].AccountID != accountID {
		t.Fatalf("cache audit account=%+v", page.Items[0])
	}
}

func TestStartupClearsStaleAccountInflightLeases(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	databasePath := filepath.Join(t.TempDir(), "test.db")
	a, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO accounts (name) VALUES ('stale-inflight')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	if _, err := a.db.Exec(`INSERT INTO account_inflight (account_id, requests) VALUES (?, 1)`, accountID); err != nil {
		t.Fatal(err)
	}
	if err := a.db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := newApp(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	var count int
	if err := reopened.db.QueryRow(`SELECT COUNT(*) FROM account_inflight`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale in-flight rows=%d", count)
	}
}

func TestOrdinaryUserReadsOnlyAllowedGroupsAndOwnedUsage(t *testing.T) {
	t.Setenv("CCMAX_ADMIN_PASSWORD", "admin-password")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	adminCookie := loginCookie(t, handler, "admin", "admin-password")
	var userA, userB panelUser
	pages := []string{"overview", "accounts", "daily", "authorization", "proxies", "access", "billing", "audit"}
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "tenant-a", "name": "Tenant A", "password": "tenant-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{"a"}, "visible_pages": pages, "balance": 25, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, &userA)
	requestJSON(t, handler, http.MethodPost, "/api/users", map[string]any{
		"username": "tenant-b", "name": "Tenant B", "password": "tenant-password", "role": "user",
		"status": "active", "allowed_group_ids": []string{"b"}, "visible_pages": pages, "balance": 50, "rpm_limit": 0,
	}, adminCookie, "", http.StatusCreated, &userB)

	createAccount := func(name, groupID string) account {
		proxyID := createTestForwardProxy(t, a)
		var created account
		requestJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
			"name": name, "platform": "anthropic", "auth_type": "oauth",
			"credentials": map[string]any{"access_token": "token-" + groupID}, "extra": map[string]any{},
			"status": "active", "schedulable": true, "concurrency": 1, "priority": 10,
			"rate_multiplier": 1, "group_ids": []string{groupID}, "proxy_pool_id": 1, "proxy_id": proxyID,
		}, adminCookie, "", http.StatusCreated, &created)
		return created
	}
	accountA := createAccount("account-a", "a")
	accountB := createAccount("account-b", "b")
	if _, err := a.db.Exec(`INSERT INTO account_groups (account_id, group_id, priority) VALUES (?, 'b', 10)`, accountA.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.recordUsage(usageInput{UserID: userA.ID, RequestID: "tenant-a-usage", PurposeKey: "default", GroupID: "a", AccountID: accountA.ID, Model: "claude-test", InputTokens: 100}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.recordUsage(usageInput{UserID: userB.ID, RequestID: "tenant-b-usage", PurposeKey: "default", GroupID: "b", AccountID: accountB.ID, Model: "claude-test", InputTokens: 200}); err != nil {
		t.Fatal(err)
	}
	a.recordAuthorization(&accountA.ID, accountA.ProxyID, accountA.Name, "test", true, "ok", "max", "127.0.0.1")
	a.recordAuthorization(&accountB.ID, accountB.ProxyID, accountB.Name, "test", true, "ok", "max", "127.0.0.1")
	a.insertAudit(userA, "tenant-a.event", http.MethodGet, "/api/test", "test", "a", "{}", "127.0.0.1", "ok", http.StatusOK, 1)
	a.insertAudit(userB, "tenant-b.event", http.MethodGet, "/api/test", "test", "b", "{}", "127.0.0.1", "ok", http.StatusOK, 1)

	userCookie := loginCookie(t, handler, "tenant-a", "tenant-password")
	var accounts []map[string]any
	requestJSON(t, handler, http.MethodGet, "/api/accounts", nil, userCookie, "", http.StatusOK, &accounts)
	if len(accounts) != 1 || int64(accounts[0]["id"].(float64)) != accountA.ID {
		t.Fatalf("scoped accounts=%+v", accounts)
	}
	if accounts[0]["name"] != accountA.Name {
		t.Fatalf("ordinary account view missing configured account name: %+v", accounts[0])
	}
	if _, leaked := accounts[0]["group_ids"]; leaked {
		t.Fatalf("ordinary account view leaked group membership: %+v", accounts[0])
	}
	var summary accountSummary
	requestJSON(t, handler, http.MethodGet, "/api/accounts/summary", nil, userCookie, "", http.StatusOK, &summary)
	if summary.Accounts != 1 || summary.Requests != 1 {
		t.Fatalf("scoped account summary=%+v", summary)
	}
	var dashboard dashboard
	requestJSON(t, handler, http.MethodGet, "/api/dashboard", nil, userCookie, "", http.StatusOK, &dashboard)
	if dashboard.AccountsTotal != 1 || len(dashboard.Groups) != 1 || dashboard.Groups[0].ID != "a" || dashboard.Today.Requests != 1 {
		t.Fatalf("scoped dashboard=%+v", dashboard)
	}
	var proxies []proxyRecord
	requestJSON(t, handler, http.MethodGet, "/api/proxies", nil, userCookie, "", http.StatusOK, &proxies)
	if len(proxies) != 1 || accountA.ProxyID == nil || proxies[0].ID != *accountA.ProxyID {
		t.Fatalf("scoped proxies=%+v", proxies)
	}
	var usagePage struct {
		Items []usageLog `json:"items"`
		Total int64      `json:"total"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/usage", nil, userCookie, "", http.StatusOK, &usagePage)
	if usagePage.Total != 1 || len(usagePage.Items) != 1 || usagePage.Items[0].RequestID != "tenant-a-usage" {
		t.Fatalf("scoped usage=%+v", usagePage)
	}
	if usagePage.Items[0].BilledCost <= 0 || usagePage.Items[0].ActualCost != 0 || usagePage.Items[0].BaseCost != 0 {
		t.Fatalf("ordinary user usage costs were not redacted: %+v", usagePage.Items[0])
	}
	var billing billingSummary
	requestJSON(t, handler, http.MethodGet, "/api/billing", nil, userCookie, "", http.StatusOK, &billing)
	if billing.Totals.Requests != 1 || billing.Totals.BilledCost <= 0 || billing.Totals.ActualCost != 0 || billing.Totals.Margin != 0 {
		t.Fatalf("scoped billing totals=%+v", billing.Totals)
	}
	if billing.AvailableBalance == nil || *billing.AvailableBalance >= 25 || len(billing.ByAccount) != 1 || billing.ByAccount[0].Key != strconv.FormatInt(accountA.ID, 10) {
		t.Fatalf("scoped billing=%+v", billing)
	}
	requestJSON(t, handler, http.MethodPut, "/api/accounts/"+strconv.FormatInt(accountA.ID, 10), map[string]any{}, userCookie, "", http.StatusForbidden, nil)
	var authorization authorizationStats
	requestJSON(t, handler, http.MethodGet, "/api/authorization-logs", nil, userCookie, "", http.StatusOK, &authorization)
	if authorization.Summary.Total != 1 || len(authorization.Items) != 1 || authorization.Items[0].AccountID == nil || *authorization.Items[0].AccountID != accountA.ID {
		t.Fatalf("scoped authorization=%+v", authorization)
	}
	var audits struct {
		Items []auditLog `json:"items"`
	}
	requestJSON(t, handler, http.MethodGet, "/api/audit-logs", nil, userCookie, "", http.StatusOK, &audits)
	for _, item := range audits.Items {
		if item.ActorUserID == nil || *item.ActorUserID != userA.ID {
			t.Fatalf("unscoped audit item=%+v", item)
		}
	}
	requestJSON(t, handler, http.MethodPut, "/api/users/"+strconv.FormatInt(userA.ID, 10), map[string]any{
		"username": "tenant-a", "name": "Tenant A", "password": "", "role": "user", "status": "active",
		"allowed_group_ids": []string{"a"}, "visible_pages": pages, "balance": 10, "rpm_limit": 0,
	}, adminCookie, "", http.StatusOK, &userA)
	var refreshed panelUser
	requestJSON(t, handler, http.MethodGet, "/api/me", nil, userCookie, "", http.StatusOK, &refreshed)
	if refreshed.Balance == nil || *refreshed.Balance != 10 {
		t.Fatalf("updated user balance=%v", refreshed.Balance)
	}
}

func TestGatewayEnforcesGroupBillingLimit(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		_, _ = w.Write([]byte(`{"id":"unexpected"}`))
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	account := createGatewayTestAccount(t, a, handler, "budget", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	if _, _, err := a.recordUsage(usageInput{UserID: key.UserID, APIKeyID: key.ID, RequestID: "budget-spent", PurposeKey: "default", GroupID: "a", AccountID: account.ID, Model: "claude-test", InputTokens: 1000}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(`UPDATE groups SET daily_limit_usd = 0.000001, monthly_limit_usd = 100 WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	requestJSON(t, handler, http.MethodPost, "/v1/messages", map[string]any{
		"model": "claude-test", "max_tokens": 8, "messages": []map[string]string{{"role": "user", "content": "hello"}},
	}, nil, key.Key, http.StatusForbidden, nil)
	if upstreamCalls != 0 {
		t.Fatalf("group budget allowed %d upstream calls", upstreamCalls)
	}
}

func TestConcurrentRequestsDoNotOverrunAPIKeyQuota(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	var requestNumber int
	var requestMu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestMu.Lock()
		requestNumber++
		current := requestNumber
		requestMu.Unlock()
		w.Header().Set("request-id", "quota-"+strconv.Itoa(current))
		_, _ = w.Write([]byte(`{"id":"msg","usage":{"input_tokens":100,"output_tokens":100}}`))
	}))
	defer upstream.Close()
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	key := createGatewayTestKey(t, handler)
	if _, err := a.db.Exec(`UPDATE api_keys SET quota = 0.0001 WHERE id = ?`, key.ID); err != nil {
		t.Fatal(err)
	}
	createGatewayTestAccount(t, a, handler, "quota", upstream.URL, 0, nil, map[string]any{"access_token": "token"})
	start := make(chan struct{})
	statuses := make(chan int, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			body := bytes.NewBufferString(`{"model":"claude-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			statuses <- response.Code
		}()
	}
	close(start)
	wait.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusOK] != 1 || counts[http.StatusForbidden] != 1 || requestNumber != 1 {
		t.Fatalf("statuses=%v upstream requests=%d", counts, requestNumber)
	}
}

func TestManualProxyAssignmentIsAlsoExclusive(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	putJSON(t, handler, http.MethodPost, "/api/proxies/batch", map[string]any{"pool_id": 1, "text": "http://127.0.0.1:13001"}, http.StatusCreated, nil)
	var proxies []proxyRecord
	putJSON(t, handler, http.MethodGet, "/api/proxies", nil, http.StatusOK, &proxies)
	if len(proxies) != 1 {
		t.Fatalf("proxy count = %d", len(proxies))
	}
	payload := map[string]any{
		"name": "manual-one", "platform": "anthropic", "auth_type": "oauth", "credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"},
		"proxy_pool_id": 1, "proxy_id": proxies[0].ID, "auto_proxy": false, "rpm_strategy": "tiered", "user_msg_queue_mode": "off",
	}
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusCreated, nil)
	payload["name"] = "manual-two"
	putJSON(t, handler, http.MethodPost, "/api/accounts", payload, http.StatusConflict, nil)
}

func TestQuotaHeadersAndFilteredAccountSummary(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	var created account
	putJSON(t, handler, http.MethodPost, "/api/accounts", map[string]any{
		"name": "summary-account", "platform": "anthropic", "auth_type": "oauth", "credentials": map[string]any{"access_token": "token"}, "extra": map[string]any{},
		"status": "active", "schedulable": true, "concurrency": 1, "priority": 10, "rate_multiplier": 1, "group_ids": []string{"a"},
	}, http.StatusCreated, &created)
	reset := time.Now().Add(time.Hour).Unix()
	headers := http.Header{}
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.25")
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset, 10))
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "0.60")
	headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(reset, 10))
	a.captureAccountUpstreamState(created.ID, &http.Response{StatusCode: http.StatusOK, Header: headers})
	updated, err := a.getAccount(created.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Quota5H != 25 || updated.Quota7D != 60 || updated.AuthStatus != "valid" {
		t.Fatalf("quota/auth snapshot = %+v", updated)
	}
	putJSON(t, handler, http.MethodPost, "/api/usage", map[string]any{"request_id": "summary-req", "purpose_key": "default", "account_id": created.ID, "model": "claude-test", "input_tokens": 1000, "output_tokens": 2000}, http.StatusCreated, nil)
	var summary accountSummary
	putJSON(t, handler, http.MethodGet, "/api/accounts/summary?search=summary&group_id=a", nil, http.StatusOK, &summary)
	if summary.Accounts != 1 || summary.Requests != 1 || summary.BilledCost <= 0 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestModelPricesSyncFromSub2APICompatibleSource(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	pricing := []byte(`{"claude-test":{"input_cost_per_token":0.000003,"output_cost_per_token":0.000015,"cache_creation_input_token_cost":0.00000375,"cache_read_input_token_cost":0.0000003,"litellm_provider":"anthropic"},"gpt-test":{"input_cost_per_token":1,"output_cost_per_token":1,"litellm_provider":"openai"}}`)
	hash := sha256.Sum256(pricing)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			_, _ = w.Write([]byte(hex.EncodeToString(hash[:])))
			return
		}
		_, _ = w.Write(pricing)
	}))
	defer server.Close()
	a, err := newApp(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	_, _ = a.db.Exec(`UPDATE pricing_sync_state SET remote_url = ?, hash_url = ? WHERE id = 1`, server.URL+"/prices.json", server.URL+"/prices.sha256")
	state, err := a.syncModelPrices(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if state.ModelCount != 1 || state.Status != "current" {
		t.Fatalf("sync state = %+v", state)
	}
	var input, output float64
	var source string
	if err := a.db.QueryRow(`SELECT input_per_million, output_per_million, source FROM model_prices WHERE model = 'claude-test'`).Scan(&input, &output, &source); err != nil {
		t.Fatal(err)
	}
	if input != 3 || output != 15 || source != "remote" {
		t.Fatalf("synced price input=%v output=%v source=%s", input, output, source)
	}
}

func loginCookie(t *testing.T, handler http.Handler, username, password string) *http.Cookie {
	t.Helper()
	response := requestJSON(t, handler, http.MethodPost, "/api/auth/login", map[string]any{"username": username, "password": password}, nil, "", http.StatusOK, nil)
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	t.Fatal("login response has no session cookie")
	return nil
}

func requestJSON(t *testing.T, handler http.Handler, method, path string, input any, cookie *http.Cookie, apiKey string, wantStatus int, output any) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	if input != nil {
		if err := json.NewEncoder(&body).Encode(input); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &body)
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+apiKey)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s status = %d, want %d; body=%s", method, path, response.Code, wantStatus, response.Body.String())
	}
	if output != nil && response.Code != http.StatusNoContent {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			t.Fatal(err)
		}
	}
	return response
}

func putJSON(t *testing.T, handler http.Handler, method, path string, input any, wantStatus int, output any) {
	t.Helper()
	requestJSON(t, handler, method, path, input, nil, "", wantStatus, output)
}

func assertClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-8 {
		t.Fatalf("got %v, want %v", got, want)
	}
}
