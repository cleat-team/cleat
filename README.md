# cleat

A cleat execution framework for Go and Rust. Workflows are written in near-standard Go (or Rust with `#[cleat_entry]`), compiled to WebAssembly, and stored in PostgreSQL. The framework handles replay, checkpointing, failover, and observability with minimal developer overhead. Includes an embedded Svelte web UI for workflow monitoring and schedule management.

## Installation

### From source

```bash
git clone https://github.com/rcownie/cleat.git
cd cleat

# CLI tools
go install ./cmd/cleat           # cleat build/vet/deploy/versions/rollback
go install ./cmd/cleat-worker/   # production worker daemon
go install ./cmd/cleat-gen/      # typed client code generator
```

### go install

```bash
go install github.com/rcownie/cleat/cmd/cleat@latest
go install github.com/rcownie/cleat/cmd/cleat-worker@latest
go install github.com/rcownie/cleat/cmd/cleat-gen@latest
```

### Dependencies

- **Go 1.26+** with `GOOS=wasip1 GOARCH=wasm` target (bundled with Go 1.22+)
- **PostgreSQL 14+** for the worker daemon and workflow storage
- **TinyGo** (optional) for smaller WASM binaries via `--target tinygo`
- **Rust toolchain** (optional) for Rust workflows via `--target rust`
- **Node.js** (optional) to build the web UI for the worker dashboard

## Quick start

The following is a complete workflow example from `testdata/basic/order.go`. It models an order-processing pipeline with nested cleat calls and compensation logic.

```go
package basic

import (
    "encoding/json"
    "fmt"
    "github.com/rcownie/cleat/cleat"
)

type CartItem struct {
    SKU      string `json:"sku"`
    Quantity int    `json:"quantity"`
}

func PlaceOrder(h cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    if len(cart) == 0 {
        return "", fmt.Errorf("cart is empty")
    }

    reservation, err := validateAndReserve(h, userID, cart)
    if err != nil {
        return "", fmt.Errorf("inventory step failed: %w", err)
    }

    charge, err := processPayment(h, userID, reservation.TotalCents)
    if err != nil {
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("payment failed: %w", err)
    }

    trackingID, err := fulfillOrder(h, reservation, charge)
    if err != nil {
        refundPayment(h, charge.ChargeID)
        releaseReservation(h, reservation.ReservationID)
        return "", fmt.Errorf("fulfillment failed: %w", err)
    }

    _ = notifyCustomer(h, userID, trackingID)
    return trackingID, nil
}
```

Build and deploy:

```bash
# 1. Compile the workflow package to WASM (Go)
cleat build -o ./out ./testdata/basic/

# 2. Compile a Rust workflow
cleat build --target rust -o ./out ./examples/rust-workflow/

# 3. Validate without compiling
cleat vet ./testdata/basic/

# 4. Deploy to PostgreSQL
cleat deploy --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --name place_order ./out/place_order.wasm

# 5. Run the worker with the web UI
cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --api-addr :8080

# 6. Manage cron schedules
cleat schedule add hourly-report --cron "0 * * * *" --def place_order

# 7. Run SDK and unit tests
go test ./...
```

## Architecture

```
+------------------+         +-------------------+         +-----------------+
|  Workflow Author  |         |  CLI (cleat)    |         |  PostgreSQL     |
|  (Go / Rust)      | ------> |  build / vet /    | ------> |  workflow_defs  |
|                   |         |  deploy / schedule|         |  (WASM blobs)   |
+------------------+         +-------------------+         +-----------------+
        |                            |                              |
        |  writes                    |  1. Load & analyze           |  stores
        v                            v                              v
+------------------+         +-------------------+         +-----------------+
|  Standard Go      |         |  Transformer       |         |  workflow_inst  |
|  + HostCalls      |         |  Pipeline:         |         |  (state, queue, |
|                   |         |  - analyzer.Load   |         |   timers)       |
|  func PlaceOrder( |         |  - callgraph.Build |         +-----------------+
|    h HostCalls,   |         |  - closure.Compute |                |
|    input string,  |         |  - transform       |                |
|  ) error { ... }  |         |  - wasm.Compile    |                v
+------------------+         +-------------------+         +-----------------+
                                                             |  Stateless      |
                                                             |  Workers        |
+------------------+                                          |                 |
|  Web UI (Svelte)  | <-- HTTP -->                           |  SKIP LOCKED    |
|  embedded in      |                                          |  claim instance |
|  worker binary    |                                          |  load WASM      |
|  /api/* endpoints |                                          |  replay / exec  |
+------------------+                                          +-----------------+
```

