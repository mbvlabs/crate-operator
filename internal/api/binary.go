package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	z "github.com/Oudwins/zog"
	"github.com/rs/xid"
)

var CreateBinaryAppRequestSchema = z.Struct(z.Shape{
	"teamId": z.String().
		Required(z.Message("Team ID must be provided")).
		UUID(z.Message("Team ID must be a valid UUID v4")),
	"deploymentId": z.String().
		Required(z.Message("DeploymentID must be provided")).
		UUID(z.Message("Deployment ID must be a valid UUID v4")),
	"assetUrl": z.String().
		Required(z.Message("Asset url must be provided")).
		URL().
		Max(2000, z.Message("Asset URL must be between 1 and 2000 characters")),
	"appId": z.String().
		Required(z.Message("App ID must be provided")).
		UUID(z.Message("App ID must be a valid UUID v4")),
	"artifactName": z.String().
		Required(z.Message("Artifact name must be provided")).
		Min(1, z.Message("Artifact name must be at least 1 character")).
		Max(200, z.Message("Artifact name must be between 1 and 200 characters")),
	"artifactSource": z.String().
		Required(z.Message("Artifact source must be provided")).
		Min(1, z.Message("Artifact source must be at least 1 character")).
		Max(2000, z.Message("Artifact source must be between 1 and 2000 characters")),
	"artifactVersion": z.String().
		Required(z.Message("Artifact version must be provided")).
		Min(1, z.Message("Artifact version must be at least 1 character")).
		Max(100, z.Message("Artifact version must be between 1 and 100 characters")),
	"environmentId": z.String().
		Required(z.Message("Environment id must be provided")).
		UUID(z.Message("Environment ID must be a valid UUID v4")),
	"callbackUrl": z.String().
		Required(z.Message("Callback URL must be provided")).
		URL().
		Max(2000, z.Message("Callback URL must be between 1 and 2000 characters")),
	"domain": z.String().
		Optional().
		Min(1, z.Message("Domain must be at least 1 character")).
		Max(253, z.Message("Domain must be between 1 and 253 characters")),
	"port": z.Int().
		Required(z.Message("Port must be provided")).
		GT(0).
		LT(65536).
		Not().OneOf([]int{80, 443, 9640}, z.Message("Ports 80, 443, and 9640 are reserved")),
})

const (
	CreateBinaryAppAction = "create_binary_app"
	DeployBinaryAppAction = "deploy_binary_app"
)

var DeployBinaryAppRequestSchema = CreateBinaryAppRequestSchema

func internalBinaryName(environmentID string) string {
	environmentParts := strings.Split(environmentID, "-")
	return fmt.Sprintf("%s-%s-app", environmentParts[0], environmentParts[1])
}

