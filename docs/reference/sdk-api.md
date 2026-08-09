# cleat SDK API Reference

Package `cleat` defines the durable SDK -- the only import a workflow author
needs. All external interactions go through the `HostCalls` interface, which
enables deterministic replay.

    import "github.com/cleat-team/cleat/cleat"

Workflow entry points receive `h cleat.HostCalls` as their first parameter.
Helper functions in the durable closure receive it through the transformer's
auto-threading pass.

```go
func PlaceOrder(h cleat.HostCalls, userID string, cart []CartItem) (string, error) {
    result, err := h.DurableCall("inventory", "CheckAvailability", userID)
    // ...
}
```

---

## HostCalls Interface

`HostCalls` is composed from capability-grouped sub-interfaces:

- `Caller` -- durable service calls, plugins, HTTP, side effects
- `Timer` -- deterministic time and sleep
- `Signaler` -- signal communication between workflows
- `Lifecycle` -- versioning, child workflows, cancellation, logging, defer
- `Promises` -- durable promise operations
- `StateManager` -- durable key-value state
- `QueryHandlers` -- workflow handler registration
- `CronScheduler` -- durable cron schedule operations
- `Scoper` -- virtual object instance scoping
- `UUIDGenerator` -- deterministic UUID generation
- `Locker` -- distributed concurrency lock operations
- `RandomSource` -- deterministic random number generation

---

## Caller -- Durable RPC

```go
DurableCall(service, operation, requestJSON string) (responseJSON string, err error)
```

Makes or replays a durable API call. On first execution, invokes the external
service and records the result. On replay, returns the cached result without
re-executing.

```go
items, _ := h.DurableCall("inventory", "CheckAvailability", `{"sku":"abc"}`)
```

---

```go
DurableCallTyped(service, operation string, request, result interface{}) error
```

Marshals `request` to JSON, makes a durable API call, and unmarshals the
response into `result`. Eliminates manual JSON handling.

```go
var items []Item
h.DurableCallTyped("inventory", "CheckAvailability", req, &items)
```

---

```go
DurableCallWithOptions(opts CallOptions, service, operation, requestJSON string) (string, error)
```

Makes a durable API call with call-level options such as retry policy.

```go
opts := cleat.CallOptions{RetryPolicy: &cleat.RetryPolicy{MaxAttempts: 5}}
result, err := h.DurableCallWithOptions(opts, "payments", "Charge", body)
```

---

```go
DurableCallWithHeartbeat(service, operation, requestJSON string,
    heartbeatInterval time.Duration,
    onProgress func(progressJSON string)) (string, error)
```

Long-running durable call with periodic progress updates from the host.

---

```go
DurableCallJSON(service, operation, requestJSON string, result interface{}) error
DurableCallJSONWithOptions(opts CallOptions, service, operation, requestJSON string, result interface{}) error
DurableCallTypedWithOptions(opts CallOptions, service, operation string, request, result interface{}) error
DurableCallTypedWithHeartbeat(service, operation string, request, result interface{},
    heartbeatInterval time.Duration, onProgress func(progressJSON string)) error
```

Variants combining typed, JSON, options, and heartbeat features.

---

```go
PluginCall(pluginName, functionName, inputJSON string) (string, error)
```

Invokes a named function on a registered plugin.

```go
result, _ := h.PluginCall("llm", "Generate", `{"prompt":"..."}`)
```

---

```go
PluginCallStreaming(pluginName, functionName, inputJSON string) (<-chan StreamEvent, error)
```

Calls a plugin function that returns a stream of events. Returns a channel
that receives `StreamEvent` chunks.

---

```go
DurableFetch(url, method string, headers map[string]string, body string) (responseJSON string, statusCode int, err error)
```

Makes an HTTP request as a durable operation. Delegates to
`DurableCall("http", "fetch", ...)`.

```go
resp, status, _ := h.DurableFetch("https://api.example.com/orders", "POST", nil, body)
```

---

```go
DurableFetchJSON(url, method string, headers map[string]string, body string, result interface{}) error
FetchGet(url string) (responseJSON string, statusCode int, err error)
FetchGetJSON(url string, result interface{}) error
```

Shorthand variants for HTTP fetch operations.

---

```go
DurableSend(service, operation, requestJSON string) error
```

Fire-and-forget one-way message to a service (not recorded as a blocking
operation).

---

```go
ScheduleInvoke(service, operation, requestJSON string, delayMs int64) error
```

Schedules a one-way message to be delivered after a delay.

---

```go
SideEffect(fn func() (string, error)) (string, error)
```

Executes a non-deterministic function on first execution, records its result
in event history, and returns the cached result on replay. On replay, `fn` is
NOT called.

```go
orderID, _ := h.SideEffect(func() (string, error) {
    return uuid.NewRandom().String(), nil
})
```

---

## Timer -- Durable Time

```go
DurableSleep(d time.Duration)
```

