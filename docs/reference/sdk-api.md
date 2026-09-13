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
- `StateManager` -- `SetQueryState`, for state a caller can read via the REST API
- `UpdateHandlers` -- workflow update-handler registration
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
    heartbeatInterval time.Duration) (string, error)
```

Long-running durable call. The host heartbeats the claim every
`heartbeatInterval` so a call that outlives the ordinary lease is not reaped as
a stale instance.

Took an `onProgress func(progressJSON string)` until cleat#854. It was removed
rather than fixed: the guest is suspended inside the `cleat_call_heartbeat`
import for the whole call, so there is no moment at which the host could run
guest code. The ABI has never carried a progress channel — see `cleat.wit`,
where `durable-call-heartbeat` takes only the four values above.

---

```go
DurableCallJSON(service, operation, requestJSON string, result interface{}) error
DurableCallJSONWithOptions(opts CallOptions, service, operation, requestJSON string, result interface{}) error
DurableCallTypedWithOptions(opts CallOptions, service, operation string, request, result interface{}) error
DurableCallTypedWithHeartbeat(service, operation string, request, result interface{},
    heartbeatInterval time.Duration) error
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

Sends a signal to another workflow and suspends until that workflow replies or
the timeout elapses.

The reply channel is a durable promise: `SendSignalAndWait` creates one, sends
its ID to the target under the reserved envelope key `cleat_reply_to`, and
awaits it. The receiver does not parse that envelope — `AwaitSignals` and
`PollSignals` strip it, so `SignalResult.Payload` is the payload exactly as
sent and `SignalResult.ReplyTo` carries the address to answer at.

Because the address is a promise ID, replying is resolving that promise, and a
reply to an address that matches nothing is an error rather than a silent
no-op. There is no host call behind this: it composes `CreatePromise`,
`SignalWorkflow` and `AwaitPromise`, each separately durable, so a crash
between the steps replays correctly.

Returns an error if nobody replies within `timeout`.

---

```go
ReplyToSignal(correlationID, response string) error
```

Answers a signal sent with `SendSignalAndWait`, waking the sender with
`response`. Pass `SignalResult.ReplyTo` as `correlationID` — it is the reply
promise's ID, so this resolves that promise.

`ReplyTo` is empty for a signal sent with `SignalWorkflow`, which is how a
receiver distinguishes a request that wants an answer from a one-way
notification; replying to an empty or unknown address returns an error rather
than reporting success.

