// Package api provides HTTP handlers for the API server.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// DeploymentEvent represents an event sent to the callback URL during deployment.
type DeploymentEvent struct {
	GroupingID        string `json:"-"`
	Timestamp         string `json:"-"`
	Action            string `json:"action"`
	Scope             string `json:"scope"`
	Step              string `json:"step,omitempty"`
	Status            string `json:"status"`
	Message           string `json:"message,omitempty"`
	Error             string `json:"error,omitempty"`
	DeploymentID      string `json:"deployment_id"`
	ActiveSlotBefore  string `json:"active_slot_before,omitempty"`
	ActiveSlotCurrent string `json:"active_slot_current,omitempty"`
	TargetVersion     string `json:"target_version,omitempty"`
}

// CallbackEmitter sends deployment events to a callback URL.
type CallbackEmitter struct {
	callbackURL  string
	apiKey       string
	deploymentID string
	client       *http.Client
}

// NewCallbackEmitter creates a new CallbackEmitter.
func NewCallbackEmitter(callbackURL, apiKey string) *CallbackEmitter {
	if callbackURL == "" {
		return nil
	}

	return &CallbackEmitter{
		callbackURL: callbackURL,
		apiKey:      apiKey,
		client:      &http.Client{Timeout: 3 * time.Minute},
	}
}

// WithDeploymentID configures a default deployment ID for callback events.
func (e *CallbackEmitter) WithDeploymentID(deploymentID string) *CallbackEmitter {
	if e == nil {
		return nil
	}
	copy := *e
	copy.deploymentID = strings.TrimSpace(deploymentID)
	return &copy
}

// EmitDeploymentEvent sends an event to the callback URL.
func (e *CallbackEmitter) EmitDeploymentEvent(ctx context.Context, event DeploymentEvent) error {
	if strings.TrimSpace(event.DeploymentID) == "" {
		event.DeploymentID = e.deploymentID
	}
	if strings.TrimSpace(event.Action) == "" ||
		strings.TrimSpace(event.Scope) == "" ||
		strings.TrimSpace(event.Status) == "" ||
		strings.TrimSpace(event.DeploymentID) == "" {
		return fmt.Errorf("callback event missing required fields")
	}

	payload, err := json.Marshal(event)
	if err != nil {
		slog.Error("failed to marshal callback event", "error", err)
		return err
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		e.callbackURL,
		bytes.NewReader(payload),
	)
	if err != nil {
		slog.Error("failed to create callback request", "error", err)
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AGENT-TOKEN", e.apiKey)

	resp, err := e.client.Do(req)
	if err != nil {
		slog.Error("failed to send callback event", "error", err, "url", e.callbackURL)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Warn("callback returned error status", "status", resp.StatusCode, "url", e.callbackURL)
		return errors.New("callback returned error status")
	}

	return nil
}
