package worker

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Process URL validation is stricter than NewOnboarder's injectable library
// contract: only the existing explicit fake process may send plaintext HTTP.
func validProcessURL(u *url.URL, origin, allowHTTP bool) bool {
	if u == nil || (u.Scheme != "https" && !(allowHTTP && u.Scheme == "http")) ||
		u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		u.RawPath != "" || strings.ContainsAny(u.Path, "\\\x00\r\n\t ") || (u.Path != "" && !strings.HasPrefix(u.Path, "/")) {
		return false
	}
	if origin && u.Path != "" && u.Path != "/" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		if len(host) > 253 {
			return false
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return false
			}
			for _, c := range label {
				if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-') {
					return false
				}
			}
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		return err == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == port && u.Host == net.JoinHostPort(host, port)
	}
	if ip != nil && strings.Contains(host, ":") {
		return u.Host == "["+host+"]"
	}
	return u.Host == host
}

func parseProcessURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	// A trailing '?' or '#' is still forbidden, even with an empty value.
	if strings.ContainsAny(raw, "?#") || u.String() != raw {
		return nil, errInvalidProcessURL
	}
	return u, nil
}