### Transformer Pipeline

The CLI's `build` command runs a five-stage pipeline:

1. **analyzer.Load** -- loads Go packages via `go/packages`, parses AST, resolves types, identifies exported functions as entry points.
2. **callgraph.Build** -- builds a static call graph of the target package using the `callgraph` package.
3. **closure.Compute** -- computes the cleat closure: the set of functions reachable from entry points that make `HostCalls`. Validates that every path through the closure passes `HostCalls` correctly.
4. **transform** -- rewrites source files: adds `HostCalls` parameters to functions that need them (auto-threading), inserts import statements, generates WASM export wrappers.
5. **wasm.Compile** -- generates WASM import declarations, host adapter code, and compiles to `wasip1` binary. Supports both `go` (standard toolchain) and `tinygo` targets.

### Host Runtime

The host runtime uses **wazero** (a zero-dependency WebAssembly runtime for Go) to execute compiled WASM modules. Execution follows a checkpoint/replay model:

- WASM modules import 15 host functions from the `env` module (e.g., `cleat_call`, `cleat_sleep`, `cleat_now`, `cleat_call_heartbeat`).
- On first execution, the host runs the entry point, records every `DurableCall` request/response in the event history, and persists state to PostgreSQL.
- On replay (e.g., after a worker crash or suspension), the host replays the event history. Completed calls return cached responses instead of re-executing. The workflow resumes from the last incomplete step.

### Worker Daemon

The worker (`cleat-worker`) polls PostgreSQL for runnable workflow instances using `SELECT ... FOR UPDATE SKIP LOCKED`. Each claimed instance loads its WASM module and event history, replays or executes the workflow, then persists new events. Workers are stateless and can be horizontally scaled.

### WASM Boundary

The WASM boundary is defined by 15 host function imports on the `env` module:

`cleat_call`, `cleat_call_heartbeat`, `cleat_sleep`, `cleat_now`, `cleat_random`, `cleat_log`, `cleat_version`, `cleat_min_version`, `cleat_defer`, `cleat_poll_cancellation`, `cleat_poll_signal`, `cleat_continue_as_new`, `cleat_child_workflow`, `cleat_await_child`, `cleat_await_signals`, `set_query_state`

Strings cross the boundary through a pointer+length protocol: the caller writes string data into the module's linear memory at a scratch region (10 MB offset) and passes `(ptr, len)` pairs. Responses are written back to the same region. The output buffer is 64 KB by default.

## SDK API overview

The SDK provides a single import -- `cleat.HostCalls` -- which is passed as the first parameter to entry point functions. All external interactions go through this interface, enabling deterministic replay.

### HostCalls interface (key methods)

```go
type HostCalls interface {
    DurableCall(service, operation, requestJSON string) (responseJSON string, err error)
    DurableCallTyped(service, operation string, request, result interface{}) error
    DurableCallWithOptions(opts CallOptions, service, operation, requestJSON string) (string, error)
    DurableSleep(d time.Duration)
    AwaitSignals(signalNames []string, timeout time.Duration) SignalResult
    DurableDefer(description string) (deferID string, err error)
    Now() time.Time
    Random() int64
    ChildWorkflow(name, inputJSON string) (runID string, err error)
    AwaitChild(runID string) (resultJSON string, err error)
    ContinueAsNew(newInputJSON string) error
    LogKV(message string, kvs ...interface{})
    // ...
}
```

