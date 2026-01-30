// Package api provides HTTP handlers for the API server.
package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

type APIHandler struct {
	version string
}

func NewAPIHandler(version string) *APIHandler {
	return &APIHandler{
		version: version,
	}
}

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

	if strings.TrimSpace(req.ArtifactSource) == "" || strings.TrimSpace(req.ArtifactVersion) == "" {
		writeUpdateResponse(
			w,
			http.StatusBadRequest,
			"error",
			"artifact_source and artifact_version are required",
		)
		return
	}

	binaryName := agentBinaryName()
	tempPath := fmt.Sprintf("/tmp/mithlond-agent.%s", req.ArtifactVersion)

	binaryURL, checksumURL, err := buildArtifactURLs(
		req.ArtifactSource,
		req.ArtifactVersion,
		binaryName,
	)
	if err != nil {
		writeUpdateResponse(w, http.StatusBadRequest, "error", err.Error())
		return
	}

	if err := downloadToFile(r.Context(), binaryURL, tempPath); err != nil {
		writeUpdateResponse(
			w,
			http.StatusInternalServerError,
			"error",
			fmt.Sprintf("failed to download binary: %v", err),
		)
		return
	}

	checksumBytes, err := fetchBytes(r.Context(), checksumURL)
	if err != nil {
		_ = os.Remove(tempPath)
		writeUpdateResponse(
			w,
			http.StatusInternalServerError,
			"error",
			fmt.Sprintf("failed to download checksum: %v", err),
		)
		return
	}

	if err := verifyChecksum(tempPath, string(checksumBytes)); err != nil {
		_ = os.Remove(tempPath)
		writeUpdateResponse(
			w,
			http.StatusBadRequest,
			"error",
			fmt.Sprintf("checksum verification failed: %v", err),
		)
		return
	}

	if err := os.Chmod(tempPath, 0o755); err != nil {
		writeUpdateResponse(
			w,
			http.StatusInternalServerError,
			"error",
			fmt.Sprintf("failed to chmod binary: %v", err),
		)
		return
	}

	if err := installAgentBinary(tempPath); err != nil {
		writeUpdateResponse(w, http.StatusInternalServerError, "error", err.Error())
		return
	}

	writeUpdateResponse(w, http.StatusOK, "restarting", "agent will restart with new version")
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

func buildArtifactURLs(source, version, fileName string) (string, string, error) {
	if strings.HasPrefix(source, "s3://") || strings.HasPrefix(source, "r2://") {
		return buildSignedS3URLs(source, version, fileName)
	}

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		sourceWithVersion := strings.ReplaceAll(source, "{version}", version)
		if strings.Contains(sourceWithVersion, "{file}") {
			binaryURL := strings.ReplaceAll(sourceWithVersion, "{file}", fileName)
			checksumURL := strings.ReplaceAll(sourceWithVersion, "{file}", fileName+".sha256")
			return binaryURL, checksumURL, nil
		}

		binaryURL, err := url.JoinPath(sourceWithVersion, fileName)
		if err != nil {
			return "", "", err
		}
		return binaryURL, binaryURL + ".sha256", nil
	}

	return "", "", fmt.Errorf("unsupported artifact_source scheme")
}

func buildSignedS3URLs(source, version, fileName string) (string, string, error) {
	parsed, err := url.Parse(source)
	if err != nil {
		return "", "", fmt.Errorf("invalid artifact_source: %w", err)
	}

	bucket := parsed.Host
	prefix := strings.TrimPrefix(parsed.Path, "/")
	prefix = strings.ReplaceAll(prefix, "{version}", version)
	var key string
	if strings.Contains(prefix, "{file}") {
		key = strings.ReplaceAll(prefix, "{file}", fileName)
	} else if prefix != "" {
		key = path.Join(prefix, version, fileName)
	} else {
		key = path.Join(version, fileName)
	}

	endpoint := os.Getenv("S3_ENDPOINT")
	region := os.Getenv("S3_REGION")
	accessKey := os.Getenv("S3_ACCESS_KEY_ID")
	secretKey := os.Getenv("S3_SECRET_ACCESS_KEY")

	if parsed.Scheme == "r2" {
		if endpoint == "" {
			accountID := os.Getenv("R2_ACCOUNT_ID")
			if accountID != "" {
				endpoint = fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)
			}
		}
		if bucket == "" {
			bucket = os.Getenv("R2_BUCKET_NAME")
		}
		if accessKey == "" {
			accessKey = os.Getenv("R2_ACCESS_KEY_ID")
		}
		if secretKey == "" {
			secretKey = os.Getenv("R2_SECRET_ACCESS_KEY")
		}
		if region == "" {
			region = "auto"
		}
	}

	if endpoint == "" || bucket == "" || accessKey == "" || secretKey == "" {
		return "", "", fmt.Errorf("missing S3/R2 configuration")
	}

	if region == "" {
		region = "us-east-1"
	}

	binaryURL, err := presignS3Get(
		endpoint,
		region,
		accessKey,
		secretKey,
		bucket,
		key,
		15*time.Minute,
	)
	if err != nil {
		return "", "", err
	}

	checksumURL, err := presignS3Get(
		endpoint,
		region,
		accessKey,
		secretKey,
		bucket,
		key+".sha256",
		15*time.Minute,
	)
	if err != nil {
		return "", "", err
	}

	return binaryURL, checksumURL, nil
}

