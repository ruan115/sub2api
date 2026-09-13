// Package app assembles a loopback-only development demo. Nothing here is wired
// into the production server, databases, shared Redis or execution accounts.
package app

import (
	"net/http"
	"net/url"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/portunex/apikeys"
	"github.com/Wei-Shaw/sub2api/internal/portunex/identity"
	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/listing"
	"github.com/Wei-Shaw/sub2api/internal/portunex/platform/namespace"
	"github.com/Wei-Shaw/sub2api/internal/portunex/providers"
	"github.com/Wei-Shaw/sub2api/internal/portunex/users"
)

const Prefix = "/__recovery__/v1"
const CookieName = "portunex_recovery_session"

type Options struct {
	Instance string
	Now      func() time.Time
}

type Demo struct {
	identity  *identity.Service
	users     users.Catalog
	keys      apikeys.Catalog
	providers providers.Catalog
}

func NewDemo(options Options) (*Demo, error) {
	if options.Instance == "" {
		options.Instance = "local-demo"
	}
	space, err := namespace.New(options.Instance)
	if err != nil {
		return nil, err
	}
	auth, err := identity.NewDemo(space, options.Now)
	if err != nil {
		return nil, err
	}
	return &Demo{identity: auth, users: demoUsers(), keys: demoKeys(), providers: demoProviders()}, nil
}

func (d *Demo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Recovery-Mode", "synthetic")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !localRequest(r) {
		writeError(w, http.StatusForbidden, "local_only", "仅允许同源本地演示请求")
		return
	}
	path := r.URL.Path
	wantedMethod := http.MethodGet
	switch path {
	case Prefix + "/login", Prefix + "/logout":
		wantedMethod = http.MethodPost
	case Prefix + "/session", Prefix + "/users", Prefix + "/api-keys", Prefix + "/providers":
	default:
		writeError(w, http.StatusNotFound, "not_found", "此演示未实现该接口")
		return
	}
	if r.Method != wantedMethod {
		w.Header().Set("Allow", wantedMethod)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "请求方法不支持")
		return
	}
	if path == Prefix+"/login" {
		d.login(w, r)
		return
	}
	if path == Prefix+"/logout" {
		d.logout(w, r)
		return
	}
	session, err := d.identity.Authenticate(cookieToken(r))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "请重新登录本地演示")
		return
	}
	if path == Prefix+"/session" {
		writeJSON(w, http.StatusOK, session)
		return
	}
	admin := session.User.Role == identity.Admin
	if !admin && path != Prefix+"/api-keys" {
		writeError(w, http.StatusForbidden, "forbidden", "需要演示管理员权限")
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "查询或分页参数无效")
		return
	}
	query, err := listing.Parse(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query", "查询或分页参数无效")
		return
	}
	switch path {
	case Prefix + "/users":
		writeJSON(w, http.StatusOK, d.users.List(query))
	case Prefix + "/api-keys":
		writeJSON(w, http.StatusOK, d.keys.List(query, session.User.ID, admin))
	case Prefix + "/providers":
		writeJSON(w, http.StatusOK, d.providers.List(query))
	}
}