`DurableCallTyped` marshals request structs to JSON and unmarshals responses automatically, eliminating magic strings and manual JSON handling. It is the recommended way to call services.

### PlaceOrder (basic cleat calls)

```go
func PlaceOrder(h cleat.HostCalls, input string) error {
    items, _ := h.DurableCall("inventory", "CheckAvailability", input)
    payment, _ := h.DurableCall("payments", "Charge", items)
    h.DurableCall("notify", "SendConfirmation", payment)
    return nil
}
```

Each `DurableCall` records the request and response in the event history. On replay, previously-completed calls return their cached results instead of re-executing.

### Saga (compensating transactions)

```go
func CreateOrder(h cleat.HostCalls, input string) error {
    defer h.DurableDeferFunc(func() {
        // Compensate on any failure.
        h.DurableCall("inventory", "ReleaseReservation", "order-123")
        h.DurableCall("payments", "Refund", "order-123")
    })
    h.DurableCall("inventory", "Reserve", input)
    h.DurableCall("payments", "Charge", input)
    return nil
}
```

`DurableDefer` runs the compensation block if the function returns an error. If the function succeeds, the deferred block is skipped. On replay, the compensation is not re-executed if it already ran.

For structured multi-step compensation, use `cleat.NewSaga()`:

```go
s := cleat.NewSaga()
s.AddStep("charge", chargeFn, refundFn)
s.AddStep("assign_driver", assignFn, releaseFn)
if err := s.Run(h); err != nil {
    return err
}
```

The Saga runs forward steps in order. If any step fails, previously completed steps are compensated in reverse order.

### RetryPolicy field name mapping

The `RetryPolicy` struct fields differ from Temporal/DBOS naming conventions:

| Field | Type | Description |
|-------|------|-------------|
| `MaxAttempts` | `int` | Maximum retry attempts (not `MaximumAttempts`) |
| `InitialInterval` | `time.Duration` | Initial backoff interval |
| `BackoffCoefficient` | `float64` | Backoff multiplier (2.0 = double) |
| `MaxInterval` | `time.Duration` | Maximum backoff interval |
| `NonRetryableErrors` | `[]string` | Error substrings that skip retry |

The struct field is `MaxAttempts` (not `MaximumAttempts`). Nil-safe accessor methods provide `MaximumAttempts`-style naming for interface compliance:

```go
rp.MaximumAttempts()  // returns rp.MaxAttempts (or 0 if rp is nil)
rp.MaximumInterval()  // returns rp.MaxInterval (or 0 if rp is nil)
```

Usage examples:

```go
// Using the field directly:
policy := cleat.RetryPolicy{
    MaxAttempts:        5,
    InitialInterval:    1 * time.Second,
    BackoffCoefficient: 2.0,
    MaxInterval:        30 * time.Second,
}

// Using DefaultRetryPolicy():
policy := cleat.DefaultRetryPolicy()

// Passing to a call:
result, err := h.DurableCallWithOptions(
    cleat.CallOptions{Retry: &policy},
    "service", "Op", requestJSON,
)
```

### DurableDefer: string vs closure

`DurableDefer` takes a **string description**, not a closure/callback:

```go
deferID, _ := h.DurableDefer("release inventory reservation")
```

The host records the description for observability during replay.

For closure-based cleanup, use `DurableDeferFunc`:

```go
h.DurableDeferFunc(func() {
    h.DurableCall("inventory", "ReleaseReservation", "order-123")
})
```

For multi-step compensation, use `cleat.NewSaga()` (defined in `cleat/runtime.go`) instead of chaining multiple defers:

```go
s := cleat.NewSaga()
s.AddStep("charge", chargeFn, refundFn)
s.AddStep("assign_driver", assignFn, releaseFn)
if err := s.Run(h); err != nil {
    return err
}
```

