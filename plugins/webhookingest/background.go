package webhookingest

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/cleat-team/cleat/plugin"
)

const defaultRetryInterval = 30 * time.Second

// Run starts the background retry worker loop. It periodically queries
// webhook_events for unprocessed events that are at least 10 seconds old
// and retries their workflow signal delivery. Returns when ctx is cancelled.
func (p *Plugin) Run(ctx context.Context) error {
	// The SCAN is cross-tenant; the per-event work is not. cleat#1512.
	//
	// processBatch asks "which webhook events anywhere are unprocessed", which
	// has no tenant and cannot have one. Each row it finds belongs to exactly
	// one tenant, and retryEvent narrows back to that tenant rather than
	// inheriting this -- see the ForTenant call in processBatch.
	ctx = plugin.AcrossAllTenants(ctx,
		"webhook-ingest retry sweep: the scan for unprocessed events spans every tenant by definition")

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

	rows, err := p.db.Query(parentCtx, queryUnprocessedWebhookEvents.For(p.dialect))
	if err != nil {
		p.logger.Error("webhook-ingest: query unprocessed events", "error", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var (
			eventID          uuid.UUID
			tenantID         uuid.UUID
			sourceID         uuid.UUID
			eventType        string
			payload          []byte
			receivedAt       time.Time
			signalWorkflowID string
			signalName       string
			retryCount       int
		)
		if err := plugin.ScanRow(rows, &eventID, &tenantID, &sourceID, &eventType, &payload, &receivedAt,
			&signalWorkflowID, &signalName, &retryCount); err != nil {
			p.logger.Error("webhook-ingest: scan event", "error", err)
			continue
		}

		// Use context.Background() for individual processing so that each
		// retry completes even if the parent context is cancelled.
		//
		// ForTenant, not the sweep's bypass. cleat#1512. Every statement
		// retryEvent issues addresses webhook_events BY ID with no tenant
		// predicate, so the policy is the only thing standing between a bug or
		// an id collision and another tenant's row.
		//
		// tenant_id was not previously selected here, which is why this is not
		// simply "the tenant was dropped": it was never fetched. Adding the
		// column is what makes the narrow answer available at all, and it is a
		// column on the row the scan already reads rather than a second query.
		//
		// Built from context.Background() rather than from parentCtx for two
		// reasons: to detach from the tick's cancellation, as before, and
		// because parentCtx carries the sweep's bypass -- and a ForTenant
		// applied on top of AcrossAllTenants is ignored without a word
		// (cleat#1515), which would leave every retry write unscoped while
		// looking correct at the call site.
		p.retryEvent(plugin.ForTenant(context.Background(), tenantID),
			eventID, sourceID, eventType, payload, receivedAt, signalWorkflowID, signalName, retryCount)
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
		p.db.Exec(ctx, `
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
	p.db.Exec(ctx, `
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
		p.db.Exec(ctx, `
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
		p.db.Exec(ctx, `
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

// The batch of webhook events still awaiting delivery.
//
// This was one raw literal reaching all three backends, and it was valid on
// exactly one of them (cleat#1133). Three separate constructs, each with no
// portable spelling:
//
//   - `NOT e.processed` -- T-SQL has no boolean type, so a BIT column is a
//     value and not a condition: Msg 4145, "An expression of non-boolean type
//     specified in a context where a condition is expected".
//   - `NOW() - INTERVAL '10 seconds'` -- PostgreSQL's interval literal. MySQL
//     spells it `INTERVAL 10 SECOND`, unquoted and singular, and answers
//     `Error 1064 (42000)` to the quoted form. Verified against a live MySQL:
//     the quoted spelling exits 1, the unquoted one exits 0.
//   - `LIMIT 100` -- T-SQL spells row limits as TOP or OFFSET/FETCH.
//
// So this statement failed on MySQL and on SQL Server, on every tick of the
// background loop, and neither failure fails an assertion anywhere: the errors
// go to the worker log, which is not in the CI console.
var queryUnprocessedWebhookEvents = plugin.Query{
	Default: `SELECT e.id, e.tenant_id, e.source_id, e.event_type, e.payload, e.received_at,
       COALESCE(s.signal_workflow_id, ''), COALESCE(s.signal_name, 'webhook_received'),
       COALESCE(e.retry_count, 0)
FROM webhook_events e
LEFT JOIN webhook_sources s ON e.source_id = s.id
WHERE NOT e.processed
  AND (e.status = 'pending' OR e.status IS NULL)
  AND e.received_at < NOW() - INTERVAL '10 seconds'
ORDER BY e.received_at
LIMIT 100`,
	MySQL: `SELECT e.id, e.tenant_id, e.source_id, e.event_type, e.payload, e.received_at,
       COALESCE(s.signal_workflow_id, ''), COALESCE(s.signal_name, 'webhook_received'),
       COALESCE(e.retry_count, 0)
FROM webhook_events e
LEFT JOIN webhook_sources s ON e.source_id = s.id
WHERE NOT e.processed
  AND (e.status = 'pending' OR e.status IS NULL)
  AND e.received_at < NOW() - INTERVAL 10 SECOND
ORDER BY e.received_at
LIMIT 100`,
	MSSQL: `SELECT e.id, e.tenant_id, e.source_id, e.event_type, e.payload, e.received_at,
       COALESCE(s.signal_workflow_id, ''), COALESCE(s.signal_name, 'webhook_received'),
       COALESCE(e.retry_count, 0)
FROM webhook_events e
LEFT JOIN webhook_sources s ON e.source_id = s.id
WHERE e.processed = 0
  AND (e.status = 'pending' OR e.status IS NULL)
  AND e.received_at < DATEADD(second, -10, SYSUTCDATETIME())
ORDER BY e.received_at
OFFSET 0 ROWS FETCH NEXT 100 ROWS ONLY`,
}
