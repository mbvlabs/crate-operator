// Package api provides HTTP handlers for the API server.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// DeploymentEvent represents an event sent to the callback URL during deployment.
type DeploymentEvent struct {
	GroupingID        string `json:"grouping_id"`
	Timestamp         string `json:"timestamp"`
	Action            string `json:"action"`
	Scope             string `json:"scope"`
	Step              string `json:"step"`
	Status            string `json:"status"`
	Message           string `json:"message"`
	Error             string `json:"error"`
	DeploymentID      string `json:"deployment_id,omitempty"`
	ActiveSlotBefore  string `json:"active_slot_before,omitempty"`
	ActiveSlotCurrent string `json:"active_slot_current,omitempty"`
	TargetVersion     string `json:"target_version,omitempty"`
}

// CallbackEmitter sends deployment events to a callback URL.
type CallbackEmitter struct {
	callbackURL string
	apiKey      string
	client      *http.Client
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

// EmitDeploymentEvent sends an event to the callback URL.
func (e *CallbackEmitter) EmitDeploymentEvent(ctx context.Context, event DeploymentEvent) error {
	event.Timestamp = time.Now().Format(time.RFC3339)
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
