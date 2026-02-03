package api

import "net/http"

// CreateDockerApp implements ServerInterface.
func (h *APIHandler) CreateDockerApp(w http.ResponseWriter, r *http.Request) {
	writeDeployResponse(
		w,
		http.StatusNotImplemented,
		"error",
		"docker app creation not yet implemented",
		"",
	)
}

// DeployDockerApp implements ServerInterface.
func (h *APIHandler) DeployDockerApp(w http.ResponseWriter, r *http.Request) {
	writeDeployResponse(
		w,
		http.StatusNotImplemented,
		"error",
		"docker app deployment not yet implemented",
		"",
	)
}
