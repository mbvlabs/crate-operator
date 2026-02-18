package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	agentServiceGreen   = "deploy-crate-agent-green.service"
	agentServiceBlue    = "deploy-crate-agent-blue.service"
	agentDeployStateDir = "/opt/deploy-crate/agent"
	agentDeployState    = "/opt/deploy-crate/agent/deployments.json"
	agentDeployLockPath = "/tmp/deploy-crate-agent-deploy.lock"
)

type AgentDeployRequest struct {
	Version                string  `json:"version"`
	Checksum               *string `json:"checksum,omitempty"`
	RequestID              *string `json:"request_id,omitempty"`
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

	checksum *string
	source   *string
	host     string
}

type agentDeployStateFile struct {
	Deployments  map[string]*AgentDeploymentStatus `json:"deployments"`
	RequestIndex map[string]string                 `json:"request_index"`
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
	m.load()
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
		DeploymentID:  deploymentID,
		RequestID:     requestID,
		TargetVersion: version,
		State:         "queued",
		CurrentStep:   "queued",
		UpdatedAt:     now,
		checksum:      req.Checksum,
		source:        req.OverrideArtifactSource,
	}

	if requestID != "" {
		m.requestIndex[requestID] = deploymentID
	}
	m.deployments[deploymentID] = status
	m.persistLocked()
	m.mu.Unlock()

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
	state, err := cm.GetAgentRouteState()
	if err != nil {
		m.fail(id, "read_active_slot", err)
		return
	}
	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "read_active_slot"
		st.ActiveSlotBefore = state.ActiveSlot
		st.ActiveSlotCurrent = state.ActiveSlot
		st.host = state.Host
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}

	inactiveSlot := otherSlot(state.ActiveSlot)
	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "determine_inactive_slot"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}

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
	_ = tempFile.Close()
	defer os.Remove(tempPath)

	source := agentArtifactSource
	if override := m.sourceOf(id); override != nil && strings.TrimSpace(*override) != "" {
		source = strings.TrimSpace(*override)
	}
	binaryURL, checksumURL, err := buildArtifactURLs(source, m.versionOf(id), agentBinaryName())
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
		expectedChecksum = string(checksumBytes)
	}
	if err := verifyChecksum(tempPath, expectedChecksum); err != nil {
		m.fail(id, "verify_checksum", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "install_inactive_slot"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := installBinaryToSlot(tempPath, inactiveSlot); err != nil {
		m.beforeCutoverFailure(id, inactiveSlot, "install_inactive_slot", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "start_inactive_slot_service"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := runSystemctl("start", serviceForSlot(inactiveSlot)); err != nil {
		m.beforeCutoverFailure(id, inactiveSlot, "start_inactive_slot_service", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "verify_inactive_slot_readiness"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := m.verifySlotHealth(ctx, inactiveSlot); err != nil {
		m.beforeCutoverFailure(id, inactiveSlot, "verify_inactive_slot_readiness", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "switch_traffic_to_inactive"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := cm.SetAgentTrafficSlot(inactiveSlot); err != nil {
		m.fail(id, "switch_traffic_to_inactive", err)
		return
	}
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
		m.fail(id, "verify_public_path_after_cutover", err)
		return
	}

	prevActive := state.ActiveSlot
	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "install_previous_active_slot"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := installBinaryToSlot(tempPath, prevActive); err != nil {
		m.afterFirstCutoverFailure(id, "install_previous_active_slot", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "restart_previous_active_slot_service"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := runSystemctl("restart", serviceForSlot(prevActive)); err != nil {
		m.afterFirstCutoverFailure(id, "restart_previous_active_slot_service", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "verify_restarted_slot_readiness"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := m.verifySlotHealth(ctx, prevActive); err != nil {
		m.afterFirstCutoverFailure(id, "verify_restarted_slot_readiness", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "switch_traffic_to_green"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := cm.SetAgentTrafficSlot(agentSlotGreen); err != nil {
		m.afterFirstCutoverFailure(id, "switch_traffic_to_green", err)
		return
	}
	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.ActiveSlotCurrent = agentSlotGreen
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "verify_public_path_after_cutback"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := m.verifyPublicHealth(ctx, id); err != nil {
		_ = m.update(id, func(st *AgentDeploymentStatus) {
			st.State = "rolling_back"
			st.CurrentStep = "rollback_to_blue"
			st.UpdatedAt = time.Now().UTC()
		})
		_ = cm.SetAgentTrafficSlot(agentSlotBlue)
		_ = m.update(id, func(st *AgentDeploymentStatus) {
			st.ActiveSlotCurrent = agentSlotBlue
			st.UpdatedAt = time.Now().UTC()
		})
		m.fail(id, "verify_public_path_after_cutback", err)
		return
	}

	if err := m.update(id, func(st *AgentDeploymentStatus) {
		st.CurrentStep = "stop_blue_service"
		st.UpdatedAt = time.Now().UTC()
	}); err != nil {
		return
	}
	if err := runSystemctl("stop", serviceForSlot(agentSlotBlue)); err != nil {
		m.fail(id, "stop_blue_service", err)
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

func (m *AgentDeploymentManager) beforeCutoverFailure(id, slot, step string, err error) {
	_ = runSystemctl("stop", serviceForSlot(slot))
	m.fail(id, step, err)
}

func (m *AgentDeploymentManager) afterFirstCutoverFailure(id, step string, err error) {
	m.fail(id, step, err)
}

func (m *AgentDeploymentManager) fail(id, step string, err error) {
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
	defer m.mu.Unlock()
	st, ok := m.deployments[id]
	if !ok {
		return errors.New("deployment not found")
	}
	mutate(st)
	m.persistLocked()
	return nil
}

func (m *AgentDeploymentManager) persistLocked() {
	if err := os.MkdirAll(agentDeployStateDir, 0o755); err != nil {
		return
	}

	state := agentDeployStateFile{
		Deployments:  map[string]*AgentDeploymentStatus{},
		RequestIndex: map[string]string{},
	}
	for k, v := range m.deployments {
		copy := cloneDeployment(v)
		state.Deployments[k] = copy
	}
	for k, v := range m.requestIndex {
		state.RequestIndex[k] = v
	}

	payload, err := json.Marshal(state)
	if err != nil {
		return
	}

	tmp := agentDeployState + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, agentDeployState)
}

func (m *AgentDeploymentManager) load() {
	payload, err := os.ReadFile(agentDeployState)
	if err != nil {
		return
	}

	var state agentDeployStateFile
	if err := json.Unmarshal(payload, &state); err != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if state.Deployments != nil {
		m.deployments = state.Deployments
	}
	if state.RequestIndex != nil {
		m.requestIndex = state.RequestIndex
	}

	for _, st := range m.deployments {
		if st.State == "running" || st.State == "queued" || st.State == "rolling_back" {
			now := time.Now().UTC()
			msg := "agent restarted during deployment; marked failed"
			st.State = "failed"
			st.Error = &msg
			st.FinishedAt = &now
			st.UpdatedAt = now
		}
	}
	m.persistLocked()
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

func installBinaryToSlot(sourcePath, slot string) error {
	slotDir := filepath.Join("/opt/deploy-crate/slots", slot)
	if err := os.MkdirAll(slotDir, 0o755); err != nil {
		return err
	}

	destPath := filepath.Join(slotDir, "crate-operator")
	tempDest := fmt.Sprintf("%s.tmp.%s", destPath, randomID())

	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()

	dest, err := os.OpenFile(tempDest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}

	if _, err := io.Copy(dest, source); err != nil {
		_ = dest.Close()
		_ = os.Remove(tempDest)
		return err
	}

	if err := dest.Close(); err != nil {
		_ = os.Remove(tempDest)
		return err
	}

	if err := os.Rename(tempDest, destPath); err != nil {
		_ = os.Remove(tempDest)
		return err
	}

	return nil
}

func runSystemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