```go
sig := h.AwaitSignals([]string{"approve"}, time.Hour)
if sig.ReplyTo != "" {
    h.ReplyToSignal(sig.ReplyTo, `{"approved":true}`)
}
```

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
RunDetached(name, inputJSON string) error
```

Starts `name` fire-and-forget: it does not become a child of this workflow and
this workflow does not wait for it. Matches `cleat_run_detached` and the same
call in the Rust, Java, AssemblyScript and Python SDKs.

Took a closure until it was changed: `RunDetached(fn func(h HostCalls) error)`.
A closure cannot cross the WASM ABI, so that form worked only under `localdev`
and `cleattest`, which populate the field in-process, and silently did nothing
in every compiled workflow.

---

```go
StartDetached(name, inputJSON string) (runID string, err error)
```

The same work as `RunDetached`, returning the run id of the workflow it started
so the caller has a handle to it — to poll it, signal it, or record it
somewhere durable. `RunDetached` computes the same id and discards it.

Bound in Go, Rust (`start_detached`), Java (`startDetached`) and
AssemblyScript (`startDetached`). **Not in Python**: the component path needs a
WIT function returning `result<string, call-failure>` and a dispatcher to match,
because an out-pointer addresses the guest's linear memory and component
dispatch writes into a host buffer. Tracked in
`sdkUnreachedBaseline` in `tests/plugin-harness/sdk_import_names_test.go`.

`cleat_run_detached` is unchanged and both calls stay registered. A host call's
arity is part of its import type, so widening the existing one would stop every
already-deployed binary instantiating — see ABI.md §2.24a.

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
init, before durable operations. The validator runs first (read-only), so a
request it refuses changes nothing and does no durable work.

An update is a request/reply call into a *running* workflow. It is the only one
of the three external interactions that both changes workflow state and returns
a value to the caller:

| | direction | changes state | returns a value |
|---|---|---|---|
| signal | in | yes | no |
| `SetQueryState` | out | no | yes |
| **update** | both | yes | yes |

A caller posts `POST /api/workflows/:id/update/:name`, gets `202` with a
`promise_id`, and waits on that promise for the handler's return value.

**An update name can be used once per workflow.** `workflow_update_requests` is
keyed `(workflow_id, update_name)` and completion marks the row rather than
deleting it, so the name is consumed for the life of the run. A second request
under the same name is refused with `409`, and the `detail` field says which of
the two refusals it is:

| `detail` | means | clears |
|---|---|---|
| `update_already_pending` | the first request has not been dispatched yet | when it is handled |
| `update_name_used` | this name has already been handled on this workflow | never |

This is recorded as the behaviour that ships, not as a contract anyone designed:
whether a name *should* be reusable is open in
[cleat#1330](https://github.com/cleat-team/cleat/issues/1330), and either answer
is a schema change. Until it is settled, treat a name as single-use and use a
distinct one per request — `bump-1`, `bump-2` — rather than relying on either
behaviour persisting.

```go
DispatchUpdates()
```

Delivers and runs every update currently pending for this workflow.

**The SDK already calls this before each suspension** -- `DurableSleep`,
`AwaitSignals`, `AwaitPromise`, `AwaitChild`, `AwaitAllChildren`,
`AwaitAnyChild` -- so an ordinary workflow needs no update-specific code. It is
exported for workflows that want to service updates at additional points.

Those call sites are *dispatch points*, and the position matters more than the
timing. An update handler is a closure in guest memory, so only guest code can
invoke it -- an arriving update cannot interrupt the workflow. Replay
re-executes the workflow and matches host calls against the recorded history in
order, so delivery has to happen at the same **program position** every run.
That is what makes the handler's effect on workflow state reproducible.

The consequence to know: **an update is handled at the next dispatch point, not
the instant it arrives.** A workflow in a tight loop of durable calls with no
suspension will not service updates until it suspends.

There is no `RegisterQueryHandler` -- it was removed 2026-08-09 (see
`docs/determinism.md`, "Why there is no RegisterQueryHandler"). It recorded a
handler name but nothing ever routed an external query to it, so it was
usable only inside an in-process test harness, never from a real client. Use
`SetQueryState`, below, to publish state any caller can read.

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

## StateManager -- Queryable State

```go
SetQueryState(key, value string)
```

Sets workflow state that is visible via the REST API (`GET /api/workflows/:id?key=X`).

```go
h.SetQueryState("order_status", "shipped")
```

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

Manages virtual object instance scoping. In the **engine**, entering a scope
takes a concurrency key named `vo:<objectType>:<instanceKey>` and holds it until
the scope is cleared or replaced, so two workflows cannot be inside the same
instance at once.

> **Gap — the Go SDK does not reach that.** `SetScope`, `GetScope` and
> `ClearScope` set local fields and never call `cleat_set_scope`: there is no
> `HostCallsOptions` field, no row in `wasm/usage.go`, and no adapter
> definition, so nothing generates the host call. **A Go workflow calling
> `SetScope` takes no lock.** Rust, Java and AssemblyScript all bind and call
> the import. See IMPROVEMENT-PLAN §3.223.
>
> `cleat/embedded` is inert for a separate reason: its scope does not touch the
> in-memory lock map that its own `AcquireLock` uses.

The returned string is an **opaque token** for stack-style save/restore — pass
it back, do not parse it. It has the shape `vo:<objectType>:<instanceKey>:`
because it once prefixed `SetState`/`GetState` keys; those calls were removed on
2026-09-05 and the shape is vestigial.

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
