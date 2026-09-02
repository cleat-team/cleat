# Common workflow patterns

This guide covers the most common workflow patterns in cleat with real code
examples drawn from the project's example directory.

## Saga (compensating transactions)

A Saga orchestrates a series of steps with automatic compensation on failure.
If any step fails, all previously completed steps are compensated in reverse
order (LIFO).

### Structured Saga with NewSaga

```go
func CreateOrder(h cleat.HostCalls, input string) error {
    var chargeID, driverID string

    s := cleat.NewSaga()
    s.AddStep("charge",
        func(h cleat.HostCalls) (string, error) {
            resp, err := h.DurableCall("payments", "Charge", input)
            if err == nil {
                chargeID = extractID(resp)
            }
            return resp, err
        },
        func(h cleat.HostCalls) error {
            _, err := h.DurableCall("payments", "Refund", chargeID)
            return err
        },
    )
    s.AddStep("assign_driver",
        func(h cleat.HostCalls) (string, error) {
            resp, err := h.DurableCall("dispatch", "AssignDriver", input)
            if err == nil {
                driverID = extractID(resp)
            }
            return resp, err
        },
        func(h cleat.HostCalls) error {
            _, err := h.DurableCall("dispatch", "ReleaseDriver", driverID)
            return err
        },
    )

    if err := s.Run(h); err != nil {
        return err
    }
    return nil
}
```

### DurableDefer (simple compensation)

For simpler compensation patterns, use `DurableDeferFunc`:

```go
func CreateOrder(h cleat.HostCalls, input string) error {
    h.DurableDeferFunc(func() {
        h.DurableCall("inventory", "ReleaseReservation", "order-123")
        h.DurableCall("payments", "Refund", "order-123")
    })
    h.DurableCall("inventory", "Reserve", input)
    h.DurableCall("payments", "Charge", input)
    return nil
}
```

Call `DurableDeferFunc` directly — do not put Go's own `defer` in front of it.
That would delay the *registration* until the function returns, which reverses
the order of two or more registrations.

The block runs when the entry point finishes, on the success path as well as
the error path, exactly like Go's `defer`. If you want cleanup only on failure,
test for it inside the block. It does not run when the workflow suspends: a
sleeping workflow has not exited, and the segment that finally completes runs
it then.

On replay the block runs again, but the durable calls inside it are served from
the recorded history rather than re-executed, so their effects are not
repeated. Anything in there that is *not* a durable call can run more than
once.

### Key differences: Saga vs DurableDefer

| Feature | Saga | DurableDefer |
|---------|------|--------------|
| Compensation order | LIFO (last step compensated first) | As defined |
| Typed results | `NewSagaTyped[T]` collects typed outputs | Not supported |
| Parallel steps | `AddParallel` for concurrent execution | Not supported |
| Terminal errors | `TerminalError` stops compensation chain | Not supported |
| Best for | Multi-step workflows with complex compensation | Simple single-level cleanup |

See the [fooddash example](../examples/fooddash/order.go) for a full Saga
implementation with typed calls, signals, and polling.

## Fan-out (parallel execution)

Use `ChildWorkflow` + `AwaitAllChildren` to fan out work across multiple child
workflows and collect results.

```go
func RunPipeline(h cleat.HostCalls, input PipelineInput) (*PipelineResult, error) {
    // Fan-out: start a child workflow for each item.
    var runIDs []string
    for _, item := range input.Items {
        runID, err := h.ChildWorkflow("process_item", item)
        if err != nil {
            return nil, fmt.Errorf("failed to start child: %w", err)
        }
        runIDs = append(runIDs, runID)
    }

    // Fan-in: await all children concurrently.
    results, err := h.AwaitAllChildren(runIDs)
    if err != nil {
        return nil, fmt.Errorf("await children failed: %w", err)
    }

    // Process results.
    var succeeded, failed int
    for _, r := range results {
        if r.Error == "" {
            succeeded++
        } else {
            failed++
        }
    }
    return &PipelineResult{Succeeded: succeeded, Failed: failed}, nil
}
```

### Fan-out with Saga.AddParallel

For parallel execution of non-child steps with automatic compensation:

