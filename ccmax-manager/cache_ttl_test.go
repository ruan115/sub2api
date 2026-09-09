package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGroupForceCacheTTL5MDefaultsAndPersistence(t *testing.T) {
	t.Setenv("CCMAX_AUTH_DISABLED", "1")
	a, handler := newGatewayTestApp(t)
	defer a.db.Close()
	var groups []group
	putJSON(t, handler, http.MethodGet, "/api/groups", nil, http.StatusOK, &groups)
	for _, g := range groups {
		if !g.ForceCacheTTL5M {
			t.Fatalf("seeded group %s is off", g.ID)
		}
	}
	key := createGatewayTestKey(t, handler)
	base := map[string]any{"name": "cache test", "rate_multiplier": 1, "status": "active"}
	var created group
	putJSON(t, handler, http.MethodPost, "/api/groups", base, http.StatusCreated, &created)
	if !created.ForceCacheTTL5M {
		t.Fatal("new group must default on")
	}
	base["force_cache_ttl_5m_enabled"] = false
	base["name"] = "cache test disabled"
	putJSON(t, handler, http.MethodPost, "/api/groups", base, http.StatusCreated, &created)
	if created.ForceCacheTTL5M {
		t.Fatal("explicit false on creation was lost")
	}
	var updated group
	base["name"] = "A cache settings"
	for _, enabled := range []bool{false, true} {
		base["force_cache_ttl_5m_enabled"] = enabled
		putJSON(t, handler, http.MethodPut, "/api/groups/a", base, http.StatusOK, &updated)
		if updated.ForceCacheTTL5M != enabled {
			t.Fatal("switch did not persist")
		}
		delete(base, "force_cache_ttl_5m_enabled")
		putJSON(t, handler, http.MethodPut, "/api/groups/a", base, http.StatusOK, &updated)
		if updated.ForceCacheTTL5M != enabled {
			t.Fatal("legacy save changed switch")
		}
		auth, err := a.authenticateGatewayKey(key.Key)
		if err != nil || auth.ForceCacheTTL5M != enabled {
			t.Fatalf("gateway flag=%t err=%v", auth.ForceCacheTTL5M, err)
		}
	}
	// Simulate an existing database, then check migration and restart idempotency.
	if _, err := a.db.Exec("ALTER TABLE groups DROP COLUMN force_cache_ttl_5m_enabled"); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateAdvancedFeatures(); err != nil {
		t.Fatal(err)
	}
	groups, err := a.listGroups()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if !g.ForceCacheTTL5M {
			t.Fatalf("migrated group %s is off", g.ID)
		}
	}
	if _, err := a.db.Exec("UPDATE groups SET force_cache_ttl_5m_enabled = 0 WHERE id = 'a'"); err != nil {
		t.Fatal(err)
	}
	if err := a.migrateAdvancedFeatures(); err != nil {
		t.Fatal(err)
	}
	auth, err := a.authenticateGatewayKey(key.Key)
	if err != nil || auth.ForceCacheTTL5M {
		t.Fatalf("restart overwrote false: %+v %v", auth.ForceCacheTTL5M, err)
	}
}

func TestGatewayForceCacheTTL5MOnWire(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		normal, passthrough, count, stream, disabled bool
	}{
		{name: "original-default"},
		{name: "distilled-default", normal: true},
		{name: "distilled-stream", normal: true, stream: true},
		{name: "count-default", count: true},
		{name: "passthrough-default", passthrough: true},
		{name: "original-off", disabled: true},
		{name: "distilled-off", normal: true, disabled: true},
		{name: "passthrough-off", passthrough: true, disabled: true},
		{name: "count-off", count: true, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CCMAX_AUTH_DISABLED", "1")
			captured := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				captured <- body
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/count_tokens") {
					_, _ = io.WriteString(w, `{"input_tokens":9}`)
				} else {
					_, _ = io.WriteString(w, `{"id":"msg_cache","model":"claude-sonnet-4-5","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":1}}`)
				}
			}))
			defer upstream.Close()
			a, handler := newGatewayTestApp(t)
			defer a.db.Close()
			settings := map[string]any{"name": "cache test", "rate_multiplier": 1, "status": "active", "normal_request_mode": tc.normal}
			if tc.disabled {
				settings["force_cache_ttl_5m_enabled"] = false
			}
			putJSON(t, handler, http.MethodPut, "/api/groups/a", settings, http.StatusOK, nil)
			key := createGatewayTestKey(t, handler)
			createGatewayTestAccount(t, a, handler, "cache-wire", upstream.URL, 0, map[string]any{"request_passthrough": tc.passthrough}, map[string]any{"access_token": "test-token"})
			payload := fmt.Sprintf(`{"model":"claude-sonnet-4-5","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`, tc.stream)
			path := "/v1/messages"
			if tc.count {
				path += "/count_tokens"
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
			request.Header.Set("Authorization", "Bearer "+key.Key)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			body := <-captured
			messages := gjson.GetBytes(body, "messages").Array()
			ttl := messages[len(messages)-1].Get("content.0.cache_control.ttl").String()
			want := "5m"
			if tc.disabled {
				want = "1h"
			}
			if ttl != want {
				t.Fatalf("wire TTL=%q want=%q body=%s", ttl, want, body)
			}
			if tc.passthrough && tc.disabled && string(body) != payload {
				t.Fatal("disabled passthrough body changed")
			}
		})
	}
}
