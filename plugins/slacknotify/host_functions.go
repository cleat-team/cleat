package slacknotify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/internal/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("slack-notify: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "send_message"}, p.sendMessage); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type sendMessageInput struct {
	ConfigID uuid.UUID        `json:"config_id"`
	Channel  string           `json:"channel,omitempty"`
	Text     string           `json:"text"`
	Blocks   json.RawMessage  `json:"blocks,omitempty"`
}

type sendMessageOutput struct {
	Success bool   `json:"success"`
	TS      string `json:"ts,omitempty"`
}

// slackWebhookPayload is the JSON body sent to the Slack incoming webhook URL.
type slackWebhookPayload struct {
	Channel string          `json:"channel,omitempty"`
	Text    string          `json:"text"`
	Blocks  json.RawMessage `json:"blocks,omitempty"`
}

// slackWebhookResponse is the JSON response from the Slack webhook endpoint.
type slackWebhookResponse struct {
	OK bool   `json:"ok"`
	TS string `json:"ts,omitempty"`
}

// ---- Host functions ----

// sendMessage sends a Slack message via an incoming webhook. It looks up the
// Slack config by ID, verifies tenant ownership, and POSTs the message to
// the configured webhook URL.
func (p *Plugin) sendMessage(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == uuid.Nil {
		return "", fmt.Errorf("slack-notify: no tenant context")
	}

	var input sendMessageInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("slack-notify: invalid input: %w", err)
	}
	if input.ConfigID == uuid.Nil {
		return "", fmt.Errorf("slack-notify: config_id is required")
	}
	if input.Text == "" {
		return "", fmt.Errorf("slack-notify: text is required")
	}

	// Look up the Slack config, verifying tenant ownership.
	var webhookURL string
	var defaultChannel *string
	err := p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT webhook_url, default_channel
			FROM slack_config
			WHERE id = $1 AND tenant_id = $2 AND enabled = true
		`, p.dialect), input.ConfigID, cc.TenantID).Scan(&webhookURL, &defaultChannel)
	if err != nil {
		return "", fmt.Errorf("slack-notify: config not found or disabled")
	}

	// Determine the channel to use. If not specified in input, fall back to
	// the config's default_channel. If neither is set, omit channel from the
	// webhook payload (Slack will use what's configured for the webhook).
	channel := input.Channel
	if channel == "" && defaultChannel != nil {
		channel = *defaultChannel
	}

	payload := slackWebhookPayload{
		Channel: channel,
		Text:    input.Text,
		Blocks:  input.Blocks,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("slack-notify: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", webhookURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return "", fmt.Errorf("slack-notify: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("slack-notify: webhook request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("slack-notify: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("slack-notify: webhook returned %d: %s", resp.StatusCode, string(respBody))
	}

	// Parse the Slack response for the message timestamp.
	var slackResp slackWebhookResponse
	if err := json.Unmarshal(respBody, &slackResp); err != nil {
		// Slack webhooks may return plain "ok" without JSON. Treat non-JSON
		// responses as success if status was 2xx.
		output := sendMessageOutput{Success: true}
		outJSON, _ := json.Marshal(output)
		return string(outJSON), nil
	}

	output := sendMessageOutput{
		Success: slackResp.OK,
		TS:      slackResp.TS,
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}
