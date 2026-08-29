package config

import (
	"testing"
	"time"
)

func TestDefaultMatchesPilotEnvelope(t *testing.T) {
	cfg := Default(RoleHostAgent)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config is invalid: %v", err)
	}
	if cfg.Limits.MaxSlots != 20 || cfg.Limits.MaxActiveCLI != 4 || cfg.Limits.MaxActiveAPI != 12 || cfg.Limits.MaxActiveTotal != 12 {
		t.Fatalf("unexpected pilot limits: %+v", cfg.Limits)
	}
	if cfg.Timings.QueueWait != 120*time.Second || cfg.Timings.Execution != 15*time.Minute {
		t.Fatalf("unexpected request timings: %+v", cfg.Timings)
	}
}

func TestLoadOverridesAndValidates(t *testing.T) {
	env := map[string]string{
		"EXECUTION_NODE_ID":          "srv74",
		"EXECUTION_LISTEN_ADDRESS":   "127.0.0.1:18092",
		"EXECUTION_MAX_SLOTS":        "40",
		"EXECUTION_MAX_ACTIVE_TOTAL": "16",
		"EXECUTION_MAX_ACTIVE_API":   "16",
		"EXECUTION_QUEUE_WAIT":       "30s",
	}
	cfg, err := Load(RoleHostAgent, func(key string) string { return env[key] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NodeID != "srv74" || cfg.Limits.MaxSlots != 40 || cfg.Timings.QueueWait != 30*time.Second {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

func TestRejectsUnsafeTimingAndConcurrency(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"offline too early", func(c *Config) { c.Timings.NodeOffline = 20 * time.Second }},
		{"failover too early", func(c *Config) { c.Timings.EpochFailover = 50 * time.Second }},
		{"CLI over total", func(c *Config) { c.Limits.MaxActiveCLI = 13 }},
		{"total over slots", func(c *Config) { c.Limits.MaxActiveTotal = 21; c.Limits.MaxActiveAPI = 21 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default(RoleOrchestrator)
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
