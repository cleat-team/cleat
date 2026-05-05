# Durable SDK — Production Hardening Plan

## Overview

The current SDK proves the core thesis: compose normal Go functions, auto-thread context, compile to WASM, replay deterministically. The fooddash example shows this works. But the gap between "works in a demo" and "trustworthy in production" is wide. This plan closes that gap across four dimensions: correctness, type safety, operational maturity, and developer experience.

## Phase 1: Correctness (bug fixes)

### 1.1 Nil compensation panic in Saga.Run

**Bug:** `CancelOrder` passes `nil` compensate functions. `Saga.Run` calls `cs.compensate()` without a nil guard. Any failure during `CancelOrder` crashes the WASM module.

**Fix:** In `Saga.Run`, skip nil compensate functions:

```go
for j := completed - 1; j >= 0; j-- {
    cs := s.steps[j]
    if cs.compensate == nil {
        continue
    }
    ...
}
```

Also: make `CancelOrder`'s compensate functions explicit rather than nil, so failures are logged instead of silently ignored.

**Files:** `durable/runtime.go:388-394`, `examples/fooddash/order.go:173-186`

### 1.2 JSON injection via manual string formatting

**Bug:** Eight call sites build JSON with `fmt.Sprintf`. A `userID` containing `"` or `}` produces malformed JSON at best, corrupt data at worst.

**Fix:** Replace all `fmt.Sprintf` JSON construction with `encoding/json.Marshal`:

```go
// Before:
request := fmt.Sprintf(`{"user_id":"%s","amount_cents":%d}`, userID, amountCents)

// After:
type chargeRequest struct {
    UserID      string `json:"user_id"`
    AmountCents int    `json:"amount_cents"`
    Currency    string `json:"currency"`
}
req, _ := json.Marshal(chargeRequest{UserID: userID, AmountCents: amountCents, Currency: "usd"})
resp, err := h.DurableCallJSON("payments", "Charge", string(req), &charge)
```

Better: add `DurableCallTyped(service, operation string, request, response interface{}) error` that handles both marshal and unmarshal in one call, eliminating JSON from the caller's sight entirely.

**Files:** `durable/runtime.go` (new method), `examples/fooddash/order.go` (update all call sites)

### 1.3 PlaceLargeOrder is non-functional

**Bug:** References `firstBatch`/`remaining` but processes neither. `ContinueAsNew` passes hardcoded empty JSON instead of actual remaining items.

**Fix:** Actually process `firstBatch`, serialize `remaining` into the `ContinueAsNew` input:

```go
func PlaceLargeOrder(h durable.HostCalls, userID string, items []OrderItem) (string, error) {
    if len(items) <= 10 {
        return processOrder(h, userID, items)
    }
    firstBatch := items[:10]
    remaining := items[10:]
    _, err := processOrder(h, userID, firstBatch)
    if err != nil {
        return "", err
    }
    input, _ := json.Marshal(placeLargeOrderInput{UserID: userID, Items: remaining})
    return "", h.ContinueAsNew(string(input))
}
```

**Files:** `examples/fooddash/order.go:203-219`

---

## Phase 2: Type Safety (eliminate magic strings)

### 2.1 Typed service client code generator

**Problem:** `h.DurableCallJSON("payments", "Charge", ...)` has three untyped strings before you get to data. Service names and operation names are not validated at compile time.

**Solution:** A `durable-gen` code generator that reads a service spec and produces typed clients.

**Service spec format** — a simple Go interface:

```go
// spec/payments/spec.go
package payments_spec

type ChargeRequest struct {
    UserID      string `json:"user_id"`
    AmountCents int    `json:"amount_cents"`
    Currency    string `json:"currency"`
}

type ChargeResponse struct {
    ChargeID string `json:"charge_id"`
    Status   string `json:"status"`
}

type RefundRequest struct {
    ChargeID string `json:"charge_id"`
}

// PaymentsClient is the typed interface backed by h.DurableCall.
type PaymentsClient struct {
    h durable.HostCalls
}

func NewPaymentsClient(h durable.HostCalls) *PaymentsClient { ... }

func (c *PaymentsClient) Charge(req ChargeRequest) (*ChargeResponse, error) {
    var resp ChargeResponse
    if err := c.h.DurableCallTyped("payments", "Charge", req, &resp); err != nil {
        return nil, err
    }
    return &resp, nil
}

func (c *PaymentsClient) Refund(req RefundRequest) error {
    return c.h.DurableCallTyped("payments", "Refund", req, nil)
}
```

**Code generation tool:** `durable-gen client <spec-dir>` generates typed clients from spec interfaces.

**Alternative (no code gen):** Use generics to build a typed wrapper at runtime:

```go
client := durable.NewTypedClient(h, "payments")
charge, err := durable.Call[ChargeRequest, ChargeResponse](client, "Charge", chargeReq)
```

The code-gen approach is preferred — better IDE support, discoverable method names.

