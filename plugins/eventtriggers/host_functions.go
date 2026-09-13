package eventtriggers

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
	Found      bool            `json:"found"`
	EventID    string          `json:"event_id,omitempty"`
	EventType  string          `json:"event_type,omitempty"`
	EventData  json.RawMessage `json:"event_data,omitempty"`
	ReceivedAt string          `json:"received_at,omitempty"`
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
	if cc == nil || cc.TenantID == "" {
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

	err := plugin.ScanRow(p.db.QueryRow(ctx,
		queryLatestUnprocessedEvent.For(p.dialect),
		cc.TenantID, input.EventType), &eventID, &eventType, &eventData, &receivedAt)

	if errors.Is(err, sql.ErrNoRows) {
		// No matching event found -- register as an awaiter so the publish
		// handler can signal this workflow when a matching event arrives.
		if cc.WorkflowID != "" {
			// Not `Found: false` on failure: that is a success report, and it
			// is exactly the lie cleat#1473 is about.
			if err := p.registerAwaiter(ctx, cc.TenantID, cc.WorkflowID, input.EventType); err != nil {
				return "", err
			}
		}

		output := awaitEventOutput{Found: false}
		outJSON, _ := json.Marshal(output)
		return string(outJSON), nil
	}
	if err != nil {
		return "", fmt.Errorf("event-triggers: query events: %w", err)
	}

	// Mark the event as consumed.
	_, err = p.db.Exec(ctx, `
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
// RETURNS ITS ERROR, and that is the whole of cleat#1473.
//
// It used to log and return nothing, so awaitEvent's caller could not tell a
// registration that happened from one that did not -- and awaitEvent went on
// to return a SUCCESSFUL `{"found": false}` either way. A workflow told "no
// event yet" settles down to wait, and the row that would have woken it does
// not exist. The failure was observed and then discarded into a log line, which
// is the worst place for it: the run hangs and nothing above it knows why.
//
// The caller propagates rather than degrading. awaitEvent already fails loudly
// for every other database error on this path -- the `query events` branch
// returns its error -- so registration was the one write whose failure was
// swallowed, and propagating makes the function uniform. A visible error beats
// an invisible wait.
func (p *Plugin) registerAwaiter(ctx context.Context, tenantID, workflowID, eventType string) error {
	_, err := p.db.Exec(ctx, plugin.Rebind(upsertAwaiter.For(p.dialect), p.dialect),
		workflowID, tenantID, eventType)
	if err != nil {
		p.logger.Warn("event-triggers: register awaiter", "error", err, "workflow_id", workflowID)
		return fmt.Errorf("event-triggers: register awaiter: %w", err)
	}
	return nil
}
