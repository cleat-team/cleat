# Phase 4b & 5b: Detailed Design

## Phase 4b: Webhookingest Push-to-Signal

### Problem

Currently, `await_webhook` host function polls the `webhook_events` table for unprocessed events. Each poll is a SQL query. If no event exists, the workflow suspends and retries. This means:
- Up to `retry_interval` latency before a webhook is processed
- Wasted DB queries polling an empty table
- No way to wake a workflow immediately when a webhook arrives

### Design: Direct Signal Delivery via Source Workflow Binding

Rather than building a general subscription system (complex, over-engineered for this stage), add a simpler mechanism: **bind a webhook source to a specific workflow ID**. When a webhook arrives for that source, deliver a signal directly to the bound workflow.

This avoids:
- A new `webhook_subscriptions` table
- Changes to the `await_webhook` host function
- Complex lifecycle management for subscriptions

#### Step 1: Add `SignalWorkflow` to `plugin.Environment`

**File:** `internal/plugin/plugin.go`

```go
// SignalWorkflow delivers a signal to a running workflow instance.
// The signal name and JSON payload are recorded deterministically.
SignalWorkflow func(ctx context.Context, workflowID, signalName, payload string) error
```

This wraps `WorkflowStore.DeliverSignal` (already exists at `internal/host/db.go:650`):
```go
// DeliverSignal satisfies the SignalStore interface.
func (s *PostgresStore) DeliverSignal(ctx context.Context, workflowID, signalName, payload string) error {
    _, err := s.db.ExecContext(ctx, `
        INSERT INTO workflow_signals (workflow_id, signal_name, payload)
        VALUES ($1, $2, $3)
        ON CONFLICT (workflow_id, signal_name) DO UPDATE SET payload = $3, delivered_at = now()
    `, workflowID, signalName, payload)
    return err
}
```

Wiring in `cmd/durable-worker/main.go` (alongside `StartWorkflow` added in Phase 1):
```go
SignalWorkflow: func(ctx context.Context, workflowID, signalName, payload string) error {
    return store.DeliverSignal(ctx, workflowID, signalName, payload)
},
```

#### Step 2: Extend webhook source schema

**File:** `plugins/webhookingest/migrations.go`

Add migration version 3:
```sql
ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS signal_workflow_id TEXT;
ALTER TABLE webhook_sources ADD COLUMN IF NOT EXISTS signal_name TEXT NOT NULL DEFAULT 'webhook_received';
```

This lets users configure a source to signal a specific workflow when a webhook arrives:
- `signal_workflow_id`: the workflow instance ID to signal (NULL = no push, poll-only behavior)
- `signal_name`: the signal name to deliver (default: `"webhook_received"`)

#### Step 3: Update ingest handler

**File:** `plugins/webhookingest/routes.go`

In `handleIngestWebhook`, after the successful INSERT (line 192), add:

```go
// If this source is configured to signal a workflow, deliver the signal.
if source.SignalWorkflowID != "" {
    signalPayload := map[string]interface{}{
        "source_id":   sourceID.String(),
        "event_id":    eventID.String(),
        "event_type":  eventType,
        "payload":     json.RawMessage(payloadJSON),
        "received_at": now.Format(time.RFC3339),
    }
    payloadBytes, _ := json.Marshal(signalPayload)
    if err := p.env.SignalWorkflow(r.Context(), source.SignalWorkflowID, source.SignalName, string(payloadBytes)); err != nil {
        p.logger.Error("webhook-ingest: signal delivery failed",
            "workflow_id", source.SignalWorkflowID,
            "signal", source.SignalName,
            "error", err,
        )
        // Don't fail the ingest — the event is stored and can be polled.
    } else {
        p.logger.Info("webhook-ingest: signal delivered",
            "workflow_id", source.SignalWorkflowID,
            "event_id", eventID,
        )
    }
}
```

#### Step 4: Update source management

**File:** `plugins/webhookingest/routes.go`

In `handleCreateSource` and `handleGetSource`, include the new fields:
- Accept `signal_workflow_id` and `signal_name` in the create request body
- Return them in the GET response

#### Step 5: Store env reference

**File:** `plugins/webhookingest/plugin.go`

Add `env *plugin.Environment` to the Plugin struct. Store in `Init()`.

### How workflows use this

**Before (poll-based, still works):**
```go
// Workflow polls for webhook events
result, err := h.DurableCallTyped("webhook-ingest", "await_webhook",
    awaitWebhookInput{SourceID: sourceID, EventType: "push"}, &output)
```

**After (push-based, new option):**
1. User creates a webhook source with `signal_workflow_id` set to their running workflow ID
2. Workflow calls `h.AwaitSignals([]string{"webhook_received"}, timeout)`
3. When a webhook arrives, the signal is delivered immediately — no polling, no latency

### File changes summary

| File | Change |
|------|--------|
| `internal/plugin/plugin.go` | Add `SignalWorkflow` func field |
| `cmd/durable-worker/main.go` | Wire `SignalWorkflow` from `store.DeliverSignal` |
| `plugins/webhookingest/plugin.go` | Store `env` reference |
| `plugins/webhookingest/migrations.go` | Add `signal_workflow_id`, `signal_name` columns |
| `plugins/webhookingest/routes.go` | Ingest handler: signal delivery; create/get source: new fields |

---

## Phase 5b: Observability & Graceful Shutdown

### Problem

- Background workers run in goroutines with no visibility: are they running? stuck? failing?
- No metrics on how many jobs were processed, how many webhooks received, etc.
- On shutdown (SIGTERM), the worker cancels the context but doesn't wait for background goroutines to finish — cleanup may be interrupted mid-operation
- The `Stoppable` interface exists but is only used for individual plugin cleanup, not coordinated shutdown