The Saga provides structured, ordered compensation with typed result collection (`NewSagaTyped[T]`), concurrent step execution (`AddParallel`), and `TerminalError` handling, which `DurableDefer` does not support.

### Per-call timeout limitations

Per-call timeouts via `CallOptions.Timeout` are defined in the SDK but **not yet enforced on the host side** during WASM execution:

```go
opts := cleat.CallOptions{
    Timeout: 30 * time.Second,
    Retry:   &policy,
}
result, err := h.DurableCallWithOptions(opts, "service", "Op", requestJSON)
```

Workaround: use `DurableSleep` + polling for timeout-aware patterns:

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

Host-side timeout enforcement is on the roadmap.

### Multi-export WASM modules (Go)

A single Go package can export multiple workflow entry points. The transformer pipeline generates a WASM export for each exported (capitalized) function that accepts `cleat.HostCalls` as its first parameter:

```go
// assembly/myworkflows.go
package myworkflows

func PlaceOrder(h cleat.HostCalls, input string) error {
    // ...
}

func CancelOrder(h cleat.HostCalls, input string) error {
    // ...
}

func GetOrderStatus(h cleat.HostCalls, input string) error {
    // ...
}
```

Compiled with `cleat build`, each function becomes a named WASM export. The host dispatches workflow invocations by matching the called workflow name to the function name. WASM exports are named after the Go function names. There is no decorator required -- any exported function accepting `HostCalls` as its first parameter is automatically treated as an entry point.

### Signals (external events)

```go
func WaitForPayment(h cleat.HostCalls, input string) error {
    h.DurableCall("payments", "CreateInvoice", input)
    result, _ := h.AwaitSignals("payment_received")
    h.DurableCall("fulfillment", "ShipOrder", result)
    return nil
}
```

`AwaitSignals` pauses the workflow until an external signal arrives or a timeout expires. Signal data is recorded in the event history for deterministic replay.

### Auto-threading

Helper functions in the call path automatically receive `h cleat.HostCalls` through the transformer's auto-threading pass rather than requiring manual propagation:

```go
func PlaceOrder(h cleat.HostCalls, input string) error {
    return processOrder(h, input)  // h is auto-threaded
}

// The transformer adds h cleat.HostCalls as the first parameter
// and threads it through to called cleat leaves.
func processOrder(h cleat.HostCalls, input string) error {
    h.DurableCall("inventory", "Check", input)
    return nil
}
```

### Timer and polling pattern

```go
// PollUntil repeatedly checks a condition at the given interval until a
// deadline is exceeded.
status, err := cleat.PollUntil(h, 30*time.Second, 30*time.Minute,
    func() (string, error) {
        return checkPickupStatus(driverID)
    },
    func(s string) bool { return s == "picked_up" },
)
```

## CLI Reference

All commands are available through the `cleat` binary.

### cleat build

Analyze and compile a workflow package to WASM.

```
cleat build [-o <dir>] [--target <target>] <package>

Flags:
  -o <dir>        Output directory for generated files (default: temp dir)
  --target        Compilation target: "go" (default), "tinygo", or "rust"
```

The pipeline loads the package, analyzes call graphs, computes cleat closures, verifies HostCalls threading, generates WASM imports/exports and host adapters, and compiles to a `wasip1` binary.

### cleat vet

Validate a workflow package without compiling to WASM.

```
cleat vet <package>
```

Reports entry points, cleat leaf functions, threading errors, closure errors, and warnings. Exits with code 1 if any errors are found.

### cleat deploy

Upload a compiled WASM workflow to PostgreSQL.

```
cleat deploy [--name <name>] [--namespace <ns>] <wasm-file>

Flags:
  --name <name>      Workflow name (derived from filename if not set)
  --namespace <ns>   Namespace (default: "default")

Common flags:
  --db <connstr>     PostgreSQL connection string (or CLEAT_DATABASE_URL env)
```

Without `--db` or `CLEAT_DATABASE_URL`, performs a dry run that prints what would be deployed.

### cleat versions

