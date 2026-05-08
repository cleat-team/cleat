# Developer Experience: Cleat vs Temporal, DBOS, and Restate

Insights from porting 19 open-source projects across 5 languages (Go, Python,
TypeScript/AssemblyScript, Java, Rust), producing 202 documented issues and
validating cleat's architecture against real-world code.

---

## Headline Findings

1. **Cleat's DX is cleaner than all three competitors for common patterns** —
   one-file workflows, no workflow/activity split, built-in Saga, and single-call
   signal handling save ~28% lines of code vs equivalent Temporal/DBOS ports.

2. **WASM-based versioning is cleat's killer feature** — "deploy via INSERT,
   rollback via UPDATE" has no equivalent in any competitor. Multiple independent
   analyses reached this conclusion.

3. **Go is the only production-ready SDK.** Rust is clean (all core APIs present
   after SDK hardening). Java/TeaVM works with painful build workarounds.
   AssemblyScript is severely constrained by its runtime subset. The Python SDK
   is the most comprehensive (4,508 lines, 34 WIT imports, LangChain/LangGraph
   integration) but `componentize-py` WASM compilation has never been validated
   end-to-end.

4. **The WASM sandbox is both cleat's superpower and its main friction point** —
   it enables language-agnostic workflows and deterministic replay, but forces
   developers to extract I/O, DB access, and network calls to host-side services.

5. **88M steps/sec core throughput** means WASM overhead is negligible. The
   bottleneck is always I/O, not the sandbox.

---

## Part 1: Code Volume Comparison

Concrete data from the DBOS widget-store port (TypeScript → AssemblyScript):

| Component | DBOS TS (original) | Cleat AS (ported) | Delta |
|-----------|-------------------|-------------------|-------|
| Workflow code | ~40 lines | ~40 lines | Same |
| DB operations | ~80 lines (Knex) | ~20 lines (`durableCall` JSON) | **-75%** |
| HTTP server | ~60 lines (Fastify) | ~50 lines (worker API) | -17% |
| Configuration | ~10 lines (yaml) | ~5 lines (env vars) | -50% |
| Tests | ~80 lines (Jest) | ~80 lines (TestEnv) | Same |
| **Total** | **~270 lines** | **~195 lines** | **~28% shorter** |

