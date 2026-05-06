# Developer Experience Comparison: Cleat vs Temporal vs DBOS

Four common workflow patterns implemented in all three frameworks, with
readability and developer friction compared side-by-side.

## Pattern 1: Subscription Billing

### Cleat (Go)

```go
func ManageSubscription(h durable.HostCalls, input SubscriptionInput) (string, error) {
    if err := chargeWithRetry(h, input); err != nil {
        return enterGracePeriod(h, input)
    }
    h.DurableSleep(30 * 24 * time.Hour)
    return h.ContinueAsNew(toJSON(input))
}

func chargeWithRetry(h durable.HostCalls, input SubscriptionInput) error {
    resp, err := h.DurableCallWithRetry(durable.CallOptions{
        RetryPolicy: &durable.RetryPolicy{
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
class BillingSteps {
  @DBOS.step({ retriesAllowed: true, maxAttempts: 3, intervalSeconds: 1, backoffRate: 2 })
  static async chargePayment(userId: string, amount: number): Promise<boolean> {
    const result = await paymentGateway.charge(userId, amount);
    return result.success;
  }
}

export class BillingWorkflow {
  @DBOS.workflow()
  static async monthlyBilling(userId: string, amount: number): Promise<void> {
    // DBOS.send(workflowID, "cancel", "billing_cancel") — sent externally
    const cancelSignal = await DBOS.recv<string>("billing_cancel", 0);
    if (cancelSignal === "cancel") { /* cancel */ return; }

    try {
      await BillingSteps.chargePayment(userId, amount);
      // Success — sleep 30 days then ContinueAsNew equivalent would be a new workflow
    } catch {
      // Grace period: DBOS.recv with 3-day timeout = sleep + signal combined
      const during = await DBOS.recv<string>("billing_cancel", 3*24*60*60*1000);
      if (during === "cancel") return;
      // Grace expired
    }
  }
}
```

