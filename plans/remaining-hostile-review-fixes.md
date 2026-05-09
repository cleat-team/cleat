# Remaining Hostile Review Fixes — Three-Stream Plan

## Context

The hostile review identified 57 findings (14 CRITICAL, 21 HIGH, 22 MEDIUM). Streams
A and B are substantially complete. The remaining 29 open issues cluster around:
exactly-once semantics, WASM cache management, output buffer handling, JSONB binary
safety, structured errors, plugin safety, TestEnv fidelity, and operational maturity.
Five CRITICALs remain — 1 in Stream A, 4 in Stream C.

---

## Stream D: Deep Engine Hardening

**Theme:** The last correctness gaps in the execution engine. Exactly-once DurableCall is
the single hardest remaining item.

**Files touched:** `internal/host/engine.go`, `internal/host/db.go`,
`internal/host/memory.go`, `internal/host/compaction.go`, `internal/host/sharded_store.go`,
`cmd/cleat-worker/main.go`, `schema.sql`

### Phase D1 — Exactly-Once DurableCall (CRITICAL)

**Problem:** `freshCall` (engine.go:750) holds the API response in memory via
`s.recordEvent()`. The event batch is flushed to DB later in `FinalizeWorkflowSegment`.
A crash between API call success and DB flush means the call re-executes on replay —
duplicate side effects.

**Solution:** Immediate flush. After every freshCall, write that single event to
`event_history` in its own transaction (new method `FlushCallEvent` on the store) before
returning to WASM. On replay, `replayCall` (engine.go:800) finds the event and returns the
cached response — true exactly-once.

Add `ErrAmbiguous` to errors.go for the case where a `pending`-status event is found
(call intent was written but response never persisted).

**Estimated:** ~80 lines in engine.go, ~30 in db.go, ~10 in errors.go. ~1 day.

### Phase D2 — Wire invokeDefersOnTrap + Version Compat + Binary-Safe JSONB (HIGH)

**2a. invokeDefersOnTrap** exists at engine.go:675 but is never called. Wire it into the
WASM trap path: in executeCompiled (line 616), if `err` is a wazero trap (not a Go error),
call `invokeDefersOnTrap` on the still-live module before `mod.Close()`.

**2b. Version compatibility** — `version_compat.go` exists. Call `ValidateVersionCompatibility`
at the top of `executeCompiled` when replaying (history is non-nil). Fail with a clear error
listing incompatible version transitions.

**2c. Binary-safe JSONB** — In `AppendEventHistoryBatch` (db.go:768), base64-encode
`request` and `response` fields before JSONB storage. Decode in `LoadEvents`.
OR: add BYTEA columns alongside JSONB for raw payloads. Prefer the base64 approach
for minimal schema change.

**Estimated:** ~60 lines across engine.go, db.go, version_compat.go. ~1 day.

### Phase D3 — Output Buffer + WASM Cache Eviction (HIGH)

**3a. Output buffer** — Replace hardcoded 64KB with configurable `maxResponseBytes`
(default 1 MB). Change `writeWasmStringOrTrap` to propagate the error instead of
discarding it (it currently calls `writeWasmString` and ignores the error at memory.go:50-52).
Change all 6 call sites in engine.go to check the return value and return an error code
to WASM when writes fail.

**3b. WASM byte cache** — Replace `map[string][]byte` (main.go:361,546) with an LRU cache
that has max entry count (default 100) AND max total bytes (default 500 MB). Reuse the
`container/list` pattern from plugin_loader.go:452-515. Expose hit/miss/eviction metrics.

**Estimated:** ~100 lines in memory.go/engine.go, ~120 lines in main.go. ~1.5 days.

### Phase D4 — Performance + Integrity Polish (MEDIUM)

- **Batch heartbeats** (db.go:1242): Replace per-workflow UPDATEs with a single batch
  UPDATE using `WHERE assigned_to = $1 AND status = 'running'`. Already partially done.
- **Parallel shard claims** (sharded_store.go:163): Launch goroutines per shard, merge
  results via channel.
- **Checksum migration**: Apply the schema migration (event_history.checksum column)
  and enable `VerifyEventHistoryIntegrity` (db.go:2262-2315).
- **Compaction size limit** (compaction.go): Cap compacted entries at 10,000.
- **Streaming goroutine leak** (engine.go): Add `ctx.Done()` select alongside
  `for chunk := range chunkCh`.
