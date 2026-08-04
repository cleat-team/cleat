# DurableCall: At-Least-Once Execution and the Ambiguity Problem

## 1. The At-Least-Once Contract

`DurableCall` (the host function `cleat_call` / `cleat_call_retry` / `cleat_call_heartbeat`) provides **at-least-once** execution semantics. The external service call is made at least once. It may be made more than once if the worker crashes, restarts, and replays history past the point of the original call.

**Why not exactly-once?**

Exactly-once execution is impossible in general distributed systems. The Two Generals' Problem proves that two parties cannot guarantee agreement on an outcome over an unreliable channel. In practical terms:

- The worker sends an HTTP request to an external service.
- The external service processes the request and returns a response.
- The worker crashes before persisting the response to the event history.
- On restart, replay progresses to the same step. There is no recorded outcome for this call, so the worker re-executes it.
- The external service receives the same request a second time.

The worker cannot distinguish "the external service never received the first request" from "the external service processed the first request but the response was lost before being persisted." Both look identical from the worker's perspective: no recorded event for this step.

Therefore, `DurableCall` guarantees at-least-once delivery. The application must be designed to tolerate duplicate execution.

---

## 2. The Crash Window

The standard `DurableCall` flow (without write-ahead intent logging) has the following crash window:

```
freshCall:
  step 1:  service.Call(request) ──► external service processes request
  step 2:  recordEvent(response)     ◄── response received
                                     ▲
                                     └── CRASH WINDOW: worker crashes here
```

If the worker crashes after the external call returns but before the event is persisted to `event_history`, the response is lost. On replay:

```
replayCall:
  step 1:  no history for step 1 ──► exitReplay() ──► freshCall()
  step 2:  service.Call(request)     ◄── external service receives DUPLICATE request
```

The call is re-executed. The external service sees the same request twice.

### With write-ahead intent logging

To narrow the window, `flushCallIntent` inserts a pending event **before** dispatching the external call:

```
intent-based flow:
  step 1:  flushCallIntent(step=1)   ◄── event_history: step 1 = pendingSentinel
  step 2:  service.Call(request)     ◄── external service processes request
  step 3:  completeCallEvent(resp)   ◄── event_history: step 1 = response
                                      ▲
                                      └── CRASH WINDOW (narrower: only between step 2 and step 3)
```

If the worker crashes between step 2 and step 3, the event history contains:

| step | service | request | error             |
|------|---------|---------|-------------------|
| 1    | my-svc  | {...}   | `__CLEAT_PENDING_INTENT__` |

On replay, `replayCall` detects the `pendingSentinel` in the error column and returns `ErrAmbiguous` instead of silently re-executing. The workflow author is notified that the outcome is unknown and must check the external service.

**The intent-logging write side is not implemented, and the code sketched for it should not be used.** The replay infrastructure (detection of `pendingSentinel` and return of `ErrAmbiguous`) is in place and correct, but nothing writes a `pendingSentinel`, so in a real crash the detector has nothing to find. `flushCallIntent` / `completeCallEvent` in `engine/flush.go` are not a working write side: wiring them in as they stand would leave every durable call permanently ambiguous. See [`durable-call-intent-design.md`](durable-call-intent-design.md) for why, and for the replacement design.

Until that lands, **the contract is exactly what §1 says: at-least-once, with duplicates on crash that are silent.** Design workflows accordingly — the cheapest mitigation available today is to make external operations idempotent yourself, for example by passing your own idempotency key derived from a workflow-stable value.

---

## 3. Ambiguity Detection

### `pendingSentinel`

The constant `pendingSentinel = "__CLEAT_PENDING_INTENT__"` is stored in the `error` column of `event_history` to mark a `DurableCall` whose external call was dispatched but whose outcome was not yet persisted.

Defined in `internal/host/engine.go`:

```go
const pendingSentinel = "__CLEAT_PENDING_INTENT__"
```

### `ErrAmbiguous`

When replay encounters a step with `pendingSentinel`, it constructs an error message and returns it to the WASM module. The error is classified as `ErrAmbiguous` in the host's error taxonomy:

```go
ErrAmbiguous  ErrorCode = 5  // call outcome unknown after crash
```

The error message returned to workflow code looks like:

```
[AMBIGUOUS] call outcome unknown at step N: the external call to
service.operation was dispatched but the response was not recorded
before a crash. Check the external service before retrying.
```

### Where ambiguity detection fires

Both `replayCall` (for standard `cleat_call`) and the `cleat_call_retry` / `cleat_call_heartbeat` replay paths check for `pendingSentinel`:

