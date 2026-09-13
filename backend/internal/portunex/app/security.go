package app

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Do not trust forwarded headers. Host checks also block DNS rebinding against
// a loopback listener. Vite's local proxy must preserve Host and Origin.
func localRequest(r *http.Request) bool {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !loopbackIP(peer) || !localHost(r.Host) {
		return false
	}
	if len(r.Header.Values("Origin")) > 1 {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "http" || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return false
		}
	}
	return true
}

func loopbackIP(value string) bool {
	ip := net.ParseIP(value)
	return ip != nil && ip.IsLoopback()
}

func localHost(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		if strings.ContainsAny(value, ":/[]@") {
			return false
		}
		host = value
	} else {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	return host == "localhost" || loopbackIP(host)
}

func cookieToken(r *http.Request) string {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}
