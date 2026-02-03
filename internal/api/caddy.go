package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
