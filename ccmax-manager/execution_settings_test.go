package main

import (
	"net/http"
	"path/filepath"
	"strconv"
	"testing"
)

func TestExecutionSettingsAPIDefaultsAndUpdates(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	handler := a.routes()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('execution-settings-account', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()

	var accountSettings accountExecutionSettings
	putJSON(t, handler, http.MethodGet, "/api/accounts/"+formatInt64(accountID)+"/execution", nil, http.StatusOK, &accountSettings)
	if accountSettings.MigrationStatus != "legacy" || accountSettings.RuntimeStatus != "legacy" || accountSettings.PreferredMode != executionModeCLINative {
		t.Fatalf("unexpected account execution defaults: %+v", accountSettings)
	}
	if len(accountSettings.AllowedModes) != 2 || accountSettings.CLINativeLimit != 1 || accountSettings.OAuthAPILimit != 3 || accountSettings.TotalLimit != 3 {
		t.Fatalf("unexpected account modes/limits: %+v", accountSettings)
	}
	if len(accountSettings.ModeHealth) != 2 || accountSettings.ModeHealth[0].Status != "unavailable" || accountSettings.DataPlaneEnabled {
		t.Fatalf("unexpected account execution health: %+v", accountSettings)
	}

	putJSON(t, handler, http.MethodPut, "/api/accounts/"+formatInt64(accountID)+"/execution", map[string]any{
		"allowed_modes": []string{executionModeOAuthAPI}, "preferred_mode": executionModeOAuthAPI,
		"cli_native_limit": 1, "oauth_api_limit": 5, "total_limit": 5,
	}, http.StatusOK, &accountSettings)
	if len(accountSettings.AllowedModes) != 1 || accountSettings.AllowedModes[0] != executionModeOAuthAPI || accountSettings.OAuthAPILimit != 5 {
		t.Fatalf("account execution update was not persisted: %+v", accountSettings)
	}

	var groupSettings groupExecutionSettings
	putJSON(t, handler, http.MethodGet, "/api/groups/a/execution", nil, http.StatusOK, &groupSettings)
	if groupSettings.ExecutionPolicy != "auto" || groupSettings.WorkerQueueMode != "queue" || groupSettings.WorkerImageChannel != "stable" || groupSettings.DataPlaneEnabled {
		t.Fatalf("unexpected group execution defaults: %+v", groupSettings)
	}
	putJSON(t, handler, http.MethodPut, "/api/groups/a/execution", map[string]any{
		"execution_policy": "api_only", "worker_queue_mode": "reject", "worker_image_channel": "canary",
	}, http.StatusOK, &groupSettings)
	if groupSettings.ExecutionPolicy != "api_only" || groupSettings.WorkerQueueMode != "reject" || groupSettings.WorkerImageChannel != "canary" {
		t.Fatalf("group execution update was not persisted: %+v", groupSettings)
	}
}

func TestExecutionSettingsRejectInvalidModeAndLimits(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, err := newApp(filepath.Join(t.TempDir(), "execution-settings-invalid.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.db.Close()
	result, err := a.db.Exec(`INSERT INTO accounts (name, credentials_json) VALUES ('execution-settings-invalid', '{}')`)
	if err != nil {
		t.Fatal(err)
	}
	accountID, _ := result.LastInsertId()
	handler := a.routes()
	putJSON(t, handler, http.MethodPut, "/api/accounts/"+formatInt64(accountID)+"/execution", map[string]any{
		"allowed_modes": []string{executionModeOAuthAPI}, "preferred_mode": executionModeCLINative,
		"cli_native_limit": 1, "oauth_api_limit": 3, "total_limit": 3,
	}, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodPut, "/api/accounts/"+formatInt64(accountID)+"/execution", map[string]any{
		"allowed_modes": []string{executionModeCLINative}, "preferred_mode": executionModeCLINative,
		"cli_native_limit": 4, "oauth_api_limit": 3, "total_limit": 3,
	}, http.StatusBadRequest, nil)
	putJSON(t, handler, http.MethodPut, "/api/groups/a/execution", map[string]any{
		"execution_policy": "legacy", "worker_queue_mode": "queue", "worker_image_channel": "stable",
	}, http.StatusBadRequest, nil)
}

func TestChooseExecutionDispatchKeepsLegacyDefaultAndNeverFallsBackAfterMigration(t *testing.T) {
	group := groupExecutionSettings{ExecutionPolicy: "auto"}
	account := accountExecutionSettings{
		AllowedModes: []string{executionModeCLINative, executionModeOAuthAPI}, PreferredMode: executionModeCLINative,
		MigrationStatus: "legacy", RuntimeStatus: "legacy",
	}
	decision := chooseExecutionDispatch(true, group, account)
	if decision.Kind != executionDispatchLegacy {
		t.Fatalf("legacy default decision=%+v", decision)
	}

	account.MigrationStatus = "migrated"
	account.RuntimeStatus = "ready"
	account.RuntimeSlotID = "ccmax-account-7"
	account.RuntimeGeneration = 3
	account.RuntimeExecutionEpoch = 9
	account.ModeHealth = []accountModeHealth{
		{Mode: executionModeCLINative, Status: "healthy"},
		{Mode: executionModeOAuthAPI, Status: "healthy"},
	}
	decision = chooseExecutionDispatch(false, group, account)
	if decision.Kind != executionDispatchUnavailable || decision.ReasonCode != "data_plane_disabled" {
		t.Fatalf("migrated account fell back while data plane disabled: %+v", decision)
	}
	decision = chooseExecutionDispatch(true, group, account)
	if decision.Kind != executionDispatchDataPlane || decision.Mode != executionModeCLINative {
		t.Fatalf("preferred execution mode decision=%+v", decision)
	}
	group.ExecutionPolicy = "api_only"
	decision = chooseExecutionDispatch(true, group, account)
	if decision.Kind != executionDispatchDataPlane || decision.Mode != executionModeOAuthAPI {
		t.Fatalf("api_only execution decision=%+v", decision)
	}
	account.ModeHealth[1].Status = "billing_blocked"
	decision = chooseExecutionDispatch(true, group, account)
	if decision.Kind != executionDispatchUnavailable || decision.ReasonCode != "no_healthy_mode" {
		t.Fatalf("unhealthy forced mode decision=%+v", decision)
	}
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