**DX notes:** DBOS uses decorator-based retry config on the step (separate from
the call site, but closer than Temporal's proxy pattern). `DBOS.recv` with a
timeout elegantly combines sleep + signal wait into one call — cleaner than
Temporal's `condition()`. Cancellation is checked only at step boundaries
(not mid-step), which is a gap for very long-running steps. DBOS requires
explicit `DBOS.launch()` + `DBOS.setConfig()` lifecycle in `main()`.

### Comparison

| Concern | Cleat | Temporal | DBOS |
|---------|-------|----------|------|
| Retry config location | Inline at call site | On activity proxy (separate) | On step decorator (on method) |
| Signal handling | `h.PollCancellation()` | 3 declarations needed | `DBOS.recv(topic, timeout)` — 1 call |
| Sleep + signal wait | `h.AwaitSignals(names, timeout)` | `condition(() => x, timeout)` | `DBOS.recv(topic, timeout)` — built in |
| File count | 1 file | 3 files (types + activities + workflow) | 1 file (workflow class + step class) |
| Worker model | Separate daemon process | Separate worker process | App IS the worker (in-process) |
| Infrastructure | PostgreSQL only | Temporal server + database | PostgreSQL only |

---

## Pattern 2: User Onboarding (Signals + Timeouts)

### Cleat (Go)

```go
func RegisterUser(h durable.HostCalls, input SignupInput) (*Profile, error) {
    h.DurableCall("email", "SendVerification", ...)

    result := h.AwaitSignals([]string{"email_verified"}, 24*time.Hour)
    if result.TimedOut {
        return handleVerificationTimeout(h, userID, input)
    }
    // Signal received — extract payload and continue.
    h.DurableCall("users", "CreateProfile", ...)
    h.DurableCall("email", "SendWelcome", ...)
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
// Signal wait + timeout combined:
const twentyFourHoursMs = 24 * 60 * 60 * 1000;
const result = await DBOS.recv<string>("email_verification", twentyFourHoursMs);

if (result === "verified") {
    await OnboardingSteps.createProfile(userId, email);
    await OnboardingSteps.sendWelcomeEmail(email);
} else {
    // Timeout or unexpected message
    await OnboardingSteps.sendReminderEmail(email);
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
s := durable.NewSaga()

s.AddStep("book_flight",
    func(h durable.HostCalls) (string, error) {
        return h.DurableCall("flights", "Book", ...)
    },
    func(h durable.HostCalls) error {
        return h.DurableCall("flights", "Cancel", ...)
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
    BookingSteps.bookFlight(bookingId, flight),
    BookingSteps.bookHotel(bookingId, hotel),
    BookingSteps.bookCar(bookingId, car),
]);

// Must use allSettled, not all — steps must start in deterministic order.
const compensations: Array<() => Promise<void>> = [];
if (results[0].status === "fulfilled") {
    compensations.push(() => BookingSteps.cancelFlight(results[0].value));
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
| Saga construct | Built-in `durable.NewSaga()` | Manual (no built-in) | Manual (no built-in) |
| Compensation | Automatic LIFO | Manual try/catch block | Manual Promise.allSettled inspection |
| Parallel + Saga | Sequential only (gap) | Manual with Promise.all | Manual with Promise.allSettled |
| Declarative intent | Yes (AddStep pairs) | No | No |

**RECOMMENDATION:** Add `Saga.AddParallel()` to cleat to close the parallel
booking gap. See implementation sketch in the "Improvements" section below.

---

## Pattern 4: Data Pipeline (Fan-out/Fan-in with Child Workflows)

### Cleat (Go)

```go
func RunPipeline(h durable.HostCalls, input PipelineInput) (*PipelineResult, error) {
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
const handles: DBOS.WorkflowHandle<string>[] = [];
for (const itemId of itemIds) {
    const handle = await DBOS.startWorkflow(ItemProcessor, {
        workflowID: `pipeline-${itemId}`,  // idempotent
    }).processItem(itemId);
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
| Child workflow call | `h.ChildWorkflow(name, input)` | `executeChild(fn, { args, ...6 options })` | `DBOS.startWorkflow(Class, opts).method(args)` |
| Result collection | `h.AwaitChild(runID)` — sequential | `Promise.all(promises)` — parallel | `handle.getResult()` — sequential |
| Task queue config | None (auto) | Required per-child | None (auto) |
| Type safety | String name (runtime) | Function ref (compile-time) | Class ref (compile-time) |
| Child independence | Part of parent's history | Part of parent's history | Fully independent workflow |
| Recovery | Replay restarts child | Replay restarts child | Auto-recovers at launch |

---

## Overall Findings

### Cleat Advantages

1. **One-file workflows.** No activity/workflow split. The entire business
   logic lives in one Go package. Temporal requires 3+ files per pattern
   (types, activities, workflow). DBOS requires a workflow class with
   separate step methods.

2. **Signal handling is dead simple.** `AwaitSignals(names, timeout)` handles
   both the wait and the timeout in one call. Temporal requires `defineSignal`
   + `setHandler` + `condition()`. DBOS requires `recv()` + `Promise.race`.

3. **Retry is inline.** `DurableCallWithRetry` puts retry policy at the call
   site. Temporal puts it on the activity proxy (separate file). DBOS puts
   it in config YAML (even further away).

4. **Saga is built in.** `durable.NewSaga()` with forward/compensate pairs
   and automatic LIFO compensation. Temporal and DBOS require manual
   compensation logic.

5. **No task queue ceremony.** Cleat workers use a single `SKIP LOCKED` poll
   loop. Temporal requires per-queue worker registration and per-child queue
   configuration. This alone saves 30+ lines of boilerplate per workflow.

### Cleat Disadvantages (Found During This Exercise)

1. **Saga is sequential only.** Can't run parallel bookings with automated
   compensation. Need `ChildWorkflow` for parallelism, but then compensation
   is manual. **Fix: Add `Saga.AddParallel()`.**

2. **`ChildWorkflow` takes a string name, not a function reference.**
   No compile-time check that the child exists or has the right signature.
   Temporal and DBOS pass actual function/class references. **Fix: Add a
   typed child workflow registry or accept `interface{}` with runtime type
   checking in `durable-gen`.**

3. **`AwaitChild` is sequential per-child in the loop.** You can't
   `Promise.all`-style await all children simultaneously. Each `AwaitChild`
   blocks until that child completes. **Fix: Add `AwaitAllChildren(runIDs)`
   that returns results when all complete, in completion order.**

4. **`DurableCallWithHeartbeat` doesn't compose with `DurableCallTyped` or
   `DurableCallWithRetry`.** For heartbeated calls with retry, you must use
   the raw string-based API and implement retry manually. **Fix: Add
   `DurableCallTypedWithHeartbeat` and `DurableCallWithHeartbeatAndRetry`.**

5. **No `AwaitSignals` variant that returns immediately if a signal is
   already pending.** The current API always blocks until timeout, even
   if the signal was delivered before `AwaitSignals` was called. **Fix:
   Ensure signal delivery before `AwaitSignals` is honored (check signal
   store before blocking), or add `PollSignal` equivalents that work
   pre-AwaitSignals.**

6. **Can't cancel a running `DurableCall` mid-execution.** Temporal's
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
   partially bridges this gap. **Consider: a `durable run --embedded`
   mode that runs workflows in-process for single-binary deployments.**

### Improvements to Action

| Pri | Improvement | Effort | Why |
|-----|-------------|--------|-----|
| P0 | `Saga.AddParallel()` | ~1 day | Parallel booking is a top-3 use case; manual compensation at scale is error-prone |
| P0 | `AwaitAllChildren(runIDs)` | ~1 day | Fan-in should be concurrent, not sequential per-child |
| P1 | `DurableCallTypedWithHeartbeat` | ~half day | Remove the typed/untyped mismatch in heartbeat API |
| P1 | `Saga.AddStep` from typed clients | ~half day | Saga steps currently require raw `DurableCall`; typed calls would eliminate boilerplate |
| P2 | `ChildWorkflow` with function references | ~2 days | Compile-time safety for child workflow dispatch |
| P2 | Ensure pending signals honored before `AwaitSignals` blocks | ~1 day | Fix signal ordering edge case |

## Verdict

Cleat's developer experience is already cleaner than both Temporal and DBOS
for the common patterns we tested. The signal/timeout pattern (`AwaitSignals`)
and Saga API are genuinely better — they express intent more directly with
fewer lines and less ceremony.

The gaps we found (parallel Saga, concurrent child await, typed heartbeats)
are additive — they don't change the core API, they extend it. Each is a few
hundred lines of code, not a redesign.

The big differentiator: **cleat workflows are one file.** No activity/workflow
split, no task queue configuration, no signal registration boilerplate, no
separate worker setup per workflow type. This is the thing to protect as you
add features.
