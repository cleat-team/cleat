# Temporal to Cleat Migration Guide

Migrate your Temporal workflows to the Cleat durable execution framework. This guide
covers conceptual mapping, API differences, code examples, and known gaps.

---

## Conceptual Mapping

| Temporal Concept | Cleat Equivalent | Notes |
|---|---|---|
| Workflow | `@durable_entry` function | No workflow/activity distinction in Cleat |
| Activity | `durable_call()` | Both service calls and activities use the same API |
| Signal | `await_signals()` / `poll_signal()` | Same semantics, different method name |
| Query | `set_query_state()` / query handlers | Cleat uses explicit key-value state |
| Child Workflow | `child_workflow()` + `await_child()` | Similar API, no `ChildWorkflowOptions` |
| Continue-As-New | `continue_as_new()` | Identical concept |
| Timer / Sleep | `durable_sleep(ms)` | **Milliseconds** in Cleat vs. `time.Duration` in Temporal Go |
| Saga / Compensation | `Saga` class + `durable_defer()` | Cleat has a built-in Saga helper |
| Workflow ID | `current_workflow_id()` | Available as a host call |
| Run ID | `current_run_id()` | Available as a host call |
| Memo / Search Attributes | Set via `durable_call("state", "set", ...)` | Use Cleat's state API |
| Side Effect | `random()` | Deterministic random is built-in |
| `Workflow.sleep` / `Thread.sleep` | `durable_sleep(ms)` | Single API for all suspension |
| `Workflow.newRandom` | `random()` | Built-in deterministic random |
| `Workflow.getVersion` / `getCurrentBuildId` | `version()` / `min_version()` | Simpler versioning via WASM blob pointer |
| Dynamic workflow (stub) | `child_workflow(name, input)` | Name-based, no stub typing needed |
| Interceptor / middleware | N/A | Cleat's WASM boundary eliminates need for interceptors |

---

## API Differences

### Workflow Definition

**Temporal (Go):**
```go
// workflow.go
package workflow

import (
    "go.temporal.io/sdk/workflow"
)

func MyWorkflow(ctx workflow.Context, input MyInput) (MyOutput, error) {
    // ...
}
```

**Cleat (Go):**
```go
// workflow.go
package workflow

import "github.com/rcownie/cleat/durable"

var h durable.HostCalls // auto-threaded by transformer

//go:durable_entry
func MyWorkflow(ctx *durable.HostCalls, input MyInput) (MyOutput, error) {
    // ...
}
```

**Cleat (Python):**
```python
from cleat_sdk import HostCalls, durable_entry

@durable_entry
def my_workflow(h: HostCalls, input: MyInput) -> str:
    pass
```

### Activity vs. Durable Call

**Temporal (Go) — activity:**
```go
// Define an activity
func ChargeCard(ctx context.Context, request PaymentRequest) (PaymentResponse, error) {
    // ...
}

// Call from workflow
var resp PaymentResponse
err := workflow.ExecuteActivity(ctx, ChargeCard, request).Get(ctx, &resp)
```

**Cleat (Go) — durable call:**
```go
// No activity definition needed — call directly
resp, err := h.DurableCall("payment", "charge", requestJSON)
```

**Cleat (Python):**
```python
resp = h.durable_call("payment", "charge", {"amount": 100})
```

### Signal Handling

**Temporal (Go):**
```go
var signalVal string
signalChan := workflow.GetSignalChannel(ctx, "my-signal")
signalChan.Receive(ctx, &signalVal)
```

**Cleat (Go):**
```go
result := h.AwaitSignals([]string{"my-signal"}, 30*time.Second)
```

**Cleat (Python):**
```python
result = h.await_signals(["my-signal"], 30000)
```

### Timer / Sleep

**Temporal (Go):**
```go
workflow.Sleep(ctx, 5*time.Second)
```

**Cleat (Go):**
```go
h.DurableSleep(5 * time.Second)
```

**Cleat (Python) — note milliseconds:**
```python
h.durable_sleep(5000)  # 5 seconds
```

### Child Workflow

**Temporal (Go):**
```go
ctx = workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
    WorkflowID: "child-123",
})
future := workflow.ExecuteChildWorkflow(ctx, MyChild, input)
var result Output
future.Get(ctx, &result)
```

**Cleat (Go):**
```go
runID, err := h.ChildWorkflow("my_child", inputJSON)
// ... do other work ...
result, err := h.AwaitChild(runID)
```

**Cleat (Python):**
```python
run_id = h.child_workflow("my_child", input_data)
result = h.await_child(run_id)
```

---

## Before/After Examples

### Payment Workflow

**Temporal:**
```go
func PaymentWorkflow(ctx workflow.Context, order Order) error {
    logger := workflow.GetLogger(ctx)

    // Activity: Reserve inventory
    var reserveResult ReserveResult
    err := workflow.ExecuteActivity(ctx, ReserveInventory, order.Items).Get(ctx, &reserveResult)
    if err != nil {
        return err
    }

    // Activity: Charge payment
    var chargeResult ChargeResult
    err = workflow.ExecuteActivity(ctx, ChargeCard, order.Total).Get(ctx, &chargeResult)
    if err != nil {
        // Compensate: release inventory
        workflow.ExecuteActivity(ctx, ReleaseInventory, reserveResult.ID)
        return err
    }

    // Wait for external signal (approval)
    signalChan := workflow.GetSignalChannel(ctx, "order_approved")
    var approved bool
    signalChan.Receive(ctx, &approved)

    // Activity: Create shipment
    var shipResult ShipResult
    err = workflow.ExecuteActivity(ctx, CreateShipment, order.ID).Get(ctx, &shipResult)
    if err != nil {
        return err
    }

    logger.Info("Order completed", "order_id", order.ID)
    return nil
}
```