List all deployed versions of a workflow, latest first.

```
cleat versions <workflow-name>

Requires:
  --db <connstr>   or CLEAT_DATABASE_URL env
```

### cleat rollback

Set the active version for new workflow instances.

```
cleat rollback <workflow-name> <version>

Requires:
  --db <connstr>   or CLEAT_DATABASE_URL env
```

Confirms the version exists and reports that new instances will use the specified version.

### cleat-gen

Generate typed client wrappers for cleat service calls.

```
cleat-gen client [-o <file>] [-service <name>] [-p <package>] <spec-dir>
```

The spec directory contains Go files with request/response structs and a `Client` interface. The generator produces a concrete implementation using `DurableCallTyped`.

## Worker deployment

The `cleat-worker` daemon polls PostgreSQL for runnable workflow instances and drives execution.

```bash
# Run with default settings
cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable"

# With explicit concurrency and heartbeat
cleat-worker --db "postgres://user:pass@localhost/cleat?sslmode=disable" \
    --concurrency 20 \
    --heartbeat 10s \
    --poll 250ms
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--db` | `DATABASE_URL` env | PostgreSQL connection URL |
| `--api-addr` | (disabled) | HTTP API listen address (e.g., `:8080`) — serves REST API, metrics, and web UI |
| `--namespace` | `default` | Workflow namespace to claim from |
| `--concurrency` | 10 | Max concurrent workflow executions |
| `--heartbeat` | 5s | Heartbeat interval for claimed instances |
| `--poll` | 500ms | Poll interval when no work is available |

### How it works

1. The dispatch loop calls `ClaimWorkflow`, which runs `SELECT ... FOR UPDATE SKIP LOCKED` on `workflow_instances` rows with `status = 'ready'` and `next_wake_at <= now()`.
2. Each claimed instance is executed in its own goroutine (up to `--concurrency`).
3. A background heartbeat goroutine updates `heartbeat_at` for all in-flight instances. If the heartbeat fails (e.g., DB connection loss), the worker reconnects with exponential backoff.
4. A reaper goroutine reclaims instances with stale heartbeats every 30 seconds.
5. A schedule loop fires due cron schedules every 15 seconds.
6. On graceful shutdown (SIGINT/SIGTERM), the worker waits for all in-flight workflows before exiting.
7. WASM modules are cached in memory (keyed by `def_name:def_version`) to avoid repeated database loads.
8. When `--api-addr` is set, the worker serves a REST API at `/api/*`, Prometheus metrics at `/metrics`, and the embedded Svelte web UI at `/`.

### Concurrency and scaling

Workers are stateless and horizontally scalable. Multiple `cleat-worker` instances can run concurrently against the same database -- `SKIP LOCKED` ensures each workflow instance is claimed by exactly one worker. Set `--concurrency` based on available CPU and the workload's I/O profile.

### Heartbeat monitoring

The heartbeat interval (`--heartbeat`, default 5s) controls how often the worker updates `heartbeat_at` in `workflow_instances`. If a worker crashes, its claimed instances become reclaimable after the heartbeat stops updating. Monitor `idx_instances_heartbeat` to detect stale assignments.

## Database setup

Run `schema.sql` against a PostgreSQL 14+ database before deploying workflows.

```bash
psql -U postgres -d cleat -f schema.sql
```

### Tables

**workflow_defs** -- stores compiled WASM blobs versioned by workflow name.

| Column | Type | Description |
|--------|------|-------------|
| name | TEXT | Workflow name (part of composite PK) |
| version | INTEGER | Monotonically increasing version |
| wasm_bytes | BYTEA | Compiled WASM module binary |
| entry_points | TEXT[] | Exported entry point names |
| created_at | TIMESTAMPTZ | Deployment timestamp |

**workflow_instances** -- tracks individual workflow execution state.