func downloadToFile(ctx context.Context, url string, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	file, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, resp.Body)
	return err
}

func fetchBytes(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func verifyChecksum(filePath string, expectedChecksum string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}

	actualChecksum := hex.EncodeToString(hash.Sum(nil))
	expectedChecksum = strings.TrimSpace(strings.Split(expectedChecksum, " ")[0])

	if actualChecksum != expectedChecksum {
		return fmt.Errorf(
			"checksum mismatch: expected %s, got %s",
			expectedChecksum,
			actualChecksum,
		)
	}

	return nil
}

func installAgentBinary(binaryPath string) error {
	currentBinary := "/opt/mithlond-agent/mithlond-agent"
	backupBinary := "/opt/mithlond-agent/mithlond-agent.backup"

	if err := os.Rename(currentBinary, backupBinary); err != nil {
		return fmt.Errorf("failed to backup current binary: %w", err)
	}

	if err := os.Rename(binaryPath, currentBinary); err != nil {
		_ = os.Rename(backupBinary, currentBinary)
		return fmt.Errorf("failed to install new binary: %w", err)
	}

	if err := os.Chmod(currentBinary, 0o755); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	go func() {
		time.Sleep(1 * time.Second)
		_ = exec.Command("systemctl", "restart", "mithlond-agent").Run()
	}()

	return nil
}

func writeUpdateResponse(w http.ResponseWriter, statusCode int, status string, message string) {
	resp := UpdateAgentResponse{
		Status:  &status,
		Message: &message,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(resp)
}

func presignS3Get(
	endpoint, region, accessKey, secretKey, bucket, key string,
	ttl time.Duration,
) (string, error) {
	baseURL, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}

	baseURL.Path = path.Join(baseURL.Path, bucket, key)

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	service := "s3"

	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, region, service)
	credential := fmt.Sprintf("%s/%s", accessKey, credentialScope)

	query := baseURL.Query()
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", credential)
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", fmt.Sprintf("%d", int(ttl.Seconds())))
	query.Set("X-Amz-SignedHeaders", "host")

	baseURL.RawQuery = query.Encode()

	canonicalRequest := strings.Join([]string{
		http.MethodGet,
		baseURL.EscapedPath(),
		baseURL.RawQuery,
		fmt.Sprintf("host:%s\n", baseURL.Host),
		"host",
		"UNSIGNED-PAYLOAD",
	}, "\n")

	hash := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		credentialScope,
		hex.EncodeToString(hash[:]),
	}, "\n")

	signingKey := deriveSigningKey(secretKey, dateStamp, region, service)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	query.Set("X-Amz-Signature", signature)
	baseURL.RawQuery = query.Encode()

	return baseURL.String(), nil
}

