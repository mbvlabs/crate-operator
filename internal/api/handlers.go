// Package api provides HTTP handlers for the API server.
package api

import (
	"encoding/json"
	"net/http"
)

type APIHandler struct {
	version string
}

func NewAPIHandler(version string) *APIHandler {
	return &APIHandler{
		version: version,
	}
}

// GetHealth implements ServerInterface.
func (h *APIHandler) GetHealth(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		Status:  Healthy,
		Version: h.version,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Ensure APIHandler implements ServerInterface
var _ ServerInterface = (*APIHandler)(nil)