| Column | Type | Description |
|--------|------|-------------|
| id | TEXT | Unique workflow instance ID |
| def_name | TEXT | Reference to workflow_defs.name |
| def_version | INTEGER | Reference to workflow_defs.version |
| status | TEXT | ready, running, completed, failed, suspended |
| input | JSONB | Workflow input arguments |
| assigned_to | TEXT | Worker ID currently claiming this instance |
| heartbeat_at | TIMESTAMPTZ | Last heartbeat from the claiming worker |
| next_wake_at | TIMESTAMPTZ | When to retry (sleep/suspend deadline) |
| result | JSONB | Workflow result (if completed) |
| error_msg | TEXT | Error message (if failed) |
| cancellation_requested | BOOLEAN | Whether cancellation has been requested |

**event_history** -- ordered list of every cleat call, sleep, signal, defer, and child workflow event.

| Column | Type | Description |
|--------|------|-------------|
| workflow_id | TEXT | Reference to workflow_instances.id |
| step | INTEGER | Monotonically increasing event sequence number |
| event_type | TEXT | call, sleep, await_signals, signal_received, defer, child_workflow, continue_as_new, heartbeat |
| service | TEXT | Target service name (for call events) |
| operation | TEXT | Target operation name (for call events) |
| request | JSONB | Request payload |
| response | JSONB | Response payload |
| error | TEXT | Error message (if call failed) |
| signal_name | TEXT | Signal name (for signal events) |
| signal_payload | JSONB | Signal payload |
| duration_ms | BIGINT | Sleep duration in milliseconds |

**workflow_signals** -- external signals delivered to running workflows.

| Column | Type | Description |
|--------|------|-------------|
| workflow_id | TEXT | Reference to workflow_instances.id |
| signal_name | TEXT | Signal name (part of composite PK) |
| payload | JSONB | Signal payload |
| delivered_at | TIMESTAMPTZ | Delivery timestamp |

### Indexes

- `idx_instances_ready` on `(status, next_wake_at)` WHERE `status = 'ready'` -- accelerates the worker poll loop.
- `idx_instances_heartbeat` on `(assigned_to, heartbeat_at)` WHERE `status = 'running'` -- enables monitoring and stale-assignment detection.
- `idx_defs_active` on `(name, version DESC)` -- speeds up latest-version lookups for deployment.

## Testing workflows

The `cleattest` package provides a `TestEnv` that simulates the host runtime without compiling to WASM. It replaces all host function calls with configurable stubs and a deterministic simulated clock.

### Full test example

```go
package myworkflow_test

import (
    "testing"
    "github.com/rcownie/cleat/cleat"
    "github.com/rcownie/cleat/cleat/cleattest"
)

func TestPlaceOrder_Success(t *testing.T) {
    env := cleattest.NewTestEnv()
    defer env.Reset()

    // Register stubs for all cleat calls the workflow will make.
    env.OnCall("inventory", "Reserve", nil).
        ReturnJSON(map[string]interface{}{
            "reservation_id": "resv_abc123",
            "total_cents":    3299,
        }, nil)
    env.OnCall("payments", "Charge", nil).
        ReturnJSON(map[string]interface{}{
            "charge_id": "chg_xyz789",
            "amount":    3299,
        }, nil)
    env.OnCall("shipping", "CreateShipment", nil).
        ReturnJSON(map[string]interface{}{"tracking_id": "TRACK-123"}, nil)
    env.OnCall("notifications", "SendEmail", nil).Return("", nil)

    // Run the workflow using the mock HostCalls.
    h := env.H()
    result, err := PlaceOrder(h, "user_42", []CartItem{
        {SKU: "SKU-001", Quantity: 2},
    })

    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if result != "TRACK-123" {
        t.Fatalf("expected tracking TRACK-123, got %s", result)
    }

    // Verify the expected calls were made.
    env.AssertCalled(t, "inventory", "Reserve")
    env.AssertCalled(t, "payments", "Charge")
    env.AssertCalled(t, "shipping", "CreateShipment")
    env.AssertNotCalled(t, "inventory", "Release")
}

func TestPlaceOrder_Error_Compensation(t *testing.T) {
    env := cleattest.NewTestEnv()
    defer env.Reset()

    // Payment succeeds but fulfillment fails -- compensation should run.
    env.OnCall("inventory", "Reserve", nil).
        ReturnJSON(map[string]interface{}{
            "reservation_id": "resv_abc123",
            "total_cents":    3299,
        }, nil)
    env.OnCall("payments", "Charge", nil).
        ReturnJSON(map[string]interface{}{
            "charge_id": "chg_xyz789",
            "amount":    3299,
        }, nil)
    env.OnCall("shipping", "CreateShipment", nil).
        Return("", fmt.Errorf("shipping service unavailable"))
    env.OnCall("payments", "Refund", nil).Return("", nil)
    env.OnCall("inventory", "Release", nil).Return("", nil)

    _, err := PlaceOrder(env.H(), "user_42", []CartItem{{SKU: "SKU-001", Quantity: 1}})
    if err == nil {
        t.Fatal("expected error from failed fulfillment")
    }

    // Compensation calls should have been made.
    env.AssertCalled(t, "payments", "Refund")
    env.AssertCalled(t, "inventory", "Release")
}
```