```go
// From internal/host/engine.go, replayCall:
if rec.Err == pendingSentinel {
    ambiguousErr := fmt.Sprintf(
        "[AMBIGUOUS] call outcome unknown at step %d: ...",
        rec.Step, rec.Service, rec.Op)
    written, _ := s.writeResult(ctx, m, responsePtr, ambiguousErr, responseMaxLen)
    return packDurableCallResult(int(written), 1, 1)
}
```

---

## 4. Application Responsibilities

Developers writing workflows that use `DurableCall` **must**:

### 4.1 Design external services to be idempotent

The external service called by `DurableCall` must tolerate receiving the same request multiple times. This is the primary requirement. If the external service cannot naturally be made idempotent, use an **idempotency key** (see section 5).

### 4.2 Handle `ErrAmbiguous`

When a workflow receives an `ErrAmbiguous` error, it means the call *may* have succeeded, and the worker cannot determine the outcome. The workflow should:

1. Check the external service's state to determine whether the operation completed.
2. If completed: proceed with the known outcome (e.g., look up the result from the external service).
3. If not completed: retry the call.

### 4.3 Not assume exactly-once

Never assume a `DurableCall` happens exactly once. Every call is at-least-once by design. Code like the following is dangerous:

```go
// DANGEROUS: assumes exactly-once
h.DurableCall("payment", "charge", `{"amount": 100}`)
h.DurableCall("inventory", "deduct", `{"sku": "ABC", "qty": 1}`)
```

If the worker crashes after the payment charge succeeds but before the inventory deduction is recorded, replay re-executes the inventory deduction -- which is fine if it's idempotent. But it also re-executes the payment charge -- which is NOT fine unless the payment service is idempotent.

---

## 5. Idempotency Patterns

### 5.1 Idempotency keys on external API calls

The most reliable pattern is to have the external service accept an idempotency key and guarantee that the same key produces the same result. Use a deterministic key derived from the workflow run ID and step number:

```go
func ShipOrder(h cleat.HostCalls, orderID string) error {
    // Derive idempotency key from run ID + operation name.
    // This is deterministic across replays: the same step always
    // produces the same key.
    idempotencyKey := h.RunID() + "/ship/" + orderID

    resp, err := h.DurableCall("shipping", "create_shipment", fmt.Sprintf(`{
        "order_id": "%s",
        "idempotency_key": "%s"
    }`, orderID, idempotencyKey))

    if err != nil {
        // Check if this is an ambiguous outcome error.
        // The actual pattern for detecting ErrAmbiguous depends on the
        // SDK version; the error message contains "[AMBIGUOUS]" when
        // the outcome is unknown.
        return fmt.Errorf("ship order: %w", err)
    }

    _ = resp
    return nil
}
```

The external shipping service uses the `idempotency_key` to deduplicate: if it has already processed this key, it returns the cached result instead of creating a duplicate shipment.

### 5.2 Handling `ErrAmbiguous` in workflow code

When the worker returns `ErrAmbiguous`, the workflow should query the external service to determine the actual outcome:

```go
func ProcessPayment(h cleat.HostCalls, paymentID string) error {
    idempotencyKey := h.RunID() + "/payment/" + paymentID

    resp, err := h.DurableCall("payment", "charge", fmt.Sprintf(`{
        "payment_id": "%s",
        "idempotency_key": "%s"
    }`, paymentID, idempotencyKey))

    if err != nil {
        // Check if the error is ambiguous (outcome unknown).
        // The "[AMBIGUOUS]" prefix is how the host signals this state.
        if strings.Contains(err.Error(), "[AMBIGUOUS]") {
            // The payment MAY have been processed. Check with the
            // payment service before deciding what to do.
            statusResp, checkErr := h.DurableCall("payment", "get_status",
                fmt.Sprintf(`{"payment_id": "%s"}`, paymentID))
            if checkErr != nil {
                return fmt.Errorf("cannot determine payment status: %w", checkErr)
            }

            var status struct {
                Completed bool   `json:"completed"`
                Result    string `json:"result"`
            }
            if err := json.Unmarshal([]byte(statusResp), &status); err != nil {
                return fmt.Errorf("parse payment status: %w", err)
            }

            if status.Completed {
                // Payment was already processed. Proceed with known result.
                resp = status.Result
            } else {
                // Payment was NOT processed. Retry the charge.
                return fmt.Errorf("payment %s: retry needed after ambiguous crash", paymentID)
            }
        } else {
            return fmt.Errorf("payment failed: %w", err)
        }
    }

    _ = resp
    return nil
}
```

