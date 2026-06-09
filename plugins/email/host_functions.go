package email

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sendgrid/sendgrid-go/helpers/mail"

	"github.com/cleat-team/cleat/plugin"
)

const sendgridActivityAPI = "https://api.sendgrid.com/v3/messages"

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("email: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "send"}, p.send); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{Name: "send_template"}, p.sendTemplate); err != nil {
		return err
	}
	if err := scope.Register(plugin.FuncOptions{Name: "check_status"}, p.checkStatus); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type sendInput struct {
	To       string   `json:"to"`
	Subject  string   `json:"subject"`
	BodyHTML string   `json:"body_html"`
	BodyText string   `json:"body_text,omitempty"`
	From     string   `json:"from,omitempty"`
	ReplyTo  string   `json:"reply_to,omitempty"`
	CC       []string `json:"cc,omitempty"`
	BCC      []string `json:"bcc,omitempty"`
}

type sendOutput struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

type sendTemplateInput struct {
	To           string         `json:"to"`
	TemplateID   string         `json:"template_id"`
	TemplateData map[string]any `json:"template_data"`
	From         string         `json:"from,omitempty"`
	ReplyTo      string         `json:"reply_to,omitempty"`
}

type sendTemplateOutput struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

type checkStatusInput struct {
	MessageID string `json:"message_id"`
}

type statusEvent struct {
	Timestamp string `json:"timestamp"`
	Event     string `json:"event"`
}

type checkStatusOutput struct {
	Status string        `json:"status"`
	Events []statusEvent `json:"events"`
}

// ---- Host functions ----