func installBinaryWithBackup(ctx context.Context, binaryURL, binaryPath string) error {
	backupBinaryPath := binaryPath + ".backup"
	tempBinaryPath := fmt.Sprintf("%s.tmp-%s", binaryPath, xid.New().String())

	if _, err := os.Stat(binaryPath); err == nil {
		if err := os.Rename(binaryPath, backupBinaryPath); err != nil {
			return fmt.Errorf("failed to backup binary: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat binary: %w", err)
	}

	if err := downloadToFile(ctx, binaryURL, tempBinaryPath); err != nil {
		_ = os.Remove(tempBinaryPath)
		if _, restoreErr := os.Stat(backupBinaryPath); restoreErr == nil {
			_ = os.Rename(backupBinaryPath, binaryPath)
		}
		return fmt.Errorf("failed to download binary: %w", err)
	}

	if err := os.Chmod(tempBinaryPath, 0o755); err != nil {
		_ = os.Remove(tempBinaryPath)
		if _, restoreErr := os.Stat(backupBinaryPath); restoreErr == nil {
			_ = os.Rename(backupBinaryPath, binaryPath)
		}
		return fmt.Errorf("failed to chmod binary: %w", err)
	}

	source, err := os.Open(tempBinaryPath)
	if err != nil {
		_ = os.Remove(tempBinaryPath)
		if _, restoreErr := os.Stat(backupBinaryPath); restoreErr == nil {
			_ = os.Rename(backupBinaryPath, binaryPath)
		}
		return fmt.Errorf("failed to open new binary: %w", err)
	}
	defer source.Close()

	dest, err := os.OpenFile(binaryPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		_ = os.Remove(tempBinaryPath)
		if _, restoreErr := os.Stat(backupBinaryPath); restoreErr == nil {
			_ = os.Rename(backupBinaryPath, binaryPath)
		}
		return fmt.Errorf("failed to open install target: %w", err)
	}

	if _, err := io.Copy(dest, source); err != nil {
		_ = dest.Close()
		_ = os.Remove(tempBinaryPath)
		if _, restoreErr := os.Stat(backupBinaryPath); restoreErr == nil {
			_ = os.Rename(backupBinaryPath, binaryPath)
		}
		return fmt.Errorf("failed to write new binary: %w", err)
	}

	if err := dest.Close(); err != nil {
		_ = os.Remove(tempBinaryPath)
		if _, restoreErr := os.Stat(backupBinaryPath); restoreErr == nil {
			_ = os.Rename(backupBinaryPath, binaryPath)
		}
		return fmt.Errorf("failed to close new binary: %w", err)
	}

	_ = os.Remove(tempBinaryPath)
	if err := os.Remove(backupBinaryPath); err != nil && !os.IsNotExist(err) {
		slog.Error("failed to remove backup binary", "error", err, "path", backupBinaryPath)
	}

	return nil
}

// CreateBinaryApp implements ServerInterface.
func (h *APIHandler) CreateBinaryApp(w http.ResponseWriter, r *http.Request) {
	var req CreateBinaryAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDeployResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("invalid request body: %v", err),
			"",
		)

		return
	}
	if validationErrors := CreateBinaryAppRequestSchema.Validate(&req); validationErrors != nil {
		// handle errors -> see Errors section
		var validationErrorMessages []string
		for _, ve := range validationErrors {
			validationErrorMessages = append(validationErrorMessages, ve.Error())
		}

		writeDeployResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("validation errors: %s", strings.Join(validationErrorMessages, "; ")),
			"",
		)
		return
	}

	writeDeployResponse(
		w,
		http.StatusAccepted,
		"accepted",
		"app creation accepted",
		"",
	)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		groupingID := xid.New().String()

		emitter := NewCallbackEmitter(req.CallbackUrl, h.apiKey)

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Scope:      "action",
			Status:     "in_progress",
			Message:    fmt.Sprintf("Starting deployment for /%s", req.EnvironmentId),
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "create_app_directory",
			Scope:      "step",
			Status:     "in_progress",
			Message:    fmt.Sprintf("Creating binary app %s", req.EnvironmentId),
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		// Create app directory (under /opt/mithlond/tenants - group-writable, no sudo needed)
		// e.g., /opt/mithlond/tenants/<team-slug>/apps/<app-id>/envs/<environment>
		appDir := path.Join(
			appsBaseDir(),
			strings.ToLower(req.TeamId),
			"apps",
			strings.ToLower(req.AppId),
			"envs",
			strings.ToLower(req.EnvironmentId),
		)
		if err := os.MkdirAll(appDir, 0o755); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     CreateBinaryAppAction,
				Step:       "create_app_directory",
				Status:     "failed",
				Message:    "Failed to create app directory",
				Error:      fmt.Sprintf("failed to create app directory: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "create_app_directory",
			Status:     "completed",
			Message:    "App directory created successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "create_config_directory",
			Status:     "in_progress",
			Message:    "Creating config directory",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		// e.g., /etc/mithlond/tenants/<team-slug>/apps/<app-id>/envs/<environment>
		configDir := path.Join(
			appsConfigDir(),
			strings.ToLower(req.TeamId),
			"apps",
			strings.ToLower(req.AppId),
			"envs",
			strings.ToLower(req.EnvironmentId),
		)
		if err := sudoMkdirAll(configDir); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     CreateBinaryAppAction,
				Step:       "create_config_directory",
				Status:     "failed",
				Message:    "Failed to create config directory",
				Error:      fmt.Sprintf("failed to create config directory: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "create_config_directory",
			Status:     "completed",
			Message:    "Config directory created successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "download",
			Status:     "in_progress",
			Message:    "Starting binary download",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		// Download binary
		binaryPath := path.Join(
			appDir,
			internalBinaryName(req.EnvironmentId),
		)
		binaryURL := req.AssetUrl
		// binaryURL, _, err := buildArtifactURLs(
		// 	req.ArtifactSource,
		// 	req.ArtifactVersion,
		// 	req.ArtifactName,
		// )
		// if err != nil {
		// 	if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
		// 		Scope:      "step",
		// 		GroupingID: groupingID,
		// 		Action:     CreateBinaryAppAction,
		// 		Step:       "download",
		// 		Status:     "failed",
		// 		Message:    "Failed to build artifact URLs",
		// 		Error:      fmt.Sprintf("failed to build artifact URLs: %v", err),
		// 	}); err != nil {
		// 		slog.Error("failed to emit deployment event", "error", err)
		// 	}
		// 	return
		// }

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "download",
			Status:     "in_progress",
			Message:    fmt.Sprintf("Downloading binary version %s", req.ArtifactVersion),
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := installBinaryWithBackup(ctx, binaryURL, binaryPath); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     CreateBinaryAppAction,
				Step:       "download",
				Status:     "failed",
				Message:    "Failed to download binary",
				Error:      err.Error(),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			Action:     CreateBinaryAppAction,
			GroupingID: groupingID,
			Step:       "download",
			Status:     "completed",
			Message:    "Binary downloaded successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "environmental_variables",
			Status:     "in_progress",
			Message:    "Writing environment variables",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		// Write env file to config directory (requires sudo)
		if req.EnvVars != nil && len(*req.EnvVars) > 0 {
			envPath := path.Join(configDir, "env")
			var envContent strings.Builder
			for key, value := range *req.EnvVars {
				fmt.Fprintf(&envContent, "%s=%s\n", key, value)
			}

			if err := sudoWriteFile(envPath, []byte(envContent.String()), 0o640); err != nil {
				if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
					Scope:      "step",
					GroupingID: groupingID,
					Action:     CreateBinaryAppAction,
					Step:       "environmental_variables",
					Status:     "failed",
					Message:    "Failed to write env file",
					Error:      fmt.Sprintf("failed to write env file: %v", err),
				}); err != nil {
					slog.Error("failed to emit deployment event", "error", err)
				}
				return
			}

		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "environmental_variables",
			Status:     "completed",
			Message:    "Environment variables configured",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "systemd_service",
			Status:     "in_progress",
			Message:    "Creating systemd service",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		// Create systemd service (system unit, requires sudo)
		serviceName := strings.ToLower(
			fmt.Sprintf("%s__%s__%s", req.TeamId, req.AppId, req.EnvironmentId),
		)
		if err := createSystemdService(serviceName, binaryPath, appDir, configDir, req.Port, nil); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     CreateBinaryAppAction,
				Step:       "systemd_service",
				Status:     "failed",
				Message:    "Failed to create systemd service",
				Error:      fmt.Sprintf("failed to create systemd service: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			Action:     CreateBinaryAppAction,
			GroupingID: groupingID,
			Step:       "systemd_service",
			Status:     "completed",
			Message:    "Systemd service created successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Step:       "start_service",
			Status:     "in_progress",
			Message:    "Starting service",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		// Enable and start service (requires sudo for system units)
		if err := sudoRun("systemctl", "daemon-reload").Run(); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				Action:     CreateBinaryAppAction,
				GroupingID: groupingID,
				Step:       "start_service",
				Status:     "failed",
				Message:    "Failed to reload systemd",
				Error:      fmt.Sprintf("failed to reload systemd: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := sudoRun("systemctl", "enable", serviceName).Run(); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				Action:     CreateBinaryAppAction,
				GroupingID: groupingID,
				Step:       "start_service",
				Status:     "failed",
				Message:    "Failed to enable service",
				Error:      fmt.Sprintf("failed to enable service: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := sudoRun("systemctl", "start", serviceName).Run(); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     CreateBinaryAppAction,
				Step:       "start_service",
				Status:     "failed",
				Message:    "Failed to start service",
				Error:      fmt.Sprintf("failed to start service: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := sudoRun("systemctl", "is-active", "--quiet", serviceName).Run(); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     CreateBinaryAppAction,
				Step:       "start_service",
				Status:     "failed",
				Message:    "Service failed to start",
				Error:      fmt.Sprintf("service is not running: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Scope:      "step",
			Action:     CreateBinaryAppAction,
			Step:       "start_service",
			Status:     "completed",
			Message:    "Service started successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Action:     CreateBinaryAppAction,
			GroupingID: groupingID,
			Step:       "caddy_configuration",
			Scope:      "step",
			Status:     "in_progress",
			Message:    "Configuring Caddy route",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		caddyManager := NewCaddyManager()
		if err := caddyManager.ConfigureRoute(req.Domain, req.Port); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Action:     CreateBinaryAppAction,
				GroupingID: groupingID,
				Scope:      "step",
				Step:       "caddy_configuration",
				Status:     "failed",
				Message:    "Failed to configure Caddy route",
				Error:      fmt.Sprintf("failed to configure Caddy route: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Scope:      "step",
			Step:       "caddy_configuration",
			Status:     "completed",
			Message:    "Caddy route configured successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Action:     CreateBinaryAppAction,
			Scope:      "action",
			Status:     "completed",
			Message:    "Deployment completed successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}
	}()
}

// TODOs:
// 1. rollbacks
// 2. updating port
// 3. update env variables

// DeployBinaryApp implements ServerInterface.
func (h *APIHandler) DeployBinaryApp(w http.ResponseWriter, r *http.Request) {
	var req DeployBinaryAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDeployResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("invalid request body: %v", err),
			"",
		)
		return
	}

	if validationErrors := CreateBinaryAppRequestSchema.Validate(&req); validationErrors != nil {
		// handle errors -> see Errors section
		var validationErrorMessages []string
		for _, ve := range validationErrors {
			validationErrorMessages = append(validationErrorMessages, ve.Error())
		}

		writeDeployResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("validation errors: %s", strings.Join(validationErrorMessages, "; ")),
			"",
		)
		return
	}

	writeDeployResponse(
		w,
		http.StatusAccepted,
		"accepted",
		"app deployment accepted",
		"",
	)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		groupingID := xid.New().String()

		emitter := NewCallbackEmitter(req.CallbackUrl, h.apiKey)
		if emitter == nil {
			slog.Error("callback emitter not configured")
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Action:     DeployBinaryAppAction,
			Scope:      "action",
			Status:     "in_progress",
			Message:    fmt.Sprintf("Starting deployment for /%s", req.EnvironmentId),
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Action:     DeployBinaryAppAction,
			Step:       "verify_app_directory",
			Scope:      "step",
			Status:     "in_progress",
			Message:    "Checking app directory",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		appDir := path.Join(
			appsBaseDir(),
			strings.ToLower(req.TeamId),
			"apps",
			strings.ToLower(req.AppId),
			"envs",
			strings.ToLower(req.EnvironmentId),
		)
		if _, err := os.Stat(appDir); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     DeployBinaryAppAction,
				Step:       "verify_app_directory",
				Status:     "failed",
				Message:    "App directory does not exist",
				Error:      fmt.Sprintf("app directory not found: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     DeployBinaryAppAction,
			Step:       "verify_app_directory",
			Status:     "completed",
			Message:    "App directory verified",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Action:     DeployBinaryAppAction,
			Step:       "download",
			Scope:      "step",
			Status:     "in_progress",
			Message:    "Starting binary download",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		binaryPath := path.Join(appDir, internalBinaryName(req.EnvironmentId))
		binaryURL := req.AssetUrl
		// binaryURL, _, err := buildArtifactURLs(
		// 	req.ArtifactSource,
		// 	req.ArtifactVersion,
		// 	req.ArtifactName,
		// )
		// if err != nil {
		// 	if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
		// 		Scope:      "step",
		// 		GroupingID: groupingID,
		// 		Action:     DeployBinaryAppAction,
		// 		Step:       "download",
		// 		Status:     "failed",
		// 		Message:    "Failed to build artifact URLs",
		// 		Error:      fmt.Sprintf("failed to build artifact URLs: %v", err),
		// 	}); err != nil {
		// 		slog.Error("failed to emit deployment event", "error", err)
		// 	}
		// 	return
		// }

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     DeployBinaryAppAction,
			Step:       "download",
			Status:     "in_progress",
			Message:    fmt.Sprintf("Downloading binary version %s", req.ArtifactVersion),
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := installBinaryWithBackup(ctx, binaryURL, binaryPath); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     DeployBinaryAppAction,
				Step:       "download",
				Status:     "failed",
				Message:    "Failed to download binary",
				Error:      err.Error(),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			Action:     DeployBinaryAppAction,
			GroupingID: groupingID,
			Step:       "download",
			Status:     "completed",
			Message:    "Binary downloaded successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     DeployBinaryAppAction,
			Step:       "switch",
			Status:     "in_progress",
			Message:    "Updating systemd service",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		serviceName := strings.ToLower(
			fmt.Sprintf("%s__%s__%s", req.TeamId, req.AppId, req.EnvironmentId),
		)
		configDir := path.Join(
			appsConfigDir(),
			strings.ToLower(req.TeamId),
			"apps",
			strings.ToLower(req.AppId),
			"envs",
			strings.ToLower(req.EnvironmentId),
		)

		desiredEnv := map[string]string{}
		if req.EnvVars != nil {
			maps.Copy(desiredEnv, *req.EnvVars)
		}

		currentEnv := map[string]string{}
		envPath := path.Join(configDir, "env")
		envContent, err := os.ReadFile(envPath)
		if err != nil && !os.IsNotExist(err) {
			slog.Error("failed to read env file", "error", err, "path", envPath)
			return
		}

		if err == nil {
			for line := range strings.SplitSeq(string(envContent), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				parts := strings.SplitN(line, "=", 2)
				if len(parts) != 2 {
					continue
				}
				currentEnv[parts[0]] = parts[1]
			}
		}

		added := 0
		changed := 0
		removed := 0

		for key, desiredValue := range desiredEnv {
			currentValue, ok := currentEnv[key]
			if !ok {
				added++
				continue
			}
			if currentValue != desiredValue {
				changed++
			}
		}
		for key := range currentEnv {
			if _, ok := desiredEnv[key]; !ok {
				removed++
			}
		}

		if added+changed+removed > 0 {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     DeployBinaryAppAction,
				Step:       "environmental_variables",
				Status:     "in_progress",
				Message:    fmt.Sprintf("Updating environment variables (added: %d, changed: %d, removed: %d)", added, changed, removed),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
				return
			}

			if len(desiredEnv) == 0 {
				if _, err := os.Stat(envPath); err == nil {
					if err := sudoRun("rm", "-f", envPath).Run(); err != nil {
						if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
							Scope:      "step",
							GroupingID: groupingID,
							Action:     DeployBinaryAppAction,
							Step:       "environmental_variables",
							Status:     "failed",
							Message:    "Failed to remove env file",
							Error:      fmt.Sprintf("failed to remove env file: %v", err),
						}); err != nil {
							slog.Error("failed to emit deployment event", "error", err)
						}
						return
					}
				}
			} else {
				keys := make([]string, 0, len(desiredEnv))
				for key := range desiredEnv {
					keys = append(keys, key)
				}
				sort.Strings(keys)

				var envContent strings.Builder
				for _, key := range keys {
					fmt.Fprintf(&envContent, "%s=%s\n", key, desiredEnv[key])
				}

				tmpOutput, err := sudoRun("mktemp", path.Join(configDir, "env.tmp.XXXXXX")).Output()
				if err != nil {
					if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
						Scope:      "step",
						GroupingID: groupingID,
						Action:     DeployBinaryAppAction,
						Step:       "environmental_variables",
						Status:     "failed",
						Message:    "Failed to create temp env file",
						Error:      fmt.Sprintf("failed to create temp env file: %v", err),
					}); err != nil {
						slog.Error("failed to emit deployment event", "error", err)
					}
					return
				}

				tmpPath := strings.TrimSpace(string(tmpOutput))
				if tmpPath == "" {
					if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
						Scope:      "step",
						GroupingID: groupingID,
						Action:     DeployBinaryAppAction,
						Step:       "environmental_variables",
						Status:     "failed",
						Message:    "Failed to create temp env file",
						Error:      "mktemp returned empty path",
					}); err != nil {
						slog.Error("failed to emit deployment event", "error", err)
					}
					return
				}

				if err := sudoWriteFile(tmpPath, []byte(envContent.String()), 0o640); err != nil {
					_ = sudoRun("rm", "-f", tmpPath).Run()
					if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
						Scope:      "step",
						GroupingID: groupingID,
						Action:     DeployBinaryAppAction,
						Step:       "environmental_variables",
						Status:     "failed",
						Message:    "Failed to write env file",
						Error:      fmt.Sprintf("failed to write temp env file: %v", err),
					}); err != nil {
						slog.Error("failed to emit deployment event", "error", err)
					}
					return
				}

				if err := sudoRun("mv", "-f", tmpPath, envPath).Run(); err != nil {
					_ = sudoRun("rm", "-f", tmpPath).Run()
					if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
						Scope:      "step",
						GroupingID: groupingID,
						Action:     DeployBinaryAppAction,
						Step:       "environmental_variables",
						Status:     "failed",
						Message:    "Failed to install env file",
						Error:      fmt.Sprintf("failed to move env file into place: %v", err),
					}); err != nil {
						slog.Error("failed to emit deployment event", "error", err)
					}
					return
				}
			}

			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     DeployBinaryAppAction,
				Step:       "environmental_variables",
				Status:     "completed",
				Message:    "Environment variables updated",
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
				return
			}
		}

		if err := createSystemdService(serviceName, binaryPath, appDir, configDir, req.Port, req.Args); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     DeployBinaryAppAction,
				Step:       "switch",
				Status:     "failed",
				Message:    "Failed to update systemd service",
				Error:      fmt.Sprintf("failed to update systemd service: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			Action:     DeployBinaryAppAction,
			GroupingID: groupingID,
			Step:       "switch",
			Status:     "completed",
			Message:    "Systemd service updated",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Scope:      "step",
			GroupingID: groupingID,
			Action:     DeployBinaryAppAction,
			Step:       "restart_service",
			Status:     "in_progress",
			Message:    "Restarting service",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := sudoRun("systemctl", "daemon-reload").Run(); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				Action:     DeployBinaryAppAction,
				GroupingID: groupingID,
				Step:       "restart_service",
				Status:     "failed",
				Message:    "Failed to reload systemd",
				Error:      fmt.Sprintf("failed to reload systemd: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := sudoRun("systemctl", "restart", serviceName).Run(); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				Action:     DeployBinaryAppAction,
				GroupingID: groupingID,
				Step:       "restart_service",
				Status:     "failed",
				Message:    "Failed to restart service",
				Error:      fmt.Sprintf("failed to restart service: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := sudoRun("systemctl", "is-active", "--quiet", serviceName).Run(); err != nil {
			if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
				Scope:      "step",
				GroupingID: groupingID,
				Action:     DeployBinaryAppAction,
				Step:       "restart_service",
				Status:     "failed",
				Message:    "Service failed to restart",
				Error:      fmt.Sprintf("service is not running: %v", err),
			}); err != nil {
				slog.Error("failed to emit deployment event", "error", err)
			}
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			GroupingID: groupingID,
			Scope:      "step",
			Action:     DeployBinaryAppAction,
			Step:       "restart_service",
			Status:     "completed",
			Message:    "Service restarted successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}

		if err := emitter.EmitDeploymentEvent(ctx, DeploymentEvent{
			Action:     DeployBinaryAppAction,
			GroupingID: groupingID,
			Scope:      "action",
			Status:     "completed",
			Message:    "Deployment completed successfully",
		}); err != nil {
			slog.Error("failed to emit deployment event", "error", err)
			return
		}
	}()
}

