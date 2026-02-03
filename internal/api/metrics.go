package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// GetNodeMetrics implements ServerInterface.
func (h *APIHandler) GetNodeMetrics(w http.ResponseWriter, r *http.Request) {
	queries := []string{
		"node_memory_MemAvailable_bytes",
		"node_memory_MemTotal_bytes",
		`node_filesystem_avail_bytes{mountpoint="/"}`,
		`node_filesystem_size_bytes{mountpoint="/"}`,
		`node_cpu_seconds_total{mode="idle"}`,
	}

	result := make(map[string]any)

	for _, query := range queries {
		queryURL := fmt.Sprintf(
			"http://localhost:9090/api/v1/query?query=%s",
			url.QueryEscape(query),
		)
		resp, err := http.Get(queryURL)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": "prometheus not available"})
			return
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).
				Encode(map[string]string{"error": "failed to read prometheus response"})
			return
		}

		var queryResult map[string]any
		if err := json.Unmarshal(body, &queryResult); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).
				Encode(map[string]string{"error": "failed to parse prometheus response"})
			return
		}

		result[query] = queryResult
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}
