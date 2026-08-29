// Command networkprobe is included only in the local Docker E2E image. It
// reports whether a literal TCP endpoint is reachable without using proxy
// environment variables.
package main

import (
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"time"
)

type result struct {
	Reachable bool `json:"reachable"`
}

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	endpoint, err := netip.ParseAddrPort(os.Args[1])
	if err != nil || !endpoint.Addr().IsValid() || endpoint.Port() == 0 {
		os.Exit(2)
	}
	connection, err := net.DialTimeout("tcp", endpoint.String(), 2*time.Second)
	reachable := err == nil
	if connection != nil {
		_ = connection.Close()
	}
	if json.NewEncoder(os.Stdout).Encode(result{Reachable: reachable}) != nil {
		os.Exit(2)
	}
}