```go
s := cleat.NewSaga()
s.AddParallel(
    cleat.SagaStep{
        Forward: func(h cleat.HostCalls) (string, error) {
            return h.DurableCall("flights", "Book", flightJSON)
        },
        Compensate: func(h cleat.HostCalls) error {
            _, err := h.DurableCall("flights", "Cancel", flightRef)
            return err
        },
    },
    cleat.SagaStep{
        Forward: func(h cleat.HostCalls) (string, error) {
            return h.DurableCall("hotels", "Book", hotelJSON)
        },
        Compensate: func(h cleat.HostCalls) error {
            _, err := h.DurableCall("hotels", "Cancel", hotelRef)
            return err
        },
    },
)
if err := s.Run(h); err != nil {
    return nil, fmt.Errorf("booking failed (all compensated): %w", err)
}
```

See the [travel booking example](../examples/travel/booking.go) for a complete
parallel booking workflow with cancellation handling.

## Cron / scheduled workflows

Use the `cleat schedule` CLI to create recurring workflows:

```bash
# Run every hour
cleat schedule add hourly-report \
    --cron "0 * * * *" \
    --def generate_report

# Run daily at midnight
cleat schedule add daily-cleanup \
    --cron "0 0 * * *" \
    --def cleanup_task \
    --input '{"retention_days": 30}'

# List schedules
cleat schedule list

# Remove a schedule
cleat schedule remove hourly-report
```

Schedules can also be managed via the REST API or web UI at
`GET /api/schedules` and `POST /api/schedules`.

### Event-triggered workflows

Workflows can be triggered by domain events via the event triggers plugin:

```bash
curl -X POST http://localhost:8080/api/events/subscriptions \
    -H "Content-Type: application/json" \
    -d '{
        "event_type": "user.signup",
        "def_name": "signup-workflow",
        "entry_point": "HandleSignup",
        "input_template": {
            "user_id": "{{.event.data.user_id}}",
            "email": "{{.event.data.email}}"
        }
    }'
```

See the [event-driven example](../examples/event-driven/subscription_workflow.go)
for a complete signup workflow.

## Signals (external events)

`AwaitSignals` pauses the workflow until an external signal arrives or a
timeout expires:

```go
func WaitForPayment(h cleat.HostCalls, input string) error {
    // Create invoice.
    h.DurableCall("payments", "CreateInvoice", input)

    // Wait for payment signal -- workflow suspends here.
    result := h.AwaitSignals([]string{"payment_received"}, 30*time.Minute)

    if result.TimedOut {
        return fmt.Errorf("payment not received within 30 minutes")
    }

    // Signal received -- proceed with fulfillment.
    h.DurableCall("fulfillment", "ShipOrder", result.Payload)
    return nil
}
```

Send a signal from outside the workflow:

```bash
curl -X POST http://localhost:8080/api/workflows/<id>/signal \
    -H "Content-Type: application/json" \
    -d '{
        "signal_name": "payment_received",
        "payload": {"transaction_id": "txn_456"}
    }'
```

### Awaiting multiple signals

```go
// Wait for one of several signals.
result := h.AwaitSignals([]string{"approved", "rejected", "escalated"}, 24*time.Hour)

switch result.Name {
case "approved":
    // Proceed with fulfillment.
case "rejected":
    // Handle rejection.
case "escalated":
    // Route to supervisor.
}
```

### SignalResult fields

| Field | Type | Description |
|-------|------|-------------|
| Name | string | Name of the received signal (empty if timed out) |
| Payload | string | JSON payload from the signal |
| TimedOut | bool | True if no signal arrived before the timeout |
| Err | error | Error if signal delivery failed |

See the [onboarding example](../examples/onboarding/signup.go) for a complete
signal-based email verification workflow.

## Child workflows

Child workflows let you compose workflows from smaller, reusable units:

```go
// Start a child workflow -- does not block.
runID, err := h.ChildWorkflow("process_item", inputJSON)
if err != nil {
    return "", fmt.Errorf("failed to start child: %w", err)
}

// Do other work while the child runs...
h.DurableCall("logging", "Record", "child started")

// Later, await the child result.
result, err := h.AwaitChild(runID)
if err != nil {
    return "", fmt.Errorf("child failed: %w", err)
}
```

### Typed child workflows

Use `ChildWorkflowTyped` for type-safe child invocations:

```go
runID, err := h.ChildWorkflowTyped("process_item",
    ChildInput{Item: item, JobID: jobID, Index: i})
```

### AwaitAllChildren (fan-in)

Combine with fan-out for parallel child execution:

```go
runIDs := make([]string, len(items))
for i, item := range items {
    runIDs[i], _ = h.ChildWorkflowTyped("process_item", item)
}

// Wait for ALL children to complete.
results, err := h.AwaitAllChildren(runIDs)
```

