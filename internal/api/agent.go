package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

const agentArtifactSource = "https://mithlond-agent.mbvlabs.com/version"

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
			"artifact_source and artifact_version are required",
		)
		return
	}

	writeUpdateResponse(w, http.StatusAccepted, "update_initiated", "agent update initiated")

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		binaryName := agentBinaryName()
		tempFile, err := os.CreateTemp(agentInstallDir(), "mithlond-agent.*.tmp")
		if err != nil {
			slog.Error("failed to create temp file", "error", err)
			return
		}

		tempPath := tempFile.Name()
		success := false
		defer func() {
			if !success {
				_ = os.Remove(tempPath)
			}
		}()

		if err := tempFile.Close(); err != nil {
			slog.Error("failed to close temp file", "error", err)
			return
		}

		var binaryURL, checksumURL string
		if req.OverrideArtifactSource != nil && *req.OverrideArtifactSource != "" {
			binaryURL, checksumURL, err = buildArtifactURLs(
				*req.OverrideArtifactSource,
				req.ArtifactVersion,
				binaryName,
			)
		} else {
			binaryURL, checksumURL, err = buildArtifactURLs(
				agentArtifactSource,
				req.ArtifactVersion,
				binaryName,
			)
		}
		if err != nil {
			slog.Error("failed to build artifact URLs", "error", err)
			return
		}

		if err := downloadToFile(ctx, binaryURL, tempPath); err != nil {
			slog.Error("failed to download binary", "error", err)
			return
		}

		checksumBytes, err := fetchBytes(ctx, checksumURL)
		if err != nil {
			slog.Error("failed to download checksum", "error", err)
			return
		}

		if err := verifyChecksum(tempPath, string(checksumBytes)); err != nil {
			slog.Error("checksum verification failed", "error", err)
			return
		}

		if err := os.Chmod(tempPath, 0o755); err != nil {
			slog.Error("failed to chmod binary", "error", err)
			return
		}

		if err := installAgentBinary(tempPath); err != nil {
			slog.Error("failed to install new agent binary", "error", err)
			return
		}

		success = true
		slog.Info("agent update successful, restarting to new version")
	}()
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

func agentBinaryName() string {
	return "mithlond-agent-linux-amd64"
}

func agentInstallDir() string {
	return "/opt/mithlond-agent"
}