func deriveSigningKey(secretKey, dateStamp, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	kSigning := hmacSHA256(kService, "aws4_request")
	return kSigning
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// CreateBinaryApp implements ServerInterface.
func (h *APIHandler) CreateBinaryApp(w http.ResponseWriter, r *http.Request) {
	var req CreateBinaryAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDeployResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("invalid request body: %v", err), "")
		return
	}

	if strings.TrimSpace(req.AppSlug) == "" || strings.TrimSpace(req.Environment) == "" {
		writeDeployResponse(w, http.StatusBadRequest, "error", "app_slug and environment are required", "")
		return
	}

	if strings.TrimSpace(req.ArtifactSource) == "" || strings.TrimSpace(req.ArtifactVersion) == "" {
		writeDeployResponse(w, http.StatusBadRequest, "error", "artifact_source and artifact_version are required", "")
		return
	}

	var logs strings.Builder
	logs.WriteString(fmt.Sprintf("Creating binary app %s/%s\n", req.AppSlug, req.Environment))

	// Create app directory
	appDir := fmt.Sprintf("/opt/%s/%s", req.AppSlug, req.Environment)
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to create app directory: %v", err), logs.String())
		return
	}
	logs.WriteString(fmt.Sprintf("Created directory: %s\n", appDir))

	// Download binary
	binaryPath := path.Join(appDir, req.ArtifactVersion)
	binaryURL, checksumURL, err := buildArtifactURLs(req.ArtifactSource, req.ArtifactVersion, req.AppSlug)
	if err != nil {
		writeDeployResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("failed to build artifact URLs: %v", err), logs.String())
		return
	}

	logs.WriteString(fmt.Sprintf("Downloading binary from %s\n", req.ArtifactSource))
	if err := downloadToFile(r.Context(), binaryURL, binaryPath); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to download binary: %v", err), logs.String())
		return
	}
	logs.WriteString("Binary downloaded successfully\n")

	// Verify checksum
	checksumBytes, err := fetchBytes(r.Context(), checksumURL)
	if err != nil {
		logs.WriteString(fmt.Sprintf("Warning: could not fetch checksum: %v\n", err))
	} else {
		if err := verifyChecksum(binaryPath, string(checksumBytes)); err != nil {
			_ = os.Remove(binaryPath)
			writeDeployResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("checksum verification failed: %v", err), logs.String())
			return
		}
		logs.WriteString("Checksum verified\n")
	}

	// Make binary executable
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to chmod binary: %v", err), logs.String())
		return
	}

	// Write .env file
	if req.EnvVars != nil && len(*req.EnvVars) > 0 {
		envPath := path.Join(appDir, ".env")
		var envContent strings.Builder
		for key, value := range *req.EnvVars {
			envContent.WriteString(fmt.Sprintf("%s=%s\n", key, value))
		}
		if err := os.WriteFile(envPath, []byte(envContent.String()), 0o644); err != nil {
			writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to write .env file: %v", err), logs.String())
			return
		}
		logs.WriteString("Created .env file\n")
	}

	// Create systemd service
	serviceName := fmt.Sprintf("%s-%s", req.AppSlug, req.Environment)
	if err := createSystemdService(serviceName, binaryPath, appDir, req.Port, req.Args); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to create systemd service: %v", err), logs.String())
		return
	}
	logs.WriteString(fmt.Sprintf("Created systemd service: %s\n", serviceName))

	// Enable and start service
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to reload systemd: %v", err), logs.String())
		return
	}

	if err := exec.Command("systemctl", "enable", serviceName).Run(); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to enable service: %v", err), logs.String())
		return
	}

	if err := exec.Command("systemctl", "start", serviceName).Run(); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to start service: %v", err), logs.String())
		return
	}
	logs.WriteString("Service enabled and started\n")

	// Configure Caddy route if domain is specified
	if req.Domain != nil && *req.Domain != "" {
		caddyManager := NewCaddyManager()
		if err := caddyManager.ConfigureRoute(*req.Domain, req.Port); err != nil {
			logs.WriteString(fmt.Sprintf("Warning: failed to configure Caddy route: %v\n", err))
		} else {
			logs.WriteString(fmt.Sprintf("Configured Caddy route: %s -> localhost:%d\n", *req.Domain, req.Port))
		}
	}

	logs.WriteString("Binary app created successfully\n")
	writeDeployResponse(w, http.StatusOK, "success", "app created and started", logs.String())
}

