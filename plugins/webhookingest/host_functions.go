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
	if err := scope.Register(plugin.FuncOptions{
		Name: "await_webhook",
		// NEITHER, same shape as eventtriggers.await_event: an await over
		// mutable state, consuming from a queue of deliveries. cleat#1318.
		Idempotent:        false,
		SameValueOnReplay: false,
	}, p.awaitWebhook); err != nil {
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
//
// A deleted source's events are cancelled, not delivered here -- owner
// decision on cleat#2199, applied in cleat-review on #2221. An event ingested
// BEFORE its source was deleted but not yet consumed by this call is marked
// status='cancelled' in the same transaction as the delete
// (handleDeleteSource, routes.go), and the WHERE clause below excludes any
// row in that state on top of that -- the same belt-and-suspenders shape
// processBatch's queryUnprocessedWebhookEvents already uses for the
// background retry path (background.go), so a future write path that forgets
// the delete-time cancellation still cannot hand a deleted source's event to
// a caller. Practically: once a source is deleted, no event of its ever
// reaches this function again, delivered or not, past or future -- an
// awaiting workflow simply keeps getting {"found": false} and is woken only
// by its own retry policy's eventual timeout, the same as if the source had
// gone quiet rather than been deleted. There is no signal here that the
// source was deleted rather than merely idle; a caller that needs to
// distinguish the two has to check GET /ingest/sources/{id} itself.
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
	//
	// LEFT JOIN webhook_sources s, not a bare FROM webhook_events: the
	// `s.deleted_at IS NULL` guard below needs it, and every selected and
	// filtered column is qualified with `e.` because joining introduces a
	// second `id` column (webhook_sources has one too) -- an unqualified
	// `id` in the SELECT list or WHERE clause would be ambiguous the moment
	// this join exists, not merely stylistically inconsistent.
	query := `
		SELECT e.id, e.event_type, e.payload, e.received_at
		FROM webhook_events e
		LEFT JOIN webhook_sources s ON e.source_id = s.id
		WHERE e.tenant_id = $1 AND e.processed = false
		  AND (e.status IS NULL OR e.status != 'cancelled')
		  AND s.deleted_at IS NULL
	`
	args := []any{cc.TenantID}
	argIdx := 2

	if sourceID != uuid.Nil {
		query += fmt.Sprintf(" AND e.source_id = $%d", argIdx)
		args = append(args, sourceID)
		argIdx++
	}
	if input.EventType != "" {
		query += fmt.Sprintf(" AND e.event_type = $%d", argIdx)
		args = append(args, input.EventType)
		argIdx++ //nolint:ineffassign // Deliberate: keeps the placeholder counter correct so the next clause added below cannot silently reuse this one's $N. Deleting it is a latent SQL bug, not a cleanup.
	}

	// plugin.LimitClause, not a literal "LIMIT 1": SQL Server has no LIMIT,
	// only OFFSET/FETCH after an ORDER BY (which this query already has).
	// cleat-review's re-check on #2198 found await_webhook erroring outright
	// on MSSQL -- a workflow could never see an ingested event there. Same
	// bug, same fix, as the two list-endpoint LIMITs in this PR.
	query += " ORDER BY e.received_at DESC " + plugin.LimitClause("1", p.dialect)

	var (
		eventID    uuid.UUID
		eventType  string
		payloadRaw []byte
		receivedAt time.Time
	)

	err := plugin.ScanRow(p.db.QueryRow(ctx, plugin.Rebind(query, p.dialect), args...),
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
