package providerlifecycle

import (
	"os"
	"testing"
)

func TestLiveDockerSkippedWithoutExplicitOptIn(t *testing.T) {
	if os.Getenv("EXECUTION_PROVIDER_LIFECYCLE_DOCKER") == "1" {
		t.Skip("live Docker dual-instance mTLS is a dedicated Linux gate; this process only records opt-in")
	}
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		t.Log("docker.sock is present; refusing to create networks or containers without EXECUTION_PROVIDER_LIFECYCLE_DOCKER=1")
	}
}
