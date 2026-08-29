package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Role string

const (
	RoleOrchestrator Role = "orchestrator"
	RoleHostAgent    Role = "host-agent"
	RoleWorker       Role = "worker"
)

type Timings struct {
	NodeHeartbeat time.Duration
	NodeOffline   time.Duration
	EpochFailover time.Duration
	QueueWait     time.Duration
	Execution     time.Duration
	SessionIdle   time.Duration
	ToolWait      time.Duration
	Sticky        time.Duration
}

type Limits struct {
	MaxSlots          int
	MaxActiveCLI      int
	MaxActiveAPI      int
	MaxActiveTotal    int
	GlobalOutstanding int
}

type Config struct {
	Role          Role
	NodeID        string
	ListenAddress string
	Timings       Timings
	Limits        Limits
}

func Default(role Role) Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "unknown-node"
	}

	address := "127.0.0.1:8091"
	switch role {
	case RoleHostAgent:
		address = "127.0.0.1:8092"
	case RoleWorker:
		address = "0.0.0.0:8093"
	}

	return Config{
		Role:          role,
		NodeID:        hostname,
		ListenAddress: address,
		Timings: Timings{
			NodeHeartbeat: 15 * time.Second,
			NodeOffline:   45 * time.Second,
			EpochFailover: 90 * time.Second,
			QueueWait:     120 * time.Second,
			Execution:     15 * time.Minute,
			SessionIdle:   15 * time.Minute,
			ToolWait:      15 * time.Minute,
			Sticky:        15 * time.Minute,
		},
		Limits: Limits{
			MaxSlots:          20,
			MaxActiveCLI:      4,
			MaxActiveAPI:      12,
			MaxActiveTotal:    12,
			GlobalOutstanding: 1000,
		},
	}
}

// Load reads the small bootstrap configuration shared by the three binaries.
// Service-specific database, Redis, KMS and mTLS configuration will live in
// dedicated structs so that credentials cannot accidentally bleed across roles.
func Load(role Role, getenv func(string) string) (Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	cfg := Default(role)

	if value := strings.TrimSpace(getenv("EXECUTION_NODE_ID")); value != "" {
		cfg.NodeID = value
	}
	if value := strings.TrimSpace(getenv("EXECUTION_LISTEN_ADDRESS")); value != "" {
		cfg.ListenAddress = value
	}

	durations := []struct {
		name   string
		target *time.Duration
	}{
		{"EXECUTION_NODE_HEARTBEAT", &cfg.Timings.NodeHeartbeat},
		{"EXECUTION_NODE_OFFLINE", &cfg.Timings.NodeOffline},
		{"EXECUTION_EPOCH_FAILOVER", &cfg.Timings.EpochFailover},
		{"EXECUTION_QUEUE_WAIT", &cfg.Timings.QueueWait},
		{"EXECUTION_TIMEOUT", &cfg.Timings.Execution},
		{"EXECUTION_SESSION_IDLE", &cfg.Timings.SessionIdle},
		{"EXECUTION_TOOL_WAIT", &cfg.Timings.ToolWait},
		{"EXECUTION_STICKY_TTL", &cfg.Timings.Sticky},
	}
	for _, field := range durations {
		if err := setDuration(getenv, field.name, field.target); err != nil {
			return Config{}, err
		}
	}

	integers := []struct {
		name   string
		target *int
	}{
		{"EXECUTION_MAX_SLOTS", &cfg.Limits.MaxSlots},
		{"EXECUTION_MAX_ACTIVE_CLI", &cfg.Limits.MaxActiveCLI},
		{"EXECUTION_MAX_ACTIVE_API", &cfg.Limits.MaxActiveAPI},
		{"EXECUTION_MAX_ACTIVE_TOTAL", &cfg.Limits.MaxActiveTotal},
		{"EXECUTION_GLOBAL_OUTSTANDING", &cfg.Limits.GlobalOutstanding},
	}
	for _, field := range integers {
		if err := setPositiveInt(getenv, field.name, field.target); err != nil {
			return Config{}, err
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	switch c.Role {
	case RoleOrchestrator, RoleHostAgent, RoleWorker:
	default:
		return fmt.Errorf("unsupported execution role %q", c.Role)
	}
	if strings.TrimSpace(c.NodeID) == "" {
		return errors.New("node id is required")
	}
	if err := validateListenAddress(c.ListenAddress); err != nil {
		return err
	}
	if c.Timings.NodeHeartbeat <= 0 {
		return errors.New("node heartbeat must be positive")
	}
	if c.Timings.NodeOffline < 3*c.Timings.NodeHeartbeat {
		return errors.New("node offline threshold must be at least three heartbeats")
	}
	if c.Timings.EpochFailover < c.Timings.NodeOffline+c.Timings.NodeHeartbeat {
		return errors.New("epoch failover must leave at least one heartbeat after node offline")
	}
	if c.Timings.QueueWait <= 0 || c.Timings.Execution <= 0 || c.Timings.SessionIdle <= 0 || c.Timings.ToolWait <= 0 || c.Timings.Sticky <= 0 {
		return errors.New("queue, execution, session, tool and sticky durations must be positive")
	}
	if c.Limits.MaxSlots <= 0 || c.Limits.MaxActiveCLI <= 0 || c.Limits.MaxActiveAPI <= 0 || c.Limits.MaxActiveTotal <= 0 || c.Limits.GlobalOutstanding <= 0 {
		return errors.New("all concurrency limits must be positive")
	}
	if c.Limits.MaxActiveCLI > c.Limits.MaxActiveTotal {
		return errors.New("CLI concurrency cannot exceed total concurrency")
	}
	if c.Limits.MaxActiveAPI > c.Limits.MaxActiveTotal {
		return errors.New("API concurrency cannot exceed total concurrency")
	}
	if c.Limits.MaxActiveTotal > c.Limits.MaxSlots {
		return errors.New("total concurrency cannot exceed slot capacity")
	}
	if c.Limits.GlobalOutstanding < c.Limits.MaxActiveTotal {
		return errors.New("global outstanding limit cannot be lower than node active capacity")
	}
	return nil
}

func setDuration(getenv func(string) string, name string, target *time.Duration) error {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*target = parsed
	return nil
}

func setPositiveInt(getenv func(string) string, name string, target *int) error {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	*target = parsed
	return nil
}

func validateListenAddress(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("listen address is required")
	}
	_, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("listen address: %w", err)
	}
	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return fmt.Errorf("listen address contains invalid port %q", port)
	}
	return nil
}
