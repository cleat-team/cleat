package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/cleat-team/cleat/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("notifications: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "send_webhook", Idempotent: false}, p.sendWebhook); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type sendWebhookInput struct {
	WebhookID uuid.UUID          `json:"webhook_id"`
	EventType string             `json:"event_type"`
	Payload   json.RawMessage    `json:"payload"`
}

type sendWebhookOutput struct {
	DeliveryID uuid.UUID `json:"delivery_id"`
}

// ---- Host functions ----

// sendWebhook triggers a webhook delivery from within a workflow. It creates a
// delivery row in 'pending' status, which the background retry loop will pick
// up and deliver. Returns the delivery ID.
func (p *Plugin) sendWebhook(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("notifications: no tenant context")
	}

	var input sendWebhookInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("notifications: invalid input: %w", err)
	}
	if input.WebhookID == uuid.Nil {
		return "", fmt.Errorf("notifications: webhook_id is required")
	}
	if input.EventType == "" {
		return "", fmt.Errorf("notifications: event_type is required")
	}
	if input.Payload == nil {
		input.Payload = json.RawMessage("{}")
	}

	// Verify the webhook belongs to the tenant.
	var exists bool
	err := p.db.QueryRow(ctx, plugin.Rebind(`
			SELECT EXISTS(SELECT 1 FROM webhook_config WHERE id = $1 AND tenant_id = $2)
		`, p.dialect), input.WebhookID, cc.TenantID).Scan(&exists)
	if err != nil {
		return "", fmt.Errorf("notifications: verify webhook: %w", err)
	}
	if !exists {
		return "", fmt.Errorf("notifications: webhook not found: %s", input.WebhookID)
	}

	deliveryID := uuid.New()
	now := time.Now()

	_, err = p.db.Exec(ctx, plugin.Rebind(`
			INSERT INTO webhook_delivery (id, webhook_id, event_type, payload, status, attempt_count, next_attempt_at, created_at)
			VALUES ($1, $2, $3, $4, 'pending', 0, $5, $6)
		`, p.dialect), deliveryID, input.WebhookID, input.EventType, string(input.Payload), now, now)
	if err != nil {
		return "", fmt.Errorf("notifications: create delivery: %w", err)
	}

	p.logger.Info("notifications: delivery created",
		"delivery_id", deliveryID,
		"webhook_id", input.WebhookID,
		"event_type", input.EventType,
	)

	output := sendWebhookOutput{DeliveryID: deliveryID}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}
