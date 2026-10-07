package email

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/sendgrid/sendgrid-go"
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
	if err := plugin.RegisterTyped(scope, plugin.FuncOptions{Name: "send"}, p.send); err != nil {
		return err
	}
	if err := plugin.RegisterTyped(scope, plugin.FuncOptions{Name: "send_template"}, p.sendTemplate); err != nil {
		return err
	}
	if err := plugin.RegisterTyped(scope, plugin.FuncOptions{Name: "check_status"}, p.checkStatus); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----
//
// Exported (cleat#2626): these are the Req/Resp types the RegisterTyped
// call sites above publish, and cmd/cleat-gen plugin-client reads them off
// this real registration -- never a hand-maintained manifest -- to generate
// a typed caller-side client.

type SendInput struct {
	To       string   `json:"to"`
	Subject  string   `json:"subject"`
	BodyHTML string   `json:"body_html"`
	BodyText string   `json:"body_text,omitempty"`
	From     string   `json:"from,omitempty"`
	ReplyTo  string   `json:"reply_to,omitempty"`
	CC       []string `json:"cc,omitempty"`
	BCC      []string `json:"bcc,omitempty"`
}

type SendOutput struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

type SendTemplateInput struct {
	To           string         `json:"to"`
	TemplateID   string         `json:"template_id"`
	TemplateData map[string]any `json:"template_data"`
	From         string         `json:"from,omitempty"`
	ReplyTo      string         `json:"reply_to,omitempty"`
}

type SendTemplateOutput struct {
	MessageID string `json:"message_id"`
	Status    string `json:"status"`
}

type CheckStatusInput struct {
	MessageID string `json:"message_id"`
}

type StatusEvent struct {
	Timestamp string `json:"timestamp"`
	Event     string `json:"event"`
}

type CheckStatusOutput struct {
	Status string        `json:"status"`
	Events []StatusEvent `json:"events"`
}

// ---- Host functions ----

// send sends a single transactional email via SendGrid.
func (p *Plugin) send(ctx context.Context, input SendInput) (SendOutput, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return SendOutput{}, fmt.Errorf("email: no tenant context")
	}

	if input.To == "" {
		return SendOutput{}, fmt.Errorf("email: to is required")
	}
	if input.Subject == "" {
		return SendOutput{}, fmt.Errorf("email: subject is required")
	}
	if input.BodyHTML == "" {
		return SendOutput{}, fmt.Errorf("email: body_html is required")
	}

	from := input.From
	if from == "" {
		from = p.defaultFrom
	}
	if from == "" {
		return SendOutput{}, fmt.Errorf("email: from is required (set in input or plugin config)")
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

	apiKey, err := p.sendGridAPIKey(ctx)
	if err != nil {
		return SendOutput{}, err
	}
	response, err := sendgrid.NewSendClient(apiKey).SendWithContext(ctx, m)
	if err != nil {
		return SendOutput{}, fmt.Errorf("email: send failed: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return SendOutput{}, fmt.Errorf("email: SendGrid returned %d: %s", response.StatusCode, response.Body)
	}

	messageID := extractMessageID(response.Headers)

	p.logger.Info("email: sent successfully",
		"message_id", messageID,
		"status_code", response.StatusCode,
		"tenant", cc.TenantID,
	)

	return SendOutput{
		MessageID: messageID,
		Status:    "sent",
	}, nil
}

// sendTemplate sends an email using a pre-defined SendGrid template.
func (p *Plugin) sendTemplate(ctx context.Context, input SendTemplateInput) (SendTemplateOutput, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return SendTemplateOutput{}, fmt.Errorf("email: no tenant context")
	}

	if input.To == "" {
		return SendTemplateOutput{}, fmt.Errorf("email: to is required")
	}
	if input.TemplateID == "" {
		return SendTemplateOutput{}, fmt.Errorf("email: template_id is required")
	}

	from := input.From
	if from == "" {
		from = p.defaultFrom
	}
	if from == "" {
		return SendTemplateOutput{}, fmt.Errorf("email: from is required (set in input or plugin config)")
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

	apiKey, err := p.sendGridAPIKey(ctx)
	if err != nil {
		return SendTemplateOutput{}, err
	}
	response, err := sendgrid.NewSendClient(apiKey).SendWithContext(ctx, m)
	if err != nil {
		return SendTemplateOutput{}, fmt.Errorf("email: send template failed: %w", err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return SendTemplateOutput{}, fmt.Errorf("email: SendGrid returned %d: %s", response.StatusCode, response.Body)
	}

	messageID := extractMessageID(response.Headers)

	p.logger.Info("email: template sent successfully",
		"message_id", messageID,
		"status_code", response.StatusCode,
		"tenant", cc.TenantID,
	)

	return SendTemplateOutput{
		MessageID: messageID,
		Status:    "sent",
	}, nil
}

// checkStatus checks the delivery status of a sent email via the SendGrid
// Activity API. If the Activity API is not enabled for the account, it
// returns a best-effort "sent" status.
func (p *Plugin) checkStatus(ctx context.Context, input CheckStatusInput) (CheckStatusOutput, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return CheckStatusOutput{}, fmt.Errorf("email: no tenant context")
	}

	if input.MessageID == "" {
		return CheckStatusOutput{}, fmt.Errorf("email: message_id is required")
	}

	// Query the SendGrid Activity API for message status.
	req, err := http.NewRequestWithContext(ctx, "GET", sendgridActivityAPI, nil)
	if err != nil {
		return CheckStatusOutput{}, fmt.Errorf("email: create request: %w", err)
	}
	plugin.SetTraceparentFromContext(ctx, req)

	q := req.URL.Query()
	q.Add("limit", "10")
	q.Add("query", fmt.Sprintf(`msg_id="%s"`, input.MessageID))
	req.URL.RawQuery = q.Encode()

	apiKey, err := p.sendGridAPIKey(ctx)
	if err != nil {
		return CheckStatusOutput{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	p.logger.Info("email: checking delivery status",
		"message_id", input.MessageID,
		"tenant", cc.TenantID,
	)

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return CheckStatusOutput{}, fmt.Errorf("email: activity API request failed: %w", err)
	}
	defer resp.Body.Close()

	// If the Activity API is not available (403/404), return best-effort status.
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		p.logger.Warn("email: Activity API not available, returning best-effort status",
			"message_id", input.MessageID,
			"status_code", resp.StatusCode,
			"tenant", cc.TenantID,
		)
		return CheckStatusOutput{
			Status: "sent",
			Events: []StatusEvent{},
		}, nil
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return CheckStatusOutput{}, fmt.Errorf("email: activity API returned %d: %s", resp.StatusCode, string(body))
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
		return CheckStatusOutput{}, fmt.Errorf("email: decode activity response: %w", err)
	}

	if len(activityResp.Messages) == 0 {
		return CheckStatusOutput{
			Status: "unknown",
			Events: []StatusEvent{},
		}, nil
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

	events := []StatusEvent{}
	if msg.LastEvent != "" {
		events = append(events, StatusEvent{
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

	return CheckStatusOutput{
		Status: status,
		Events: events,
	}, nil
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
