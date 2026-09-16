package fixedtransport_test

import (
	"net/url"
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/provider"
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/worker/fixedtransport"
)

func TestProviderAndWorkerAgreeOnFixedProxyOrigin(t *testing.T) {
	const base = "http://host-agent.execution.internal:18080"
	for _, raw := range []string{base, base + "/", "http://host-agent.execution.internal:1", "http://host-agent.execution.internal:65535"} {
		policy := provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: raw}
		if policy.Validate() != nil || fixedtransport.ValidateProxyURL(raw) != nil {
			t.Fatal("provider and worker rejected a supported internal proxy origin")
		}
	}
	credentialURL, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	credentialURL.User = url.UserPassword("synthetic-user", "synthetic-password")
	for _, raw := range []string{
		"", base + "?", base + "#", base + "/%2f", base + "//", base + "/path", base + "?extra=1", base + "#extra",
		"http://host-agent.execution.internal:0", "http://host-agent.execution.internal:65536", "http://host-agent.execution.internal:018080",
		"http://host-agent.execution.internal:", "http://host-agent.execution.internal", "http://[host-agent.execution.internal]:18080",
		"https://host-agent.execution.internal:18080", "http://localhost:18080", "http://host-agent.execution.internal.:18080",
		"http://host-agent.execution.internal.attacker.test:18080", credentialURL.String(), " " + base, base + " ", base + "\n",
	} {
		policy := provider.NetworkPolicy{DenyDirectInternet: true, EgressProxyEndpoint: raw}
		if policy.Validate() == nil || fixedtransport.ValidateProxyURL(raw) == nil {
			t.Errorf("unsafe proxy origin accepted by provider or worker: %q", raw)
		}
	}
}