// send sends a single transactional email via SendGrid.
func (p *Plugin) send(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("email: no tenant context")
	}

	var input sendInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("email: invalid input: %w", err)
	}
	if input.To == "" {
		return "", fmt.Errorf("email: to is required")
	}
	if input.Subject == "" {
		return "", fmt.Errorf("email: subject is required")
	}
	if input.BodyHTML == "" {
		return "", fmt.Errorf("email: body_html is required")
	}

	from := input.From
	if from == "" {
		from = p.defaultFrom
	}
	if from == "" {
		return "", fmt.Errorf("email: from is required (set in input or plugin config)")
	}

	fromEmail := mail.NewEmail("", from)
	toEmail := mail.NewEmail("", input.To)

	m := mail.NewV3Mail()
	m.SetFrom(fromEmail)
	m.Subject = input.Subject

	// Add content.
	if input.BodyHTML != "" {
		m.AddContent(mail.NewContent("text/html", input.BodyHTML))
	}
	if input.BodyText != "" {
		m.AddContent(mail.NewContent("text/plain", input.BodyText))
	}

	// Build personalization.
	personalization := mail.NewPersonalization()
	personalization.AddTos(toEmail)

	for _, ccAddr := range input.CC {
		if ccAddr != "" {
			personalization.AddCCs(mail.NewEmail("", ccAddr))
		}
	}
	for _, bccAddr := range input.BCC {
		if bccAddr != "" {
			personalization.AddBCCs(mail.NewEmail("", bccAddr))
		}
	}
	m.AddPersonalizations(personalization)

	// Set reply-to if provided.
	if input.ReplyTo != "" {
		m.SetReplyTo(mail.NewEmail("", input.ReplyTo))
	}

	p.logger.Info("email: sending email",
		"to", input.To, "subject", input.Subject,
		"cc", len(input.CC), "bcc", len(input.BCC),
		"tenant", cc.TenantID,
	)

	response, err := p.client.SendWithContext(ctx, m)
	if err != nil {
		return "", fmt.Errorf("email: send failed: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("email: SendGrid returned %d: %s", response.StatusCode, response.Body)
	}

	messageID := extractMessageID(response.Headers)

	p.logger.Info("email: sent successfully",
		"message_id", messageID,
		"status_code", response.StatusCode,
		"tenant", cc.TenantID,
	)

	output := sendOutput{
		MessageID: messageID,
		Status:    "sent",
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}

// sendTemplate sends an email using a pre-defined SendGrid template.
func (p *Plugin) sendTemplate(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("email: no tenant context")
	}

	var input sendTemplateInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("email: invalid input: %w", err)
	}
	if input.To == "" {
		return "", fmt.Errorf("email: to is required")
	}
	if input.TemplateID == "" {
		return "", fmt.Errorf("email: template_id is required")
	}

	from := input.From
	if from == "" {
		from = p.defaultFrom
	}
	if from == "" {
		return "", fmt.Errorf("email: from is required (set in input or plugin config)")
	}

	fromEmail := mail.NewEmail("", from)
	toEmail := mail.NewEmail("", input.To)

	m := mail.NewV3Mail()
	m.SetFrom(fromEmail)
	m.SetTemplateID(input.TemplateID)

	personalization := mail.NewPersonalization()
	personalization.AddTos(toEmail)

	// Set dynamic template data.
	for k, v := range input.TemplateData {
		personalization.SetDynamicTemplateData(k, v)
	}

	m.AddPersonalizations(personalization)

	if input.ReplyTo != "" {
		m.SetReplyTo(mail.NewEmail("", input.ReplyTo))
	}

	p.logger.Info("email: sending template",
		"to", input.To, "template_id", input.TemplateID,
		"tenant", cc.TenantID,
	)

	response, err := p.client.SendWithContext(ctx, m)
	if err != nil {
		return "", fmt.Errorf("email: send template failed: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("email: SendGrid returned %d: %s", response.StatusCode, response.Body)
	}

	messageID := extractMessageID(response.Headers)

	p.logger.Info("email: template sent successfully",
		"message_id", messageID,
		"status_code", response.StatusCode,
		"tenant", cc.TenantID,
	)

	output := sendTemplateOutput{
		MessageID: messageID,
		Status:    "sent",
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}

// checkStatus checks the delivery status of a sent email via the SendGrid
// Activity API. If the Activity API is not enabled for the account, it
// returns a best-effort "sent" status.
func (p *Plugin) checkStatus(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("email: no tenant context")
	}

	var input checkStatusInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("email: invalid input: %w", err)
	}
	if input.MessageID == "" {
		return "", fmt.Errorf("email: message_id is required")
	}

	// Query the SendGrid Activity API for message status.
	req, err := http.NewRequestWithContext(ctx, "GET", sendgridActivityAPI, nil)
	if err != nil {
		return "", fmt.Errorf("email: create request: %w", err)
	}

	q := req.URL.Query()
	q.Add("limit", "10")
	q.Add("query", fmt.Sprintf(`msg_id="%s"`, input.MessageID))
	req.URL.RawQuery = q.Encode()

	req.Header.Set("Authorization", "Bearer "+p.apiKey)

	p.logger.Info("email: checking delivery status",
		"message_id", input.MessageID,
		"tenant", cc.TenantID,
	)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("email: activity API request failed: %w", err)
	}
	defer resp.Body.Close()

	// If the Activity API is not available (403/404), return best-effort status.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		p.logger.Warn("email: Activity API not available, returning best-effort status",
			"message_id", input.MessageID,
			"status_code", resp.StatusCode,
			"tenant", cc.TenantID,
		)
		output := checkStatusOutput{
			Status: "sent",
			Events: []statusEvent{},
		}
		outJSON, _ := json.Marshal(output)
		return string(outJSON), nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("email: activity API returned %d: %s", resp.StatusCode, string(body))
	}

	var activityResp struct {
		Messages []struct {
			MsgID       string `json:"msg_id"`
			Status      string `json:"status"`
			LastEvent   string `json:"last_event_time"`
			OpensCount  int    `json:"opens_count"`
			ClicksCount int    `json:"clicks_count"`
		} `json:"messages"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&activityResp); err != nil {
		return "", fmt.Errorf("email: decode activity response: %w", err)
	}

	if len(activityResp.Messages) == 0 {
		output := checkStatusOutput{
			Status: "unknown",
			Events: []statusEvent{},
		}
		outJSON, _ := json.Marshal(output)
		return string(outJSON), nil
	}

	msg := activityResp.Messages[0]

	// Derive a high-level status from the message data.
	status := msg.Status
	if status == "" {
		switch {
		case msg.OpensCount > 0:
			status = "opened"
		case msg.ClicksCount > 0:
			status = "clicked"
		default:
			status = "delivered"
		}
	}

	events := []statusEvent{}
	if msg.LastEvent != "" {
		events = append(events, statusEvent{
			Timestamp: msg.LastEvent,
			Event:     status,
			// We only add the most recent event for brevity.
		})
	}

	p.logger.Info("email: delivery status retrieved",
		"message_id", input.MessageID,
		"status", status,
		"tenant", cc.TenantID,
	)

	output := checkStatusOutput{
		Status: status,
		Events: events,
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}

// extractMessageID extracts the X-Message-Id value from the SendGrid
// response headers. The headers map may use any casing for the key.
func extractMessageID(headers map[string][]string) string {
	// http.Header canonical form: "X-Message-Id"
	if vals, ok := headers["X-Message-Id"]; ok && len(vals) > 0 {
		return vals[0]
	}
	// Fallback to lowercase (how Go's http client stores it when
	// the response is processed outside the standard library).
	if vals, ok := headers["x-message-id"]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}
