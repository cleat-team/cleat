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

3. **Go is the only production-ready SDK.** Rust is clean but missing APIs.
   Java/TeaVM works with painful build workarounds. AssemblyScript is severely
   constrained. The Python SDK exists (4,508 lines) but WASM compilation has
   never been validated end-to-end.

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

### Rust (1 port — Clean but Missing APIs)

The smallest SDK at 537 lines total (host_calls 290 + memory 126 + proc-macro 121).
The port produced a 141 KB WASM binary (release, stripped).

**Strengths:**
- `#[cleat_entry]` proc-macro — compile-time code generation, praised as best DX
- Clean ABI boundary, no unnecessary abstractions
- Smallest WASM binaries

**Critical gaps (4):**
- No K/V state operations (`get_state`/`set_state`/`delete_state`) — Virtual Object patterns impossible
- No `resolve_promise` — can create and await promises, but can't resolve externally
- No `ctx.run()` equivalent for wrapping non-deterministic code
- No test harness — no way to test workflows without a running host

### Java / TeaVM (2 ports — Works, Painful Build)

The saga example compiles to WASM via TeaVM 0.10.2 (692 KB binary). However,
6 pre-existing SDK/build blockers were found.

**Critical issues:**
- **TeaVM tree-shaking is a fundamental design problem** — `@DurableEntry` generates
  `*_Export` classes, but TeaVM removes them because they're unreachable from `mainClass`.
  Every entry point must be manually listed in `preservedClasses`. For large projects,
  this is "cumbersome and error-prone" — the single most significant SDK issue across
  all ports.
- `JsonHelper.parse()` only supports `String.class` — all inputs must be pre-serialized
- No Saga abstraction in Java SDK
- `String.replace()` compiles to `Pattern.compile()`, which TeaVM's WASM target doesn't support
- No `getQueryState()` in SDK
- Multi-project Gradle plugin version conflicts

### AssemblyScript (3 ports — Severely Constrained)

WASM binaries are small (~12-14 KB) but the language subset is restrictive.

**Critical constraints:**
- **No try/catch** with `--runtime stub` — all error handling must use return-value checks
- **No closures** — only named top-level functions. Breaks Saga's expected API.
- **No async/await** — cleat's sync + suspend model is correct but different from original TS
- **No `any` type** — all types must be explicit
- **SUSPEND_SENTINEL bug** — bit 62 overlaps with signal name length field, causing
  potential out-of-bounds reads on `awaitSignals`
- No equivalent of DBOS's `setEvent`/`getEvent` for external workflow communication
- `@durableEntry` transform partially fixed but untested end-to-end with `durableSleep`/`awaitSignals`

### Python (5 ports — Exists on Paper, Not Validated)

The most requested language. The SDK is 4,508 lines with full ABI conformance
(22 host imports defined, `@cleat_entry` decorator, 80 tests passing, 3 example
workflows), but the `componentize-py` WASM compilation pipeline has never been
validated end-to-end.

**The critical finding:** All 22 `_import_*` functions in `host_calls.py` raise
`NotImplementedError`. They define the correct interface but are not wired to
actual WASM imports. The Python SDK exists on paper but no Python workflow has
ever run in a cleat worker.

**Issues found across Python ports:**
- No `TerminalError` in core SDK
- No Virtual Object / key-scoped state
- `@durable_entry` replaces the original function, making direct invocation impossible
- No blocking `get_event` equivalent — must poll via `set_query_state`
- No external signal-sending API
- No in-process `LocalRuntime` for development
- Saga uses lambda closures — unknown whether `componentize-py` supports closures
  across WASM suspend/resume
- `CleatTestHarness` is stub-based, doesn't persist state across calls

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

### 4. No Virtual Object / Key-Scoped State

Restate's single-writer-per-key is one of its most compelling features.
Multiple ports called the absence of this in cleat "BLOCKING for stateful
workflows." Manual key prefixing is the workaround; it's error-prone.

### 5. No `ctx.run()` / Side Effect Wrapper

Both Restate and Temporal let you wrap non-deterministic code once and replay
the cached result. Cleat has no equivalent. Any pure function that runs during
replay executes again. Relevant for random number generation, UUID generation,
and external ID assignment within workflow code.

### 6. Testing Varies Wildly by Language

| Language | Testing | Status |
|----------|---------|--------|
| Go | `cleattest.TestEnv` — WASM-free, simulated clock, call assertions | Mature |
| Python | `CleatTestHarness` — stub-based, no state persistence across calls | Basic |
| Rust | None | Missing |
| Java | Standard JUnit, no cleat-specific harness | Basic |
| AS | None — must compile to WASM and test via Go runner | Missing |

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
- **Python**: 16 issues — SDK not wired to WASM, missing primitives, test harness gaps
- **AS**: 10 issues — no try/catch, no closures, SUSPEND_SENTINEL bug, no test framework
- **Java**: 13 issues — TeaVM tree-shaking, `JsonHelper` limitations, missing Saga
- **Rust**: 4 critical gaps — no K/V state, no resolve_promise, no ctx.run, no test harness

### Architecture Gaps

- No `ctx.run()` / side-effect wrapper (6 ports affected)
- No entity workflow / Virtual Object pattern (4 ports)
- No `AwaitCondition` / predicate-based blocking (3 ports)
- Per-call timeouts defined but not enforced (2 ports)
- `DurableDefer` is description-only, not a closure (3 ports)

### Build System

- TeaVM Gradle plugin resolution (Java)
- `componentize-py` end-to-end untested (Python)
- `@durableEntry` tree-shaking by TeaVM (Java)
- WASM export class warnings with `--runtime stub` (AS)

### Documentation

- Service contracts must be defined explicitly (no auto-discovery)
- No per-language migration guide from Temporal/DBOS/Restate (Go only)
- No documented WASM size expectations per language

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
1. **Validate Python WASM end-to-end** — the SDK exists, the compilation pipeline exists, but they've never been connected
2. **Fix TeaVM tree-shaking** — the `preservedClasses` requirement is the biggest single SDK issue
3. **Add K/V state to Rust SDK** — 4 critical gaps, all small
4. **Add test harnesses for Rust, Java, and AS** — without WASM-free testing, developer velocity suffers

The engine is ready. The SDKs need the work.
