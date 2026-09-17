package daemon

import (
	"encoding/json"
	"net/http"
)

// Handler reports process liveness, never business or control-session readiness.
// It contains no identity paths, addresses, certificates or slot observations.
func Handler() http.Handler {
	mux := http.NewServeMux()
	write := func(status int, state string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(struct {
				Status          string `json:"status"`
				Mode            string `json:"mode"`
				ProductionReady bool   `json:"production_ready"`
			}{state, "lifecycle_only", false})
		}
	}
	mux.HandleFunc("GET /healthz", write(http.StatusOK, "running"))
	mux.HandleFunc("GET /readyz", write(http.StatusServiceUnavailable, "business_runtime_disabled"))
	return mux
}