// TODO: make naming explictly binary app actions

// StartApp implements ServerInterface.
func (h *APIHandler) StartApp(w http.ResponseWriter, r *http.Request) {
	var req AppActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppActionResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("invalid request body: %v", err),
			"",
		)
		return
	}

	serviceName := fmt.Sprintf("%s-%s", req.AppSlug, req.Environment)
	output, err := sudoRun("systemctl", "start", serviceName).CombinedOutput()
	if err != nil {
		writeAppActionResponse(
			w,
			http.StatusInternalServerError,
			"error",
			fmt.Sprintf("failed to start service: %v", err),
			string(output),
		)
		return
	}

	writeAppActionResponse(
		w,
		http.StatusOK,
		"success",
		fmt.Sprintf("service %s started", serviceName),
		string(output),
	)
}

// StopApp implements ServerInterface.
func (h *APIHandler) StopApp(w http.ResponseWriter, r *http.Request) {
	var req AppActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppActionResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("invalid request body: %v", err),
			"",
		)
		return
	}

	serviceName := fmt.Sprintf("%s-%s", req.AppSlug, req.Environment)
	output, err := sudoRun("systemctl", "stop", serviceName).CombinedOutput()
	if err != nil {
		writeAppActionResponse(
			w,
			http.StatusInternalServerError,
			"error",
			fmt.Sprintf("failed to stop service: %v", err),
			string(output),
		)
		return
	}

	writeAppActionResponse(
		w,
		http.StatusOK,
		"success",
		fmt.Sprintf("service %s stopped", serviceName),
		string(output),
	)
}

