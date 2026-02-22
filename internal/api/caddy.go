package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
	Dial   string `json:"dial"`
	Weight int    `json:"weight,omitempty"`
}

type AgentRouteState struct {
	RouteIndex int
	Host       string
	ActiveSlot string
	Route      map[string]any
}

// ConfigureRoute configures a Caddy reverse proxy route
func (cm *CaddyManager) ConfigureRoute(domain string, port int) error {
	routeID := fmt.Sprintf("crate_operator_%s", sanitizeDomain(domain))

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

func (cm *CaddyManager) GetAgentRouteState() (*AgentRouteState, error) {
	routes, err := cm.getRoutes()
	if err != nil {
		return nil, err
	}

	for i, route := range routes {
		routeMap, ok := route.(map[string]any)
		if !ok {
			continue
		}

		handle, ok := routeMap["handle"].([]any)
		if !ok {
			continue
		}

		rpHandle := findReverseProxyInHandles(handle)
		if rpHandle == nil {
			continue
		}

		upstreams, ok := rpHandle["upstreams"].([]any)
		if !ok || len(upstreams) < 2 {
			continue
		}

		weights, found := extractAgentWeights(rpHandle, upstreams)
		if !found {
			continue
		}

		activeSlot, err := activeSlotFromWeights(weights.green, weights.blue)
		if err != nil {
			return nil, err
		}

		host := extractRouteHost(routeMap)
		return &AgentRouteState{
			RouteIndex: i,
			Host:       host,
			ActiveSlot: activeSlot,
			Route:      routeMap,
		}, nil
	}

	return nil, errors.New(
		"agent route with localhost:9640 and localhost:9641 upstreams not found in Caddy config",
	)
}

// findReverseProxyInHandles searches handles (and nested subroute handles) for a reverse_proxy handler.
// Returns the handler map directly so callers can modify it in place.
func findReverseProxyInHandles(handles []any) map[string]any {
	for _, h := range handles {
		hMap, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if hMap["handler"] == "reverse_proxy" {
			return hMap
		}
		if hMap["handler"] == "subroute" {
			routes, ok := hMap["routes"].([]any)
			if !ok {
				continue
			}
			for _, r := range routes {
				rMap, ok := r.(map[string]any)
				if !ok {
					continue
				}
				subHandles, ok := rMap["handle"].([]any)
				if !ok {
					continue
				}
				if found := findReverseProxyInHandles(subHandles); found != nil {
					return found
				}
			}
		}
	}
	return nil
}

func (cm *CaddyManager) SetAgentTrafficSlot(slot string) error {
	targetGreen, targetBlue := 100, 0
	if slot == agentSlotBlue {
		targetGreen, targetBlue = 0, 100
	}

	state, err := cm.GetAgentRouteState()
	if err != nil {
		return err
	}

	handle, ok := state.Route["handle"].([]any)
	if !ok {
		return errors.New("invalid Caddy route handle")
	}

	rpHandle := findReverseProxyInHandles(handle)
	if rpHandle == nil {
		return errors.New("failed to find agent upstreams for weight update")
	}

	upstreams, ok := rpHandle["upstreams"].([]any)
	if !ok {
		return errors.New("failed to find agent upstreams for weight update")
	}

	greenIdx, blueIdx := -1, -1
	for idx, u := range upstreams {
		upstreamMap, ok := u.(map[string]any)
		if !ok {
			continue
		}
		dial, _ := upstreamMap["dial"].(string)
		switch dial {
		case fmt.Sprintf("localhost:%d", agentGreenPort):
			upstreamMap["weight"] = targetGreen
			greenIdx = idx
		case fmt.Sprintf("localhost:%d", agentBluePort):
			upstreamMap["weight"] = targetBlue
			blueIdx = idx
		}
	}

	if greenIdx == -1 || blueIdx == -1 {
		return errors.New("failed to find agent upstreams for weight update")
	}

	// Update load_balancing.selection_policy.weights if present (Caddyfile-configured routes).
	if lb, ok := rpHandle["load_balancing"].(map[string]any); ok {
		if sp, ok := lb["selection_policy"].(map[string]any); ok {
			if w, ok := sp["weights"].([]any); ok && greenIdx < len(w) && blueIdx < len(w) {
				w[greenIdx] = targetGreen
				w[blueIdx] = targetBlue
			}
		}
	}

	rpHandle["lb_policy"] = fmt.Sprintf("%d/%d", targetGreen, targetBlue)

	state.Route["handle"] = handle
	if err := cm.putRoute(state.RouteIndex, state.Route); err != nil {
		return err
	}

	verified, err := cm.GetAgentRouteState()
	if err != nil {
		return err
	}
	if verified.ActiveSlot != slot {
		return fmt.Errorf(
			"caddy weight verification failed: expected active slot %s, got %s",
			slot,
			verified.ActiveSlot,
		)
	}
	return nil
}

func (cm *CaddyManager) getRoutes() ([]any, error) {
	url := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes", cm.apiURL)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf(
			"caddy routes query failed: status=%d body=%s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	var routes []any
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		return nil, err
	}

	return routes, nil
}