### Design: Structured Logging + WaitGroup Shutdown

The simplest high-value approach: use the existing `*slog.Logger` for metrics-equivalent structured logging, and add `sync.WaitGroup` coordination for shutdown.

#### Step 1: Background worker metrics pattern

Each background worker's `Run(ctx)` follows the same pattern (ticker loop). Add instrumentation to the loop:

```go
func (p *Plugin) Run(ctx context.Context) error {
    ticker := time.NewTicker(p.interval)
    defer ticker.Stop()

    p.logger.Info("plugin: worker started", "plugin", p.Info().Name, "interval", p.interval)

    for {
        select {
        case <-ctx.Done():
            p.logger.Info("plugin: worker stopped", "plugin", p.Info().Name)
            return nil
        case <-ticker.C:
            start := time.Now()
            if err := p.doWork(ctx); err != nil {
                p.logger.Error("plugin: work cycle failed",
                    "plugin", p.Info().Name,
                    "error", err,
                )
                continue
            }
            p.logger.Info("plugin: work cycle completed",
                "plugin", p.Info().Name,
                "duration_ms", time.Since(start).Milliseconds(),
            )
        }
    }
}
```

The key structured log fields:
- `plugin`: the plugin name (for filtering: `grep "plugin=audit-log"`)
- `duration_ms`: cycle duration (for latency monitoring)
- `error`: any error during the cycle

These structured log lines can be ingested by any log aggregator (Loki, ELK, Datadog) and turned into dashboards.

#### Step 2: Add item counts to work cycle logs

For plugins that process batches (notifications, jobqueue, scheduler), add count fields:

```go
p.logger.Info("plugin: work cycle completed",
    "plugin", p.Info().Name,
    "duration_ms", time.Since(start).Milliseconds(),
    "deliveries_attempted", len(pending),
    "deliveries_succeeded", successCount,
    "deliveries_failed", failCount,
)
```

This gives:
- Throughput: `deliveries_attempted` per cycle
- Error rate: `deliveries_failed / deliveries_attempted`

#### Step 3: Graceful shutdown with WaitGroup

**File:** `cmd/durable-worker/main.go`

Currently (lines 216-228):
```go
for _, lp := range plugList {
    if p, ok := lp.Plugin.(plugin.HasBackground); ok {
        go func(bg plugin.HasBackground) {
            if berr := bg.Run(ctx); berr != nil {
                log.Printf("[worker %s] plugin %s: background worker exited: %v",
                    workerID, bg.Info().Name, berr)
            }
        }(p)
    }
}
```

Change to track all background goroutines and wait for them on shutdown:
```go
var bgWg sync.WaitGroup
for _, lp := range plugList {
    if p, ok := lp.Plugin.(plugin.HasBackground); ok {
        bgWg.Add(1)
        go func(bg plugin.HasBackground) {
            defer bgWg.Done()
            if berr := bg.Run(ctx); berr != nil {
                log.Printf("[worker %s] plugin %s: background worker exited: %v",
                    workerID, bg.Info().Name, berr)
            }
        }(p)
    }
}

// In the shutdown handler (SIGTERM/SIGINT signal handler):
cancel()  // cancels ctx, which stops all background workers
log.Printf("[worker %s] waiting for background workers to stop...", workerID)
done := make(chan struct{})
go func() {
    bgWg.Wait()
    close(done)
}()
select {
case <-done:
    log.Printf("[worker %s] all background workers stopped", workerID)
case <-time.After(30 * time.Second):
    log.Printf("[worker %s] timed out waiting for background workers", workerID)
}
```

Read the existing signal handler in `cmd/durable-worker/main.go` to find where SIGTERM is handled, and add the bgWg.Wait() there.

#### Step 4: Plugin-specific instrumentation spots

Each plugin with a background worker gets the standardized logging:

| Plugin | Background worker | Log fields to add |
|--------|------------------|-------------------|
| auditlog | Hourly retention cleanup | `deleted_events` count |
| blobstore | Hourly 3-phase cleanup | `stale_refs`, `expired_entries`, `orphaned_blobs` per phase |
| eventstore | Hourly retention cleanup | `deleted_events` count |
| jobqueue | 5-second poll + dispatch | `jobs_claimed`, `jobs_dispatched`, `jobs_failed` |
| notifications | 30-second delivery attempt | `deliveries_attempted`, `deliveries_succeeded`, `deliveries_failed` |
| ratelimiter | 30-second config reload | `configs_reloaded` count |
| scheduler | 60-second due schedule check | `schedules_due`, `workflows_started`, `workflows_failed` |
| webhookingest | N/A (no background worker) | N/A |

### File changes summary

| File | Change |
|------|--------|
| `cmd/durable-worker/main.go` | Add WaitGroup, graceful shutdown wait loop |
| `plugins/auditlog/background.go` | Add duration + count logging |
| `plugins/blobstore/background.go` | Add per-phase counts |
| `plugins/eventstore/background.go` | Add deleted count logging |
| `plugins/jobqueue/background.go` | Add dispatch counts |
| `plugins/notifications/background.go` | Add delivery attempt counts |
| `plugins/ratelimiter/background.go` | Add reload count |
| `plugins/scheduler/background.go` | Add due/started/failed counts |

---

## Implementation Effort

| Phase | Items | Estimate |
|-------|-------|----------|
| 4b | SignalWorkflow env + webhookingest push | ~1-2 hours |
| 5b | Observability logging + WaitGroup shutdown | ~1-2 hours |
| **Total** | | **~2-4 hours** |

## Dependencies

- Phase 4b depends on Phase 1 (`StartWorkflow` already done) — needs `SignalWorkflow` too
- Phase 5b depends on Phases 2a-2c (background workers must be functional to instrument)
