package webhookingest

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cleat-team/cleat/plugin"
	"github.com/google/uuid"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry. The plugin name is implicit -- each plugin gets its own
// scope, so function names need not be globally unique.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("webhook-ingest: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "await_webhook", Idempotent: true}, p.awaitWebhook); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type awaitWebhookInput struct {
	SourceID  string `json:"source_id"`
	EventType string `json:"event_type,omitempty"`
}

type awaitWebhookOutput struct {
	Found      bool            `json:"found"`
	ID         string          `json:"id,omitempty"`
	EventType  string          `json:"event_type,omitempty"`
	Payload    json.RawMessage `json:"payload,omitempty"`
	ReceivedAt string          `json:"received_at,omitempty"`
}

// ---- Host functions ----

// awaitWebhook queries for the latest matching webhook event for the workflow's
// tenant. If a matching event is found, it is marked as processed and returned.
// If none is found, the output {"found": false} is returned and the workflow
// engine will retry according to its retry policy.
func (p *Plugin) awaitWebhook(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == "" {
		return "", fmt.Errorf("webhook-ingest: no tenant context")
	}

	var input awaitWebhookInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("webhook-ingest: invalid input: %w", err)
	}

	// Parse source_id if provided.
	var sourceID uuid.UUID
	if input.SourceID != "" {
		var err error
		sourceID, err = uuid.Parse(input.SourceID)
		if err != nil {
			return "", fmt.Errorf("webhook-ingest: invalid source_id: %w", err)
		}
	}

	// Build query for the latest matching unprocessed event.
	query := `
		SELECT id, event_type, payload, received_at
		FROM webhook_events
		WHERE tenant_id = $1 AND processed = false
	`
	args := []any{cc.TenantID}
	argIdx := 2

	if sourceID != uuid.Nil {
		query += fmt.Sprintf(" AND source_id = $%d", argIdx)
		args = append(args, sourceID)
		argIdx++
	}
	if input.EventType != "" {
		query += fmt.Sprintf(" AND event_type = $%d", argIdx)
		args = append(args, input.EventType)
		argIdx++
	}

	query += " ORDER BY received_at DESC LIMIT 1"

	var (
		eventID    uuid.UUID
		eventType  string
		payloadRaw []byte
		receivedAt time.Time
	)

	err := p.db.QueryRow(ctx, plugin.Rebind(query, p.dialect), args...).Scan(
		&eventID, &eventType, &payloadRaw, &receivedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		output := awaitWebhookOutput{Found: false}
		outJSON, _ := json.Marshal(output)
		return string(outJSON), nil
	}
	if err != nil {
		return "", fmt.Errorf("webhook-ingest: query events: %w", err)
	}

	// Mark the event as processed.
	_, err = p.db.Exec(ctx, plugin.Rebind(`
		UPDATE webhook_events SET processed = true WHERE id = $1
	`, p.dialect), eventID)
	if err != nil {
		p.logger.Error("webhook-ingest: mark processed", "event_id", eventID, "error", err)
		// Continue even if marking fails -- the event will be returned and
		// the workflow will make progress.
	}

	p.logger.Info("webhook-ingest: event consumed",
		"event_id", eventID,
		"event_type", eventType,
		"tenant", cc.TenantID,
	)

	output := awaitWebhookOutput{
		Found:      true,
		ID:         eventID.String(),
		EventType:  eventType,
		Payload:    json.RawMessage(payloadRaw),
		ReceivedAt: receivedAt.Format(time.RFC3339),
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}
