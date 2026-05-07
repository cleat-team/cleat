package webhookingest

import (
	"context"
	"time"

	"github.com/google/uuid"
)

const defaultRetryInterval = 30 * time.Second

// Run starts the background retry worker loop. It periodically queries
// webhook_events for unprocessed events that are at least 10 seconds old
// and retries their workflow signal delivery. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	if p.db == nil {
		p.logger.Warn("webhook-ingest: no database, background retry worker disabled")
		<-ctx.Done()
		return nil
	}

	interval := defaultRetryInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	p.logger.Info("webhook-ingest: background retry worker started",
		"interval", interval)

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("webhook-ingest: background retry worker stopped")
			return nil

		case <-ticker.C:
			p.processBatch(ctx)
		}
	}
}

// processBatch queries unprocessed webhook events and retries each one.
func (p *Plugin) processBatch(parentCtx context.Context) {
	start := time.Now()

	rows, err := p.db.QueryContext(parentCtx, `
		SELECT e.id, e.source_id, e.event_type, e.payload, e.received_at,
		       COALESCE(s.signal_workflow_id, ''), COALESCE(s.signal_name, 'webhook_received'),
		       COALESCE(e.retry_count, 0)
		FROM webhook_events e
		LEFT JOIN webhook_sources s ON e.source_id = s.id
		WHERE NOT e.processed
		  AND (e.status = 'pending' OR e.status IS NULL)
		  AND e.received_at < NOW() - INTERVAL '10 seconds'
		ORDER BY e.received_at
		LIMIT 100
	`)
	if err != nil {
		p.logger.Error("webhook-ingest: query unprocessed events", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			eventID          uuid.UUID
			sourceID         uuid.UUID
			eventType        string
			payload          []byte
			receivedAt       time.Time
			signalWorkflowID string
			signalName       string
			retryCount       int
		)
		if err := rows.Scan(&eventID, &sourceID, &eventType, &payload, &receivedAt,
			&signalWorkflowID, &signalName, &retryCount); err != nil {
			p.logger.Error("webhook-ingest: scan event", "error", err)
			continue
		}

		// Use context.Background() for individual processing so that each
		// retry completes even if the parent context is cancelled.
		p.retryEvent(context.Background(), eventID, sourceID, eventType, payload, receivedAt, signalWorkflowID, signalName, retryCount)
	}

	if err := rows.Err(); err != nil {
		p.logger.Error("webhook-ingest: rows iteration error", "error", err)
	}

	elapsed := time.Since(start)
	p.logger.Info("webhook-ingest: retry cycle completed",
		"duration_ms", elapsed.Milliseconds())
}

// retryEvent processes a single unprocessed webhook event by delivering a
// signal to the bound workflow. On failure the event is updated with retry
// information; after max_retries (3) it is moved to dead_letter status.
func (p *Plugin) retryEvent(ctx context.Context, eventID uuid.UUID, sourceID uuid.UUID, eventType string, payload []byte, receivedAt time.Time, signalWorkflowID, signalName string, retryCount int) {
	if signalWorkflowID == "" {
		// No workflow bound — mark as completed (nothing to retry).
		p.db.ExecContext(ctx, `
			UPDATE webhook_events
			SET processed = true, status = 'completed', error_msg = 'no signal_workflow_id configured'
			WHERE id = $1
		`, eventID)
		p.logger.Info("webhook-ingest: event completed with no signal workflow bound",
			"event_id", eventID)
		return
	}

	if p.env != nil && p.env.SignalWorkflow != nil {
		if err := p.env.SignalWorkflow(ctx, signalWorkflowID, signalName, string(payload)); err != nil {
			p.logger.Warn("webhook-ingest: signal delivery failed",
				"event_id", eventID,
				"workflow_id", signalWorkflowID,
				"error", err,
			)
			p.markRetryFailed(ctx, eventID, retryCount, "signal delivery failed: "+err.Error())
			return
		}
	}

	// Success — mark the event as processed.
	p.db.ExecContext(ctx, `
		UPDATE webhook_events
		SET processed = true, status = 'completed', error_msg = NULL
		WHERE id = $1
	`, eventID)
	p.logger.Info("webhook-ingest: signal delivered via retry",
		"event_id", eventID,
		"workflow_id", signalWorkflowID,
	)
}

// markRetryFailed updates the event with retry information. After 3 retries
// the event is moved to dead_letter status.
func (p *Plugin) markRetryFailed(ctx context.Context, eventID uuid.UUID, currentRetryCount int, errMsg string) {
	newRetryCount := currentRetryCount + 1

	maxRetries := 3
	if newRetryCount >= maxRetries {
		p.db.ExecContext(ctx, `
			UPDATE webhook_events
			SET retry_count = $2, error_msg = $3, last_retry_at = NOW(), status = 'dead_letter', processed = true
			WHERE id = $1
		`, eventID, newRetryCount, errMsg)
		p.logger.Warn("webhook-ingest: event moved to dead letter",
			"event_id", eventID,
			"retry_count", newRetryCount,
			"max_retries", maxRetries,
			"error", errMsg,
		)
	} else {
		p.db.ExecContext(ctx, `
			UPDATE webhook_events
			SET retry_count = $2, error_msg = $3, last_retry_at = NOW(), status = 'pending'
			WHERE id = $1
		`, eventID, newRetryCount, errMsg)
		p.logger.Warn("webhook-ingest: retry failed, will retry",
			"event_id", eventID,
			"retry_count", newRetryCount,
			"max_retries", maxRetries,
			"error", errMsg,
		)
	}
}
