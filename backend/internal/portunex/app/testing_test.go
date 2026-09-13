package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func call(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:18090"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:31000"
	r.Header.Set("Origin", "http://127.0.0.1:18090")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func demo(t *testing.T) *Demo {
	t.Helper()
	h, err := NewDemo(Options{})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func loginAs(t *testing.T, handler http.Handler, member bool) *http.Cookie {
	t.Helper()
	body := `{"email":"admin@example.invalid","password":"Demo-admin-2026!"}`
	if member {
		body = `{"email":"member@example.invalid","password":"Demo-member-2026!"}`
	}
	w := call(t, handler, http.MethodPost, Prefix+"/login", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("login failed: %d", w.Code)
	}
	for _, cookie := range w.Result().Cookies() {
		if cookie.Name == CookieName {
			return cookie
		}
	}
	t.Fatal("missing session cookie")
	return nil
}
