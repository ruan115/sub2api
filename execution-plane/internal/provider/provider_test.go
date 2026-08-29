package provider

import (
	"strings"
	"testing"
)

func validSpec() SlotSpec {
	return SlotSpec{
		SlotID:      "slot-1",
		AccountID:   "account-1",
		Epoch:       1,
		ImageDigest: "registry.example/execution-worker@sha256:" + strings.Repeat("a", 64),
		Resources: ResourceLimits{
			CPUMilli:    500,
			MemoryBytes: 512 << 20,
			PIDs:        128,
			TmpfsBytes:  128 << 20,
		},
		Security: SecurityPolicy{
			RunAsUser:           65532,
			ReadOnlyRootFS:      true,
			NoNewPrivileges:     true,
			DropAllCapabilities: true,
			SeccompProfile:      "execution-worker",
			AppArmorProfile:     "execution-worker",
		},
		Network: NetworkPolicy{
			DenyDirectInternet:  true,
			EgressProxyEndpoint: "http://host-agent.execution.internal:18080",
		},
	}
}

func TestSlotSpecRejectsMutableOrUnsafeRuntime(t *testing.T) {
	spec := validSpec()
	if err := spec.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*SlotSpec)
	}{
		{"root", func(s *SlotSpec) { s.Security.RunAsUser = 0 }},
		{"writable root", func(s *SlotSpec) { s.Security.ReadOnlyRootFS = false }},
		{"direct internet", func(s *SlotSpec) { s.Network.DenyDirectInternet = false }},
		{"external egress endpoint", func(s *SlotSpec) { s.Network.EgressProxyEndpoint = "http://example.com:8080" }},
		{"egress credentials", func(s *SlotSpec) {
			s.Network.EgressProxyEndpoint = "http://user:pass@host-agent.execution.internal:18080"
		}},
		{"mutable image", func(s *SlotSpec) { s.ImageDigest = "" }},
		{"tagged image", func(s *SlotSpec) { s.ImageDigest = "registry.example/execution-worker:latest" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := validSpec()
			tt.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestNetworkPolicyRejectsEndpointSmuggling(t *testing.T) {
	for _, endpoint := range []string{
		"http://host-agent.execution.internal:18080/connect",
		"http://host-agent.execution.internal:18080/?target=example.com",
		"http://host-agent.execution.internal:18080/#fragment",
	} {
		t.Run(endpoint, func(t *testing.T) {
			policy := NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: endpoint}
			if err := policy.Validate(); err == nil || !strings.Contains(err.Error(), "origin") {
				t.Fatalf("expected origin-only endpoint rejection, got %v", err)
			}
		})
	}
}