### TestEnv API

| Method | Description |
|--------|-------------|
| `NewTestEnv()` | Creates a TestEnv with clock starting at 2024-01-01T00:00:00Z |
| `H()` | Returns the mock `HostCalls` for workflow code |
| `OnCall(service, op, matcher)` | Registers a stub; matcher can be nil, string, or `func(string) bool` |
| `Signal(name, payload)` | Delivers a signal immediately |
| `AfterSignal(delay, name, payload)` | Schedules a signal at a future simulated time |
| `AdvanceTime(d)` | Advances the simulated clock, wakes sleepers, delivers due signals |
| `SetTime(t)` | Sets the simulated clock to an absolute time |
| `Now()` | Returns the current simulated time |
| `CallHistory()` | Returns all calls made through the mock |
| `AssertCalled(t, svc, op)` | Fails the test if the call was not made |
| `AssertNotCalled(t, svc, op)` | Fails the test if the call was made |
| `SetRandomSeq(seq)` | Configures deterministic random values |
| `SetVersion(v)` | Sets the workflow version for testing versioned code |
| `QueryState(key)` | Reads query state set via `H().SetQueryState()` |
| `Reset()` | Clears all stubs, history, signals, and resets the clock |

Using `TestEnv` avoids the full WASM compilation cycle (seconds, not milliseconds), allows deterministic time control without `time.Sleep`, and lets you assert exact call patterns including compensation paths.

## Status and roadmap

### Done

- Go transformer pipeline (`cleat build`) — analysis, call graph, closure, transform, WASM compile
- Rust transformer pipeline (`cleat build --target rust`) — `cleat-sdk` crate + `#[cleat_entry]` proc-macro
- Timers/sleep, retry policies, server-side retry (`cleat_call_retry`)
- Signals/external events, workflow cancellation
- DurableDefer, Saga (compensating transactions)
- Child workflows, ContinueAsNew
- Testing framework (`cleattest.TestEnv`) — WASM-free, deterministic clock
- Dev mode (`cleat dev`) — WASM-free local execution
- Dead letter queue
- Queries (`SetQueryState` + `GET /api/workflows/:id/query?key=X`)
- Cron scheduling (`cleat schedule` CLI + REST API)
- Activity heartbeating (`DurableCallWithHeartbeat`)
- Namespace isolation (`--namespace` flag)
- Prometheus metrics (`/metrics`)
- Svelte web UI (dashboard, workflow list/detail, schedule management)
- Typed client code generator (`cleat-gen`)

### Next

- **Task routing** — route workflow types to specific worker pools
- **Automatic history compaction** — prune long event histories
- **Getting-started tutorial** — end-to-end walkthrough
- **Load testing and performance benchmarks**
- **Ecosystem integrations** — Helm chart, Grafana dashboards
