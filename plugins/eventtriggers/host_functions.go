package eventtriggers

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/rcownie/durable/internal/plugin"
)

// RegisterHostFunctions registers workflow-callable functions on the scoped
// function registry under the "event-triggers" plugin namespace.
func (p *Plugin) RegisterHostFunctions(scope plugin.FuncRegistry) error {
	if scope == nil {
		return fmt.Errorf("event-triggers: nil function registry")
	}
	if err := scope.Register(plugin.FuncOptions{Name: "await_event", Idempotent: true}, p.awaitEvent); err != nil {
		return err
	}
	return nil
}

// ---- Input/output types ----

type awaitEventInput struct {
	EventType string `json:"event_type"`
	TimeoutMs int64  `json:"timeout_ms"`
}

type awaitEventOutput struct {
	Found       bool              `json:"found"`
	EventID     string            `json:"event_id,omitempty"`
	EventType   string            `json:"event_type,omitempty"`
	EventData   json.RawMessage   `json:"event_data,omitempty"`
	ReceivedAt  string            `json:"received_at,omitempty"`
}

// ---- Host functions ----

// awaitEvent queries for the latest matching unprocessed event for the
// workflow's tenant.  If a matching event is found, it is returned and the
// workflow proceeds.  If none is found, the output {"found": false} is
// returned and the workflow engine will retry according to its retry policy.
//
// The publish handler (handlePublishEvent) also broadcasts a signal named
// "__evt:<eventType>" so that workflows awaiting this event type can be
// woken up promptly instead of waiting for the next poll cycle.
func (p *Plugin) awaitEvent(ctx context.Context, inputJSON string) (string, error) {
	cc := plugin.CallContextFromContext(ctx)
	if cc == nil || cc.TenantID == uuid.Nil {
		return "", fmt.Errorf("event-triggers: no tenant context")
	}

	var input awaitEventInput
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		return "", fmt.Errorf("event-triggers: invalid input: %w", err)
	}
	if input.EventType == "" {
		return "", fmt.Errorf("event-triggers: event_type is required")
	}

	// Query for the latest matching unprocessed event for this tenant + type.
	var (
		eventID    uuid.UUID
		eventType  string
		eventData  []byte
		receivedAt time.Time
	)

	err := p.db.QueryRowContext(ctx, `
		SELECT id, event_type, event_data, received_at
		FROM ingested_events
		WHERE tenant_id = $1
		  AND event_type = $2
		  AND NOT processed
		ORDER BY received_at DESC
		LIMIT 1
	`, cc.TenantID, input.EventType).Scan(&eventID, &eventType, &eventData, &receivedAt)

	if err == sql.ErrNoRows {
		// No matching event found -- register as an awaiter so the publish
		// handler can signal this workflow when a matching event arrives.
		if cc.WorkflowID != "" {
			p.registerAwaiter(ctx, cc.TenantID, cc.WorkflowID, input.EventType)
		}

		output := awaitEventOutput{Found: false}
		outJSON, _ := json.Marshal(output)
		return string(outJSON), nil
	}
	if err != nil {
		return "", fmt.Errorf("event-triggers: query events: %w", err)
	}

	// Mark the event as consumed.
	_, err = p.db.ExecContext(ctx, `
		UPDATE ingested_events
		SET processed = true, status = 'consumed'
		WHERE id = $1
	`, eventID)
	if err != nil {
		p.logger.Error("event-triggers: mark event consumed", "event_id", eventID, "error", err)
		// Continue even if marking fails.
	}

	p.logger.Info("event-triggers: event consumed via await_event",
		"event_id", eventID,
		"event_type", eventType,
		"tenant", cc.TenantID,
		"workflow_id", cc.WorkflowID,
	)

	// Clean up any pending awaiter registration for this workflow + event type.
	unregisterAwaiter(ctx, p.db, p.logger, cc.WorkflowID, input.EventType)

	output := awaitEventOutput{
		Found:      true,
		EventID:    eventID.String(),
		EventType:  eventType,
		EventData:  json.RawMessage(eventData),
		ReceivedAt: receivedAt.Format(time.RFC3339),
	}
	outJSON, _ := json.Marshal(output)
	return string(outJSON), nil
}

// registerAwaiter records that the given workflow is waiting for an event of
// the specified type.  This allows the publish handler to deliver a signal
// when a matching event arrives.
func (p *Plugin) registerAwaiter(ctx context.Context, tenantID uuid.UUID, workflowID, eventType string) {
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO event_awaiters (workflow_id, tenant_id, event_type, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (workflow_id, event_type) DO UPDATE
			SET created_at = NOW()
	`, workflowID, tenantID, eventType)
	if err != nil {
		p.logger.Warn("event-triggers: register awaiter", "error", err, "workflow_id", workflowID)
	}
}

