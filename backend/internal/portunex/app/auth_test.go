package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/portunex/identity"
)

func TestCookieExpiryRevocationAndRelogin(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h, err := NewDemo(Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, h, false)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != Prefix || cookie.Domain != "" || cookie.MaxAge != 1800 {
		t.Fatal("cookie policy invalid")
	}
	w := call(t, h, http.MethodGet, Prefix+"/session", "", cookie)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), cookie.Value) {
		t.Fatal("session JSON must not disclose raw token")
	}
	w = call(t, h, http.MethodPost, Prefix+"/login", `{"email":"admin@example.invalid","password":"Demo-admin-2026!"}`, cookie)
	if w.Code != http.StatusOK {
		t.Fatal("re-login failed")
	}
	newCookie := w.Result().Cookies()[0]
	if call(t, h, http.MethodGet, Prefix+"/session", "", cookie).Code != http.StatusUnauthorized {
		t.Fatal("old token not retired")
	}
	now = now.Add(identity.SessionTTL)
	if call(t, h, http.MethodGet, Prefix+"/session", "", newCookie).Code != http.StatusUnauthorized {
		t.Fatal("expiry boundary not enforced")
	}
	cookie = loginAs(t, h, true)
	w = call(t, h, http.MethodPost, Prefix+"/logout", "", cookie)
	if w.Code != http.StatusNoContent || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("cookie not removed")
	}
	if call(t, h, http.MethodGet, Prefix+"/api-keys", "", cookie).Code != http.StatusUnauthorized {
		t.Fatal("logout did not revoke token")
	}
	if call(t, h, http.MethodPost, Prefix+"/logout", "", cookie).Code != http.StatusNoContent {
		t.Fatal("logout should be idempotent")
	}
}

func TestInvalidLoginBodiesAndCredentials(t *testing.T) {
	h := demo(t)
	for _, body := range []string{`null`, `{}`, `[]`, `{"email":1}`, `{"email":"x","password":"x","role":"admin"}`, `{"email":"x","password":"x"}{}`} {
		if w := call(t, h, http.MethodPost, Prefix+"/login", body, nil); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid JSON status = %d", w.Code)
		}
	}
	w := call(t, h, http.MethodPost, Prefix+"/login", `{"email":"x","password":"`+strings.Repeat("x", 5000)+`"}`, nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("limit status = %d", w.Code)
	}
	for _, body := range []string{`{"email":"admin@example.invalid","password":"wrong"}`, `{"email":"missing@example.invalid","password":"wrong"}`, `{"email":"disabled@example.invalid","password":"Demo-member-2026!"}`} {
		w := call(t, h, http.MethodPost, Prefix+"/login", body, nil)
		if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), `"code":"invalid_credentials"`) || len(w.Result().Cookies()) != 0 {
			t.Fatal("credential failure must be generic and set no cookie")
		}
	}
	if call(t, h, http.MethodPost, Prefix+"/login", "", nil).Code != http.StatusUnsupportedMediaType {
		t.Fatal("missing content type not rejected")
	}
}

func TestSeparateInstancesCannotUseEachOthersSession(t *testing.T) {
	a, err := NewDemo(Options{Instance: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewDemo(Options{Instance: "b"})
	if err != nil {
		t.Fatal(err)
	}
	cookie := loginAs(t, a, false)
	if call(t, b, http.MethodGet, Prefix+"/users", "", cookie).Code != http.StatusUnauthorized {
		t.Fatal("session crossed recovery instance")
	}
}
