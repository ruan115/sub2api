package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/config"
)

func TestHealthHandler(t *testing.T) {
	cfg := config.Default(config.RoleHostAgent)
	cfg.NodeID = "srv74"
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	Handler(cfg).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", recorder.Code)
	}
	var response healthResponse
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "ok" || response.Role != config.RoleHostAgent || response.NodeID != "srv74" {
		t.Fatalf("unexpected response: %+v", response)
	}
}
