package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	agentSlotGreen      = "green"
	agentSlotBlue       = "blue"
	agentGreenPort      = 9640
	agentBluePort       = 9641
	agentServiceGreen   = "deploy-crate-agent-green"
	agentServiceBlue    = "deploy-crate-agent-blue"
	agentDeployStateDir = "/opt/deploy-crate/agent"
	agentDeployLockPath = "/tmp/deploy-crate-agent-deploy.lock"
)

type AgentDeployRequest struct {
	Version                string  `json:"version"`
	Checksum               *string `json:"checksum,omitempty"`
	RequestID              *string `json:"request_id,omitempty"`
	DeploymentID           *string `json:"deployment_id,omitempty"`
	CallbackURL            *string `json:"callback_url,omitempty"`
	OverrideArtifactSource *string `json:"-"`
}

type AgentDeployResponse struct {
	DeploymentID string `json:"deployment_id"`
	State        string `json:"state"`
}

type AgentDeploymentStatus struct {
	DeploymentID      string     `json:"deployment_id"`
	RequestID         string     `json:"request_id,omitempty"`
	TargetVersion     string     `json:"target_version"`
	State             string     `json:"state"`
	ActiveSlotBefore  string     `json:"active_slot_before"`
	ActiveSlotCurrent string     `json:"active_slot_current"`
	CurrentStep       string     `json:"current_step"`
	Error             *string    `json:"error"`
	StartedAt         *time.Time `json:"started_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	FinishedAt        *time.Time `json:"finished_at"`

	checksum         *string
	source           *string
	host             string
	callbackURL      string
	externalDeployID string
}

type AgentDeploymentManager struct {
	mu           sync.Mutex
	deployments  map[string]*AgentDeploymentStatus
	requestIndex map[string]string
	queue        chan string
	apiKey       string
}

func NewAgentDeploymentManager(apiKey string) *AgentDeploymentManager {
	m := &AgentDeploymentManager{
		deployments:  map[string]*AgentDeploymentStatus{},
		requestIndex: map[string]string{},
		queue:        make(chan string, 64),
		apiKey:       apiKey,
	}
	go m.worker()
	return m
}

func (m *AgentDeploymentManager) Trigger(req AgentDeployRequest) (*AgentDeploymentStatus, bool, error) {
	version := strings.TrimSpace(req.Version)
	if version == "" {
		return nil, false, errors.New("version is required")
	}

	requestID := ""
	if req.RequestID != nil {
		requestID = strings.TrimSpace(*req.RequestID)
	}
	externalDeployID := ""
	if req.DeploymentID != nil {
		externalDeployID = strings.TrimSpace(*req.DeploymentID)
	}
	callbackURL := ""
	if req.CallbackURL != nil {
		callbackURL = strings.TrimSpace(*req.CallbackURL)
	}

	m.mu.Lock()
	if requestID != "" {
		if existingID, ok := m.requestIndex[requestID]; ok {
			existing, ok := m.deployments[existingID]
			if ok {
				dup := cloneDeployment(existing)
				m.mu.Unlock()
				return dup, true, nil
			}
		}
	}

	deploymentID := randomID()
	now := time.Now().UTC()
	status := &AgentDeploymentStatus{
		DeploymentID:     deploymentID,
		RequestID:        requestID,
		TargetVersion:    version,
		State:            "queued",
		CurrentStep:      "queued",
		UpdatedAt:        now,
		checksum:         req.Checksum,
		source:           req.OverrideArtifactSource,
		callbackURL:      callbackURL,
		externalDeployID: externalDeployID,
	}

	if requestID != "" {
		m.requestIndex[requestID] = deploymentID
	}
	m.deployments[deploymentID] = status
	m.mu.Unlock()

	m.emitCallbackStatus(cloneDeployment(status), callbackURL, externalDeployID)
	m.queue <- deploymentID
	return cloneDeployment(status), false, nil
}

func (m *AgentDeploymentManager) Get(id string) (*AgentDeploymentStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.deployments[id]
	if !ok {
		return nil, false
	}
	return cloneDeployment(st), true
}

func (m *AgentDeploymentManager) worker() {
	for id := range m.queue {
		m.execute(id)
	}
}

func (m *AgentDeploymentManager) execute(id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		now := time.Now().UTC()
		st.State = "running"
		st.CurrentStep = "acquire_lock"
		st.StartedAt = &now
		st.UpdatedAt = now
	}); err != nil {
		return
	}

	lock, err := acquireDeploymentLock()
	if err != nil {
		m.fail(id, "acquire_lock", fmt.Errorf("failed to acquire deployment lock: %w", err))
		return
	}
	defer lock.release()

	cm := NewCaddyManager()

	if err := os.MkdirAll(agentDeployStateDir, 0o755); err != nil {
		m.fail(id, "download_artifact", err)
		return
	}
	tempFile, err := os.CreateTemp(agentDeployStateDir, "crate-operator.*.tmp")
	if err != nil {
		m.fail(id, "download_artifact", err)
		return
	}
	tempPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		m.fail(id, "download_artifact", fmt.Errorf("failed to prepare temporary artifact file: %w", err))
		return
	}
	defer os.Remove(tempPath)

	source := agentArtifactSource
	if override := m.sourceOf(id); override != nil && strings.TrimSpace(*override) != "" {
		source = strings.TrimSpace(*override)
	}
	binaryName := agentBinaryName()
	binaryURL, checksumURL, err := buildArtifactURLs(source, m.versionOf(id), binaryName)
	if err != nil {
		m.fail(id, "download_artifact", err)
		return
	}
	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "download_artifact"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := downloadToFile(ctx, binaryURL, tempPath); err != nil {
		m.fail(id, "download_artifact", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "verify_checksum"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	expectedChecksum := ""
	if checksum := m.checksumOf(id); checksum != nil && strings.TrimSpace(*checksum) != "" {
		expectedChecksum = strings.TrimSpace(*checksum)
	} else {
		checksumBytes, fetchErr := fetchBytes(ctx, checksumURL)
		if fetchErr != nil {
			m.fail(id, "verify_checksum", fetchErr)
			return
		}
		parsedChecksum, parseErr := parseChecksumFromChecksumsFile(checksumBytes, binaryName)
		if parseErr != nil {
			m.fail(id, "verify_checksum", parseErr)
			return
		}
		expectedChecksum = parsedChecksum
	}
	if err := verifyChecksum(tempPath, expectedChecksum); err != nil {
		m.fail(id, "verify_checksum", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "swap_agent_binary"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	agentBinaryPath, err := os.Executable()
	if err != nil {
		m.fail(id, "swap_agent_binary", fmt.Errorf("failed to resolve agent binary path: %w", err))
		return
	}
	backupPath, err := swapAgentBinary(tempPath, agentBinaryPath)
	if err != nil {
		m.fail(id, "swap_agent_binary", err)
		return
	}
	restoreBinary := func(baseErr error) error {
		if restoreErr := restoreAgentBinary(agentBinaryPath, backupPath); restoreErr != nil {
			return errors.Join(baseErr, fmt.Errorf("rollback restore failed: %w", restoreErr))
		}
		return baseErr
	}
	removeBackup := func() error {
		if backupPath == "" {
			return nil
		}
		if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove backup binary %s: %w", backupPath, err)
		}
		return nil
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "detect_running_service"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	activeSlot, err := runningAgentSlot()
	if err != nil {
		m.fail(id, "detect_running_service", restoreBinary(err))
		return
	}
	inactiveSlot := otherSlot(activeSlot)
	activeService := serviceForSlot(activeSlot)
	inactiveService := serviceForSlot(inactiveSlot)
	emitAgentDeployEvent(id, "running_service_detected",
		"active_slot", activeSlot,
		"active_service", activeService,
		"inactive_slot", inactiveSlot,
		"inactive_service", inactiveService,
	)
	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.ActiveSlotBefore = activeSlot
		st.ActiveSlotCurrent = activeSlot
		st.CurrentStep = "determine_inactive_slot"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "start_inactive_slot_service"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := runSystemctl("start", serviceForSlot(inactiveSlot)); err != nil {
		m.fail(id, "start_inactive_slot_service", restoreBinary(err))
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "verify_inactive_slot_readiness"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := m.verifySlotHealth(ctx, inactiveSlot); err != nil {
		cleanupErr := err
		if stopErr := runSystemctl("stop", serviceForSlot(inactiveSlot)); stopErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to stop inactive service during rollback: %w", stopErr))
		}
		m.fail(id, "verify_inactive_slot_readiness", restoreBinary(cleanupErr))
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "switch_traffic_to_inactive"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := cm.SetAgentTrafficSlot(inactiveSlot); err != nil {
		cleanupErr := err
		if stopErr := runSystemctl("stop", serviceForSlot(inactiveSlot)); stopErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to stop inactive service during rollback: %w", stopErr))
		}
		m.fail(id, "switch_traffic_to_inactive", restoreBinary(cleanupErr))
		return
	}
	emitAgentDeployEvent(id, "traffic_switched",
		"active_slot", inactiveSlot,
		"active_service", inactiveService,
		"previous_slot", activeSlot,
		"previous_service", activeService,
	)
	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.ActiveSlotCurrent = inactiveSlot
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "verify_public_path_after_cutover"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := m.verifyPublicHealth(ctx, id); err != nil {
		cleanupErr := err
		if caddyErr := cm.SetAgentTrafficSlot(activeSlot); caddyErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to restore caddy traffic during rollback: %w", caddyErr))
		}
		if stopErr := runSystemctl("stop", serviceForSlot(inactiveSlot)); stopErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("failed to stop inactive service during rollback: %w", stopErr))
		}
		m.fail(id, "verify_public_path_after_cutover", restoreBinary(cleanupErr))
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "stop_previous_active_service"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := runSystemctl("stop", serviceForSlot(activeSlot)); err != nil {
		m.fail(id, "stop_previous_active_service", err)
		return
	}
	emitAgentDeployEvent(id, "previous_service_stopped", "stopped_service", activeService)
	if err := removeBackup(); err != nil {
		m.fail(id, "cleanup_backup_binary", err)
		return
	}

	_ = m.update(id, func(st *AgentDeploymentStatus) {
		finished := time.Now().UTC()
		st.State = "succeeded"
		st.CurrentStep = "completed"
		st.Error = nil
		st.FinishedAt = &finished
		st.UpdatedAt = finished
	})
}

func (m *AgentDeploymentManager) fail(id, step string, err error) {
	emitAgentDeployEvent(id, "deployment_failed", "step", step, "error", err.Error())
	_ = m.update(id, func(st *AgentDeploymentStatus) {
		now := time.Now().UTC()
		msg := err.Error()
		st.State = "failed"
		st.CurrentStep = step
		st.Error = &msg
		st.FinishedAt = &now
		st.UpdatedAt = now
	})
}

func emitAgentDeployEvent(deploymentID, event string, attrs ...any) {
	base := []any{"deployment_id", deploymentID, "event", event}
	slog.Info("agent deployment event", append(base, attrs...)...)
}

func (m *AgentDeploymentManager) verifySlotHealth(ctx context.Context, slot string) error {
	port := portForSlot(slot)
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	return m.checkHealth(ctx, url, "")
}

func (m *AgentDeploymentManager) verifyPublicHealth(ctx context.Context, deploymentID string) error {
	m.mu.Lock()
	st, ok := m.deployments[deploymentID]
	if !ok {
		m.mu.Unlock()
		return errors.New("deployment not found")
	}
	activeSlot := st.ActiveSlotCurrent
	m.mu.Unlock()

	if activeSlot != agentSlotGreen && activeSlot != agentSlotBlue {
		return fmt.Errorf("invalid active slot for health verification: %s", activeSlot)
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/health", portForSlot(activeSlot))
	return m.checkHealth(ctx, url, "")
}

func (m *AgentDeploymentManager) checkHealth(ctx context.Context, url, host string) error {
	hCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(hCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if host != "" {
		req.Host = host
		req.Header.Set("Host", host)
	}
	req.Header.Set("X-API-Key", m.apiKey)

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("health check failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	return nil
}

func (m *AgentDeploymentManager) update(id string, mutate func(st *AgentDeploymentStatus)) error {
	m.mu.Lock()
	st, ok := m.deployments[id]
	if !ok {
		m.mu.Unlock()
		return errors.New("deployment not found")
	}
	mutate(st)
	snapshot := cloneDeployment(st)
	callbackURL := st.callbackURL
	externalDeployID := st.externalDeployID
	m.mu.Unlock()

	m.emitCallbackStatus(snapshot, callbackURL, externalDeployID)
	return nil
}

func (m *AgentDeploymentManager) versionOf(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.deployments[id]; ok {
		return st.TargetVersion
	}
	return ""
}

func (m *AgentDeploymentManager) checksumOf(id string) *string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.deployments[id]; ok {
		return st.checksum
	}
	return nil
}

func (m *AgentDeploymentManager) sourceOf(id string) *string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.deployments[id]; ok {
		return st.source
	}
	return nil
}

func cloneDeployment(st *AgentDeploymentStatus) *AgentDeploymentStatus {
	if st == nil {
		return nil
	}
	copy := *st
	copy.checksum = nil
	copy.source = nil
	copy.host = ""
	copy.callbackURL = ""
	copy.externalDeployID = ""
	if st.Error != nil {
		errCopy := *st.Error
		copy.Error = &errCopy
	}
	if st.StartedAt != nil {
		started := *st.StartedAt
		copy.StartedAt = &started
	}
	if st.FinishedAt != nil {
		finished := *st.FinishedAt
		copy.FinishedAt = &finished
	}
	return &copy
}

func (m *AgentDeploymentManager) emitCallbackStatus(
	st *AgentDeploymentStatus,
	callbackURL string,
	externalDeployID string,
) {
	if st == nil || strings.TrimSpace(callbackURL) == "" {
		return
	}

	emitter := NewCallbackEmitter(callbackURL, m.apiKey)
	if emitter == nil {
		return
	}

	groupingID := st.DeploymentID
	if strings.TrimSpace(externalDeployID) != "" {
		groupingID = externalDeployID
	}

	event := DeploymentEvent{
		GroupingID:        groupingID,
		Action:            "update_agent",
		Scope:             "step",
		Step:              st.CurrentStep,
		Status:            "in_progress",
		Message:           fmt.Sprintf("agent update %s", st.CurrentStep),
		Error:             "",
		DeploymentID:      st.DeploymentID,
		ActiveSlotBefore:  st.ActiveSlotBefore,
		ActiveSlotCurrent: st.ActiveSlotCurrent,
		TargetVersion:     st.TargetVersion,
	}

	switch st.State {
	case "succeeded":
		event.Scope = "action"
		event.Status = "completed"
		event.Step = "completed"
		event.Message = "agent update completed"
	case "failed":
		event.Scope = "step"
		event.Status = "failed"
		event.Message = "agent update failed"
		if st.Error != nil {
			event.Error = *st.Error
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := emitter.EmitDeploymentEvent(ctx, event); err != nil {
		slog.Warn(
			"failed to emit agent update callback event",
			"deployment_id", st.DeploymentID,
			"grouping_id", groupingID,
			"url", callbackURL,
			"error", err,
		)
	}
}

type deploymentLock struct {
	file *os.File
}

func acquireDeploymentLock() (*deploymentLock, error) {
	f, err := os.OpenFile(agentDeployLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &deploymentLock{file: f}, nil
}

func (l *deploymentLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}

func otherSlot(slot string) string {
	if slot == agentSlotGreen {
		return agentSlotBlue
	}
	return agentSlotGreen
}

func serviceForSlot(slot string) string {
	if slot == agentSlotGreen {
		return agentServiceGreen
	}
	return agentServiceBlue
}

func portForSlot(slot string) int {
	if slot == agentSlotGreen {
		return agentGreenPort
	}
	return agentBluePort
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("sudo", append([]string{"-n", "systemctl"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("sudo -n systemctl %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func runningAgentSlot() (string, error) {
	greenActive, err := isServiceActive(agentServiceGreen)
	if err != nil {
		return "", err
	}
	blueActive, err := isServiceActive(agentServiceBlue)
	if err != nil {
		return "", err
	}

	switch {
	case greenActive && blueActive:
		return "", errors.New("both agent services are running; expected exactly one active service")
	case !greenActive && !blueActive:
		return "", errors.New("neither agent service is running; expected exactly one active service")
	case greenActive:
		return agentSlotGreen, nil
	default:
		return agentSlotBlue, nil
	}
}

func isServiceActive(service string) (bool, error) {
	cmd := exec.Command("sudo", "-n", "systemctl", "is-active", "--quiet", service)
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, fmt.Errorf("sudo -n systemctl is-active %s failed: %w", service, err)
	}
	return true, nil
}

func swapAgentBinary(sourcePath, destPath string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return "", err
	}

	backupPath := destPath + ".backup." + randomID()
	if err := os.Rename(destPath, backupPath); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("agent binary missing at %s", destPath)
		}
		return "", fmt.Errorf("failed to backup current binary: %w", err)
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		_ = os.Rename(backupPath, destPath)
		return "", fmt.Errorf("failed to open downloaded binary: %w", err)
	}
	defer source.Close()

	dest, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		_ = os.Rename(backupPath, destPath)
		return "", fmt.Errorf("failed to open install target: %w", err)
	}

	if _, err := io.Copy(dest, source); err != nil {
		_ = dest.Close()
		_ = os.Rename(backupPath, destPath)
		return "", fmt.Errorf("failed to write new binary: %w", err)
	}

	if err := dest.Close(); err != nil {
		_ = os.Rename(backupPath, destPath)
		return "", fmt.Errorf("failed to close new binary: %w", err)
	}

	return backupPath, nil
}

func restoreAgentBinary(destPath, backupPath string) error {
	if backupPath == "" {
		return nil
	}
	if err := os.Rename(backupPath, destPath); err != nil {
		return fmt.Errorf("failed to restore backup binary: %w", err)
	}
	return nil
}

func parseChecksumFromChecksumsFile(content []byte, fileName string) (string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		checksum := fields[0]
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if name == fileName {
			return checksum, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("checksum for %s not found in checksums file", fileName)
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
