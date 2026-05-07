package eventtriggers

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const defaultRetryInterval = 30 * time.Second

// Run starts the background retry worker loop. It periodically queries
// ingested_events for unprocessed events that are at least 10 seconds old
// and retries their workflow dispatch. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("event-triggers: no database, background retry worker disabled")
		<-ctx.Done()
		return nil
	}

	interval := defaultRetryInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.logger.Info("event-triggers: background retry worker started",
		"interval", interval)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("event-triggers: background retry worker stopped")
			return nil

		case <-ticker.C:
			p.processBatch(ctx)
		}
	}
}

// processBatch queries unprocessed events and retries each one.
func (p *Plugin) processBatch(parentCtx context.Context) {
	start := time.Now()

	rows, err := p.db.QueryContext(parentCtx, `
		SELECT id, tenant_id, event_type, event_data, retry_count
		FROM ingested_events
		WHERE NOT processed
		  AND (status = 'pending' OR status IS NULL)
		  AND received_at < NOW() - INTERVAL '10 seconds'
		ORDER BY received_at
		LIMIT 100
	`)
	if err != nil {
		p.logger.Error("event-triggers: query unprocessed events", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			eventID    uuid.UUID
			tenantID   uuid.UUID
			eventType  string
			eventData  []byte
			retryCount int
		)
		if err := rows.Scan(&eventID, &tenantID, &eventType, &eventData, &retryCount); err != nil {
			p.logger.Error("event-triggers: scan event", "error", err)
			continue
		}

		// Use context.Background() for individual processing so that each
		// retry completes even if the parent context is cancelled.
		p.retryEvent(context.Background(), eventID, tenantID, eventType, eventData, retryCount)
	}

	if err := rows.Err(); err != nil {
		p.logger.Error("event-triggers: rows iteration error", "error", err)
	}

	elapsed := time.Since(start)
	p.logger.Info("event-triggers: retry cycle completed",
		"duration_ms", elapsed.Milliseconds())
}

// retryEvent processes a single unprocessed event: parses its data, matches
// subscriptions, dispatches workflows, and updates the event status.
func (p *Plugin) retryEvent(ctx context.Context, eventID uuid.UUID, tenantID uuid.UUID, eventType string, eventDataJSON []byte, retryCount int) {
	// Parse event data back into the map expected by the matching logic.
	var eventData map[string]interface{}
	if err := json.Unmarshal(eventDataJSON, &eventData); err != nil {
		p.logger.Error("event-triggers: unmarshal event data for retry",
			"event_id", eventID, "error", err)
		p.db.ExecContext(ctx, `
			UPDATE ingested_events
			SET processed = true, status = 'dead_letter', error_msg = $2
			WHERE id = $1
		`, eventID, "invalid event data: "+err.Error())
		return
	}

	// Look up matching subscriptions and dispatch workflows.
	matched, err := p.triggerMatchingWorkflows(ctx, eventID, tenantID, eventType, eventData)
	if err != nil {
		p.logger.Warn("event-triggers: retry failed to query subscriptions",
			"event_id", eventID, "error", err)
		p.markRetryFailed(ctx, eventID, retryCount, "subscription query failed: "+err.Error())
		return
	}

	if matched > 0 {
		// At least one workflow was started successfully.
		p.db.ExecContext(ctx, `
			UPDATE ingested_events
			SET processed = true, status = 'completed', error_msg = NULL
			WHERE id = $1
		`, eventID)
		p.eventsProcessed.Add(1)
		p.logger.Info("event-triggers: retry successful",
			"event_id", eventID,
			"workflows_started", matched,
		)
	} else {
		// No subscriptions matched or all dispatch attempts failed.
		// Check if any enabled subscriptions exist for this event type.
		var subCount int
		p.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM event_subscriptions
			WHERE tenant_id = $1 AND event_type = $2 AND enabled = true
		`, tenantID, eventType).Scan(&subCount)

		if subCount == 0 {
			// No subscriptions exist — no point retrying. Mark as completed.
			p.db.ExecContext(ctx, `
				UPDATE ingested_events
				SET processed = true, status = 'completed', error_msg = 'no matching subscriptions'
				WHERE id = $1
			`, eventID)
			p.logger.Info("event-triggers: event completed with no subscriptions",
				"event_id", eventID,
			)
		} else {
			// Subscriptions exist but dispatch failed (filters or StartWorkflow).
			p.markRetryFailed(ctx, eventID, retryCount, "no workflows started (filters or dispatch failures)")
		}
	}
}

// markRetryFailed updates the event with retry information. If retry_count
// has reached max_retries, the event is moved to dead_letter status.
func (p *Plugin) markRetryFailed(ctx context.Context, eventID uuid.UUID, currentRetryCount int, errMsg string) {
	newRetryCount := currentRetryCount + 1

	// Determine max_retries from the subscriptions for this event type.
	// Default to 3 if no subscriptions are found.
	maxRetries := 3
	row := p.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(max_retries), 3) FROM event_subscriptions
		WHERE tenant_id = (SELECT tenant_id FROM ingested_events WHERE id = $1)
		  AND event_type = (SELECT event_type FROM ingested_events WHERE id = $1)
	`, eventID)
	if scanErr := row.Scan(&maxRetries); scanErr != nil {
		maxRetries = 3
	}
	if maxRetries < 1 {
		maxRetries = 1
	}

	if newRetryCount >= maxRetries {
		p.db.ExecContext(ctx, `
			UPDATE ingested_events
			SET retry_count = $2, error_msg = $3, last_retry_at = NOW(), status = 'dead_letter', processed = true
			WHERE id = $1
		`, eventID, newRetryCount, errMsg)
		p.eventsDeadLetter.Add(1)
		p.logger.Warn("event-triggers: event moved to dead letter",
			"event_id", eventID,
			"retry_count", newRetryCount,
			"max_retries", maxRetries,
			"error", errMsg,
		)
	} else {
		p.db.ExecContext(ctx, `
			UPDATE ingested_events
			SET retry_count = $2, error_msg = $3, last_retry_at = NOW(), status = 'pending'
			WHERE id = $1
		`, eventID, newRetryCount, errMsg)
		p.logger.Warn("event-triggers: retry failed, will retry",
			"event_id", eventID,
			"retry_count", newRetryCount,
			"max_retries", maxRetries,
			"error", errMsg,
		)
	}
}