Suspends the workflow for the given duration. On replay, the sleep is
skipped and `Now()` reflects the time after the sleep completed.

```go
h.DurableSleep(5 * time.Second)
```

---

```go
Now() time.Time
```

Returns the deterministic current time. On first execution, returns wall-clock
timestamps. On replay, returns the recorded timestamps from the event history,
so the workflow sees the same time values every replay.

```go
deadline := h.Now().Add(30 * time.Second)
```

---

```go
DurableSleepMs(ms int64)
NowMs() int64
```

Millisecond variants. Prefer `DurableSleep(time.Duration)` and `Now()` for
readability.

---

## Signaler -- Signals

```go
AwaitSignals(signalNames []string, timeout time.Duration) SignalResult
```

Blocks until one of the named signals arrives or the timeout expires.

```go
result := h.AwaitSignals([]string{"payment_received", "cancelled"}, 1*time.Hour)
if result.TimedOut {
    // handle timeout
}
```

---

```go
DurableAwaitSignals(signalNames []string, timeoutMs int64) (signalName, payload string, timedOut bool, err error)
```

Low-level signal wait. Prefer `AwaitSignals`.

---

```go
SendSignalAndWait(targetRunID, signalName, payload string, timeout time.Duration) (response string, err error)
```

Sends a signal to another workflow with an embedded correlation ID and waits
for a reply.

---

```go
ReplyToSignal(correlationID, response string) error
```

Sends a response back to the sender of a signal identified by a correlation
ID. Used inside a signal handler.

---

```go
AwaitSignalsWithQuorum(signalNames []string, minCount int, maxRejections int, timeout time.Duration) ([]SignalResult, error)
```

Waits for at least `minCount` signals from the named set. When
`maxRejections >= 0`, signals with `"rejected":true` in their JSON payload
count toward the rejection limit.

---

```go
SignalWorkflow(targetRunID, signalName, payload string) error
```

Fire-and-forget signal to another workflow from within a workflow.

```go
h.SignalWorkflow("run-abc-123", "notify", `{"msg":"done"}`)
```

---

```go
PollSignal(signalName string) (payload string, found bool, err error)
```

Non-blocking signal check.

---

## Lifecycle -- Workflow Management

```go
ContinueAsNew(newInputJSON string) error
```

Restarts the workflow with fresh event history, passing the current state
as input.

```go
h.ContinueAsNew(`{"page":2}`)
```

---

```go
ChildWorkflow(name, inputJSON string) (runID string, err error)
```

Starts a child workflow that runs with its own event history.

```go
runID, _ := h.ChildWorkflow("send_notification", `{"to":"user@example.com"}`)
```

---

```go
ChildWorkflowWithOptions(name, inputJSON string, opts ChildWorkflowOptions) (runID string, err error)
```

Starts a child workflow with a pinned version or other options.

---

```go
AwaitChild(runID string) (resultJSON string, err error)
```

Waits for a child workflow to complete.

```go
result, _ := h.AwaitChild(runID)
```

---

```go
AwaitAllChildren(runIDs []string) ([]ChildResult, error)
```

Waits for all child workflows concurrently. Results match the input order.

---

```go
ChildWorkflowTyped(name string, request interface{}) (runID string, err error)
AwaitChildTyped(runID string, result interface{}) error
```

Typed variants that marshal/unmarshal request and result automatically.

---

```go
RunDetached(fn func(h HostCalls) error) error
```

Runs `fn` with a fresh `HostCalls` that ignores cancellation. `fn` is
executed on every replay (not replayed from cache).

---

```go
WorkflowID() string
RunID() string
```

Returns the current workflow and run identifiers.

---

```go
Version() int
MinVersion() int
```

Returns the current and minimum compatible workflow versions for schema
evolution detection.

---

```go
DurableDefer(description string) (deferID string, err error)
```

Registers a deferred cleanup action (LIFO order). The deferred action runs
when the workflow exits, even on error.

```go
h.DurableDefer("release inventory reservation")
```

---

```go
DurableDeferFunc(fn func()) (deferID string, err error)
```

Like `DurableDefer` but accepts a function closure instead of a description.

```go
h.DurableDeferFunc(func() {
    h.DurableCall("inventory", "ReleaseReservation", "order-123")
})
```

---

```go
PollCancellation() (cancelled bool, reason string)
```

Checks whether a cancellation has been requested. Workflows should poll this
at their own cancellation points.

```go
if cancelled, reason := h.PollCancellation(); cancelled {
    return fmt.Errorf("cancelled: %s", reason)
}
```

---

```go
RegisterUpdateHandler(name string,
    handler func(payloadJSON string) (resultJSON string, err error),
    validator func(payloadJSON string) error)
```

Registers a handler for the named workflow update. Called during workflow
init, before durable operations. The validator runs first (read-only).

---

```go
RegisterQueryHandler(name string, handler func(payloadJSON string) (resultJSON string, err error))
```

Registers a read-only query handler that can be invoked on demand by external
callers without journaling.

