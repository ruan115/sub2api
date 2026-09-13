package hostagent

import (
	"github.com/Wei-Shaw/sub2api/execution-plane/internal/route"
)

func mergeDataplaneEndpoint(labels map[string]string, endpoint string) (map[string]string, error) {
	merged := make(map[string]string, len(labels)+1)
	for key, value := range labels {
		merged[key] = value
	}
	if endpoint != "" {
		merged[route.DataplaneEndpointKey] = endpoint
	}
	advertised, exists := merged[route.DataplaneEndpointKey]
	if !exists {
		return merged, nil
	}
	validated, err := route.EndpointFromLabels(map[string]string{route.DataplaneEndpointKey: advertised})
	if err != nil {
		return nil, err
	}
	merged[route.DataplaneEndpointKey] = validated
	return merged, nil
}