Each `ChildResult` has `RunID`, `Result` (JSON), and `Error` fields.

See the [data pipeline example](../examples/datapipeline/pipeline.go) for a
complete fan-out/fan-in pipeline with heartbeating and typed child workflows.

## Activity heartbeating

For long-running operations, `DurableCallWithHeartbeat` periodically reports
progress. If the heartbeat stops, the worker can detect the stall:

```go
func ProcessItem(h cleat.HostCalls, input ChildInput) (*ChildResult, error) {
    var fetchData struct {
        Raw  string `json:"raw"`
        Size int    `json:"size"`
    }

    err := h.DurableCallTypedWithHeartbeat(
        "data", "Fetch",
        map[string]string{"item": input.Item},
        &fetchData,
        5*time.Second, // heartbeat interval
        func(progressJSON string) {
            // Progress callback -- update queryable state.
            var p struct{ Percent int }
            json.Unmarshal([]byte(progressJSON), &p)
            h.SetQueryState("fetch_progress", fmt.Sprintf("%d%%", p.Percent))
        },
    )
    if err != nil {
        return nil, fmt.Errorf("fetch failed: %w", err)
    }
    // ...
}
```

The heartbeat interval (5 seconds in this example) controls how often the host
updates progress. On the worker side, stale heartbeats are detected by the
reaper goroutine, which reclaims instances with expired heartbeats.

## ContinueAsNew

For long-running workflows that would accumulate large event histories, use
`ContinueAsNew` to restart with fresh history:

```go
func ManageSubscription(h cleat.HostCalls, input SubscriptionInput) (string, error) {
    // Process monthly billing.
    if err := chargeWithRetry(h, input); err != nil {
        return enterGracePeriod(h, input)
    }

    // Wait one month.
    h.DurableSleep(30 * 24 * time.Hour)

    // ContinueAsNew restarts with fresh event history,
    // carrying forward the subscription input.
    if err := h.ContinueAsNew(toJSON(input)); err != nil {
        return "", fmt.Errorf("continue_as_new failed: %w", err)
    }
    return "", nil // unreachable
}
```

### When to use ContinueAsNew

- **Monthly billing** -- one billing cycle per execution, restarts each month
- **Long-running data pipelines** -- process batches in chunks, continue as new
  with the remaining items
- **Event history limits** -- event history grows unbounded without it
- **Worker memory** -- replaying a very long history consumes memory and time

See the [subscription billing example](../examples/subscription/billing.go) for
a complete implementation with grace periods, retries, and cancellation.

```go
// PlaceLargeOrder uses ContinueAsNew to process items in batches of 10.
func PlaceLargeOrder(h cleat.HostCalls, userID string, items []OrderItem) (string, error) {
    if len(items) <= 10 {
        return processOrderSmall(h, userID, items)
    }

    // Process first batch.
    _, err := processOrderSmall(h, userID, items[:10])
    if err != nil {
        return "", err
    }

    // Continue as new with remaining items.
    remaining := items[10:]
    input, _ := json.Marshal(map[string]interface{}{
        "user_id": userID,
        "items":   remaining,
    })
    return "", h.ContinueAsNew(string(input))
}
```

## Polling pattern

`cleat.PollUntil` repeatedly checks a condition at a given interval until a
deadline is exceeded:

```go
status, err := cleat.PollUntil(h, 30*time.Second /*poll interval*/, 30*time.Minute /*deadline*/,
    func() (string, error) {
        // Check external status.
        return checkPickupStatus(driverID)
    },
    func(s string) bool {
        // Success condition.
        return s == "picked_up"
    },
)
if err != nil {
    return "", fmt.Errorf("pickup polling failed: %w", err)
}
```

### Manual polling with DurableSleep

For more control over the polling loop:

```go
func PollUntilCustom(h cleat.HostCalls, orderID string) (string, error) {
    deadline := h.Now().Add(30 * time.Minute)

    for h.Now().Before(deadline) {
        result, err := h.DurableCall("dispatch", "CheckStatus", orderID)
        if err != nil {
            return "", err
        }

        if isComplete(result) {
            return result, nil
        }

        // Check for cancellation between poll cycles.
        if cancelled, reason := h.PollCancellation(); cancelled {
            return "", fmt.Errorf("cancelled: %s", reason)
        }

        h.DurableSleep(30 * time.Second)
    }

    return "", fmt.Errorf("timed out after 30 minutes")
}
```

