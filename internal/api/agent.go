package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
)

const agentArtifactSource = "https://crate-operator.deploycrate.com/version"

// UpdateAgent implements ServerInterface.
func (h *APIHandler) UpdateAgent(w http.ResponseWriter, r *http.Request) {
	var req UpdateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeUpdateResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("invalid request body: %v", err),
		)
		return
	}

	if strings.TrimSpace(req.ArtifactVersion) == "" {
		writeUpdateResponse(
			w,
			http.StatusBadRequest,
			"error",
			"artifact_version is required",
		)
		return
	}

	requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	deployReq := AgentDeployRequest{Version: req.ArtifactVersion}
	if requestID != "" {
		deployReq.RequestID = &requestID
	}
	if req.OverrideArtifactSource != nil && strings.TrimSpace(*req.OverrideArtifactSource) != "" {
		deployReq.OverrideArtifactSource = req.OverrideArtifactSource
	}

	status, _, err := h.agentDeployMgr.Trigger(deployReq)
	if err != nil {
		writeUpdateResponse(w, http.StatusBadRequest, "error", err.Error())
		return
	}

	msg := fmt.Sprintf("agent update initiated (deployment_id=%s)", status.DeploymentID)
	writeUpdateResponse(w, http.StatusAccepted, "update_initiated", msg)
}

// GetHealth implements ServerInterface.
func (h *APIHandler) GetHealth(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		Status:  Healthy,
		Version: h.version,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *APIHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/agent/deploy", h.handleAgentDeploy)
	mux.HandleFunc("GET /v1/agent/deploy/{deployment_id}", h.handleAgentDeploymentStatus)
	mux.HandleFunc("GET /readyz", h.handleReadyz)
}

func (h *APIHandler) handleAgentDeploy(w http.ResponseWriter, r *http.Request) {
	var req AgentDeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	status, reused, err := h.agentDeployMgr.Trigger(req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	state := status.State
	if !reused && state == "queued" {
		state = "queued"
	}
	writeJSON(w, http.StatusAccepted, AgentDeployResponse{
		DeploymentID: status.DeploymentID,
		State:        state,
	})
}

func (h *APIHandler) handleAgentDeploymentStatus(w http.ResponseWriter, r *http.Request) {
	deploymentID := strings.TrimSpace(r.PathValue("deployment_id"))
	if deploymentID == "" {
		writeJSONError(w, http.StatusBadRequest, "deployment_id is required")
		return
	}

	status, ok := h.agentDeployMgr.Get(deploymentID)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "deployment not found")
		return
	}

	writeJSON(w, http.StatusOK, status)
}

func (h *APIHandler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Stat("/opt/deploy-crate/agent"); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("agent path unavailable: %v", err))
		return
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("systemctl unavailable: %v", err))
		return
	}
	if _, err := NewCaddyManager().GetAgentRouteState(); err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("caddy route unavailable: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func agentBinaryName() string {
	return "crate-operator-linux-amd64"
}

func writeJSON(w http.ResponseWriter, statusCode int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	writeJSON(w, statusCode, map[string]string{"error": message})
}
