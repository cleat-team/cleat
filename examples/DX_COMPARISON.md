# Developer Experience Comparison: Cleat vs Temporal vs DBOS

Four common workflow patterns implemented in all three frameworks, with
readability and developer friction compared side-by-side.

> **The DBOS snippets below use the functional API, as the executed ports do.**
> The pairs under `examples/*-dbos-port/` are CI-run — `ci.yml`'s three
> `*-dbos-pair-scenario` jobs *execute* them, not merely build them — and are the
> reference for DBOS's current TypeScript API. An earlier revision of this
> document used the class/decorator style (`@DBOS.workflow()`, `@DBOS.step()`).
> `docs.dbos.dev` still documents that style, so it has not been removed — but
> the SDK's own `README` presents the functional one as canonical, and
> `examples/order-lifecycle-dbos-port/ISSUES.md` (section "API churn inside
> 'current docs'") records both and why the port chose it. Comparing against a
> superseded API overstates the other side's friction, so each snippet below is
> re-based to the functional spelling and names the port file that demonstrates
> the call shape it uses. cleat#2996 is the audit that found this; cleat#3018 is
> the re-baseline.
>
> **Timeout arguments below are in SECONDS, and this document used to be wrong
> about that.** `DBOS.recv<T>(topic, options?: number | RecvOptions)` — a numeric
> argument is used *directly* as `timeoutSeconds` in `@dbos-inc/dbos-sdk@5.2.11`
> (`resolveTimeoutSeconds` returns it unchanged; only `deadlineEpochMS` is
> divided by 1000). An earlier revision passed millisecond literals, so a "3 day"
> grace period (`3 * 24 * 60 * 60 * 1000`) was read by the API as roughly 3000
> days, and a "24 hour" wait (`twentyFourHoursMs`) as roughly 2700 years. Nothing
> caught it because the snippets are illustrative and are never executed — which
> is the same reason the class/decorator drift survived as long as it did.

## Pattern 1: Subscription Billing

### Cleat (Go)

```go
func ManageSubscription(h cleat.HostCalls, input SubscriptionInput) (string, error) {
    if err := chargeWithRetry(h, input); err != nil {
        return enterGracePeriod(h, input)
    }
    h.DurableSleep(30 * 24 * time.Hour)
    return h.ContinueAsNew(toJSON(input))
}

func chargeWithRetry(h cleat.HostCalls, input SubscriptionInput) error {
    resp, err := h.CallWithRetry(cleat.CallOptions{
        RetryPolicy: &cleat.RetryPolicy{
            MaxAttempts: 4, InitialInterval: 1 * time.Second,
            BackoffCoefficient: 2.0, MaxInterval: 30 * time.Second,
        },
    }, "billing", "Charge", req)
    // ... handle result ...
}
```

**DX notes:** The retry policy is inline with the call, not configured elsewhere.
Cancellation is checked with `h.PollCancellation()` at natural boundaries — no
separate signal registration needed.

### Temporal (TypeScript)

```typescript
// Retry configured on the proxy, separate from the call site:
const { chargePayment } = proxyActivities<BillingActivities>({
  startToCloseTimeout: '30 seconds',
  retry: { maximumAttempts: 3, initialInterval: '1 second', ... },
});

// Signal requires defineSignal + setHandler pair, setHandler must come before first await:
export const cancelSubscriptionSignal = defineSignal('cancel_subscription');
setHandler(cancelSubscriptionSignal, () => { cancelled = true; });

// Waiting for signal OR timeout uses condition():
const signalReceived = await condition(() => cancelled, '3 days');
```

**DX friction:** Retry config is decoupled from the call site. Signal handling
requires three separate declarations (define, setHandler, condition). The
`condition()` pattern for "wait for signal or timeout" is non-obvious to
newcomers (sleep + flag check introduces race conditions during replay).
Workflow/activity split means you maintain two sets of files (workflows + activities).

### DBOS (TypeScript)

```typescript
async function chargePayment(userId: string, amount: number): Promise<boolean> {
  const result = await paymentGateway.charge(userId, amount);
  return result.success;
}

async function monthlyBilling(userId: string, amount: number): Promise<void> {
  // DBOS.send(workflowID, "cancel", "billing_cancel") — sent externally
  const cancelSignal = await DBOS.recv<string>("billing_cancel", { timeoutSeconds: 0 });
  if (cancelSignal === "cancel") { /* cancel */ return; }

  try {
    await DBOS.runStep(() => chargePayment(userId, amount), {
      name: "chargePayment",
      retriesAllowed: true, maxAttempts: 3, intervalSeconds: 1, backoffRate: 2,
    });
    // Success — sleep 30 days then ContinueAsNew equivalent would be a new workflow
  } catch {
    // Grace period: one recv with a 3-day timeout = sleep + signal combined
    const during = await DBOS.recv<string>("billing_cancel", { timeoutSeconds: 3 * 24 * 60 * 60 });
    if (during === "cancel") return;
    // Grace expired
  }
}

export const billingWorkflow = DBOS.registerWorkflow(monthlyBilling, { name: "monthlyBilling" });
```

**DX notes:** DBOS takes retry config on the `runStep` options object — *at the
call site*, not on a separate declaration. The config is `StepConfig & { name?:
string }` in the pinned SDK, i.e. `name` plus `StepConfig`'s `retriesAllowed`,
`intervalSeconds`, `maxAttempts`, `backoffRate`; the call shape is the one
`examples/b2b-saas-control-plane-dbos-port/src/workflow.ts:195` executes. On this axis DBOS is as close to the call as cleat's `CallWithRetry`
and closer than Temporal's separate activity proxy. `DBOS.recv` with a timeout
combines sleep + signal wait into one call (the options form is what
`examples/order-lifecycle-dbos-port/src/workflow.ts:122` runs). Cancellation is
checked only at step boundaries (not mid-step), which is a gap for very
long-running steps. DBOS requires an explicit `DBOS.launch()` +
`DBOS.setConfig()` lifecycle in `main()`.

### Comparison

| Concern | Cleat | Temporal | DBOS |
|---------|-------|----------|------|
| Retry config location | Inline at call site | On activity proxy (separate) | On `runStep` options, at call site |
| Signal handling | `h.PollCancellation()` | 3 declarations needed | `DBOS.recv(topic, timeout)` — 1 call |
| Sleep + signal wait | `h.AwaitSignals(names, timeout)` | `condition(() => x, timeout)` | `DBOS.recv(topic, timeout)` — built in |
| File count | 1 file | 3 files (types + activities + workflow) | 1 file (functions) |
| Worker model | Separate daemon process | Separate worker process | App IS the worker (in-process) |
| Infrastructure | PostgreSQL only | Temporal server + database | PostgreSQL only |

---

## Pattern 2: User Onboarding (Signals + Timeouts)

### Cleat (Go)

```go
func RegisterUser(h cleat.HostCalls, input SignupInput) (*Profile, error) {
    h.Call("email", "SendVerification", ...)

    result := h.AwaitSignals([]string{"email_verified"}, 24*time.Hour)
    if result.TimedOut {
        return handleVerificationTimeout(h, userID, input)
    }
    // Signal received — extract payload and continue.
    h.Call("users", "CreateProfile", ...)
    h.Call("email", "SendWelcome", ...)
}
```

**DX notes:** `AwaitSignals` handles both signal delivery and timeout in one
call. The `SignalResult` struct cleanly separates the timed-out case from the
signal-received case. No signal "registration" boilerplate.

### Temporal (TypeScript)

```typescript
export const emailVerifiedSignal = defineSignal<[string]>('email_verified');

setHandler(emailVerifiedSignal, (email) => { verifiedEmail = email; });

const verified = await condition(() => verifiedEmail !== undefined, '24 hours');

if (verified) {
    await createProfile(...);
    await sendWelcomeEmail(...);
} else {
    await sendReminderEmail(...);
}
```

**DX friction:** The mutable closure variable pattern (`verifiedEmail`) for
signal delivery is error-prone. The `condition()` API is Temporal-specific —
developers coming from standard async/await expect `Promise.race([sleep, signal])`.

### DBOS (TypeScript)

```typescript
// Signal wait + timeout combined, in one call:
const result = await DBOS.recv<string>("email_verification", { timeoutSeconds: 24 * 60 * 60 });

if (result === "verified") {
    await DBOS.runStep(() => createProfile(userId, email), { name: "createProfile" });
    await DBOS.runStep(() => sendWelcomeEmail(email), { name: "sendWelcomeEmail" });
} else {
    // Timeout or unexpected message
    await DBOS.runStep(() => sendReminderEmail(email), { name: "sendReminderEmail" });
}

// External HTTP handler that sends the signal:
// DBOS.send(workflowID, "verified", "email_verification");
```

**DX friction:** `DBOS.recv` with a timeout is cleaner than Temporal's
`condition()` pattern. But the workflow ID must be passed to the client
(via verification link URL) for `DBOS.send` to work — an extra coordination
step not needed in cleat or Temporal. No built-in "signal" type safety
beyond the generic `<string>`.

### Comparison

| Concern | Cleat | Temporal | DBOS |
|---------|-------|----------|------|
| Signal + timeout pattern | 1 call: `AwaitSignals` | 3 declarations + `condition()` | 1 call: `DBOS.recv(topic, timeout)` |
| Signal payload | Typed via JSON unmarshal | Typed via generic `<[T]>` | Untyped (string generic) |
| Timeout handling | `result.TimedOut` boolean | `condition()` returns false | `recv` returns null |
| Mutability needed | None (result struct) | Mutable closure var | None (null check) |

---

## Pattern 3: Travel Booking (Parallel Saga)

### Cleat (Go)

```go
s := cleat.NewSaga()

s.AddStep("book_flight",
    func(h cleat.HostCalls) (string, error) {
        return h.Call("flights", "Book", ...)
    },
    func(h cleat.HostCalls) error {
        return h.Call("flights", "Cancel", ...)
    },
)
s.AddStep("book_hotel", bookHotelFn, cancelHotelFn)
s.AddStep("book_car",   bookCarFn,   cancelCarFn)

if err := s.Run(h); err != nil {
    // All completed steps already compensated in LIFO order.
}
```

**DX notes:** The Saga pattern is a first-class construct. Forward and
compensate functions are declared together at each step. Compensation is
automatic — no manual `try/catch` or compensation loop needed. The builder
pattern (`AddStep` + `Run`) is readable even to non-Go developers.

**BUT: All steps execute sequentially through `Saga`. For parallel booking,
you need to manually use `ChildWorkflow` for concurrency, then `AwaitChild`
to collect results.** The current Saga API only supports sequential steps.

**IMPROVEMENT OPPORTUNITY:** Add `Saga.AddParallel(steps ...SagaStep)` for
concurrent execution with collective compensation. Each step still has its
own forward/compensate pair, but they run in parallel. If any parallel step
fails, all completed parallel steps are compensated.

### Temporal (TypeScript)

```typescript
// Manual Saga helper class or CancellationScope pattern:
try {
    const [flight, hotel, car] = await Promise.all([
        bookFlight(...).then(r => { flightRef = r.ref; return r; }),
        bookHotel(...).then(r => { hotelRef = r.ref; return r; }),
        bookCar(...).then(r => { carRef = r.ref; return r; }),
    ]);
} catch (err) {
    // Manually compensate completed bookings:
    if (flightRef) await cancelFlight({ bookingRef: flightRef });
    if (hotelRef) await cancelHotel({ confirmationNumber: hotelRef });
    if (carRef) await cancelCar({ reservationId: carRef });
}
```

**DX friction:** Compensation is entirely manual. You must track what succeeded
via mutable variables and write an explicit compensation block. For 3 steps
this is manageable; for 10+ it becomes unwieldy. Every developer implements
their own Saga helper differently.

### DBOS (TypeScript)

```typescript
// DBOS has no Saga framework. Compensation is entirely manual.
const results = await Promise.allSettled([
    DBOS.runStep(() => bookFlight(bookingId, flight), { name: "bookFlight" }),
    DBOS.runStep(() => bookHotel(bookingId, hotel), { name: "bookHotel" }),
    DBOS.runStep(() => bookCar(bookingId, car), { name: "bookCar" }),
]);

// Must use allSettled, not all — steps must start in deterministic order.
const compensations: Array<() => Promise<void>> = [];
if (results[0].status === "fulfilled") {
    compensations.push(() => DBOS.runStep(() => cancelFlight(results[0].value), { name: "cancelFlight" }));
}
// ... repeat for hotel, car ...

// Compensate in LIFO order on any failure.
for (let i = compensations.length - 1; i >= 0; i--) {
    try { await compensations[i](); } catch { /* log and continue */ }
}
```

**DX friction:** No Saga construct — every developer builds their own
compensation array + LIFO loop. `Promise.allSettled` required (not
`Promise.all` — a non-obvious gotcha). Cancellation is only checked at
step boundaries, not mid-step. For 3 steps it's manageable; for 10+
it becomes unwieldy and error-prone.

### Comparison

| Concern | Cleat | Temporal | DBOS |
|---------|-------|----------|------|
| Saga construct | Built-in `cleat.NewSaga()` | Manual (no built-in) | Manual (no built-in) |
| Compensation | Automatic LIFO | Manual try/catch block | Manual Promise.allSettled inspection |
| Parallel + Saga | Sequential only (gap) | Manual with Promise.all | Manual with Promise.allSettled |
| Declarative intent | Yes (AddStep pairs) | No | No |

**RECOMMENDATION:** Add `Saga.AddParallel()` to cleat to close the parallel
booking gap. See implementation sketch in the "Improvements" section below.

---

## Pattern 4: Data Pipeline (Fan-out/Fan-in with Child Workflows)

### Cleat (Go)

```go
func RunPipeline(h cleat.HostCalls, input PipelineInput) (*PipelineResult, error) {
    var runIDs []string
    for _, item := range input.Items {
        runID, _ := h.ChildWorkflow("process_item", toJSON(ChildInput{
            Item: item, JobID: input.JobID, ...
        }))
        runIDs = append(runIDs, runID)
    }

    for _, runID := range runIDs {
        resultJSON, err := h.AwaitChild(runID)
        // ... collect results ...
    }
}
```

**DX notes:** `ChildWorkflow` + `AwaitChild` is the simplest fan-out/fan-in
API of the three. No separate task queue configuration needed. The child
workflow name is a string (not a function reference), which means it's resolved
at runtime — flexible but loses compile-time type checking.

### Temporal (TypeScript)

```typescript
const childPromises = input.items.map(item =>
    executeChild(processItemWorkflow, {
        args: [{ itemId: item.id, sourceUrl: item.url }],
        workflowId: `pipeline-${jobId}-item-${item.id}`,
        taskQueue: 'pipeline-workflows',
        workflowExecutionTimeout: '10 minutes',
        retry: { maximumAttempts: 2, initialInterval: '10 seconds' },
    })
);
await Promise.all(childPromises);
```

**DX friction:** Each child workflow call needs 6 lines of options (workflowId,
taskQueue, timeouts, retry). The worker also needs explicit registration for
each task queue. Strong typing on child inputs/outputs (good), but at the
cost of all the configuration. Three separate workers needed (parent, child
workflow, child activities).

### DBOS (TypeScript)

```typescript
// Fan out: start one child workflow per item.
// `processItem` is a function registered with DBOS.registerWorkflow;
// `startWorkflow(fn, params)` returns a function you then call with the args.
const handles: WorkflowHandle<string>[] = [];
for (const itemId of itemIds) {
    const handle = await DBOS.startWorkflow(processItem, {
        workflowID: `pipeline-${itemId}`,  // idempotent
    })(itemId);
    handles.push(handle);
}

// Fan in: collect results sequentially (each await is a durable checkpoint).
const results: string[] = [];
for (const handle of handles) {
    try {
        results.push(await handle.getResult());
    } catch (e) {
        results.push(`failed: ${(e as Error).message}`);
    }
}
```

**DX notes:** DBOS child workflows are independent workflows — not
in-process like Temporal's `executeChild`. The `workflowID` parameter
provides idempotency (exactly-once for that child). `handle.getResult()`
awaits the child's completion. Children are recovered automatically at
`DBOS.launch()` together with their parent.

### Comparison

| Concern | Cleat | Temporal | DBOS |
|---------|-------|----------|------|
| Child workflow call | `h.ChildWorkflow(name, input)` | `executeChild(fn, { args, ...6 options })` | `DBOS.startWorkflow(fn, opts)(args)` |
| Result collection | `h.AwaitChild(runID)` — sequential | `Promise.all(promises)` — parallel | `handle.getResult()` — sequential |
| Task queue config | None (auto) | Required per-child | None (auto) |
| Type safety | String name (runtime) | Function ref (compile-time) | Function ref (compile-time) |
| Child independence | Part of parent's history | Part of parent's history | Fully independent workflow |
| Recovery | Replay restarts child | Replay restarts child | Auto-recovers at launch |

---

## Overall Findings

### Cleat Advantages

1. **One-file workflows.** No activity/workflow split. The entire business
   logic lives in one Go package. Temporal requires 3+ files per pattern
   (types, activities, workflow). DBOS in the functional style also keeps steps
   inline at the call site (`DBOS.runStep(() => ...)`), so on file count it, too,
   is one file — the advantage over Temporal stands, the one over DBOS does not.

2. **Signal handling is dead simple.** `AwaitSignals(names, timeout)` handles
   both the wait and the timeout in one call. Temporal requires `defineSignal`
   + `setHandler` + `condition()`. DBOS is one call too
   (`DBOS.recv(topic, { timeoutSeconds })` — `examples/order-lifecycle-dbos-port/src/workflow.ts`
   uses exactly that form), so here cleat **matches** DBOS rather than beating it.

3. **Retry is inline.** `CallWithRetry` puts retry policy at the call
   site. Temporal puts it on the activity proxy (separate file). DBOS's
   functional `runStep` also takes retry options at the call site, so here
   cleat ties DBOS rather than beating it — only the comparison with Temporal is
   a gap.

4. **Saga is built in.** `cleat.NewSaga()` with forward/compensate pairs
   and automatic LIFO compensation. Temporal and DBOS require manual
   compensation logic.

5. **No task queue ceremony.** Cleat workers use a single `SKIP LOCKED` poll
   loop. Temporal requires per-queue worker registration and per-child queue
   configuration. This alone saves 30+ lines of boilerplate per workflow.

### Cleat Disadvantages (Found During This Exercise)

1. **Saga was sequential only**, so parallel bookings could not carry automated
   compensation. **Since fixed:** `Saga.AddParallel` (`cleat/runtime_workflow.go`).

2. **`ChildWorkflow` takes a string name, not a function reference.**
   No compile-time check that the child exists or has the right signature.
   Temporal and DBOS pass actual function/class references. **Partly
   addressed:** `ChildWorkflowTyped` (`cleat/runtime_children.go`) types the
   *input*, but the name is still a string, so **the function-reference half —
   and with it the compile-time check — is still open.**

3. **Awaiting children was sequential per-child in the loop** — each
   `AwaitChild` blocked until that child completed. **Since fixed:**
   `AwaitAllChildren(runIDs)` (`cleat/runtime_children.go`).

4. **`CallWithHeartbeat` did not compose with `CallTyped`.** **Half fixed:**
   the typed heartbeated call now exists — `DurableCallTypedWithHeartbeat`
   (`cleat/runtime.go`). **The other half stands:** there is still no
   `CallWithHeartbeatAndRetry` in `cleat/`, so a heartbeated call *with retry*
   has to be assembled from the raw string API.

5. **`AwaitSignals` did not honour a signal delivered before it was called.**
   **Fixed:** the durable await checks the signal store *before* it records the
   await and suspends — `engine/signaller.go:254` ("Fresh execution: check signal
   store first") on the fresh path, and the same check at `:114` on the replay
   path — so a signal already waiting is returned rather than waited out.
   `PollSignals` / `PollSignal` remain for an explicit non-blocking peek. This
   row read "not re-checked" through the prior revision; cleat#3018 settled it
   against the code rather than against the table.

6. **Can't cancel a running `Call` mid-execution.** Temporal's
   `CancellationScope` can cancel in-flight activities. DBOS checks
   cancellation at step boundaries. Cleat has no mechanism to abort a
   long-running service call once started. **Fix: pass context
   cancellation into `ServiceCaller.Call()` so the external HTTP/gRPC
   call can be cancelled.**

7. **Worker model is separate daemon, unlike DBOS's embedded library.**
   DBOS runs in-process with your application — deploying your app
   deploys your workflows. Cleat requires a separate worker process.
   This is architecturally cleaner (independent scaling) but adds
   operational complexity for simple use cases. The `--api-addr` web UI
   partially bridges this gap. **Consider: a `cleat run --embedded`
   mode that runs workflows in-process for single-binary deployments.**

### Improvements to Action

**Five of the rows below were implemented after this exercise was written**, and
each names the symbol that closes it; the rest are open. The rows are kept rather
than deleted so a reader can see both the gaps that were found and that they have
since been closed — a table of completed work presented as backlog tells a reader
the project knows about gaps it has already fixed.

| Improvement | Status |
|-------------|--------|
| `Saga.AddParallel()` | **Implemented** — `Saga.AddParallel`, `cleat/runtime_workflow.go:705` |
| `AwaitAllChildren(runIDs)` | **Implemented** — `cleat/runtime_children.go:66` |
| typed heartbeated call | **Implemented** — `DurableCallTypedWithHeartbeat`, `cleat/runtime.go:1283`. The *retry* composition half is still open; see below |
| typed saga steps | **Implemented** — `Saga.AddStepCall`, `cleat/runtime_workflow.go:530` |
| `ChildWorkflow` with function references | **Open** — `ChildWorkflowTyped` (`cleat/runtime_children.go`) types the *input*; the name is still a string, so there is still no compile-time check that the child exists |
| Pending signals honoured before `AwaitSignals` blocks | **Implemented** — the durable await polls the signal store before it records the await and suspends (`engine/signaller.go:254`, fresh path; `:114`, replay path), so a signal delivered before the call is returned, not waited out |
| Heartbeated call **with retry** in one call | **Open** — no `CallWithHeartbeatAndRetry` in `cleat/` |

## Verdict

Cleat's developer experience is cleaner for the common patterns we tested. The
signal/timeout pattern (`AwaitSignals`) and the Saga API express intent
directly, without the activity/workflow split or the queue and
signal-registration boilerplate the other two require.

The gaps we found (parallel Saga, concurrent child await, typed heartbeats)
were **closed afterwards as additive changes** — they did not alter the core
API, they extended it, which is what this list predicted. What remains open is
named in the table above.

**One caveat on this verdict.** It originally rested on a "~28% shorter"
line-count exercise that the base measurement later found **false**. The
sentence was removed; the conclusion is **not** re-derived from a replacement
measurement here. What the verdict stands on is the pattern-by-pattern evidence
above, not a line count. The DBOS column has since been re-baselined onto the
functional API the executed ports use (note at the top); that moved two rows —
retry-config location and file count — from "cleat wins" to "cleat ties".

The big differentiators: **cleat workflows are one file** (no activity/workflow
split), the **Saga is a first-class construct** (`cleat.NewSaga()`, which neither
other framework provides), and **no task-queue ceremony** (Temporal needs
per-queue worker registration; DBOS avoids it by running in-process). The
one-file property is a real gap for Temporal, which needs 3+ files per pattern;
it is **not** a gap for DBOS, whose functional style is also one file. Those are
the things to protect as you add features.