func (cm *CaddyManager) putRoute(routeIndex int, route map[string]any) error {
	payload, err := json.Marshal(route)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/config/apps/http/servers/srv0/routes/%d", cm.apiURL, routeIndex)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"caddy route update failed: status=%d body=%s",
			resp.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}

	return nil
}

type agentWeights struct {
	green int
	blue  int
}

func extractAgentWeights(handle map[string]any, upstreams []any) (agentWeights, bool) {
	weights := agentWeights{}
	foundGreen := false
	foundBlue := false
	greenIdx := -1
	blueIdx := -1

	for idx, u := range upstreams {
		upstreamMap, ok := u.(map[string]any)
		if !ok {
			continue
		}
		dial, _ := upstreamMap["dial"].(string)
		weight, hasWeight := upstreamWeight(upstreamMap)

		switch dial {
		case fmt.Sprintf("localhost:%d", agentGreenPort):
			if hasWeight {
				weights.green = weight
			}
			foundGreen = true
			greenIdx = idx
		case fmt.Sprintf("localhost:%d", agentBluePort):
			if hasWeight {
				weights.blue = weight
			}
			foundBlue = true
			blueIdx = idx
		}
	}

	if !foundGreen || !foundBlue {
		return weights, false
	}

	if weights.green != 0 || weights.blue != 0 {
		return weights, true
	}

	policyGreen, policyBlue, ok := policyWeights(handle)
	if ok {
		return agentWeights{green: policyGreen, blue: policyBlue}, true
	}

	lbGreen, lbBlue, ok := loadBalancingWeights(handle, greenIdx, blueIdx)
	if ok {
		return agentWeights{green: lbGreen, blue: lbBlue}, true
	}

	return weights, false
}

func loadBalancingWeights(handle map[string]any, greenIdx, blueIdx int) (int, int, bool) {
	lb, ok := handle["load_balancing"].(map[string]any)
	if !ok {
		return 0, 0, false
	}
	sp, ok := lb["selection_policy"].(map[string]any)
	if !ok {
		return 0, 0, false
	}
	raw, ok := sp["weights"].([]any)
	if !ok || greenIdx >= len(raw) || blueIdx >= len(raw) {
		return 0, 0, false
	}
	green, ok1 := jsonInt(raw[greenIdx])
	blue, ok2 := jsonInt(raw[blueIdx])
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return green, blue, true
}

func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

func upstreamWeight(upstream map[string]any) (int, bool) {
	raw, ok := upstream["weight"]
	if !ok {
		return 0, false
	}

	switch v := raw.(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		parsed, err := strconv.Atoi(v)
		if err == nil {
			return parsed, true
		}
	}

	return 0, false
}

func policyWeights(handle map[string]any) (int, int, bool) {
	raw, ok := handle["lb_policy"]
	if !ok {
		return 0, 0, false
	}
	s, ok := raw.(string)
	if !ok {
		return 0, 0, false
	}
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) != 2 {
		return 0, 0, false
	}
	green, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, false
	}
	blue, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, false
	}
	return green, blue, true
}

func activeSlotFromWeights(green, blue int) (string, error) {
	switch {
	case (green == 1 || green == 100) && blue == 0:
		return agentSlotGreen, nil
	case green == 0 && (blue == 1 || blue == 100):
		return agentSlotBlue, nil
	default:
		return "", fmt.Errorf("invalid agent upstream weights: green=%d blue=%d", green, blue)
	}
}

func extractRouteHost(route map[string]any) string {
	matches, ok := route["match"].([]any)
	if !ok {
		return ""
	}
	for _, m := range matches {
		matcher, ok := m.(map[string]any)
		if !ok {
			continue
		}
		hosts, ok := matcher["host"].([]any)
		if !ok || len(hosts) == 0 {
			continue
		}
		host, _ := hosts[0].(string)
		if host != "" {
			return host
		}
	}
	return ""
}

func sanitizeDomain(domain string) string {
	domain = strings.ReplaceAll(domain, ".", "_")
	domain = strings.ReplaceAll(domain, "-", "_")
	return domain
}