### 5.3 Checking via workflow-level API

Application code can also use the `backendkit` client to inspect workflow history from outside the workflow, checking whether a specific call event completed:

```go
client := backendkit.New("http://worker:8080")
detail, err := client.GetWorkflow(ctx, workflowID)
if err != nil {
    // handle error
}
for _, event := range detail.History {
    if event.Step == targetStep && event.Err != "" {
        // step errored; may need manual resolution
    }
}
```

### 5.4 No SDK-level `ErrAmbiguous` type yet

The current Go SDK (`cleat/runtime.go`) does not expose a dedicated `ErrAmbiguous` type in the `Caller` interface. The ambiguous-outcome signal is carried in the error message string prefixed with `[AMBIGUOUS]`. A typed error or sentinel will be added in a future SDK version.

---

## 6. Comparison with Competitors

All durable execution frameworks share the same fundamental constraint: exactly-once is impossible in distributed systems. Each framework makes different trade-offs in how it handles the crash window.

### Temporal

Temporal activities are at-least-once by default. The SDK replays the workflow code, and when replay reaches a previously recorded `ActivityTaskScheduled` event, the cached result is returned. The crash window exists between the activity completing and the result being recorded in the history.

Temporal does not expose an ambiguous-outcome signal to workflow code. If a worker crashes after an activity completes but before the result is recorded, the activity times out (via `ScheduleToStartTimeout` or `StartToCloseTimeout`) and is retried. The workflow never learns that the activity *may* have completed -- it only sees a timeout or retry.

Both Cleat and Temporal require external services to be idempotent. The key difference:

| Aspect | Temporal | Cleat |
|--------|----------|-------|
| Crash window signal | Activity timeout / retry | `ErrAmbiguous` (via `pendingSentinel`) |
| Workflow knows call may have succeeded? | No (sees timeout) | Yes (receives `[AMBIGUOUS]`) |
| Idempotency requirement | Yes | Yes |
| Write-ahead intent log | No | Planned (`flushCallIntent`/`completeCallEvent`) |

### DBOS

DBOS uses the database transaction as the unit of durability. Workflow state and side-effect results are committed atomically in the same database transaction. This narrows the crash window considerably: the external call's result is stored in the same transaction that advances the workflow.

However, DBOS workflows are at-least-once too. Between `@Step`-annotated methods, ordinary code runs without durability. If a crash occurs between steps, the previous step is re-executed. DBOS does not provide an ambiguous-outcome signal.

| Aspect | DBOS | Cleat |
|--------|------|-------|
| Unit of durability | DB transaction + step boundary | Event history |
| Between-step crash | Re-runs previous step | Replay from last event |
| Ambiguity signal | None | `ErrAmbiguous` (via `pendingSentinel`) |

### AWS Step Functions

Step Functions uses task tokens for callbacks. Each task emits a token that the external service must return with its result. If the Lambda or activity execution completes but the response is lost, the task times out and can be retried.

Step Functions tasks are at-least-once. The callback/token pattern provides a form of ambiguity detection: if the token is not returned, the task status remains "in progress" and eventually times out. But the workflow code itself does not receive an ambiguous-outcome signal -- it only sees success, failure, or timeout.

| Aspect | AWS Step Functions | Cleat |
|--------|--------------------|-------|
| Interaction model | Task tokens (callback) | Synchronous call |
| Crash window signal | Timeout | `ErrAmbiguous` |
| Idempotency requirement | Yes | Yes |

### Summary

| | Cleat | Temporal | DBOS | AWS Step Functions |
|---|---|---|---|---|
| Execution guarantee | At-least-once | At-least-once | At-least-once | At-least-once |
| Exactly-once claimed? | No | No | No (marketing claims refer to DB tx state) | No |
| Ambiguous outcome signal | Yes (`ErrAmbiguous`) | No (timeout/retry) | No | No (timeout) |
| Idempotency required? | Yes | Yes | Yes | Yes |
| Write-ahead intent log | Planned | No | Via DB tx | Via callback token |
| Replay model | Deterministic | Deterministic | DB replay | State machine |

No framework provides exactly-once execution for external side effects. The best any framework can do is:
1. Minimize the crash window.
2. Signal ambiguity when it occurs.
3. Provide tools (idempotency keys, idempotent service design) to make at-least-once safe.

Cleat addresses #2 with the `ErrAmbiguous` mechanism and `pendingSentinel` intent logging. Frameworks that claim exactly-once are either limiting the scope to database state (which can be transactional) or making assumptions that break in real distributed deployments.
