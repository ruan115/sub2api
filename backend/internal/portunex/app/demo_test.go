package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/portunex/apikeys"
	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/listing"
)

func TestAdminLoginAndThreeLists(t *testing.T) {
	h := demo(t)
	cookie := loginAs(t, h, false)
	for _, resource := range []string{"users", "api-keys", "providers"} {
		t.Run(resource, func(t *testing.T) {
			w := call(t, h, http.MethodGet, Prefix+"/"+resource+"?page=1&page_size=2", "", cookie)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			var result listing.Page[json.RawMessage]
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Total != 3 || len(result.Items) != 2 || result.PageSize != 2 || result.Page != 1 {
				t.Fatalf("wrong page: %+v", result)
			}
			if w.Header().Get("X-Recovery-Mode") != "synthetic" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("missing demo/cache guards")
			}
			for _, forbidden := range []string{"password", "credential", "access_token", cookie.Value} {
				if strings.Contains(w.Body.String(), forbidden) {
					t.Fatal("sensitive field in list")
				}
			}
		})
	}
}

func TestAnonymousAndMemberBoundaries(t *testing.T) {
	h := demo(t)
	for _, resource := range []string{"users", "api-keys", "providers", "session"} {
		if w := call(t, h, http.MethodGet, Prefix+"/"+resource, "", nil); w.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s = %d", resource, w.Code)
		}
	}
	cookie := loginAs(t, h, true)
	for _, resource := range []string{"users", "providers"} {
		if w := call(t, h, http.MethodGet, Prefix+"/"+resource, "", cookie); w.Code != http.StatusForbidden {
			t.Fatalf("member %s = %d", resource, w.Code)
		}
	}
	w := call(t, h, http.MethodGet, Prefix+"/api-keys", "", cookie)
	var result listing.Page[apikeys.Key]
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || result.Total != 1 || len(result.Items) != 1 || result.Items[0].Owner != "u-member" {
		t.Fatal("member key isolation failed")
	}
	w = call(t, h, http.MethodGet, Prefix+"/api-keys?query=u-admin", "", cookie)
	if !strings.Contains(w.Body.String(), `"total":0`) {
		t.Fatal("search leaks another owner")
	}
}

func TestListQueryBoundariesAndEmptyResults(t *testing.T) {
	h := demo(t)
	cookie := loginAs(t, h, false)
	for _, query := range []string{"page=0", "page=-1", "page=1000001", "page_size=101", "page_size=x", "page=1&page=2", "unknown=x", "page=%zz", "query=%00"} {
		if w := call(t, h, http.MethodGet, Prefix+"/users?"+query, "", cookie); w.Code != http.StatusBadRequest {
			t.Errorf("query %s = %d", query, w.Code)
		}
	}
	for _, query := range []string{"query=does-not-exist", "page=100"} {
		w := call(t, h, http.MethodGet, Prefix+"/users?"+query, "", cookie)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"items":[]`) {
			t.Fatal("empty results must be a JSON array")
		}
	}
}

func TestOldRoutesAndWrongMethodsAreNotCompatibility(t *testing.T) {
	h := demo(t)
	for _, path := range []string{"/portunex/auth/login", "/portunex/admin/users", "/v1/messages"} {
		if w := call(t, h, http.MethodPost, path, `{}`, nil); w.Code != http.StatusNotFound {
			t.Fatalf("old route accepted: %s", path)
		}
	}
	w := call(t, h, http.MethodGet, Prefix+"/login", "", nil)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "POST" {
		t.Fatal("method guard missing")
	}
}
