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
across all compliant hardware. This is a property of the WASM specification
rather than of any one engine: both engines cleat runs — wasmtime on a worker,
wazero under CLI and test tooling — implement strict IEEE 754 semantics without
"fast math" optimizations.

> Corrected 2026-09-06. This paragraph credited the guarantee to "wazero's
> interpreter (used by cleat)", which was wrong twice over. A worker has not
> run on wazero since #459 (2026-08-10) deleted that backend; and cleat does
> not use wazero's interpreter in any case — `engine/runtime.go` builds it with
> `wazero.NewRuntimeConfigCompiler()`. The gotchas below are restated as
> properties of WASM and of the host, which is what they always were.

**Gotchas:**

1. **NaN payloads**: IEEE 754 allows multiple bit patterns for NaN. WASM
   operations that produce NaN may return different NaN payloads across CPU
   architectures or engine versions. Use `math.Float64bits()` /
   `math.Float32bits()` for exact comparison, or avoid NaN entirely.
2. **Denormal numbers**: Some CPUs flush denormals to zero; a conforming WASM
   engine preserves them. This is only visible if comparing floats
   for equality near the denormal range.
3. **FMA (Fused Multiply-Add)**: The host Go compiler may apply FMA,
   changing the exact result of `a*b + c` vs `fma(a, b, c)`. WASM code
   is not affected, but be aware if mixing host and WASM computations.

### Cleat API Determinism

| API | Deterministic? | Notes |
|-----|---------------|-------|
| `h.Now()` | Yes | Virtual time from event history |
| `h.Random()` | Yes | Seeded replay-safe PRNG |
| `h.DurableCall()` | Yes | Recorded in event history |
| `h.DurableSleep()` | Yes | Virtual time |
| `h.AwaitSignals()` | Yes | Recorded signal replay |
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

## The monotonic clock is real elapsed time

**`h.Now()` is replayable and the monotonic clock is not.** A guest that reads its language's monotonic clock (Python's `time.monotonic()`,
`time.perf_counter()` and their `_ns` forms, C#'s `Stopwatch` and `Environment.TickCount64`, Go's `time.Since`) gets the host's real elapsed time. A
replay of the same workflow reads a different value, so anything computed from it differs too: an elapsed time, a timeout measured in the guest, a
`while monotonic() - t0 < 0.5` loop that ran 41 times and runs 42. The wall clock the workflow sees through `h.Now()` is recorded and is the same on
replay; so is a sleep taken with `h.DurableSleep()` (`h.sleep()` in Python).

| guests on | what the monotonic clock is | how that was established |
|---|---|---|
| the core-module path (Go runs here) | real elapsed time since the instance started, never zero and never backwards (`nextMonotonicNs`) | **read from the code.** It was a count of reads until cleat#1300, which made a 500 ms sleep take 62.5 s, so it was made real on purpose |
| the component path (Python, C#) | the host's own monotonic clock; `wasi:clocks/monotonic-clock` is not shadowed | **measured**, Python 2026-11-23 on componentize-py 0.25.1: two runs of one workflow id read `0.5029` and `0.5021` seconds for a `time.sleep(0.5)`, and the deadline loop above ran 41 and then 42 iterations. C# `Stopwatch`: measured to differ on every run, as the C# README records |

**Which path a guest takes follows its artifact, not its language**: `isComponentWasm` in `engine/backend_wasmtime.go` sends a Component Model binary down
the component path and anything else down the core-module path. Python and C# ship components; Go ships a core module; Rust's guest is a
`wasm32-unknown-unknown` core module (`tests/cross-language`), where what `Instant::now` does was **not looked at**; Java and JavaScript were **not
checked**. So the heading's claim is as wide as the two rows above it, and no wider.

**Why the component path is not simply pinned.** A shadow of that interface replaces all of it, and the interface also exports `subscribe-instant` and
`subscribe-duration`, which return `pollable` resources the host cannot supply from outside the WASI implementation it links. Measured: a guest
instantiated against a shadow of `now` and `resolution` alone fails with `component imports instance wasi:clocks/monotonic-clock@0.2.9, but a matching
implementation was not found in the linker`. So the clock stays real and the tooling says so.

**What to write instead.** Take two `h.Now()` readings and subtract them for an elapsed time, sleep with the durable sleep, and do not loop on a
monotonic deadline.

**What `cleat vet` says**, per language (what it refuses and what it does not):

| language | refused or flagged |
|---|---|
| Python | `time.sleep` (PY004, error, through an alias too); `time.time` (PY005, error); **`time.monotonic`, `monotonic_ns`, `perf_counter`, `perf_counter_ns`, `process_time`, `process_time_ns` (PY016, warning)**; **`select.select` and a blocking `queue.Queue.get` (PY017, warning)** |
| C# | `Stopwatch` (CV006), `Environment.TickCount` and `TickCount64` (CV007), `Thread.Sleep` (CV005) |
| Go | `time.Now` (E003), `time.Sleep` (E004), `time.After`, `NewTicker`, `NewTimer` (E014) |
| Rust | `std::time::Instant::now` (R005) |
| Java | `System.currentTimeMillis` (J001) and `Thread.sleep` (J003); `System.nanoTime` is **not** flagged |
| TypeScript | no vet exists |

PY016 is a warning, not an error, so it never refuses a build that runs today. It looks at what the workflow executes. A module-level
`T0 = time.monotonic()` is **not** flagged: componentize-py runs module code while it builds, so both runs read the same value (measured, `2.607878167`
twice). A module-level *reference*, `clock = time.perf_counter`, is flagged where it is called, because that reads the real clock at run time (measured,
`0.0199` then `0.0109`). Python's `time.sleep` also blocks its execution for its whole duration and
the instance timeout does not interrupt it (cleat#4403, measured: a 0.5 s sleep under a 150 ms timeout ran its full 524 ms and failed only after it
returned. `time.process_time()` is not a monotonic clock but is flagged for the same reason: two runs read `-2.0412` and `-2.0397` seconds, negative on both. **Not measured:** the monotonic clock in Rust, Java and TypeScript guests, and what a blocking sleep does to other workflows on the same worker.

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

## The C# determinism check

`cleat build --target csharp` and `cleat vet --lang csharp <dir>` read a C# project's `.cs` files
and refuse the APIs a component cannot run or cannot replay: `Task.Run`, `Task.Factory.StartNew`, `Task.Delay`, a blocking
`.Wait()`, `new Thread`, `Thread.Sleep`, `Stopwatch`, `Environment.TickCount[64]`,
`RandomNumberGenerator`, `File`/`Directory` and `HttpClient`. Every rule cites a measurement made
against the real engine (`tests/csharp/determinism_probe_test.go`), and so does what it leaves
alone: `DateTime.UtcNow`, `Random` and `Guid.NewGuid()` replay, because the engine pins the wall
clock to the workflow's start and seeds entropy from the workflow's identity. A catch that can
swallow `CleatSuspendException` is a warning, not an error. The reasoning and the limits (no type
information, aliases, `.Result`) are in `docs/contributor/design/csharp-determinism-checker.md`.
C# is a tier-2 language on a prerelease toolchain (`tiers.yaml`, entry `sdk-csharp`, lists what is and is not claimed).

## Why there is no `RegisterQueryHandler`

Every SDK (Go, Rust, Java, Python) carried a
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