- **Real WASM benchmarks**: Extend `benchmarks/wasm_bench_test.go` to run end-to-end
  through the full engine.

**Estimated:** ~200 lines. ~2 days.

---

## Stream E: Platform Security & Data Hygiene

**Theme:** Structured errors, scoped plugin DB access, data lifecycle. These are the
remaining security and observability gaps after Stream B.

**Files touched:** `internal/host/db.go`, `internal/host/errors.go`,
`cmd/cleat-worker/main.go`, `internal/plugin/capabilities.go`,
`internal/plugin/registry.go`

### Phase E1 — Structured Errors + Plugin DB Scoping (HIGH)

**E1a. Persist structured errors** — Change `FailWorkflow` (db.go) to accept
`CleatError` struct with `code`, `op`, `message` columns instead of a flat error string.
Store `error_code`, `error_op`, `error_message` separately on `workflow_instances`.
Modify caller in main.go line ~720 to pass structured error instead of `err.Error()`.

**E1b. Restrict plugin database access** — Add `DatabaseAccess` capability levels
(None, ReadOnly, ReadWrite) to the capability system. Plugins default to None and
must declare requirements. At registration time, wrap the `*sql.DB` with a proxy that
enforces the declared level (read-only TX for ReadOnly, deny for None).

**Estimated:** ~100 lines in db.go/errors.go/main.go, ~80 lines in capabilities.go/registry.go. ~1.5 days.

### Phase E2 — Event Retention Policy (HIGH)

Add a configurable `--retention-days` flag (default 30). A background goroutine runs
daily, deleting `event_history` rows for completed/failed/dead_lettered workflows older
than the cutoff. Respects the sharded store (deletes from each shard). Expose
`cleat_retention_deleted_total` counter. A value of 0 disables cleanup.

**Estimated:** ~80 lines in main.go, ~40 in db.go. ~1 day.

---

## Stream F: Developer Experience & Operational Maturity

**Theme:** TestEnv fidelity, tooling, documentation, multi-language validation,
crash isolation. These are mostly Stream C items with a few Stream A leftovers.

**Files touched:** `cleat/cleattest/cleattest.go`, `cleat/runtime.go`,
`internal/wasm/build.go`, `cmd/cleat-worker/main.go`, `internal/plugin/`,
`migrations/`, `sdk/python/`, `docs/guide/`, `.github/workflows/`

### Phase F1 — TestEnv Fidelity (HIGH/MEDIUM)

**F1a. Simulated clock for SendSignalAndWait** (cleattest.go): Replace `time.After`
with `env.simulatedClock.After` so `AdvanceTime` correctly triggers timeouts. Check
the current implementation in cleattest.go around line 982.

**F1b. Build tags in transformer** (internal/wasm/build.go): Before copying files to
the build directory, parse `//go:build` constraints and evaluate against
`GOOS=wasip1 GOARCH=wasm`. Skip files excluded for the WASM target. Add constraint
awareness to the closure validator.

**F1c. TinyGo in CI**: Install TinyGo in the CI workflow. Run the full test suite
under TinyGo compilation. Fail the build if the TinyGo path breaks.

**Estimated:** ~60 lines in cleattest.go, ~100 lines in internal/wasm/build.go, ~30 lines in CI config. ~1.5 days.

### Phase F2 — HostCalls Interface Split (HIGH)

Break the 69-method `HostCalls` interface into composable groups:
```go
type Caller interface { DurableCall(...); DurableCallJSON(...); DurableCallWithRetry(...); ... }
type Timer interface { DurableSleep(...); Now(); ... }
type Signaler interface { AwaitSignals(...); SendSignalAndWait(...); ... }
type Lifecycle interface { ContinueAsNew(...); DurableDefer(...); ... }
type Scoper interface { AcquireMutex(...); ReleaseMutex(...); ... }
```

`HostCalls` remains as the composite embedding all of them. Update TestEnv, localdev,
and embedded implementations to implement only the groups they need. This is a
mechanical refactor with no behavior change.

**Estimated:** ~200 lines across cleat/runtime.go and all implementations. ~2 days.

### Phase F3 — Migration Runner + Worker Drain + Search (MEDIUM)