---

```go
DurableLog(message string)
LogKV(message string, kvs ...interface{})
```

Emits structured log messages recorded in the event history.

```go
h.LogKV("payment processed", "amount", 5000, "currency", "USD")
```

---

## StateManager -- Key-Value State

```go
SetQueryState(key, value string)
```

Sets workflow state that is visible via the REST API (`GET /api/workflows/:id?key=X`).

```go
h.SetQueryState("order_status", "shipped")
```

---

```go
SetState(key string, value interface{})
GetState(key string, result interface{}) error
DeleteState(key string)
HasState(key string) bool
IncrState(key string, delta int64) int64
ListState(prefix string) []string
```

Full key-value state management scoped to the current workflow.

---

## Promises -- External Interaction

```go
CreatePromise(name string) (promiseID string, err error)
AwaitPromise(promiseID string, timeout time.Duration) (result string, timedOut bool, err error)
```

Creates a durable promise that can be resolved or rejected by an external
caller via the REST API (`POST /api/workflows/:id/promises/:promiseId/resolve`).

```go
promiseID, _ := h.CreatePromise("manager_approval")
// external system resolves via API
result, timedOut, _ := h.AwaitPromise(promiseID, 30*time.Minute)
```

Typed convenience:

```go
promise, _ := cleat.NewPromiseTyped[ApprovalResult](h, "manager_approval")
result, timedOut, err := promise.Await(30 * time.Minute)
```

---

## CronScheduler -- Recurring Triggers

```go
ScheduleCron(workflowName, cronExpr, timezone, inputJSON string) (scheduleID string, err error)
DeleteCron(scheduleID string) error
ListCrons() (string, error)
```

Creates, deletes, and lists recurring workflow triggers from cron
expressions.

```go
sid, _ := h.ScheduleCron("daily_report", "0 6 * * *", "America/New_York", `{}`)
```

`cronExpr` is a standard 5-field expression. Day-of-month and day-of-week are
**OR**ed when both are restricted, as POSIX cron specifies: `0 0 13 * 5` fires
on the 13th *and* on every Friday, not only on Friday the 13th. `timezone` is an
IANA name; `""` means UTC. Both are validated when the schedule is created —
a schedule the scheduler could not act on is refused at the call, because a
background loop has nobody to report one to later.

`ListCrons` returns a JSON array, ordered by schedule ID:

```json
[{"schedule_id":"cron-…","workflow_name":"daily_report","cron_expr":"0 6 * * *",
  "timezone":"America/New_York","input":"{}","enabled":true}]
```

**Delivery is at-least-once.** A firing may be delivered more than once; it will
not be silently skipped. If duplicates matter to your workflow, make it
idempotent — that is the caller's job, and it is the only guarantee worth
offering: at-most-once is close to useless for scheduled work, and exactly-once
is not attainable across a process boundary.

`ScheduleCron` itself is safe to retry. Schedule IDs are derived from the
calling workflow and step rather than generated randomly, so a workflow that
creates a schedule and crashes before its event is journaled will address the
same schedule when it replays, instead of leaving an unreferenced one firing
forever. `DeleteCron` on an already-deleted schedule is likewise a success, not
an error.

**Availability.** Go and AssemblyScript. **Not available to Python workflows** —
`python-sdk/wit/cleat.wit` declares no interface for these calls, so
componentize-py generates no binding and they raise. The Rust and Java SDKs
declare no cron surface at all. See `tiers.yaml`, `workflow-callable-cron`.

The embedded and localdev runners refuse these calls: neither has a schedule
store, so nothing there could ever fire a schedule.

---

## Scoper -- Virtual Object Scoping

```go
SetScope(objectType, instanceKey string) (previousScope string)
GetScope() (objectType, instanceKey string)
ClearScope() (previousScope string)
```

Manages virtual object instance scoping for concurrency control.

---

## UUIDGenerator -- Deterministic UUIDs

```go
UUID(seed string) string
NewUUID() string
NewUUIDv7() string
```

Generates deterministic UUIDs. `UUID(seed)` produces the same value on every
replay for the same seed. `NewUUID` and `NewUUIDv7` produce time-ordered
UUIDs.

---

## Locker -- Distributed Locks

```go
AcquireLock(key string, ttl time.Duration) (acquired bool, err error)
ReleaseLock(key string) error
AcquireLockMs(key string, ttlMs int64) (acquired bool, err error)
```

Distributed concurrency lock operations backed by the database.

```go
ok, _ := h.AcquireLock("order-123", 30*time.Second)
if ok {
    defer h.ReleaseLock("order-123")
}
```

---

```go
AwaitCondition(predicate func() bool, pollInterval, timeout time.Duration) (met bool)
```

Polls a predicate function until it returns true or the timeout expires.

---

## RandomSource -- Deterministic Randomness

```go
Random() int64
```

Returns a deterministic random value. The same sequence is produced on every
replay.
