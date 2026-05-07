# Cleat Go Widget Store

Port of the DBOS widget-store Go application to the Cleat Go durable execution SDK.

Source: `dbos-demo-apps/golang/widget-store/`
Cleat SDK: `github.com/rcownie/durable/durable`

## Migration Map: DBOS to Cleat API

| DBOS API | Cleat Equivalent | Notes |
|----------|-----------------|-------|
| `dbos.RunAsStep(ctx, fn)` | `h.DurableCall(service, op, json)` / `h.DurableCallTyped(service, op, req, result)` | Cleat abstracts side effects as durable service calls rather than wrapping closures |
| `dbos.Recv[string](ctx, topic, timeout)` | `h.AwaitSignals(names, timeout)` | Returns a `SignalResult` struct (Name, Payload, TimedOut, Err) |
| `dbos.Send(ctx, destID, msg, topic)` | `h.SignalWorkflow(destID, topic, msg)` | Fire-and-forget signal delivery between workflows |
| `dbos.SetEvent(ctx, key, msg)` | `h.SetQueryState(key, value)` | Publish key-value state that external handlers can read |
| `dbos.GetEvent[string](ctx, wfID, key, timeout)` | Runtime query API | External retrieval of workflow state requires the Cleat runtime API |
| `dbos.Sleep(ctx, duration)` | `h.DurableSleep(duration)` | Signals are delivered during sleep; uses deterministic clock |
| `dbos.RunWorkflow(ctx, fn, input)` | `h.ChildWorkflow(name, inputJSON)` | Child workflow; use `h.AwaitChild(runID)` to wait for completion |
| `ctx.GetWorkflowID()` | `h.WorkflowID()` | Returns string (no error); returns empty string if unset |
| `dbos.RunAsStep` error propagation | `DurableCallTyped` error propagation | Errors propagate through the return chain |
| N/A | `durable.NewSaga()` with `AddStep()` / `Run()` | Structured compensation pattern; only compensates on `TerminalError` |
| N/A | `h.CreatePromise()` / `h.AwaitPromise()` | External resolution via REST API |
| `dbos.RegisterWorkflow(ctx, fn)` | N/A (code-gen) | Cleat uses code transformation to register workflows |
| `dbosContext.Launch()` / `Shutdown()` | N/A (embedded runner) | Cleat provides `durable/embedded` runner for local dev |

## Key Differences

### 1. Side effects are abstracted as DurableCall

DBOS wraps arbitrary Go closures as steps (`dbos.RunAsStep`). Cleat abstracts all side effects behind `DurableCall` (service, operation, request). Database operations are modeled as calls to a "store" service. This allows deterministic replay and testing without a real database.

### 2. WorkflowID() returns string, no error

DBOS's `GetWorkflowID()` returns `(string, error)`. Cleat's `WorkflowID()` returns just a string. Check for empty string instead of error.

### 3. Signal API differences

DBOS uses `Send`/`Recv` with topic-based communication. Cleat uses `SignalWorkflow`/`AwaitSignals` with signal names. The `AwaitSignals` result includes a `TimedOut` boolean instead of returning a timeout error.

### 4. No implicit event channel

DBOS provides `SetEvent`/`GetEvent` for publishing events between workflows and external callers. Cleat uses `SetQueryState` for exposing workflow state, but external retrieval requires the runtime's query API.

### 5. Saga compensation only on TerminalError

The Cleat Saga's `Run()` method compensates completed steps only when a forward step returns a `TerminalError`. Regular errors are returned without compensation. This differs from Temporal's default behavior of always compensating on failure.

### 6. External integration requires the Cleat runtime API

The original app's HTTP handlers start workflows and send signals via the DBOS context. In Cleat, these operations require the host runtime API (e.g., `runtime.StartWorkflow`, `runtime.SignalWorkflow`), which is available when running with the Cleat embedded runner or WASM host.

## Test Patterns

Tests use `durabletest.TestEnv` which provides:
- `OnCall(service, operation, matcher).Return(response, err)` — stub DurableCall responses
- `OnChildWorkflow(name).Return(result, err)` — stub child workflow results
- `Signal(name, payload)` / `AfterSignal(delay, name, payload)` — deliver signals
- `AdvanceTime(duration)` — advance the simulated clock, waking sleeps and timed-out signal waits
- `QueryState(key)` — read values set via `SetQueryState`
- `AssertCalled(t, service, operation)` / `AssertNotCalled(t, service, operation)` — verify call history

Because workflow functions block on `DurableSleep` and `AwaitSignals`, they must run in a separate goroutine:

```go
env := durabletest.NewTestEnv()
env.OnCall("store", "createOrder", nil).ReturnJSON(1, nil)

go func() {
    result, err := checkoutWorkflow(env.H(), "")
    resultCh <- testResult{result, err}
}()

time.Sleep(50 * time.Millisecond)
env.Signal(PAYMENT_STATUS, "paid")
r := <-resultCh
```

## Running the Tests

```bash
cd cleat-go-widget-store
go test -v -count=1 ./...
```