## Retry policies

Configure retry behavior with `RetryPolicy`:

```go
policy := cleat.RetryPolicy{
    MaxAttempts:        5,
    InitialInterval:    1 * time.Second,
    BackoffCoefficient: 2.0,
    MaxInterval:        30 * time.Second,
}

result, err := h.DurableCallWithOptions(
    cleat.CallOptions{Retry: &policy},
    "payments", "Charge", requestJSON,
)
```

### RetryPolicy fields

| Field | Type | Description |
|-------|------|-------------|
| `MaxAttempts` | int | Maximum retry attempts (default 1 = no retry) |
| `InitialInterval` | time.Duration | Initial backoff interval |
| `BackoffCoefficient` | float64 | Backoff multiplier (2.0 = double each attempt) |
| `MaxInterval` | time.Duration | Maximum backoff interval cap |
| `NonRetryableErrors` | []string | Error substrings that skip retry |

### Non-retryable errors

```go
policy := cleat.RetryPolicy{
    MaxAttempts:        3,
    InitialInterval:    1 * time.Second,
    BackoffCoefficient: 2.0,
    NonRetryableErrors: []string{"INVALID_REQUEST", "NOT_FOUND"},
}
```

### Default retry policy

```go
policy := cleat.DefaultRetryPolicy()
// MaxAttempts=10, InitialInterval=1s, BackoffCoefficient=2.0, MaxInterval=60s
```

### Workaround for per-call timeouts

Host-side timeout enforcement is not yet implemented. Use polling for
timeout-aware patterns:

```go
deadline := h.Now().Add(30 * time.Second)
for {
    result, err := h.DurableCall("service", "Op", requestJSON)
    if err == nil {
        return result, nil
    }
    if h.Now().After(deadline) {
        return "", fmt.Errorf("timed out after 30s")
    }
    h.DurableSleep(1 * time.Second)
}
```

## Auto-threading

Helper functions that use HostCalls do not need to declare `h` in their
signature. The transformer automatically threads it through:

```go
// Package-level declaration -- the transformer detects this.
var h cleat.HostCalls

func PlaceOrder(h cleat.HostCalls, input string) error {
    return processOrder(input) // h is auto-threaded
}

// The transformer adds h cleat.HostCalls as the first parameter
// and threads it through to called cleat leaves.
func processOrder(input string) error {
    h.DurableCall("inventory", "Check", input) // h is available here
    return nil
}
```

This works because the transformer traces the call graph from each entry point
and adds `h` wherever it is used. See `testdata/autothread/order.go` for a
complete example.

## Cancellation detection

Check for cancellation in long-running workflows:

```go
func LongRunningProcess(h cleat.HostCalls, input string) error {
    for i := 0; i < 100; i++ {
        h.DurableCall("processor", "Step", stepJSON(i))

        // Check if cancellation was requested.
        if cancelled, reason := h.PollCancellation(); cancelled {
            // Perform cleanup.
            h.DurableCall("cleanup", "Rollback", input)
            return fmt.Errorf("cancelled: %s", reason)
        }
    }
    return nil
}
```

Cancellation is requested via the REST API:

```bash
curl -X POST http://localhost:8080/api/workflows/<id>/cancel
```

## Workflow versioning

Cleat versions workflows by storing WASM blobs in the database. Each
`cleat deploy` creates a new version. Use `version()` and `min_version()` for
backward-compatible logic:

```go
func PlaceOrder(h cleat.HostCalls, input string) error {
    // Declare minimum version this code supports.
    _ = h.MinVersion()

    if h.Version() >= 2 {
        // v2+ behavior: new validation logic.
        return placeOrderV2(input)
    }
    // v1 behavior: legacy validation.
    return placeOrderV1(input)
}
```

Rollback is a database operation:

```bash
cleat rollback place_order 1
```

This is fundamentally different from Temporal's model where old workers must
remain running. With cleat, the WASM blob IS the version.

## Query state

Set queryable state during workflow execution and read it externally:

```go
// In the workflow:
h.SetQueryState("order_status", "confirmed")
h.SetQueryState("driver_name", driver.DriverName)
h.SetQueryState("eta_minutes", driver.ETAMinutes)
```

```bash
# Read from outside the workflow:
curl "http://localhost:8080/api/workflows/<id>/query?key=order_status"
curl "http://localhost:8080/api/workflows/<id>/query?key=driver_name"
```

Query state persists across replays and ContinueAsNew.
