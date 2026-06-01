package pagerdutyalert

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("pagerduty: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "trigger_incident"}, p.triggerIncident); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{Name: "resolve_incident"}, p.resolveIncident); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type triggerIncidentInput struct {
	ConfigID uuid.UUID `json:"config_id"`
	Summary  string    `json:"summary"`
	Severity string    `json:"severity"` // "critical", "error", or "warning"
	Source   string    `json:"source"`
	Details  *string   `json:"details,omitempty"`
}

type triggerIncidentOutput struct {
	IncidentKey string `json:"incident_key"`
	Status      string `json:"status"`
}

type resolveIncidentInput struct {
	ConfigID    uuid.UUID `json:"config_id"`
	IncidentKey string    `json:"incident_key"`
}

type resolveIncidentOutput struct {
	Status string `json:"status"`
}

// ---- PagerDuty Events API v2 types ----

type pdEventPayload struct {
	Summary       string                 `json:"summary"`
	Severity      string                 `json:"severity"`
	Source        string                 `json:"source"`
	CustomDetails map[string]interface{} `json:"custom_details,omitempty"`
}

type pdEventRequest struct {
	RoutingKey  string          `json:"routing_key"`
	EventAction string          `json:"event_action"`
	Payload     *pdEventPayload `json:"payload,omitempty"`
	DedupKey    string          `json:"dedup_key,omitempty"`
}

type pdEventResponse struct {
	Status      string   `json:"status"`
	DedupKey    string   `json:"dedup_key"`
	Message     string   `json:"message,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

const pdEventsAPIURL = "https://events.pagerduty.com/v2/enqueue"

// ---- Host functions ----

// triggerIncident creates a PagerDuty incident. It looks up the PagerDuty
// config by ID, verifies tenant ownership, and POSTs to the PagerDuty Events
// API v2.
func (p *Plugin) triggerIncident(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("pagerduty: no tenant context")
	}

	var input triggerIncidentInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("pagerduty: invalid input: %w", err)
	}
	if input.ConfigID == uuid.Nil {
		return "", fmt.Errorf("pagerduty: config_id is required")
	}
	if input.Summary == "" {
		return "", fmt.Errorf("pagerduty: summary is required")
	}
	if input.Severity == "" {
		return "", fmt.Errorf("pagerduty: severity is required")
	}
	if input.Source == "" {
		return "", fmt.Errorf("pagerduty: source is required")
	}

	// Validate severity.
	switch input.Severity {
	case "critical", "error", "warning":
	default:
		return "", fmt.Errorf("pagerduty: invalid severity: %q (must be critical, error, or warning)", input.Severity)
	}

	// Look up the PagerDuty config, verifying tenant ownership.
	var routingKey string
	err := p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT routing_key
			FROM pd_config
			WHERE id = $1 AND tenant_id = $2 AND enabled = true
		`, p.dialect), input.ConfigID, cc.TenantID).Scan(&routingKey)
	if err != nil {
		return "", fmt.Errorf("pagerduty: config not found or disabled")
	}

	// Build the custom_details from optional details field.
	customDetails := make(map[string]interface{})
	if input.Details != nil && *input.Details != "" {
		// Try to parse details as JSON for structured details.
		var structured interface{}
		if err := json.Unmarshal([]byte(*input.Details), &structured); err == nil {
			customDetails["details"] = structured
		} else {
			customDetails["details"] = *input.Details
		}
	}

	payload := pdEventRequest{
		RoutingKey:  routingKey,
		EventAction: "trigger",
		Payload: &pdEventPayload{
			Summary:       input.Summary,
			Severity:      input.Severity,
			Source:        input.Source,
			CustomDetails: customDetails,
		},
	}

	return p.postToPagerDuty(ctx, payload)
}

// resolveIncident resolves a PagerDuty incident. It looks up the PagerDuty
// config by ID, verifies tenant ownership, and POSTs a resolve event to the
// PagerDuty Events API v2.
func (p *Plugin) resolveIncident(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("pagerduty: no tenant context")
	}

	var input resolveIncidentInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("pagerduty: invalid input: %w", err)
	}
	if input.ConfigID == uuid.Nil {
		return "", fmt.Errorf("pagerduty: config_id is required")
	}
	if input.IncidentKey == "" {
		return "", fmt.Errorf("pagerduty: incident_key is required")
	}

	// Look up the PagerDuty config, verifying tenant ownership.
	var routingKey string
	err := p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT routing_key
			FROM pd_config
			WHERE id = $1 AND tenant_id = $2 AND enabled = true
		`, p.dialect), input.ConfigID, cc.TenantID).Scan(&routingKey)
	if err != nil {
		return "", fmt.Errorf("pagerduty: config not found or disabled")
	}

	payload := pdEventRequest{
		RoutingKey:  routingKey,
		EventAction: "resolve",
		DedupKey:    input.IncidentKey,
	}

	return p.postToPagerDuty(ctx, payload)
}

// postToPagerDuty sends an event to the PagerDuty Events API v2 and returns
// the parsed response.
func (p *Plugin) postToPagerDuty(ctx context.Context, req pdEventRequest) (string, error) {
	bodyBytes, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("pagerduty: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", pdEventsAPIURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("pagerduty: create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("pagerduty: API request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("pagerduty: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("pagerduty: API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var pdResp pdEventResponse
	if err := json.Unmarshal(respBody, &pdResp); err != nil {
		return "", fmt.Errorf("pagerduty: parse response: %w", err)
	}

	if req.EventAction == "trigger" {
		output := triggerIncidentOutput{
			IncidentKey: pdResp.DedupKey,
			Status:      pdResp.Status,
		}
		outJSON, _ := json.Marshal(output)
		return string(outJSON), nil
	}

	output := resolveIncidentOutput{
		Status: pdResp.Status,
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}
