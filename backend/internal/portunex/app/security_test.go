package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalHostPeerAndOriginGuards(t *testing.T) {
	h := demo(t)
	for _, test := range []struct {
		name, host, peer, origin string
		want                     int
	}{
		{"local", "127.0.0.1:18090", "127.0.0.1:30000", "http://127.0.0.1:18090", 401},
		{"vite proxy preserved host", "127.0.0.1:5173", "127.0.0.1:30000", "http://127.0.0.1:5173", 401},
		{"cli no origin", "localhost:18090", "[::1]:30000", "", 401},
		{"remote peer", "localhost:18090", "192.0.2.1:30000", "", 403},
		{"rebinding host", "attacker.invalid:18090", "127.0.0.1:30000", "", 403},
		{"cross site", "127.0.0.1:18090", "127.0.0.1:30000", "https://attacker.invalid", 403},
		{"null origin", "127.0.0.1:18090", "127.0.0.1:30000", "null", 403},
		{"origin path", "127.0.0.1:18090", "127.0.0.1:30000", "http://127.0.0.1:18090/", 403},
		{"bad port", "127.0.0.1:abc", "127.0.0.1:30000", "", 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://localhost"+Prefix+"/session", nil)
			r.Host, r.RemoteAddr = test.host, test.peer
			if test.origin != "" {
				r.Header.Set("Origin", test.origin)
			}
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			r.Header.Set("X-Forwarded-Host", "localhost:18090")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.want {
				t.Fatalf("status = %d", w.Code)
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("demo must not enable cross-origin access")
			}
		})
	}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18090"+Prefix+"/session", nil)
	r.RemoteAddr = "127.0.0.1:30000"
	r.Header.Add("Origin", "http://127.0.0.1:18090")
	r.Header.Add("Origin", "https://attacker.invalid")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatal("duplicate Origin accepted")
	}
}