// DeployBinaryApp implements ServerInterface.
func (h *APIHandler) DeployBinaryApp(w http.ResponseWriter, r *http.Request) {
	var req DeployBinaryAppRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeDeployResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("invalid request body: %v", err), "")
		return
	}

	if strings.TrimSpace(req.AppSlug) == "" || strings.TrimSpace(req.Environment) == "" {
		writeDeployResponse(w, http.StatusBadRequest, "error", "app_slug and environment are required", "")
		return
	}

	var logs strings.Builder
	logs.WriteString(fmt.Sprintf("Deploying binary app %s/%s version %s\n", req.AppSlug, req.Environment, req.ArtifactVersion))

	appDir := fmt.Sprintf("/opt/%s/%s", req.AppSlug, req.Environment)
	if _, err := os.Stat(appDir); os.IsNotExist(err) {
		writeDeployResponse(w, http.StatusBadRequest, "error", "app does not exist, use create endpoint first", logs.String())
		return
	}

	// Download new binary
	binaryPath := path.Join(appDir, req.ArtifactVersion)
	binaryURL, checksumURL, err := buildArtifactURLs(req.ArtifactSource, req.ArtifactVersion, req.AppSlug)
	if err != nil {
		writeDeployResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("failed to build artifact URLs: %v", err), logs.String())
		return
	}

	logs.WriteString(fmt.Sprintf("Downloading binary version %s\n", req.ArtifactVersion))
	if err := downloadToFile(r.Context(), binaryURL, binaryPath); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to download binary: %v", err), logs.String())
		return
	}
	logs.WriteString("Binary downloaded successfully\n")

	// Verify checksum
	checksumBytes, err := fetchBytes(r.Context(), checksumURL)
	if err != nil {
		logs.WriteString(fmt.Sprintf("Warning: could not fetch checksum: %v\n", err))
	} else {
		if err := verifyChecksum(binaryPath, string(checksumBytes)); err != nil {
			_ = os.Remove(binaryPath)
			writeDeployResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("checksum verification failed: %v", err), logs.String())
			return
		}
		logs.WriteString("Checksum verified\n")
	}

	// Make binary executable
	if err := os.Chmod(binaryPath, 0o755); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to chmod binary: %v", err), logs.String())
		return
	}

	// Update systemd service to point to new version
	serviceName := fmt.Sprintf("%s-%s", req.AppSlug, req.Environment)
	servicePath := fmt.Sprintf("/etc/systemd/system/%s.service", serviceName)

	// Read current service file to get port
	serviceContent, err := os.ReadFile(servicePath)
	if err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to read service file: %v", err), logs.String())
		return
	}

	// Extract port from existing service (simple parsing)
	port := 0
	for _, line := range strings.Split(string(serviceContent), "\n") {
		if strings.Contains(line, "PORT=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &port)
			}
		}
	}

	if err := createSystemdService(serviceName, binaryPath, appDir, port, req.Args); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to update systemd service: %v", err), logs.String())
		return
	}
	logs.WriteString("Updated systemd service\n")

	// Reload and restart service
	if err := exec.Command("systemctl", "daemon-reload").Run(); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to reload systemd: %v", err), logs.String())
		return
	}

	if err := exec.Command("systemctl", "restart", serviceName).Run(); err != nil {
		writeDeployResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to restart service: %v", err), logs.String())
		return
	}
	logs.WriteString("Service restarted with new version\n")

	writeDeployResponse(w, http.StatusOK, "success", fmt.Sprintf("deployed version %s", req.ArtifactVersion), logs.String())
}

// CreateDockerApp implements ServerInterface.
func (h *APIHandler) CreateDockerApp(w http.ResponseWriter, r *http.Request) {
	writeDeployResponse(w, http.StatusNotImplemented, "error", "docker app creation not yet implemented", "")
}

// DeployDockerApp implements ServerInterface.
func (h *APIHandler) DeployDockerApp(w http.ResponseWriter, r *http.Request) {
	writeDeployResponse(w, http.StatusNotImplemented, "error", "docker app deployment not yet implemented", "")
}

// StartApp implements ServerInterface.
func (h *APIHandler) StartApp(w http.ResponseWriter, r *http.Request) {
	var req AppActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppActionResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("invalid request body: %v", err), "")
		return
	}

	serviceName := fmt.Sprintf("%s-%s", req.AppSlug, req.Environment)
	output, err := exec.Command("systemctl", "start", serviceName).CombinedOutput()
	if err != nil {
		writeAppActionResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to start service: %v", err), string(output))
		return
	}

	writeAppActionResponse(w, http.StatusOK, "success", fmt.Sprintf("service %s started", serviceName), string(output))
}

// StopApp implements ServerInterface.
func (h *APIHandler) StopApp(w http.ResponseWriter, r *http.Request) {
	var req AppActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppActionResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("invalid request body: %v", err), "")
		return
	}

	serviceName := fmt.Sprintf("%s-%s", req.AppSlug, req.Environment)
	output, err := exec.Command("systemctl", "stop", serviceName).CombinedOutput()
	if err != nil {
		writeAppActionResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to stop service: %v", err), string(output))
		return
	}

	writeAppActionResponse(w, http.StatusOK, "success", fmt.Sprintf("service %s stopped", serviceName), string(output))
}

