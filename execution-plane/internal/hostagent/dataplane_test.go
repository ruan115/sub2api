package hostagent

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/execution-plane/internal/route"
)

func TestMergeDataplaneEndpointAdvertisesPrivateAddress(t *testing.T) {
	labels, err := mergeDataplaneEndpoint(map[string]string{"region": "ap-shanghai"}, "10.8.0.12:8091")
	if err != nil || labels[route.DataplaneEndpointKey] != "10.8.0.12:8091" || labels["region"] != "ap-shanghai" {
		t.Fatalf("labels = %+v err=%v", labels, err)
	}
}

func TestMergeDataplaneEndpointRejectsPublicAddress(t *testing.T) {
	if _, err := mergeDataplaneEndpoint(nil, "8.8.8.8:8091"); err == nil {
		t.Fatal("public dataplane endpoint was accepted")
	}
	if _, err := mergeDataplaneEndpoint(map[string]string{route.DataplaneEndpointKey: "1.1.1.1:443"}, ""); err == nil {
		t.Fatal("public label endpoint was accepted")
	}
}
