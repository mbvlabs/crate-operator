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
	"runtime"
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
		writeUpdateResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("invalid request body: %v", err))
		return
	}

	if strings.TrimSpace(req.ArtifactSource) == "" || strings.TrimSpace(req.ArtifactVersion) == "" {
		writeUpdateResponse(w, http.StatusBadRequest, "error", "artifact_source and artifact_version are required")
		return
	}

	binaryName := agentBinaryName()
	tempPath := fmt.Sprintf("/tmp/mithlond-agent.%s", req.ArtifactVersion)

	binaryURL, checksumURL, err := buildArtifactURLs(req.ArtifactSource, req.ArtifactVersion, binaryName)
	if err != nil {
		writeUpdateResponse(w, http.StatusBadRequest, "error", err.Error())
		return
	}

	if err := downloadToFile(r.Context(), binaryURL, tempPath); err != nil {
		writeUpdateResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to download binary: %v", err))
		return
	}

	checksumBytes, err := fetchBytes(r.Context(), checksumURL)
	if err != nil {
		_ = os.Remove(tempPath)
		writeUpdateResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to download checksum: %v", err))
		return
	}

	if err := verifyChecksum(tempPath, string(checksumBytes)); err != nil {
		_ = os.Remove(tempPath)
		writeUpdateResponse(w, http.StatusBadRequest, "error", fmt.Sprintf("checksum verification failed: %v", err))
		return
	}

	if err := os.Chmod(tempPath, 0o755); err != nil {
		writeUpdateResponse(w, http.StatusInternalServerError, "error", fmt.Sprintf("failed to chmod binary: %v", err))
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
	return fmt.Sprintf("mithlond-agent-%s-%s", runtime.GOOS, runtime.GOARCH)
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

	binaryURL, err := presignS3Get(endpoint, region, accessKey, secretKey, bucket, key, 15*time.Minute)
	if err != nil {
		return "", "", err
	}

	checksumURL, err := presignS3Get(endpoint, region, accessKey, secretKey, bucket, key+".sha256", 15*time.Minute)
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
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
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

func presignS3Get(endpoint, region, accessKey, secretKey, bucket, key string, ttl time.Duration) (string, error) {
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
		queryURL := fmt.Sprintf("http://localhost:9090/api/v1/query?query=%s", url.QueryEscape(query))
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
			json.NewEncoder(w).Encode(map[string]string{"error": "failed to read prometheus response"})
			return
		}

		var queryResult map[string]interface{}
		if err := json.Unmarshal(body, &queryResult); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "failed to parse prometheus response"})
			return
		}

		result[query] = queryResult
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// Ensure APIHandler implements ServerInterface
var _ ServerInterface = (*APIHandler)(nil)
