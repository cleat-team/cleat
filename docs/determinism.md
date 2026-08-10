# Determinism in Cleat Workflows

Cleat workflows must be deterministic to ensure correct replay. This document
describes the determinism guarantees, common pitfalls, and how to avoid them.

## Why Determinism Matters

Cleat records the event history of each workflow execution. When a workflow is
replayed (e.g., after a crash, for recovery, or during replay testing), the
workflow code is re-executed and must produce the same sequence of events.
Any non-determinism in the workflow code can cause replay divergence, leading
to incorrect results or workflow failure.

## Determinism Guarantees

### WASM Floating-Point (IEEE 754)

WASM `f32` and `f64` operations follow **IEEE 754-2019**, which guarantees
bit-identical results for the same arithmetic operations on the same inputs
across all compliant hardware. wazero's interpreter (used by cleat) implements
strict IEEE 754 semantics without "fast math" optimizations.

**Gotchas:**

1. **NaN payloads**: IEEE 754 allows multiple bit patterns for NaN. WASM
   operations that produce NaN may return different NaN payloads across CPU
   architectures or wazero versions. Use `math.Float64bits()` /
   `math.Float32bits()` for exact comparison, or avoid NaN entirely.
2. **Denormal numbers**: Some CPUs flush denormals to zero; wazero's
   interpreter preserves them. This is only visible if comparing floats
   for equality near the denormal range.
3. **FMA (Fused Multiply-Add)**: The host Go compiler may apply FMA,
   changing the exact result of `a*b + c` vs `fma(a, b, c)`. WASM code
   inside wazero is not affected, but be aware if mixing host and WASM
   computations.

### Cleat API Determinism

| API | Deterministic? | Notes |
|-----|---------------|-------|
| `h.Now()` | Yes | Virtual time from event history |
| `h.Random()` | Yes | Seeded replay-safe PRNG |
| `h.DurableCall()` | Yes | Recorded in event history |
| `h.Sleep()` | Yes | Virtual time |
| `h.AwaitSignal()` | Yes | Recorded signal replay |
| `time.Now()` | No | **Blocked** at vet/build time |
| `crypto/rand` | No | **Blocked** at vet/build time |

## Common Non-Determinism Patterns

### 1. Map Iteration (E021)

Go map iteration order is intentionally non-deterministic and varies between
runs. This is detected by `cleat vet` as error **E021**.

**Bad:**
```go
for name, score := range scores {
    h.DurableCall("svc", "report", ...)
}
```

**Fix:**
```go
keys := slices.Collect(maps.Keys(scores))
slices.Sort(keys)
for _, name := range keys {
    score := scores[name]
    h.DurableCall("svc", "report", ...)
}
```

### 2. Floating-Point in Control Flow (W002)

Floating-point comparisons in conditions (`if`, `for`, `switch`) can produce
different results across hardware or during replay. This is detected by
`cleat vet` as warning **W002**.

**Bad:**
```go
if val > 0.5 {
    h.DurableCall("svc", "op", `{"branch":"high"}`)
}
```

**Fix:**
```go
threshold := math.Float64bits(0.5)
vbits := math.Float64bits(val)
if vbits > threshold {
    h.DurableCall("svc", "op", `{"branch":"high"}`)
}
```

Or use integer arithmetic instead of floats.

### 3. Non-Deterministic APIs

The following patterns are blocked at build time:
- `time.Now()`, `time.Since()` -- use `h.Now()` instead
- `crypto/rand`, `math/rand` -- use `h.Random()` instead
- `sync.Mutex`, `sync.RWMutex` -- use cleat's built-in concurrency
- `os` package access -- blocked for WASM target
- `net/http` -- use `h.DurableCall()` for service calls
- `reflect` -- blocked for WASM target
- `fmt.Print*` -- blocked (captures stdout non-deterministically)
- Goroutines, channels -- use cleat's async patterns

## Why there is no `isReplaying()`

Cleat does not give a workflow a way to ask whether it is currently replaying.
This is deliberate, and it is the opposite of what several other engines do --
if you are arriving from Temporal, `workflow.IsReplaying()` has no equivalent
here on purpose.

