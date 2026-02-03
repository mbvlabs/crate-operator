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
	"syscall"
	"time"
)

type APIHandler struct {
	version string
	apiKey  string
}

func NewAPIHandler(version, apiKey string) *APIHandler {
	return &APIHandler{
		version: version,
		apiKey:  apiKey,
	}
}

// func agentBinaryPath() string {
// 	return path.Join(agentInstallDir(), "mithlond-agent")
// }

func appsBaseDir() string {
	return "/opt/mithlond/apps"
}

func appsConfigDir() string {
	return "/etc/mithlond/apps"
}

// sudoRun executes a command with sudo privileges.
func sudoRun(name string, args ...string) *exec.Cmd {
	sudoArgs := append([]string{name}, args...)
	return exec.Command("sudo", sudoArgs...)
}

// sudoMkdirAll creates directories using sudo mkdir -p.
func sudoMkdirAll(path string) error {
	return sudoRun("mkdir", "-p", path).Run()
}

// sudoWriteFile writes content to a file using sudo tee.
func sudoWriteFile(path string, content []byte, mode os.FileMode) error {
	cmd := sudoRun("tee", path)
	cmd.Stdin = strings.NewReader(string(content))
	cmd.Stdout = nil // suppress tee's stdout
	if err := cmd.Run(); err != nil {
		return err
	}
	return sudoRun("chmod", fmt.Sprintf("%o", mode), path).Run()
}

func buildArtifactURLs(source, version, fileName string) (string, string, error) {
	if strings.HasPrefix(source, "s3://") || strings.HasPrefix(source, "r2://") {
		return buildSignedS3URLs(source, version, fileName)
	}

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		binaryURL, err := url.JoinPath(source, version, fileName)
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
	currentBinary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to resolve current binary: %w", err)
	}
	backupBinary := currentBinary + ".backup"

	if _, err := os.Stat(currentBinary); err == nil {
		if err := os.Rename(currentBinary, backupBinary); err != nil {
			return fmt.Errorf("failed to backup current binary: %w", err)
		}
	}

	source, err := os.Open(binaryPath)
	if err != nil {
		_ = os.Rename(backupBinary, currentBinary)
		return fmt.Errorf("failed to open new binary: %w", err)
	}
	defer source.Close()

	dest, err := os.OpenFile(currentBinary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		_ = os.Rename(backupBinary, currentBinary)
		return fmt.Errorf("failed to open install target: %w", err)
	}

	if _, err := io.Copy(dest, source); err != nil {
		_ = dest.Close()
		_ = os.Rename(backupBinary, currentBinary)
		return fmt.Errorf("failed to write new binary: %w", err)
	}

	if err := dest.Close(); err != nil {
		_ = os.Rename(backupBinary, currentBinary)
		return fmt.Errorf("failed to close new binary: %w", err)
	}

	_ = os.Remove(binaryPath)
	_ = os.Remove(backupBinary)

	go func() {
		time.Sleep(1 * time.Second)
		_ = syscall.Exec(currentBinary, os.Args, os.Environ())
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

// Ensure APIHandler implements ServerInterface
var _ ServerInterface = (*APIHandler)(nil)