// RestartApp implements ServerInterface.
func (h *APIHandler) RestartApp(w http.ResponseWriter, r *http.Request) {
	var req AppActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAppActionResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("invalid request body: %v", err), "")
		return
	}

	serviceName := fmt.Sprintf("%s-%s", req.AppSlug, req.Environment)
	output, err := exec.Command("systemctl", "restart", serviceName).CombinedOutput()
	if err != nil {
		writeAppActionResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to restart service: %v", err), string(output))
		return
	}

	writeAppActionResponse(w, http.StatusOK, "success", fmt.Sprintf("service %s restarted", serviceName), string(output))
}

// GetNodeMetrics implements ServerInterface.
func (h *APIHandler) GetNodeMetrics(w http.ResponseWriter, r *http.Request) {
	queries := []string{
		"node_memory_MemAvailable_bytes",
		"node_memory_MemTotal_bytes",
		`node_filesystem_avail_bytes{mountpoint="/"}`,
		`node_filesystem_size_bytes{mountpoint="/"}`,
		`node_cpu_seconds_total{mode="idle"}`,
	}

	result := make(map[string]interface{})

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

		var queryResult map[string]interface{}
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

func createSystemdService(serviceName, binaryPath, workDir string, port int, args *[]string) error {
	var argsStr string
	if args != nil && len(*args) > 0 {
		argsStr = " " + strings.Join(*args, " ")
	}

	envFile := path.Join(workDir, ".env")
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

	servicePath := fmt.Sprintf("/etc/systemd/system/%s.service", serviceName)
	return os.WriteFile(servicePath, []byte(serviceContent), 0o644)
}

func writeDeployResponse(w http.ResponseWriter, statusCode int, status, message, logs string) {
	resp := DeployResponse{
		Status:  &status,
		Message: &message,
	}
	if logs != "" {
		resp.Logs = &logs
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(resp)
}

func writeAppActionResponse(w http.ResponseWriter, statusCode int, status, message, output string) {
	resp := AppActionResponse{
		Status:  &status,
		Message: &message,
	}
	if output != "" {
		resp.Output = &output
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(resp)
}

// CaddyManager handles Caddy reverse proxy configuration
type CaddyManager struct {
	apiURL string
}

// NewCaddyManager creates a new CaddyManager
func NewCaddyManager() *CaddyManager {
	return &CaddyManager{
		apiURL: "http://localhost:2019",
	}
}

// CaddyRoute represents a Caddy route configuration
type CaddyRoute struct {
	ID       string         `json:"@id,omitempty"`
	Match    []CaddyMatcher `json:"match"`
	Handle   []CaddyHandler `json:"handle"`
	Terminal bool           `json:"terminal"`
}

// CaddyMatcher represents a Caddy matcher
type CaddyMatcher struct {
	Host []string `json:"host"`
}

// CaddyHandler represents a Caddy handler
type CaddyHandler struct {
	Handler   string          `json:"handler"`
	Upstreams []CaddyUpstream `json:"upstreams,omitempty"`
}

// CaddyUpstream represents a Caddy upstream
type CaddyUpstream struct {
	Dial string `json:"dial"`
}

// ConfigureRoute configures a Caddy reverse proxy route
func (cm *CaddyManager) ConfigureRoute(domain string, port int) error {
	routeID := fmt.Sprintf("mithlond_%s", sanitizeDomain(domain))

	route := CaddyRoute{
		ID: routeID,
		Match: []CaddyMatcher{
			{Host: []string{domain}},
		},
		Handle: []CaddyHandler{
			{
				Handler: "reverse_proxy",
				Upstreams: []CaddyUpstream{
					{Dial: fmt.Sprintf("localhost:%d", port)},
				},
			},
		},
		Terminal: true,
	}

	// Remove existing route first
	cm.RemoveRoute(routeID)

	jsonData, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("failed to marshal route: %w", err)
	}

	routeURL := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", cm.apiURL)
	req, err := http.NewRequest("POST", routeURL, strings.NewReader(string(jsonData)))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to configure route: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("caddy API returned status %d: %s", resp.StatusCode, string(body))
	}

	return nil
}

// RemoveRoute removes a Caddy route by ID
func (cm *CaddyManager) RemoveRoute(routeID string) error {
	routeURL := fmt.Sprintf("%s/id/%s", cm.apiURL, routeID)
	req, err := http.NewRequest("DELETE", routeURL, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil // Ignore errors when removing (route might not exist)
	}
	defer resp.Body.Close()

	return nil
}

func sanitizeDomain(domain string) string {
	domain = strings.ReplaceAll(domain, ".", "_")
	domain = strings.ReplaceAll(domain, "-", "_")
	return domain
}

// Ensure APIHandler implements ServerInterface
var _ ServerInterface = (*APIHandler)(nil)