**F3a. Migration runner**: Write `internal/migration/runner.go`. Reads versioned SQL
from `migrations/` directory, tracks applied versions in `schema_migrations` table,
runs pending migrations in order at worker startup. Fail startup if migrations can't
be applied.

**F3b. Worker drain API**: Add `POST /api/admin/drain` that sets a `draining` flag,
stops claiming new work, and returns 202 with in-flight count. `GET /api/admin/drain`
returns current count. Worker exits cleanly when count reaches 0.

**F3c. Search/filter**: Add `?input_contains=X` and `?error_contains=X` to workflow
list endpoint. Use PostgreSQL JSONB containment operators or full-text search with
a GIN index.

**Estimated:** ~150 lines for migration runner, ~100 lines for drain API, ~100 lines for search. ~2 days.

### Phase F4 — Docs + Stack Traces + Python WASM + Plugin Isolation

**F4a. DWARF stack traces**: Capture wazero trap information (instruction pointer, function
index). If the WASM module was compiled with DWARF debug info, resolve IP to source
location. Store in `error_msg` on failure.

**F4b. Python WASM end-to-end**: Write an end-to-end test: Python workflow →
componentize-py → WASM → deploy to PostgreSQL → execute on real worker → verify
event history. Fix ABI mismatches between Python stubs and actual cleat host imports.

**F4c. Plugin crash isolation**: Wrap each Go-compiled plugin host function call in
`defer`/`recover`. If a plugin panics, mark it as unhealthy, log the stack trace,
return an error to the workflow. Do not crash the worker.

**F4d. Documentation**: Write `docs/guide/upgrading.md`, `docs/guide/disaster-recovery.md`,
`docs/guide/zero-downtime-deploy.md`.

**Estimated:** ~150 lines for stack traces, ~60 lines for plugin recovery, ~100 lines for Python test, docs are docs. ~3 days.

---

## Stream Independence

Streams D, E, and F touch mostly disjoint files:
- **Stream D**: engine.go, db.go, memory.go, compaction.go, sharded_store.go, main.go (cache only), schema.sql
- **Stream E**: db.go (structured errors), errors.go, capabilities.go, registry.go, main.go (retention goroutine)
- **Stream F**: cleattest.go, cleat/runtime.go, internal/wasm/build.go, main.go (drain/search), plugin/, migrations/, docs/

Where streams touch the same file (db.go, main.go), they touch different functions.
Stream D modifies `AppendEventHistoryBatch`/`FlushCallEvent`/`LoadEvents`. Stream E
modifies `FailWorkflow` and adds a retention query. Stream F adds drain/search endpoints.
Clean merge.

## Execution Order

```
Stream D (4 phases, ~6 days)
├── D1: Exactly-once DurableCall (CRITICAL)
├── D2: invokeDefersOnTrap + version compat + binary-safe JSONB
├── D3: Output buffer + WASM cache eviction
└── D4: Performance polish + integrity + benchmarks

Stream E (2 phases, ~2.5 days)
├── E1: Structured errors + plugin DB scoping
└── E2: Event retention policy

Stream F (4 phases, ~8.5 days)
├── F1: TestEnv fidelity (clock + build tags + TinyGo CI)
├── F2: HostCalls interface split
├── F3: Migration runner + drain API + search
└── F4: Stack traces + Python WASM + plugin isolation + docs
```

Streams D, E, F can be worked in parallel — no dependencies between them.

## Verification

- **D1**: Workflow that makes a DurableCall, kill worker mid-execution, verify no
  duplicate API call on replay
- **D2**: Trap a WASM module, verify defer runs. Run v1 event history against v2
  code, verify clear error. Store non-UTF-8 bytes, verify they survive round-trip
- **D3**: Send >64KB response, verify error propagation. Load 200 workflow versions,
  verify cache stays within bounds
- **E1**: Verify structured error columns populated on failure. Verify plugin with
  `ReadOnly` access cannot write
- **E2**: Insert old events, run retention, verify cleanup
- **F1**: Call SendSignalAndWait with AdvanceTime, verify timeout triggers correctly
- **F2**: Compile all SDKs and mocks against split interface
- **F3**: Apply migrations fresh and from prior version. Drain worker, verify no new
  claims
- **F4**: Trap a workflow with DWARF, verify source location in error_msg. Run Python
  workflow end-to-end
