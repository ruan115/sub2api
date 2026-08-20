package main

import "testing"

func TestParseProxyLineFormats(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		protocol string
		host     string
		port     int
		username string
		password string
	}{
		{name: "host port", line: "proxy.example.com:8080", protocol: "socks5", host: "proxy.example.com", port: 8080},
		{name: "host first colon", line: "proxy.example.com:8080:alice:secret", protocol: "socks5", host: "proxy.example.com", port: 8080, username: "alice", password: "secret"},
		{name: "user first colon", line: "alice:secret:proxy.example.com:8080", protocol: "socks5", host: "proxy.example.com", port: 8080, username: "alice", password: "secret"},
		{name: "user first at", line: "alice:pa:ss@proxy.example.com:8080", protocol: "socks5", host: "proxy.example.com", port: 8080, username: "alice", password: "pa:ss"},
		{name: "host first at", line: "proxy.example.com:8080@alice:pa:ss", protocol: "socks5", host: "proxy.example.com", port: 8080, username: "alice", password: "pa:ss"},
		{name: "http url", line: "http://alice:p%40ss@proxy.example.com:3128", protocol: "http", host: "proxy.example.com", port: 3128, username: "alice", password: "p@ss"},
		{name: "socks5h url", line: "socks5h://alice:secret@127.0.0.1:1080", protocol: "socks5", host: "127.0.0.1", port: 1080, username: "alice", password: "secret"},
		{name: "ipv6", line: "[2001:db8::1]:1080", protocol: "socks5", host: "2001:db8::1", port: 1080},
		{name: "ipv6 auth", line: "alice:secret@[2001:db8::1]:1080", protocol: "socks5", host: "2001:db8::1", port: 1080, username: "alice", password: "secret"},
		{name: "reverse url authority", line: "https://proxy.example.com:8443@alice:secret", protocol: "https", host: "proxy.example.com", port: 8443, username: "alice", password: "secret"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			item, err := parseProxyLine(test.line, "socks5")
			if err != nil {
				t.Fatal(err)
			}
			if item.Protocol != test.protocol || item.Host != test.host || item.Port != test.port || item.Username != test.username || item.Password != test.password {
				t.Fatalf("parseProxyLine(%q) = %+v", test.line, item)
			}
		})
	}
}

func TestParseProxyLineRejectsInvalidValues(t *testing.T) {
	for _, line := range []string{"", "proxy.example.com", "proxy.example.com:0", "proxy.example.com:70000", "ftp://proxy.example.com:21", "alice@proxy.example.com:8080"} {
		if item, err := parseProxyLine(line, "socks5"); err == nil {
			t.Fatalf("parseProxyLine(%q) = %+v, want error", line, item)
		}
	}
}

func TestProxyTextFromAPIEncodesCredentials(t *testing.T) {
	body := []byte(`{"data":[{"host":"proxy.example.com","port":8080,"protocol":"http","username":"a@b","password":"p:a ss"}]}`)
	item, err := parseProxyLine(proxyTextFromAPI(body), "socks5")
	if err != nil {
		t.Fatal(err)
	}
	if item.Protocol != "http" || item.Username != "a@b" || item.Password != "p:a ss" {
		t.Fatalf("API proxy = %+v", item)
	}
}
