package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// RuntimeLeaseKeyPrefix is shared by execution-lease validators and future
// authoritative writers. Route publication is not a source of execution leases.
const RuntimeLeaseKeyPrefix = "execution:lease:v1:"

type RuntimeEnrollmentConfig struct {
	Enabled        bool
	LeaseRedisAddr string
	Timeout        time.Duration
}

func DefaultRuntimeEnrollmentConfig() RuntimeEnrollmentConfig {
	return RuntimeEnrollmentConfig{Timeout: 2 * time.Second}
}

func LoadRuntimeEnrollment(getenv func(string) string) (RuntimeEnrollmentConfig, error) {
	if getenv == nil {
		return RuntimeEnrollmentConfig{}, errors.New("runtime enrollment environment reader is required")
	}
	c := DefaultRuntimeEnrollmentConfig()
	var err error
	c.Enabled, err = parseStrictBool("EXECUTION_RUNTIME_ENROLLMENT_ENABLED", getenv("EXECUTION_RUNTIME_ENROLLMENT_ENABLED"))
	if err != nil {
		return RuntimeEnrollmentConfig{}, err
	}
	if !c.Enabled {
		return c, nil
	}
	// There is deliberately no fallback to EXECUTION_ROUTE_REDIS_ADDR.
	c.LeaseRedisAddr = getenv("EXECUTION_LEASE_REDIS_ADDR")
	if err := setDuration(getenv, "EXECUTION_RUNTIME_ENROLLMENT_TIMEOUT", &c.Timeout); err != nil {
		return RuntimeEnrollmentConfig{}, errors.New("runtime enrollment timeout is invalid")
	}
	if err := c.Validate(); err != nil {
		return RuntimeEnrollmentConfig{}, err
	}
	return c, nil
}

func (c RuntimeEnrollmentConfig) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Timeout <= 0 || c.Timeout > 5*time.Second || !validLeaseRedisAddress(c.LeaseRedisAddr) {
		return errors.New("runtime enrollment configuration is invalid")
	}
	return nil
}

func (c RuntimeEnrollmentConfig) String() string {
	return fmt.Sprintf("RuntimeEnrollmentConfig{Enabled:%t LeaseRedisConfigured:%t Timeout:%s}", c.Enabled, c.LeaseRedisAddr != "", c.Timeout)
}

func (c RuntimeEnrollmentConfig) GoString() string { return c.String() }

func (c RuntimeEnrollmentConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Enabled              bool          `json:"enabled"`
		LeaseRedisConfigured bool          `json:"lease_redis_configured"`
		Timeout              time.Duration `json:"timeout"`
	}{c.Enabled, c.LeaseRedisAddr != "", c.Timeout})
}

func validLeaseRedisAddress(address string) bool {
	if address == "" || len(address) > 260 || strings.TrimSpace(address) != address || strings.ContainsAny(address, "\x00\r\n\t /@?#%") {
		return false
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return false
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsPrivate() || ip.IsLoopback()
	}
	// Private DNS names are allowed, but URLs, userinfo and arbitrary dial
	// schemes are not. Deployment networking remains responsible for DNS/ACLs.
	if len(host) > 253 || strings.Contains(host, ":") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if c < 'a' || c > 'z' {
				if c < '0' || c > '9' {
					if c != '-' {
						return false
					}
				}
			}
		}
	}
	return true
}
