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

For an operation declared `WriteAheadIntent` (worker flag `--write-ahead-intent-ops
service.operation,...`), the engine writes a pending row **before** dispatching the external call:

```
intent-based flow:
  step 1:  WriteCallIntent(step=1)    ◄── event_history: step 1 pending (intent_at set)
  step 2:  service.Call(request)      ◄── external service processes request
  step 3:  CompleteCallIntent(resp)   ◄── event_history: step 1 = response (intent_at cleared)
                                       ▲
                                       └── CRASH WINDOW (narrower: only between step 2 and step 3)
```

If the worker crashes between step 2 and step 3, the row is left pending: `intent_at IS NOT NULL
AND checksum IS NULL` is what "pending" means on disk, set by `WriteCallIntent` and cleared by
`CompleteCallIntent` in the same statement that writes the outcome, so the two columns cannot
disagree with it.

On replay, `LoadEventHistory` surfaces that as `EventRecord.Pending`, and `isPendingIntent`
returns `ErrAmbiguous` instead of silently re-executing. The workflow author is notified that the
outcome is unknown and must check the external service.

**Write-ahead intent is implemented and live, but it is opt-in per operation and off by default.**
`engine/callintent.go` (engine half) and `engine/store_intent.go` (store half — Postgres, MySQL and
SQL Server all implement `WriteCallIntent`/`CompleteCallIntent`) are the write side;
`freshCallWithIntent` is where a declared call routes instead of the plain dispatch-then-record
path above. See [`durable-call-intent-design.md`](durable-call-intent-design.md) for the tiers
this sits inside and the phasing history — idempotency keys (§5.1 below) remain the only mechanism
for an operation that is not declared.

**An earlier version of this write side was deleted rather than wired in** (a sentinel string in
`event_history.error`, written by functions no longer in the tree) because every completion path's
upsert guarded on `error IS NULL`, so a sentinel row could never be completed and stayed pending
forever. The current design keeps `error` meaning only "the call failed" and tracks pending state
in its own columns instead, which is why it is a different shape from what this section described
before, not just a renamed one.

For any operation **not** declared `WriteAheadIntent` — which is every operation, unless an
operator names it — **the contract is exactly what §1 says: at-least-once, with duplicates on
crash that are silent.** Design workflows accordingly — the cheapest mitigation that needs no
worker configuration is to make external operations idempotent yourself, for example by passing
your own idempotency key derived from a workflow-stable value.

---

## 3. Ambiguity Detection

### `EventRecord.Pending`

A `DurableCall` whose external call was dispatched but whose outcome was not yet persisted is
marked by `intent_at IS NOT NULL AND checksum IS NULL` on its `event_history` row — not a sentinel
value in any column. `LoadEventHistory` surfaces this as the `Pending` field on the `EventRecord`
it returns:

```go
// engine/types.go
Pending bool `json:"-"`
```

`json:"-"`: this is server-side replay state, not part of the shape a client reads back through
`GetWorkflow` (§5.3 below reads `event.Err`, which is what a client actually has).

### `ErrAmbiguous`

When replay encounters a step with `Pending` set, it constructs an error message and returns it to the WASM module. The error is classified as `ErrAmbiguous` in the host's error taxonomy:

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

Both the standard `cleat_call` replay path (`engine/durablecalls.go`) and the `cleat_call_retry` / `cleat_call_heartbeat` replay path (`engine/heartbeats.go`) check `isPendingIntent()`, the method behind the `Pending` field above:

```go
// engine/durablecalls.go
if rec.isPendingIntent() {
    s.recordAmbiguity(rec) // structured, for an operator query -- not just the message text
    ambiguousErr := fmt.Sprintf(
        "[AMBIGUOUS] call outcome unknown at step %d: the external call to %s.%s "+
            "was dispatched but the response was not recorded before a crash. "+
            "Check the external service before retrying.",
        rec.Step, rec.Service, rec.Op)
    written, _ := s.writeResult(ctx, m, responsePtr, ambiguousErr, responseMaxLen)
    return packDurableCallResult(int(written), callErrorUnknown, 1)
}
```

**Before that report happens, an optional resolver gets a chance to make it a non-event.**
`WithAmbiguityResolver` (an `EngineOption`) lets an embedder supply a lookup — keyed on the same
per-step idempotency key the pattern in §5.1 uses — that checks the external service directly. If
it answers, the outcome is recorded and replay carries on as though the call had returned
normally: the crash lost the answer, not the effect, and the workflow never sees `[AMBIGUOUS]` at
all. **`cleat-worker` does not configure one** — `grep -rn WithAmbiguityResolver
cmd/cleat-worker/` finds nothing — so on the shipped worker binary every ambiguity reaches the
report above; this is an extension point for an embedder, and as of this writing nothing in the
tree, embedded or otherwise, calls it (cleat#1871).

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

### 5.3 Checking via workflow-level API, and resolving it from outside the workflow

Application code can also use the `backendkit` client to inspect workflow history from outside the workflow, checking whether a specific call event completed:

```go
client := backendkit.New("http://worker:8080")
detail, err := client.GetWorkflow(ctx, workflowID)
if err != nil {
    // handle error
}
for _, event := range detail.History {
    if event.Step == targetStep && event.Err != "" {
        // ambiguous -- see below for how an operator settles this from outside the workflow
    }
}
```

Once the external service's true state is known, an operator settles the step directly rather
than waiting for the workflow itself to retry — `POST
/api/admin/instances/{id}/steps/{step}/resolve` (`engine.ResolveStep`), with header
`X-Confirm: resolve-step` and a body naming the outcome to record:

```
POST /api/admin/instances/wf-123/steps/4/resolve
X-Confirm: resolve-step
{"response": "<the outcome confirmed against the external service>"}
```

This writes the response replay will treat as the call's real result for the rest of the
workflow's life, so it is a claim that the operator has actually checked — not a guess. See
`cmd/cleat-worker/api_admin.go`'s handler doc comment for why the header is required.

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
| Crash window signal | Activity timeout / retry | `ErrAmbiguous` (§3) |
| Workflow knows call may have succeeded? | No (sees timeout) | Yes (receives `[AMBIGUOUS]`) |
| Idempotency requirement | Yes | Yes |
| Write-ahead intent log | No | Yes, opt-in per operation (§2) |

### DBOS

DBOS uses the database transaction as the unit of durability. Workflow state and side-effect results are committed atomically in the same database transaction. This narrows the crash window considerably: the external call's result is stored in the same transaction that advances the workflow.

However, DBOS workflows are at-least-once too. Between `@Step`-annotated methods, ordinary code runs without durability. If a crash occurs between steps, the previous step is re-executed. DBOS does not provide an ambiguous-outcome signal.

| Aspect | DBOS | Cleat |
|--------|------|-------|
| Unit of durability | DB transaction + step boundary | Event history |
| Between-step crash | Re-runs previous step | Replay from last event |
| Ambiguity signal | None | `ErrAmbiguous` (§3) |

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
| Write-ahead intent log | Yes, opt-in per operation | No | Via DB tx | Via callback token |
| Replay model | Deterministic | Deterministic | DB replay | State machine |

No framework provides exactly-once execution for external side effects. The best any framework can do is:
1. Minimize the crash window.
2. Signal ambiguity when it occurs.
3. Provide tools (idempotency keys, idempotent service design) to make at-least-once safe.

Cleat addresses #2 with the `ErrAmbiguous` mechanism and write-ahead call intent (§2). Frameworks that claim exactly-once are either limiting the scope to database state (which can be transactional) or making assumptions that break in real distributed deployments.