**Files:** `cmd/durable-gen/main.go` (new), `internal/gen/` (new), `durable/runtime.go` (add `DurableCallTyped`)

### 2.2 Retrofit fooddash with typed clients

Replace all raw `h.DurableCall`/`h.DurableCallJSON` calls with typed client calls:

```go
// Before:
response, err := h.DurableCallJSON("dispatch", "FindDriver", request, &resp)

// After:
dispatch := dispatch.NewClient(h)
driverID, err := dispatch.FindDriver(address)
```

**Files:** `examples/fooddash/order.go`

---

## Phase 3: Operational Maturity

### 3.1 Retry policies per call

**Problem:** No retry configuration. A transient network error fails the workflow immediately.

**Solution:** Add `CallOptions` with embedded retry policy:

```go
type CallOptions struct {
    Retry *RetryPolicy
}

type RetryPolicy struct {
    MaxAttempts       int
    InitialInterval   time.Duration
    BackoffCoefficient float64
    MaxInterval       time.Duration
    NonRetryableErrors []string // error substrings that skip retry
}

// New methods on HostCalls:
DurableCallWithOptions(opts CallOptions, service, operation, requestJSON string) (string, error)
DurableCallJSONWithOptions(opts CallOptions, service, operation, requestJSON string, result interface{}) error
```

Default: no retry (backward compatible). When `opts.Retry != nil`, the SDK retries with exponential backoff. Non-retryable errors (e.g., "item not found") skip retry and fail immediately.

**Files:** `durable/runtime.go`

### 3.2 PollUntil without event bloat

**Problem:** Every 30-second poll iteration appends a history event via `DurableSleep`. With 30-minute timeouts, orders of magnitude more events than necessary.

**Solution:** Add `DurableTimer(deadline time.Time) (fired bool, err error)` that the runtime resolves with a single event at the deadline. `PollUntil` is rewritten to use it internally:

```go
func PollUntil[T any](h HostCalls, interval, timeout time.Duration,
    fn func() (T, error), done func(T) bool) (T, error) {

    deadline := h.Now().Add(timeout)
    for {
        val, err := fn()
        if err != nil { return zero, err }
        if done(val) { return val, nil }
        if h.Now().After(deadline) { return zero, fmt.Errorf("deadline exceeded") }
        // Single timer event instead of repeated sleep events
        nextCheck := h.Now().Add(interval)
        if nextCheck.After(deadline) {
            nextCheck = deadline
        }
        h.DurableSleep(nextCheck.Sub(h.Now()))
    }
}
```

Note: `DurableSleep` is still an event, but now it's one event per iteration that waits the full interval, rather than the old behavior. The real fix for event bloat requires host-side timer interrupt support (Phase 5), but increasing the sleep to a single long sleep per iteration is a pragmatic improvement. Actually, the real fix is that `DurableSleep` with a short interval (30s) creates one event per iteration regardless. The key insight is: there's no way around one history event per external interaction. The fix is not in PollUntil but in making the polling unnecessary — use signals (push) instead of polling (pull). We should document this pattern:

```go
// Better than PollUntil: use AwaitSignals with long timeout
result := h.AwaitSignals([]string{"pickup_confirmed"}, 30*time.Minute)
```

**Files:** `durable/runtime.go` (add `DurableTimer`), `examples/fooddash/order.go` (replace PollUntil with AwaitSignals where appropriate)

### 3.3 Versioning API with documentation

**Problem:** `h.Version()` exists on the interface but has zero examples.

**Solution:** Document the pattern with a concrete example in the fooddash app:

```go
func PlaceOrder(h durable.HostCalls, ...) (OrderResult, error) {
    // v1: items were []OrderItem
    // v2: items became []OrderItem with added DietaryPreferences field
    var validated []validatedItem
    var err error

    if h.Version() >= 2 {
        validated, err = validateMenuItemsV2(restaurantID, items)
    } else {
        validated, err = validateMenuItems(restaurantID, items)
    }
    ...
}
```

Also add `h.MinVersion() int` that declares the minimum version this code requires. If a workflow replays against newer code than it started with, the runtime can detect version skew.

**Files:** `durable/runtime.go` (add `MinVersion`), `examples/fooddash/order.go` (add versioned example)

### 3.4 Structured error types for service calls

**Problem:** `DurableCall` returns `(string, error)`. Callers can't distinguish between a network timeout (retryable) and "item not found" (non-retryable) without string-matching the error message.

**Solution:** Define standard error types:

```go
type CallError struct {
    Service   string
    Operation string
    Code      CallErrorCode
    Message   string
}

type CallErrorCode int
const (
    CallErrorUnknown       CallErrorCode = iota
    CallErrorTimeout                     // retryable
    CallErrorUnavailable                 // retryable
    CallErrorNotFound                    // non-retryable
    CallErrorInvalidRequest              // non-retryable
    CallErrorPermissionDenied            // non-retryable
)
```

The host adapter packs error codes into the int64 return alongside the response length, so the WASM module receives structured errors without string parsing.