A raw replay flag has exactly one safe use and one very tempting unsafe one.

**Unsafe:** branching workflow *logic* on it.

```go
// Never. This records different events on replay than it did on execution,
// which is the one thing replay exists to prevent.
if !isReplaying() {
    h.DurableCall("billing", "charge", ...)
}
```

**The safe use** -- not repeating a side effect that is invisible to the event
history, such as a log line or a metric -- is what `SideEffect` is for:

```go
requestID := h.SideEffect(uuid.NewString())   // computed once, replayed after
```

`SideEffect` records the result on first execution and returns the recorded
value on every replay afterwards. The value is replay-consistent *by
construction* rather than because the author remembered to check a flag, and
the difference matters: a missed check is silent, and the corresponding missed
`SideEffect` is a divergence the engine detects.

The AssemblyScript SDK carried an `isReplaying()` method until 2026-08-09. It
was hardcoded to return `false` and no host call ever backed it, so every
"only on first execution" branch fired on every replay too. It has been
removed rather than implemented, for the reason above.

## Why there is no `RegisterQueryHandler`

Every SDK (Go, Rust, Java, Python, AssemblyScript) carried a
`RegisterQueryHandler` / `register_query_handler` method until 2026-08-09.
Unlike `isReplaying()`, the underlying host call was real -- it recorded the
handler name and returned success -- but nothing downstream ever used what
it recorded. No worker code, HTTP route, or CLI command routed an external
query to a registered handler; the only thing that ever invoked one was each
SDK's own in-process test harness (`cleattest` in Go, and the equivalent in
the other SDKs), which called the registered closure directly rather than
going through any dispatch the production worker also uses. A workflow
author who wrote a query handler and tested it in `cleattest` would see it
work -- and then discover, only against a real deployment, that nothing
external could ever reach it.

This is not a small wiring gap to be closed later. Cleat's execution model
does not keep a workflow resident in a worker's memory the way Temporal's
does: a workflow instance exists only for the duration of a
claim-execute-persist cycle, and most of the time no worker holds it in
memory at all. A `QueryWorkflow`-style call that expects to reach a *live*
handler inside a *currently executing* instance does not have an addressee
most of the time it would be made. Building one for real would mean keeping
workflow instances resident and reachable, or routing a query to whichever
worker (if any) currently holds a claim on the target workflow and blocking
until it does -- a materially different execution model, not an extra host
function.

`SetQueryState` is the mechanism that actually fits how cleat runs
workflows: the workflow proactively records queryable state to the database
at points of its choosing, and that state is durable and externally readable
-- via `GetQueryState` or `GET /api/workflows/:id/query?key=X` -- regardless
of whether any worker currently has the workflow loaded. It answers "what is
this workflow's status" without needing anything to be running at query
time.

```go
h.SetQueryState("order_status", "shipped")
```

```
curl http://worker:8080/api/workflows/<id>/query?key=order_status
```

The removed host call (`cleat_register_query_handler`) is still accepted by
the engine as a no-op, purely so that guests already compiled against it
still instantiate; no SDK exposes a way to call it going forward, and no new
caller should be added on either side of that boundary.

## Verifying Determinism

Run `cleat vet` before deploying any workflow:

```bash
cleat vet ./workflows/my-workflow/
```

For CI/CD pipelines, use `--json` and `--ci` flags for machine-readable output
and strict enforcement.

## Event History Integrity

Cleat computes a **SHA-256 checksum** for each event in the event history.
The checksum is stored alongside the event data and verified during replay.
This provides runtime detection of data corruption, tampering, or
non-deterministic behavior.

If the checksum verification fails during replay, the engine:
1. Increments the `cleat_replay_checksum_failures_total` Prometheus counter
2. Logs the failure with the workflow ID and step number
3. If `failOnChecksumMismatch` is true (default), aborts the replay
4. If `failOnChecksumMismatch` is false, logs a warning and continues

This provides a safety net for non-determinism that was not caught at vet time.