**Cleat (Go):**
```go
//go:durable_entry
func PaymentWorkflow(h durable.HostCalls, order Order) error {
    h.DurableLog("Payment workflow started")

    // Durable call: Reserve inventory
    reserveResp, err := h.DurableCall("inventory", "Reserve", marshal(order.Items))
    if err != nil {
        return err
    }

    // Durable call: Charge payment
    chargeResp, err := h.DurableCall("payment", "Charge", marshal(order.Total))
    if err != nil {
        h.DurableCall("inventory", "Release", reserveResp) // compensate
        return err
    }

    // Wait for external signal
    sig := h.AwaitSignals([]string{"order_approved"}, 5*time.Minute)
    if sig.TimedOut {
        return errors.New("approval timed out")
    }

    // Durable call: Create shipment
    _, err = h.DurableCall("shipping", "CreateShipment", marshal(order.ID))
    if err != nil {
        return err
    }

    h.DurableLog("Order completed")
    return nil
}
```

**Cleat (Python):**
```python
@durable_entry
def payment_workflow(h: HostCalls, order: dict) -> str:
    # Reserve inventory
    reserve_resp = h.durable_call("inventory", "Reserve", order.get("items"))

    # Charge payment
    try:
        charge_resp = h.durable_call("payment", "Charge", order.get("total"))
    except Exception:
        # Compensate: release inventory
        h.durable_call("inventory", "Release", reserve_resp)
        raise

    # Wait for approval signal
    sig = h.await_signals(["order_approved"], 300000)

    # Create shipment
    h.durable_call("shipping", "CreateShipment", order["id"])

    return json.dumps({"status": "completed"})
```

---

## Known Gaps and Workarounds

### 1. No Workflow/Activity Distinction

Temporal enforces a split between deterministic workflow code and non-deterministic
activity code. Cleat has a single `HostCalls` interface — every external interaction
goes through `durable_call()`. This is simpler but means:

- **Gap**: No built-in activity retry isolation. A retryable activity in Temporal retries
  independently; in Cleat, `durable_call_with_retry()` provides server-side retry but
  the whole workflow step is re-executed on replay.
- **Workaround**: Use `durable_call_with_retry()` for operations that need automatic
  retry. Use `durable_call_with_heartbeat()` for long-running operations with progress
  reporting.

### 2. No Context Propagation

Temporal passes a `workflow.Context` through the call chain for cancellation,
deadlines, and child workflow options. Cleat passes `HostCalls` directly.

- **Gap**: No built-in cancellation propagation. Cleat workflows check cancellation
  explicitly via `poll_cancellation()`.
- **Workaround**: Periodically call `h.poll_cancellation()` in long-running workflows.
  Use `run_detached()` for sections that should survive cancellation.

### 3. Unit Differences

- **Gap**: Cleat uses milliseconds for sleep (`durable_sleep(5000)` = 5s), while
  Temporal Go uses `time.Duration` (nanosecond precision) and Temporal Python uses
  `timedelta` / seconds.
- **Workaround**: Use constants: `h.durable_sleep(5000)` for 5 seconds in Python;
  `h.DurableSleep(5 * time.Second)` in Go.

### 4. No Namespace/Visibility

Temporal has task queues, namespaces, and visibility stores. Cleat has task queues
(starting in v0.9+) and single-database visibility via SQL queries.

- **Workaround**: Use Cleat's `task_queue` field on workflow deployments to route
  workflows to different worker pools. Use SQL queries on `workflow_instances` for
  custom visibility.

### 5. No List/Describe Workflow APIs (REST)

Temporal has `ListOpenWorkflow` / `ListClosedWorkflow` gRPC APIs. Cleat exposes
REST endpoints at `/api/workflows` for listing and filtering.

- **Workaround**: Use Cleat's REST API (`GET /api/workflows?status=running&limit=50`)
  or query `workflow_instances` directly via SQL.

### 6. No Side Effect Equivalent

Temporal has `Workflow.SideEffect` for non-deterministic code that runs once and is
recorded. Cleat does not have an equivalent.

- **Workaround**: Use `durable_call("side_effect", "compute", {data: ...})` with a
  stub service that runs the non-deterministic computation and records the result.
  Or use `random()` for deterministic randomness.

### 7. Versioning Model Differences

Temporal versions workflows by keeping old worker binaries running. Cleat versions
by storing WASM blobs in the database.

- **Gap**: No concept of "worker build ID" — the WASM blob IS the version.
- **Advantage**: No need to keep old worker pools alive for in-flight workflows.
  Rollback is a database UPDATE, not a worker re-deploy.
- **Workaround**: Use `version()` and `min_version()` in workflow code for
  backward-compatible logic branches.