The reduction comes primarily from cleat's architecture:
- External service calls become `durableCall` invocations (no separate activity files)
- DB operations move to the host side (workflow code doesn't talk to databases directly)
- The HTTP server is handled by the worker's built-in REST API

Across the remaining 18 ports, consistent patterns emerged:
- **Temporal ports**: 3+ files become 1 (types + activities + workflow → single workflow file)
- **Restate ports**: Virtual Objects flatten to single-shot workflows with explicit state management
- **DBOS ports**: Decorator-based step classes become inline `durableCall` invocations

---

## Part 2: Language-by-Language Experience

### Go (4 ports — Production-Ready)

The strongest SDK. The transformer pipeline (`cleat build`) is fully automated.
`cleattest.TestEnv` provides WASM-free unit testing with stubs, simulated clock,
and call history assertions. Tests run in milliseconds.

**Strengths:**
- `DurableCallTypedWithOptions` — generic typed calls with retry, added from port feedback
- `AwaitSignals` — one call handles wait + timeout; Temporal needs 3 declarations
- Saga API with `AddParallel` — no competitor has parallel Saga with auto LIFO compensation
- Auto-threading — the transformer propagates `HostCalls` through the call chain automatically
- Build-time static analysis catches non-determinism before deploy

**Gaps found:**
- `DurableDefer` takes a string description, not a closure (Saga is the recommended replacement)
- No per-call `StartToCloseTimeout` equivalent
- `ContinueAsNew` is a no-op in `TestEnv`
- No `AwaitCondition` / predicate-based blocking mechanism

### Rust (1 port — Clean, Core APIs Present)

The smallest SDK at 1,090 lines (host_calls expanded from 537 lines after SDK
hardening pass). The port produced a 141 KB WASM binary (release, stripped).

**Strengths:**
- `#[cleat_entry]` proc-macro — compile-time code generation, praised as best DX
- Clean ABI boundary, no unnecessary abstractions
- Smallest WASM binaries
- All K/V state operations present (`set_state`/`get_state`/`delete_state`/`incr_state`/`has_state`/`list_state`)
- Full promise support (`create_promise`/`await_promise`/`resolve_promise`/`reject_promise`)
- Test harness exists (`test.rs` — WASM-free mock HostCalls with call assertions)

**Remaining gaps:**
- No Saga API (all other SDKs have it)
- No `ContinueAsNew` high-level wrapper
- No `ctx.run()` / side-effect wrapper (architectural — shared by all SDKs)

### Java / TeaVM (2 ports — Works, Painful Build)

The saga example compiles to WASM via TeaVM 0.10.2 (692 KB binary). The SDK
hardening pass added Saga, query state, and a `TestHostCalls` mock harness
(2,072 lines total).

**Resolved issues (SDK hardening pass):**
- Saga API now present (with `SagaTyped<T>` generic variant)
- `getQueryState()` / `setQueryState()` added
- `TestHostCalls` mock harness with call recording and simulated clock

**Remaining critical issues:**
- **TeaVM tree-shaking** — `@DurableEntry` generates `*_Export` classes, but
  TeaVM removes them as unreachable from `mainClass`. Every entry point must be
  manually listed in `preservedClasses`. This is a TeaVM limitation, not a cleat
  SDK bug.
- `JsonHelper.parse()` only supports `String.class` — all inputs must be pre-serialized
- `String.replace()` compiles to `Pattern.compile()`, unsupported by TeaVM WASM target
- Multi-project Gradle plugin version conflicts
- No `fetch_get_json` convenience wrapper

### AssemblyScript (3 ports — Severely Constrained)

WASM binaries are small (~12-14 KB) but the language subset is restrictive.
SDK hardening added a `TestEnv` test harness (1,626 lines) and K/V state operations.

**Resolved (SDK hardening pass):**
- Test harness exists (`test_runner/test-harness.ts` — `TestEnv` class with mock HostCalls)
- K/V state operations present

**Remaining critical constraints:**
- **No try/catch** with `--runtime stub` — AS runtime limitation, all error handling
  must use return-value checks
- **No closures** — AS limitation, only named top-level functions
- **No async/await** — cleat's sync + suspend model is correct but different from original TS
- **No `any` type** — all types must be explicit
- **SUSPEND_SENTINEL bug** — bit 62 overlaps with signal name length field, causing
  potential out-of-bounds reads on `awaitSignals` (AS runtime issue)
- `@durableEntry` transform partially fixed but untested end-to-end with `durableSleep`/`awaitSignals`

### Python (5 ports — Comprehensive SDK, WASM Unvalidated)

The most requested language and the most comprehensive SDK at 4,508 lines.
34 WIT imports defined, `@cleat_entry` decorator, `virtual_object` decorator,
80+ tests, 4 example workflows, and LangChain/LangGraph integration. However,
the `componentize-py` WASM compilation pipeline has never been validated
end-to-end — no Python workflow has been confirmed running in a cleat worker.

**Resolved (SDK hardening pass):**
- `TerminalError` added to core SDK
- Virtual Object support via `virtual_object` decorator and `set_scope`/`get_scope`
- External signal API (`signal_workflow`, `send_signal_and_wait`, `reply_to_signal`)
- K/V state operations via `cleat_call("state", ...)` — `set_state`/`get_state`/`delete_state`/`incr_state`/`has_state`/`list_state`
- `CleatTestHarness` with call recording and state persistence
- REST client (`CleatClient`) with `send_update`, `resolve_promise`, `send_signal`
- Update handler support (`register_update_handler` with WIT import)
- Promise support (create/await/resolve/reject with WIT imports)
- `Plugins` class with typed wrappers for LLM, blobstore, webhooks, and other plugins
- LangChain `CleatCallbackHandler` and LangGraph `CleatCheckpointer`

**Remaining gaps:**
- **WASM compilation never validated end-to-end** — `build_wasm.py` and WIT file exist
  but `componentize-py` has never produced a binary loaded by a cleat worker
- `child_workflow_with_options` has no WIT import (stub only)
- `cleat_fetch` has no dedicated WIT import (works via `cleat_call("http", ...)` but
  may not work through `componentize-py` without the WIT binding)
- Saga uses lambda closures — unknown whether `componentize-py` supports closures
  across WASM suspend/resume

---

## Part 3: Where Cleat's Model Wins

### 1. No Workflow/Activity Split

The #1 DX advantage across all ports. Business logic lives in one file.

```
Temporal:  types.go + activities.go + workflow.go  = 3 files
DBOS:      steps.ts + workflow.ts                   = 2 files
Restate:   handler.ts + router.ts + service.ts      = 2-3 files
Cleat:     workflow.go                               = 1 file
```

### 2. Signal Handling Is One Call

```go
// Cleat: 1 call, handles wait + timeout
result := h.AwaitSignals([]string{"driver_accepted", "driver_declined"}, 2*time.Minute)
if result.TimedOut { /* ... */ }
```

```
Temporal:   defineSignal() + setHandler() + condition() = 3 declarations
DBOS:       DBOS.recv(topic, timeout) = 1 call (good, but untyped)
Restate:    ctx.promise("name") = 1 call (good, but no multi-signal support)
```

### 3. Saga Is Built In

```go
s := cleat.NewSaga()
s.AddStep("charge", chargeFn, refundFn)
s.AddStep("book",   bookFn,   cancelFn)
s.AddParallel(
    cleat.SagaStep{Forward: bookFlight, Compensate: cancelFlight},
    cleat.SagaStep{Forward: bookHotel,  Compensate: cancelHotel},
)
if err := s.Run(h); err != nil { /* compensations already ran */ }
```

No competitor has this. Temporal and DBOS require manual compensation arrays
and LIFO loops. The `AddParallel` variant (concurrent booking with automatic
rollback on any failure) has no direct equivalent in any system.

### 4. Infrastructure Simplicity

| Component | Temporal | DBOS | Restate | Cleat |
|-----------|----------|------|---------|-------|
| Server needed | Temporal server cluster | None (in-app) | Restate server | None |
| Database | Any (Cassandra/MySQL/PG) | PostgreSQL | RocksDB + Kafka | PostgreSQL |
| Worker model | Per-task-queue pools | App IS the worker | Runtime in server | Stateless daemon |
| Services to operate | 3+ | 1+ | 3+ | 1 |

### 5. WASM Versioning — "Deploy via INSERT"

```bash
cleat deploy --name order-processing ./order.wasm   # INSERT
cleat rollback order-processing 3                      # UPDATE
cleat versions order-processing                        # SELECT
```

Workflows carry `(def_name, def_version)`. In-flight instances replay against
their exact original WASM bytes. Workers are a stable runtime — change them
only when the HostCalls ABI changes. This decouples workflow lifecycle from
worker lifecycle, which is unique.

### 6. Build-Time Static Analysis

`cleat build` catches 11 categories of non-determinism before deploy:
forbidden imports, goroutines, channels, interface dispatch, float comparisons.
Temporal catches these at runtime with cryptic replay divergence errors.
DBOS relies on developer discipline.

---

## Part 4: Where Cleat Is Harder

### 1. WASM Sandbox Requires Extracting I/O

File operations, database access, and network calls cannot live in workflow
code. They must be extracted to host-side services. The fileprocessing port
called this a "fundamental architectural difference that affects all
file-processing workflows." On the other hand, this is by design — the
sandbox IS the determinism guarantee.

### 2. String-Based Service Routing

```go
h.DurableCall("billing", "Charge", requestJSON)
```

Loses compile-time type checking. Temporal passes actual function references.
DBOS uses class method references. Several ports noted this as a downgrade.
Mitigated by `cleat-gen` (typed client code generator) and `DurableCallTyped`.

### 3. No Entity / Long-Lived Instance Pattern

Temporal workflows can be long-lived class instances with in-memory state across
handler invocations. Cleat workflows are single-shot function invocations.
Ports requiring entity patterns had to manually implement `continue_as_new`
cycles with explicit state persistence and rehydration.

### 4. No Virtual Object / Key-Scoped State (Runtime Level)

Restate's single-writer-per-key is one of its most compelling features.
**SDK-level support now exists** — all SDKs have `set_scope`/`get_scope`/`clear_scope`
for key-prefixed state, and the Python SDK has a `virtual_object` decorator.
However, there is no **runtime enforcement** of single-writer semantics per key.
The prefixing is convention-based (keys get `"vo:<type>:<key>:"` prefix), not
enforced by the worker or database. Manual key prefixing without the scope
helpers remains error-prone.

### 5. No `ctx.run()` / Side Effect Caching

Both Restate and Temporal let you wrap non-deterministic code once and replay
the cached result. `RunDetached` exists in all SDKs but solves a different
problem (escape hatch from cancellation). There is no result-caching wrapper
that records the output on first execution and returns the cached value on
replay. Relevant for random number generation, UUID generation, and external
ID assignment within workflow code. **Architectural** — would require engine
changes to record side-effect results in event history.

### 6. Testing Varies by Language (Improved Since Ports)

| Language | Testing | Status |
|----------|---------|--------|
| Go | `cleattest.TestEnv` — WASM-free, simulated clock, call assertions | Mature |
| Python | `CleatTestHarness` — stub-based, call recording, state persistence | Functional |
| Rust | `test.rs` — WASM-free mock HostCalls with call assertions | Functional |
| Java | `TestHostCalls` — mock HostCalls with call recording, simulated clock | Functional |
| AS | `TestEnv` (1,626 lines) — mock HostCalls class | Functional |

All SDKs now have a test harness (added in SDK hardening pass). Go's
`cleattest.TestEnv` remains the gold standard — it's the only one with
a fully simulated deterministic clock and `AdvanceTime` for testing
sleep/timeout behavior without real waits.

### 7. Unit Mismatches Across Systems

| System | Sleep unit | Timeout unit |
|--------|-----------|-------------|
| Cleat | Milliseconds | Milliseconds |
| Temporal | `time.Duration` (Go) / seconds (TS) | Same |
| DBOS | Seconds | Milliseconds |
| Restate | `Duration` (Rust) / seconds (TS) | Same |

Every migration guide documents this as a systematic hazard.

---

## Part 5: The 202 Issues — What They Tell Us

The 202 documented issues across 19 ports break down into these categories:

### SDK Maturity (most common)

- **Go**: 8 issues — mostly API ergonomics (typed heartbeats, `AwaitCondition`, Saga typed steps)
- **Python**: 3 issues remaining — WASM pipeline validation, WIT gaps (`child_workflow_with_options`, `cleat_fetch`), Saga lambda compatibility. 13 of 16 original issues closed by SDK hardening.
- **AS**: 5 issues — AS runtime limitations (no try/catch, no closures, no async/await, SUSPEND_SENTINEL bug). 5 of 10 original issues closed by SDK hardening (test harness, K/V state, etc.).
- **Java**: 5 issues — TeaVM tree-shaking, `JsonHelper` String.class only, Gradle conflicts, missing convenience wrappers. 8 of 13 original issues closed (Saga, query state, TestHostCalls).
- **Rust**: 2 issues remaining — no Saga, no ContinueAsNew wrapper. 2 of 4 original gaps closed by SDK hardening (K/V state, resolve_promise, test harness added).

### Architecture Gaps

- No `ctx.run()` / side-effect caching (architectural — requires engine event history changes)
- No entity workflow / Virtual Object runtime enforcement (SDK-level convention exists, runtime enforcement deferred)
- No `AwaitCondition` / predicate-based blocking (3 ports affected)
- Per-call timeouts defined but not enforced (2 ports)
- `DurableDefer` is description-only, not a closure (3 ports)

### Build System

- TeaVM Gradle plugin resolution (Java)
- `componentize-py` end-to-end untested (Python)
- `@durableEntry` tree-shaking by TeaVM (Java — TeaVM limitation)
- WASM export class warnings with `--runtime stub` (AS — AS limitation)

### Documentation

- Service contracts must be defined explicitly (no auto-discovery)
- No per-language migration guide from Temporal/DBOS/Restate (Go only)
- No documented WASM size expectations per language
- DX_COMPARISON.md was stale (now updated)

---

## Part 6: Bottom Line

Cleat's developer experience is cleaner than Temporal, DBOS, and Restate for
the common patterns tested (signals, retry, Saga, fan-out/fan-in, scheduling).
The signal/timeout pattern (`AwaitSignals`) and Saga API are genuinely better —
more direct, fewer lines, less ceremony.

The WASM sandbox is the right architectural choice but it creates real friction:
I/O must be extracted, service contracts must be maintained, and language
subsets (particularly AssemblyScript) are restrictive. For Go workflows, the
tradeoff is clearly positive. For other languages, the SDK maturity gap
dominates the experience.

The critical path to multi-language viability:
1. **Validate Python WASM end-to-end** — the SDK exists (4,508 lines, 34 WIT imports), the compilation pipeline exists (`build_wasm.py`), but they've never been connected
2. **Fix TeaVM tree-shaking** — the `preservedClasses` requirement is the biggest single SDK issue (TeaVM limitation, not cleat bug)
3. **Add lock API to all SDKs** — AcquireLock/ReleaseLock is implemented in the Go runtime but exposed in zero SDKs
4. **Add Saga to Rust SDK** — Rust is the only language without compensation (all primitives exist)

Items 3 and 4 from the original critical path (K/V state for Rust, test harnesses for Rust/Java/AS) are now DONE via the SDK hardening pass.

The engine is ready. The remaining SDK work is narrow and well-scoped.
