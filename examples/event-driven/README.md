# Event-Driven Workflows

This example demonstrates how to trigger workflows from domain events using the
event-triggers plugin.

## Overview

The event-triggers plugin lets you:

1.  **Subscribe** a workflow definition to a domain event type via the REST API
2.  **Publish** domain events that the worker stores durably and dispatches to
    matching subscriptions
3.  **Await** events from within a running workflow (via the `await_event` host
    function)
4.  **Retry** failed event dispatch automatically in the background
5.  **Replay** dead-lettered events on demand

Events are stored in the `ingested_events` table with idempotency (same `id`
can be safely re-published). The background retry worker periodically re-tries
events that weren't successfully dispatched.

## Prerequisites

- A running PostgreSQL database
- The `durable-worker` binary compiled with the event-triggers plugin

## Running the Worker

Start the worker with the HTTP API enabled:

```bash
durable-worker \
  --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
  --api-addr :8080
```

The event-triggers plugin loads automatically at startup.  Check that it is
healthy:

```bash
curl http://localhost:8080/api/plugins
```

Expected output includes:

```json
{
  "name": "event-triggers",
  "version": "0.1.0",
  "healthy": true
}
```

## Creating a Subscription

A subscription binds an event type to a workflow definition.  When an event of
that type is published, the worker looks up matching subscriptions and starts a
workflow for each one whose filter expression passes.

```bash
curl -X POST http://localhost:8080/api/events/subscriptions \
  -H "Content-Type: application/json" \
  -H "X-Tenant-ID: <tenant-uuid>" \
  -d '{
    "event_type": "user.signup",
    "def_name": "event-driven",
    "entry_point": "HandleSignup",
    "input_template": {
      "user_id": "{{.event.data.user_id}}",
      "email": "{{.event.data.email}}",
      "name": "{{.event.data.name}}"
    },
    "filter_expr": "true"
  }'
```

### Subscription Fields

| Field          | Required | Description                                    |
|----------------|----------|------------------------------------------------|
| `event_type`   | yes      | Domain event type to subscribe to              |
| `def_name`     | yes      | Workflow definition name                       |
| `entry_point`  | no       | Workflow entry-point function (default: `place_order`) |
| `input_template` | no    | JSON template merged with event data           |
| `filter_expr`  | no       | Expression evaluated against event data (`true` matches all) |
| `max_retries`  | no       | Max retry attempts before dead-letter (default: 3)         |

### Filter Expressions

Use `event.data.<path>` to reference event fields:

```bash
# Only trigger when price > 100
"filter_expr": "event.data.price > 100"

# Only for specific status values
"filter_expr": "event.data.status in('active', 'pending')"

# Nested field access
"filter_expr": "event.data.user.name == 'alice'"

# Structured JSON filter (supports $gt, $gte, $lt, $lte, $ne, $in, $nin, $exists)
"filter_expr": "{\"event.data.amount\": {\"$gt\": 100}}"
```

## Publishing an Event

Events are published via the REST API with an idempotent `id` field:

```bash
curl -X POST http://localhost:8080/api/events/publish \
  -H "Content-Type: application/json" \
  -H "X-Tenant-ID: <tenant-uuid>" \
  -d '{
    "id": "evt_001",
    "event_type": "user.signup",
    "data": {
      "user_id": "usr_abc123",
      "email":   "alice@example.com",
      "name":    "Alice Smith"
    }
  }'
```

The response shows how many workflows were started:

```json
{
  "status": "published",
  "matched": 1
}
```

Re-publishing the same `id` returns `"duplicate"` (idempotent).

## Checking Event Status

```bash
# The event-triggers plugin does not expose a dedicated event-status endpoint
# directly.  Query ingested_events in the database:
psql -c "SELECT id, event_type, processed, status, error_msg FROM ingested_events WHERE id = 'evt_001';"
```

## Awaiting Events from Within a Workflow

A running workflow can wait for a domain event using the `await_event` host
function registered by the event-triggers plugin.  It creates a signal named
`__evt:<eventType>` and waits for it.

In the workflow code:

```go
// Wait for a "user.activated" event, timeout after 7 days.
result := h.AwaitSignals([]string{"__evt:user.activated"}, 7*24*time.Hour)
if !result.TimedOut {
    var payload struct { ActivatedAt string `json:"activated_at"` }
    json.Unmarshal([]byte(result.Payload), &payload)
    // handle activation
}
```

When an event of type `user.activated` is published, the publish handler
broadcasts a signal to any workflows registered as awaiters for that event type.

## Retrying Dead-Lettered Events

If an event exhausts its retry attempts it is moved to `dead_letter` status.
You can manually replay it:

```bash
curl -X POST http://localhost:8080/api/events/evt_001/retry \
  -H "X-Tenant-ID: <tenant-uuid>"
```

Response:

```json
{
  "status": "retried",
  "event_id": "evt_001",
  "workflows_started": 1
}
```

The event's processing state is reset (`processed=false`, `retry_count=0`) and
dispatch is re-attempted immediately.  Awaiting workflows are also signalled so
they wake up promptly.

## Building the Workflow

Build the example workflow to WASM:

```bash
durable build -o /tmp/out ./examples/event-driven/

# Deploy the WASM module:
durable deploy event-driven /tmp/out/event-driven.wasm
```

## Architecture

```
  Publish Event                    ┌──────────────────┐
  ─────────────► POST /api/events/ │ handlePublishEvent│
                  publish          └────────┬─────────┘
                                            │
              ┌─────────────────────────────┼─────────────────────┐
              ▼                             ▼                     │
   ┌──────────────────┐         ┌────────────────────┐           │
   │ triggerMatching-  │         │ signalAwaiters     │           │
   │ Workflows         │         │ (broadcast signal  │           │
   │ (start workflows) │         │  to waiting wfs)   │           │
   └────────┬─────────┘         └──────────┬─────────┘           │
            │                              │                     │
            ▼                              ▼                     │
   ┌────────────────────┐        ┌──────────────────┐           │
   │ Subscription-based │        │ AwaitEvent-based │           │
   │ workflows started  │        │ workflows resume │           │
   └────────────────────┘        └──────────────────┘           │
                                                                 │
   ┌─────────────────────────────────────────────────────────────┘
   │
   ▼
┌──────────────────┐
│ Background retry │ ←── retries unprocessed events every 30s
│ worker           │
└──────────────────┘
```