// RestartApp implements ServerInterface.
func (h *APIHandler) RestartApp(w http.ResponseWriter, r *http.Request) {
	var req AppActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppActionResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("invalid request body: %v", err),
			"",
		)
		return
	}

	serviceName := fmt.Sprintf("%s-%s", req.AppSlug, req.Environment)
	output, err := sudoRun("systemctl", "restart", serviceName).CombinedOutput()
	if err != nil {
		writeAppActionResponse(
			w,
			http.StatusInternalServerError,
			"error",
			fmt.Sprintf("failed to restart service: %v", err),
			string(output),
		)
		return
	}

	writeAppActionResponse(
		w,
		http.StatusOK,
		"success",
		fmt.Sprintf("service %s restarted", serviceName),
		string(output),
	)
}

func createSystemdService(
	serviceName, binaryPath, workDir, configDir string,
	port int,
	args *[]string,
) error {
	servicePath := systemdServicePath(serviceName)

	var argsStr string
	if args != nil && len(*args) > 0 {
		argsStr = " " + strings.Join(*args, " ")
	}

	envFile := path.Join(configDir, "env")
	envFileDirective := ""
	if _, err := os.Stat(envFile); err == nil {
		envFileDirective = fmt.Sprintf("EnvironmentFile=%s\n", envFile)
	}

	serviceContent := fmt.Sprintf(`[Unit]
Description=%s service
After=network.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s%s
Restart=always
RestartSec=5
Environment=PORT=%d
%s
[Install]
WantedBy=multi-user.target
`, serviceName, workDir, binaryPath, argsStr, port, envFileDirective)

	return sudoWriteFile(servicePath, []byte(serviceContent), 0o644)
}

func systemdServicePath(serviceName string) string {
	return path.Join("/etc/systemd/system", serviceName+".service")
}
