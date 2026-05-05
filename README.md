# cleat

A durable execution framework for Go. Workflows are written in near-standard Go, compiled to WebAssembly, and stored in PostgreSQL. The framework handles replay, checkpointing, failover, and observability with minimal developer overhead.

## Quick start

```bash
# Build and vet a workflow package
go run ./cmd/durable build -o ./out ./testdata/basic/
go run ./cmd/durable vet ./testdata/basic/

# Deploy a compiled WASM workflow to the database (dry run without --db)
go run ./cmd/durable deploy --db "postgres://..." --name myworkflow ./out/place_order.wasm

# Run tests
go test ./...
```

## Architecture

```
+------------------+         +-------------------+         +-----------------+
|  Workflow Author  |         |  CLI (durable)    |         |  PostgreSQL     |
|  (Go source)      | ------> |  build / vet /    | ------> |  workflow_defs  |
|                   |         |  deploy           |         |  (WASM blobs)   |
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
                                                             |                 |
                                                             |  SKIP LOCKED    |
                                                             |  claim instance |
                                                             |  load WASM      |
                                                             |  replay / exec  |
                                                             +-----------------+
```

## SDK API overview

The SDK provides a single import -- `durable.HostCalls` -- which is passed as the first parameter to entry point functions. All external interactions go through this interface, enabling deterministic replay.

### PlaceOrder (basic durable calls)

```go
func PlaceOrder(h durable.HostCalls, input string) error {
    items, _ := h.DurableCall("inventory", "CheckAvailability", input)
    payment, _ := h.DurableCall("payments", "Charge", items)
    h.DurableCall("notify", "SendConfirmation", payment)
    return nil
}
```

Each `DurableCall` records the request and response in the event history. On replay, previously-completed calls return their cached results instead of re-executing.

### Saga (compensating transactions)

```go
func CreateOrder(h durable.HostCalls, input string) error {
    defer h.DurableDefer(func() {
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

### Signals (external events)

```go
func WaitForPayment(h durable.HostCalls, input string) error {
    h.DurableCall("payments", "CreateInvoice", input)
    result, _ := h.AwaitSignals("payment_received")
    h.DurableCall("fulfillment", "ShipOrder", result)
    return nil
}
```

`AwaitSignals` pauses the workflow until an external signal arrives or a timeout expires. Signal data is recorded in the event history for deterministic replay.

### Auto-threading

Helper functions in the call path automatically receive `h durable.HostCalls` through the transformer's auto-threading pass rather than requiring manual propagation:

```go
func PlaceOrder(h durable.HostCalls, input string) error {
    return processOrder(h, input)  // h is auto-threaded
}

// The transformer adds h durable.HostCalls as the first parameter
// and threads it through to called durable leaves.
func processOrder(h durable.HostCalls, input string) error {
    h.DurableCall("inventory", "Check", input)
    return nil
}
```

## Status and roadmap

**P0 (stable):** Timers/sleep, retry policies, idempotency keys, dead letter queue, secrets/credentials.

**P1 (stable):** Signals/external events, workflow cancellation, DurableDefer (cleanup on exit), testing framework, transformer (Go to WASM).

**P2 (planned):** Queries, schema evolution, scheduling/CRON, child workflows.

**P3 (future):** Multi-tenancy, workflow prioritization, history compaction.