**Files:** `durable/runtime.go`, `internal/wasm/adapter.go`, `internal/wasm/generator.go`

---

## Phase 4: Developer Experience

### 4.1 Workflow testing framework

**Problem:** No way to test a workflow without compiling to WASM and running a full host.

**Solution:** A `durable/durabletest` package with a mock `HostCalls` implementation:

```go
import "github.com/rcownie/durable/durable/durabletest"

func TestPlaceOrder(t *testing.T) {
    env := durabletest.NewTestEnv()
    
    // Register service responses
    env.OnCall("menu", "LookupItem", `{"sku":"pizza"}`).Return(`{"name":"Pizza","price_cents":1299,"available":true}`, nil)
    env.OnCall("payments", "Charge", durabletest.Any).Return(`{"charge_id":"chg_1","status":"ok"}`, nil)
    
    // Send a signal after 1 second (simulated time)
    env.AfterSignal(1*time.Second, "driver_accepted", `{"driver_name":"Alex"}`)
    
    // Run the workflow
    result, err := PlaceOrder(env.H(), "user_1", "rest_1", 
        []fooddash.OrderItem{{SKU: "pizza", Quantity: 1}},
        fooddash.DeliveryAddress{Street: "123 Main", City: "NYC", ZipCode: "10001"},
    )
    
    assert.NoError(t, err)
    assert.Equal(t, "confirmed", result.Status)
}
```

Key features:
- `OnCall(service, op, requestMatcher)` — register expected calls with responses
- `AfterSignal(delay, name, payload)` — deliver a signal after simulated time
- `H()` — returns a `HostCalls` that records all durable operations
- `AssertCalled(t, service, op)` — verify expected calls were made
- `AdvanceTime(d)` — manually advance simulated time

**Files:** `durable/durabletest/` (new package)

### 4.2 Heartbeat for long-running operations

**Problem:** No way to report progress during long-running calls like `checkPickupStatus`.

**Solution:** Add `DurableCallWithHeartbeat` that periodically invokes a callback during long calls:

```go
resp, err := h.DurableCallWithHeartbeat("shipping", "CreateShipment", req,
    10*time.Second, // heartbeat interval
    func(details string) {
        h.SetQueryState("shipment_progress", details)
    },
)
```

The host sends heartbeat events that the WASM module receives as progress callbacks.

**Files:** `durable/runtime.go`

### 4.3 Selector for waiting on multiple futures

**Problem:** `AwaitSignals` only does OR on signal names. Can't simultaneously wait for a signal AND a child workflow completion AND a timer.

**Solution:** Add a `Selector` type:

```go
sel := durable.NewSelector(h)
var signalPayload string
var childResult string
var timerFired bool

sel.AddSignal("driver_accepted", &signalPayload)
sel.AddChildWorkflow(runID, &childResult)
sel.AddTimer(5*time.Minute, &timerFired)

winner := sel.Select() // blocks until one future resolves
switch winner {
case "driver_accepted":
    // handle signal
case runID:
    // handle child result
case durable.SelectorTimer:
    // handle timeout
}
```

**Files:** `durable/runtime.go` (new `Selector` type + methods)

---

## Phase 5: Infrastructure

These items require host-side (runtime/PostgreSQL) changes and are described here for completeness but are out of scope for the pure SDK work:

### 5.1 Timer interrupts
Allow `DurableSleep` to be interrupted by a signal, eliminating the polling pattern entirely.

### 5.2 Visibility server
A web dashboard for inspecting workflow state, event history, and stack traces. Reads from the PostgreSQL event store.

### 5.3 Schedule/cron support
Built at the infrastructure layer: a scheduler that starts workflow instances on a cron expression.

---

## Implementation Order

| #  | Item | Effort | Impact | Prereqs |
|----|------|--------|--------|---------|
| 1  | Nil compensation guard | S | Critical bug | none |
| 2  | JSON injection fix | S | Security | none |
| 3  | `DurableCallTyped` (no code gen) | M | High | 2 |
| 4  | PlaceLargeOrder fix | S | Demo credibility | none |
| 5  | Retry policies | M | High | none |
| 6  | CallError structured types | M | Medium | none |
| 7  | PollUntil → AwaitSignals in example | S | Medium | none |
| 8  | Versioning example + docs | S | Medium | none |
| 9  | Workflow testing framework | L | High | none |
| 10 | Service client code generator | L | High | 3 |
| 11 | Heartbeat support | M | Medium | host change |
| 12 | Selector | M | Medium | host change |
| 13 | Timer interrupts | L | Medium | host change |
| 14 | Visibility dashboard | XL | High | host change |

**First session deliverables** (items 1-8): Bug fixes, typed calls, retry policies, versioning docs, structured errors. The fooddash example becomes a credible reference after these changes.

**Second session** (items 9-10): Testing framework and code generator. These unlock the developer experience.

**Post-SDK** (items 11-14): Host-side changes needed for production deployment.
